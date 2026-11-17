package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bindingFaults reads one generated Go file with go/ast and gives each construct by which the service would decide
// at startup or per request what runs (ST-GEN-INV14): a func init, an import of plugin, a read of the declaration
// file or of the binary's own file or build information, and a look at struct tags.
func bindingFaults(t *testing.T, name string, data []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var faults []string
	for _, spec := range file.Imports {
		if path, _ := strconv.Unquote(spec.Path.Value); path == "plugin" {
			faults = append(faults, "imports plugin")
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:embed") && strings.Contains(comment.Text, ".yaml") {
				faults = append(faults, "embeds "+comment.Text)
			}
		}
	}
	faults = append(faults, packageVarCalls(file)...)
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.Recv == nil && n.Name.Name == "init" && !protobufRegistration(data, file, n) {
				faults = append(faults, "declares func init")
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && strings.Contains(n.Value, "service.yaml") {
				faults = append(faults, "names service.yaml")
			}
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok {
				switch x.Name + "." + n.Sel.Name {
				case "os.Executable", "debug.ReadBuildInfo", "reflect.StructTag":
					faults = append(faults, "calls "+x.Name+"."+n.Sel.Name)
				}
			}
			if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Tag" && (n.Sel.Name == "Get" || n.Sel.Name == "Lookup") {
				faults = append(faults, "reads a struct tag")
			}
		}
		return true
	})
	return faults
}

// startCalls are the functions that a package-level var of generated code may call, by the import path of their
// package ("" for a builtin or a function of the same package), each a pattern of names. A var runs its value at
// start, as a func init does, so any other call there is a fault (ST-GEN-INV14; security review of #401, L3). Each
// entry is one that a generator writes: an error value, a compiled pattern, the declared commands, queries,
// entities, refusals and workers of the runtime, the parsed relations of authorization, the entity views of the
// generated data package, and the helpers must, mustCommand, mustQuery and declare that wrap them, and make in the
// protobuf code.
var startCalls = map[string]*regexp.Regexp{
	"":       regexp.MustCompile(`^(make|must|mustCommand|mustQuery|declare)$`),
	"errors": regexp.MustCompile(`^New$`),
	"regexp": regexp.MustCompile(`^MustCompile$`),
	"github.com/jsell-rh/stego/registry/runtime/command": regexp.MustCompile(`^(Must|NewCommand|NewEntity|NewList|NewRefusal|NewWorker)$`),
	"github.com/jsell-rh/stego/registry/runtime/query":   regexp.MustCompile(`^(NewEntity|NewQuery|NewListQuery)$`),
	"github.com/jsell-rh/stego/registry/runtime/authz":   regexp.MustCompile(`^ParseRelation$`),
	"/internal/api/data": regexp.MustCompile(`^Entity[A-Za-z0-9]+View$`),
}

// startCallAllowed tells whether a call in the value of a package-level var is one of startCalls or a conversion:
// to a parenthesized type (conversion) or to a type that the file declares (typeNames).
// The package of a qualified call is found by the imports of the file; the generated data package is matched by the
// end of its path, which holds the module of the service.
func startCallAllowed(call *ast.CallExpr, imports map[string]string, typeNames map[string]bool) bool {
	if conversion(call, typeNames) {
		return true
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		// A name of a type that the file declares is a conversion, such as Edition(1) in the defaults of the
		// protobuf code.
		return typeNames[fun.Name] || startCalls[""].MatchString(fun.Name)
	case *ast.SelectorExpr:
		x, isIdent := fun.X.(*ast.Ident)
		if !isIdent {
			return false
		}
		path, imported := imports[x.Name]
		if !imported {
			return false
		}
		if names, listed := startCalls[path]; listed {
			return names.MatchString(fun.Sel.Name)
		}
		return strings.HasSuffix(path, "/internal/api/data") && startCalls["/internal/api/data"].MatchString(fun.Sel.Name)
	}
	return false
}

