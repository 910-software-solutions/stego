package rhssoauth

import (
	"go/ast"
	"go/format"
	goparser "go/parser"
	"go/token"
	"path"
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

// ssoSyntax parses the files that rh-sso-auth emits beside the shared runtime, and gives each with the
// identifiers that the shared runtime declares at package level: its functions, types, variables and constants, and
// its method names.
func ssoSyntax(t *testing.T) (map[string]*ast.File, map[string]bool, map[string]bool) {
	t.Helper()
	own, all := ssoOwnFiles(t)
	shared, methods := map[string]bool{}, map[string]bool{}
	parsed := map[string]*ast.File{}
	for name, text := range all {
		file, err := goparser.ParseFile(token.NewFileSet(), name, text, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, mine := own[name]; mine {
			parsed[name] = file
			continue
		}
		for _, declaration := range file.Decls {
			switch d := declaration.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					shared[d.Name.Name] = true
				} else {
					methods[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						shared[s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							shared[n.Name] = true
						}
					}
				}
			}
		}
	}
	return parsed, shared, methods
}

// importNames gives the local name of each import of a file by its path.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(importPath)
		if importPath == "github.com/golang-jwt/jwt/v5" {
			name = "jwt"
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[importPath] = name
	}
	return names
}

// ssoSharedAllowed are the declarations of the shared runtime that the SSO files may use: the one verifier and its
// configuration, the identity it gives, and helpers that read settings and write a denial. Any other reference to the
// shared runtime, such as NewVerifier, compileJWKS, VerifyWithJWKS or NewAuthMiddleware, is a verifier of its own.
var ssoSharedAllowed = map[string]bool{"NewJWKSVerifier": true, "JWKSConfig": true, "JWKSVerifier": true, "Config": true, "Identity": true,
	"identityKey": true, "declaredValue": true, "envOrDefault": true, "extractToken": true, "writeAuthError": true}

// ssoImports are the packages the SSO files may import: none of them makes a network client or a timer.
var ssoImports = map[string]bool{"context": true, "errors": true, "net/http": true, "os": true, "strings": true, "github.com/golang-jwt/jwt/v5": true}

// ssoVerifierFaults gives each place where the SSO files verify a token or make a verifier other than the one common
// verifier: a reference to a shared declaration outside ssoSharedAllowed, a method of the shared runtime other than
// Verify and Stop of h.verifier, a jwt name other than MapClaims, Token and NewParser, a Parse call other than
// jwt.NewParser().ParseUnverified, and a ParseUnverified that comes before the common verifier checks the token.
func ssoVerifierFaults(files map[string]*ast.File, shared, methods map[string]bool) []string {
	var faults []string
	for name, file := range files {
		jwtName := importNames(file)["github.com/golang-jwt/jwt/v5"]
		for _, declaration := range file.Decls {
			// Each declaration, a function body or a package-level value: a value such as var f = compileJWKS is
			// a verifier of its own too.
			var body ast.Node = declaration
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Body == nil {
					continue
				}
				body = function.Body
			}
			verified := token.NoPos
			ast.Inspect(body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.Ident:
					if shared[n.Name] && !ssoSharedAllowed[n.Name] {
						faults = append(faults, name+": "+n.Name)
					}
				case *ast.SelectorExpr:
					if x, ok := n.X.(*ast.Ident); ok && x.Name == jwtName && jwtName != "" {
						if n.Sel.Name != "MapClaims" && n.Sel.Name != "Token" && n.Sel.Name != "NewParser" {
							faults = append(faults, name+": jwt."+n.Sel.Name)
						}
						return true
					}
					if methods[n.Sel.Name] {
						receiver, _ := n.X.(*ast.SelectorExpr)
						if receiver == nil || receiver.Sel.Name != "verifier" || (n.Sel.Name != "Verify" && n.Sel.Name != "Stop") {
							faults = append(faults, name+": the shared method "+n.Sel.Name)
						} else if n.Sel.Name == "Verify" && verified == token.NoPos {
							verified = n.Pos()
						}
					}
					if strings.HasPrefix(n.Sel.Name, "Parse") {
						inner, _ := n.X.(*ast.CallExpr)
						constructor, _ := func() (*ast.SelectorExpr, bool) {
							if inner == nil {
								return nil, false
							}
							s, ok := inner.Fun.(*ast.SelectorExpr)
							return s, ok
						}()
						if n.Sel.Name != "ParseUnverified" || constructor == nil || constructor.Sel.Name != "NewParser" {
							faults = append(faults, name+": the parse "+n.Sel.Name)
						} else if verified == token.NoPos || verified > n.Pos() {
							faults = append(faults, name+": ParseUnverified before the common verifier")
						}
					}
				}
				return true
			})
		}
	}
	return faults
}

// ssoClientFaults gives each place where the SSO files import a package outside ssoImports, start a goroutine, or
// use a client, a transport or a request function of net/http, under any import name.
func ssoClientFaults(files map[string]*ast.File) []string {
	var faults []string
	denied := map[string]bool{"Client": true, "Transport": true, "DefaultClient": true, "DefaultTransport": true, "RoundTripper": true,
		"Get": true, "Head": true, "Post": true, "PostForm": true, "NewRequest": true, "NewRequestWithContext": true}
	for name, file := range files {
		imports := importNames(file)
		for importPath := range imports {
			if !ssoImports[importPath] {
				faults = append(faults, name+": imports "+importPath)
			}
		}
		httpName := imports["net/http"]
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				faults = append(faults, name+": starts a goroutine")
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && httpName != "" && x.Name == httpName && denied[n.Sel.Name] {
					faults = append(faults, name+": net/http."+n.Sel.Name)
				}
			}
			return true
		})
	}
	return faults
}

func TestSSOUsesTheSharedVerifier(t *testing.T) {
	t.Run("ST-SUP-RC21-OC11-T01 The SSO code creates no second signature verifier", func(t *testing.T) {
		_, all := ssoOwnFiles(t)
		calls := 0
		for name, text := range all {
			file, err := goparser.ParseFile(token.NewFileSet(), name, text, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "NewJWKSVerifier" {
						calls++
					}
				}
				return true
			})
		}
		if calls != 1 {
			t.Fatalf("the emitted code calls NewJWKSVerifier %d times, want once", calls)
		}
		files, shared, methods := ssoSyntax(t)
		if faults := ssoVerifierFaults(files, shared, methods); len(faults) > 0 {
			t.Fatalf("the SSO files verify a token apart from the common verifier: %v", faults)
		}
	})
	t.Run("ST-SUP-RC21-OC11-T02 The SSO code creates no second HTTP client", func(t *testing.T) {
		files, _, _ := ssoSyntax(t)
		if faults := ssoClientFaults(files); len(faults) > 0 {
			t.Fatalf("the SSO files make a client, a request or a task of their own: %v", faults)
		}
	})
}
