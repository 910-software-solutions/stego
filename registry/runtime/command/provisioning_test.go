package command

import (
	"net/http"
	"net/http/httptest"
	"slices"
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

	t.Run("ST-SUP-RC35-OC13-T03 Provision stages one tuple for each worker in its tenant, a replay stages none, and Retire deletes them", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		rt := e.runtime(func(c *Config) { c.Entities = nil })
		workers := []string{"sync-partner-orders", "apply-partner-replies"}
		mux := http.NewServeMux()
		for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
			"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
			h, err := ServeTenant(rt, TenantSpec{Action: action, ResourceType: "service", Permission: string(action), Schema: "public",
				SubjectType: "user", Workers: workers})
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
		claims := authntest.Claims("https://issuer.example", "orders", "op-1", time.Now())
		claims["tenant"] = "ops"
		token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
		e.decision.setAllow(func(_, _, resource string) bool { return resource == "UNQUALIFIED service:orders" })
		defer e.decision.setAllow(nil)
		send := func(path, key, body string) int {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", `"`+key+`"`)
			w := httptest.NewRecorder()
			verifier.Middleware(mux).ServeHTTP(w, r)
			return w.Code
		}
		// tuples gives the staged tuple writes of the tenant, each with its resource ID as written.
		tuples := func(tenantID string) []string {
			rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op' || ' ' || (p->'tuple'->>'resource_type') || ':' || (p->'tuple'->>'resource_id') || '#' ||
 (p->'tuple'->>'relation') || '@' || (p->'tuple'->>'subject_type') || ':' || (p->'tuple'->>'subject_id')
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, created_at, id FROM stego_outbox.messages
 WHERE tenant_id = $1 AND destination = 'authz') m ORDER BY created_at, id`, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				out = append(out, line)
			}
			return out
		}
		resourceOf := func(tenantID string) string {
			resource, err := authz.TenantResource(mustTenant(t, tenantID))
			if err != nil {
				t.Fatal(err)
			}
			return resource.Type + ":" + resource.ID
		}
		want := func(op, tenantID string) []string {
			return []string{op + " " + resourceOf(tenantID) + "#worker_sync_partner_orders@service_account:sync-partner-orders",
				op + " " + resourceOf(tenantID) + "#worker_apply_partner_replies@service_account:apply-partner-replies"}
		}
		for _, id := range []string{"org-a", "org-b"} {
			if got := send("/stego/tenants", "k-"+id, `{"tenant_id":"`+id+`"}`); got != http.StatusCreated {
				t.Fatalf("the provision of %s answered %d, want 201", id, got)
			}
			if got := tuples(id); !slices.Equal(got, want("touch", id)) {
				t.Fatalf("the provision of %s staged %v, want %v", id, got, want("touch", id))
			}
		}
		// The same request again replays its answer and stages no second tuple.
		if got := send("/stego/tenants", "k-org-a", `{"tenant_id":"org-a"}`); got != http.StatusCreated {
			t.Fatalf("the replay of the provision of org-a answered %d, want 201", got)
		}
		if got := tuples("org-a"); len(got) != 2 {
			t.Fatalf("the replay staged more tuples: %v", got)
		}
		// Retire deletes the tuples of the retired tenant only.
		if got := send("/stego/tenants/org-b/retire", "k-retire-org-b", ""); got != http.StatusOK {
			t.Fatalf("the retire of org-b answered %d, want 200", got)
		}
		if got := tuples("org-b"); !slices.Equal(got, append(want("touch", "org-b"), want("delete", "org-b")...)) {
			t.Fatalf("the retire of org-b staged %v", got)
		}
		if got := tuples("org-a"); len(got) != 2 {
			t.Fatalf("the retire of org-b staged tuples in org-a: %v", got)
		}
		// Each tuple is joined to the revocation of its transaction, and none has a wildcard.
		if n := e.count(`SELECT count(*) FROM authz.authz_revocation_tuples WHERE tenant_id IN ('org-a', 'org-b')`); n != 6 {
			t.Fatalf("%d tuples of a revocation, want 6", n)
		}
		for _, line := range append(tuples("org-a"), tuples("org-b")...) {
			if strings.Contains(line, "*") {
				t.Fatalf("the tuple %s holds a wildcard", line)
			}
		}
		// When the second worker tuple cannot be staged, the change of the tenant list, its audit row and the first
		// tuple roll back together (security review of PR #376, L4).
		e.exec(`CREATE FUNCTION authz.refuse_second_tuple() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
  IF EXISTS (SELECT 1 FROM authz.authz_revocation_tuples WHERE revocation_id = NEW.revocation_id) THEN
    RAISE EXCEPTION 'the check refuses the second tuple';
  END IF;
  RETURN NEW;
END $f$`, `CREATE TRIGGER refuse_second_tuple BEFORE INSERT ON authz.authz_revocation_tuples FOR EACH ROW EXECUTE FUNCTION authz.refuse_second_tuple()`)
		if got := send("/stego/tenants", "k-org-r", `{"tenant_id":"org-r"}`); got != http.StatusInternalServerError {
			t.Fatalf("the provision whose second tuple fails answered %d, want 500", got)
		}
		e.exec(`DROP TRIGGER refuse_second_tuple ON authz.authz_revocation_tuples`, `DROP FUNCTION authz.refuse_second_tuple()`)
		if n := e.count(`SELECT count(*) FROM stego_tenants WHERE tenant_id = 'org-r'`); n != 0 || len(tuples("org-r")) != 0 ||
			e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'org-r' AND outcome = 'ok'`) != 0 ||
			e.count(`SELECT count(*) FROM authz.authz_revocation_tuples WHERE tenant_id = 'org-r'`) != 0 {
			t.Fatal("the provision whose second tuple failed committed a part of its change")
		}
		// Two workers whose names give one relation, such as a-b and a_b, are refused: each would hold the other's
		// grant (security review of PR #376, L2).
		if _, err := ServeTenant(rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
			Schema: "public", SubjectType: "user", Workers: []string{"a-b", "a_b"}}); err == nil || !strings.Contains(err.Error(), "worker_a_b") {
			t.Errorf("the workers a-b and a_b gave %v, want a refusal that names the relation worker_a_b", err)
		}
		// A worker name that no tuple can hold is refused before any request.
		for _, name := range []string{"a/b", "*", ""} {
			_, err := ServeTenant(rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
				Schema: "public", SubjectType: "user", Workers: []string{name}})
			if err == nil || !strings.Contains(err.Error(), "worker") {
				t.Errorf("the worker name %q gave %v, want a refusal that names the worker", name, err)
			}
		}
	})
}