// conversion tells whether a call is a conversion to a type that only a type expression can name: a call of a
// parenthesized pointer type with the one argument nil, such as (*T)(nil), of a parenthesized type that the file
// declares (typeNames), such as (Edition)(0), or of a parenthesized array, map,
// channel, function, interface or struct type. A parenthesized name or selector, such as (register)() or
// (os.Setenv)("A", "b"), is a call (security review of #401, L1).
func conversion(call *ast.CallExpr, typeNames map[string]bool) bool {
	paren, isParen := call.Fun.(*ast.ParenExpr)
	if !isParen {
		return false
	}
	switch inner := paren.X.(type) {
	case *ast.Ident:
		// (Edition)(0) in the type list of the protobuf code: a type that the file declares.
		return typeNames[inner.Name]
	case *ast.StarExpr:
		nilArg := len(call.Args) == 1
		if nilArg {
			ident, isIdent := call.Args[0].(*ast.Ident)
			nilArg = isIdent && ident.Name == "nil"
		}
		switch inner.X.(type) {
		case *ast.Ident, *ast.SelectorExpr:
			return nilArg
		}
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	}
	return false
}

// declaredTypes gives the names of the types that a file declares at package level.
func declaredTypes(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				names[spec.(*ast.TypeSpec).Name.Name] = true
			}
		}
	}
	return names
}

// fileImports gives the import path of each package name of a file: its alias, or the last element of its path.
func fileImports(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

// packageVarCalls gives a fault for each call in the value of a package-level var, of any name, that startCalls does
// not list and that is not a conversion: such a var runs code at start as a func init does (security review of
// #394, L3, and of #401, L3). The body of a function literal in a value runs only when it is called, so it is not
// checked here.
func packageVarCalls(file *ast.File) []string {
	imports := fileImports(file)
	typeNames := declaredTypes(file)
	var faults []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			for _, expression := range spec.(*ast.ValueSpec).Values {
				ast.Inspect(expression, func(node ast.Node) bool {
					switch n := node.(type) {
					case *ast.FuncLit:
						return false
					case *ast.CallExpr:
						if !startCallAllowed(n, imports, typeNames) {
							faults = append(faults, fmt.Sprintf("calls %s in the value of a package-level var, which runs at start as a func init does", types.ExprString(n.Fun)))
						}
					}
					return true
				})
			}
		}
	}
	return faults
}

// protobufInit is the name of the function by which the protobuf code of protoc-gen-go registers one file's
// descriptors.
var protobufInit = regexp.MustCompile(`^file_[A-Za-z0-9_]+_init$`)

// protobufCalls are the package functions that the body of a file_<name>_init of protoc-gen-go calls, beside len,
// the file_<name>_init of the files it imports, PkgPath and Build (registersOnly): it builds the file's types from its
// raw descriptor and reads its own package path (google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo,
// genReflectFileDescriptor).
var protobufCalls = map[string]bool{"reflect.TypeOf": true, "unsafe.Slice": true, "unsafe.StringData": true}

// protobufRegistration tells whether the func init is the one that protoc-gen-go emits: a file with the
// protoc-gen-go header whose init only calls file_<name>_init(), which the same file declares, and whose body makes
// only the calls of protoc-gen-go's registration (protobufCalls, a file_<name>_init and a conversion such as
// (*T)(nil)). It registers the file's descriptors and decides nothing that runs, so ST-GEN-INV14 does not cover it
// (decision on ST-GEN-Q42, reading a). Any other func init is a fault, and so is a file_<name>_init that calls
// anything else (#394 security review L3).
func protobufRegistration(data []byte, file *ast.File, init *ast.FuncDecl) bool {
	if !bytes.HasPrefix(data, []byte("// Code generated by protoc-gen-go.")) || init.Body == nil || len(init.Body.List) != 1 {
		return false
	}
	statement, ok := init.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	name, ok := call.Fun.(*ast.Ident)
	if !ok || !protobufInit.MatchString(name.Name) {
		return false
	}
	for _, decl := range file.Decls {
		if register, isFunc := decl.(*ast.FuncDecl); isFunc && register.Recv == nil && register.Name.Name == name.Name && register.Body != nil {
			return registersOnly(register.Body, declaredTypes(file))
		}
	}
	return false
}

