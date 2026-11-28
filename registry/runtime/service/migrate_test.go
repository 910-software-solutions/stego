package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/910-software-solutions/stego/registry/runtime/audit"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/command"
	"github.com/910-software-solutions/stego/registry/runtime/internal/authzclock"
	"github.com/910-software-solutions/stego/registry/runtime/internal/pgtest"
	"github.com/910-software-solutions/stego/registry/runtime/lease"
	"github.com/910-software-solutions/stego/registry/runtime/manifest"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
	"github.com/910-software-solutions/stego/registry/runtime/outbox"
	"github.com/910-software-solutions/stego/registry/runtime/outbox/kafkainbox"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// steps records the step names of Migrate and the backend of the connection that each runs on.
type steps struct {
	mu       sync.Mutex
	names    []string
	backends map[int]bool
}

func (s *steps) record(t *testing.T) func(string, *sql.Conn) {
	return func(name string, conn *sql.Conn) {
		var pid int
		if err := conn.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Errorf("the step %s has no open connection: %v", name, err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.names = append(s.names, name)
		s.backends[pid] = true
	}
}

func TestMigrate(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T30 The job ends only when SpiceDB serves every definition of the schema it wrote, and else fails closed at the step authorization_schema_served", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		saved := awaitSchema
		t.Cleanup(func() { awaitSchema = saved })
		// The real wait passes once the local SpiceDB serves the schema; the run also leaves a schema in SpiceDB, so
		// the next run reads a token before its write.
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("Migrate with the real wait: %v", err)
		}
		var waited [][]string
		writesBefore := 0
		writes := 0
		awaitSchema = func(ctx context.Context, conn grpc.ClientConnInterface, definitions []string, since authz.Token) error {
			if since.IsZero() {
				t.Error("the job waits with no token of the schema before its write")
			}
			waited = append(waited, definitions)
			writesBefore = writes
			return fmt.Errorf("%w: test", authz.ErrSchemaNotServed)
		}
		s := &steps{backends: map[int]bool{}}
		stepped = s.record(t)
		err := Migrate(context.Background(), settings, f.migration(&writes))
		stepped = nil
		var step *StepError
		if !errors.As(err, &step) || step.Step != StepSchemaServed || Stage(err) != "migrate.authorization_schema_served" || !errors.Is(err, authz.ErrSchemaNotServed) {
			t.Fatalf("a schema that SpiceDB does not serve gives %v, want a fault of the step %s with authz.ErrSchemaNotServed", err, StepSchemaServed)
		}
		if len(waited) != 1 || !slices.Equal(waited[0], []string{"user", "service", "order", "outbox_destination", "stego_floor"}) || writesBefore != 2 {
			t.Fatalf("the job waited for %v after %d schema writes, want one wait for every definition after both", waited, writesBefore)
		}
		if last := s.names[len(s.names)-1]; last != StepSchemaServed || slices.Contains(s.names, StepManifestBefore) {
			t.Fatalf("the job ran %v, want it to stop at %s", s.names, StepSchemaServed)
		}
		awaitSchema = saved
		// A job with no definitions is refused before it opens anything.
		bare := f.migration(nil)
		bare.Definitions = nil
		if err := Migrate(context.Background(), settings, bare); !errors.As(err, &step) || step.Step != StepSettings {
			t.Fatalf("a job with no definitions gives %v, want a fault of the step %s", err, StepSettings)
		}
	})
	t.Run("ST-SUP-RP14-LC03-T01 Migrate runs the migration job in its fixed order on one connection", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		want := []string{StepOwnerRole, StepJobLock, StepLedgerCheck, StepAuthorizationSchema, StepTables, StepTenantKeys, StepTenantKeysValidate, StepOutboxQueue, StepOutboxSequence, StepAuditTables, StepAuditSink,
			StepIdempotencyKeys, StepAuthorizationTables, StepRuntimeGrants, StepAuditPartitions, StepReleaseReports, StepSchemaAgain, StepSchemaServed, StepManifestBefore, StepBackfill, StepManifest, StepReleaseIdentity}
		run := func(m Migration) (*steps, error) {
			s := &steps{backends: map[int]bool{}}
			stepped = s.record(t)
			defer func() { stepped = nil }()
			return s, Migrate(context.Background(), settings, m)
		}
		writes := 0
		for _, pass := range []string{"a fresh database", "the same database again"} {
			m := f.migration(&writes)
			s, err := run(m)
			if err != nil {
				t.Fatalf("Migrate on %s: %v", pass, err)
			}
			if !slices.Equal(s.names, want) || len(s.backends) != 1 {
				t.Fatalf("Migrate on %s ran %v on %d connections, want %v on one", pass, s.names, len(s.backends), want)
			}
			// The last step writes the one release identity row of the release and its workers (ST-SUP-RC35-OC13).
			got, found, err := tenant.ReadIdentity(context.Background(), f.admin, "public")
			if err != nil || !found || got != m.Parts.Identity() || got.Workers != tenant.WorkersDigest(m.Parts.WorkerNames) {
				t.Fatalf("Migrate on %s left the release identity row %+v (found %v, %v), want %+v", pass, got, found, err, m.Parts.Identity())
			}
		}
		if writes != 4 {
			t.Fatalf("the schema was written %d times in two runs, want twice in each", writes)
		}
		// The release reports step deletes, as the owner role, the reports older than one day and keeps the current
		// ones (ST-SUP-RP09-LC14; security review of PR #316, M1).
		releases, err := lease.Releases(f.runtime)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET ROLE " + f.owner, "CREATE SCHEMA IF NOT EXISTS stego_schema", releases,
			"INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v0', 10000, now() - interval '25 hours'), ('current', 'step_0001_x_expand', 'v1', 10000, now())",
			"RESET ROLE"} {
			if _, err := f.admin.Exec(statement); err != nil {
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		if _, err := run(f.migration(nil)); err != nil {
			t.Fatalf("Migrate with release reports: %v", err)
		}
		var reports string
		if err := f.admin.QueryRow("SELECT string_agg(holder, ',' ORDER BY holder) FROM stego_schema.releases").Scan(&reports); err != nil || reports != "current" {
			t.Fatalf("after the job the release reports are %q (%v), want only the current one", reports, err)
		}
		// The job made the objects and the grants of the runtime role, and the partitions of the audit table.
		var granted bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.orders', 'INSERT') AND has_table_privilege($1, 'public.idempotency_keys', 'UPDATE')
 AND has_function_privilege($1, 'public.idempotency_keys_purge(timestamptz, integer)', 'EXECUTE') AND to_regclass('stego_outbox.messages') IS NOT NULL AND to_regclass('public.authz_floor') IS NOT NULL`, f.runtime).Scan(&granted); err != nil || !granted {
			t.Fatalf("the job did not make the objects and the grants of the runtime role: %v", err)
		}
		// The tables step made the tenant list of the tenant runtime, owned by the owner role, which the runtime role
		// reads and writes only through its functions (ST-SUP-RP04-LC07, ST-SUP-Q97).
		var listed bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.stego_tenants', 'SELECT') AND NOT has_table_privilege($1, 'public.stego_tenants', 'INSERT')
 AND has_function_privilege($1, 'public.stego_provision_tenant(text)', 'EXECUTE')
 AND (SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'stego_tenants') = $2`, f.runtime, f.owner).Scan(&listed); err != nil || !listed {
			t.Fatalf("the job did not make the tenant list for the runtime role: %v", err)
		}
		// The audit sink step made the default table of the audit sink, owned by the owner role, with SELECT and INSERT
		// and no UPDATE, DELETE or TRUNCATE for the worker role (ST-SUP-RP05-LC15).
		var sinkTable bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'stego_audit_sink.entries', 'SELECT') AND has_table_privilege($1, 'stego_audit_sink.entries', 'INSERT')
 AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'UPDATE') AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'DELETE')
 AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'TRUNCATE') AND NOT has_table_privilege($2, 'stego_audit_sink.entries', 'SELECT')
 AND (SELECT tableowner FROM pg_tables WHERE schemaname = 'stego_audit_sink' AND tablename = 'entries') = $3`, f.worker, f.runtime, f.owner).Scan(&sinkTable); err != nil || !sinkTable {
			t.Fatalf("the job did not make the default audit sink for the worker role: %v", err)
		}
		var partitions int
		if err := f.admin.QueryRow(`SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.audit_event'::regclass`).Scan(&partitions); err != nil || partitions < 2 {
			t.Fatalf("the job made %d audit partitions, want this month and the next: %v", partitions, err)
		}

		// A step that fails stops the job there, with a fault that names it, and no later step runs.
		broken := f.migration(nil)
		broken.Authorization = "SELECT 1 / 0"
		s, err := run(broken)
		var step *StepError
		if !errors.As(err, &step) || step.Step != StepAuthorizationTables || Stage(err) != "migrate.authorization_tables" {
			t.Fatalf("the broken job gave %v, want a fault of the step %s", err, StepAuthorizationTables)
		}
		if last := s.names[len(s.names)-1]; last != StepAuthorizationTables || slices.Contains(s.names, StepRuntimeGrants) {
			t.Fatalf("the broken job ran %v, want it to stop at %s", s.names, StepAuthorizationTables)
		}
		// The read-only ledger check refuses a database whose tables this release did not make, before any write.
		if _, err := f.admin.Exec(`CREATE TABLE public.foreign_orders (id text)`); err != nil {
			t.Fatal(err)
		}
		writes = 0
		s, err = run(f.migration(&writes))
		if Stage(err) != "migrate.ledger_check" || writes != 0 || slices.Contains(s.names, StepTables) {
			t.Fatalf("the job on a foreign table gave %v after %v with %d schema writes, want a fault of the ledger check and no write", err, s.names, writes)
		}
		if Stage(errors.New("other")) != "migrate" || !strings.HasPrefix(err.Error(), "migrate: ledger_check: ") {
			t.Fatalf("Stage and the fault text are not the step's: %v", err)
		}
		// A job that lacks a part is refused before it opens the database.
		if err := Migrate(context.Background(), settings, Migration{}); Stage(err) != "migrate.settings" {
			t.Fatalf("a job with no parts gave %v", err)
		}
	})
}

// boardsComplete is the completeness CHECK of the rename of title to heading, NOT VALID as the step adds it.
const boardsComplete = "ALTER TABLE public.boards ADD CONSTRAINT boards_heading_complete CHECK ((heading IS NOT NULL) OR (title IS NULL)) NOT VALID"

// unmove clears the new column of the rows of boards that where selects, with the sync trigger off, as for rows that
// the release before wrote before the step, and adds the completeness CHECK again NOT VALID, as the step adds it.
func unmove(t *testing.T, f *fixture, where string) {
	t.Helper()
	if _, err := f.admin.ExecContext(context.Background(), `ALTER TABLE public.boards DROP CONSTRAINT IF EXISTS boards_heading_complete;
ALTER TABLE public.boards DISABLE TRIGGER boards_heading_sync; UPDATE public.boards SET heading = NULL WHERE `+where+`;
ALTER TABLE public.boards ENABLE TRIGGER boards_heading_sync; `+boardsComplete); err != nil {
		t.Fatal(err)
	}
}

