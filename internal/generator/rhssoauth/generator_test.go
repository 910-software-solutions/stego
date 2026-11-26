package rhssoauth

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	goparser "go/parser"
	"go/token"
	gotypes "go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
)

func TestGenerateSharedRuntime(t *testing.T) {
	for _, ns := range []string{"", "internal/auth", "pkg/authn"} {
		ctx := gen.Context{OutputNamespace: ns, ServiceName: "test-api", BasePath: "/api/v1", ErrorTypeBase: "https://errors.example/", ComponentConfig: map[string]any{"issuer": "https://issuer.example", "audience": "test", "jwk_cert_file": "/etc/jwks.json", "public_paths": []any{"/health", "/api/v1/ready"}}}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 7 {
			t.Fatalf("got %d files", len(files))
		}
		if !wiring.ConstructorReturnsError[0] || wiring.ConstructorDeferCalls[0] != "Stop()" || wiring.MiddlewareWrapExpr != "%s.Build()(%s)" {
			t.Fatal("startup errors or shutdown are not wired")
		}
		if resources := wiring.ConstructorResources[0]; len(resources) != 1 || resources[0] != gen.ServiceContext {
			t.Fatal("SSO startup does not use the service context")
		}
		if len(wiring.GoModRequires) != 1 || wiring.GoModRequires["github.com/golang-jwt/jwt/v5"] == "" {
			t.Fatal("SSO does not use the common JWT dependency")
		}
		source := map[string]string{}
		for _, f := range files {
			if _, err := format.Source(f.Bytes()); err != nil {
				t.Fatal(err)
			}
			source[f.Path] = string(f.Content)
		}
		prefix := ns
		if prefix == "" {
			prefix = "internal/auth"
		}
		for _, text := range []string{"/api/v1/health", "/api/v1/ready", "/api/v1/openapi", "https://issuer.example", "/etc/jwks.json"} {
			if !strings.Contains(source[prefix+"/sso.go"], text) {
				t.Fatalf("missing SSO configuration %s", text)
			}
		}
		if !strings.Contains(source[prefix+"/middleware.go"], "TEST-AUT-001") {
			t.Fatal("SSO error code changed")
		}
	}
}

func TestSSOConfigurationValidation(t *testing.T) {
	for _, config := range []map[string]any{
		{"issuer": 42}, {"issuer": "http://issuer.example"}, {"audience": " test"},
		{"jwk_cert_url": "https://user:secret@example.com/keys"}, {"jwk_cert_file": false},
		{"public_paths": "/private"}, {"public_paths": []any{42}}, {"public_paths": []any{"/private/../admin"}},
		{"public_paths": []any{"private"}}, {"public_paths": []any{"/private?skip=true"}},
	} {
		if _, _, err := new(Generator).Generate(gen.Context{ComponentConfig: config}); err == nil {
			t.Fatalf("accepted invalid config: %#v", config)
		}
	}
	paths, err := publicPaths(gen.Context{ComponentConfig: map[string]any{"public_paths": []any{}}})
	if err != nil || len(paths) != 1 || paths[0] != "/openapi" {
		t.Fatal("an empty path list must not restore defaults")
	}
	paths, err = publicPaths(gen.Context{BasePath: "/api", ComponentConfig: map[string]any{"public_paths": []any{"/api2/health"}}})
	if err != nil || paths[0] != "/api/api2/health" {
		t.Fatal("base path requires a segment boundary")
	}
}

func TestRuntimeAuth(t *testing.T) {
	t.Run("ST-SUP-RC21-OC15-T02 rh-sso-auth on the runtime data path takes the Red Hat SSO form and claims only", func(t *testing.T) {
		config := func(change map[string]any) gen.Context {
			c := map[string]any{"issuer": "https://sso.redhat.com/auth/realms/redhat-external", "audience": "orders-api",
				"jwk_cert_url": defaultKeysURL, "public_paths": []any{"/healthcheck", "/metrics"}}
			for k, v := range change {
				if v == nil {
					delete(c, k)
				} else {
					c[k] = v
				}
			}
			return gen.Context{ComponentConfig: c}
		}
		g := &Generator{}
		auth, err := g.RuntimeAuth(config(map[string]any{"jwk_ca_file": "/etc/sso/ca.pem"}))
		if err != nil {
			t.Fatal(err)
		}
		if auth.Issuer != "https://sso.redhat.com/auth/realms/redhat-external" || auth.SubjectClaim != "sub" || auth.TenantClaim != "org_id" ||
			len(auth.Algorithms) != 1 || auth.Algorithms[0] != "RS256" || auth.CAFile != "/etc/sso/ca.pem" || auth.Audience != "orders-api" {
			t.Fatalf("the settings are %+v", auth)
		}
		for name, change := range map[string]map[string]any{
			"an issuer of another form": {"issuer": "https://sso.redhat.com/realms/redhat-external"},
			"an issuer with a subpath":  {"issuer": "https://sso.redhat.com/auth/realms/redhat-external/x"},
			"an http issuer":            {"issuer": "http://sso.redhat.com/auth/realms/redhat-external"},
			"a key file":                {"jwk_cert_file": "/etc/sso/certs.json"},
			"another key URL":           {"jwk_cert_url": "https://keys.example.com/certs"},
			"other public paths":        {"public_paths": []any{"/healthcheck"}},
			"no audience":               {"audience": nil},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := g.RuntimeAuth(config(change)); err == nil || !strings.Contains(err.Error(), "rh-sso-auth") {
					t.Fatalf("rh-sso-auth with %s gave %v, want a refusal that names rh-sso-auth", name, err)
				}
			})
		}
	})
}

