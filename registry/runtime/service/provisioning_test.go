package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
)

// gateFixture is the tenant check of a service whose provisioning routes are under /api/v1, with no tenant list: a
// request that reaches the list fails the check, so a test sees each request that the check does not exempt.
func gateFixture(t *testing.T) *tenantProvisioning {
	t.Helper()
	p := &Provisioning{ResourceType: "service", Provision: "provision", Retire: "retire",
		ProvisionRoute: "POST /api/v1/stego/tenants", RetireRoute: "POST /api/v1/stego/tenants/{tenantId}/retire"}
	if err := p.check(); err != nil {
		t.Fatal(err)
	}
	tp := &tenantProvisioning{exempt: http.NewServeMux(), marker: &exemptRoute{},
		patterns: map[string]bool{p.ProvisionRoute: true, p.RetireRoute: true}}
	tp.exempt.Handle(p.ProvisionRoute, tp.marker)
	tp.exempt.Handle(p.RetireRoute, tp.marker)
	return tp
}

func TestTenantGate(t *testing.T) {
	t.Run("ST-SUP-RC27-OC11-T04 Only the two provisioning patterns of the main router pass the tenant check", func(t *testing.T) {
		tp := gateFixture(t)
		// The main router of a generated service, with an owned-data route under /stego/ that is more specific than
		// the pattern of Retire, as a declared collection with that path_prefix would register it.
		reached := ""
		router := http.NewServeMux()
		handle := func(pattern, name string) {
			router.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) { reached = name })
		}
		handle("POST /api/v1/stego/tenants", "provision")
		handle("POST /api/v1/stego/tenants/{tenantId}/retire", "retire")
		handle("POST /api/v1/stego/tenants/x/retire", "owned-data create")
		handle("GET /api/v1/orders", "orders")
		cases := []struct {
			method, target string
			exempt         bool
		}{
			{"POST", "/api/v1/stego/tenants", true},
			{"POST", "/api/v1/stego/tenants/org-a/retire", true},
			{"POST", "/api/v1/stego/tenants/a%2Fb/retire", true},
			{"POST", "/api/v1/stego/tenants/%2e%2e/retire", true},
			{"POST", "/api/v1/stego/tenants?x=/orders", true},
			{"POST", "/api/v1/stego/tenants/x/retire", false},
			{"POST", "/api/v1//stego/tenants", false},
			{"POST", "//api/v1/stego/tenants", false},
			{"POST", "/api/v1/./stego/tenants", false},
			{"POST", "/api/v1/stego/tenants/", false},
			{"POST", "/api/v1/stego/tenants/org-a/retire/", false},
			{"POST", "/api/v1/stego/tenants/../tenants", false},
			{"POST", "/api/v1/stego/tenants/org-a/../retire", false},
			{"POST", "/api/v1/stego%2Ftenants", false},
			{"POST", "/api/v1/STEGO/tenants", false},
			{"POST", "/stego/tenants", false},
			{"POST", "/x/api/v1/stego/tenants", false},
			{"HEAD", "/api/v1/stego/tenants", false},
			{"OPTIONS", "/api/v1/stego/tenants", false},
			{"GET", "/api/v1/stego/tenants", false},
			{"PUT", "/api/v1/stego/tenants/org-a/retire", false},
			{"DELETE", "/api/v1/stego/tenants/org-a/retire", false},
			{"CONNECT", "/api/v1/stego/tenants", false},
			{"post", "/api/v1/stego/tenants", false},
			{"GET", "/api/v1/orders", false},
		}
		for _, c := range cases {
			r := httptest.NewRequest(c.method, c.target, nil)
			if got := tp.exempted(router, r); got != c.exempt {
				t.Errorf("%s %s: exempt %t, want %t", c.method, c.target, got, c.exempt)
			}
		}
		// With no main router, no request is exempt.
		if tp.exempted(nil, httptest.NewRequest("POST", "/api/v1/stego/tenants", nil)) {
			t.Error("a provisioning route is exempt with no main router")
		}
		// The owned-data route under /stego/ goes through the tenant check: a request of no tenant does not reach it.
		w := httptest.NewRecorder()
		tp.gated(router, router).ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/stego/tenants/x/retire", nil))
		if reached != "" || w.Code == http.StatusOK {
			t.Errorf("the owned-data route under /stego/ answered %d and reached %q past the tenant check", w.Code, reached)
		}
		// The exempt routes reach the provisioning handlers of the router.
		for target, want := range map[string]string{"/api/v1/stego/tenants": "provision", "/api/v1/stego/tenants/org-a/retire": "retire"} {
			reached = ""
			tp.gated(router, router).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", target, nil))
			if reached != want {
				t.Errorf("POST %s reached %q, want %q", target, reached, want)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC11-T06 A verified token with no valid tenant claim is refused as not provisioned", func(t *testing.T) {
		tp := gateFixture(t)
		signing := authntest.NewKey(t, "k1")
		keys, err := authn.ParseKeys(authntest.Document(signing.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		// A verifier with no tenant claim gives an identity that names no tenant.
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		token := authntest.Mint(t, authntest.Header("k1"), authntest.Claims("https://issuer.example", "orders", "u-1", time.Now()), signing)
		reached := false
		router := http.NewServeMux()
		router.HandleFunc("GET /api/v1/orders", func(http.ResponseWriter, *http.Request) { reached = true })
		r := httptest.NewRequest("GET", "/api/v1/orders", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		verifier.Middleware(tp.gated(router, router)).ServeHTTP(w, r)
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &problem)
		if w.Code != http.StatusForbidden || problem.Code != "tenant.not_provisioned" || reached {
			t.Errorf("a token with no tenant claim answered %d %s and reached the handler %t; want 403 with the code tenant.not_provisioned",
				w.Code, w.Body.String(), reached)
		}
		// A gRPC call with the same token is refused with PERMISSION_DENIED and the same code.
		svc := &Service{verifier: verifier, tenants: tp}
		if _, err := svc.Authenticate(context.Background(), "Bearer "+token); status.Code(err) != codes.PermissionDenied ||
			status.Convert(err).Message() != "tenant.not_provisioned" {
			t.Errorf("a gRPC call with no tenant claim gave %v, want PERMISSION_DENIED with the code tenant.not_provisioned", err)
		}
	})
}