// boardsDDL is the table of a rename at expand of title to heading, owned by tenants and under forced row-level
// security, with the sync trigger of the change, the table of the backfill checkpoints, its functions and the row of
// the change, as the migration of the step makes them as the owner role.
func boardsDDL(t *testing.T, owner string) string {
	t.Helper()
	function, err := manifest.SyncFunction("stego_steps", "boards_heading_sync", manifest.SyncVersion, manifest.Sync{Change: "board-title-rename", Owner: owner, K: "title", U: "heading",
		Conversion: manifest.Conversion{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	return `CREATE TABLE public.boards (tenant_id text NOT NULL DEFAULT pg_catalog.current_setting('app.tenant'), id uuid NOT NULL, title text NOT NULL, heading text,
  PRIMARY KEY (tenant_id, id));
ALTER TABLE public.boards ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.boards FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.boards USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
CREATE SCHEMA stego_steps;
` + function + `;
CREATE TRIGGER boards_heading_sync BEFORE INSERT OR UPDATE ON public.boards FOR EACH ROW EXECUTE FUNCTION stego_steps.boards_heading_sync();
CREATE SCHEMA IF NOT EXISTS stego_schema;
` + lease.BackfillsTable + ";\n" + lease.BackfillFunctions + `
INSERT INTO stego_schema.backfills (change_id) VALUES ('board-title-rename');
`
}

// renameJob gives a fixture with the table boards of a rename board-title-rename at expand, 25 rows in three tenants
// of which 24 have no new value, and the migration of a job that backfills it in batches of 4, whose declared parts
// also have the manifest of boards.
func renameJob(t *testing.T) (*fixture, Migration, []Backfill) {
	t.Helper()
	return renameJobWith(t, nil)
}

// renameJobWith is renameJob with the tenants added by provision, or by the owner role straight into the tenant list
// when provision is nil.
func renameJobWith(t *testing.T, provision func(f *fixture, id string)) (*fixture, Migration, []Backfill) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	owner := func(statement string, args ...any) {
		t.Helper()
		tx, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+f.owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%.60s: %v", statement, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tenants, err := tenant.Migration("public", f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	owner(tenants)
	owner(lease.LeasesTable)
	owner(boardsDDL(t, f.owner))
	counts := map[string]int{"tenant-a": 10, "tenant-b": 0, "tenant-c": 15}
	for _, id := range []string{"tenant-c", "tenant-a", "tenant-b"} {
		if provision != nil {
			provision(f, id)
		} else {
			owner(`INSERT INTO public.stego_tenants (tenant_id) VALUES ($1)`, id)
		}
		for i := range counts[id] {
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{"SET LOCAL ROLE " + f.owner, "SELECT pg_catalog.set_config('app.tenant', '" + id + "', true)"} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.boards (id, title) VALUES (gen_random_uuid(), $1)`, fmt.Sprintf("%s-%02d", id, i)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The sync trigger of the release before copied each insert, so the backfill has rows to move only after the
	// new column is cleared, as for rows that the release before wrote before the step.
	// One row keeps the new value that the trigger gave it, so the backfill leaves it and moves the 24 others.
	unmove(t, f, "title <> 'tenant-c-03'")
	backfills, err := CopyBackfills([]lease.Copy{{Change: "board-title-rename", Table: "boards", TenantColumn: "tenant_id", From: "title", To: "heading", Check: "boards_heading_complete"}}, "public")
	if err != nil {
		t.Fatal(err)
	}
	backfills[0].Settings.BatchSize = 4
	m := f.migration(nil)
	m.Service, m.Backfills, m.BackfillLease = "svc", backfills, lease.Settings{TTL: 2 * time.Second, Renew: 500 * time.Millisecond, Retry: 100 * time.Millisecond}
	// The manifest names the table boards with its sync trigger, which each batch of the backfill accepts.
	declared := *m.Parts
	declared.Schema = manifest.Join(declared.Schema, boardsManifest(t, f.owner))
	m.Parts = &declared
	return f, m, backfills
}

func TestMigrateBackfill(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T05 The job backfills each open rename last, as the owner role, under its lease, tenant by tenant, and resumes from its checkpoint", func(t *testing.T) {
		f, m, backfills := renameJob(t)
		ctx := context.Background()
		// Two jobs at once: each takes the lease in turn, and the rows move once.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				job := m
				job.Holder = fmt.Sprintf("job-%d", i)
				errs[i] = runBackfills(ctx, f.admin, job)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("job %d: %v", i, err)
			}
		}
		var moved int64
		var completed bool
		var stored string
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL, last_key_type FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed, &stored); err != nil ||
			moved != 24 || !completed || stored != string(lease.KeyComposite) {
			t.Fatalf("the checkpoint has %d moved, complete %v, the key type %q (error %v); want 24, complete and composite", moved, completed, stored, err)
		}
		var unmoved int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.boards WHERE heading IS DISTINCT FROM title`).Scan(&unmoved); err != nil || unmoved != 0 {
			t.Fatalf("%d rows have no new value equal to the old one (error %v)", unmoved, err)
		}
		var leases int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.leases WHERE name = 'stego.backfill.board-title-rename'`).Scan(&leases); err != nil || leases != 1 {
			t.Fatalf("there are %d leases stego.backfill.board-title-rename (error %v), want 1", leases, err)
		}
		// A job that stops after its first batches resumes from the checkpoint in the next run, and moves each row once.
		unmove(t, f, "true")
		if _, err := f.admin.ExecContext(ctx, `UPDATE stego_schema.backfills SET last_key = NULL, last_key_type = NULL, rows_moved = 0, completed_at = NULL`); err != nil {
			t.Fatal(err)
		}
		stopCtx, stop := context.WithCancel(ctx)
		first := m
		first.Backfills = slices.Clone(backfills)
		first.Backfills[0].Settings.Observer = &stopAfter{rows: 8, stop: stop}
		if err := runBackfills(stopCtx, f.admin, first); err == nil {
			t.Fatal("the stopped job ended with no error")
		}
		var after int64
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&after); err != nil || after < 8 || after >= 25 {
			t.Fatalf("the stopped job moved %d rows (error %v), want at least 8 and fewer than 25", after, err)
		}
		// The resumed job loses its lease to another holder after its first batch: its next batch is fenced, and it
		// waits for the lease again and completes the change.
		resumed := m
		resumed.Backfills = slices.Clone(backfills)
		var once sync.Once
		took := make(chan int64, 1)
		step := resumed.Backfills[0].Step
		resumed.Backfills[0].Step = func(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
			once.Do(func() {
				go func() {
					// It waits for the lock of the batch's fence on the lease row, so it takes the lease after the batch.
					if _, err := f.admin.ExecContext(context.Background(), `UPDATE public.leases SET expires_at = now() - interval '1 second' WHERE name = 'stego.backfill.board-title-rename'`); err != nil {
						t.Error(err)
					}
					other, err := lease.New(f.admin, "stego.backfill.board-title-rename", "other", lease.Settings{TTL: time.Second, Renew: 500 * time.Millisecond})
					if err != nil {
						t.Error(err)
						return
					}
					held, err := other.Acquire(context.Background())
					if err != nil {
						t.Error(err)
						took <- 0
						return
					}
					took <- held.Token()
				}()
			})
			return step(ctx, tx, after, limit)
		}
		if err := runBackfills(ctx, f.admin, resumed); err != nil {
			t.Fatal(err)
		}
		if token := <-took; token == 0 {
			t.Fatal("the other holder did not take the lease")
		}
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil || moved != 25 || !completed {
			t.Fatalf("after the resumed job the checkpoint has %d moved, complete %v (error %v); want 25 and complete", moved, completed, err)
		}
	})
}

// runtimeGrants gives the runtime role of f what the migration job gives it next to a rename at expand: USAGE on
// stego_schema, where the schema generation marker is (the database access of the adapter), and SELECT and UPDATE on
// the lease table, the role of the lease client (controllerLease).
func runtimeGrants(t *testing.T, f *fixture) {
	t.Helper()
	for _, statement := range []string{"GRANT USAGE ON SCHEMA stego_schema TO " + f.runtime, "GRANT SELECT, UPDATE ON TABLE public.leases TO " + f.runtime} {
		if _, err := f.admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// sqlState gives the SQLSTATE of err, or "" for no error.
func sqlState(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState()
	}
	return err.Error()
}

// unmoved counts the rows of the boards table of renameJob whose new value is not the old one.
func unmoved(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.admin.QueryRow(`SELECT count(*) FROM public.boards WHERE heading IS DISTINCT FROM title`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateBackfillComplete(t *testing.T) {
	t.Run("ST-SUP-RC27-OC12-T02 A rename backfill over the tenants that Provision added, one of them retired, completes and keeps every row", func(t *testing.T) {
		ctx := context.Background()
		var pool *sql.DB
		f, m, _ := renameJobWith(t, func(f *fixture, id string) {
			t.Helper()
			if pool == nil {
				u, err := url.Parse(f.database)
				if err != nil {
					t.Fatal(err)
				}
				u.User = url.UserPassword(f.runtime, rolePassword)
				if pool, err = tenant.Open(ctx, tenant.Config{InsecureLoopback: true, DSN: u.String()}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pool.Close() })
			}
			// The tenants come into the list only through Provision, as the runtime role (ST-SUP-RP04-LC07).
			if err := tenant.Provision(ctx, pool, "public", mustTenant(t, id), nil); err != nil {
				t.Fatal(err)
			}
		})
		if err := tenant.Retire(ctx, pool, "public", mustTenant(t, "tenant-c"), nil); err != nil {
			t.Fatal(err)
		}
		if err := runBackfills(ctx, f.admin, m); err != nil {
			t.Fatalf("the backfill over the provisioned tenants failed: %v", err)
		}
		var moved int64
		var completed bool
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil ||
			moved != 24 || !completed {
			t.Fatalf("the checkpoint has %d moved and complete %v (error %v); want 24 and complete", moved, completed, err)
		}
		if n := unmoved(t, f); n != 0 {
			t.Fatalf("%d rows are not moved", n)
		}
		// The retired tenant keeps its 15 rows, each moved, and its row in the list with a retire time.
		var kept, retired int
		if err := f.admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.boards WHERE tenant_id = 'tenant-c' AND heading = title),
 (SELECT count(*) FROM public.stego_tenants WHERE tenant_id = 'tenant-c' AND retired_at IS NOT NULL)`).Scan(&kept, &retired); err != nil || kept != 15 || retired != 1 {
			t.Fatalf("the retired tenant keeps %d moved rows and %d retired list rows (error %v); want 15 and 1", kept, retired, err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T11 A row of a tenant that the tenant list does not hold blocks completion, the job fails and names the change, and the next run completes once the row is fixed", func(t *testing.T) {
		f, m, _ := renameJob(t)
		ctx := context.Background()
		// Two rows of tenant-d, which stego_tenants does not hold, as the release before wrote them before the step.
		for i := range 2 {
			if _, err := f.admin.ExecContext(ctx, `INSERT INTO public.boards (tenant_id, id, title) VALUES ('tenant-d', gen_random_uuid(), $1)`, fmt.Sprintf("tenant-d-%02d", i)); err != nil {
				t.Fatal(err)
			}
		}
		unmove(t, f, "tenant_id = 'tenant-d'")
		job := m
		job.Holder = "job-1"
		err := runBackfills(ctx, f.admin, job)
		if !errors.Is(err, lease.ErrIncomplete) {
			t.Fatalf("the job with unmoved rows of an unlisted tenant gave %v, want ErrIncomplete", err)
		}
		for _, want := range []string{"board-title-rename", "boards_heading_complete"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the failure %q does not name %s", err, want)
			}
		}
		if strings.Contains(err.Error(), "tenant-d") {
			t.Errorf("the failure %q holds a row value", err)
		}
		var completed, validated bool
		read := func() {
			t.Helper()
			if err := f.admin.QueryRowContext(ctx, `SELECT (SELECT completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'),
  (SELECT convalidated FROM pg_constraint WHERE conname = 'boards_heading_complete')`).Scan(&completed, &validated); err != nil {
				t.Fatal(err)
			}
		}
		read()
		if completed || validated {
			t.Fatalf("after the failed job the change is complete %v and its CHECK validated %v, want neither", completed, validated)
		}
		// The rows of the listed tenants moved; those of tenant-d did not.
		var unmoved int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.boards WHERE heading IS NULL`).Scan(&unmoved); err != nil || unmoved != 2 {
			t.Fatalf("%d rows have no new value (error %v), want the 2 of tenant-d", unmoved, err)
		}
		// The fix: tenant-d is provisioned. The next run passes over the rows again and completes.
		if _, err := f.admin.ExecContext(ctx, `INSERT INTO public.stego_tenants (tenant_id) VALUES ('tenant-d')`); err != nil {
			t.Fatal(err)
		}
		job.Holder = "job-2"
		if err := runBackfills(ctx, f.admin, job); err != nil {
			t.Fatalf("the next run after the fix: %v", err)
		}
		read()
		if !completed || !validated {
			t.Fatalf("after the next run the change is complete %v and its CHECK validated %v, want both", completed, validated)
		}
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.boards WHERE heading IS DISTINCT FROM title`).Scan(&unmoved); err != nil || unmoved != 0 {
			t.Fatalf("%d rows have no new value equal to the old one (error %v)", unmoved, err)
		}
	})
	t.Run("ST-SUP-RP14-LC03-T18 A backfill whose validation backend an administrator ends holds the lease again with new sessions and completes the change", func(t *testing.T) {
		f, m, backfills := renameJob(t)
		ctx := context.Background()
		// A session holds SHARE UPDATE EXCLUSIVE on the table, so the validation waits; its backend is ended with
		// pg_terminate_backend while it waits (SQLSTATE 57P01), which breaks the batch session of the hold (review
		// of PR #305, L3).
		hold, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback() }()
		if _, err := hold.ExecContext(ctx, "LOCK TABLE public.boards IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		backfills[0].Settings.LockTimeout = 20 * time.Second
		m.Backfills = backfills
		m.BackfillTimeout = 40 * time.Second
		// The job waits the retry period of the lease after a broken session (review of PR #305, info).
		m.BackfillLease.Retry = 3 * time.Second
		ended := make(chan int, 1)
		go func() {
			defer close(ended)
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				var n int
				_ = f.admin.QueryRowContext(ctx, `SELECT count(pg_catalog.pg_terminate_backend(pid)) FROM pg_catalog.pg_stat_activity
  WHERE query LIKE '%VALIDATE CONSTRAINT%boards_heading_complete%' AND wait_event_type = 'Lock' AND pid <> pg_catalog.pg_backend_pid()`).Scan(&n)
				if n > 0 {
					_ = hold.Rollback()
					ended <- n
					return
				}
			}
		}()
		start := time.Now()
		err = runBackfills(ctx, f.admin, m)
		if n := <-ended; n == 0 {
			t.Fatal("no validation was ended")
		}
		if err != nil {
			t.Fatalf("the backfill after the validation was ended gave %v after %v, want the change complete", err, time.Since(start).Round(time.Millisecond))
		}
		if took := time.Since(start); took > 25*time.Second || took < m.BackfillLease.Retry {
			t.Fatalf("the backfill took %v, want it to hold the lease again after the retry period %v and not to wait for its time limit", took, m.BackfillLease.Retry)
		}
		if !boardsCompleted(t, f) {
			t.Fatal("the change is not complete")
		}
	})
}

func TestBackfillFunctions(t *testing.T) {
	t.Run("ST-SUP-RC01-OC25-T02 The runtime role cannot forge, poison or lock the backfill checkpoint", func(t *testing.T) {
		f, m, _ := renameJob(t)
		ctx := context.Background()
		runtimeGrants(t, f)
		// The functions as the migration text makes them: SECURITY DEFINER of the owner, with the fixed search path,
		// and EXECUTE for the owner alone.
		rows, err := f.admin.Query(`SELECT p.proname, p.prosecdef, coalesce(p.proconfig::text, ''), pg_catalog.pg_get_userbyid(p.proowner),
 coalesce((SELECT string_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END, ',' ORDER BY 1)
  FROM pg_catalog.aclexplode(p.proacl) a WHERE a.privilege_type = 'EXECUTE'), 'PUBLIC by default')
 FROM pg_catalog.pg_proc p WHERE p.pronamespace = 'stego_schema'::regnamespace ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		functions := 0
		for rows.Next() {
			var name, config, owner, grantees string
			var definer bool
			if err := rows.Scan(&name, &definer, &config, &owner, &grantees); err != nil {
				t.Fatal(err)
			}
			functions++
			if !definer || config != `{"search_path=pg_catalog, pg_temp"}` || owner != f.owner || grantees != f.owner {
				t.Errorf("%s: SECURITY DEFINER %v, settings %s, owner %s, EXECUTE for %s; want true, the fixed search path, %s and %s alone", name, definer, config, owner, grantees, f.owner, f.owner)
			}
		}
		rows.Close()
		if functions != 2 {
			t.Fatalf("stego_schema has %d functions, want backfill_read and backfill_checkpoint", functions)
		}
		var public bool
		if err := f.admin.QueryRow(`SELECT has_function_privilege('public', 'stego_schema.backfill_read(text)', 'EXECUTE')
 OR has_function_privilege('public', 'stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text, text, text)', 'EXECUTE')`).Scan(&public); err != nil || public {
			t.Fatalf("PUBLIC may run a backfill function: %v (error %v)", public, err)
		}
		rt, err := sql.Open("pgx", f.runtimeURL())
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		leaseName := BackfillLeaseName("board-title-rename")
		// The runtime role can change a lease row (UPDATE on public.leases), so the forged calls name a lease row
		// that they match: only the missing grant refuses them.
		if _, err := f.admin.Exec(`INSERT INTO public.leases (name) VALUES ($1)`, leaseName); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Exec(`UPDATE public.leases SET holder = 'forger', expires_at = now() + interval '1 hour', token = 1 WHERE name = $1`, leaseName); err != nil {
			t.Fatal(err)
		}
		forged := []struct{ name, statement string }{
			{"a forged completion", `SELECT stego_schema.backfill_checkpoint('board-title-rename', NULL, 0, 0, 0, '', true, 1, NULL, '` + leaseName + `', 'forger')`},
			{"a token of the maximum value", `SELECT stego_schema.backfill_checkpoint('board-title-rename', NULL, 0, 0, 0, '', false, 9223372036854775807, NULL, '` + leaseName + `', 'forger')`},
			{"a FOR UPDATE lock of the row", `SELECT count(*) FROM stego_schema.backfill_read('board-title-rename')`},
			{"a direct read of the row", `SELECT count(*) FROM stego_schema.backfills`},
			{"a direct change of the row", `UPDATE stego_schema.backfills SET completed_at = now()`},
		}
		for _, call := range forged {
			var n int64
			if err := rt.QueryRow(call.statement).Scan(&n); sqlState(err) != "42501" {
				t.Errorf("as the runtime role %s gives %v, want SQLSTATE 42501", call.name, err)
			}
		}
		var token int64
		var completed bool
		if err := f.admin.QueryRow(`SELECT token, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&token, &completed); err != nil || token != 0 || completed {
			t.Fatalf("after the forged calls the checkpoint has the token %d, complete %v (error %v); want 0 and not complete", token, completed, err)
		}
		// The job then moves every row: nothing that the runtime role did stops it or makes it skip the change.
		if _, err := f.admin.Exec(`UPDATE public.leases SET expires_at = '-infinity' WHERE name = $1`, leaseName); err != nil {
			t.Fatal(err)
		}
		job := m
		job.BackfillTimeout = 20 * time.Second
		if err := runBackfills(ctx, f.admin, job); err != nil {
			t.Fatalf("the job after the forged calls: %v", err)
		}
		if n := unmoved(t, f); n != 0 {
			t.Fatalf("after the job %d rows have no new value", n)
		}
	})

	t.Run("ST-SUP-RP09-LC07-T02 The checkpoint function refuses the owner when the lease row does not hold its holder and token unexpired", func(t *testing.T) {
		f, _, _ := renameJob(t)
		ctx := context.Background()
		leaseName := BackfillLeaseName("board-title-rename")
		if _, err := f.admin.Exec(`INSERT INTO public.leases (name, holder, expires_at, token) VALUES ($1, 'job-a', now() + interval '1 hour', 3)`, leaseName); err != nil {
			t.Fatal(err)
		}
		// The lease of another change, held by the same holder with the same token (recheck of PR #291).
		if _, err := f.admin.Exec(`INSERT INTO public.leases (name, holder, expires_at, token) VALUES ('stego.backfill.other-change', 'job-a', now() + interval '1 hour', 3)`); err != nil {
			t.Fatal(err)
		}
		// checkpoint runs the function as the owner role and gives the count of rows it changed.
		checkpoint := func(token int64, name, holder string) int64 {
			t.Helper()
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec("SET LOCAL ROLE " + f.owner); err != nil {
				t.Fatal(err)
			}
			var n int64
			if err := tx.QueryRow(`SELECT stego_schema.backfill_checkpoint('board-title-rename', '\x01'::bytea, 5, 0, 0, '', false, $1, 'uuid', $2, $3)`, token, name, holder).Scan(&n); err != nil {
				t.Fatalf("the checkpoint as the owner role: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			return n
		}
		for _, c := range []struct {
			name, lease, holder string
			token               int64
		}{
			{"a stale token", leaseName, "job-a", 2},
			{"a token above the lease row's", leaseName, "job-a", 4},
			{"another holder", leaseName, "job-b", 3},
			{"another lease row", "stego.backfill.other", "job-a", 3},
			{"the lease of another change", "stego.backfill.other-change", "job-a", 3},
		} {
			if n := checkpoint(c.token, c.lease, c.holder); n != 0 {
				t.Errorf("the checkpoint with %s changed %d rows, want 0", c.name, n)
			}
		}
		if n := checkpoint(3, leaseName, "job-a"); n != 1 {
			t.Fatalf("the checkpoint of the holder with its token changed %d rows, want 1", n)
		}
		if _, err := f.admin.Exec(`UPDATE public.leases SET expires_at = now() - interval '1 second' WHERE name = $1`, leaseName); err != nil {
			t.Fatal(err)
		}
		if n := checkpoint(3, leaseName, "job-a"); n != 0 {
			t.Errorf("the checkpoint under an expired lease changed %d rows, want 0", n)
		}
		var moved, token int64
		if err := f.admin.QueryRow(`SELECT rows_moved, token FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &token); err != nil || moved != 5 || token != 3 {
			t.Fatalf("the checkpoint row has %d moved and the token %d (error %v), want 5 and 3 from the one write of the holder", moved, token, err)
		}
	})
}

// poolSessions takes n sessions of db at once and gives the current user, the session user and the search path of
// each.
func poolSessions(t *testing.T, db *sql.DB, n int) [][3]string {
	t.Helper()
	ctx := context.Background()
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	var out [][3]string
	for range n {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var s [3]string
		if err := c.QueryRowContext(ctx, `SELECT current_user, session_user, pg_catalog.current_setting('search_path')`).Scan(&s[0], &s[1], &s[2]); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestMigrateBackfillInherited(t *testing.T) {
	t.Run("ST-SUP-RP09-LC09-T07 The copy batch of a rename updates only its table, so a table that inherits from it has no row changed and no trigger fired", func(t *testing.T) {
		f, m, _ := renameJob(t)
		ctx := context.Background()
		// Review of PR #311 (info): a child that inherits from boards, whose row has the key of an unmoved row of
		// boards, and whose own row trigger the trigger check of boards does not read.
		for _, statement := range []string{
			"SET ROLE " + f.owner,
			"CREATE TABLE public.lc09t07_log (n int)",
			"CREATE TABLE public.boards_child (tenant_id text NOT NULL, id uuid NOT NULL, title text NOT NULL, heading text)",
			"SELECT pg_catalog.set_config('app.tenant', 'tenant-a', false)",
			"INSERT INTO public.boards_child (tenant_id, id, title) SELECT tenant_id, id, title FROM ONLY public.boards WHERE heading IS NULL AND tenant_id = 'tenant-a' LIMIT 1",
			"RESET app.tenant",
			// The child needs the CHECK of boards to inherit from it; NOT VALID keeps its unmoved row.
			"ALTER TABLE public.boards_child ADD CONSTRAINT boards_heading_complete CHECK ((heading IS NOT NULL) OR (title IS NULL)) NOT VALID",
			"ALTER TABLE public.boards_child INHERIT public.boards",
			`CREATE FUNCTION public.lc09t07_f() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO public.lc09t07_log VALUES (1); RETURN NEW; END$$`,
			"CREATE TRIGGER lc09t07_child BEFORE UPDATE ON public.boards_child FOR EACH ROW EXECUTE FUNCTION public.lc09t07_f()",
			"RESET ROLE",
		} {
			if _, err := f.admin.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		err := runBackfills(ctx, f.admin, m)
		var fired, childMoved, parentUnmoved int
		if err := f.admin.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM public.lc09t07_log), (SELECT count(*) FROM ONLY public.boards_child WHERE heading IS NOT NULL),
  (SELECT count(*) FROM ONLY public.boards WHERE heading IS NULL)`).Scan(&fired, &childMoved, &parentUnmoved); err != nil {
			t.Fatal(err)
		}
		if fired != 0 || childMoved != 0 || parentUnmoved != 0 {
			t.Fatalf("the backfill (%v) fired the child's trigger %d times, moved %d child rows and left %d rows of boards unmoved, want 0, 0 and 0", err, fired, childMoved, parentUnmoved)
		}
	})
}

func TestMigrateBackfillSessions(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T06 After an error and after a cancel in the backfill step no pooled session runs as the owner role", func(t *testing.T) {
		cases := []struct {
			name    string
			step    lease.Step
			timeout time.Duration
		}{
			{"a batch that fails in SQL", func(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
				_, err := tx.ExecContext(ctx, `SELECT 1 / 0`)
				return lease.Batch{}, err
			}, 0},
			{"a batch that the end of the job cancels in its statement", func(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
				_, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_sleep(5)`)
				return lease.Batch{}, err
			}, 1500 * time.Millisecond},
		}
		for _, c := range cases {
			f, m, backfills := renameJob(t)
			pool, err := sql.Open("pgx", f.database)
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(4)
			pool.SetMaxIdleConns(4)
			var path string
			if err := pool.QueryRow(`SELECT pg_catalog.current_setting('search_path')`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			job := m
			job.Backfills = slices.Clone(backfills)
			job.Backfills[0].Step = c.step
			job.BackfillTimeout = 2 * time.Second
			ctx, cancel := context.WithCancel(context.Background())
			if c.timeout > 0 {
				ctx, cancel = context.WithTimeout(context.Background(), c.timeout)
			}
			err = runBackfills(ctx, pool, job)
			cancel()
			if err == nil {
				t.Errorf("%s: the backfill step ended with no error", c.name)
			}
			for _, s := range poolSessions(t, pool, 4) {
				if s[0] == f.owner || s[0] != s[1] || s[2] != path {
					t.Errorf("%s: a pooled session runs as %s (session user %s) with the search path %q; want the login role and %q", c.name, s[0], s[1], s[2], path)
				}
			}
			_ = pool.Close()
		}
	})
}

func TestMigrateBackfillSessionEnd(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T25 A transaction of a backfill session ends only on its own goroutine, so a cancel never closes the session under its reset", func(t *testing.T) {
		db, err := sql.Open("pgx", pgtest.Database(t, "lc03t25"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(2)
		background := context.Background()
		session := func() *sql.Conn {
			t.Helper()
			conn, err := db.Conn(background)
			if err != nil {
				t.Fatal(err)
			}
			return conn
		}
		// The end of the context of an idle transaction does not roll it back on another goroutine: the
		// transaction stays open until the caller ends it, and the session then runs the reset.
		conn := session()
		ctx, cancel := context.WithCancel(background)
		tx, err := endedTxConn{conn}.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		time.Sleep(200 * time.Millisecond)
		if _, err := tx.ExecContext(background, `SELECT 1`); err != nil {
			t.Fatalf("a statement in the transaction after the end of its context: %v, want the transaction still open", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("the rollback of the caller: %v", err)
		}
		if _, err := conn.ExecContext(background, "RESET ROLE; RESET search_path"); err != nil {
			t.Fatalf("the reset after the rollback: %v", err)
		}
		_ = conn.Close()
		// A context that has ended begins no transaction.
		conn = session()
		if _, err := (endedTxConn{conn}).BeginTx(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("a begin with a context that has ended: %v, want context.Canceled", err)
		}
		_ = conn.Close()
		// The context of the caller still cancels a statement in the transaction, and the reset then runs on the
		// goroutine of the caller with no panic, whatever the delay after the rollback (the race of the
		// TestMigrateBackfillSessions panic, about 2 in 3000 with no fix).
		for i := range 300 {
			conn := session()
			ctx, cancel := context.WithTimeout(background, 2*time.Millisecond)
			started := time.Now()
			if tx, err := (endedTxConn{conn}).BeginTx(ctx, nil); err == nil {
				if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_sleep(5)`); err == nil {
					t.Fatal("a statement whose context ends after 2 ms slept 5 s")
				}
				_ = tx.Rollback()
			}
			cancel()
			if took := time.Since(started); took > 3*time.Second {
				t.Fatalf("a statement whose context ends after 2 ms took %v", took)
			}
			for spin := time.Now(); time.Since(spin) < time.Duration(i*37%400)*time.Microsecond; {
			}
			_, _ = conn.ExecContext(background, "RESET ROLE; RESET search_path")
			_ = conn.Close()
		}
	})
}