// ssoOwnFiles gives the Go files that rh-sso-auth emits beside the shared runtime of jwt-auth, which holds the one
// signature verifier, the one key source and its HTTP client, by path, with the whole set of emitted files.
func ssoOwnFiles(t *testing.T, tracing bool) (map[string]string, map[string]string) {
	t.Helper()
	ctx := gen.Context{OutputNamespace: "internal/auth", ServiceName: "test-api", BasePath: "/api/v1", ErrorTypeBase: "https://errors.example/",
		ComponentConfig: map[string]any{"issuer": "https://issuer.example", "audience": "test", "jwk_cert_file": "/etc/jwks.json"}}
	if tracing {
		ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces = "example.com/sso", "out", map[string]string{"otel-tracing": "tracing"}
	}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shared := ctx
	shared.ServiceName = "test"
	common, _, err := new(jwtauth.Generator).Generate(shared)
	if err != nil {
		t.Fatal(err)
	}
	runtime := map[string]bool{}
	for _, f := range common {
		runtime[f.Path] = true
	}
	own, all := map[string]string{}, map[string]string{}
	for _, f := range files {
		all[f.Path] = string(f.Content)
		if !runtime[f.Path] {
			own[f.Path] = string(f.Content)
		}
	}
	if len(own) == 0 {
		t.Fatal("rh-sso-auth emits no file of its own")
	}
	return own, all
}

