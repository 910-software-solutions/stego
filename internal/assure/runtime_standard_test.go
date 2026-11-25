package assure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The tests of the rules of ST-STD-RUNTIME (the runtime library), of ST-STD-TRUST-R05 (delivery to a GitOps tool)
// and of ST-STD-TXN-R14 (rows are owned by the lock that claimed them). Each reads the Go files of the tree at HEAD
// that are not test files and not under a testdata folder, so run them with -count=1 after a commit.

// runtimeModule is the folder of the runtime library's Go module.
const runtimeModule = "registry/runtime"

// connectorPackage is the one package of the runtime module that may import a database driver (ST-STD-RUNTIME-R06).
const connectorPackage = runtimeModule + "/pgconn"

// sourceFile is a Go file that is not a test file, parsed.
type sourceFile struct {
	name string
	data []byte
	file *ast.File
}

// goSources parses each Go file of the tree under the folder that is not a test file and not under a testdata
// folder. A file that does not parse fails the test.
func goSources(t *testing.T, tree Tree, folder string) []sourceFile {
	t.Helper()
	var files []sourceFile
	for _, name := range tree.paths() {
		if !strings.HasPrefix(name, folder+"/") || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/testdata/") {
			continue
		}
		data, _ := tree.File(name)
		file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		full, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
		if err == nil {
			file = full
		}
		files = append(files, sourceFile{name: name, data: data, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("the tree holds no Go source file under %s", folder)
	}
	return files
}

// importsOf gives the import paths of the file.
func importsOf(file *ast.File) []string {
	var paths []string
	for _, spec := range file.Imports {
		if value, err := strconv.Unquote(spec.Path.Value); err == nil {
			paths = append(paths, value)
		}
	}
	return paths
}

// internalRegistration is the form of the one init that ST-STD-RUNTIME-R01 admits: a call of Register of an
// internal package of the runtime module, which gives that package a setter or an accessor of the registering
// package's own type, so a test of the module can reach a field that the code a service generates cannot set. It
// registers nothing by name, struct tag or a service's value.
func internalRegistration(function *ast.FuncDecl, imports map[string]string) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	statement, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Register" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && strings.HasPrefix(imports[pkg.Name], "github.com/jsell-rh/stego/registry/runtime/internal/")
}

// tableFill is the other form of init that ST-STD-RUNTIME-R01 admits: one loop that fills a package-level map from
// a string constant of the file, such as the reserved words of PostgreSQL. It reads no name of a service.
func tableFill(function *ast.FuncDecl) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	loop, ok := function.Body.List[0].(*ast.RangeStmt)
	if !ok {
		return false
	}
	call, ok := loop.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Fields" || len(call.Args) != 1 {
		return false
	}
	_, literal := call.Args[0].(*ast.BasicLit)
	return literal
}

// processGlobals are the registries and the settings of the whole process that a library must not use on its own:
// the default HTTP mux, the default logger and its output, the database driver and gob registries, the global
// providers of OpenTelemetry, the environment and signals (review of PR #504, M4). The flag package outside a
// FlagSet of its own is one too.
var processGlobals = map[string]bool{
	"net/http.Handle": true, "net/http.HandleFunc": true, "net/http.DefaultServeMux": true, "net/http.DefaultClient": true,
	"log/slog.SetDefault": true, "log.SetOutput": true, "log.SetFlags": true, "log.SetPrefix": true,
	"database/sql.Register": true, "encoding/gob.Register": true, "encoding/gob.RegisterName": true,
	"go.opentelemetry.io/otel.SetTracerProvider": true, "go.opentelemetry.io/otel.SetMeterProvider": true,
	"go.opentelemetry.io/otel.SetTextMapPropagator": true, "go.opentelemetry.io/otel.SetErrorHandler": true,
	"os.Setenv": true, "os.Unsetenv": true, "os/signal.Notify": true, "expvar.Publish": true,
}

// admittedGlobals are the uses of a setting of the whole process that ST-STD-RUNTIME-R01 admits, by file and name,
// each because the service's own code calls the function that holds it: service.UseJSONLog sets the default logger
// when the generated NewService and Migrate call it. A new use fails the test.
var admittedGlobals = map[string][]string{
	runtimeModule + "/service/service.go": {"log/slog.SetDefault"},
}

// admittedGlobal tells if the use is one of admittedGlobals.
func admittedGlobal(source sourceFile, name string) bool {
	return slices.Contains(admittedGlobals[source.name], name)
}