func TestMigrateBackfillSessionRelease(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T26 A backfill session whose reset fails is closed and does not go back to the pool", func(t *testing.T) {
		// The pgx driver already drops a session that is in a transaction, and a reset of a session that is idle
		// and open does not fail on PostgreSQL, so a driver whose reset fails on request stands in for a failure
		// that the release cannot foresee.
		d := &resetDriver{}
		sql.Register("lc03t26", d)
		db, err := sql.Open("lc03t26", "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		ctx := context.Background()
		release := func() {
			t.Helper()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			releaseBackfillSession(ctx, endedTxConn{conn})
		}
		// A session whose reset passes goes back to the pool, and the next session is the same one.
		release()
		release()
		if d.opened != 1 || d.closed != 0 || d.resets != 2 {
			t.Fatalf("two releases with resets that pass opened %d sessions and closed %d (%d resets), want one session open and two resets", d.opened, d.closed, d.resets)
		}
		// A session whose reset fails is closed, and the next session is a new one.
		d.fail = true
		release()
		if d.closed != 1 {
			t.Fatalf("a release whose reset fails closed %d sessions, want the session closed", d.closed)
		}
		d.fail = false
		release()
		if d.opened != 2 || d.closed != 1 {
			t.Fatalf("the release after a failed reset opened %d sessions in all and closed %d, want a new session", d.opened, d.closed)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T29 The reset of a backfill session runs after the end of the job's context, for at most its timeout", func(t *testing.T) {
		d := &resetDriver{}
		sql.Register("lc03t27", d)
		db, err := sql.Open("lc03t27", "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The context of the job has ended when the session goes back: the reset still runs, and it has a deadline
		// of at most backfillResetTimeout, so a server that does not answer cannot hold the release.
		job, cancel := context.WithCancel(context.Background())
		cancel()
		before := time.Now()
		releaseBackfillSession(job, endedTxConn{conn})
		if d.resets != 1 || d.last == nil {
			t.Fatalf("a release after the end of the job ran %d resets, want one", d.resets)
		}
		deadline, ok := d.last.Deadline()
		if !ok || deadline.After(time.Now().Add(backfillResetTimeout)) || deadline.Before(before) {
			t.Errorf("the reset had the deadline %v (set: %t), want one at most %v after the release", deadline, ok, backfillResetTimeout)
		}
	})
}

// resetDriver is a database/sql driver whose sessions run only the reset of a backfill session, which fails while
// fail is set. It counts the sessions it opens and closes and the resets that pass, and keeps the context of the
// last reset.
type resetDriver struct {
	fail                   bool
	opened, closed, resets int
	last                   context.Context
}

func (d *resetDriver) Open(string) (driver.Conn, error) {
	d.opened++
	return resetConn{d}, nil
}

type resetConn struct{ d *resetDriver }

func (c resetConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no statements") }
func (c resetConn) Begin() (driver.Tx, error)           { return nil, errors.New("no transactions") }
func (c resetConn) Close() error {
	c.d.closed++
	return nil
}

func (c resetConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if query != "RESET ROLE; RESET search_path" {
		return nil, fmt.Errorf("unexpected statement %q", query)
	}
	c.d.last = ctx
	// As the pgx driver does, a statement whose context has ended does not run.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.d.fail {
		return nil, errors.New("the reset failed")
	}
	c.d.resets++
	return driver.RowsAffected(0), nil
}

func TestMigrateBackfillStall(t *testing.T) {
	t.Run("ST-SUP-RP09-LC12-T02 The migration job emits a stall, runs on until its backfill time limit, and its next run resumes", func(t *testing.T) {
		f, m, backfills := renameJob(t)
		ctx := context.Background()
		// A session holds a row of tenant-c, so the batches of tenant-a move and the first batch of tenant-c waits.
		lock, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback() }()
		if _, err := lock.ExecContext(ctx, `SELECT id FROM public.boards WHERE title = 'tenant-c-07' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		// The lease, its renew, the retry of the batch, the stall watch and the time limit run on a hand clock. The
		// renew interval and the retry period are one tick, so each tick ends the three waits of the job at once.
		// The lease of 1 min is long on the database clock: no renew is late while the test runs.
		clock := newBackfillClock()
		ttl, tick := time.Minute, 20*time.Second
		job := m
		job.Backfills = slices.Clone(backfills)
		job.BackfillLease = lease.Settings{TTL: ttl, Renew: tick, Retry: tick, Clock: clock}
		job.BackfillTimeout = 12 * tick
		seen := &stallRecorder{now: clock.Now}
		job.Backfills[0].Settings.Observer = seen
		job.Backfills[0].Settings.LockTimeout = 200 * time.Millisecond
		// No pause after a batch: the batches that commit do not wait on the clock, so they all commit at the start.
		job.Backfills[0].Settings.Pause = 0
		start := clock.Now()
		done := make(chan error, 1)
		go func() { done <- runBackfillsWithin(ctx, f.admin, job, clock.deadline) }()
		// The job is at rest when the renew, the watch and the retry of the waiting batch wait on the clock. It asks
		// for its time limit before it starts, so the limit is there by then.
		clock.waits(t, 3, tick)
		select {
		case got := <-clock.limits:
			if got != job.BackfillTimeout {
				t.Fatalf("the backfill step has a time limit of %v, want %v", got, job.BackfillTimeout)
			}
		default:
			t.Fatal("the backfill step has no time limit on the clock")
		}
		for n := 1; n < 12; n++ {
			clock.Advance(tick)
			clock.waits(t, 3, tick)
			select {
			case err := <-done:
				t.Fatalf("the stalled job ended at %v, before its backfill time limit of %v: %v", clock.Now().Sub(start), job.BackfillTimeout, err)
			default:
			}
			// Three lease durations with no batch are nine ticks: the stall comes at that tick, and once.
			stalls, lastRows := seen.state()
			want := 0
			if n >= 9 {
				want = 1
			}
			if len(stalls) != want || (want == 1 && stalls[0].Sub(lastRows) != 3*ttl) || !lastRows.Equal(start) {
				t.Fatalf("after %v schema.backfill.stalled came %d times (%v; last batch at %v), want %d, at three TTLs after the last batch", clock.Now().Sub(start), len(stalls), stalls, lastRows, want)
			}
		}
		// The time limit ends the step.
		clock.Advance(tick)
		if err := <-done; err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the stalled job ends with %v, want the end of its backfill time limit", err)
		}
		var moved int64
		var completed bool
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil ||
			moved < 10 || moved >= 24 || completed {
			t.Fatalf("after the stalled job the checkpoint has %d moved, complete %v (error %v); want the 10 rows of tenant-a at least, fewer than 24, and not complete", moved, completed, err)
		}
		// The time limit releases the lease, whether it ends the job before or while a renew runs: the row keeps the
		// holder's token and expires at once, so the next run does not wait one lease duration for it (review of PR
		// #387, L2).
		var released bool
		if err := f.admin.QueryRowContext(ctx, `SELECT expires_at = '-infinity' FROM public.leases WHERE name = $1`, BackfillLeaseName("board-title-rename")).Scan(&released); err != nil || !released {
			t.Fatalf("after the time limit the lease of the stalled job is released %v (error %v), want released", released, err)
		}
		// The next run, after the lock is released, resumes from the checkpoint and moves each other row once.
		_ = lock.Rollback()
		next := m
		next.Backfills = slices.Clone(backfills)
		resumed := &stallRecorder{}
		next.Backfills[0].Settings.Observer = resumed
		if err := runBackfills(ctx, f.admin, next); err != nil {
			t.Fatal(err)
		}
		if got := resumed.moved(); got != 24-moved {
			t.Fatalf("the next run moved %d rows, want the %d that the stalled job left", got, 24-moved)
		}
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil || moved != 24 || !completed {
			t.Fatalf("after the next run the checkpoint has %d moved, complete %v (error %v); want 24 and complete", moved, completed, err)
		}
		if stalls, _ := resumed.state(); len(stalls) != 0 {
			t.Fatalf("the next run emitted %d stalls, want none", len(stalls))
		}
	})
}

// backfillClock is a lease clock that moves only when the test advances it, for ST-SUP-RP09-LC12-T02. Each call of
// After sends its duration on asked, so the test knows when the job waits. Its deadline gives the time limit of the
// backfill step on the same clock: each limit sends its duration on limits, and ends with
// context.DeadlineExceeded when the test moves the clock to it.
type backfillClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []backfillWaiter
	limits  chan time.Duration
	asked   chan time.Duration
	ends    []*handLimit
}

type backfillWaiter struct {
	at time.Time
	ch chan time.Time
}

func newBackfillClock() *backfillClock {
	return &backfillClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), limits: make(chan time.Duration, 16),
		asked: make(chan time.Duration, 256)}
}

func (c *backfillClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *backfillClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
	} else {
		c.waiters = append(c.waiters, backfillWaiter{at: c.now.Add(d), ch: ch})
	}
	c.asked <- d
	return ch
}

// Advance moves the clock, fires each timer that is due and ends each time limit that is due.
func (c *backfillClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
	for _, end := range c.ends {
		if !end.at.After(c.now) {
			end.end(context.DeadlineExceeded)
		}
	}
}

// waits receives the next n waits of the job on the clock, and fails the test when one is not of d.
func (c *backfillClock) waits(t *testing.T, n int, d time.Duration) {
	t.Helper()
	for range n {
		if got := <-c.asked; got != d {
			t.Fatalf("the job waits %v on the clock, want %v", got, d)
		}
	}
}

func (c *backfillClock) deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx := &handLimit{parent: parent, done: make(chan struct{})}
	c.mu.Lock()
	ctx.at = c.now.Add(d)
	c.ends = append(c.ends, ctx)
	c.mu.Unlock()
	stop := context.AfterFunc(parent, func() { ctx.end(parent.Err()) })
	c.limits <- d
	return ctx, func() {
		stop()
		ctx.end(context.Canceled)
	}
}

// handLimit is a context that ends when its clock reaches its time, with context.DeadlineExceeded as a context of
// context.WithTimeout does, when its parent ends, or when it is cancelled.
type handLimit struct {
	parent context.Context
	at     time.Time
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func (h *handLimit) end(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
		close(h.done)
	}
}

func (h *handLimit) Deadline() (time.Time, bool) { return h.parent.Deadline() }
func (h *handLimit) Done() <-chan struct{}       { return h.done }
func (h *handLimit) Value(key any) any           { return h.parent.Value(key) }
func (h *handLimit) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// stallRecorder records the stalls of a backfill, the time of its last batch and the rows it moved.
type stallRecorder struct {
	// now is the clock of the times; nil is time.Now.
	now    func() time.Time
	mu     sync.Mutex
	stalls []time.Time
	last   time.Time
	rows   int64
}

func (s *stallRecorder) BackfillRows(_ string, rows int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows += rows
	s.last = s.clock()
}
func (s *stallRecorder) BackfillFailedRows(string, int64)      {}
func (s *stallRecorder) BackfillDuplicateGroups(string, int64) {}
func (s *stallRecorder) BackfillCompleted(string)              {}
func (s *stallRecorder) BackfillThrottled(string, string)      {}
func (s *stallRecorder) BackfillStalled(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalls = append(s.stalls, s.clock())
}

func (s *stallRecorder) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *stallRecorder) state() ([]time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.stalls), s.last
}

func (s *stallRecorder) moved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// stopAfter ends the context of a backfill once it has moved the given rows.
type stopAfter struct {
	mu    sync.Mutex
	rows  int64
	moved int64
	stop  context.CancelFunc
}

func (s *stopAfter) BackfillRows(_ string, rows int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moved += rows; s.moved >= s.rows {
		s.stop()
	}
}
func (s *stopAfter) BackfillFailedRows(string, int64)      {}
func (s *stopAfter) BackfillDuplicateGroups(string, int64) {}
func (s *stopAfter) BackfillCompleted(string)              {}
func (s *stopAfter) BackfillStalled(string)                {}
func (s *stopAfter) BackfillThrottled(string, string)      {}

func TestMigrateLedgerRole(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T02 Migrate runs the ledger of the adapter as the owner role, not as the login of the job", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		m := f.migration(nil)
		var current, session string
		ledger := m.Ledger
		m.Ledger = func(ctx context.Context, conn *sql.Conn) error {
			if err := conn.QueryRowContext(ctx, "SELECT current_user, session_user").Scan(&current, &session); err != nil {
				return err
			}
			return ledger(ctx, conn)
		}
		if err := Migrate(context.Background(), settings, m); err != nil {
			t.Fatal(err)
		}
		login := mustURL(t, f.database).User.Username()
		if current != f.owner || session != login {
			t.Fatalf("the ledger ran as %s in a session of %s, want the owner role %s in the session of the login %s", current, session, f.owner, login)
		}
		var owner string
		if err := f.admin.QueryRow("SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'orders'").Scan(&owner); err != nil || owner != f.owner {
			t.Fatalf("the table of the ledger is owned by %q (%v), want the owner role %s", owner, err, f.owner)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T04 Migrate refuses an outbox identity sequence that is behind its stored messages", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// A stored message at sequence 100, as after a failover that carried the rows and not the sequence.
		if _, err := f.admin.Exec(`INSERT INTO stego_outbox.messages (id, sequence, tenant_id, destination, resource_key, kind, payload, outgoing_key,
 created_at, available_at) OVERRIDING SYSTEM VALUE VALUES (gen_random_uuid(), 100, 't1', 'd', 'r', 'k', 'p', 'o', now(), now())`); err != nil {
			t.Fatal(err)
		}
		for _, behind := range []string{"SELECT setval('stego_outbox.messages_sequence_seq', 5)", "SELECT setval('stego_outbox.messages_sequence_seq', 100, false)"} {
			if _, err := f.admin.Exec(behind); err != nil {
				t.Fatal(err)
			}
			err := Migrate(context.Background(), settings, f.migration(nil))
			if Stage(err) != "migrate."+StepOutboxSequence || !errors.Is(err, errSequenceBehind) {
				t.Fatalf("Migrate after %q gave %v at the stage %q, want the refusal of the sequence", behind, err, Stage(err))
			}
		}
		// The sequence past the highest message: the job runs.
		if _, err := f.admin.Exec("SELECT setval('stego_outbox.messages_sequence_seq', 100)"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("Migrate with the sequence at the highest message: %v", err)
		}
		// An enqueue that commits between the two reads of the check, as under live traffic, does not make the healthy
		// queue look behind (#256 G1).
		betweenSequenceReads = func() {
			if _, err := f.admin.Exec(`INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload, outgoing_key,
 created_at, available_at) VALUES (gen_random_uuid(), 't1', 'd', 'r', 'k', 'p', 'o', now(), now())`); err != nil {
				t.Error(err)
			}
		}
		defer func() { betweenSequenceReads = nil }()
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("Migrate with an enqueue between the reads of the check: %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T03 Migrate makes the tables of the runtime in public when a schema is named after the owner role", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		// The default search path "$user", public of the owner puts a schema of its name first (#245 Q2).
		if _, err := f.admin.Exec("CREATE SCHEMA " + f.owner + " AUTHORIZATION " + f.owner); err != nil {
			t.Fatal(err)
		}
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		var keys string
		if err := f.admin.QueryRow("SELECT pg_catalog.to_regclass('public.idempotency_keys')::text").Scan(&keys); err != nil || keys == "" {
			t.Fatalf("the key table is not in public: %q, %v", keys, err)
		}
		var elsewhere int
		if err := f.admin.QueryRow("SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1", f.owner).Scan(&elsewhere); err != nil || elsewhere != 0 {
			t.Fatalf("the schema of the owner holds %d relations (%v), want none", elsewhere, err)
		}
	})
}

func TestMigrateLocks(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T07 A run again of the job waits at most the lock timeout behind a long transaction, and the reads of the table go on", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// A purge policy of the key table is gone, so the run again has a change to make, which needs ACCESS
		// EXCLUSIVE on the table; a run with nothing to change takes no such lock (T11).
		if _, err := f.admin.Exec("DROP POLICY idempotency_keys_purge_delete ON public.idempotency_keys"); err != nil {
			t.Fatal(err)
		}
		// A long transaction of the running release reads the key table and holds it.
		long, err := f.admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = long.Rollback() }()
		if _, err := long.Exec("SELECT count(*) FROM public.idempotency_keys"); err != nil {
			t.Fatal(err)
		}
		waits := make(chan string, 64)
		retried = func(step string) { waits <- step }
		defer func() { retried = nil }()
		m := f.migration(nil)
		m.Timeout = 2 * time.Minute
		ran := make(chan error, 1)
		go func() { ran <- Migrate(context.Background(), settings, m) }()
		reader, err := f.admin.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if _, err := reader.ExecContext(context.Background(), "SET statement_timeout = '2s'"); err != nil {
			t.Fatal(err)
		}
		read := func(what string) {
			var n int
			if err := reader.QueryRowContext(context.Background(), "SELECT count(*) FROM public.idempotency_keys").Scan(&n); err != nil {
				t.Fatalf("a read of the key table %s failed: %v", what, err)
			}
		}
		// The reader reads the key table while the job waits for its lock, until the job's lock wait has ended at the
		// lock timeout twice; the job that waited with no lock timeout would never get there, and each read would
		// queue behind it until its statement timeout.
		for seen := 0; seen < 2; {
			select {
			case step := <-waits:
				if step != StepIdempotencyKeys {
					t.Fatalf("the step %s waited for a lock, want the idempotency step", step)
				}
				seen++
			case err := <-ran:
				t.Fatalf("the job ended with %v while the long transaction held the key table, want it to wait", err)
			case <-time.After(30 * time.Second):
				t.Fatal("the job's lock wait did not end at the lock timeout within 30 s")
			default:
				read("while the job waits")
			}
		}
		read("after two lock waits of the job")
		// The long transaction ends, and the job runs the text again and passes.
		if err := long.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-ran:
			if err != nil {
				t.Fatalf("the job after the long transaction ended gave %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the job did not end within 60 s after the long transaction ended")
		}
	})

	t.Run("ST-SUP-RP14-LC03-T12 A run again of the job with nothing to change takes no lock that conflicts with the writes of the running release, and a run after a change of the catalog makes the objects again", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		before := catalogObjects(t, f.admin)
		// A transaction of the running release holds ROW EXCLUSIVE, the lock of a write, on each table that the job
		// made, apart from the table of the test ledger, whose text the adapter's ledger runs only once. A statement
		// of the job that needs SHARE or a stronger lock on one of them waits until its lock timeout.
		writer, err := f.admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.Rollback() }()
		var tables []string
		rows, err := writer.Query(`SELECT pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname) FROM pg_catalog.pg_class AS c
 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p') AND n.nspname IN ('public', 'stego_outbox', 'stego_audit_sink')
 AND c.relname <> 'orders' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, table)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"public.idempotency_keys", "stego_outbox.messages", "public.audit_event", "public.authz_revocation_tuples", "stego_audit_sink.entries"} {
			if !slices.Contains(tables, table) {
				t.Fatalf("the job made the tables %v, want %s among them", tables, table)
			}
		}
		if _, err := writer.Exec("LOCK TABLE " + strings.Join(tables, ", ") + " IN ROW EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		waits := make(chan string, 64)
		retried = func(step string) { waits <- step }
		defer func() { retried = nil }()
		ran := make(chan error, 1)
		go func() { ran <- Migrate(context.Background(), settings, f.migration(nil)) }()
		select {
		case step := <-waits:
			_ = writer.Rollback()
			<-ran
			t.Fatalf("the step %s of a run with nothing to change waited for a lock that a write of the release holds", step)
		case err := <-ran:
			if err != nil {
				t.Fatalf("the run with nothing to change gave %v while the release writes", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the run with nothing to change did not end within 60 s")
		}
		if err := writer.Rollback(); err != nil {
			t.Fatal(err)
		}
		if after := catalogObjects(t, f.admin); !slices.Equal(after, before) {
			t.Fatalf("the run with nothing to change changed the catalog:\n%v\nwant\n%v", after, before)
		}
		// Each kind of object changes in the catalog: row-level security that is no longer forced, a dropped and a
		// changed policy, a policy made again as restrictive, a policy on other roles, a dropped index, a dropped
		// trigger, a disabled trigger and a dropped constraint trigger. The run again makes each one as the text
		// states it.
		for _, statement := range []string{
			"ALTER TABLE stego_outbox.messages NO FORCE ROW LEVEL SECURITY",
			"ALTER TABLE public.audit_chain_heads DISABLE ROW LEVEL SECURITY",
			"DROP POLICY idempotency_keys_purge_delete ON public.idempotency_keys",
			// The same name, command, roles and expression, but restrictive: Policy.Holds reads polpermissive (#301 L1).
			"DROP POLICY idempotency_keys_purge_select ON public.idempotency_keys",
			"CREATE POLICY idempotency_keys_purge_select ON public.idempotency_keys AS RESTRICTIVE FOR SELECT TO " + f.purge + " USING (true)",
			"ALTER POLICY messages_tenant ON stego_outbox.messages USING (true)",
			"ALTER POLICY tenant_wall ON public.audit_event WITH CHECK (true)",
			"ALTER POLICY authz_revocations_tenant ON public.authz_revocations TO " + f.runtime,
			"ALTER POLICY messages_relay_select ON stego_outbox.messages TO " + f.runtime + ", " + f.outboxRelay,
			"DROP INDEX stego_outbox.messages_done",
			"DROP INDEX public.idempotency_keys_expires_at",
			"DROP INDEX public.authz_revocation_tuples_revocation",
			"DROP TRIGGER audit_retention_checkpoints_reject_statement ON public.audit_retention_checkpoints",
			"ALTER TABLE public.audit_anchor_boundaries DISABLE TRIGGER audit_anchor_boundaries_guard",
			"DROP TRIGGER authz_revocation_tuple_check ON public.authz_revocation_tuples",
		} {
			if _, err := f.admin.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if changed := catalogObjects(t, f.admin); slices.Equal(changed, before) {
			t.Fatal("the changes of the catalog are not seen")
		}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		if after := catalogObjects(t, f.admin); !slices.Equal(after, before) {
			t.Fatalf("the run after the changes of the catalog left:\n%v\nwant\n%v", after, before)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T13 The migration job refuses before its backfills and at its end a database that does not satisfy the schema manifest, which the start of a pod only alerts", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// The same change that ST-SUP-RP14-LC06-T01 starts on with an alert. No migration text makes the column
		// again, so the database still does not satisfy the manifest when the job ends its texts.
		if _, err := f.admin.Exec("ALTER TABLE public.orders ALTER COLUMN status TYPE character varying(20)"); err != nil {
			t.Fatal(err)
		}
		var steps []string
		stepped = func(step string, _ *sql.Conn) { steps = append(steps, step) }
		defer func() { stepped = nil }()
		err := Migrate(context.Background(), settings, f.migration(nil))
		if Stage(err) != "migrate."+StepManifestBefore || !errors.Is(err, manifest.ErrNotSatisfied) ||
			!strings.Contains(err.Error(), "table public.orders: column status has the type character varying(20)") {
			t.Fatalf("the job on a database with another column type gave %v at the stage %q, want the refusal of the step %s that names the column", err, Stage(err), StepManifestBefore)
		}
		if len(steps) < 3 || steps[len(steps)-1] != StepManifestBefore || steps[len(steps)-2] != StepSchemaServed || steps[len(steps)-3] != StepSchemaAgain {
			t.Fatalf("the refused job ran the steps %v, want it to end at %s, after %s and %s and before any backfill", steps, StepManifestBefore, StepSchemaAgain, StepSchemaServed)
		}
		if _, err := f.admin.Exec("ALTER TABLE public.orders ALTER COLUMN status TYPE text"); err != nil {
			t.Fatal(err)
		}
		// A change of the catalog while the backfills run is refused at the end of the job.
		steps = nil
		stepped = func(step string, _ *sql.Conn) {
			steps = append(steps, step)
			if step == StepBackfill {
				if _, err := f.admin.Exec("ALTER TABLE public.orders ALTER COLUMN status TYPE character varying(20)"); err != nil {
					t.Error(err)
				}
			}
		}
		err = Migrate(context.Background(), settings, f.migration(nil))
		if Stage(err) != "migrate."+StepManifest || !errors.Is(err, manifest.ErrNotSatisfied) ||
			!strings.Contains(err.Error(), "table public.orders: column status has the type character varying(20)") {
			t.Fatalf("the job whose database changed in its backfills gave %v at the stage %q, want the refusal of the step %s", err, Stage(err), StepManifest)
		}
		if len(steps) < 2 || steps[len(steps)-1] != StepManifest || steps[len(steps)-2] != StepBackfill {
			t.Fatalf("the refused job ran the steps %v, want it to end at %s, after %s", steps, StepManifest, StepBackfill)
		}
		stepped = nil
		// The parts are needed: a job with none is refused at the start.
		bare := f.migration(nil)
		bare.Parts = nil
		if err := Migrate(context.Background(), settings, bare); Stage(err) != "migrate."+StepSettings {
			t.Fatalf("the job with no declared parts gave %v at the stage %q, want %s", err, Stage(err), StepSettings)
		}
		if _, err := f.admin.Exec("ALTER TABLE public.orders ALTER COLUMN status TYPE text"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("the job after the column was made again gave %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T27 The job gives the worker role the least grants of a consuming worker's writes, and takes them back when no consumer is left", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		consuming := func(tables ...string) Migration {
			m := f.migration(nil)
			m.Parts.Workers.Consumers = func(*command.Runtime, *outbox.Worker, func(string) command.WorkerObserver) error { return nil }
			m.Parts.Workers.ConsumerTables = tables
			return m
		}
		privileges := func(table string) string {
			var got []string
			for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				var has bool
				if err := f.admin.QueryRow(`SELECT has_table_privilege($1, $2, $3)`, f.worker, table, privilege).Scan(&has); err != nil {
					t.Fatal(err)
				}
				if has {
					got = append(got, privilege)
				}
			}
			return strings.Join(got, ",")
		}
		want := map[string]string{"public.orders": "SELECT,INSERT,UPDATE", "public.audit_event": "INSERT",
			"public.audit_chain_heads": "SELECT,INSERT,UPDATE", "public.idempotency_keys": "SELECT,INSERT,UPDATE",
			"public.authz_floor": "SELECT", "public.audit_anchor_boundaries": "", "public.audit_retention_checkpoints": "",
			"public.authz_revocations": "", "public.authz_revocation_tuples": "", "public.stego_tenants": ""}
		// The job with a consuming worker that writes orders passes its schema manifest check at its end, which holds
		// the grant of the worker role on the revocation floor and on the key table.
		if err := Migrate(context.Background(), settings, consuming("orders")); err != nil {
			t.Fatal(err)
		}
		for table, wantPrivileges := range want {
			if got := privileges(table); got != wantPrivileges {
				t.Errorf("the worker role holds %q on %s, want %q", got, table, wantPrivileges)
			}
		}
		// A write that the worker did not declare is refused: a worker declares no delete.
		if _, err := f.admin.Exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('t1'); INSERT INTO public.orders (tenant_id, id, status) VALUES ('t1', 'o1', 'open')`); err != nil {
			t.Fatal(err)
		}
		asWorker := func(statement string) error {
			tx, err := f.admin.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`SET LOCAL ROLE ` + f.worker + `; SELECT pg_catalog.set_config('app.tenant', 't1', true)`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(statement)
			return err
		}
		if err := asWorker(`UPDATE public.orders SET status = 'synced' WHERE id = 'o1'`); err != nil {
			t.Fatalf("the update of a declared write as the worker role gave %v, want it to pass", err)
		}
		// A consuming worker that creates a row with a reference relates its tuple through the authorization handle
		// in a transaction of the worker role and commits: the column INSERTs of the job are enough, and the foreign
		// key and the deferred check of the tuple row pass (walkthrough 8, W8-B1; security review of PR #421, L2).
		relateAsWorker(t, f, endpoint, key)
		var tuples int
		if err := f.admin.QueryRow(`SELECT count(*) FROM public.authz_revocation_tuples`).Scan(&tuples); err != nil || tuples != 1 {
			t.Fatalf("the worker's relate recorded %d tuple rows (%v), want 1", tuples, err)
		}
		var refused interface{ SQLState() string }
		for _, statement := range []string{`SELECT count(*) FROM public.authz_revocations`, `SELECT count(*) FROM public.authz_revocation_tuples`,
			`UPDATE public.authz_revocations SET done = 1`,
			`INSERT INTO public.authz_revocations (id, tenant_id, principal, tuple_count) VALUES (gen_random_uuid(), 't1', 'service_account:w', 9)`} {
			if err := asWorker(statement); !errors.As(err, &refused) || refused.SQLState() != "42501" {
				t.Fatalf("%s as the worker role gave %v, want SQLSTATE 42501", statement, err)
			}
		}
		var coded interface{ SQLState() string }
		if err := asWorker(`DELETE FROM public.orders WHERE id = 'o1'`); !errors.As(err, &coded) || coded.SQLState() != "42501" {
			t.Fatalf("the delete as the worker role gave %v, want SQLSTATE 42501", err)
		}
		// A consumer table that the runtime role gets no grant on is refused before the job opens the database.
		if err := Migrate(context.Background(), settings, consuming("foreign_orders")); Stage(err) != "migrate."+StepSettings {
			t.Fatalf("the job with the consumer table foreign_orders gave %v at the stage %q, want %s", err, Stage(err), StepSettings)
		}
		// With no consuming worker left, the job takes every grant back, and its manifest check passes.
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("the job with no consumer gave %v", err)
		}
		for table := range want {
			if got := privileges(table); got != "" {
				t.Errorf("with no consumer the worker role holds %q on %s, want nothing", got, table)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC19-T02 The job gives the worker role only SELECT on the columns of the view of an entity that a consuming worker only reads", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		m := f.migration(nil)
		m.Parts.Workers.Consumers = func(*command.Runtime, *outbox.Worker, func(string) command.WorkerObserver) error { return nil }
		// The view of the read entity reads tenant_id, id and version, and not status (security review of PR #451, L4).
		m.Parts.Workers.ConsumerReadTables = map[string][]string{"orders": {"tenant_id", "id", "version"}}
		if err := Migrate(context.Background(), settings, m); err != nil {
			t.Fatal(err)
		}
		for privilege, want := range map[string]bool{"SELECT": false, "INSERT": false, "UPDATE": false, "DELETE": false} {
			var has bool
			if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.orders', $2)`, f.worker, privilege).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("the worker role holds %s on the table orders: %v, want %v", privilege, has, want)
			}
		}
		for column, want := range map[string]bool{"tenant_id": true, "id": true, "version": true, "status": false} {
			var has bool
			if err := f.admin.QueryRow(`SELECT has_column_privilege($1, 'public.orders', $2, 'SELECT')`, f.worker, column).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has != want {
				t.Errorf("the worker role holds SELECT on orders.%s: %v, want %v", column, has, want)
			}
		}
		// A read table that the runtime role gets no grant on is refused before the job opens the database.
		m.Parts.Workers.ConsumerReadTables = map[string][]string{"foreign_orders": {"id"}}
		if err := Migrate(context.Background(), settings, m); Stage(err) != "migrate."+StepSettings {
			t.Fatalf("the job with the read table foreign_orders gave %v at the stage %q, want %s", err, Stage(err), StepSettings)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T28 No committed step of a run again of the job leaves the worker role without a grant that the running release needs", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		// The job of a release with a consuming worker that writes orders, whose authorization text gives the worker
		// role the column grants on the token of the protected table orders, as the generated text does.
		release := func(consumer bool) Migration {
			m := f.migration(nil)
			if consumer {
				m.Parts.Workers.Consumers = func(*command.Runtime, *outbox.Worker, func(string) command.WorkerObserver) error { return nil }
				m.Parts.Workers.ConsumerTables = []string{"orders"}
			}
			m.Authorization += ";\nGRANT SELECT (tenant_id, id), UPDATE (authz_token) ON public.orders TO " + f.worker
			return m
		}
		consuming := func() Migration { return release(true) }
		if err := Migrate(context.Background(), settings, consuming()); err != nil {
			t.Fatal(err)
		}
		needs := []struct{ query, what string }{
			{`SELECT has_column_privilege($1, 'public.orders', 'authz_token', 'UPDATE')`, "UPDATE of the token of orders"},
			{`SELECT has_column_privilege($1, 'public.orders', 'tenant_id', 'SELECT')`, "SELECT of the tenant of orders"},
			{`SELECT has_table_privilege($1, 'public.orders', 'UPDATE')`, "UPDATE on orders"},
			{`SELECT has_table_privilege($1, 'public.idempotency_keys', 'INSERT')`, "INSERT on idempotency_keys"},
			{`SELECT has_table_privilege($1, 'public.audit_event', 'INSERT')`, "INSERT on audit_event"},
			{`SELECT has_table_privilege($1, 'public.authz_floor', 'SELECT')`, "SELECT on authz_floor"},
		}
		var lost []string
		// The relay of a release with no consumer needs the column grants on the token only.
		var checked = needs
		check := func(after string) {
			for _, n := range checked {
				var has bool
				if err := f.admin.QueryRow(n.query, f.worker).Scan(&has); err != nil {
					t.Error(err)
					return
				}
				if !has {
					lost = append(lost, n.what+" before "+after)
				}
			}
		}
		// Each step runs in its own transaction, so before each step the database is in the state that the steps
		// before it committed, as a job that fails there leaves it to the running release.
		stepped = func(step string, _ *sql.Conn) { check(step) }
		defer func() { stepped = nil }()
		if err := Migrate(context.Background(), settings, consuming()); err != nil {
			t.Fatal(err)
		}
		stepped = nil
		check("the end")
		if len(lost) > 0 {
			t.Fatalf("a committed state of the job with a consumer lacks grants of the worker role: %v", lost)
		}
		// The job of a release with no consumer: orders is no consumer table, so only the authorization text gives
		// the worker role its token grants back after the REVOKE.
		checked = needs[:2]
		stepped = func(step string, _ *sql.Conn) { check(step) }
		if err := Migrate(context.Background(), settings, release(false)); err != nil {
			t.Fatal(err)
		}
		stepped = nil
		check("the end")
		if len(lost) > 0 {
			t.Fatalf("a committed state of the job with no consumer lacks the token grants of the worker role: %v", lost)
		}
		// The worker role reads no audit event: its insert returns no row.
		var reads bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.audit_event', 'SELECT')`, f.worker).Scan(&reads); err != nil || reads {
			t.Fatalf("the worker role may read audit_event (%v), want no SELECT", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T08 Two jobs started at once run one after the other, and both pass", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		// Both jobs wait at the start of their first step until both have started, so they reach the tables of a
		// fresh database together when nothing orders them.
		var started sync.WaitGroup
		started.Add(2)
		stepped = func(step string, _ *sql.Conn) {
			if step == StepOwnerRole {
				started.Done()
				started.Wait()
			}
		}
		defer func() { stepped = nil }()
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- Migrate(context.Background(), settings, f.migration(nil)) }()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("one of two jobs started at once gave %v at the stage %s, want both to pass", err, Stage(err))
			}
		}
	})
}