// ssoPackage type-checks the generated auth package, the shared runtime of jwt-auth and the files of rh-sso-auth,
// with go/types, and gives the SSO files, the type information and the package. The standard library comes from
// its source in GOROOT, and github.com/golang-jwt/jwt/v5 from its source in the module cache, so no network is used.
func ssoPackage(t *testing.T, tracing bool) (map[string]*ast.File, *gotypes.Info, *gotypes.Package) {
	t.Helper()
	own, all := ssoOwnFiles(t, tracing)
	fset := token.NewFileSet()
	var files []*ast.File
	mine := map[string]*ast.File{}
	for name, text := range all {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := goparser.ParseFile(fset, name, text, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		if _, ok := own[name]; ok {
			mine[name] = file
		}
	}
	info := &gotypes.Info{Uses: map[*ast.Ident]gotypes.Object{}, Defs: map[*ast.Ident]gotypes.Object{}, Selections: map[*ast.SelectorExpr]*gotypes.Selection{}, Types: map[ast.Expr]gotypes.TypeAndValue{}}
	config := gotypes.Config{Importer: localImporter(t, fset)}
	pkg, err := config.Check("example.com/sso/out/internal/auth", fset, files, info)
	if err != nil {
		t.Fatalf("the emitted auth package does not type-check: %v", err)
	}
	return mine, info, pkg
}

// localImporter imports the standard library from GOROOT and github.com/golang-jwt/jwt/v5 from its source in the
// module cache, at the version that jwt-auth requires.
func localImporter(t *testing.T, fset *token.FileSet) gotypes.Importer {
	t.Helper()
	std := importer.ForCompiler(fset, "source", nil)
	// go mod download gives the folder of the module at the version jwt-auth requires, from the module cache when it
	// holds it, as the generated-module tests of this package fetch it.
	download := exec.Command("go", "mod", "download", "-json", "github.com/golang-jwt/jwt/v5@"+jwtVersion(t))
	download.Dir = t.TempDir()
	download.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	if err := os.WriteFile(filepath.Join(download.Dir, "go.mod"), []byte("module example.com/download\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := download.Output()
	if err != nil {
		t.Fatalf("go mod download of github.com/golang-jwt/jwt/v5: %v", err)
	}
	var module struct{ Dir string }
	if err := json.Unmarshal(out, &module); err != nil || module.Dir == "" {
		t.Fatalf("go mod download gave no folder of github.com/golang-jwt/jwt/v5: %v\n%s", err, out)
	}
	jwtDir := module.Dir
	var jwt *gotypes.Package
	var tracing *gotypes.Package
	return importerFunc(func(importPath string) (*gotypes.Package, error) {
		if importPath == tracingPath {
			// The tracing package of otel-tracing, as far as the SSO files reach it: the type Runtime with its method
			// Context, and TraceAuthKeyRefresh, with the signatures that otel-tracing emits.
			if tracing == nil {
				stub, err := goparser.ParseFile(fset, "tracing.go", tracingStub, 0)
				if err != nil {
					return nil, err
				}
				tracing, err = (&gotypes.Config{Importer: std}).Check(tracingPath, fset, []*ast.File{stub}, nil)
				if err != nil {
					return nil, err
				}
			}
			return tracing, nil
		}
		if importPath != "github.com/golang-jwt/jwt/v5" {
			return std.Import(importPath)
		}
		if jwt != nil {
			return jwt, nil
		}
		names, err := filepath.Glob(filepath.Join(jwtDir, "*.go"))
		if err != nil || len(names) == 0 {
			return nil, fmt.Errorf("github.com/golang-jwt/jwt/v5 has no Go file at %s", jwtDir)
		}
		var files []*ast.File
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := goparser.ParseFile(fset, name, nil, 0)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
		}
		config := gotypes.Config{Importer: std}
		jwt, err = config.Check(importPath, fset, files, nil)
		return jwt, err
	})
}

// jwtVersion gives the version of github.com/golang-jwt/jwt/v5 that the shared runtime of jwt-auth requires.
func jwtVersion(t *testing.T) string {
	t.Helper()
	_, wiring, err := new(jwtauth.Generator).Generate(gen.Context{OutputNamespace: "internal/auth", ServiceName: "test", BasePath: "/api/v1", ErrorTypeBase: "https://errors.example/",
		ComponentConfig: map[string]any{"issuer": "https://issuer.example", "audience": "test", "jwk_cert_file": "/etc/jwks.json"}})
	if err != nil || wiring.GoModRequires["github.com/golang-jwt/jwt/v5"] == "" {
		t.Fatalf("jwt-auth names no version of github.com/golang-jwt/jwt/v5: %v", err)
	}
	return wiring.GoModRequires["github.com/golang-jwt/jwt/v5"]
}

// tracingPath is the import path of the tracing package in the variant with the otel-tracing peer.
const tracingPath = "example.com/sso/out/tracing"

// tracingStub states the part of the tracing package that the SSO files and the shared runtime use.
const tracingStub = "package tracing\nimport \"context\"\ntype Runtime struct{}\nfunc (r *Runtime) Context(ctx context.Context) context.Context { return ctx }\nfunc TraceAuthKeyRefresh(ctx context.Context, source string) (context.Context, func(string)) { return ctx, func(string) {} }\n"

type importerFunc func(string) (*gotypes.Package, error)

func (f importerFunc) Import(importPath string) (*gotypes.Package, error) { return f(importPath) }

// ssoValues are the types of the shared runtime that the SSO files may build and read: the configuration they give
// the one verifier, and the identity it gives back. Every other type of the shared runtime, such as JWKSVerifier or
// Verifier, may not be built, converted to, embedded or read.
var ssoValues = map[string]bool{"JWKSConfig": true, "Config": true, "Identity": true}

// ssoAllowed are the objects of the shared runtime and of jwt that the SSO files may use, by package and name: the
// one verifier with its configuration and identity, the helpers that read settings and write a denial, and the
// decode of the claims of a token that the verifier checked. A method is named by its receiver type and its name.
var ssoAllowed = map[string]bool{
	"auth.NewJWKSVerifier": true, "auth.JWKSConfig": true, "auth.JWKSVerifier": true, "auth.Config": true, "auth.Identity": true,
	"auth.identityKey": true, "auth.declaredValue": true, "auth.envOrDefault": true, "auth.extractToken": true, "auth.writeAuthError": true,
	"auth.JWKSVerifier.Verify": true, "auth.JWKSVerifier.Stop": true,
	"jwt.MapClaims": true, "jwt.Token": true, "jwt.NewParser": true, "jwt.Parser.ParseUnverified": true,
}

// ssoImports are the packages the SSO files may import, each by its own name, by template variant: none of them
// makes a network client or a task. Only the variant with the otel-tracing peer imports the tracing package.
func ssoImports(tracing bool) map[string]bool {
	imports := map[string]bool{"context": true, "errors": true, "net/http": true, "os": true, "strings": true, "github.com/golang-jwt/jwt/v5": true}
	if tracing {
		imports[tracingPath] = true
	}
	return imports
}

// ssoTypes are the only types that the SSO files may declare, by name and by the kind of their underlying type: the
// handler, the context key and the payload. A type of the SSO files that holds a type of the shared runtime is
// checked apart (holdsShared).
var ssoTypes = map[string]string{"JWTHandler": "struct", "ctxKey": "basic", "Payload": "struct"}

// ssoAssertions are the only types that a type assertion of the SSO files may name; an assertion to an interface
// reaches methods that the files may not name.
var ssoAssertions = map[string]bool{"string": true, "*github.com/golang-jwt/jwt/v5.Token": true, "github.com/golang-jwt/jwt/v5.MapClaims": true}

// objectName gives the name of an object for ssoAllowed: the last element of its package path and its name, with
// the receiver type of a method.
func objectName(object gotypes.Object) string {
	if object.Pkg() == nil {
		return object.Name()
	}
	base := path.Base(object.Pkg().Path())
	if base == "v5" {
		base = "jwt"
	}
	if function, ok := object.(*gotypes.Func); ok {
		if receiver := function.Type().(*gotypes.Signature).Recv(); receiver != nil {
			if n, ok := gotypes.Unalias(derefType(receiver.Type())).(*gotypes.Named); ok {
				return base + "." + n.Obj().Name() + "." + object.Name()
			}
		}
	}
	return base + "." + object.Name()
}

// derefType gives the element type of a pointer, unaliased, and else the type, unaliased.
func derefType(typ gotypes.Type) gotypes.Type {
	typ = gotypes.Unalias(typ)
	if pointer, ok := typ.(*gotypes.Pointer); ok {
		return gotypes.Unalias(pointer.Elem())
	}
	return typ
}

// sharedNamed gives the named type of the shared runtime that a type is, through aliases and a pointer, when it is
// one that the SSO files may not build, convert to or read: a type of the package that the SSO files do not declare
// and that is not in ssoValues.
func sharedNamed(typ gotypes.Type, pkg *gotypes.Package, own func(gotypes.Object) bool) *gotypes.TypeName {
	named, ok := derefType(typ).(*gotypes.Named)
	if !ok || named.Obj().Pkg() != pkg || own(named.Obj()) || ssoValues[named.Obj().Name()] {
		return nil
	}
	return named.Obj()
}

// holdsShared tells whether a type is, or holds through its fields, elements, pointers or signature, a type of the
// shared runtime other than the values.
func holdsShared(typ gotypes.Type, pkg *gotypes.Package, own func(gotypes.Object) bool, seen map[gotypes.Type]bool) bool {
	typ = gotypes.Unalias(typ)
	if seen[typ] {
		return false
	}
	seen[typ] = true
	switch t := typ.(type) {
	case *gotypes.Named:
		if t.Obj().Pkg() == pkg && !own(t.Obj()) {
			return !ssoValues[t.Obj().Name()]
		}
		if t.Obj().Pkg() == pkg {
			return holdsShared(t.Underlying(), pkg, own, seen)
		}
		return false
	case *gotypes.Pointer:
		return holdsShared(t.Elem(), pkg, own, seen)
	case *gotypes.Slice:
		return holdsShared(t.Elem(), pkg, own, seen)
	case *gotypes.Array:
		return holdsShared(t.Elem(), pkg, own, seen)
	case *gotypes.Map:
		return holdsShared(t.Key(), pkg, own, seen) || holdsShared(t.Elem(), pkg, own, seen)
	case *gotypes.Chan:
		return holdsShared(t.Elem(), pkg, own, seen)
	case *gotypes.Struct:
		for i := range t.NumFields() {
			if holdsShared(t.Field(i).Type(), pkg, own, seen) {
				return true
			}
		}
	case *gotypes.Signature:
		for _, tuple := range []*gotypes.Tuple{t.Params(), t.Results()} {
			for i := range tuple.Len() {
				if holdsShared(tuple.At(i).Type(), pkg, own, seen) {
					return true
				}
			}
		}
	}
	return false
}

// underlyingKind gives the kind of an underlying type for ssoTypes.
func underlyingKind(typ gotypes.Type) string {
	switch typ.Underlying().(type) {
	case *gotypes.Struct:
		return "struct"
	case *gotypes.Basic:
		return "basic"
	case *gotypes.Interface:
		return "interface"
	default:
		return fmt.Sprintf("%T", typ.Underlying())
	}
}

// ssoFaults judges the SSO files with go/types, and gives the faults of a verifier and the faults of a client or a
// task apart. The rules are a positive list of both the objects and the shapes of the code:
//   - imports: only ssoImports, each by its own name, and no dot or blank import;
//   - types: only the declarations of ssoTypes, and the only field that holds a shared type is the verifier of
//     JWTHandler, of type *JWKSVerifier; no alias of a shared type, and no other type that is or holds one;
//   - objects: a function, type, variable, constant or method of the shared runtime or of jwt only from ssoAllowed,
//     through any selection, promoted or not; a field of a shared struct only of the values;
//   - values: no composite literal, new or conversion of a shared type other than the values, and a type assertion
//     only to a type of ssoAssertions, never to an interface;
//   - tasks and clients: no go statement, no context.AfterFunc, and no client, transport or request of net/http;
//   - order: the one ParseUnverified only after the error of h.verifier.Verify is checked and returns, in the same
//     block (unverifiedParses).
func ssoFaults(files map[string]*ast.File, info *gotypes.Info, pkg *gotypes.Package, tracing bool) (verifier, client []string) {
	var faults []string
	httpDenied := map[string]bool{"Client": true, "Transport": true, "RoundTripper": true, "DefaultClient": true, "DefaultTransport": true,
		"Get": true, "Head": true, "Post": true, "PostForm": true, "NewRequest": true, "NewRequestWithContext": true}
	imports := ssoImports(tracing)
	own := func(object gotypes.Object) bool {
		for _, file := range files {
			if object.Pos() >= file.Pos() && object.Pos() <= file.End() {
				return true
			}
		}
		return false
	}
	for name, file := range files {
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if !imports[importPath] {
				client = append(client, name+": imports "+importPath)
			}
			if spec.Name != nil && (spec.Name.Name == "." || spec.Name.Name == "_") {
				client = append(client, name+": "+spec.Name.Name+" import of "+importPath)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				client = append(client, name+": starts a goroutine")
			case *ast.TypeSpec:
				object := info.Defs[n.Name]
				if object == nil {
					return true
				}
				if n.Assign != 0 {
					faults = append(faults, name+": declares the alias "+n.Name.Name)
					return true
				}
				if kind, listed := ssoTypes[n.Name.Name]; !listed || kind != underlyingKind(object.Type()) {
					faults = append(faults, name+": declares the type "+n.Name.Name+" ("+underlyingKind(object.Type())+")")
					return true
				}
				if structType, ok := object.Type().Underlying().(*gotypes.Struct); ok {
					for i := range structType.NumFields() {
						field := structType.Field(i)
						if n.Name.Name == "JWTHandler" && field.Name() == "verifier" && !field.Embedded() {
							if pointer, ok := gotypes.Unalias(field.Type()).(*gotypes.Pointer); ok {
								if named, ok := gotypes.Unalias(pointer.Elem()).(*gotypes.Named); ok && named.Obj().Pkg() == pkg && named.Obj().Name() == "JWKSVerifier" {
									continue
								}
							}
						}
						if holdsShared(field.Type(), pkg, own, map[gotypes.Type]bool{}) {
							faults = append(faults, name+": the field "+field.Name()+" of "+n.Name.Name+" holds a shared type")
						}
					}
				}
			case *ast.CompositeLit:
				if tv, ok := info.Types[n]; ok {
					if shared := sharedNamed(tv.Type, pkg, own); shared != nil {
						faults = append(faults, name+": builds a "+shared.Name()+" by hand")
					}
				}
			case *ast.TypeAssertExpr:
				if n.Type == nil {
					return true
				}
				tv, ok := info.Types[n.Type]
				if !ok {
					return true
				}
				if _, isInterface := gotypes.Unalias(tv.Type).Underlying().(*gotypes.Interface); isInterface || !ssoAssertions[gotypes.TypeString(gotypes.Unalias(tv.Type), nil)] {
					faults = append(faults, name+": asserts the type "+gotypes.TypeString(tv.Type, nil))
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
					if tv, ok := info.Types[n.Args[0]]; ok {
						if shared := sharedNamed(tv.Type, pkg, own); shared != nil {
							faults = append(faults, name+": new of "+shared.Name())
						}
					}
				}
				// A conversion: the callee is a type.
				if tv, ok := info.Types[n.Fun]; ok && tv.IsType() {
					if shared := sharedNamed(tv.Type, pkg, own); shared != nil {
						faults = append(faults, name+": converts to "+shared.Name())
					}
				}
			case *ast.SelectorExpr:
				selection, ok := info.Selections[n]
				if !ok {
					return true
				}
				switch selection.Kind() {
				case gotypes.MethodVal, gotypes.MethodExpr:
					if method := selection.Obj(); method.Pkg() != nil && (method.Pkg() == pkg || method.Pkg().Path() == "github.com/golang-jwt/jwt/v5") && !own(method) && !ssoAllowed[objectName(method)] {
						faults = append(faults, name+": "+objectName(method))
					}
				case gotypes.FieldVal:
					if field := selection.Obj(); field.Pkg() == pkg && !own(field) {
						if named, ok := derefType(selection.Recv()).(*gotypes.Named); !ok || (!own(named.Obj()) && !ssoValues[named.Obj().Name()]) {
							faults = append(faults, name+": reads the field "+field.Name()+" of the shared runtime")
						}
					}
				}
			case *ast.Ident:
				object := info.Uses[n]
				if object == nil || object.Pkg() == nil {
					return true
				}
				switch object.Pkg().Path() {
				case "net/http":
					if httpDenied[object.Name()] {
						client = append(client, name+": net/http."+object.Name())
					}
				case "context":
					if object.Name() == "AfterFunc" {
						client = append(client, name+": context.AfterFunc")
					}
				case pkg.Path(), "github.com/golang-jwt/jwt/v5":
					if v, ok := object.(*gotypes.Var); ok && v.IsField() {
						return true
					}
					if !own(object) && !ssoAllowed[objectName(object)] {
						faults = append(faults, name+": "+objectName(object))
					}
				}
			case *ast.BlockStmt:
				faults = append(faults, unverifiedParses(name, n, info, pkg)...)
			case *ast.InterfaceType:
				faults = append(faults, name+": an interface type")
			case *ast.TypeSwitchStmt:
				faults = append(faults, name+": a type switch")
			case *ast.FuncDecl:
				if n.Type.TypeParams != nil {
					faults = append(faults, name+": the type parameters of "+n.Name.Name)
				}
			case *ast.FuncType:
				if n.TypeParams != nil {
					faults = append(faults, name+": a type parameter list")
				}
			}
			return true
		})
		faults = append(faults, interfaceCalls(name, file, info)...)
		faults = append(faults, verifierFlow(name, file, info, pkg)...)
		faults = append(faults, declarationFaults(name, file, info, pkg, tracing)...)
	}
	return faults, client
}