// frameworkFaults gives each fault of ST-STD-RUNTIME-R01 in the runtime module: an init that is not one of the two
// admitted forms, a read of a struct tag, a lookup of a type or a function by name (plugin, reflect), and a call of
// a function of the process's global registries (flag, expvar, the default HTTP mux).
func frameworkFaults(files []sourceFile) []string {
	var faults []string
	for _, source := range files {
		imports := map[string]string{}
		for _, spec := range source.file.Imports {
			value, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(value)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = value
		}
		for _, value := range importsOf(source.file) {
			if value == "plugin" || value == "expvar" {
				faults = append(faults, fmt.Sprintf("%s imports %s, which registers or discovers by name", source.name, value))
			}
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "init" && !internalRegistration(n, imports) && !tableFill(n) {
					faults = append(faults, fmt.Sprintf("%s has an init that registers or discovers something", source.name))
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok {
					imported := imports[pkg.Name]
					name := imported + "." + n.Sel.Name
					if processGlobals[name] || imported == "flag" && !slices.Contains([]string{"NewFlagSet", "ContinueOnError", "ExitOnError", "PanicOnError", "FlagSet", "ErrHelp", "Value", "Flag"}, n.Sel.Name) {
						if !admittedGlobal(source, name) {
							faults = append(faults, fmt.Sprintf("%s uses %s, a registry or a setting of the whole process", source.name, name))
						}
					}
				}
				if n.Sel.Name == "Tag" || n.Sel.Name == "Lookup" && strings.Contains(fmt.Sprint(n.X), "Tag") {
					faults = append(faults, fmt.Sprintf("%s reads a struct tag", source.name))
				}
			}
			return true
		})
	}
	return faults
}

// driverImport tells if an import path is a database driver or its connection package.
var driverImport = regexp.MustCompile(`^(github\.com/jackc/pgx(/v[0-9]+)?(/|$)|github\.com/lib/pq$|github\.com/go-sql-driver/|github\.com/mattn/go-sqlite3$|modernc\.org/sqlite$)`)

// A store transaction that a function of the runtime module takes: a parameter whose type names Tx, other than
// *sql.Tx, an interface that the package declares, or a type of the module's own packages.
func foreignTransactions(files []sourceFile) []string {
	var faults []string
	for _, source := range files {
		imports := map[string]string{}
		for _, spec := range source.file.Imports {
			value, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(value)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = value
		}
		for _, decl := range source.file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				expr := field.Type
				if star, ok := expr.(*ast.StarExpr); ok {
					expr = star.X
				}
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := selector.X.(*ast.Ident)
				if ok && strings.HasSuffix(selector.Sel.Name, "Tx") && driverImport.MatchString(imports[pkg.Name]) {
					faults = append(faults, fmt.Sprintf("%s: %s takes the transaction %s.%s of a driver, not *sql.Tx", source.name, function.Name.Name, pkg.Name, selector.Sel.Name))
				}
			}
		}
	}
	return faults
}

// driverFaults gives each fault of ST-STD-RUNTIME-R06 in the runtime module: a file outside the connector package
// that imports a database driver, and an exported function that takes the transaction type of a driver.
func driverFaults(files []sourceFile, admitted func(name, imported string) bool) []string {
	var faults []string
	for _, source := range files {
		for _, imported := range importsOf(source.file) {
			if driverImport.MatchString(imported) && !admitted(source.name, imported) {
				faults = append(faults, fmt.Sprintf("%s imports the database driver package %s outside %s", source.name, imported, connectorPackage))
			}
		}
	}
	return append(faults, foreignTransactions(files)...)
}

// libraryFiles gives the Go source files of the runtime library: those of the runtime module, apart from its test
// harnesses pgtest and kafkatest, which are tooling (ST-SUP-TOOL14, ST-SUP-TOOL15) that only tests import.
func libraryFiles(t *testing.T, tree Tree) []sourceFile {
	t.Helper()
	return slices.DeleteFunc(goSources(t, tree, runtimeModule), func(source sourceFile) bool {
		return strings.HasPrefix(source.name, runtimeModule+"/internal/pgtest/") || strings.HasPrefix(source.name, runtimeModule+"/kafkatest/")
	})
}

func TestRuntimeLibraryStandard(t *testing.T) {
	_, tree := readHead(t)
	files := libraryFiles(t, tree)

	t.Run("ST-STD-RUNTIME-R01-T01 The runtime library registers and discovers nothing by name, struct tag or init", func(t *testing.T) {
		report(t, "ST-STD-RUNTIME-R01", frameworkFaults(files))
	})

	t.Run("ST-STD-RUNTIME-R02-T01 No file of the runtime library imports reflect or unsafe", func(t *testing.T) {
		var faults []string
		for _, source := range files {
			for _, imported := range importsOf(source.file) {
				if imported == "reflect" || imported == "unsafe" {
					faults = append(faults, fmt.Sprintf("%s imports %s", source.name, imported))
				}
			}
		}
		report(t, "ST-STD-RUNTIME-R02", faults)
	})

	t.Run("ST-STD-RUNTIME-R06-T01 Only the connector package of the runtime library imports a database driver", func(t *testing.T) {
		report(t, "ST-STD-RUNTIME-R06", driverFaults(files, func(name, imported string) bool {
			return path.Dir(name) == connectorPackage || strings.HasPrefix(path.Dir(name), connectorPackage+"/") || knownDriverImport(name, imported)
		}))
	})
}