func TestMigrateLockAndLease(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T09 Two jobs started at once run their schema steps one after the other and their backfills under the lease", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f, m, _ := renameJob(t)
		runtimeGrants(t, f)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		var (
			mu         sync.Mutex
			started    sync.WaitGroup
			afterLock  = map[*sql.Conn]time.Time{} // when each job passed the job lock
			atBackfill = map[*sql.Conn]time.Time{} // when each job came to the backfill step
		)
		started.Add(2)
		stepped = func(step string, conn *sql.Conn) {
			switch step {
			case StepOwnerRole:
				started.Done()
				started.Wait()
			case StepLedgerCheck:
				mu.Lock()
				afterLock[conn] = time.Now()
				mu.Unlock()
			case StepBackfill:
				mu.Lock()
				atBackfill[conn] = time.Now()
				mu.Unlock()
				// Each job waits here until the other one also comes to its backfill, which it can only when the job
				// lock is free while a backfill runs.
				deadline := time.After(45 * time.Second)
				for {
					mu.Lock()
					both := len(atBackfill) == 2
					mu.Unlock()
					if both {
						return
					}
					select {
					case <-deadline:
						return
					case <-time.After(50 * time.Millisecond):
					}
				}
			}
		}
		defer func() { stepped = nil }()
		errs := make(chan error, 2)
		for i := range 2 {
			job := m
			job.Holder = fmt.Sprintf("job-%d", i)
			go func() { errs <- Migrate(context.Background(), settings, job) }()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("one of two jobs started at once gave %v at the stage %s, want both to pass", err, Stage(err))
			}
		}
		if len(atBackfill) != 2 || len(afterLock) != 2 {
			t.Fatalf("%d jobs came to the backfill and %d passed the job lock, want 2 and 2", len(atBackfill), len(afterLock))
		}
		var first, second *sql.Conn
		for conn := range afterLock {
			if first == nil || afterLock[conn].Before(afterLock[first]) {
				first, second = conn, first
			} else {
				second = conn
			}
		}
		if afterLock[second].Before(atBackfill[first]) {
			t.Errorf("the second job passed the job lock at %v, before the first job ended its schema steps at %v", afterLock[second], atBackfill[first])
		}
		if gap := atBackfill[second].Sub(atBackfill[first]); gap > 30*time.Second {
			t.Errorf("the second job came to its backfill %v after the first, which waited for it: the job lock was held through the backfill", gap)
		}
		var moved int64
		var completed bool
		if err := f.admin.QueryRowContext(context.Background(), `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil || moved != 24 || !completed {
			t.Fatalf("the checkpoint has %d moved, complete %v (error %v); want each of the 24 rows moved once, and complete", moved, completed, err)
		}
		var unmoved int
		if err := f.admin.QueryRowContext(context.Background(), `SELECT count(*) FROM public.boards WHERE heading IS DISTINCT FROM title`).Scan(&unmoved); err != nil || unmoved != 0 {
			t.Fatalf("%d rows have no new value equal to the old one (error %v)", unmoved, err)
		}
	})
}

func TestMigrateJobLockIndex(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T10 Two jobs pass while the first one builds an index concurrently, with no deadlock and no invalid index", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		var started sync.WaitGroup
		started.Add(2)
		var tries atomic.Int32
		stepped = func(step string, _ *sql.Conn) {
			if step == StepOwnerRole {
				started.Done()
				started.Wait()
			}
		}
		waited = func() { tries.Add(1) }
		defer func() { stepped, waited = nil, nil }()
		m := f.migration(nil)
		tables := m.Ledger
		var building atomic.Bool
		// The job that holds the job lock builds an index concurrently while the other job waits for the lock. A
		// wait in pg_advisory_lock holds a snapshot, which CREATE INDEX CONCURRENTLY waits for: a deadlock.
		m.Ledger = func(ctx context.Context, conn *sql.Conn) error {
			if err := tables(ctx, conn); err != nil {
				return err
			}
			if building.CompareAndSwap(false, true) {
				// The other job tries the lock twice, or, with a blocking wait that never comes back to try again,
				// waits in it for 2 s.
				deadline := time.Now().Add(2 * time.Second)
				for tries.Load() < 2 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
			}
			_, err := conn.ExecContext(ctx, "CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_job_lock_probe ON public.orders (id)")
			return err
		}
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- Migrate(context.Background(), settings, m) }()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("one of two jobs gave %v at the stage %s, want both to pass", err, Stage(err))
			}
		}
		var invalid int
		if err := f.admin.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_catalog.pg_index WHERE NOT indisvalid`).Scan(&invalid); err != nil || invalid != 0 {
			t.Fatalf("%d indexes are not valid (error %v), want none", invalid, err)
		}
	})
}

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	stopSpiceDB()
	os.Exit(code)
}

