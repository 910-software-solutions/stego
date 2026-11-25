package rhssoauth

import (
	"go/format"
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

func TestSSOUsesTheSharedVerifier(t *testing.T) {
	t.Run("ST-SUP-RC21-OC11-T01 The SSO code creates no second signature verifier", func(t *testing.T) {
		own, all := ssoOwnFiles(t)
		calls := 0
		for _, text := range all {
			calls += strings.Count(text, "NewJWKSVerifier(") - strings.Count(text, "func NewJWKSVerifier(")
		}
		if calls != 1 {
			t.Fatalf("the emitted code calls NewJWKSVerifier %d times, want once", calls)
		}
		for name, text := range own {
			for _, word := range []string{"jwt.Parse(", "ParseWithClaims(", "func(token *jwt.Token)", "Keyfunc", ".Verify(raw"} {
				if strings.Contains(text, word) && !(word == ".Verify(raw" && strings.Contains(text, "h.verifier.Verify(")) {
					t.Fatalf("%s holds %q: a verifier of its own", name, word)
				}
			}
			// The SSO payload decodes the claims of a token without its key only after the common verifier has
			// checked the token.
			if at := strings.Index(text, "ParseUnverified("); at >= 0 {
				if verified := strings.Index(text, "h.verifier.Verify("); verified < 0 || verified > at {
					t.Fatalf("%s decodes a token before the common verifier checks it", name)
				}
			}
		}
	})
	t.Run("ST-SUP-RC21-OC11-T02 The SSO code creates no second HTTP client", func(t *testing.T) {
		own, _ := ssoOwnFiles(t)
		for name, text := range own {
			for _, word := range []string{"http.Client", "http.Transport", "http.Get(", "http.Post(", "http.DefaultClient", "http.NewRequest"} {
				if strings.Contains(text, word) {
					t.Fatalf("%s holds %q: an HTTP client of its own", name, word)
				}
			}
		}
	})
}
