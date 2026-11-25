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
func ssoOwnFiles(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()
	ctx := gen.Context{OutputNamespace: "internal/auth", ServiceName: "test-api", BasePath: "/api/v1", ErrorTypeBase: "https://errors.example/",
		ComponentConfig: map[string]any{"issuer": "https://issuer.example", "audience": "test", "jwk_cert_file": "/etc/jwks.json"}}
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
func ssoPackage(t *testing.T) (map[string]*ast.File, *gotypes.Info, *gotypes.Package) {
	t.Helper()
	own, all := ssoOwnFiles(t)
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
	pkg, err := config.Check("example.com/sso/internal/auth", fset, files, info)
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
	return importerFunc(func(importPath string) (*gotypes.Package, error) {
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

type importerFunc func(string) (*gotypes.Package, error)

func (f importerFunc) Import(importPath string) (*gotypes.Package, error) { return f(importPath) }

// ssoValues are the types of the shared runtime that the SSO files may build and read: the configuration they give
// the one verifier, and the identity it gives back. Every other type of the shared runtime, such as JWKSVerifier or
// Verifier, may not be built by hand, and its fields may not be read.
var ssoValues = map[string]bool{"JWKSConfig": true, "Config": true, "Identity": true}

// ssoAllowed are the objects that the SSO files may use, by package path and name: the one verifier of the shared
// runtime with its configuration and identity, the helpers that read settings and write a denial, and the decode of
// the claims of a token that the verifier checked. A method is named by its receiver type and its name.
var ssoAllowed = map[string]bool{
	"auth.NewJWKSVerifier": true, "auth.JWKSConfig": true, "auth.JWKSVerifier": true, "auth.Config": true, "auth.Identity": true,
	"auth.identityKey": true, "auth.declaredValue": true, "auth.envOrDefault": true, "auth.extractToken": true, "auth.writeAuthError": true,
	"auth.JWKSVerifier.Verify": true, "auth.JWKSVerifier.Stop": true,
	"jwt.MapClaims": true, "jwt.Token": true, "jwt.NewParser": true, "jwt.Parser.ParseUnverified": true,
}

// ssoImports are the packages the SSO files may import, each by its own name: none of them makes a network client
// or a task.
var ssoImports = map[string]bool{"context": true, "errors": true, "net/http": true, "os": true, "strings": true, "github.com/golang-jwt/jwt/v5": true}

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
			named := receiver.Type()
			if pointer, ok := named.(*gotypes.Pointer); ok {
				named = pointer.Elem()
			}
			if n, ok := named.(*gotypes.Named); ok {
				return base + "." + n.Obj().Name() + "." + object.Name()
			}
		}
	}
	return base + "." + object.Name()
}

// ssoFaults judges every use in the SSO files by its object, as go/types resolves it, and gives the faults of a
// verifier and the faults of a client or a task apart. It refuses an import outside
// ssoImports or a dot or a blank import; a use of a function, a type, a variable or a constant of the shared runtime
// or of the jwt package outside ssoAllowed; a composite literal or a new of a type of the shared runtime, which
// builds a verifier by hand; a selection of a field of a struct of the shared runtime; a use of net/http Client,
// Transport, RoundTripper, DefaultClient, DefaultTransport, Get, Head, Post, PostForm and NewRequest*;
// context.AfterFunc and a go statement, which start a task; and a ParseUnverified that does not follow the check
// of the error of Verify, in the same block.
func ssoFaults(files map[string]*ast.File, info *gotypes.Info, pkg *gotypes.Package) (verifier, client []string) {
	var faults []string
	httpDenied := map[string]bool{"Client": true, "Transport": true, "RoundTripper": true, "DefaultClient": true, "DefaultTransport": true,
		"Get": true, "Head": true, "Post": true, "PostForm": true, "NewRequest": true, "NewRequestWithContext": true}
	own := func(object gotypes.Object) bool {
		_, declared := object.(*gotypes.Var)
		for _, file := range files {
			if object.Pos() >= file.Pos() && object.Pos() <= file.End() {
				return true
			}
		}
		return declared && object.Parent() != pkg.Scope() && object.Parent() != nil && object.Parent() != gotypes.Universe
	}
	for name, file := range files {
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if !ssoImports[importPath] {
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
			case *ast.CompositeLit:
				if tv, ok := info.Types[n]; ok {
					if named, ok := tv.Type.(*gotypes.Named); ok && named.Obj().Pkg() == pkg && !own(named.Obj()) && !ssoValues[named.Obj().Name()] {
						faults = append(faults, name+": builds a "+named.Obj().Name()+" by hand")
					}
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
					if tv, ok := info.Types[n.Args[0]]; ok {
						if named, ok := tv.Type.(*gotypes.Named); ok && named.Obj().Pkg() == pkg && !own(named.Obj()) {
							faults = append(faults, name+": new of "+named.Obj().Name())
						}
					}
				}
			case *ast.StructType:
				// A struct of the SSO files that embeds a type of the shared runtime takes its methods as its own.
				for _, embedded := range n.Fields.List {
					if len(embedded.Names) == 0 {
						if tv, ok := info.Types[embedded.Type]; ok {
							embeddedType := tv.Type
							if p, ok := embeddedType.(*gotypes.Pointer); ok {
								embeddedType = p.Elem()
							}
							if named, ok := embeddedType.(*gotypes.Named); ok && named.Obj().Pkg() != nil && !own(named.Obj()) {
								faults = append(faults, name+": embeds "+objectName(named.Obj()))
							}
						}
					}
				}
			case *ast.SelectorExpr:
				// A method call through a promoted or an indirect selection is judged by the method it reaches.
				if selection, ok := info.Selections[n]; ok && (selection.Kind() == gotypes.MethodVal || selection.Kind() == gotypes.MethodExpr) {
					if method := selection.Obj(); method.Pkg() != nil && (method.Pkg() == pkg || method.Pkg().Path() == "github.com/golang-jwt/jwt/v5") && !own(method) && !ssoAllowed[objectName(method)] {
						faults = append(faults, name+": "+objectName(method))
					}
				}
				if selection, ok := info.Selections[n]; ok && selection.Kind() == gotypes.FieldVal {
					if field := selection.Obj(); field.Pkg() == pkg && !own(field) {
						if receiver := selection.Recv(); receiver != nil {
							if p, ok := receiver.(*gotypes.Pointer); ok {
								receiver = p.Elem()
							}
							if named, ok := receiver.(*gotypes.Named); !ok || (!own(named.Obj()) && !ssoValues[named.Obj().Name()]) {
								faults = append(faults, name+": reads the field "+field.Name()+" of the shared runtime")
							}
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
					if _, field := object.(*gotypes.Var); field && object.(*gotypes.Var).IsField() {
						return true
					}
					if !own(object) && !ssoAllowed[objectName(object)] {
						faults = append(faults, name+": "+objectName(object))
					}
				}
			case *ast.BlockStmt:
				faults = append(faults, unverifiedParses(name, n, info)...)
			}
			return true
		})
	}
	return faults, client
}

// unverifiedParses gives a fault for each call of ParseUnverified in the block that does not come after a statement
// that checks the error of a call of Verify of the block and returns: if err != nil { return ... } after
// x, err := h.verifier.Verify(...).
func unverifiedParses(name string, block *ast.BlockStmt, info *gotypes.Info) []string {
	var faults []string
	verified := false
	var verifyErr gotypes.Object
	for _, statement := range block.List {
		if assign, ok := statement.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Verify" && len(assign.Lhs) == 2 {
					if id, ok := assign.Lhs[1].(*ast.Ident); ok {
						verifyErr = info.Uses[id]
						if verifyErr == nil {
							verifyErr = info.Defs[id]
						}
					}
				}
			}
		}
		if check, ok := statement.(*ast.IfStmt); ok && verifyErr != nil && check.Init == nil {
			if binary, ok := check.Cond.(*ast.BinaryExpr); ok && binary.Op == token.NEQ {
				if id, ok := binary.X.(*ast.Ident); ok && info.Uses[id] == verifyErr {
					if last := check.Body.List; len(last) > 0 {
						// The check returns at its end: if err != nil { writeAuthError(...); return }.
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
	t.Run("ST-SUP-RC21-OC11-T01 The SSO code creates no second signature verifier", func(t *testing.T) {
		files, info, pkg := ssoPackage(t)
		calls := 0
		for _, object := range info.Uses {
			if object.Name() == "NewJWKSVerifier" && object.Pkg() == pkg {
				calls++
			}
		}
		if calls != 1 {
			t.Fatalf("the emitted code calls NewJWKSVerifier %d times, want once", calls)
		}
		if faults, client := ssoFaults(files, info, pkg); len(faults) > 0 || len(client) > 0 {
			t.Fatalf("the SSO files verify a token apart from the common verifier: %v, or import a package that could: %v", faults, client)
		}
	})
	t.Run("ST-SUP-RC21-OC11-T02 The SSO code creates no second HTTP client", func(t *testing.T) {
		files, info, pkg := ssoPackage(t)
		if _, client := ssoFaults(files, info, pkg); len(client) > 0 {
			t.Fatalf("the SSO files make a client, a request or a task of their own: %v", client)
		}
	})
}
