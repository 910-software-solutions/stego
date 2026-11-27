package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
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

	t.Run("ST-SUP-RC27-OC14-T01 A request that no route matches gets the answer of the router, and a route gets the tenant check", func(t *testing.T) {
		// The tenant list is on a closed port: a request that reaches the tenant check gets 503 unavailable, so the
		// test sees each request that the check reads the list for.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := listener.Addr().String()
		_ = listener.Close()
		db, err := sql.Open("pgx", "postgres://runtime:none@"+closed+"/app?sslmode=disable&connect_timeout=5")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		tp := gateFixture(t)
		if tp.gate, err = tenant.NewGate(db, "public", time.Second); err != nil {
			t.Fatal(err)
		}
		signing := authntest.NewKey(t, "k1")
		keys, err := authn.ParseKeys(authntest.Document(signing.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
			TenantClaim: "tenant", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		claims := authntest.Claims("https://issuer.example", "orders", "u-1", time.Now())
		claims["tenant"] = "initech"
		token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
		reached := ""
		router := http.NewServeMux()
		router.HandleFunc("GET /api/v1/orders", func(http.ResponseWriter, *http.Request) { reached = "orders" })
		router.HandleFunc("POST /api/v1/stego/tenants", func(http.ResponseWriter, *http.Request) { reached = "provision" })
		router.HandleFunc("POST /api/v1/stego/tenants/{tenantId}/retire", func(http.ResponseWriter, *http.Request) { reached = "retire" })
		handler := verifier.Middleware(tp.gated(router, router))
		for _, c := range []struct {
			method, target string
			bearer         bool
			want           int
			code           string
		}{
			{"POST", "/api/v1/stego/tenants/initech:retire", true, http.StatusNotFound, ""},
			{"GET", "/api/v1/nothing", true, http.StatusNotFound, ""},
			{"DELETE", "/api/v1/orders", true, http.StatusMethodNotAllowed, ""},
			{"GET", "/api/v1/orders", true, http.StatusServiceUnavailable, "unavailable"},
			{"GET", "/api/v1/nothing", false, http.StatusUnauthorized, ""},
		} {
			reached = ""
			r := httptest.NewRequest(c.method, c.target, nil)
			if c.bearer {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var problem struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &problem)
			if w.Code != http.StatusUnauthorized && w.Header().Get("Content-Type") != "application/problem+json" {
				t.Errorf("%s %s answered with %q, want a problem response", c.method, c.target, w.Header().Get("Content-Type"))
			}
			if w.Code == http.StatusMethodNotAllowed && w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("the 405 has Allow %q, want GET, HEAD", w.Header().Get("Allow"))
			}
			if w.Code != c.want || (c.code != "" && problem.Code != c.code) || reached != "" {
				t.Errorf("%s %s (token %t) answered %d %s and reached %q, want %d %s and no handler", c.method, c.target, c.bearer,
					w.Code, w.Body.String(), reached, c.want, c.code)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC17-T01 The problems of the tenant check have the type of their code under the problem base", func(t *testing.T) {
		const base = "https://errors.example.test/orders/"
		for _, c := range []struct {
			status int
			code   string
			want   string
		}{
			{http.StatusForbidden, tenant.ErrNotProvisionedCode, base + "tenant-not-provisioned"},
			{http.StatusServiceUnavailable, "unavailable", base + "service-unavailable"},
			{http.StatusInternalServerError, "internal", base + "internal"},
		} {
			w := httptest.NewRecorder()
			writeTenantProblem(w, c.status, base, c.code)
			var p map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p["type"] != c.want || p["code"] != c.code || w.Code != c.status {
				t.Errorf("the tenant problem %d %s is %v, want the type %s, never the bare base", c.status, c.code, p, c.want)
			}
			// A browser does not sniff the problem as another type (security review of PR 445, I-2).
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("the tenant problem %d has X-Content-Type-Options %q, want nosniff", c.status, got)
			}
		}
		w := httptest.NewRecorder()
		writeTenantProblem(w, http.StatusForbidden, "", tenant.ErrNotProvisionedCode)
		if !strings.Contains(w.Body.String(), `"type":"about:blank"`) {
			t.Errorf("with no base the tenant problem is %s, want the type about:blank", w.Body)
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

func TestTenantGateUnavailable(t *testing.T) {
	t.Run("ST-SUP-RP10-LC43-T03 A tenant list that the database cannot serve now is 503 with Retry-After over HTTP and Unavailable over gRPC", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := listener.Addr().String()
		_ = listener.Close()
		db, err := sql.Open("pgx", "postgres://runtime:none@"+closed+"/app?sslmode=disable&connect_timeout=5")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		gate, err := tenant.NewGate(db, "public", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		tp := gateFixture(t)
		tp.gate = gate
		signing := authntest.NewKey(t, "k1")
		keys, err := authn.ParseKeys(authntest.Document(signing.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
			Keys: keys, TenantClaim: "tenant"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(verifier.Stop)
		claims := authntest.Claims("https://issuer.example", "orders", "u-1", time.Now())
		claims["tenant"] = "t1"
		token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
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
		if w.Code != http.StatusServiceUnavailable || problem.Code != "unavailable" || w.Header().Get("Retry-After") == "" || reached ||
			strings.Contains(w.Body.String(), "refused") {
			t.Errorf("a tenant list that cannot be read answered %d %v %s and reached the handler %t; want 503 unavailable with Retry-After",
				w.Code, w.Header(), w.Body.String(), reached)
		}
		svc := &Service{verifier: verifier, tenants: tp}
		if _, err := svc.Authenticate(context.Background(), "Bearer "+token); status.Code(err) != codes.Unavailable ||
			status.Convert(err).Message() != "unavailable" {
			t.Errorf("a gRPC call gave %v, want UNAVAILABLE with the code unavailable", err)
		}
	})
}

func TestWorkerReconcileGuard(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T11 A provisioning service whose schema has no definition tenant runs no reconcile of the worker tuples", func(t *testing.T) {
		// The definitions of examples/user-management: provisioning on service, and no definition tenant.
		svc := &Service{parts: Parts{Definitions: []string{"activity_entry", "member", "org_setting", "organization", "service", "stego_floor", "user"}},
			tenants: &tenantProvisioning{}}
		if r, err := svc.workerReconcile(); r != nil || err != nil {
			t.Fatalf("a schema with no definition tenant gave the reconcile %v and %v, want none and no error", r, err)
		}
		// With the definition tenant, the service builds the reconcile, which refuses the missing command runtime of
		// this check: the guard let it through.
		svc.parts.Definitions = append(svc.parts.Definitions, "tenant")
		if _, err := svc.workerReconcile(); err == nil {
			t.Fatal("a schema with the definition tenant built no reconcile")
		}
		if (Parts{}).declaresTenant() {
			t.Fatal("parts with no definitions declare the definition tenant")
		}
	})
}

func TestRefusalCodes(t *testing.T) {
	t.Run("ST-SUP-RP03-LC12-T04 A refusal before the router gives its fixed problem code, and no token or tenant, to the access line of the request", func(t *testing.T) {
		// erp-demo production run (2026-11-27): a 401 of the authentication and a 403 tenant.not_provisioned of the
		// tenant check logged only their status, so an operator could not tell why from the log of the service.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closed := listener.Addr().String()
		_ = listener.Close()
		db, err := sql.Open("pgx", "postgres://runtime:none@"+closed+"/app?sslmode=disable&connect_timeout=5")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		unavailable := gateFixture(t)
		if unavailable.gate, err = tenant.NewGate(db, "public", time.Second); err != nil {
			t.Fatal(err)
		}
		signing := authntest.NewKey(t, "k1")
		keys, err := authn.ParseKeys(authntest.Document(signing.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		withClaim, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
			TenantClaim: "tenant", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(withClaim.Stop)
		// A verifier with no tenant claim gives an identity that names no tenant: 403 tenant.not_provisioned.
		noClaim, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS", Keys: keys})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(noClaim.Stop)
		const tenantID = "initech-4711"
		claims := authntest.Claims("https://issuer.example", "orders", "u-1", time.Now())
		claims["tenant"] = tenantID
		token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
		router := http.NewServeMux()
		router.HandleFunc("GET /api/v1/orders", func(http.ResponseWriter, *http.Request) {})
		for _, c := range []struct {
			name    string
			handler http.Handler
			bearer  string
			status  int
			code    string
		}{
			{"no token", withClaim.Middleware(unavailable.gated(router, router)), "", http.StatusUnauthorized, "ORDERS-AUT-001"},
			{"a token that does not verify", withClaim.Middleware(unavailable.gated(router, router)), token + "x", http.StatusUnauthorized, "ORDERS-AUT-001"},
			{"no tenant", noClaim.Middleware(gateFixture(t).gated(router, router)), token, http.StatusForbidden, tenant.ErrNotProvisionedCode},
			{"a tenant list that cannot be read", withClaim.Middleware(unavailable.gated(router, router)), token, http.StatusServiceUnavailable, "unavailable"},
		} {
			r := httptest.NewRequest("GET", "/api/v1/orders?tenant="+tenantID, nil)
			if c.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+c.bearer)
			}
			// The place for the code that the telemetry runtime of the service gives each request (otel.WithErrorCode).
			r = r.WithContext(otel.WithErrorCode(r.Context()))
			w := httptest.NewRecorder()
			c.handler.ServeHTTP(w, r)
			code, ok := otel.ErrorCode(r.Context())
			if w.Code != c.status || !ok || code != c.code {
				t.Errorf("%s: the answer is %d and the code of the access line %q (%t), want %d and %q", c.name, w.Code, code, ok, c.status, c.code)
			}
			if strings.Contains(code, tenantID) || strings.Contains(code, token) {
				t.Errorf("%s: the code of the access line %q holds a value of the request", c.name, code)
			}
		}
	})
}