// ssoInterfaceCalls are the only methods that the SSO files may call on a receiver of an interface type, by the
// interface and the method: the ones the template uses, and Error of error. A call on another interface could reach a
// method that the allow list of objects does not name, such as read or refresh of the key source.
var ssoInterfaceCalls = map[string]bool{"context.Context.Value": true, "net/http.Handler.ServeHTTP": true, "error.Error": true}

// interfaceCalls gives each method call of the file whose receiver has an interface type, unless ssoInterfaceCalls
// names it.
func interfaceCalls(name string, file *ast.File, info *gotypes.Info) []string {
	var faults []string
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		selection, ok := info.Selections[selector]
		if !ok || selection.Kind() != gotypes.MethodVal || !gotypes.IsInterface(selection.Recv()) {
			return true
		}
		if key := gotypes.TypeString(gotypes.Unalias(selection.Recv()), nil) + "." + selector.Sel.Name; !ssoInterfaceCalls[key] {
			faults = append(faults, name+": calls "+key+" on an interface")
		}
		return true
	})
	return faults
}

// verifierFlow holds the flow of the one verifier in the file: it enters only through NewJWKSVerifier, into a local
// variable that only the composite literal of JWTHandler takes, under the key verifier; and the field
// JWTHandler.verifier is used only as h.verifier.Verify(...) or h.verifier.Stop(), called on the spot. Any other use
// of a value of a shared type other than the configuration and the identity is a fault: an assignment, an argument,
// a result, a capture, a conversion, a comparison, a method value or a field of another type. This also refuses a
// conversion of the verifier to any or to an interface.
func verifierFlow(name string, file *ast.File, info *gotypes.Info, pkg *gotypes.Package) []string {
	var faults []string
	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parent[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	// shared tells whether a type is a type of the shared runtime that the SSO files may not hold freely: not one
	// that the SSO files declare, and not a value of ssoValues.
	shared := func(typ gotypes.Type) bool {
		named, ok := derefType(typ).(*gotypes.Named)
		if !ok || named.Obj().Pkg() != pkg || ssoValues[named.Obj().Name()] {
			return false
		}
		_, declared := ssoTypes[named.Obj().Name()]
		return !declared
	}
	// inHandlerLiteral tells whether a node is the key or the value of the element verifier of a composite literal of
	// JWTHandler.
	inHandlerLiteral := func(node ast.Node, asKey bool) bool {
		element, ok := parent[node].(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		key, ok := element.Key.(*ast.Ident)
		if !ok || key.Name != "verifier" || (asKey && element.Key != node) || (!asKey && element.Value != node) {
			return false
		}
		literal, ok := parent[element].(*ast.CompositeLit)
		if !ok {
			return false
		}
		tv, ok := info.Types[literal]
		if !ok {
			return false
		}
		named, ok := derefType(tv.Type).(*gotypes.Named)
		return ok && named.Obj().Name() == "JWTHandler"
	}
	// calledMethod tells whether a selector h.verifier is the receiver of a call of Verify or Stop on the spot.
	calledMethod := func(field *ast.SelectorExpr) bool {
		method, ok := parent[field].(*ast.SelectorExpr)
		if !ok || method.X != field || (method.Sel.Name != "Verify" && method.Sel.Name != "Stop") {
			return false
		}
		call, ok := parent[method].(*ast.CallExpr)
		return ok && call.Fun == method
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			selection, ok := info.Selections[n]
			if !ok || selection.Kind() != gotypes.FieldVal || !shared(selection.Obj().Type()) {
				return true
			}
			// The receiver is the receiver object of the method that holds the use, as go/types resolves it, never
			// another variable that has the name h.
			receiver, isIdent := n.X.(*ast.Ident)
			if n.Sel.Name != "verifier" || !isIdent || !isReceiver(info.Uses[receiver], pkg) || !calledMethod(n) {
				faults = append(faults, name+": uses the verifier other than h.verifier.Verify or h.verifier.Stop called on the spot")
			}
			return false
		case *ast.Ident:
			object := info.Uses[n]
			if object == nil {
				object = info.Defs[n]
			}
			v, ok := object.(*gotypes.Var)
			if !ok || !shared(v.Type()) || (v.Pkg() == pkg && v.Parent() == pkg.Scope() && ssoAllowed[objectName(v)]) {
				return true
			}
			switch {
			case v.IsField() && info.Defs[n] == v && n.Name == "verifier":
				// The declaration of the field JWTHandler.verifier; the type rule holds it to *JWKSVerifier.
			case v.IsField() && inHandlerLiteral(n, true):
			case !v.IsField() && info.Defs[n] == v:
				// The definition of the local verifier: its value must come from NewJWKSVerifier, in a := statement.
				made := false
				if assign, isAssign := parent[n].(*ast.AssignStmt); isAssign && len(assign.Rhs) == 1 {
					if call, isCall := assign.Rhs[0].(*ast.CallExpr); isCall {
						if callee, isIdent := call.Fun.(*ast.Ident); isIdent && callee.Name == "NewJWKSVerifier" && info.Uses[callee] != nil && info.Uses[callee].Pkg() == pkg {
							made = true
						}
					}
				}
				if !made {
					faults = append(faults, name+": holds a verifier that NewJWKSVerifier did not make")
				}
			case !v.IsField() && inHandlerLiteral(n, false):
			default:
				faults = append(faults, name+": uses the verifier "+n.Name+" other than in the handler it builds")
			}
		}
		return true
	})
	return faults
}