// registersOnly tells whether every call in the body of a file_<name>_init is one that protoc-gen-go's
// registration makes: a file_<name>_init, len, reflect.TypeOf, unsafe.Slice, unsafe.StringData, a conversion such
// as (*T)(nil), PkgPath only on the value of reflect.TypeOf, and Build only on a protoimpl.TypeBuilder literal
// (security review of #401, L2).
func registersOnly(body *ast.BlockStmt, typeNames map[string]bool) bool {
	only := true
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return only
		}
		switch fun := call.Fun.(type) {
		case *ast.ParenExpr:
			only = only && conversion(call, typeNames)
		case *ast.Ident:
			only = only && (fun.Name == "len" || protobufInit.MatchString(fun.Name))
		case *ast.SelectorExpr:
			switch x := fun.X.(type) {
			case *ast.Ident:
				only = only && protobufCalls[x.Name+"."+fun.Sel.Name]
			case *ast.CallExpr:
				// reflect.TypeOf(x{}).PkgPath()
				inner, isSelector := x.Fun.(*ast.SelectorExpr)
				only = only && fun.Sel.Name == "PkgPath" && isSelector && types.ExprString(inner) == "reflect.TypeOf"
			case *ast.CompositeLit:
				// protoimpl.TypeBuilder{...}.Build()
				only = only && fun.Sel.Name == "Build" && x.Type != nil && types.ExprString(x.Type) == "protoimpl.TypeBuilder"
			default:
				only = false
			}
		default:
			only = false
		}
		return only
	})
	return only
}

