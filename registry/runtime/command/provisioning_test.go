package command

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
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

	t.Run("ST-SUP-RC27-OC09-T06 A Provision, a Retire and a tenant list that the provider cannot decide now are 503 undecided with Retry-After", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		e.exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('ops'), ('org-a')`)
		rt := e.runtime(func(c *Config) { c.Entities = nil })
		cursors, err := query.New(query.Config{DB: rt.cfg.DB, Service: "orders", CursorKey: bytes.Repeat([]byte{'k'}, query.MinCursorKey)})
		if err != nil {
			t.Fatal(err)
		}
		active := func(context.Context, tenant.Tenant) (bool, error) { return true, nil }
		mux := http.NewServeMux()
		for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
			"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
			h, err := ServeTenant(rt, TenantSpec{Action: action, ResourceType: "service", Permission: string(action), Schema: "public",
				SubjectType: "user", Active: active})
			if err != nil {
				t.Fatal(err)
			}
			mux.Handle(route, h)
		}
		list, err := ServeTenantList(rt, TenantListSpec{ResourceType: "service", Schema: "public", SubjectType: "user",
			Active: active, Cursors: cursors})
		if err != nil {
			t.Fatal(err)
		}
		mux.Handle("GET /stego/tenants", list)
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
		send := func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			if method == http.MethodPost {
				r.Header.Set("Idempotency-Key", `"k-`+NewID()+`"`)
			}
			w := httptest.NewRecorder()
			verifier.Middleware(mux).ServeHTTP(w, r)
			return w
		}
		// The provider runs out of time on each decision, as for some seconds after a schema write.
		e.decision.setFail(func(string) error { return fmt.Errorf("the provider call: %w", context.DeadlineExceeded) })
		defer e.decision.setFail(nil)
		for _, c := range []struct{ method, path, body string }{{http.MethodPost, "/stego/tenants", `{"tenant_id":"org-x"}`},
			{http.MethodPost, "/stego/tenants/org-a/retire", ""}, {http.MethodGet, "/stego/tenants", ""}} {
			w := send(c.method, c.path, c.body)
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusServiceUnavailable || body["code"] != audit.ErrorCodeUndecided || w.Header().Get("Retry-After") == "" {
				t.Fatalf("%s %s answered %d %v %v, want 503 undecided with Retry-After", c.method, c.path, w.Code, w.Header(), body)
			}
		}
		if n := e.count(`SELECT count(*) FROM public.stego_tenants WHERE tenant_id = 'org-x' OR retired_at IS NOT NULL`); n != 0 {
			t.Fatalf("the undecided calls changed %d rows of the tenant list, want none", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'ops' AND outcome = 'failed'`); n != 3 {
			t.Fatalf("%d failed rows in the chain of ops, want 3", n)
		}
		// A provider that refuses the call stays 500 internal.
		e.decision.setFail(func(string) error { return errors.New("the provider refused the call") })
		if w := send(http.MethodPost, "/stego/tenants", `{"tenant_id":"org-y"}`); w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
			t.Fatalf("a refused provider call answered %d %v, want 500 with no Retry-After", w.Code, w.Header())
		}
	})

	t.Run("ST-SUP-RC27-OC16-T01 The operator lists the tenants in pages with a cursor, a deny is 404, and each call is audited", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		rt := e.runtime(func(c *Config) { c.Entities = nil })
		cursors, err := query.New(query.Config{DB: rt.cfg.DB, Service: "orders", CursorKey: bytes.Repeat([]byte{'k'}, query.MinCursorKey)})
		if err != nil {
			t.Fatal(err)
		}
		// The caller's tenant ops is active, so each call writes its audit row in the chain of ops.
		for _, id := range []string{"ops", "org-a", "org-b", "org-c"} {
			e.exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('` + id + `')`)
		}
		e.exec(`UPDATE public.stego_tenants SET retired_at = now() WHERE tenant_id = 'org-b'`)
		active := func(_ context.Context, t tenant.Tenant) (bool, error) { return t.ID() != "org-b", nil }
		list, err := ServeTenantList(rt, TenantListSpec{ResourceType: "service", Schema: "public", SubjectType: "user",
			Active: active, Cursors: cursors})
		if err != nil {
			t.Fatal(err)
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
		tokenOf := func(subject string) string {
			claims := authntest.Claims("https://issuer.example", "orders", subject, time.Now())
			claims["tenant"] = "ops"
			return authntest.Mint(t, authntest.Header("k1"), claims, signing)
		}
		var decided []string
		e.decision.setAllow(func(subject, action, resource string) bool {
			decided = append(decided, action+" "+resource)
			// The subject ID of a person ends with the base64url form of the sub claim: b3AtMQ for op-1.
			return action == ListTenantsPermission && resource == "UNQUALIFIED service:orders" && strings.HasSuffix(subject, "|b3AtMQ")
		})
		defer e.decision.setAllow(nil)
		get := func(subject, target string) (int, map[string]any) {
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r.Header.Set("Authorization", "Bearer "+tokenOf(subject))
			w := httptest.NewRecorder()
			verifier.Middleware(list).ServeHTTP(w, r)
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			return w.Code, body
		}
		ids := func(body map[string]any) []string {
			var out []string
			items, _ := body["items"].([]any)
			for _, item := range items {
				m := item.(map[string]any)
				out = append(out, fmt.Sprint(m["tenant_id"], " ", m["state"], " ", m["retired_at"] != nil, " ", m["provisioned_at"] != nil))
			}
			return out
		}
		status, first := get("op-1", "/stego/tenants?limit=2")
		if status != http.StatusOK || !slices.Equal(ids(first), []string{"ops active false true", "org-a active false true"}) || first["next"] == nil {
			t.Fatalf("the first page is %d %v, want ops and org-a with a cursor", status, first)
		}
		status, second := get("op-1", "/stego/tenants?limit=2&cursor="+first["next"].(string))
		if status != http.StatusOK || !slices.Equal(ids(second), []string{"org-b retired true true", "org-c active false true"}) || second["next"] != nil {
			t.Fatalf("the second page is %d %v, want org-b retired and org-c, with no cursor", status, second)
		}
		// The cursor of op-1 does not open for another principal, and a bad limit is 400.
		if status, body := get("op-2", "/stego/tenants?cursor="+first["next"].(string)); status != http.StatusBadRequest || body["code"] != ErrorCodeInvalid {
			t.Fatalf("the cursor of another principal gave %d %v, want 400 invalid", status, body)
		}
		if status, _ := get("op-1", "/stego/tenants?limit=0"); status != http.StatusBadRequest {
			t.Fatalf("limit 0 gave %d, want 400", status)
		}
		// A caller without list_tenants gets 404 denied, and reads no list.
		if status, body := get("op-2", "/stego/tenants"); status != http.StatusNotFound || body["code"] != ErrorCodeDenied || body["items"] != nil {
			t.Fatalf("a caller without list_tenants got %d %v, want 404 denied", status, body)
		}
		if !slices.Contains(decided, "list_tenants UNQUALIFIED service:orders") {
			t.Fatalf("the decisions were %v, want list_tenants on service:orders", decided)
		}
		// Each allowed call writes ok, the deny writes denied, in the chain of the caller's tenant.
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'ops' AND action = 'list-tenants' AND outcome = 'ok'`); n != 2 {
			t.Fatalf("%d ok rows of list-tenants, want 2", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'ops' AND action = 'list-tenants' AND outcome = 'denied'`); n != 1 {
			t.Fatalf("%d denied rows of list-tenants, want 1", n)
		}
		// A cursor that cannot be sealed after the allow is 500 internal, and audited as failed (security review of
		// PR #437, I6a).
		failing, err := ServeTenantList(rt, TenantListSpec{ResourceType: "service", Schema: "public", SubjectType: "user",
			Active: active, Cursors: sealFails{cursors}})
		if err != nil {
			t.Fatal(err)
		}
		list = failing
		if status, body := get("op-1", "/stego/tenants?limit=1"); status != http.StatusInternalServerError || body["code"] != ErrorCodeInternal {
			t.Fatalf("a cursor that is not sealed gave %d %v, want 500 internal", status, body)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 'ops' AND action = 'list-tenants' AND outcome = 'failed'`); n != 1 {
			t.Fatalf("%d failed rows of list-tenants, want 1", n)
		}
	})

	t.Run("ST-SUP-RC27-OC15-T02 Retire stages the delete of the tuples of each tenant type of the retired tenant, and Provision stages none", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		rt := e.runtime(func(c *Config) { c.Entities = nil })
		mux := http.NewServeMux()
		for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
			"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
			h, err := ServeTenant(rt, TenantSpec{Action: action, ResourceType: "service", Permission: string(action), Schema: "public",
				SubjectType: "user", TenantTypes: []string{"order", "tenant"}})
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
		staged := func(tenantID string) []string {
			rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op' || ' ' || (p->'tuple'->>'resource_type') || ':' || (p->'tuple'->>'resource_id') || '#' ||
 (p->'tuple'->>'relation') || ' ' || resource_key FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, resource_key, created_at, id
 FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'authz') m ORDER BY created_at, id`, tenantID)
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
		if got := send("/stego/tenants", "k-org-x", `{"tenant_id":"org-x"}`); got != http.StatusCreated {
			t.Fatalf("the provision of org-x answered %d, want 201", got)
		}
		if got := staged("org-x"); len(got) != 0 {
			t.Fatalf("the provision staged %v, want no delete", got)
		}
		if got := send("/stego/tenants/org-x/retire", "k-retire-org-x", ""); got != http.StatusOK {
			t.Fatalf("the retire of org-x answered %d, want 200", got)
		}
		prefix := authz.TenantPrefix(mustTenant(t, "org-x"))
		want := []string{"delete_tenant order:" + prefix + "# order:", "delete_tenant tenant:" + prefix + "# tenant:"}
		if got := staged("org-x"); !slices.Equal(got, want) {
			t.Fatalf("the retire of org-x staged %v, want %v", got, want)
		}
		// The replay of the retire stages no second delete.
		if got := send("/stego/tenants/org-x/retire", "k-retire-org-x", ""); got != http.StatusOK || len(staged("org-x")) != 2 {
			t.Fatalf("the replay of the retire answered %d and staged %v", got, staged("org-x"))
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
		// Retire deletes the tuples of the retired tenant only. It takes the lock of the worker tuples of org-b first,
		// which the reconcile takes too (ST-SUP-RC35-OC13): while a session of the check holds that lock, the retire
		// waits for it.
		hold, err := e.admin.BeginTx(e.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.ExecContext(e.ctx, workerTuplesLock, "org-b"); err != nil {
			t.Fatal(err)
		}
		answered := make(chan int, 1)
		go func() { answered <- send("/stego/tenants/org-b/retire", "k-retire-org-b", "") }()
		for polls, waiting := 1, false; !waiting; polls++ {
			if polls%400 == 0 {
				t.Logf("after %d polls, no session waits for the lock of the worker tuples of org-b", polls)
			}
			select {
			case got := <-answered:
				_ = hold.Rollback()
				t.Fatalf("the retire of org-b answered %d while the lock of its worker tuples was held", got)
			default:
			}
			waiting = e.count(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted
 AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())`) > 0
			if !waiting {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if err := hold.Rollback(); err != nil {
			t.Fatal(err)
		}
		if got := <-answered; got != http.StatusOK {
			t.Fatalf("the retire of org-b answered %d, want 200", got)
		}
		if got := tuples("org-b"); !slices.Equal(got, append(want("touch", "org-b"), want("delete", "org-b")...)) {
			t.Fatalf("the retire of org-b staged %v", got)
		}
		if got := tuples("org-a"); len(got) != 2 {
			t.Fatalf("the retire of org-b staged tuples in org-a: %v", got)
		}
		// A provision of a retired ID is refused with its own code, and one of an active ID with tenant.provisioned
		// (walkthrough 8, K13).
		for _, c := range []struct{ id, code string }{{"org-b", ErrorCodeRetired}, {"org-a", ErrorCodeProvisioned}} {
			r := httptest.NewRequest(http.MethodPost, "/stego/tenants", strings.NewReader(`{"tenant_id":"`+c.id+`"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Idempotency-Key", `"k-again-`+c.id+`"`)
			w := httptest.NewRecorder()
			verifier.Middleware(mux).ServeHTTP(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) {
				t.Fatalf("a provision of %s again answered %d %s, want 409 with the code %s", c.id, w.Code, w.Body.String(), c.code)
			}
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
	t.Run("ST-SUP-RC27-OC13-T02 Provision checks its admins, stages their grants in the new tenant only, and a replay with other admins is refused", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		rt := e.runtime(func(c *Config) { c.Entities = nil })
		serve := func(admin string) *http.ServeMux {
			mux := http.NewServeMux()
			for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
				"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
				// The Retire has the admin relation too, as the service gives it: it takes no admins and deletes the
				// admin tuples of the tenant (security review of PR #414, L4).
				h, err := ServeTenant(rt, TenantSpec{Action: action, ResourceType: "service", Permission: string(action), Schema: "public",
					SubjectType: "user", AdminRelation: admin, TrustedIssuers: []string{"https://issuer.example", "https://other.example"}})
				if err != nil {
					t.Fatal(err)
				}
				mux.Handle(route, h)
			}
			return mux
		}
		withAdmin, withoutAdmin := serve("admin"), serve("")
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
		mint := func(sub string) string {
			claims := authntest.Claims("https://issuer.example", "orders", sub, time.Now())
			claims["tenant"] = "ops"
			return authntest.Mint(t, authntest.Header("k1"), claims, signing)
		}
		token, strangerToken := mint("op-1"), mint("stranger")
		stranger, err := authz.PersonSubject("user", "https://issuer.example", "stranger")
		if err != nil {
			t.Fatal(err)
		}
		e.decision.setAllow(func(subject, _, resource string) bool {
			return subject != stranger.String() && resource == "UNQUALIFIED service:orders"
		})
		defer e.decision.setAllow(nil)
		type answer struct {
			status int
			body   string
		}
		sendAs := func(bearer string, mux *http.ServeMux, path, key, body string) answer {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+bearer)
			r.Header.Set("Idempotency-Key", `"`+key+`"`)
			w := httptest.NewRecorder()
			verifier.Middleware(mux).ServeHTTP(w, r)
			return answer{w.Code, w.Body.String()}
		}
		send := func(mux *http.ServeMux, path, key, body string) answer { return sendAs(token, mux, path, key, body) }
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
		person := func(issuer, sub string) string {
			subject, err := authz.PersonSubject("user", issuer, sub)
			if err != nil {
				t.Fatal(err)
			}
			return subject.String()
		}
		// Each refused body is 400 with the field admins, before any decision, and changes nothing.
		long := strings.Repeat("x", 1025)
		many := make([]string, 11)
		for i := range many {
			many[i] = `{"subject":"u` + string(rune('a'+i)) + `"}`
		}
		for name, c := range map[string]struct {
			mux  *http.ServeMux
			path string
			body string
		}{
			"no admins":           {withAdmin, "/stego/tenants", `{"tenant_id":"org-n"}`},
			"an empty list":       {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[]}`},
			"eleven persons":      {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[` + strings.Join(many, ",") + `]}`},
			"the same person":     {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":"alice"},{"subject":"alice","issuer":"https://issuer.example"}]}`},
			"an empty subject":    {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":""}]}`},
			"a control character": {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":"al\u0000ice"}]}`},
			"a long issuer":       {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":"alice","issuer":"` + long + `"}]}`},
			"an untrusted issuer": {withAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":"alice","issuer":"https://evil.example"}]}`},
			"admins when none":    {withoutAdmin, "/stego/tenants", `{"tenant_id":"org-n","admins":[{"subject":"alice"}]}`},
			"admins on a retire":  {withAdmin, "/stego/tenants/org-n/retire", `{"tenant_id":"org-n","admins":[{"subject":"alice"}]}`},
		} {
			got := send(c.mux, c.path, "k-"+NewID(), c.body)
			if got.status != http.StatusBadRequest || !strings.Contains(got.body, `"admins"`) {
				t.Errorf("%s: answered %d %s, want 400 with the field admins", name, got.status, got.body)
			}
		}
		if e.count(`SELECT count(*) FROM stego_tenants WHERE tenant_id = 'org-n'`) != 0 || len(tuples("org-n")) != 0 {
			t.Fatal("a refused body changed the tenant list or staged a tuple")
		}
		// A caller who may not provision gets 404 before any check of the admins, so a refusal tells it nothing of
		// them (security review of PR #414, L3).
		for _, body := range []string{`{"tenant_id":"org-n","admins":[]}`, `{"tenant_id":"org-n","admins":[{"subject":"alice","issuer":"https://evil.example"}]}`, `{"tenant_id":"org-n"}`} {
			if got := sendAs(strangerToken, withAdmin, "/stego/tenants", "k-"+NewID(), body); got.status != http.StatusNotFound {
				t.Errorf("the provision of a denied caller with the body %s answered %d %s, want 404", body, got.status, got.body)
			}
		}
		if got := sendAs(strangerToken, withoutAdmin, "/stego/tenants", "k-"+NewID(), `{"tenant_id":"org-n","admins":[{"subject":"alice"}]}`); got.status != http.StatusNotFound {
			t.Errorf("the provision of a denied caller with admins that the provisioning does not take answered %d, want 404", got.status)
		}
		// A body with an unknown member of an admin is refused as a bad request.
		if got := send(withAdmin, "/stego/tenants", "k-"+NewID(), `{"tenant_id":"org-n","admins":[{"subject":"alice","role":"owner"}]}`); got.status != http.StatusBadRequest {
			t.Errorf("an unknown member of an admin answered %d, want 400", got.status)
		}
		// Provision stages one touch for each admin on the tenant-qualified service-level resource of the new tenant.
		const body = `{"tenant_id":"org-a","admins":[{"subject":"alice"},{"subject":"bob","issuer":"https://other.example"}]}`
		if got := send(withAdmin, "/stego/tenants", "k-org-a", body); got.status != http.StatusCreated {
			t.Fatalf("the provision of org-a with admins answered %d %s, want 201", got.status, got.body)
		}
		resource, err := authz.NewResource(mustTenant(t, "org-a"), "service", "orders")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"touch service:" + resource.ID + "#admin@" + person("https://issuer.example", "alice"),
			"touch service:" + resource.ID + "#admin@" + person("https://other.example", "bob")}
		if got := tuples("org-a"); !slices.Equal(got, want) {
			t.Fatalf("the provision of org-a staged %v, want %v", got, want)
		}
		if !strings.Contains(resource.ID, "/") {
			t.Fatalf("the grant is on %s, which is not tenant-qualified", resource.ID)
		}
		// The audit row names the relation and the count, and no subject or issuer.
		var after string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT coalesce(after::text, '') FROM audit_event WHERE tenant_id = 'org-a' AND action = $1`, ProvisionCommand).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(after, `"admin_relation": "admin"`) || !strings.Contains(after, `"admins": 2`) || strings.Contains(after, "alice") || strings.Contains(after, "issuer") {
			t.Fatalf("the audit row of the provision holds the after state %s, want the relation and the count only", after)
		}
		// The same key with the same body replays the answer and stages nothing again; with other admins it is refused.
		if got := send(withAdmin, "/stego/tenants", "k-org-a", body); got.status != http.StatusCreated {
			t.Fatalf("the replay of the provision of org-a answered %d, want 201", got.status)
		}
		other := `{"tenant_id":"org-a","admins":[{"subject":"mallory"}]}`
		if got := send(withAdmin, "/stego/tenants", "k-org-a", other); got.status != http.StatusUnprocessableEntity {
			t.Fatalf("the replay of the key with other admins answered %d %s, want 422", got.status, got.body)
		}
		if got := send(withAdmin, "/stego/tenants", "k-org-a-2", other); got.status != http.StatusConflict {
			t.Fatalf("a second provision of org-a with other admins answered %d %s, want 409", got.status, got.body)
		}
		if got := tuples("org-a"); !slices.Equal(got, want) {
			t.Fatalf("the replays staged more tuples: %v", got)
		}
		// A Retire of org-a deletes every tuple of the admin relation of org-a, whatever its subject, and of no other
		// tenant (security review of PR #414, L4).
		if got := send(withAdmin, "/stego/tenants/org-a/retire", "k-org-a-retire", `{"tenant_id":"org-a"}`); got.status != http.StatusOK {
			t.Fatalf("the retire of org-a answered %d %s, want 200", got.status, got.body)
		}
		retired := append(slices.Clone(want), "delete_relation service:"+resource.ID+"#admin@:")
		if got := tuples("org-a"); !slices.Equal(got, retired) {
			t.Fatalf("the retire of org-a staged %v, want %v", got, retired)
		}
		// An admin relation that the runtime writes itself, or that is no relation, is refused before any request.
		for _, relation := range []string{"tenant", "worker_sync", "a b"} {
			if _, err := ServeTenant(rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
				Schema: "public", SubjectType: "user", AdminRelation: relation, TrustedIssuers: []string{"https://issuer.example"}}); err == nil {
				t.Errorf("the admin relation %q was accepted", relation)
			}
		}
		// An admin relation with no trusted issuer is refused before any request (L1).
		if _, err := ServeTenant(rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
			Schema: "public", SubjectType: "user", AdminRelation: "admin"}); err == nil || !strings.Contains(err.Error(), "trusted issuers") {
			t.Errorf("an admin relation with no trusted issuer gave %v, want a refusal that names the trusted issuers", err)
		}
	})

}

// sealFails is a cursor sealer that opens as the query runtime does and seals no cursor.
type sealFails struct{ TenantCursors }

func (sealFails) CursorText(*query.Query, tenant.Tenant, authz.Principal, query.Cursor) (string, error) {
	return "", errors.New("query: the cursor was not sealed")
}

// provisioningFixture is a tenant runtime that serves Provision and Retire for an operator op-1 of the tenant ops,
// whom the decision fake allows on the unqualified service-level resource service:orders only.
type provisioningFixture struct {
	e      *env
	rt     *Runtime
	mux    *http.ServeMux
	verify *authn.Verifier
	token  string
}

func newProvisioningFixture(t *testing.T, change func(*Config), spec TenantSpec) *provisioningFixture {
	t.Helper()
	e := newEnv(t)
	migration, err := tenant.Migration("public", e.roles[0])
	if err != nil {
		t.Fatal(err)
	}
	e.exec(migration)
	rt := e.runtime(func(c *Config) {
		c.Entities = nil
		if change != nil {
			change(c)
		}
	})
	mux := http.NewServeMux()
	for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
		"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
		s := spec
		s.Action, s.ResourceType, s.Permission, s.Schema, s.SubjectType = action, "service", string(action), "public", "user"
		h, err := ServeTenant(rt, s)
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
	e.decision.setAllow(func(_, _, resource string) bool { return resource == "UNQUALIFIED service:orders" })
	t.Cleanup(func() { e.decision.setAllow(nil) })
	return &provisioningFixture{e: e, rt: rt, mux: mux, verify: verifier, token: authntest.Mint(t, authntest.Header("k1"), claims, signing)}
}

// send posts body to path with the idempotency key key, and gives the status and the body of the answer.
func (f *provisioningFixture) send(path, key, body string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Idempotency-Key", `"`+key+`"`)
	w := httptest.NewRecorder()
	f.verify.Middleware(f.mux).ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// tuples gives each tuple that the transactions of the tenant staged for the relay, as op type:id#relation@type:id.
func (f *provisioningFixture) tuples(t *testing.T, tenantID string) []string {
	t.Helper()
	rows, err := f.e.admin.QueryContext(f.e.ctx, `SELECT p->>'op' || ' ' || (p->'tuple'->>'resource_type') || ':' || (p->'tuple'->>'resource_id') || '#' ||
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

func TestProvisioningRelations(t *testing.T) {
	t.Run("ST-SUP-RP10-LC52-T01 A Provision with the service relation relates the tenant resource to the service-level resource of the tenant, and a Retire deletes it", func(t *testing.T) {
		f := newProvisioningFixture(t, nil, TenantSpec{ServiceRelation: true})
		org := mustTenant(t, "org-s")
		tenantResource, err := authz.TenantResource(org)
		if err != nil {
			t.Fatal(err)
		}
		serviceResource, err := authz.NewResource(org, "service", "orders")
		if err != nil {
			t.Fatal(err)
		}
		want := tenantResource.String() + "#service@" + serviceResource.String()
		if status, body := f.send("/stego/tenants", "k-s1", `{"tenant_id":"org-s"}`); status != http.StatusCreated {
			t.Fatalf("the Provision answered %d %s", status, body)
		}
		if got := f.tuples(t, "org-s"); !slices.Contains(got, "touch "+want) {
			t.Fatalf("the Provision staged %v, want the touch of %s", got, want)
		}
		if strings.Contains(want, "@service:orders") {
			t.Fatalf("the tuple %s names the unqualified service-level resource", want)
		}
		// A replay of the key stages nothing again.
		before := len(f.tuples(t, "org-s"))
		if status, _ := f.send("/stego/tenants", "k-s1", `{"tenant_id":"org-s"}`); status != http.StatusCreated || len(f.tuples(t, "org-s")) != before {
			t.Fatalf("the replay answered %d and staged %d tuples more", status, len(f.tuples(t, "org-s"))-before)
		}
		if status, body := f.send("/stego/tenants/org-s/retire", "k-s2", `{}`); status != http.StatusOK {
			t.Fatalf("the Retire answered %d %s", status, body)
		}
		if got := f.tuples(t, "org-s"); !slices.Contains(got, "delete "+want) {
			t.Fatalf("the Retire staged %v, want the delete of %s", got, want)
		}

		// A schema with no relation service of the type tenant gets no such tuple.
		plain := newProvisioningFixture(t, nil, TenantSpec{})
		if status, body := plain.send("/stego/tenants", "k-p1", `{"tenant_id":"org-p"}`); status != http.StatusCreated {
			t.Fatalf("the Provision answered %d %s", status, body)
		}
		for _, tuple := range plain.tuples(t, "org-p") {
			if strings.Contains(tuple, "#service@") {
				t.Fatalf("a Provision with no service relation staged %s", tuple)
			}
		}
	})

	t.Run("ST-SUP-RP16-LC05-T02 A Provision and a Retire of a runtime with a key ring store a keyed fingerprint and a sealed answer, and replay it", func(t *testing.T) {
		ring := vaultRing(t, "1 "+throwawayKey(t))
		f := newProvisioningFixture(t, func(c *Config) { c.Ring = ring }, TenantSpec{})
		status, first := f.send("/stego/tenants", "k-r1", `{"tenant_id":"org-k"}`)
		if status != http.StatusCreated || !strings.Contains(first, "org-k") {
			t.Fatalf("the Provision answered %d %s", status, first)
		}
		if status, body := f.send("/stego/tenants/org-k/retire", "k-r2", `{}`); status != http.StatusOK {
			t.Fatalf("the Retire answered %d %s", status, body)
		}
		rows, err := f.e.admin.QueryContext(f.e.ctx, `SELECT fingerprint, response_body FROM idempotency_keys WHERE tenant_id = 'org-k'`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var fingerprint string
			var body []byte
			if err := rows.Scan(&fingerprint, &body); err != nil {
				t.Fatal(err)
			}
			n++
			if !strings.HasPrefix(fingerprint, "s1:k1:") || !bytes.HasPrefix(body, sealed.Prefix(1)) || bytes.Contains(body, []byte("org-k")) {
				t.Fatalf("a key row of the tenant holds the fingerprint %q and the body % x, want a keyed fingerprint and a sealed body", fingerprint, body)
			}
		}
		if err := rows.Close(); err != nil || n != 2 {
			t.Fatalf("the tenant has %d key rows (%v), want 2", n, err)
		}
		// A replay of the Provision gives the same answer; another body under the same key is refused.
		if status, again := f.send("/stego/tenants", "k-r1", `{"tenant_id":"org-k"}`); status != http.StatusCreated || again != first {
			t.Fatalf("the replay answered %d %s, want %s", status, again, first)
		}
		if status, body := f.send("/stego/tenants", "k-r1", `{"tenant_id":"org-k","admins":[]}`); status != http.StatusUnprocessableEntity && status != http.StatusConflict && status != http.StatusBadRequest {
			t.Fatalf("another body under the key answered %d %s, want a refusal", status, body)
		}
	})
}

func TestProvisioningUnavailable(t *testing.T) {
	t.Run("ST-SUP-RC27-OC09-T07 A Provision whose decision cannot begin on a database that cannot serve now is 503 undecided, a long run of one cause logs an error, and the begin and the provider have runs of their own", func(t *testing.T) {
		// No server listens on port 1, so each connection is refused, as for a database that is down.
		db, err := sql.Open("pgx", "postgres://stego:none@127.0.0.1:1/stego?sslmode=disable&connect_timeout=5")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		var logs bytes.Buffer
		previous, heldLog, heldRuns := slog.Default(), decisionLog, tenantRuns
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		decisionLog, tenantRuns = authz.NewCauseLog(func() time.Time { return now }), authz.NewCauseRuns(func() time.Time { return now })
		t.Cleanup(func() { slog.SetDefault(previous); decisionLog, tenantRuns = heldLog, heldRuns })
		rt := &Runtime{cfg: Config{DB: db, Service: "orders"}}
		spec := TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision", Schema: "public", SubjectType: "user"}
		decide := func(rt *Runtime, want ...string) {
			t.Helper()
			logs.Reset()
			_, err := decideTenant(context.Background(), rt, spec, Request{Tenant: mustTenant(t, "ops"), Resource: "org-x"})
			if o := tenantOutcome(err); !errors.Is(err, ErrUndecided) || o.Status != http.StatusServiceUnavailable || o.Code != audit.ErrorCodeUndecided {
				t.Fatalf("decideTenant gives %v and the answer %d %s, want ErrUndecided and 503 undecided", err, o.Status, o.Code)
			}
			for _, part := range append(want, "step=decide_tenant", "permission=provision", "cause="+authz.CauseStoreUnavailable, "transient=true", `msg="command: the transaction of the tenant decision`) {
				if !strings.Contains(logs.String(), part) {
					t.Errorf("the log %q does not hold %s", logs.String(), part)
				}
			}
			if strings.Contains(logs.String(), "127.0.0.1") {
				t.Errorf("the log %q holds the text of the database error", logs.String())
			}
		}
		decide(rt, "level=WARN", "run_seconds=0")
		now = now.Add(LongUndecided)
		decide(rt, "level=ERROR", "run_seconds=120")
		// The runtime of another service in the same process has its own run and its own log limit (security
		// review of PR #456, I2).
		decide(&Runtime{cfg: Config{DB: db, Service: "billing"}}, "level=WARN", "run_seconds=0")
		// So has the tenant list, also for a TenantSpec permission named list_tenants.
		_, down := tenant.Begin(context.Background(), db, mustTenant(t, "ops"), nil)
		logs.Reset()
		logTenantDecision(context.Background(), tenantLogKey{service: "orders", step: "list_tenants", permission: "provision"}, down)
		if line := logs.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "step=list_tenants") || !strings.Contains(line, "run_seconds=0") {
			t.Errorf("the log of the tenant list is %q, want a warning of its own run", line)
		}
		// A failure of the provider of the same cause has a run and a log limit of its own, apart from the begin
		// (security review of PR #458, I2).
		logs.Reset()
		logTenantDecision(context.Background(), tenantLogKey{service: "orders", step: "decide_tenant", permission: "provision"}, down)
		if line := logs.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, `msg="command: the authorization provider could not decide"`) || !strings.Contains(line, "run_seconds=0") {
			t.Errorf("the log of a provider failure after a long run of begin failures is %q, want a warning of its own run", line)
		}
		// A begin that succeeds ends the run of the begin, so the next failure to begin starts a new run.
		logTenantDecision(context.Background(), tenantLogKey{service: "orders", step: "decide_tenant", permission: "provision", begin: true}, nil)
		now = now.Add(authz.LogEvery)
		decide(rt, "level=WARN", "run_seconds=0")
		// A failure that a retry does not change, such as the zero tenant, stays 500 internal, and its line names
		// the transaction, not the provider (security review of PR #456, I3).
		logs.Reset()
		if _, err := decideTenant(context.Background(), rt, spec, Request{Resource: "org-x"}); errors.Is(err, ErrUndecided) || !errors.Is(err, ErrInternal) {
			t.Errorf("decideTenant with the zero tenant gives %v, want an internal error", err)
		}
		if line := logs.String(); !strings.Contains(line, `msg="command: the transaction of the tenant decision did not begin"`) || strings.Contains(line, "provider") {
			t.Errorf("the log of the zero tenant is %q, want the line of a transaction that did not begin", line)
		}
	})
}