// ssoFunctions are the only functions and methods that the SSO files may declare, by file and name, as the template
// gives them: the three constructors of the handler (NewJWTHandlerWithTelemetry only in the variant with the
// otel-tracing peer), its methods Build and Stop, and the helpers of context.go. A method is named by its receiver
// type. Another declaration could hold a second verifier, such as a handler made per request.
func ssoFunctions(tracing bool) map[string]bool {
	functions := map[string]bool{"sso.go NewJWTHandler": true, "sso.go NewJWTHandlerWithContext": true, "sso.go JWTHandler.Build": true, "sso.go JWTHandler.Stop": true,
		"context.go GetAuthPayload": true, "context.go GetAuthPayloadFromContext": true, "context.go GetUsernameFromContext": true,
		"context.go SetUsernameContext": true, "context.go TokenFromContext": true, "context.go claimString": true, "context.go extractPayload": true}
	if tracing {
		functions["sso.go NewJWTHandlerWithTelemetry"] = true
	}
	return functions
}

// declarationFaults holds the SSO files to ssoFunctions, and holds each call of a constructor of the handler: the
// one call of NewJWTHandlerWithContext is the return expression of NewJWTHandler and of NewJWTHandlerWithTelemetry,
// and no other call of a NewJWTHandler function is in the SSO files. So the files make one verifier, at start, and
// never a second one at run time, per request, on a reload or in a loop.
func declarationFaults(name string, file *ast.File, info *gotypes.Info, pkg *gotypes.Package, tracing bool) []string {
	var faults []string
	allowed := ssoFunctions(tracing)
	base := path.Base(name)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		key := base + " " + function.Name.Name
		if function.Recv != nil && len(function.Recv.List) == 1 {
			if named, ok := derefType(info.Types[function.Recv.List[0].Type].Type).(*gotypes.Named); ok {
				key = base + " " + named.Obj().Name() + "." + function.Name.Name
			}
		}
		if !allowed[key] {
			faults = append(faults, name+": declares "+key)
		}
		// A return statement of NewJWTHandler or NewJWTHandlerWithTelemetry may be one call of
		// NewJWTHandlerWithContext; every other call of a constructor is a fault.
		wrapper := function.Recv == nil && (function.Name.Name == "NewJWTHandler" || function.Name.Name == "NewJWTHandlerWithTelemetry")
		returned := map[*ast.CallExpr]bool{}
		if wrapper {
			// A return of a function literal is the literal's, not the wrapper's: the walk does not go into one.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if _, literal := node.(*ast.FuncLit); literal {
					return false
				}
				if ret, ok := node.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
					if call, ok := ret.Results[0].(*ast.CallExpr); ok {
						returned[call] = true
					}
				}
				return true
			})
		}
		faults = append(faults, runFaults(name, function)...)
		ast.Inspect(function, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			object, ok := info.Uses[callee].(*gotypes.Func)
			if !ok || object.Pkg() != pkg || !strings.HasPrefix(object.Name(), "NewJWTHandler") {
				return true
			}
			if !(returned[call] && object.Name() == "NewJWTHandlerWithContext") {
				faults = append(faults, name+": calls "+object.Name()+" in "+function.Name.Name)
			}
			return true
		})
	}
	// A package variable can keep a constructor or a closure that makes a verifier for a later call. The template
	// declares none; its keys are constants.
	for _, declaration := range file.Decls {
		if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.VAR {
			faults = append(faults, name+": declares a package variable")
		}
	}
	faults = append(faults, literalFaults(name, file)...)
	// A constructor named as a value, not called, could be called elsewhere.
	ast.Inspect(file, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok {
			if object, ok := info.Uses[id].(*gotypes.Func); ok && object.Pkg() == pkg && strings.HasPrefix(object.Name(), "NewJWTHandler") {
				if call, isCall := parentCall(file, id); !isCall || call.Fun != ast.Expr(id) {
					faults = append(faults, name+": uses "+object.Name()+" as a value")
				}
			}
		}
		return true
	})
	return faults
}