const rolePassword = "service-test"

// fixture is one database of the local PostgreSQL server of pgtest with the roles of one service, as the tenancy
// component's role migration makes them: the owner role, the runtime role with a login, the worker and relay roles,
// and the owner's membership in the outbox relay role.
type fixture struct {
	t                             *testing.T
	admin                         *sql.DB
	database                      string // the superuser URL of the database
	owner, runtime, worker, relay string
	purge, outboxRelay            string
}

// fixtureOwner is the owner role of the last fixture, which the parts of the test service name; the tests of the
// package do not run in parallel.
var fixtureOwner, fixtureWorker, fixtureOutboxRelay, fixtureRelay, fixturePurge string

func newFixture(t *testing.T) *fixture {
	t.Helper()
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	f := &fixture{t: t, owner: "svc_owner_" + tag, runtime: "svc_rt_" + tag, worker: "svc_worker_" + tag, relay: "svc_relay_" + tag,
		purge: "svc_" + tag + "_idempotency_purge", outboxRelay: "svc_" + tag + outbox.RelaySuffix}
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	f.database = pgtest.Database(t, "service")
	if f.admin, err = sql.Open("pgx", f.database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.admin.Close() })
	name := strings.TrimPrefix(mustURL(t, f.database).Path, "/")
	for _, statement := range []string{
		"CREATE ROLE " + f.owner + " NOLOGIN",
		"CREATE ROLE " + f.runtime + " LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '" + rolePassword + "'",
		"CREATE ROLE " + f.worker + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + f.relay + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + f.outboxRelay + " NOLOGIN NOINHERIT",
		"GRANT " + f.outboxRelay + " TO " + f.owner + " WITH SET TRUE, INHERIT FALSE",
		"CREATE ROLE " + f.purge + " NOLOGIN NOINHERIT",
		"GRANT " + f.purge + " TO " + f.owner + " WITH SET TRUE, INHERIT FALSE",
		"GRANT CREATE ON DATABASE " + name + " TO " + f.owner,
	} {
		if _, err := root.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%.60s: %v", statement, err)
		}
	}
	// As the tenancy role migration gives them: the owner may grant CREATE on public, which the idempotency and
	// authorization migrations give the purge role and the outbox relay role only while they make their functions.
	for _, statement := range []string{
		"GRANT CREATE ON SCHEMA public TO " + f.owner + " WITH GRANT OPTION",
	} {
		if _, err := f.admin.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge, f.outboxRelay} {
			_, _ = f.admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge, f.outboxRelay} {
			_, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		}
	})
	fixtureOwner, fixtureWorker, fixtureOutboxRelay, fixtureRelay, fixturePurge = f.owner, f.worker, f.outboxRelay, f.relay, f.purge
	return f
}

// catalogObjects gives the row-level security, the policies, the indexes and the triggers of the tables of the job, by
// name and as the catalog gives their definitions, in one order.
func catalogObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT 'table ' || c.oid::regclass::text || ' ' || c.relrowsecurity || ' ' || c.relforcerowsecurity
 FROM pg_catalog.pg_class AS c JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p') AND n.nspname IN ('public', 'stego_outbox', 'stego_audit_sink')
UNION ALL SELECT 'policy ' || p.polrelid::regclass::text || ' ' || p.polname || ' ' || p.polcmd::text || ' ' || p.polpermissive ||
 ' ' || (SELECT string_agg(CASE WHEN r = 0 THEN 'public' ELSE r::regrole::text END, ',' ORDER BY 1) FROM unnest(p.polroles) AS r) ||
 ' ' || coalesce(pg_get_expr(p.polqual, p.polrelid), '-') || ' ' || coalesce(pg_get_expr(p.polwithcheck, p.polrelid), '-') FROM pg_catalog.pg_policy AS p
UNION ALL SELECT 'index ' || pg_get_indexdef(i.indexrelid) FROM pg_catalog.pg_index AS i JOIN pg_catalog.pg_class AS c ON c.oid = i.indexrelid
 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace WHERE n.nspname IN ('public', 'stego_outbox', 'stego_audit_sink')
UNION ALL SELECT 'trigger ' || pg_get_triggerdef(g.oid) || ' ' || g.tgenabled::text FROM pg_catalog.pg_trigger AS g WHERE NOT g.tgisinternal
ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

func mustURL(t *testing.T, text string) *url.URL {
	t.Helper()
	u, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// runtimeURL is the URL of the runtime role's login on the database.
func (f *fixture) runtimeURL() string {
	u := mustURL(f.t, f.database)
	u.User = url.UserPassword(f.runtime, rolePassword)
	return u.String()
}

// tenantConfig is the connection configuration of the adapter in the tests: the local server, without TLS on the
// loopback address, as the adapter's loopback test mode allows.
func tenantConfig(calls *int) func(string) (tenant.Config, error) {
	return func(dsn string) (tenant.Config, error) {
		if calls != nil {
			*calls++
		}
		return tenant.Config{DSN: dsn, InsecureLoopback: true}, nil
	}
}

// ordersDDL is the ledger of the test service: one tenant-owned table of orders with row-level security.
const ordersDDL = `CREATE TABLE IF NOT EXISTS public.orders (tenant_id text NOT NULL, id text NOT NULL, status text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.orders FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS orders_tenant ON public.orders;
CREATE POLICY orders_tenant ON public.orders USING (tenant_id = current_setting('app.tenant', true))
  WITH CHECK (tenant_id = current_setting('app.tenant', true));`

// ordersTenantKey is the foreign key of orders to the tenant list, which the manifest of orders names.
const ordersTenantKey = "orders_tenant_id_fkey"

// ordersManifest is the schema manifest of the orders table of ordersDDL.
func ordersManifest() manifest.Manifest {
	return manifest.Manifest{Tables: []manifest.Table{{Schema: "public", Name: "orders", RowSecurity: true,
		Indexes: []manifest.Index{{Name: "orders_pkey", Columns: []string{"tenant_id", "id"}}},
		// The tenant key, which the job adds after the tenant list (decision on ST-SUP-Q124 (a)).
		Constraints: []manifest.Constraint{{Name: ordersTenantKey, Kind: manifest.KindForeignKey, Definition: "FOREIGN KEY (tenant_id) REFERENCES public.stego_tenants(tenant_id)"}},
		TenantKey:   &manifest.TenantKey{Name: ordersTenantKey, Column: "tenant_id"},
		Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "status", Type: "text", NotNull: true},
			{Name: "version", Type: "bigint", Default: "1"}, {Name: "authz_token", Type: "text", Default: "''::text"}},
		Policies: []manifest.Policy{{Name: "orders_tenant", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
			WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}}}}}
}

// migration is the migration job of the test service, with a schema writer that writes into calls. Its declared
// parts have the manifest of the orders table, which the job checks at its end.
func (f *fixture) migration(calls *int) Migration {
	declared := parts(nil, nil)
	declared.Schema = ordersManifest()
	t := f.t
	queue, err := outbox.Migration(f.outboxRelay, f.runtime, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	auditText, err := audit.DDL(audit.Config{RuntimeRole: f.runtime, AnchorPeriod: time.Hour, RetentionMonths: 12})
	if err != nil {
		t.Fatal(err)
	}
	authzText, err := authz.Migration("public", f.runtime, f.relay, f.outboxRelay, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	return Migration{
		OwnerRole: f.owner, RuntimeRole: f.runtime, WorkerRole: f.worker, PurgeRole: f.purge, OutboxRelayRole: f.outboxRelay, TenantConfig: tenantConfig(nil),
		Parts: &declared,
		LedgerCheck: func(ctx context.Context, conn *sql.Conn) error {
			var foreign bool
			if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.foreign_orders') IS NOT NULL`).Scan(&foreign); err != nil {
				return err
			}
			if foreign {
				return fmt.Errorf("the database holds a table that this release did not make")
			}
			return nil
		},
		Ledger: func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, ordersDDL)
			return err
		},
		Outbox: queue, Audit: auditText, Authorization: authzText,
		WriteSchema: func(ctx context.Context, conn grpc.ClientConnInterface) error {
			if calls != nil {
				*calls++
			}
			if spiceServer.endpoint == "" {
				return nil
			}
			_, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: spiceSchema})
			return err
		},
		Definitions: []string{"user", "service", "order", "outbox_destination", "stego_floor"},
		Grants:      fixtureGrants,
	}
}

// fixtureGrants are the GRANT statements of the runtime role of the fixture, without the grantee, which the
// migration job grants and the parts give the schema manifest (ST-SUP-RP14-LC06).
var fixtureGrants = []string{`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "public"."orders"`,
	`GRANT SELECT, INSERT, UPDATE ON TABLE "public"."idempotency_keys"`}

// The wait for the schema is a no-op in a test with no SpiceDB, as WriteSchema of the fixture is.
func init() {
	wait := awaitSchema
	awaitSchema = func(ctx context.Context, conn grpc.ClientConnInterface, definitions []string, since authz.Token) error {
		if spiceServer.endpoint == "" {
			return nil
		}
		return wait(ctx, conn, definitions, since)
	}
}

// writeFile writes data to a new file of the test, which only its owner may read, and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the other runtime tests pin
// it.
const spiceDBImage = "authzed/spicedb:v1.57.1@sha256:9ea6df2d3cca23ed63aa7205639cf6babaaf1dc9a264ffb1f50c6455658fd6ba"

// spiceSchema is the schema of the test service.
const spiceSchema = `definition user {}
definition service {
	relation operator: user
	permission create_order = operator
}
definition order {
	relation owner: user
	permission view = owner
	permission create_order = owner
}
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}
definition stego_floor {
	relation marker: stego_floor
}`

// spiceServer is the one SpiceDB container of the test binary, with its preshared key.
var spiceServer struct {
	once                sync.Once
	name, endpoint, key string
	err                 error
}

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, once for
// the test binary, and gives its endpoint and preshared key.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	// STEGO_TEST_SPICEDB names a running local SpiceDB server in place of the container, with its key in
	// STEGO_TEST_SPICEDB_KEY, as the tests of the generators take it (ST-STD-LOCAL).
	if endpoint := os.Getenv("STEGO_TEST_SPICEDB"); endpoint != "" {
		spiceServer.endpoint, spiceServer.key = endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY")
		return spiceServer.endpoint, spiceServer.key
	}
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv(pgtest.NoContainersVariable) == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: this test needs a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	spiceServer.once.Do(func() {
		suffix := make([]byte, 6)
		_, _ = rand.Read(suffix)
		spiceServer.name, spiceServer.key = "stego-service-spicedb-"+hex.EncodeToString(suffix), "service-"+hex.EncodeToString(suffix)
		run := exec.Command("docker", "run", "-d", "--rm", "--name", spiceServer.name, "--memory", "256m", "--cpus", "1",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage,
			"serve", "--grpc-preshared-key", spiceServer.key, "--datastore-engine", "memory")
		var stderr bytes.Buffer
		run.Stderr = &stderr
		if err := run.Run(); err != nil {
			spiceServer.err = fmt.Errorf("start the SpiceDB container: %v: %s", err, stderr.String())
			return
		}
		// The SpiceDB image has no time limit: the reaper removes the container also when the test binary is killed;
		// such containers leaked for days before.
		pgtest.Reap("docker", spiceServer.name)
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			out, err := exec.Command("docker", "port", spiceServer.name, "50051/tcp").Output()
			if err == nil && len(out) > 0 {
				spiceServer.endpoint = strings.TrimSpace(strings.Split(string(out), "\n")[0])
				break
			}
		}
		if spiceServer.endpoint == "" {
			spiceServer.err = fmt.Errorf("the SpiceDB container publishes no port")
			return
		}
		// The container answers once its health check serves.
		conn, err := DialSpiceDB(SpiceDB{Endpoint: spiceServer.endpoint, Key: spiceServer.key, InsecureLoopback: true})
		if err != nil {
			spiceServer.err = err
			return
		}
		defer conn.Close()
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if spiceServer.err = reach(context.Background(), conn); spiceServer.err == nil {
				return
			}
		}
	})
	if spiceServer.err != nil {
		t.Fatal(spiceServer.err)
	}
	return spiceServer.endpoint, spiceServer.key
}

