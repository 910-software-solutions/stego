package command

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestProvisioning(t *testing.T) {
	t.Run("ST-SUP-RC27-OC10-T02 A denied provisioning call of a caller whose tenant is not active writes no audit row and a counted log line", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		logs := &logRecorder{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
		rt := e.runtime(func(c *Config) { c.Entities = nil; c.Logger = provider })
		gate, err := tenant.NewGate(e.pool, "public", tenant.MaxGateAge)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
			"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
			h, err := ServeTenant(rt, TenantSpec{Action: action, ResourceType: "service", Permission: string(action), Schema: "public",
				SubjectType: "user", Changed: gate.Forget, Active: gate.Active})
			if err != nil {
				t.Fatal(err)
			}
			mux.Handle(route, h)
		}
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
		type caller struct{ token, subject string }
		as := func(user, tenantID string) caller {
			claims := authntest.Claims("https://issuer.example", "orders", user, time.Now())
			claims["tenant"] = tenantID
			token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
			identity, err := verifier.Verify(e.ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			p, err := authz.Person("user", identity)
			if err != nil {
				t.Fatal(err)
			}
			return caller{token: token, subject: p.Subject().String()}
		}
		send := func(c caller, path, body string) int {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+c.token)
			r.Header.Set("Idempotency-Key", `"k-`+NewID()+`"`)
			w := httptest.NewRecorder()
			verifier.Middleware(mux).ServeHTTP(w, r)
			return w.Code
		}
		chain := func(tenantID string) int {
			return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1`, tenantID)
		}
		// The operator of the tenant ops, which the list never holds, may provision and retire on service:orders.
		op := as("op-1", "ops")
		e.decision.setAllow(func(subject, _, resource string) bool {
			return subject == op.subject && resource == "UNQUALIFIED service:orders"
		})
		defer e.decision.setAllow(nil)
		if got := send(op, "/stego/tenants", `{"tenant_id":"org-a"}`); got != http.StatusCreated {
			t.Fatalf("the provision of org-a by the operator answered %d, want 201", got)
		}
		if chain("ops") != 0 || chain("org-a") != 1 {
			t.Fatalf("the provision left %d rows in the chain of ops and %d in org-a, want none and its one row", chain("ops"), chain("org-a"))
		}
		// mallory of the tenant ghost, which the list does not hold, is denied: no row in any chain, one counted log line.
		mallory := as("mallory", "ghost")
		if got := send(mallory, "/stego/tenants/org-a/retire", ""); got != http.StatusNotFound {
			t.Fatalf("the retire of org-a by mallory answered %d, want 404", got)
		}
		if got := send(mallory, "/stego/tenants", `{"tenant_id":"org-x"}`); got != http.StatusNotFound {
			t.Fatalf("the provision of org-x by mallory answered %d, want 404", got)
		}
		if chain("ghost") != 0 || chain("org-a") != 1 || chain("org-x") != 0 {
			t.Fatalf("the denied calls of mallory left rows: ghost %d, org-a %d, org-x %d", chain("ghost"), chain("org-a"), chain("org-x"))
		}
		if n := rt.UnlistedOutcomes(); n != 2 {
			t.Fatalf("the counter of the outcomes with no row is %d, want 2", n)
		}
		records := logs.all()
		if len(records) != 2 {
			t.Fatalf("%d log records, want 2", len(records))
		}
		for i, action := range []string{RetireCommand, ProvisionCommand} {
			if records[i].EventName() != "command.tenant_outcome.unlisted" {
				t.Errorf("the log record %d is the event %q", i, records[i].EventName())
			}
			got := map[string]string{}
			records[i].WalkAttributes(func(kv attribute.KeyValue) bool {
				got[string(kv.Key)] = kv.Value.Emit()
				return true
			})
			if got["stego.action"] != action || got["stego.outcome"] != "denied" || got["stego.error_code"] != ErrorCodeDenied || len(got) != 3 {
				t.Errorf("the log record %d holds %v", i, got)
			}
			for key, value := range got {
				if strings.Contains(value, "ghost") || strings.Contains(value, "org-") || strings.Contains(value, "mallory") {
					t.Errorf("the log record %d holds %s=%s", i, key, value)
				}
			}
		}
		// bob of the active tenant org-a is denied: the outcome row is in the chain of org-a, as before.
		bob := as("bob", "org-a")
		if got := send(bob, "/stego/tenants", `{"tenant_id":"org-y"}`); got != http.StatusNotFound {
			t.Fatalf("the provision of org-y by bob answered %d, want 404", got)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'org-a' AND outcome = 'denied' AND action = $1`, ProvisionCommand); n != 1 || rt.UnlistedOutcomes() != 2 {
			t.Fatalf("the denied call of bob left %d denied rows in org-a and the counter at %d, want one row and 2", n, rt.UnlistedOutcomes())
		}
	})
}