// isConstructor tells whether the function declaration is one of the constructors of the handler.
func isConstructor(function *ast.FuncDecl) bool {
	return function.Recv == nil && strings.HasPrefix(function.Name.Name, "NewJWTHandler")
}

// runFaults holds the constructors to run their statements once: no label, goto, for or range in their bodies. The
// one call of NewJWKSVerifier is a direct statement of the body of NewJWTHandlerWithContext, x, err :=
// NewJWKSVerifier(...), never in a function literal, a loop, a branch or a labeled statement. A count of call sites
// is not a count of runs; these rules make the two the same.
func runFaults(name string, function *ast.FuncDecl) []string {
	var faults []string
	if !isConstructor(function) || function.Body == nil {
		return nil
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.LabeledStmt:
			faults = append(faults, name+": a label in "+function.Name.Name)
		case *ast.BranchStmt:
			if n.Tok == token.GOTO {
				faults = append(faults, name+": a goto in "+function.Name.Name)
			}
		case *ast.ForStmt, *ast.RangeStmt:
			faults = append(faults, name+": a loop in "+function.Name.Name)
		}
		return true
	})
	isVerifierCall := func(expression ast.Expr) bool {
		call, ok := expression.(*ast.CallExpr)
		if !ok {
			return false
		}
		callee, ok := call.Fun.(*ast.Ident)
		return ok && callee.Name == "NewJWKSVerifier"
	}
	direct := 0
	for _, statement := range function.Body.List {
		if assign, ok := statement.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && isVerifierCall(assign.Rhs[0]) {
			direct++
		}
	}
	total := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if expression, ok := node.(ast.Expr); ok && isVerifierCall(expression) {
			total++
		}
		return true
	})
	want := 0
	if function.Name.Name == "NewJWTHandlerWithContext" {
		want = 1
	}
	if direct != want || total != want {
		faults = append(faults, fmt.Sprintf("%s: %s calls NewJWKSVerifier %d times, %d as a direct statement of its body, want %d", name, function.Name.Name, total, direct, want))
	}
	return faults
}