func stopSpiceDB() {
	if spiceServer.name != "" {
		_ = exec.Command("docker", "rm", "-f", spiceServer.name).Run()
	}
}

// boardsManifest is the manifest of the table boards of renameJob as the release of the rename states it: the table
// of a declared entity under row-level security, with the sync trigger of the open rename, whose function is in the
// held schema stego_steps, and its completeness CHECK as optional objects.
func boardsManifest(t *testing.T, owner string) manifest.Manifest {
	t.Helper()
	source, err := manifest.SyncSource(manifest.SyncVersion, manifest.Sync{Change: "board-title-rename", Owner: owner, K: "title", U: "heading",
		Conversion: manifest.Conversion{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	return manifest.Manifest{Schemas: []manifest.Schema{{Name: "stego_steps", Steps: true}},
		Tables: []manifest.Table{{Schema: "public", Name: "boards", RowSecurity: true, Entity: true,
			Indexes: []manifest.Index{{Name: "boards_pkey", Columns: []string{"tenant_id", "id"}}},
			Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true, Default: "current_setting('app.tenant'::text)"},
				{Name: "id", Type: "uuid", NotNull: true}, {Name: "title", Type: "text", NotNull: true}, {Name: "heading", Type: "text"}},
			Policies: []manifest.Policy{{Name: "tenant_isolation", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
				WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}},
			OptionalTriggers: []manifest.Trigger{{Name: "boards_heading_sync", Timing: "BEFORE", Row: true, Events: []string{"INSERT", "UPDATE"},
				Function: manifest.Function{Name: "stego_steps.boards_heading_sync", Language: "plpgsql", Source: source, Config: []string{manifest.SyncConfig}}}},
			OptionalConstraints: []manifest.Constraint{{Name: "boards_heading_complete", Kind: manifest.KindCheck, Definition: "CHECK (((heading IS NOT NULL) OR (title IS NULL)))"}}}}}
}

// boardsJob is the job of renameJob, whose declared parts also have the manifest of boards, with the grants of the
// runtime role and the settings of a job against the SpiceDB of the tests.
func boardsJob(t *testing.T) (*fixture, Migration, MigrationSettings) {
	t.Helper()
	endpoint, key := startSpiceDB(t)
	f, m, _ := renameJob(t)
	runtimeGrants(t, f)
	return f, m, MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
}

// boardsCompleted tells that the change board-title-rename is marked complete.
func boardsCompleted(t *testing.T, f *fixture) bool {
	t.Helper()
	var done bool
	if err := f.admin.QueryRow("SELECT completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'").Scan(&done); err != nil {
		t.Fatal(err)
	}
	return done
}

// recordSteps records the steps of each Migrate of the test in steps.
func recordSteps(t *testing.T, steps *[]string) {
	t.Helper()
	stepped = func(step string, _ *sql.Conn) { *steps = append(*steps, step) }
	t.Cleanup(func() { stepped = nil })
}

func TestMigrateManifestBefore(t *testing.T) {
	ctx := context.Background()
	t.Run("ST-SUP-RP14-LC03-T14 The migration job refuses before its backfills a trigger that the manifest does not name on an entity table, so it never runs as the owner role", func(t *testing.T) {
		f, m, settings := boardsJob(t)
		// Review of PR #305, M1: a trigger of a role that holds only TRIGGER on boards ran 10 times in the backfill
		// as the owner role, before the only check of the job.
		for _, statement := range []string{
			"CREATE SCHEMA lc03t14", "CREATE TABLE lc03t14.log (role text)", "GRANT USAGE ON SCHEMA lc03t14 TO PUBLIC",
			"GRANT INSERT ON lc03t14.log TO PUBLIC",
			`CREATE FUNCTION lc03t14.f() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO lc03t14.log VALUES (current_user); RETURN NULL; END$$`,
			"CREATE TRIGGER lc03t14_evil AFTER UPDATE ON public.boards FOR EACH STATEMENT EXECUTE FUNCTION lc03t14.f()",
		} {
			if _, err := f.admin.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%.50s: %v", statement, err)
			}
		}
		var steps []string
		recordSteps(t, &steps)
		err := Migrate(ctx, settings, m)
		if Stage(err) != "migrate."+StepManifestBefore || !strings.Contains(err.Error(), "table public.boards: trigger lc03t14_evil is a trigger that the manifest does not name") {
			t.Fatalf("the job with a trigger that the manifest does not name gave %v at the stage %q, want the refusal of the step %s that names it", err, Stage(err), StepManifestBefore)
		}
		var ran int
		if err := f.admin.QueryRow("SELECT count(*) FROM lc03t14.log").Scan(&ran); err != nil {
			t.Fatal(err)
		}
		if ran != 0 || slices.Contains(steps, StepBackfill) || boardsCompleted(t, f) || unmoved(t, f) != 24 {
			t.Fatalf("the refused job ran the trigger %d times in the steps %v, complete %v with %d rows unmoved, want no run, no backfill and 24 rows unmoved",
				ran, steps, boardsCompleted(t, f), unmoved(t, f))
		}
		// With no trigger of its own, the same job accepts the open change's NOT VALID completeness CHECK before
		// the backfill, completes the change and passes its last check.
		if _, err := f.admin.ExecContext(ctx, "DROP TRIGGER lc03t14_evil ON public.boards"); err != nil {
			t.Fatal(err)
		}
		steps = nil
		if err := Migrate(ctx, settings, m); err != nil {
			t.Fatalf("the job with no trigger of its own gave %v after the steps %v", err, steps)
		}
		if !boardsCompleted(t, f) || unmoved(t, f) != 0 || len(steps) < 2 || steps[len(steps)-2] != StepManifest || steps[len(steps)-1] != StepReleaseIdentity {
			t.Fatalf("the job ran the steps %v, complete %v with %d rows unmoved, want the change complete and the steps %s and %s last", steps, boardsCompleted(t, f), unmoved(t, f), StepManifest, StepReleaseIdentity)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T19 A trigger made on an entity table after the check before the backfills is refused by the first batch, which writes nothing, and the job refuses the database at its schema manifest check", func(t *testing.T) {
		f, m, settings := boardsJob(t)
		// Decision on the #305 info: the job lock ends before the backfill, so a trigger made in that gap passes
		// the check before the backfills. Each batch checks the triggers of its table again.
		for _, statement := range []string{
			"CREATE SCHEMA lc03t19", "CREATE TABLE lc03t19.log (role text)", "GRANT USAGE ON SCHEMA lc03t19 TO PUBLIC",
			"GRANT INSERT ON lc03t19.log TO PUBLIC",
			`CREATE FUNCTION lc03t19.f() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO lc03t19.log VALUES (current_user); RETURN NULL; END$$`,
		} {
			if _, err := f.admin.ExecContext(ctx, statement); err != nil {
				t.Fatalf("%.50s: %v", statement, err)
			}
		}
		var steps []string
		stepped = func(step string, _ *sql.Conn) {
			steps = append(steps, step)
			if step == StepBackfill {
				if _, err := f.admin.ExecContext(ctx, "CREATE TRIGGER lc03t19_evil AFTER UPDATE ON public.boards FOR EACH STATEMENT EXECUTE FUNCTION lc03t19.f()"); err != nil {
					t.Errorf("the trigger in the gap: %v", err)
				}
			}
		}
		t.Cleanup(func() { stepped = nil })
		err := Migrate(ctx, settings, m)
		if Stage(err) != "migrate."+StepManifest || !strings.Contains(err.Error(), "table public.boards: trigger lc03t19_evil is a trigger that the manifest does not name") {
			t.Fatalf("the job with a trigger made in the gap gave %v at the stage %q, want the refusal of the step %s that names it", err, Stage(err), StepManifest)
		}
		var ran int
		if err := f.admin.QueryRow("SELECT count(*) FROM lc03t19.log").Scan(&ran); err != nil {
			t.Fatal(err)
		}
		if ran != 0 || boardsCompleted(t, f) || unmoved(t, f) != 24 {
			t.Fatalf("the refused job ran the trigger %d times, complete %v with %d rows unmoved, want no run and 24 rows unmoved", ran, boardsCompleted(t, f), unmoved(t, f))
		}
		// With the trigger gone, the next job completes the change.
		stepped = nil
		if _, err := f.admin.ExecContext(ctx, "DROP TRIGGER lc03t19_evil ON public.boards"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, settings, m); err != nil || !boardsCompleted(t, f) || unmoved(t, f) != 0 {
			t.Fatalf("the next job gave %v, complete %v with %d rows unmoved, want the change complete", err, boardsCompleted(t, f), unmoved(t, f))
		}
	})

	t.Run("ST-SUP-RP14-LC03-T20 The wait after a broken backfill session is the retry period of the lease, the default retry period when it has none, and never less than MinSessionPause", func(t *testing.T) {
		for _, c := range []struct {
			retry, want time.Duration
		}{{0, lease.DefaultRetry}, {-time.Second, lease.DefaultRetry}, {time.Nanosecond, MinSessionPause}, {50 * time.Millisecond, MinSessionPause},
			{MinSessionPause, MinSessionPause}, {3 * time.Second, 3 * time.Second}} {
			if got := sessionPause(lease.Settings{Retry: c.retry}); got != c.want {
				t.Errorf("the pause for the retry period %v is %v, want %v", c.retry, got, c.want)
			}
		}
		if MinSessionPause < 100*time.Millisecond {
			t.Fatalf("MinSessionPause is %v, want at least 100 ms", MinSessionPause)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T15 Before its backfills the migration job accepts the completeness CHECK NOT VALID only in its exact form and only for a change it backfills", func(t *testing.T) {
		f, m, settings := boardsJob(t)
		var steps []string
		recordSteps(t, &steps)
		// The NOT VALID completeness CHECK of a change that the job does not backfill.
		open := m.Backfills
		m.Backfills = nil
		err := Migrate(ctx, settings, m)
		if Stage(err) != "migrate."+StepManifestBefore || !strings.Contains(err.Error(), "table public.boards: constraint boards_heading_complete is not validated") {
			t.Fatalf("the job that does not backfill the change gave %v at the stage %q, want the refusal of the NOT VALID completeness CHECK at %s", err, Stage(err), StepManifestBefore)
		}
		// Review of PR #305, M1 (TestSec304Validate): a CHECK (true) NOT VALID of the same name let the change be
		// marked complete with rows unmoved.
		m.Backfills = open
		if _, err := f.admin.ExecContext(ctx, "ALTER TABLE public.boards DROP CONSTRAINT boards_heading_complete; ALTER TABLE public.boards ADD CONSTRAINT boards_heading_complete CHECK (true) NOT VALID"); err != nil {
			t.Fatal(err)
		}
		steps = nil
		err = Migrate(ctx, settings, m)
		if Stage(err) != "migrate."+StepManifestBefore || !strings.Contains(err.Error(), "table public.boards: constraint boards_heading_complete is not the constraint of the manifest") ||
			slices.Contains(steps, StepBackfill) || boardsCompleted(t, f) {
			t.Fatalf("the job with the CHECK (true) gave %v at the stage %q after the steps %v, complete %v, want the refusal at %s before any backfill",
				err, Stage(err), steps, boardsCompleted(t, f), StepManifestBefore)
		}
		// The CHECK in its exact form: the job completes the change, and a job that backfills no change then accepts
		// the validated CHECK before its backfills.
		if _, err := f.admin.ExecContext(ctx, "ALTER TABLE public.boards DROP CONSTRAINT boards_heading_complete; "+boardsComplete); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, settings, m); err != nil || !boardsCompleted(t, f) {
			t.Fatalf("the job with the exact CHECK gave %v, complete %v", err, boardsCompleted(t, f))
		}
		m.Backfills = nil
		if err := Migrate(ctx, settings, m); err != nil {
			t.Fatalf("the job with the validated CHECK and no change to backfill gave %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T16 The migration job refuses the search path and the settings that would give the runtime role another table", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(ctx, settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		database := mustURL(t, f.database).Path[1:]
		refused := func(what, change, restore string, faults ...string) {
			t.Helper()
			if _, err := f.admin.ExecContext(ctx, change); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			err := Migrate(ctx, settings, f.migration(nil))
			if _, rerr := f.admin.ExecContext(ctx, restore); rerr != nil {
				t.Fatalf("restore %s: %v", what, rerr)
			}
			if Stage(err) != "migrate."+StepManifestBefore {
				t.Fatalf("%s: the job gave %v at the stage %q, want the refusal of the step %s", what, err, Stage(err), StepManifestBefore)
			}
			for _, fault := range faults {
				if !strings.Contains(err.Error(), fault) {
					t.Fatalf("%s: the refusal %v does not name %q", what, err, fault)
				}
			}
		}
		// Review of PR #305, M2: a schema named after the runtime role, with an orders table with no row-level
		// security, which "$user", public puts first.
		refused("a schema named after the runtime role",
			"CREATE SCHEMA "+f.runtime+" AUTHORIZATION "+f.runtime+"; CREATE TABLE "+f.runtime+".orders (tenant_id text, status text); ALTER TABLE "+f.runtime+".orders OWNER TO "+f.runtime,
			"DROP SCHEMA "+f.runtime+" CASCADE",
			"the schema "+f.runtime+" has the name of the role of the session", "the relation "+f.runtime+".orders comes before public.orders on the search path of the session")
		// A search path of the role, with a quoted name, that puts a schema with an orders table first.
		refused("a search path of the role",
			`CREATE SCHEMA "Lc03T16"; CREATE TABLE "Lc03T16".orders (status text); ALTER ROLE `+f.runtime+` SET search_path = "Lc03T16", public`,
			`ALTER ROLE `+f.runtime+` RESET search_path; DROP SCHEMA "Lc03T16" CASCADE`,
			"the role "+f.runtime+" gets search_path from a setting of the role, which the manifest does not allow", "the relation Lc03T16.orders comes before public.orders")
		// PostgreSQL stores a name with no quotes in lower case.
		refused("a search path of the database, with a name in capitals",
			`CREATE SCHEMA lc03t16b; CREATE TABLE lc03t16b.orders (status text); ALTER DATABASE "`+database+`" SET search_path = LC03T16B, public`,
			`ALTER DATABASE "`+database+`" RESET search_path; DROP SCHEMA lc03t16b CASCADE`,
			"gets search_path from a setting of the database", "the relation lc03t16b.orders comes before public.orders")
		refused("a session_replication_role of the role in the database",
			`ALTER ROLE `+f.runtime+` IN DATABASE "`+database+`" SET session_replication_role = replica`,
			`ALTER ROLE `+f.runtime+` IN DATABASE "`+database+`" RESET session_replication_role`,
			"the role "+f.runtime+" gets session_replication_role from a setting of the role in the database, which skips the triggers")
		// The search path public, pg_temp is accepted only as the role's own setting, which the role migration of the
		// service makes (internal/generator/tenancy), and only in that order.
		refused("the path of the role migration as a setting of the database",
			`ALTER DATABASE "`+database+`" SET search_path = public, pg_temp`, `ALTER DATABASE "`+database+`" RESET search_path`,
			"the role "+f.runtime+" gets search_path from a setting of the database, which the manifest does not allow")
		refused("the temporary schema first in a search path of the role",
			"ALTER ROLE "+f.runtime+" SET search_path = pg_temp, public", "ALTER ROLE "+f.runtime+" RESET search_path",
			"the role "+f.runtime+" gets search_path from a setting of the role, which the manifest does not allow")
		// The path of the role migration, a setting of another name, and a setting of another role are no fault.
		if _, err := f.admin.ExecContext(ctx, "ALTER ROLE "+f.runtime+" SET search_path = public, pg_temp; ALTER ROLE "+f.runtime+" SET statement_timeout = '5s'; ALTER ROLE "+f.worker+" SET search_path = public"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(ctx, settings, f.migration(nil)); err != nil {
			t.Fatalf("the job with other settings gave %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T17 The last schema manifest check of the migration job has its own time limit after a backfill that ends past the time limit of the job", func(t *testing.T) {
		f, m, settings := boardsJob(t)
		m.Timeout = 5 * time.Second
		// Review of PR #305, L1: the step stands for a backfill that ends past the time limit of the job.
		stepped = func(step string, _ *sql.Conn) {
			if step == StepBackfill {
				time.Sleep(6 * time.Second)
			}
		}
		t.Cleanup(func() { stepped = nil })
		if err := Migrate(ctx, settings, m); err != nil {
			t.Fatalf("the job whose backfill ended past its time limit gave %v at the stage %q, want it to pass", err, Stage(err))
		}
		if !boardsCompleted(t, f) {
			t.Fatal("the change is not complete")
		}
	})
}

// tenantKeyOf reads the tenant key of orders: whether it is there, validated, NO ACTION on delete and on update, and
// refers to public.stego_tenants.
func tenantKeyOf(t *testing.T, f *fixture) (present, validated, noAction bool) {
	t.Helper()
	err := f.admin.QueryRow(`SELECT k.convalidated, k.confdeltype = 'a' AND k.confupdtype = 'a' AND k.confrelid = to_regclass('public.stego_tenants')
  FROM pg_constraint k WHERE k.conrelid = to_regclass('public.orders') AND k.conname = $1 AND k.contype = 'f'`, ordersTenantKey).Scan(&validated, &noAction)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true, validated, noAction
}

// asRuntime runs statement as the runtime role in one transaction with the tenant setting tenant, and gives the
// SQLSTATE of its error, or empty.
func asRuntime(t *testing.T, db *sql.DB, tenant, statement string, args ...any) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(statement, args...); err != nil {
		return sqlState(err)
	}
	if err := tx.Commit(); err != nil {
		return sqlState(err)
	}
	return ""
}

func TestMigrateTenantKeys(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T21 The job adds the tenant key of each owned table after the tenant list and validates it, and row-level security still refuses a write of another tenant, listed or not", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		// The key is not there when the tables step ends, and is there NOT VALID when the add step ends.
		var atAdd, atValidate [2]bool
		stepped = func(step string, conn *sql.Conn) {
			present, validated, _ := tenantKeyOf(t, f)
			switch step {
			case StepTenantKeys:
				atAdd = [2]bool{present, validated}
			case StepTenantKeysValidate:
				atValidate = [2]bool{present, validated}
			}
		}
		defer func() { stepped = nil }()
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		stepped = nil
		if atAdd != [2]bool{false, false} || atValidate != [2]bool{true, false} {
			t.Fatalf("the key at the add step is %v and at the validate step %v (there, validated), want none and then NOT VALID", atAdd, atValidate)
		}
		if present, validated, noAction := tenantKeyOf(t, f); !present || !validated || !noAction {
			t.Fatalf("after the job the tenant key of orders is there %v, validated %v, NO ACTION to public.stego_tenants %v", present, validated, noAction)
		}
		// An index leads with the tenant column, so the check of a delete from the tenant list is an index probe.
		var led bool
		if err := f.admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
  WHERE i.indrelid = 'public.orders'::regclass AND a.attname = 'tenant_id' AND i.indisvalid AND i.indpred IS NULL)`).Scan(&led); err != nil || !led {
			t.Fatalf("no valid index of orders leads with tenant_id (error %v)", err)
		}
		for _, id := range []string{"tenant-a", "tenant-b"} {
			if _, err := f.admin.Exec("INSERT INTO public.stego_tenants (tenant_id) VALUES ($1)", id); err != nil {
				t.Fatal(err)
			}
		}
		runtime, err := sql.Open("pgx", f.runtimeURL())
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		insert := "INSERT INTO public.orders (tenant_id, id, status) VALUES ($1, $2, 'new')"
		if code := asRuntime(t, runtime, "tenant-a", insert, "tenant-a", "o1"); code != "" {
			t.Fatalf("a write of the listed tenant of the transaction gave %s", code)
		}
		// The WITH CHECK of the tenant policy refuses a row of another tenant before the foreign key is checked:
		// of a listed tenant and of an unlisted one alike.
		for _, other := range []string{"tenant-b", "tenant-z"} {
			if code := asRuntime(t, runtime, "tenant-a", insert, other, "o2"); code != "42501" {
				t.Errorf("a write of tenant-a for the tenant %s gave %q, want the row-level security refusal 42501", other, code)
			}
		}
		// A write of a tenant that the list does not hold is refused by the key, and so is the delete of a list row
		// that has rows.
		if code := asRuntime(t, runtime, "tenant-z", insert, "tenant-z", "o3"); code != "23503" {
			t.Errorf("a write of the unlisted tenant of the transaction gave %q, want the foreign key violation 23503", code)
		}
		if _, err := f.admin.Exec("DELETE FROM public.stego_tenants WHERE tenant_id = 'tenant-a'"); sqlState(err) != "23503" {
			t.Errorf("the delete of a list row with rows gave %v, want the foreign key violation 23503", err)
		}
		if _, err := f.admin.Exec("DELETE FROM public.stego_tenants WHERE tenant_id = 'tenant-b'"); err != nil {
			t.Errorf("the delete of a list row with no rows gave %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T22 A row of a tenant that the tenant list does not hold refuses the validation of the tenant key with the remedy, and the next run validates the key once the row is gone", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// A release before the key wrote a row of the tenant secret-orphan, which the list does not hold.
		for _, statement := range []string{"ALTER TABLE public.orders DROP CONSTRAINT " + ordersTenantKey,
			"INSERT INTO public.orders (tenant_id, id, status) VALUES ('secret-orphan', 'o1', 'new')"} {
			if _, err := f.admin.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		var steps []string
		recordSteps(t, &steps)
		err := Migrate(context.Background(), settings, f.migration(nil))
		var refused interface{ Refusal() string }
		if Stage(err) != "migrate."+StepTenantKeysValidate || !errors.As(err, &refused) {
			t.Fatalf("the job with an orphan row gave %v, want a refusal of the step %s", err, StepTenantKeysValidate)
		}
		for _, text := range []string{err.Error(), refused.Refusal()} {
			if !strings.Contains(text, "public.orders") || !strings.Contains(text, ordersTenantKey) || !strings.Contains(text, "provision the tenant or remove its rows") || strings.Contains(text, "secret-orphan") {
				t.Fatalf("the refusal %q does not name the table, the key and the remedy, or names a row value", text)
			}
		}
		if slices.Contains(steps, StepOutboxQueue) {
			t.Fatalf("the refused job ran on after the validation: %v", steps)
		}
		if present, validated, _ := tenantKeyOf(t, f); !present || validated {
			t.Fatalf("after the refusal the key is there %v and validated %v, want it NOT VALID", present, validated)
		}
		if _, err := f.admin.Exec("DELETE FROM public.orders WHERE tenant_id = 'secret-orphan'"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("the job after the orphan row was removed gave %v", err)
		}
		if present, validated, _ := tenantKeyOf(t, f); !present || !validated {
			t.Fatalf("after the run again the key is there %v and validated %v", present, validated)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T23 The add of the tenant key waits at most the lock timeout behind a long write of the running release, and runs again", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.Exec("ALTER TABLE public.orders DROP CONSTRAINT " + ordersTenantKey); err != nil {
			t.Fatal(err)
		}
		// When the add step starts, a long write transaction of the running release holds ROW EXCLUSIVE on orders,
		// which the SHARE ROW EXCLUSIVE lock of the add waits for. (The ledger of the test service makes its policy
		// again with no lock timeout, so the transaction starts after the tables step.)
		long, err := f.admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = long.Rollback() }()
		stepped = func(step string, _ *sql.Conn) {
			if step == StepTenantKeys {
				if _, err := long.Exec("UPDATE public.orders SET status = status"); err != nil {
					t.Error(err)
				}
			}
		}
		defer func() { stepped = nil }()
		waits := make(chan string, 64)
		retried = func(step string) { waits <- step }
		defer func() { retried = nil }()
		m := f.migration(nil)
		m.Timeout = 2 * time.Minute
		ran := make(chan error, 1)
		go func() { ran <- Migrate(context.Background(), settings, m) }()
		select {
		case step := <-waits:
			if step != StepTenantKeys {
				t.Fatalf("the step %s waited for a lock, want %s", step, StepTenantKeys)
			}
		case err := <-ran:
			t.Fatalf("the job ended with %v while the long transaction held orders, want it to wait", err)
		case <-time.After(30 * time.Second):
			t.Fatal("the lock wait of the add did not end at the lock timeout within 30 s")
		}
		if err := long.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-ran:
			if err != nil {
				t.Fatalf("the job after the long transaction ended gave %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the job did not end within 60 s after the long transaction ended")
		}
		if present, validated, _ := tenantKeyOf(t, f); !present || !validated {
			t.Fatalf("after the job the key is there %v and validated %v", present, validated)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T24 The owner role cannot delete a tenant with rows from the tenant list while the tenant setting of another tenant hides those rows", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"tenant-a", "tenant-b"} {
			if _, err := f.admin.Exec("INSERT INTO public.stego_tenants (tenant_id) VALUES ($1)", id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.admin.Exec("INSERT INTO public.orders (tenant_id, id, status) VALUES ('tenant-a', 'o1', 'new')"); err != nil {
			t.Fatal(err)
		}
		// As the owner role with the tenant setting tenant-b: row-level security hides the order of tenant-a, and
		// the check of the key on the delete still finds it (security review of PR #320, D1b).
		tx, err := f.admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		for _, statement := range []string{"SET LOCAL ROLE " + f.owner, "SELECT pg_catalog.set_config('app.tenant', 'tenant-b', true)"} {
			if _, err := tx.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		var seen int
		if err := tx.QueryRow("SELECT count(*) FROM public.orders").Scan(&seen); err != nil || seen != 0 {
			t.Fatalf("the owner role with the tenant setting tenant-b sees %d orders (error %v), want none", seen, err)
		}
		if _, err := tx.Exec("DELETE FROM public.stego_tenants WHERE tenant_id = 'tenant-a'"); sqlState(err) != "23503" {
			t.Fatalf("the delete of tenant-a as the owner role with the tenant setting tenant-b gave %v, want the foreign key violation 23503", err)
		}
		_ = tx.Rollback()
		var listed bool
		if err := f.admin.QueryRow("SELECT EXISTS (SELECT 1 FROM public.stego_tenants WHERE tenant_id = 'tenant-a')").Scan(&listed); err != nil || !listed {
			t.Fatalf("tenant-a is not in the list after the refused delete (error %v)", err)
		}
	})
}

func TestMigrateInbox(t *testing.T) {
	t.Run("ST-SUP-RP14-LC08-T01 The job makes the table of the refused records of a declared subscription after the audit sink, as the owner, for the worker role", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		m := f.migration(nil)
		text, err := kafkainbox.Migration(f.worker)
		if err != nil {
			t.Fatal(err)
		}
		m.Inbox = text
		s := &steps{backends: map[int]bool{}}
		stepped = s.record(t)
		defer func() { stepped = nil }()
		if err := Migrate(context.Background(), settings, m); err != nil {
			t.Fatal(err)
		}
		at := slices.Index(s.names, StepInbox)
		if at < 1 || s.names[at-1] != StepAuditSink || at+1 >= len(s.names) || s.names[at+1] != StepInboxValidate {
			t.Fatalf("the job ran %v, want %s right after %s and then %s", s.names, StepInbox, StepAuditSink, StepInboxValidate)
		}
		// The CHECK of the reasons is validated, in the step of its own after the text (security review of PR #429, L2).
		var notValid int
		if err := f.admin.QueryRow(`SELECT count(*) FROM pg_catalog.pg_constraint
			WHERE conrelid = 'stego_inbox.refused'::regclass AND contype = 'c' AND NOT convalidated`).Scan(&notValid); err != nil || notValid != 0 {
			t.Fatalf("the table of the refused records has %d CHECKs that are not valid (%v), want 0", notValid, err)
		}
		var owned, insert bool
		if err := f.admin.QueryRow(`SELECT (SELECT tableowner FROM pg_tables WHERE schemaname = 'stego_inbox' AND tablename = 'refused') = $1,
			has_table_privilege($2, 'stego_inbox.refused', 'INSERT')`, f.owner, f.worker).Scan(&owned, &insert); err != nil || !owned || !insert {
			t.Fatalf("the table of the refused records is owned by the owner %t and writable by the worker %t (%v)", owned, insert, err)
		}
		// The worker role reads the tenant list in the write of each record, and does not write it (security review of
		// PR #418, L1).
		var reads, writes bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.stego_tenants', 'SELECT'),
			has_table_privilege($1, 'public.stego_tenants', 'INSERT') OR has_table_privilege($1, 'public.stego_tenants', 'UPDATE')`,
			f.worker).Scan(&reads, &writes); err != nil || !reads || writes {
			t.Fatalf("the worker role reads the tenant list %t and writes it %t (%v), want read only", reads, writes, err)
		}
		// A service with no subscription runs no such step.
		s = &steps{backends: map[int]bool{}}
		stepped = s.record(t)
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil || slices.Contains(s.names, StepInbox) ||
			slices.Contains(s.names, StepInboxValidate) {
			t.Fatalf("a job with no inbox text ran %v (%v)", s.names, err)
		}
	})
}

func TestInboxSetting(t *testing.T) {
	t.Run("ST-SUP-RP14-LC08-T02 A refused setting of the Kafka inbox names the stage of kafka-consumer and the variable, and no value", func(t *testing.T) {
		t.Setenv("STEGO_KAFKA_BROKERS", "127.0.0.1:9092")
		t.Setenv("STEGO_KAFKA_INSECURE_LOOPBACK", "1")
		topic := kafkainbox.Setting(kafkainbox.SubscriptionTopicSetting, "order-changes")
		t.Setenv(topic, "")
		_, err := kafkainbox.ReadSettings("fulfilment", "order-changes")
		if stage := StartStage(inboxSetting(err)); stage != "service.start.setting.kafka-consumer."+topic {
			t.Fatalf("the stage of a missing topic is %q", stage)
		}
		t.Setenv(topic, "secret-topic-name!")
		_, err = kafkainbox.ReadSettings("fulfilment", "order-changes")
		if err = inboxSetting(err); err == nil || strings.Contains(err.Error(), "secret-topic-name") {
			t.Fatalf("the refusal of a malformed topic is %v", err)
		}
		if other := errors.New("other"); inboxSetting(other) != other {
			t.Fatal("inboxSetting changed an error that is not a setting")
		}
	})
}

func TestInboxReadiness(t *testing.T) {
	t.Run("ST-SUP-RP14-LC08-T03 A stopped Kafka inbox is named by the readiness of the worker mode", func(t *testing.T) {
		w := &Workers{inboxes: []namedInbox{{name: "inbox-order-changes", state: &failingInbox{}}, {name: "inbox-carrier-changes", state: &failingInbox{}}}}
		w.track(OutboxWorkerName)
		w.track("inbox-carrier-changes")
		w.track("inbox-order-changes").running.Store(false)
		names := append([]string{OutboxWorkerName, RelayWatchName}, w.inboxNames()...)
		if reason := w.stopped(names...); reason != "the component inbox-order-changes stopped" {
			t.Fatalf("the reason is %q", reason)
		}
		// An inbox that its retention check stopped names that stage (core builder 16).
		w.inboxes[0].state = &failingInbox{stage: "kafka-consumer.retention"}
		if reason := w.stopped(names...); reason != "the component inbox-order-changes stopped at the stage kafka-consumer.retention" {
			t.Fatalf("the reason of an inbox stopped by its retention is %q", reason)
		}
		w.track("inbox-order-changes")
		if reason := w.stopped(names...); reason != "" {
			t.Fatalf("a running inbox gives the reason %q", reason)
		}
		// The second inbox stops on its own (decision on ST-SUP-Q204).
		w.track("inbox-carrier-changes").running.Store(false)
		if reason := w.stopped(names...); reason != "the component inbox-carrier-changes stopped" {
			t.Fatalf("the reason of the second inbox is %q", reason)
		}
	})
}

// failingInbox is an inbox whose sessions fail with class, or do not fail when it is empty.
type failingInbox struct{ class, stage string }

func (f *failingInbox) Failed() string { return f.stage }

func (f *failingInbox) Failing() string { return f.class }

func TestInboxFailingReadiness(t *testing.T) {
	t.Run("ST-SUP-RP14-LC08-T04 An inbox that fails each session is named with its failure class by the readiness of the worker mode", func(t *testing.T) {
		inbox, other := &failingInbox{class: "broker:TOPIC_AUTHORIZATION_FAILED"}, &failingInbox{}
		w := &Workers{inboxes: []namedInbox{{name: "inbox-order-changes", state: inbox}, {name: "inbox-carrier-changes", state: other}}}
		if reason := w.inboxFailing(); reason != "the component inbox-order-changes fails: broker:TOPIC_AUTHORIZATION_FAILED" {
			t.Fatalf("the reason is %q", reason)
		}
		other.class = "fetch:UNKNOWN_TOPIC_OR_PARTITION"
		if reason := w.inboxFailing(); reason != "the component inbox-order-changes fails: broker:TOPIC_AUTHORIZATION_FAILED; the component inbox-carrier-changes fails: fetch:UNKNOWN_TOPIC_OR_PARTITION" {
			t.Fatalf("the reason of two failing inboxes is %q", reason)
		}
		inbox.class, other.class = "", ""
		if reason := w.inboxFailing(); reason != "" {
			t.Fatalf("an inbox that reads gives the reason %q", reason)
		}
		if reason := (&Workers{}).inboxFailing(); reason != "" {
			t.Fatalf("a service with no inbox gives the reason %q", reason)
		}
	})
}

func TestDestinationFailingReadiness(t *testing.T) {
	t.Run("ST-SUP-RP09-LC05-T06 A destination of the outbox that fails each delivery in a row is named by the readiness of the worker mode", func(t *testing.T) {
		db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		queue, err := outbox.New(db, outbox.Options{})
		if err != nil {
			t.Fatal(err)
		}
		worker, err := outbox.NewWorker(queue, outbox.DefaultWorkerConfig())
		if err != nil {
			t.Fatal(err)
		}
		class := ""
		deliver := func(context.Context, *sql.Tx, outbox.Delivery) error { return nil }
		for _, d := range []outbox.Destination{{Name: "kafka", Idempotent: true, Handler: deliver, Failing: func() string { return class }},
			{Name: "audit-sink", Idempotent: true, MustLand: true, Handler: deliver}} {
			if err := worker.Register(d); err != nil {
				t.Fatal(err)
			}
		}
		w := &Workers{worker: worker}
		if reason := w.destinationFailing(); reason != "" {
			t.Fatalf("a destination that delivers gives the reason %q", reason)
		}
		// The Kafka destination in a broker outage (walkthrough 9).
		class = "broker-unavailable"
		if reason := w.destinationFailing(); reason != "the destination kafka fails: broker-unavailable" {
			t.Fatalf("the reason is %q", reason)
		}
		class = ""
		if reason := w.destinationFailing(); reason != "" {
			t.Fatalf("a destination that delivers again gives the reason %q", reason)
		}
		if reason := (&Workers{}).destinationFailing(); reason != "" {
			t.Fatalf("a process with no outbox worker gives the reason %q", reason)
		}
	})
}

// relateAsWorker relates one tuple through the authorization handle in a transaction of tenant t1 as the worker role
// of the fixture, and commits it, as a consuming worker that creates a row with a reference does.
func relateAsWorker(t *testing.T, f *fixture, endpoint, key string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.admin.Exec("ALTER ROLE " + f.worker + " LOGIN PASSWORD '" + rolePassword + "'"); err != nil {
		t.Fatal(err)
	}
	u := mustURL(t, f.database)
	u.User = url.UserPassword(f.worker, rolePassword)
	// The worker role runs the outbox relay functions, as the worker mode opens its pool (ST-SUP-RP01-LC19).
	workerDB, err := tenant.Open(ctx, tenant.Config{DSN: u.String(), InsecureLoopback: true, Claims: true})
	if err != nil {
		t.Fatal(err)
	}
	defer workerDB.Close()
	runtimeDB, err := tenant.Open(ctx, tenant.Config{DSN: f.runtimeURL(), InsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeDB.Close()
	queue, err := outbox.New(workerDB, outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := DialSpiceDB(SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	provider, err := authz.NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	az, err := authz.New(authz.Config{Provider: provider, DB: runtimeDB, Schema: "public", Outbox: queue})
	if err != nil {
		t.Fatal(err)
	}
	tn := mustTenant(t, "t1")
	tx, err := tenant.Begin(ctx, workerDB, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	worker, err := authz.System("ship-orders")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := az.Bind(tx, tn, worker, []authz.Relation{{ResourceType: "order", Name: "owner"}})
	if err != nil {
		t.Fatal(err)
	}
	resource, err := authz.NewResource(tn, "order", "o1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Relate(ctx, authz.Tuple{Resource: resource, Relation: "owner", Subject: authz.Subject{Type: "user", ID: "u-7"}}); err != nil {
		t.Fatalf("the relate of the worker role gave %v", err)
	}
	if err := tenant.Commit(ctx, tx, tn); err != nil {
		t.Fatalf("the commit of the worker's relate gave %v", err)
	}
}

// TestMigrateCauseClass proves the cause class of a step that a gRPC server failed, which the failure record of the
// job gives with the stage (walkthrough 19, small items).
func TestMigrateCauseClass(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T31 A step that SpiceDB failed gives the fixed class of its status code, never its text, and another step none", func(t *testing.T) {
		for _, c := range []struct {
			err  error
			want string
		}{
			{fmt.Errorf("authz: %w", status.Error(codes.Unavailable, "dial tcp 10.1.2.3:50051: connect: connection refused")), "unavailable"},
			{status.Error(codes.Unauthenticated, "bad key k-secret"), "authentication_failed"},
			{status.Error(codes.DeadlineExceeded, "late"), "timeout"},
			{status.Error(codes.DataLoss, "lost"), "provider_error"},
			{errors.New("no row for buyer@example.com"), ""},
			// A class that is not in the strict form of a name is dropped (security review of PR #478, I2).
			{fmt.Errorf("authz: %w", ownClass("Refused by 10.1.2.3")), ""},
			{fmt.Errorf("authz: %w", ownClass("unavailable")), "unavailable"},
		} {
			step := &StepError{Step: StepAuthorizationSchema, Err: c.err}
			if got := step.CauseClass(); got != c.want {
				t.Errorf("the cause %v gave the class %q, want %q", c.err, got, c.want)
			}
		}
		// A real job whose SpiceDB refuses the connection fails at the step authorization_schema with the class
		// unavailable, which names no address.
		closed, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := closed.Addr().String()
		closed.Close()
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: "k-secret", InsecureLoopback: true}}
		err = Migrate(context.Background(), settings, f.migration(nil))
		var step *StepError
		if !errors.As(err, &step) || Stage(err) != "migrate."+StepAuthorizationSchema {
			t.Fatalf("a job whose SpiceDB refuses the connection gave %v, want a fault of the step %s", err, StepAuthorizationSchema)
		}
		var classed interface{ CauseClass() string }
		if !errors.As(err, &classed) || classed.CauseClass() != "unavailable" {
			t.Fatalf("the fault %v gives no class unavailable", err)
		}
	})
}

// TestMigrateRefusalReason proves the fixed class of the reason of a refusal of SpiceDB, such as of a schema write that
// would remove a definition that has relationships (walkthrough 22, small items).
func TestMigrateRefusalReason(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T32 A refusal of SpiceDB gives the class of its code and the fixed class of its reason, never its text", func(t *testing.T) {
		withReason := func(code codes.Code, message, reason string) error {
			s, err := status.New(code, message).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "authzed.com"})
			if err != nil {
				t.Fatal(err)
			}
			return s.Err()
		}
		for _, c := range []struct {
			err  error
			want string
		}{
			{fmt.Errorf("authz: SpiceDB refuses the schema of the release: %w", status.Error(codes.InvalidArgument,
				"cannot delete object definition `job` in schema, as a relationship exists under it")), "invalid_argument.relationships_exist"},
			{status.Error(codes.FailedPrecondition, "Cannot delete relation `viewer` in object definition `job`, as relationships exist under it"), "precondition_failed.relationships_exist"},
			{withReason(codes.InvalidArgument, "parse error at buyer@example.com", "ERROR_REASON_SCHEMA_PARSE_ERROR"), "invalid_argument.schema_parse_error"},
			// A reason that is not a value of the enum, and a refusal with no reason, give only the class of the code.
			{withReason(codes.InvalidArgument, "bad", "ERROR_REASON_SECRET_10.1.2.3"), "invalid_argument"},
			{withReason(codes.InvalidArgument, "bad", "ERROR_REASON_UNSPECIFIED"), "invalid_argument"},
			{status.Error(codes.InvalidArgument, "definition `job` has the key k-secret"), "invalid_argument"},
			// Only a refusal has a reason class.
			{withReason(codes.Unavailable, "cannot delete, relationships exist", "ERROR_REASON_SERVICE_READ_ONLY"), "unavailable"},
		} {
			step := &StepError{Step: StepAuthorizationSchema, Err: c.err}
			if got := step.CauseClass(); got != c.want {
				t.Errorf("the cause %v gave the class %q, want %q", c.err, got, c.want)
			}
		}
	})
}

