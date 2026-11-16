package command

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
}

func (f *fakeTuples) RelationsOf(_ context.Context, definition string) ([]string, error) {
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

func TestWorkerTuples(t *testing.T) {
	t.Run("ST-SUP-RC35-OC13-T05 The reconcile stages the missing worker tuples and deletes those of undeclared workers, once, in pages, audited", func(t *testing.T) {
		e := newEnv(t)
		migration, err := tenant.Migration("public", e.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		e.exec(migration)
		logs := &logRecorder{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
		rt := e.runtime(func(c *Config) { c.Entities = nil; c.Logger = provider })
		ids := []string{"org-a", "org-b", "org-c", "org-d", "org-e"}
		for _, id := range ids {
			if err := tenant.Provision(e.ctx, e.pool, "public", mustTenant(t, id), nil); err != nil {
				t.Fatal(err)
			}
		}
		resourceOf := func(tenantID string) string {
			resource, err := authz.TenantResource(mustTenant(t, tenantID))
			if err != nil {
				t.Fatal(err)
			}
			return resource.Type + ":" + resource.ID
		}
		syncT := func(id string) string {
			return resourceOf(id) + "#worker_sync_partner_orders@service_account:sync-partner-orders"
		}
		apply := func(id string) string {
			return resourceOf(id) + "#worker_apply_partner_replies@service_account:apply-partner-replies"
		}
		old := func(id string) string { return resourceOf(id) + "#worker_old_sync@service_account:old-sync" }
		member := func(id string) string { return resourceOf(id) + "#member@user:u1" }
		// org-a and org-b hold their tuples and a member; org-a also the tuple of the removed worker old-sync; org-c
		// was provisioned before apply-partner-replies was added; org-d and org-e hold no worker tuple.
		reader := &fakeTuples{relations: []string{"member", "worker_apply_partner_replies", "worker_old_sync", "worker_sync_partner_orders"}}
		reader.set(syncT("org-a"), apply("org-a"), old("org-a"), member("org-a"), syncT("org-b"), apply("org-b"), member("org-b"), syncT("org-c"))
		r, err := NewWorkerReconcile(rt, "public", []string{"sync-partner-orders", "apply-partner-replies"}, reader)
		if err != nil {
			t.Fatal(err)
		}
		r.page = 2
		// staged gives the staged tuple writes of the tenant, in order.
		staged := func(tenantID string) []string {
			rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op' || ' ' || (p->'tuple'->>'resource_type') || ':' || (p->'tuple'->>'resource_id') || '#' ||
 (p->'tuple'->>'relation') || '@' || coalesce(p->'tuple'->>'subject_type', '') || ':' || coalesce(p->'tuple'->>'subject_id', '')
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
		audited := func(tenantID string) int {
			return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = $2 AND actor_kind = 'system'
 AND actor_id = 'service_account:stego-controller' AND resource_type = 'tenant' AND resource_id = $1 AND outcome = 'ok'`, tenantID, ReconcileCommand)
		}
		report, err := r.Run(e.ctx)
		if err != nil || report != (ReconcileReport{Tenants: 5, Changed: 4}) {
			t.Fatalf("the first run gave %+v and %v, want 5 tenants, 4 changed and no error", report, err)
		}
		want := map[string][]string{
			"org-a": {"delete_relation " + resourceOf("org-a") + "#worker_old_sync@:"},
			"org-b": nil,
			"org-c": {"touch " + apply("org-c")},
			"org-d": {"touch " + syncT("org-d"), "touch " + apply("org-d")},
			"org-e": {"touch " + syncT("org-e"), "touch " + apply("org-e")},
		}
		for _, id := range ids {
			if got := staged(id); !slices.Equal(got, want[id]) {
				t.Fatalf("the reconcile staged %v for %s, want %v", got, id, want[id])
			}
			if n, wantRows := audited(id), min(len(want[id]), 1); n != wantRows {
				t.Fatalf("the reconcile wrote %d audit rows for %s, want %d", n, id, wantRows)
			}
			for _, line := range staged(id) {
				if strings.Contains(line, "*") || !strings.Contains(line, resourceOf(id)+"#") {
					t.Fatalf("the reconcile staged %s for %s: a wildcard or a tuple of another tenant", line, id)
				}
			}
		}
		// The relay delivers: a second run finds nothing to change and writes nothing.
		reader.set(syncT("org-a"), apply("org-a"), member("org-a"), syncT("org-b"), apply("org-b"), member("org-b"), syncT("org-c"), apply("org-c"),
			syncT("org-d"), apply("org-d"), syncT("org-e"), apply("org-e"))
		messages := e.count(`SELECT count(*) FROM stego_outbox.messages`)
		rows := e.count(`SELECT count(*) FROM audit_event`)
		if report, err := r.Run(e.ctx); err != nil || report != (ReconcileReport{Tenants: 5}) {
			t.Fatalf("the second run gave %+v and %v, want 5 tenants, none changed", report, err)
		}
		if e.count(`SELECT count(*) FROM stego_outbox.messages`) != messages || e.count(`SELECT count(*) FROM audit_event`) != rows {
			t.Fatal("the second run wrote an outbox message or an audit row with nothing to change")
		}
		// A retired tenant is not visited, and its tuples are not written again.
		if err := tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-e"), nil); err != nil {
			t.Fatal(err)
		}
		reader.set(syncT("org-a"), apply("org-a"), syncT("org-b"), apply("org-b"), syncT("org-c"), apply("org-c"), syncT("org-d"), apply("org-d"))
		if report, err := r.Run(e.ctx); err != nil || report != (ReconcileReport{Tenants: 4}) {
			t.Fatalf("the run after the retire of org-e gave %+v and %v, want 4 tenants, none changed", report, err)
		}
		// A tenant retired after its page was read gets no write: the reconcile reads it again in its transaction.
		reader.set(syncT("org-a"), apply("org-a"), syncT("org-b"), apply("org-b"), syncT("org-c"), apply("org-c"), syncT("org-d"))
		var once sync.Once
		reader.onRead = func(resourceID string) {
			if resourceID == strings.TrimPrefix(resourceOf("org-d"), "tenant:") {
				once.Do(func() {
					if err := tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-d"), nil); err != nil {
						t.Error(err)
					}
				})
			}
		}
		if report, err := r.Run(e.ctx); err != nil || report != (ReconcileReport{Tenants: 4}) {
			t.Fatalf("the run with org-d retired during it gave %+v and %v, want 4 tenants, none changed", report, err)
		}
		reader.onRead = nil
		if got := staged("org-d"); !slices.Equal(got, want["org-d"]) {
			t.Fatalf("the reconcile staged %v for org-d after its retire, want only %v", got, want["org-d"])
		}
		// A Retire that holds the lock of the worker tuples of org-c makes the reconcile wait, and the reconcile then
		// reads org-c as retired and stages nothing for it.
		reader.set(syncT("org-a"), apply("org-a"), syncT("org-b"), apply("org-b"), syncT("org-c"))
		locked, release := make(chan struct{}), make(chan struct{})
		retired := make(chan error, 1)
		go func() {
			retired <- tenant.Retire(e.ctx, e.pool, "public", mustTenant(t, "org-c"), []tenant.Hook{func(ctx context.Context, q tenant.Querier, tn tenant.Tenant) error {
				if _, err := q.ExecContext(ctx, workerTuplesLock, tn.ID()); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			}})
		}()
		<-locked
		done := make(chan error, 1)
		go func() {
			_, err := r.Run(e.ctx)
			done <- err
		}()
		// The reconcile either waits for the lock, which the check sees in pg_locks, or, with no lock, ends first.
		for waiting := false; !waiting; {
			select {
			case err := <-done:
				close(release)
				<-retired
				t.Fatalf("the reconcile ended (%v) while a Retire of org-c held the lock of its worker tuples", err)
			default:
			}
			waiting = e.count(`SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted
 AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())`) > 0
			if !waiting {
				time.Sleep(5 * time.Millisecond)
			}
		}
		close(release)
		if err := <-retired; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := staged("org-c"); !slices.Equal(got, want["org-c"]) {
			t.Fatalf("the reconcile staged %v for org-c while its Retire committed, want only %v", got, want["org-c"])
		}
		// A failure in one tenant does not stop the next; the run fails with the count and the first class, and its
		// one log line names no tenant and no tuple.
		if err := tenant.Provision(e.ctx, e.pool, "public", mustTenant(t, "org-f"), nil); err != nil {
			t.Fatal(err)
		}
		reader.set(syncT("org-a"), apply("org-a"))
		reader.fail = strings.TrimPrefix(resourceOf("org-a"), "tenant:")
		report, err = r.Run(e.ctx)
		if err == nil || !strings.Contains(err.Error(), "failed in 1 of 3 tenants") || !strings.Contains(err.Error(), "first error class is tuple_read") ||
			strings.Contains(err.Error(), "org-") {
			t.Fatalf("the run with a failing tenant gave %v, want an error that counts 1 of 3 failed tenants, names tuple_read and no tenant", err)
		}
		if report.Changed != 2 || report.Failed != 1 {
			t.Fatalf("the run with a failing tenant gave %+v, want org-b and org-f changed and org-a failed", report)
		}
		records := logs.all()
		if len(records) != 1 || records[0].EventName() != "command.worker_tuples.failed" {
			t.Fatalf("the failed run logged %d records, want the one command.worker_tuples.failed", len(records))
		}
		got := map[string]string{}
		records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			got[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if got["stego.job"] != WorkerTuplesJob || got["stego.failed_tenants"] != "1" || got["stego.tenants"] != "3" ||
			got["stego.error_class"] != ReconcileTupleRead || len(got) != 4 {
			t.Fatalf("the log line of the failed run holds %v", got)
		}
		if body := records[0].Body().AsString(); strings.Contains(body, "org-") || strings.Contains(body, "service_account") {
			t.Fatalf("the log line of the failed run names a tenant or a tuple: %s", body)
		}
		// The reconcile refuses what Provision refuses, and no worker.
		for _, workers := range [][]string{nil, {"a-b", "a_b"}, {"*"}, {"a/b"}} {
			if _, err := NewWorkerReconcile(rt, "public", workers, reader); err == nil || !strings.Contains(err.Error(), "worker") {
				t.Errorf("the workers %q gave %v, want a refusal that names the worker", workers, err)
			}
		}
	})
}