// literalFaults refuses each function literal of the SSO files but the two of Build, by their place: the literal that
// Build returns, func(next http.Handler) http.Handler, and the one it returns through http.HandlerFunc, func(w, r).
// A literal elsewhere could hold a verifier and run it many times.
func literalFaults(name string, file *ast.File) []string {
	allowed := map[*ast.FuncLit]bool{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv == nil || function.Name.Name != "Build" || function.Body == nil || len(function.Body.List) != 1 {
			continue
		}
		outer := returnedLiteral(function.Body.List[0])
		if outer == nil || len(outer.Body.List) != 1 {
			continue
		}
		allowed[outer] = true
		if ret, ok := outer.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			if call, ok := ret.Results[0].(*ast.CallExpr); ok && len(call.Args) == 1 {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "HandlerFunc" {
					if inner, ok := call.Args[0].(*ast.FuncLit); ok {
						allowed[inner] = true
					}
				}
			}
		}
	}
	var faults []string
	ast.Inspect(file, func(node ast.Node) bool {
		if literal, ok := node.(*ast.FuncLit); ok && !allowed[literal] {
			faults = append(faults, name+": a function literal other than the two of Build")
		}
		if labeled, ok := node.(*ast.LabeledStmt); ok {
			faults = append(faults, name+": the label "+labeled.Label.Name)
		}
		return true
	})
	return faults
}