// ownClass is a cause that gives its own cause class.
type ownClass string

func (c ownClass) Error() string      { return "own class " + string(c) }
func (c ownClass) CauseClass() string { return string(c) }

// namedFault is a cause that names its own fault, as a refused unique step of the adapter does.
type namedFault struct{}

func (namedFault) Error() string {
	return "the unique index of assets is not built: rows hold buyer@example.com"
}
func (namedFault) FaultClass() string  { return "schema_change.unique_duplicates" }
func (namedFault) FaultObject() string { return "assets_asset_tag_key" }
func (namedFault) FaultRemedy() string { return "correct or remove those rows" }

// TestMigrateFaults proves the fault class, the object and the remedy of the steps of the job that the failure record
// names (walkthrough 23, D-f).
func TestMigrateFaults(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T33 A job that cannot open the database or reach SpiceDB, or whose schema SpiceDB refuses, names the fault, the object and the remedy", func(t *testing.T) {
		fault := func(err error) [3]string {
			var named interface {
				FaultClass() string
				FaultObject() string
				FaultRemedy() string
			}
			if !errors.As(err, &named) {
				return [3]string{}
			}
			return [3]string{named.FaultClass(), named.FaultObject(), named.FaultRemedy()}
		}
		for _, c := range []struct {
			step         string
			err          error
			class, which string
		}{
			{StepOpen, fmt.Errorf("dial: %w", syscall.ECONNREFUSED), MigrateDatabaseFault, "database"},
			{StepAuthorizationSchema, status.Error(codes.Unavailable, "dial tcp 10.1.2.3:50051"), MigrateSpiceDBFault, "spicedb"},
			{StepSchemaAgain, status.Error(codes.InvalidArgument, "cannot delete relation `viewer`, as relationships exist under it"), MigrateSchemaRefusedFault, "spicedb"},
			{StepAuthorizationSchema, status.Error(codes.FailedPrecondition, "Cannot delete relation `viewer`, as relationships exist"), MigrateSchemaRefusedFault, "spicedb"},
			{StepSchemaServed, errors.New("job not served"), MigrateSchemaServedFault, "spicedb"},
			{StepTables, namedFault{}, "schema_change.unique_duplicates", "assets_asset_tag_key"},
			{StepLedgerCheck, errors.New("no ledger"), "", ""},
		} {
			got := fault(&StepError{Step: c.step, Err: c.err})
			if got[0] != c.class || got[1] != c.which || (c.class != "" && got[2] == "") {
				t.Errorf("the step %s with %v gave the fault %q, want the class %q and the object %q with a remedy", c.step, c.err, got, c.class, c.which)
			}
			if strings.Contains(got[2], "10.1.2.3") || strings.Contains(got[2], "buyer@") || strings.Contains(got[2], "viewer") {
				t.Errorf("the remedy %q holds a text of the cause", got[2])
			}
		}
		// A real job whose database refuses the connection fails at the step open with the fault of the database.
		closed, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := closed.Addr().(*net.TCPAddr).Port
		closed.Close()
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: fmt.Sprintf("postgres://migrator:pw@127.0.0.1:%d/app?sslmode=disable&connect_timeout=5", port),
			SpiceDB: SpiceDB{Endpoint: "127.0.0.1:1", Key: "k-secret", InsecureLoopback: true}}
		err = Migrate(context.Background(), settings, f.migration(nil))
		if Stage(err) != "migrate."+StepOpen {
			t.Fatalf("a job whose database refuses the connection gave the stage %s (%v), want migrate.%s", Stage(err), err, StepOpen)
		}
		if got := fault(err); got[0] != MigrateDatabaseFault || got[1] != "database" || got[2] == "" {
			t.Fatalf("the job at the step open gave the fault %q", got)
		}
	})
}

