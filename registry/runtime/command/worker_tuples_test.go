package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// fakeTuples is the reader of SpiceDB of the reconcile checks: the relations of the type tenant and the tuples that
// SpiceDB holds, as resource#relation@subject, which the check changes as the relay would. onRead, when it is not
// nil, runs before each read of a tenant resource, and fail makes the reads of one tenant resource fail.
type fakeTuples struct {
	mu        sync.Mutex
	relations []string
	held      map[string]bool
	fail      string
	onRead    func(resourceID string)
	// relationsErr, when it is not nil, is the error of each read of the schema.
	relationsErr error
}

func (f *fakeTuples) RelationsOf(_ context.Context, definition string) ([]string, error) {
	if f.relationsErr != nil {
		return nil, f.relationsErr
	}
	if definition != authz.TenantType {
		return nil, errors.New("fake: no definition " + definition)
	}
	return f.relations, nil
}

func (f *fakeTuples) Exists(_ context.Context, filter authz.Filter) (bool, error) {
	if f.onRead != nil {
		f.onRead(filter.ResourceID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if filter.ResourceID == f.fail {
		return false, errors.New("fake: SpiceDB does not answer for " + filter.ResourceID)
	}
	prefix := filter.ResourceType + ":" + filter.ResourceID + "#" + filter.Relation + "@"
	for tuple := range f.held {
		if filter.SubjectType == "" && strings.HasPrefix(tuple, prefix) {
			return true, nil
		}
		if tuple == prefix+filter.SubjectType+":"+filter.SubjectID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeTuples) set(tuples ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = map[string]bool{}
	for _, tuple := range tuples {
		f.held[tuple] = true
	}
}

// reconcileFixture is the database, the command runtime and the reader of SpiceDB of the reconcile checks.
type reconcileFixture struct {
	t       *testing.T
	e       *env
	rt      *Runtime
	logs    *logRecorder
	reader  *fakeTuples
	workers []string
}

const reconcileRelease = "step_0002_add-x_expand"

func newReconcileFixture(t *testing.T, ids ...string) *reconcileFixture {
	t.Helper()
	e := newEnv(t)
	migration, err := tenant.Migration("public", e.roles[0])
	if err != nil {
		t.Fatal(err)
	}
	identity, err := tenant.IdentityMigration("public", e.roles[0])
	if err != nil {
		t.Fatal(err)
	}
	e.exec(migration, identity)
	logs := &logRecorder{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	f := &reconcileFixture{t: t, e: e, logs: logs,
		rt:      e.runtime(func(c *Config) { c.Entities = nil; c.Logger = provider }),
		reader:  &fakeTuples{relations: []string{"member", "worker_apply_partner_replies", "worker_old_sync", "worker_sync_partner_orders"}},
		workers: []string{"sync-partner-orders", "apply-partner-replies"}}
	for _, id := range ids {
		if err := tenant.Provision(e.ctx, e.pool, "public", mustTenant(t, id), nil); err != nil {
			t.Fatal(err)
		}
	}
	f.identity(tenant.ReleaseIdentity{Release: reconcileRelease, Workers: tenant.WorkersDigest(f.workers)})
	return f
}

// identity writes the release identity row as the migration job does.
func (f *reconcileFixture) identity(id tenant.ReleaseIdentity) {
	f.t.Helper()
	if err := tenant.WriteIdentity(f.e.ctx, f.e.admin, "public", id); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reconcileFixture) reconcile() *WorkerReconcile {
	f.t.Helper()
	r, err := NewWorkerReconcile(f.rt, "public", f.workers, f.reader, reconcileRelease)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *reconcileFixture) resourceOf(tenantID string) string {
	f.t.Helper()
	resource, err := authz.TenantResource(mustTenant(f.t, tenantID))
	if err != nil {
		f.t.Fatal(err)
	}
	return resource.Type + ":" + resource.ID
}

func (f *reconcileFixture) syncT(id string) string {
	return f.resourceOf(id) + "#worker_sync_partner_orders@service_account:sync-partner-orders"
}

func (f *reconcileFixture) apply(id string) string {
	return f.resourceOf(id) + "#worker_apply_partner_replies@service_account:apply-partner-replies"
}

func (f *reconcileFixture) old(id string) string {
	return f.resourceOf(id) + "#worker_old_sync@service_account:old-sync"
}

// staged gives the staged tuple writes of the tenant, in order.
func (f *reconcileFixture) staged(tenantID string) []string {
	f.t.Helper()
	rows, err := f.e.admin.QueryContext(f.e.ctx, `SELECT p->>'op' || ' ' || (p->'tuple'->>'resource_type') || ':' || (p->'tuple'->>'resource_id') || '#' ||
 (p->'tuple'->>'relation') || '@' || coalesce(p->'tuple'->>'subject_type', '') || ':' || coalesce(p->'tuple'->>'subject_id', '')
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, created_at, id FROM stego_outbox.messages
 WHERE tenant_id = $1 AND destination = 'authz') m ORDER BY created_at, id`, tenantID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

// writes counts the outbox messages and the audit rows of the database.
func (f *reconcileFixture) writes() int {
	return f.e.count(`SELECT (SELECT count(*) FROM stego_outbox.messages) + (SELECT count(*) FROM audit_event)`)
}

// awaitLockWait waits, with no deadline, until a session of the database waits for an advisory lock, and gives false
// when done ends first. It logs the state it saw each 2 s, so a wait that never ends can be read in the log.
func (f *reconcileFixture) awaitLockWait(done <-chan error) (bool, error) {
	f.t.Helper()
	for polls := 1; ; polls++ {
		select {
		case err := <-done:
			return false, err
		default:
		}
		waiting := f.e.count(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted
 AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())`)
		if waiting > 0 {
			return true, nil
		}
		if polls%400 == 0 {
			f.t.Logf("still no session waits for an advisory lock after %d polls; granted advisory locks: %d", polls,
				f.e.count(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND granted`))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tenantRoutes serves Provision and Retire of spec on rt for the operator op-1, and gives the sender of a request.
func (f *reconcileFixture) tenantRoutes(spec TenantSpec) func(path, key, body string) int {
	t := f.t
	t.Helper()
	mux := http.NewServeMux()
	for route, action := range map[string]tenant.RequestAction{"POST /stego/tenants": tenant.ProvisionAction,
		"POST /stego/tenants/{tenantId}/retire": tenant.RetireAction} {
		spec := spec
		spec.Action, spec.Permission = action, string(action)
		h, err := ServeTenant(f.rt, spec)
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
	f.e.decision.setAllow(func(_, _, resource string) bool { return resource == "UNQUALIFIED service:orders" })
	t.Cleanup(func() { f.e.decision.setAllow(nil) })
	return func(path, key, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", `"`+key+`"`)
		w := httptest.NewRecorder()
		verifier.Middleware(mux).ServeHTTP(w, r)
		return w.Code
	}
}

// logged gives the attributes of each log record of the event name.
func (f *reconcileFixture) logged(name string) []map[string]string {
	var out []map[string]string
	for _, record := range f.logs.all() {
		if record.EventName() != name {
			continue
		}
		got := map[string]string{}
		record.WalkAttributes(func(kv attribute.KeyValue) bool {
			got[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		out = append(out, got)
	}
	return out
}

func TestWorkerTuples(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T05 The reconcile stages the missing worker tuples and deletes those of undeclared workers, once, in pages, audited", func(t *testing.T) {
		ids := []string{"org-a", "org-b", "org-c", "org-d", "org-e"}
		f := newReconcileFixture(t, ids...)
		e, reader := f.e, f.reader
		member := func(id string) string { return f.resourceOf(id) + "#member@user:u1" }
		// org-a and org-b hold their tuples and a member; org-a also the tuple of the removed worker old-sync; org-c
		// was provisioned before apply-partner-replies was added; org-d and org-e hold no worker tuple.
		reader.set(f.syncT("org-a"), f.apply("org-a"), f.old("org-a"), member("org-a"), f.syncT("org-b"), f.apply("org-b"), member("org-b"), f.syncT("org-c"))
		r := f.reconcile()
		r.page = 2
		audited := func(tenantID string) int {
			return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = $2 AND actor_kind = 'system'
 AND actor_id = 'service_account:stego-controller' AND resource_type = 'tenant' AND resource_id = $1 AND outcome = 'ok'`, tenantID, ReconcileCommand)
		}
		// A failed read of the schema writes nothing at all.
		reader.relationsErr = errors.New("fake: SpiceDB does not serve the schema")
		before := f.writes()
		if report, err := r.Run(e.ctx, nil); err == nil || !strings.Contains(err.Error(), ReconcileSchemaRead) || report.Tenants != 0 {
			t.Fatalf("the run with a failed read of the schema gave %+v and %v, want an error of the class schema_read and no tenant", report, err)
		}
		if f.writes() != before {
			t.Fatal("the run with a failed read of the schema wrote an outbox message or an audit row")
		}
		reader.relationsErr = nil
		report, err := r.Run(e.ctx, nil)
		if err != nil || report != (ReconcileReport{Tenants: 5, Changed: 4}) {
			t.Fatalf("the first run gave %+v and %v, want 5 tenants, 4 changed and no error", report, err)
		}
		want := map[string][]string{
			"org-a": {"delete_relation " + f.resourceOf("org-a") + "#worker_old_sync@:"},
			"org-b": nil,
			"org-c": {"touch " + f.apply("org-c")},
			"org-d": {"touch " + f.syncT("org-d"), "touch " + f.apply("org-d")},
			"org-e": {"touch " + f.syncT("org-e"), "touch " + f.apply("org-e")},
		}
		for _, id := range ids {
			if got := f.staged(id); !slices.Equal(got, want[id]) {
				t.Fatalf("the reconcile staged %v for %s, want %v", got, id, want[id])
			}
			if n, wantRows := audited(id), min(len(want[id]), 1); n != wantRows {
				t.Fatalf("the reconcile wrote %d audit rows for %s, want %d", n, id, wantRows)
			}
			for _, line := range f.staged(id) {
				if strings.Contains(line, "*") || !strings.Contains(line, f.resourceOf(id)+"#") {
					t.Fatalf("the reconcile staged %s for %s: a wildcard or a tuple of another tenant", line, id)
				}
			}
		}
		// The relay delivers: a second run finds nothing to change and writes nothing.
		reader.set(f.syncT("org-a"), f.apply("org-a"), member("org-a"), f.syncT("org-b"), f.apply("org-b"), member("org-b"), f.syncT("org-c"), f.apply("org-c"),
			f.syncT("org-d"), f.apply("org-d"), f.syncT("org-e"), f.apply("org-e"))
		before = f.writes()
		if report, err := r.Run(e.ctx, nil); err != nil || report != (ReconcileReport{Tenants: 5}) {
			t.Fatalf("the second run gave %+v and %v, want 5 tenants, none changed", report, err)
		}
		if f.writes() != before {
			t.Fatal("the second run wrote an outbox message or an audit row with nothing to change")
		}
		// A retired tenant is not visited. With 4 active tenants, a page of 2 divides them exactly: each is visited
		// once, and the run ends after the empty page.
		if err := tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-e"), nil); err != nil {
			t.Fatal(err)
		}
		var visited []string
		reader.onRead = func(resourceID string) { visited = append(visited, resourceID) }
		if report, err := r.Run(e.ctx, nil); err != nil || report != (ReconcileReport{Tenants: 4}) {
			t.Fatalf("the run after the retire of org-e gave %+v and %v, want 4 tenants, none changed", report, err)
		}
		reader.onRead = nil
		perTenant := map[string]int{}
		for _, id := range visited {
			perTenant[id]++
		}
		if len(perTenant) != 4 || len(visited) != 4*3 {
			t.Fatalf("the run with a page that divides the tenants read %d tuples of %d tenants, want three reads for each of 4 tenants, once", len(visited), len(perTenant))
		}
		// A tenant retired after its page was read gets no write: the reconcile reads it again in its transaction.
		reader.set(f.syncT("org-a"), f.apply("org-a"), f.syncT("org-b"), f.apply("org-b"), f.syncT("org-c"), f.apply("org-c"), f.syncT("org-d"))
		var once sync.Once
		reader.onRead = func(resourceID string) {
			if resourceID == strings.TrimPrefix(f.resourceOf("org-d"), "tenant:") {
				once.Do(func() {
					if err := tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-d"), nil); err != nil {
						t.Error(err)
					}
				})
			}
		}
		if report, err := r.Run(e.ctx, nil); err != nil || report != (ReconcileReport{Tenants: 4}) {
			t.Fatalf("the run with org-d retired during it gave %+v and %v, want 4 tenants, none changed", report, err)
		}
		reader.onRead = nil
		if got := f.staged("org-d"); !slices.Equal(got, want["org-d"]) {
			t.Fatalf("the reconcile staged %v for org-d after its retire, want only %v", got, want["org-d"])
		}
		// A failure in one tenant does not stop the next; the run fails with the count and the first class, and its
		// one log line names no tenant and no tuple.
		if err := tenant.Provision(e.ctx, e.pool, "public", mustTenant(t, "org-f"), nil); err != nil {
			t.Fatal(err)
		}
		reader.set(f.syncT("org-a"), f.apply("org-a"), f.syncT("org-c"), f.apply("org-c"))
		reader.fail = strings.TrimPrefix(f.resourceOf("org-a"), "tenant:")
		report, err = r.Run(e.ctx, nil)
		if err == nil || !strings.Contains(err.Error(), "failed in 1 of 4 tenants") || !strings.Contains(err.Error(), "first error class is tuple_read") ||
			strings.Contains(err.Error(), "org-") {
			t.Fatalf("the run with a failing tenant gave %v, want an error that counts 1 of 4 failed tenants, names tuple_read and no tenant", err)
		}
		if report.Changed != 2 || report.Failed != 1 {
			t.Fatalf("the run with a failing tenant gave %+v, want org-b and org-f changed and org-a failed", report)
		}
		failed := f.logged("command.worker_tuples.failed")
		if len(failed) != 2 {
			t.Fatalf("the failed runs logged %d records command.worker_tuples.failed, want 2 (the schema read and the tenant)", len(failed))
		}
		got := failed[1]
		if got["stego.job"] != WorkerTuplesJob || got["stego.failed_tenants"] != "1" || got["stego.tenants"] != "4" ||
			got["stego.error_class"] != ReconcileTupleRead || len(got) != 4 {
			t.Fatalf("the log line of the failed run holds %v", got)
		}
		for _, record := range f.logs.all() {
			if body := record.Body().AsString(); strings.Contains(body, "org-") || strings.Contains(body, "service_account") {
				t.Fatalf("a log line of the reconcile names a tenant or a tuple: %s", body)
			}
		}
		// The reconcile refuses what Provision refuses.
		for _, workers := range [][]string{{"a-b", "a_b"}, {"*"}, {"a/b"}} {
			if _, err := NewWorkerReconcile(f.rt, "public", workers, reader, reconcileRelease); err == nil || !strings.Contains(err.Error(), "worker") {
				t.Errorf("the workers %q gave %v, want a refusal that names the worker", workers, err)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC13-T07 Only a binary of the release identity row reconciles or deletes undeclared worker tuples, and Retire always deletes its declared workers' tuples", func(t *testing.T) {
		f := newReconcileFixture(t, "org-a", "org-b", "org-c")
		e, reader := f.e, f.reader
		reader.set(f.old("org-a"))
		r := f.reconcile()
		own := tenant.ReleaseIdentity{Release: reconcileRelease, Workers: tenant.WorkersDigest(f.workers)}
		// The digest is the one that `stego plan` shows (ST-SUP-RC35-OC13-T09): the same in any order.
		if own.Workers != "da200a1d8cb9d7dea09dde2570b5341b474a1c3c234361ea5e13598d40e8a7d1" ||
			tenant.WorkersDigest([]string{f.workers[1], f.workers[0]}) != own.Workers {
			t.Fatalf("the workers digest of %v is %s, want the vector of the plan in any order", f.workers, own.Workers)
		}
		// No row, a row of another release and a row of another worker set: the run writes nothing and logs one
		// notice each with both identities and no tenant.
		e.exec(`DELETE FROM public.stego_release_identity`)
		others := []tenant.ReleaseIdentity{{Release: "step_0003_add-y_expand", Workers: own.Workers},
			{Release: reconcileRelease, Workers: tenant.WorkersDigest([]string{"sync-partner-orders"})}}
		for i, row := range append([]tenant.ReleaseIdentity{{}}, others...) {
			if i > 0 {
				f.identity(row)
			}
			if got := r.Degraded(); got != "" {
				t.Fatalf("after %d fenced runs the job is degraded: %s, want degraded only after %d", i, got, DegradedAfter)
			}
			before := f.writes()
			report, err := r.Run(e.ctx, nil)
			if err != nil || !report.Fenced || report.Tenants != 0 || f.writes() != before {
				t.Fatalf("the run with the row %+v gave %+v and %v, and wrote %d, want a fenced run that writes nothing", row, report, err, f.writes()-before)
			}
			notices := f.logged("command.worker_tuples.fenced")
			wantRow := "missing"
			if i > 0 {
				wantRow = row.String()
			}
			if len(notices) != i+1 || notices[i]["stego.job"] != WorkerTuplesJob || notices[i]["stego.identity"] != own.String() ||
				notices[i]["stego.row_identity"] != wantRow || len(notices[i]) != 3 {
				t.Fatalf("the notices of the fenced runs are %v, want one more with the job, %s and %s", notices, own, wantRow)
			}
		}
		for _, record := range f.logs.all() {
			if record.EventName() == "command.worker_tuples.fenced" && record.Severity() != otellog.SeverityWarn {
				t.Fatalf("a notice of a fenced run has the severity %v, want warn", record.Severity())
			}
		}
		// After 3 fenced runs in a row the job is degraded, with both identities; a run that is not fenced ends it.
		if got, want := r.Degraded(), "fenced: identity row "+others[1].String()+" is not "+own.String(); got != want {
			t.Fatalf("after %d fenced runs the job gives %q, want %q", DegradedAfter, got, want)
		}
		f.identity(own)
		reader.set(f.syncT("org-a"), f.apply("org-a"), f.syncT("org-b"), f.apply("org-b"), f.syncT("org-c"), f.apply("org-c"))
		if report, err := r.Run(e.ctx, nil); err != nil || report.Fenced || report.Tenants != 3 || r.Degraded() != "" {
			t.Fatalf("the run with its own row gave %+v and %v, degraded %q, want a run of 3 tenants that is not fenced and no degraded job", report, err, r.Degraded())
		}
		reader.set(f.old("org-a"))
		// The row of another worker set changes in the middle of a run: the run stops at the next tenant's
		// transaction and writes nothing more.
		f.identity(own)
		var once sync.Once
		reader.onRead = func(string) { once.Do(func() { f.identity(others[1]) }) }
		before := f.writes()
		if report, err := r.Run(e.ctx, nil); err != nil || !report.Fenced || f.writes() != before {
			t.Fatalf("the run whose row changed during it gave %+v and %v, and wrote %d, want a fenced run that writes nothing", report, err, f.writes()-before)
		}
		reader.onRead = nil
		// A Retire stages the deletes of its declared workers' tuples with any row; the delete of the undeclared
		// relation worker_old_sync only with its own row.
		send := f.tenantRoutes(TenantSpec{ResourceType: "service", Schema: "public", SubjectType: "user", Workers: f.workers,
			Reader: reader, Release: reconcileRelease})
		declared := func(id string) []string {
			return []string{"delete " + f.syncT(id), "delete " + f.apply(id)}
		}
		for _, c := range []struct {
			id  string
			row *tenant.ReleaseIdentity
		}{{"org-a", nil}, {"org-b", &others[0]}, {"org-c", &own}} {
			if c.row == nil {
				e.exec(`DELETE FROM public.stego_release_identity`)
			} else {
				f.identity(*c.row)
			}
			if got := send("/stego/tenants/"+c.id+"/retire", "k-retire-"+c.id, ""); got != http.StatusOK {
				t.Fatalf("the retire of %s answered %d, want 200", c.id, got)
			}
			want := declared(c.id)
			if c.row != nil && *c.row == own {
				want = append(want, "delete_relation "+f.resourceOf(c.id)+"#worker_old_sync@:")
			}
			if got := f.staged(c.id); !slices.Equal(got, want) {
				t.Fatalf("the retire of %s with the row %v staged %v, want %v", c.id, c.row, got, want)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC13-T08 Retire and the reconcile take their locks in one order, and the reconcile stops when it loses the lease", func(t *testing.T) {
		f := newReconcileFixture(t, "org-a", "org-b")
		e, reader := f.e, f.reader
		send := f.tenantRoutes(TenantSpec{ResourceType: "service", Schema: "public", SubjectType: "user", Workers: f.workers,
			Reader: reader, Release: reconcileRelease})
		// The reconcile holds the lock of the worker tuples of org-a and waits; a Retire of org-a waits for that lock,
		// before it takes the chain lock of its audit row, so both end with no deadlock.
		r := f.reconcile()
		locked, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		r.afterLock = func() {
			once.Do(func() {
				close(locked)
				<-release
			})
		}
		done := make(chan error, 1)
		go func() {
			_, err := r.Run(e.ctx, nil)
			done <- err
		}()
		<-locked
		answered := make(chan error, 1)
		go func() {
			if got := send("/stego/tenants/org-a/retire", "k-retire-org-a", ""); got != http.StatusOK {
				answered <- fmt.Errorf("the retire of org-a answered %d, want 200", got)
				return
			}
			answered <- nil
		}()
		if waiting, err := f.awaitLockWait(answered); !waiting {
			close(release)
			t.Fatalf("the retire of org-a ended (%v) while the reconcile held the lock of its worker tuples", err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the reconcile gave %v, want no error and no deadlock", err)
		}
		if err := <-answered; err != nil {
			t.Fatal(err)
		}
		// The reconcile staged its touches first, and the Retire's deletes come after them.
		want := []string{"touch " + f.syncT("org-a"), "touch " + f.apply("org-a"), "delete " + f.syncT("org-a"), "delete " + f.apply("org-a"),
			"delete_relation " + f.resourceOf("org-a") + "#worker_old_sync@:"}
		if got := f.staged("org-a"); !slices.Equal(got, want) {
			t.Fatalf("org-a staged %v, want %v", got, want)
		}
		// A lost lease stops the run before the tenant's write commits.
		before := f.writes()
		report, err := r.Run(e.ctx, lostLease{})
		if err == nil || !strings.Contains(err.Error(), ReconcileLease) || report.Changed != 0 || f.writes() != before {
			t.Fatalf("the run whose lease is lost gave %+v and %v, and wrote %d, want an error of the class lease and no write", report, err, f.writes()-before)
		}
		if visited := report.Tenants; visited != 1 {
			t.Fatalf("the run whose lease is lost visited %d tenants, want it to stop at the first", visited)
		}
	})
	t.Run("ST-SUP-RP10-LC52-T02 The reconcile touches the service tuple of each active tenant provisioned before the relation service, once, and Retire waits for its lock", func(t *testing.T) {
		// The release adds the relation service of the type tenant and declares no worker. org-a, org-b and org-c were
		// provisioned before it, so none holds the service tuple; org-c is retired.
		f := newReconcileFixture(t, "org-a", "org-b", "org-c")
		e, reader := f.e, f.reader
		f.workers = nil
		f.identity(tenant.ReleaseIdentity{Release: reconcileRelease, Workers: tenant.WorkersDigest(nil)})
		reader.relations = []string{"member", TenantServiceRelation}
		reader.set()
		if err := tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-c"), nil); err != nil {
			t.Fatal(err)
		}
		serviceT := func(id string) string {
			t.Helper()
			of, err := authz.NewResource(mustTenant(t, id), "service", "orders")
			if err != nil {
				t.Fatal(err)
			}
			return f.resourceOf(id) + "#service@" + of.String()
		}
		// A reconcile with no service relation, as before this decision, writes nothing: the tuples stay missing.
		plain := f.reconcile()
		if report, err := plain.Run(e.ctx, nil); err != nil || report.Changed != 0 || f.writes() != 0 {
			t.Fatalf("the reconcile with no service relation gave %+v and %v and wrote %d, want no change", report, err, f.writes())
		}
		r := f.reconcile()
		if err := r.RelateService(""); err == nil {
			t.Fatal("RelateService took an empty resource type")
		}
		if err := r.RelateService(authz.TenantType); err == nil {
			t.Fatal("RelateService took the type tenant")
		}
		if err := r.RelateService("service"); err != nil {
			t.Fatal(err)
		}
		report, err := r.Run(e.ctx, nil)
		if err != nil || report != (ReconcileReport{Tenants: 2, Changed: 2}) {
			t.Fatalf("the reconcile gave %+v and %v, want the 2 active tenants changed", report, err)
		}
		for _, id := range []string{"org-a", "org-b"} {
			if got, want := f.staged(id), []string{"touch " + serviceT(id)}; !slices.Equal(got, want) {
				t.Fatalf("the reconcile staged %v for %s, want %v", got, id, want)
			}
			if strings.Contains(serviceT(id), "@service:orders") {
				t.Fatalf("the tuple %s names the unqualified service-level resource", serviceT(id))
			}
			if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = $2 AND actor_kind = 'system'`, id, ReconcileCommand); n != 1 {
				t.Fatalf("the reconcile wrote %d audit rows for %s, want 1", n, id)
			}
		}
		if got := f.staged("org-c"); len(got) != 0 {
			t.Fatalf("the reconcile staged %v for the retired org-c, want nothing", got)
		}
		// The relay delivers: a run again writes nothing.
		reader.set(serviceT("org-a"), serviceT("org-b"))
		before := f.writes()
		if report, err := r.Run(e.ctx, nil); err != nil || report != (ReconcileReport{Tenants: 2}) || f.writes() != before {
			t.Fatalf("the run again gave %+v and %v and wrote %d, want no change", report, err, f.writes()-before)
		}
		// The tuple of org-a is lost, as after a restore of SpiceDB: the reconcile holds the lock of org-a, and a
		// Retire of a spec with the service relation and no worker waits for it.
		reader.set(serviceT("org-b"))
		send := f.tenantRoutes(TenantSpec{ResourceType: "service", Schema: "public", SubjectType: "user", ServiceRelation: true, Release: reconcileRelease})
		locked, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		r.afterLock = func() {
			once.Do(func() {
				close(locked)
				<-release
			})
		}
		done := make(chan error, 1)
		go func() {
			_, err := r.Run(e.ctx, nil)
			done <- err
		}()
		<-locked
		answered := make(chan error, 1)
		go func() {
			if got := send("/stego/tenants/org-a/retire", "k-retire-org-a", ""); got != http.StatusOK {
				answered <- fmt.Errorf("the retire of org-a answered %d, want 200", got)
				return
			}
			answered <- nil
		}()
		if waiting, err := f.awaitLockWait(answered); !waiting {
			close(release)
			t.Fatalf("the retire of org-a ended (%v) while the reconcile held the lock of its tenant tuples", err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the reconcile gave %v, want no error", err)
		}
		if err := <-answered; err != nil {
			t.Fatal(err)
		}
		want := []string{"touch " + serviceT("org-a"), "touch " + serviceT("org-a"), "delete " + serviceT("org-a")}
		if got := f.staged("org-a"); !slices.Equal(got, want) {
			t.Fatalf("org-a staged %v, want the touch of the reconcile before the delete of the Retire: %v", got, want)
		}
		// A run after the Retire visits org-b alone and writes nothing.
		before = f.writes()
		r.afterLock = nil
		if report, err := r.Run(e.ctx, nil); err != nil || report != (ReconcileReport{Tenants: 1}) || f.writes() != before {
			t.Fatalf("the run after the Retire gave %+v and %v and wrote %d, want org-b alone and no write", report, err, f.writes()-before)
		}
	})
	t.Run("ST-SUP-RC35-OC13-T10 With no definition tenant, or a failed read of the schema, Retire still retires and deletes its declared workers' tuples, and the reconcile writes nothing", func(t *testing.T) {
		f := newReconcileFixture(t, "org-a", "org-b", "org-c")
		e := f.e
		// A provisioning service with no worker whose schema has no definition tenant, as examples/user-management.
		none := &fakeTuples{relationsErr: fmt.Errorf("fake: the schema declares no definition tenant: %w", authz.ErrNoDefinition)}
		f.identity(tenant.ReleaseIdentity{Release: reconcileRelease, Workers: tenant.WorkersDigest(nil)})
		send := f.tenantRoutes(TenantSpec{ResourceType: "service", Schema: "public", SubjectType: "user", Reader: none, Release: reconcileRelease})
		if got := send("/stego/tenants/org-a/retire", "k-retire-org-a", ""); got != http.StatusOK {
			t.Fatalf("the retire of org-a in a service with no definition tenant answered %d, want 200", got)
		}
		r, err := NewWorkerReconcile(f.rt, "public", nil, none, reconcileRelease)
		if err != nil {
			t.Fatal(err)
		}
		before := f.writes()
		if report, err := r.Run(e.ctx, nil); err != nil || report.Fenced || f.writes() != before {
			t.Fatalf("the reconcile with no definition tenant gave %+v and %v, and wrote %d, want no error and no write, so the job stays healthy", report, err, f.writes()-before)
		}
		// With workers and a read of the schema that fails for another cause, the Retire still retires org-b and
		// deletes its declared workers' tuples, skips the undeclared ones and logs one warn line with no tenant.
		f.identity(tenant.ReleaseIdentity{Release: reconcileRelease, Workers: tenant.WorkersDigest(f.workers)})
		down := &fakeTuples{relationsErr: errors.New("fake: SpiceDB does not answer for org-b")}
		send = f.tenantRoutes(TenantSpec{ResourceType: "service", Schema: "public", SubjectType: "user", Workers: f.workers, Reader: down, Release: reconcileRelease})
		if got := send("/stego/tenants/org-b/retire", "k-retire-org-b", ""); got != http.StatusOK {
			t.Fatalf("the retire of org-b with a failed read of the schema answered %d, want 200", got)
		}
		if n := e.count(`SELECT count(*) FROM stego_tenants WHERE tenant_id = 'org-b' AND retired_at IS NOT NULL`); n != 1 {
			t.Fatal("the retire of org-b with a failed read of the schema did not retire it")
		}
		if got, want := f.staged("org-b"), []string{"delete " + f.syncT("org-b"), "delete " + f.apply("org-b")}; !slices.Equal(got, want) {
			t.Fatalf("the retire of org-b staged %v, want the deletes of its declared workers only %v", got, want)
		}
		warned := f.logged("command.worker_tuples.read_failed")
		if len(warned) != 1 || warned[0]["stego.job"] != RetireCommand || warned[0]["stego.error_class"] != ReconcileSchemaRead || len(warned[0]) != 2 {
			t.Fatalf("the failed read logged %v, want one line with the command and the class only", warned)
		}
		for _, record := range f.logs.all() {
			if strings.Contains(record.Body().AsString(), "org-") {
				t.Fatalf("a log line names a tenant: %s", record.Body().AsString())
			}
		}
	})
}

// lostLease is a lease that another holder took: each fence fails.
type lostLease struct{}

func (lostLease) Fence(context.Context, *sql.Tx) (int64, error) {
	return 0, errors.New("fake: the lease row holds another holder")
}