// returnedLiteral gives the function literal that a statement return func(...) {...} returns, or nil.
func returnedLiteral(statement ast.Stmt) *ast.FuncLit {
	ret, ok := statement.(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil
	}
	literal, _ := ret.Results[0].(*ast.FuncLit)
	return literal
}

// parentCall gives the call whose function is the identifier, when there is one.
func parentCall(file *ast.File, id *ast.Ident) (*ast.CallExpr, bool) {
	var found *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && call.Fun == ast.Expr(id) {
			found = call
			return false
		}
		return found == nil
	})
	return found, found != nil
}

// isVerify tells whether a call is the method Verify of auth.JWKSVerifier, as go/types resolves it, on the field
// verifier of the receiver h of the SSO handler: h.verifier.Verify(...).
func isVerify(call *ast.CallExpr, info *gotypes.Info, pkg *gotypes.Package) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	selection, ok := info.Selections[selector]
	if !ok || selection.Kind() != gotypes.MethodVal || selection.Obj().Pkg() != pkg || objectName(selection.Obj()) != "auth.JWKSVerifier.Verify" {
		return false
	}
	field, ok := selector.X.(*ast.SelectorExpr)
	if !ok || field.Sel.Name != "verifier" {
		return false
	}
	receiver, ok := field.X.(*ast.Ident)
	return ok && isReceiver(info.Uses[receiver], pkg)
}

// isReceiver tells whether the object is the receiver of a method of a type that the package declares, as go/types
// resolves it. A receiver is in scope only in the body of its own method, so a use of it is the receiver of the
// method that holds the use, never another variable that has the same name.
func isReceiver(object gotypes.Object, pkg *gotypes.Package) bool {
	if object == nil {
		return false
	}
	for _, name := range pkg.Scope().Names() {
		named, ok := pkg.Scope().Lookup(name).Type().(*gotypes.Named)
		if !ok {
			continue
		}
		for i := 0; i < named.NumMethods(); i++ {
			if named.Method(i).Type().(*gotypes.Signature).Recv() == object {
				return true
			}
		}
	}
	return false
}

// unverifiedParses gives a fault for each call of ParseUnverified in the block that does not come after a statement
// that checks the error of h.verifier.Verify of the block and returns: if err != nil { ...; return } after
// x, err := h.verifier.Verify(...).
func unverifiedParses(name string, block *ast.BlockStmt, info *gotypes.Info, pkg *gotypes.Package) []string {
	var faults []string
	verified := false
	var verifyErr gotypes.Object
	for _, statement := range block.List {
		if assign, ok := statement.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && len(assign.Lhs) == 2 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && isVerify(call, info, pkg) {
				if id, ok := assign.Lhs[1].(*ast.Ident); ok {
					verifyErr = info.Uses[id]
					if verifyErr == nil {
						verifyErr = info.Defs[id]
					}
				}
			}
		}
		if check, ok := statement.(*ast.IfStmt); ok && verifyErr != nil && check.Init == nil {
			if binary, ok := check.Cond.(*ast.BinaryExpr); ok && binary.Op == token.NEQ {
				if id, ok := binary.X.(*ast.Ident); ok && info.Uses[id] == verifyErr {
					// The check returns at its end: if err != nil { writeAuthError(...); return }.
					if last := check.Body.List; len(last) > 0 {
						if _, returns := last[len(last)-1].(*ast.ReturnStmt); returns {
							verified = true
						}
					}
				}
			}
		}
		ast.Inspect(statement, func(node ast.Node) bool {
			if _, nested := node.(*ast.BlockStmt); nested {
				return false
			}
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ParseUnverified" && !verified {
				faults = append(faults, name+": ParseUnverified before the checked result of the common verifier")
			}
			return true
		})
	}
	return faults
}

func TestSSOUsesTheSharedVerifier(t *testing.T) {
	// Each test runs for both variants of the template: with and without the otel-tracing peer.
	t.Run("ST-SUP-RC21-OC11-T01 The SSO code creates no second signature verifier", func(t *testing.T) {
		for _, tracing := range []bool{false, true} {
			files, info, pkg := ssoPackage(t, tracing)
			calls := 0
			for _, object := range info.Uses {
				if object.Name() == "NewJWKSVerifier" && object.Pkg() == pkg {
					calls++
				}
			}
			if calls != 1 {
				t.Fatalf("tracing %v: the emitted code calls NewJWKSVerifier %d times, want once", tracing, calls)
			}
			if faults, client := ssoFaults(files, info, pkg, tracing); len(faults) > 0 || len(client) > 0 {
				t.Fatalf("tracing %v: the SSO files verify a token apart from the common verifier: %v, or import a package that could: %v", tracing, faults, client)
			}
		}
	})
	t.Run("ST-SUP-RC21-OC11-T02 The SSO code creates no second HTTP client", func(t *testing.T) {
		for _, tracing := range []bool{false, true} {
			files, info, pkg := ssoPackage(t, tracing)
			if _, client := ssoFaults(files, info, pkg, tracing); len(client) > 0 {
				t.Fatalf("tracing %v: the SSO files make a client, a request or a task of their own: %v", tracing, client)
			}
		}
	})
}