// jobLines gives the event name and the attributes of each JSON line of the migration job in out.
func jobLines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a line of the job is not JSON: %v: %s", err, line)
		}
		lines = append(lines, record)
	}
	return lines
}

func TestMigrateLines(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T34 The job writes a line when it starts, when each step starts and ends, and when it ends, with no value", func(t *testing.T) {
		var out bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(otel.NewSlogHandler(&out)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		attr := func(line map[string]any, key string) any {
			a, _ := line["attributes"].(map[string]any)
			return a["stego."+key]
		}
		// A job whose ledger check fails: the steps before it end ok, and its step ends failed, with no text of the
		// cause and no line of the end of the job.
		f := newFixture(t)
		m := f.migration(nil)
		m.LedgerCheck = func(context.Context, *sql.Conn) error {
			return errors.New("the row of buyer@example.test is not of this release")
		}
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: "127.0.0.1:1", Key: "k-private", InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, m); Stage(err) != "migrate."+StepLedgerCheck {
			t.Fatalf("the job gave the stage %s (%v), want migrate.%s", Stage(err), err, StepLedgerCheck)
		}
		var got []string
		for _, line := range jobLines(t, &out) {
			event, _ := line["event.name"].(string)
			step, _ := attr(line, "step").(string)
			outcome, _ := attr(line, "outcome").(string)
			got = append(got, strings.TrimSpace(fmt.Sprintf("%s %s %s %s", line["severity"], event, step, outcome)))
			if event == StepCompletedEvent && attr(line, "duration_seconds") == nil {
				t.Fatalf("the line %v has no duration", line)
			}
		}
		want := []string{"INFO migrate.job.started", "INFO migrate.step.started owner_role", "INFO migrate.step.completed owner_role ok",
			"INFO migrate.step.started job_lock", "INFO migrate.step.completed job_lock ok", "INFO migrate.step.started ledger_check",
			"ERROR migrate.step.completed ledger_check failed"}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("the job wrote the lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, value := range []string{"buyer@", "k-private", f.database, rolePassword} {
			if strings.Contains(out.String(), value) {
				t.Fatalf("a line of the job holds %q: %s", value, out.String())
			}
		}
		// A job that passes ends with migrate.job.completed, after the end of its last step.
		out.Reset()
		endpoint, key := startSpiceDB(t)
		f = newFixture(t)
		settings = MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		lines := jobLines(t, &out)
		started, completed := 0, 0
		for _, line := range lines {
			switch line["event.name"] {
			case StepStartedEvent:
				started++
			case StepCompletedEvent:
				if attr(line, "outcome") != "ok" || line["severity"] != "INFO" {
					t.Fatalf("a step of a job that passes ended with %v", line)
				}
				completed++
			}
		}
		last := lines[len(lines)-1]
		if started == 0 || started != completed || last["event.name"] != JobCompletedEvent || attr(last, "outcome") != "ok" ||
			attr(lines[len(lines)-2], "step") != StepReleaseIdentity {
			t.Fatalf("a job that passes wrote %d starts, %d ends and last %v, want the same count and migrate.job.completed after release_identity", started, completed, last)
		}
		if strings.Contains(out.String(), key) || strings.Contains(out.String(), rolePassword) {
			t.Fatalf("a line of the job holds the key or a password: %s", out.String())
		}
		// A value of STEGO_LOG_LEVEL that is not a level stops the job at the step settings, with no echo.
		t.Setenv(otel.LogLevelSetting, "verbose-private")
		if err := Migrate(context.Background(), settings, f.migration(nil)); Stage(err) != "migrate."+StepSettings || strings.Contains(err.Error(), "verbose-private") {
			t.Fatalf("a bad STEGO_LOG_LEVEL gave %v, want the stage migrate.%s and no echo of the value", err, StepSettings)
		}
	})
}

// The schemas of the read-back test: r1, and r2, which changes only the .zed schema with the same Release name.
const (
	catalogR1 = spiceSchema + `
definition org {
	relation owner: user
	permission catalog_edit = owner
}
definition cost_item {
	relation org: org
	permission edit = org->catalog_edit
}`
	catalogR2 = spiceSchema + `
definition org {
	relation owner: user
	relation frozen: user
	permission catalog_edit = owner
}
definition cost_item {
	relation org: org
	permission edit = org->catalog_edit - org->frozen
}`
)

// writing gives a schema writer of the job that writes text.
func writing(text string) func(ctx context.Context, conn grpc.ClientConnInterface) error {
	return func(ctx context.Context, conn grpc.ClientConnInterface) error {
		_, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: text})
		return err
	}
}

// reordered gives text with its definitions in the opposite order and a comment before each.
func reordered(text string) string {
	parts := strings.Split(text, "\ndefinition ")
	out := []string{}
	for i := len(parts) - 1; i >= 1; i-- {
		out = append(out, "// written again\ndefinition "+parts[i])
	}
	return strings.Join(append(out, parts[0]), "\n")
}

// emptySpiceDB deletes each relationship of each served definition, so that a schema write that removes a relation is
// not refused for relationships that another test wrote on the same local SpiceDB server (STEGO_TEST_SPICEDB).
func emptySpiceDB(t *testing.T, spice SpiceDB) {
	t.Helper()
	conn, err := DialSpiceDB(spice)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	read, err := v1.NewSchemaServiceClient(conn).ReadSchema(ctx, &v1.ReadSchemaRequest{})
	if status.Code(err) == codes.NotFound {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^definition\s+(\S+)`).FindAllStringSubmatch(read.GetSchemaText(), -1) {
		if _, err := v1.NewPermissionsServiceClient(conn).DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
			RelationshipFilter: &v1.RelationshipFilter{ResourceType: m[1]},
		}); err != nil {
			t.Fatalf("delete the relationships of %s: %v", m[1], err)
		}
	}
}

func TestMigrateSchemaReadBack(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T13 A pod uses a proof only while the served schema is its own release's: an older pod beside a newer schema, a failed read and a mismatched read-back fail closed, and order and comments do not change the digest", func(t *testing.T) {
		ctx := context.Background()
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		emptySpiceDB(t, settings.SpiceDB)
		// The later tests of the package write schemas with no cost_item on the same server.
		t.Cleanup(func() { emptySpiceDB(t, settings.SpiceDB) })
		job := func(text, schema string) Migration {
			m := f.migration(nil)
			m.WriteSchema, m.Schema = writing(text), schema
			return m
		}
		if err := Migrate(ctx, settings, job(catalogR1, catalogR1)); err != nil {
			t.Fatalf("the job of r1 gave %v, want its read-back to pass", err)
		}
		conn, err := DialSpiceDB(settings.SpiceDB)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		provider, err := authz.NewSpiceDB(conn)
		if err != nil {
			t.Fatal(err)
		}
		// The pod of r1 computes its digest at start from the schema of its release.
		pod, err := authz.NewSchemaGate(catalogR1, provider, 0)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(1793966400, 0)
		authzclock.SetGate(pod, func() time.Time { return now })
		if !pod.Holds(ctx) {
			t.Fatal("the pod of r1 does not hold beside the schema of r1")
		}
		// r2 changes only the .zed schema, with the same Release name: within the bound the pod uses its reading,
		// after it the pod decides at the provider.
		if err := Migrate(ctx, settings, job(catalogR2, catalogR2)); err != nil {
			t.Fatalf("the job of r2 gave %v", err)
		}
		now = now.Add(time.Second)
		if !pod.Holds(ctx) {
			t.Fatal("the pod of r1 does not use its reading within the bound")
		}
		now = now.Add(authz.SchemaDigestBound)
		if pod.Holds(ctx) {
			t.Fatal("the pod of r1 holds beside the schema of r2 after the bound")
		}
		// Another writer writes a schema between the write of the job and its read-back: the job fails at the step of
		// the read-back, with no schema text.
		beforeReadBack = func(ctx context.Context, conn grpc.ClientConnInterface) { _ = writing(catalogR2)(ctx, conn) }
		err = Migrate(ctx, settings, job(catalogR1, catalogR1))
		beforeReadBack = nil
		var step *StepError
		if !errors.As(err, &step) || step.Step != StepSchemaReadBack || strings.Contains(err.Error(), "definition") ||
			step.FaultClass() != MigrateSchemaReadFault {
			t.Fatalf("the job with a foreign write gave %v, want a fault of the step %s with no schema text", err, StepSchemaReadBack)
		}
		// The schema of r1 written again in another order and with comments has the digest of r1.
		if err := Migrate(ctx, settings, job(reordered(catalogR1), reordered(catalogR1))); err != nil {
			t.Fatalf("the job of the reordered r1 gave %v", err)
		}
		now = now.Add(authz.SchemaDigestBound)
		if !pod.Holds(ctx) {
			t.Fatal("the pod of r1 does not hold beside the reordered schema of r1")
		}
	})
}