// generatedGo reads every Go file that apply wrote into the output folder of the project, by its path in it.
func generatedGo(t *testing.T, project string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	out := filepath.Join(project, "out")
	err := filepath.WalkDir(out, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(out, path)
		files[relative] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestServiceBindsItsDeclarationAtBuildTime(t *testing.T) {
	if testing.Short() {
		t.Skip("applies a project, builds the generated service and runs it")
	}
	t.Run("ST-GEN-INV14-T A generated service reads no declaration, scans nothing and registers nothing by name, tag or init", func(t *testing.T) {
		// ST-DECL-TD03 on the runtime data path, with two modes: a pooled service with authorization, tenancy, the
		// command and query components and the postgres-adapter.
		service := edit(t, td03Service(t), "base_path:", "modes: [api, worker]\nbase_path:")
		project := td03Project(t, service)
		if apply := stegoIn(t, project, nil, "apply"); apply.code != 0 {
			t.Fatalf("stego apply refused the declaration, exit %d:\n%s\n%s", apply.code, apply.stdout, apply.stderr)
		}
		files := generatedGo(t, project)
		if len(files) == 0 || files["main.go"] == nil {
			t.Fatalf("apply wrote no main.go: %d Go files", len(files))
		}
		for name, data := range files {
			for _, fault := range bindingFaults(t, name, data) {
				t.Errorf("the generated file out/%s %s", name, fault)
			}
		}
		// The construction in main.go is fixed when the code is generated: a second apply of the same declaration
		// gives the same bytes.
		first := string(files["main.go"])
		if again := stegoIn(t, project, nil, "apply"); again.code != 0 {
			t.Fatalf("the second stego apply failed, exit %d:\n%s\n%s", again.code, again.stdout, again.stderr)
		}
		if second := string(generatedGo(t, project)["main.go"]); second != first {
			t.Error("a second apply of the same declaration wrote another main.go")
		}

		// The generated service builds into one binary from one main package.
		binary := filepath.Join(t.TempDir(), "service")
		buildWithRuntime(t, project, "-o", binary, "./out")
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the build wrote no binary: %v", err)
		}
	})
}

func TestProtobufRegistrationIsTheOneAllowedInit(t *testing.T) {
	t.Run("ST-GEN-INV14-T Only the func init of protoc-gen-go that calls file_<name>_init is allowed", func(t *testing.T) {
		header := "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage v1\n\n"
		for _, c := range []struct {
			name, source string
			allowed      bool
		}{
			{"the protoc-gen-go registration", header + "func init() { file_shop_proto_init() }\nfunc file_shop_proto_init() {}\n", true},
			{"a protoc-gen-go file whose init does more", header + "func init() { file_shop_proto_init(); register() }\nfunc file_shop_proto_init() {}\nfunc register() {}\n", false},
			{"a protoc-gen-go file whose init calls another function", header + "func init() { register() }\nfunc register() {}\n", false},
			{"the same init in a file that protoc-gen-go did not write", "// Code generated by stego. DO NOT EDIT.\n\npackage v1\n\nfunc init() { file_shop_proto_init() }\nfunc file_shop_proto_init() {}\n", false},
			{"the registration body that protoc-gen-go writes", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody(""), true},
			{"a file_<name>_init that the file does not declare", header + "func init() { file_shop_proto_init() }\n", false},
			{"a file_<name>_init that also calls a function of the package", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("register()") + "func register() {}\n", false},
			{"a file_<name>_init that also calls a function of another package", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("os.Setenv(\"A\", \"b\")"), false},
			{"a file_<name>_init that calls Build of another package", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("evil.Build()"), false},
			{"a file_<name>_init that calls a parenthesized function", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("(os.Setenv)(\"A\", \"b\")"), false},
			{"a file_<name>_init that calls Build on another value", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("(evil).Build()"), false},
			{"a file_<name>_init that calls Build on a field", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("a.b.Build()"), false},
			{"a file_<name>_init that calls PkgPath on another value", header + "func init() { file_shop_proto_init() }\n" + protobufInitBody("evil().PkgPath()"), false},
		} {
			faults := bindingFaults(t, "shop.pb.go", []byte(c.source))
			if allowed := len(faults) == 0; allowed != c.allowed {
				t.Errorf("%s: faults %v, want allowed %v", c.name, faults, c.allowed)
			}
		}
	})
}

// protobufInitBody gives a file_shop_proto_init in the form that protoc-gen-go writes, with the statement extra
// before the type builder when it is not empty.
func protobufInitBody(extra string) string {
	return "func file_shop_proto_init() {\n\tif File_shop_proto != nil {\n\t\treturn\n\t}\n\tfile_common_proto_init()\n" +
		"\tfile_shop_proto_msgTypes[1].OneofWrappers = []any{(*Order_Sku)(nil)}\n\t" + extra + "\n\ttype x struct{}\n" +
		"\tout := protoimpl.TypeBuilder{\n\t\tFile: protoimpl.DescBuilder{\n\t\t\tGoPackagePath: reflect.TypeOf(x{}).PkgPath(),\n" +
		"\t\t\tRawDescriptor: unsafe.Slice(unsafe.StringData(file_shop_proto_rawDesc), len(file_shop_proto_rawDesc)),\n\t\t},\n" +
		"\t\tGoTypes: file_shop_proto_goTypes,\n\t}.Build()\n\tFile_shop_proto = out.File\n}\n"
}

func TestPackageStartCalls(t *testing.T) {
	t.Run("ST-GEN-INV14-T A package-level var whose value calls a function that a generator does not write there is refused, and a listed call or a conversion is not", func(t *testing.T) {
		header := "// Code generated by stego. DO NOT EDIT.\n\npackage v1\n\nimport (\n\t\"errors\"\n\t\"os\"\n\t\"regexp\"\n\n\t\"github.com/jsell-rh/stego/registry/runtime/command\"\n\tdata \"github.com/example/service/out/internal/api/data\"\n)\n\n"
		for _, c := range []struct {
			name, source string
			allowed      bool
		}{
			{"a var _ that calls a function of the package", header + "var _ = register()\nfunc register() bool { return true }\n", false},
			{"a named var that calls a function of the package", header + "var hidden = register()\nfunc register() bool { return true }\n", false},
			{"a var of two names whose second value calls", header + "var a, _ = 1, register()\nfunc register() bool { return true }\n", false},
			{"a var in a group whose value calls inside a literal", header + "var (\n\tlist = []bool{register()}\n)\nfunc register() bool { return true }\n", false},
			{"a var that calls a function of another package", header + "var set = os.Setenv(\"A\", \"b\")\n", false},
			{"a var that calls a parenthesized function", header + "var _ = (register)()\nfunc register() bool { return true }\n", false},
			{"a var that calls a parenthesized function of another package", header + "var _ = (os.Setenv)(\"A\", \"b\")\n", false},
			{"a var that calls a function of the runtime that no generator writes there", header + "var run = command.Run()\n", false},
			{"a var _ that converts nil to a pointer type", header + "type T struct{}\ntype I interface{}\nvar _ I = (*T)(nil)\n", true},
			{"a var that converts a number to a type of the file", header + "type Edition int32\nvar Default_Edition = Edition(1)\nvar goTypes = []any{(Edition)(0)}\n", true},
			{"the listed calls of the generators", header + "var errFields = errors.New(\"x\")\nvar name = regexp.MustCompile(\"x\")\nvar entity = command.Must(command.NewEntity(\"x\"))\nvar view = must(data.EntityOrderView())\nvar types = make([]int, 2)\nvar call = func() bool { return os.Getenv(\"A\") == \"\" }\n", true},
		} {
			faults := bindingFaults(t, "shop.go", []byte(c.source))
			if allowed := len(faults) == 0; allowed != c.allowed {
				t.Errorf("%s: faults %v, want allowed %v", c.name, faults, c.allowed)
			}
		}
	})
}