// knownDriverImport tells if the import is one that the record of the runtime package names as a gap of
// ST-STD-RUNTIME-R06 not closed yet: ST-SUP-RP04-LF08 (registry/runtime/pgconn/connect.go) is not written, so the
// tenant runtime's connector and session guard still sit in package tenant, and the command runtime reads the
// error type of pgconn. The list holds each file and import by name, so a new driver import fails the test.
func knownDriverImport(name, imported string) bool {
	known := map[string][]string{
		"registry/runtime/tenant/owner.go":     {"github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgconn", "github.com/jackc/pgx/v5/stdlib"},
		"registry/runtime/tenant/provision.go": {"github.com/jackc/pgx/v5"},
		"registry/runtime/tenant/tx.go":        {"github.com/jackc/pgx/v5"},
		"registry/runtime/command/handle.go":   {"github.com/jackc/pgx/v5/pgconn"},
	}
	return slices.Contains(known[name], imported)
}

// importNames gives the name by which the file refers to each import path: its alias, or its last element.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(value)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = value
	}
	return names
}

// gitopsFaults gives each fault of ST-STD-TRUST-R05 in a file that delivers to a GitOps tool: an import of a network,
// process, gRPC, Kubernetes or Argo CD client, by any alias, a read of the environment, where a credential of the tool
// would be, and a read of a file, where a credential or a kube configuration would be (review of PR #504, L6). The
// adapter writes its files; the caller reads the declaration and passes its values.
func gitopsFaults(source sourceFile) []string {
	var faults []string
	for _, imported := range importsOf(source.file) {
		switch {
		case imported == "net/http", imported == "net", imported == "os/exec", imported == "syscall", strings.HasPrefix(imported, "google.golang.org/grpc"),
			strings.HasPrefix(imported, "k8s.io/client-go"), strings.HasPrefix(imported, "sigs.k8s.io/controller-runtime"),
			strings.HasPrefix(imported, "github.com/argoproj/"):
			faults = append(faults, fmt.Sprintf("%s imports %s, so it can call the tool or the cluster", source.name, imported))
		}
	}
	names := importNames(source.file)
	reads := map[string]bool{"os.Getenv": true, "os.LookupEnv": true, "os.Environ": true, "os.ReadFile": true, "os.Open": true,
		"os.OpenFile": true, "io/ioutil.ReadFile": true, "os.UserHomeDir": true}
	ast.Inspect(source.file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := selector.X.(*ast.Ident); ok && reads[names[pkg.Name]+"."+selector.Sel.Name] {
					faults = append(faults, fmt.Sprintf("%s calls %s.%s, which reads the environment or a file where a credential of the tool would be", source.name, names[pkg.Name], selector.Sel.Name))
				}
			}
		}
		return true
	})
	return faults
}

// gitopsAdapter is the technical record of the adapter that delivers to Argo CD (ST-INT03).
const gitopsAdapter = "ST-INT03-AD"

func TestGitOpsDeliveryWritesFilesOnly(t *testing.T) {
	spec, tree := readHead(t)
	var files []string
	for _, record := range spec.Technical {
		if record.ID == gitopsAdapter {
			files = append(append(files, record.File), record.Helpers...)
		}
	}
	if len(files) == 0 {
		t.Fatalf("the spec at HEAD has no adapter %s", gitopsAdapter)
	}
	t.Run("ST-STD-TRUST-R05-T01 The Argo CD adapter writes files and calls no API, runs no command and reads no credential", func(t *testing.T) {
		var faults []string
		for _, name := range files {
			data, held := tree.File(name)
			if !held {
				faults = append(faults, name+" is not in the tree")
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s does not parse: %v", name, err)
			}
			faults = append(faults, gitopsFaults(sourceFile{name: name, data: data, file: file})...)
		}
		report(t, "ST-STD-TRUST-R05", faults)
	})
}

// consoleSink is the variable that turns the console sink of the domain probes on (ST-SUP-RP12-LC16).
const consoleSink = "STEGO_PROBE_CONSOLE"

func TestDeploymentLeavesTheConsoleSinkOff(t *testing.T) {
	_, tree := readHead(t)
	t.Run("ST-STD-OBS-R03-T01 No Kubernetes base or GitOps object of a committed example sets the console sink", func(t *testing.T) {
		var faults []string
		read := 0
		for _, name := range tree.paths() {
			parts := strings.Split(name, "/")
			if len(parts) < 4 || parts[0] != "examples" || parts[2] != "deploy" || parts[3] != "base" && parts[3] != "components" && parts[3] != "argocd" {
				continue
			}
			if ext := path.Ext(name); ext != ".yaml" && ext != ".yml" {
				continue
			}
			read++
			data, _ := tree.File(name)
			if strings.Contains(string(data), consoleSink) {
				faults = append(faults, name+" names "+consoleSink)
			}
		}
		if read == 0 {
			faults = append(faults, "the tree holds no deploy/base file of an example, so the test reads nothing")
		}
		report(t, "ST-STD-OBS-R03", faults)
	})
}
