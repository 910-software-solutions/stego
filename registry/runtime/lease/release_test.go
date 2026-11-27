package lease

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/910-software-solutions/stego/registry/runtime/internal/pgtest"
)

// releaseRow is the report of one holder.
type releaseRow struct {
	release, version string
	every            int
	seen             time.Time
}

func readRelease(t *testing.T, d *database, holder string) (releaseRow, bool) {
	t.Helper()
	var r releaseRow
	err := d.other.QueryRowContext(context.Background(), "SELECT release, version, every_ms, seen_at FROM stego_schema.releases WHERE holder = $1", holder).Scan(&r.release, &r.version, &r.every, &r.seen)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

// sqlState gives the SQLSTATE of err, or empty.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestReleaseReports(t *testing.T) {
	t.Run("ST-SUP-RP09-LC14-T01 A process reports its release until Stop, and the runtime role can only report and stop its own process", func(t *testing.T) {
		ctx := context.Background()
		d := openDatabase(t)
		role := "lease_release_rt"
		d.exec(t, "DROP ROLE IF EXISTS "+role)
		d.exec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'lease-release'")
		t.Cleanup(func() {
			_, _ = d.other.Exec("DROP OWNED BY " + role)
			_, _ = d.other.Exec("DROP ROLE IF EXISTS " + role)
		})
		if _, err := Releases("Bad-Role"); err == nil {
			t.Error("Releases accepts a role that is not a lower-case SQL name")
		}
		text, err := Releases(role)
		if err != nil {
			t.Fatal(err)
		}
		// The text runs again with no error on a database that it made.
		for range 2 {
			d.exec(t, text)
		}
		config, err := pgx.ParseConfig(d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.User, config.Password = role, "lease-release"
		pool := stdlib.OpenDB(*config)
		t.Cleanup(func() { _ = pool.Close() })
		// A report older than one day stays at a report of another process, and goes at the cleanup of the owner.
		d.exec(t, "INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v0', 10000, now() - interval '25 hours')")
		beat, err := startBeat(ctx, pool, "step_0002_x_expand", "v2\x01é", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		first, ok := readRelease(t, d, beat.Holder())
		if !ok || first.release != "step_0002_x_expand" || first.version != "v2" || first.every != 1000 {
			t.Fatalf("after the start the report is %+v (there %v), want the release, the label v2 and 1000 ms", first, ok)
		}
		if _, old := readRelease(t, d, "old"); !old {
			t.Error("a report of another process deleted the report older than one day")
		}
		clean(t, d)
		if _, old := readRelease(t, d, "old"); old {
			t.Error("the report older than one day is still there after the cleanup")
		}
		if _, ok := readRelease(t, d, beat.Holder()); !ok {
			t.Error("the cleanup deleted a current report")
		}
		if len(beat.Holder()) != 32 {
			t.Errorf("the holder %q is not 16 random bytes in hexadecimal", beat.Holder())
		}
		time.Sleep(2500 * time.Millisecond)
		if again, _ := readRelease(t, d, beat.Holder()); !again.seen.After(first.seen) {
			t.Errorf("the report did not come again: first %v, then %v", first.seen, again.seen)
		}
		// The runtime role can neither read nor change the table, nor report out of range.
		for _, statement := range []string{"SELECT count(*) FROM stego_schema.releases",
			"UPDATE stego_schema.releases SET seen_at = now()", "DELETE FROM stego_schema.releases"} {
			if _, err := pool.ExecContext(ctx, statement); sqlState(err) != "42501" {
				t.Errorf("the runtime role runs %q with %v, want 42501", statement, err)
			}
		}
		for _, args := range [][]any{{"not-hex", "step_0002_x_expand", "v2", 1000}, {strings.Repeat("d", 32), "Step", "v2", 1000},
			{strings.Repeat("d", 32), "step_0002_x_expand", strings.Repeat("v", 65), 1000}, {strings.Repeat("d", 32), "step_0002_x_expand", "v2", 999},
			{strings.Repeat("d", 32), "step_0002_x_expand", "v2", 60001}, {strings.Repeat("d", 32), nil, "v2", 1000}} {
			if _, err := pool.ExecContext(ctx, "SELECT stego_schema.release_beat($1, $2, $3, $4)", args...); sqlState(err) != "22023" {
				t.Errorf("the report %v gives %v, want 22023", args, err)
			}
		}
		// A stop of another holder changes nothing; Stop deletes the process's own report, once.
		if _, err := pool.ExecContext(ctx, "SELECT stego_schema.release_stop($1)", strings.Repeat("e", 32)); err != nil {
			t.Fatal(err)
		}
		if _, ok := readRelease(t, d, beat.Holder()); !ok {
			t.Fatal("the stop of another holder deleted the report")
		}
		if err := beat.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := readRelease(t, d, beat.Holder()); ok {
			t.Error("the report is there after Stop")
		}
		if err := beat.Stop(ctx); err != nil {
			t.Errorf("a second Stop: %v", err)
		}
		time.Sleep(1500 * time.Millisecond)
		if _, ok := readRelease(t, d, beat.Holder()); ok {
			t.Error("a report came after Stop")
		}
		// On a database where no step made the function, the start does not fail.
		bare := stdlib.OpenDB(*mustConfig(t, pgtest.Database(t, "lease_bare")))
		t.Cleanup(func() { _ = bare.Close() })
		other, err := startBeat(ctx, bare, "001_initial", "v1", time.Second)
		if err != nil {
			t.Fatalf("the start on a database with no report function: %v", err)
		}
		_ = other.Stop(ctx)
		if _, err := StartBeat(ctx, pool, "Not A Migration", "v1"); err == nil {
			t.Error("StartBeat accepts a release that is not a migration name")
		}
	})
}

// clean runs CleanReleases in a transaction of the owner role, as the migration job does, within 2 s.
func clean(t *testing.T, d *database) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := d.other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
		t.Fatal(err)
	}
	if err := CleanReleases(ctx, tx); err != nil {
		t.Fatalf("the cleanup of the reports: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseHeldRow(t *testing.T) {
	t.Run("ST-SUP-RP09-LC14-T02 A session that holds its own report does not stop the report of another process, and the cleanup passes over it", func(t *testing.T) {
		ctx := context.Background()
		d := openDatabase(t)
		role := "lease_release_held"
		d.exec(t, "DROP ROLE IF EXISTS "+role)
		d.exec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'lease-release'")
		t.Cleanup(func() {
			_, _ = d.other.Exec("DROP OWNED BY " + role)
			_, _ = d.other.Exec("DROP ROLE IF EXISTS " + role)
		})
		text, err := Releases(role)
		if err != nil {
			t.Fatal(err)
		}
		d.exec(t, text)
		// Before any table, the cleanup does nothing and does not fail.
		bare := stdlib.OpenDB(*mustConfig(t, pgtest.Database(t, "lease_clean_bare")))
		t.Cleanup(func() { _ = bare.Close() })
		bareTx, err := bare.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := CleanReleases(ctx, bareTx); err != nil {
			t.Errorf("the cleanup on a database with no table of the reports: %v", err)
		}
		_ = bareTx.Rollback()
		config := mustConfig(t, d.dsn)
		config.User, config.Password = role, "lease-release"
		pool := stdlib.OpenDB(*config)
		t.Cleanup(func() { _ = pool.Close() })
		// The runtime role reports a row of its own that is almost one day old, and holds it in an open transaction
		// until its committed version is older than one day; another old row is not held.
		held, v1 := strings.Repeat("f", 32), strings.Repeat("9", 32)
		if _, err := pool.ExecContext(ctx, "SELECT stego_schema.release_beat($1, 'step_9999_zz', 'x', 10000)", held); err != nil {
			t.Fatal(err)
		}
		d.exec(t, "UPDATE stego_schema.releases SET seen_at = clock_timestamp() - interval '1 day' + interval '2 seconds' WHERE holder = $1", held)
		d.exec(t, "INSERT INTO stego_schema.releases VALUES ('stale', 'step_0001_x_expand', 'v0', 10000, now() - interval '25 hours')")
		if _, err := pool.ExecContext(ctx, "SELECT stego_schema.release_beat($1, 'step_0001_x_expand', 'v1', 1000)", v1); err != nil {
			t.Fatal(err)
		}
		hold, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Rollback()
		if _, err := hold.ExecContext(ctx, "SELECT stego_schema.release_beat($1, 'step_9999_zz', 'x', 10000)", held); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		// Each report of the other process, under a time limit of one of its intervals, succeeds.
		for i := range 5 {
			beatCtx, cancel := context.WithTimeout(ctx, time.Second)
			_, err := pool.ExecContext(beatCtx, "SELECT stego_schema.release_beat($1, 'step_0001_x_expand', 'v1', 1000)", v1)
			cancel()
			if err != nil {
				t.Errorf("report %d of another process while a session holds its own row: %v", i+1, err)
			}
		}
		if r, ok := readRelease(t, d, v1); !ok || time.Since(r.seen) > 2*time.Second {
			t.Errorf("the report of the other process is %+v (there %v), want a current one", r, ok)
		}
		// The cleanup of the owner passes over the held row with no wait and deletes the other old row.
		clean(t, d)
		if _, ok := readRelease(t, d, "stale"); ok {
			t.Error("the cleanup left the old row that no session holds")
		}
		if _, ok := readRelease(t, d, held); !ok {
			t.Error("the cleanup deleted the held row")
		}
		_ = hold.Rollback()
		clean(t, d)
		if _, ok := readRelease(t, d, held); ok {
			t.Error("the cleanup left the old row after its session ended")
		}
		// Both functions have a lock timeout of 1 s, and the table an index on the report time.
		var timeouts, indexed int
		if err := d.other.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p WHERE p.pronamespace = 'stego_schema'::regnamespace
 AND p.proname IN ('release_beat', 'release_stop') AND 'lock_timeout=1s' = ANY (p.proconfig)`).Scan(&timeouts); err != nil {
			t.Fatal(err)
		}
		if err := d.other.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_indexes WHERE schemaname = 'stego_schema' AND tablename = 'releases'
 AND indexdef LIKE '%(seen_at)'`).Scan(&indexed); err != nil {
			t.Fatal(err)
		}
		if timeouts != 2 || indexed != 1 {
			t.Errorf("%d report functions have the lock timeout of 1 s and %d indexes are on seen_at; want 2 and 1", timeouts, indexed)
		}
	})
}

func mustConfig(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// failedStatements records each statement of a pool and each one that ended with an error.
type failedStatements struct {
	mu           sync.Mutex
	sent, faults []string
}

func (f *failedStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, data.SQL)
	return context.WithValue(ctx, failedKey{}, data.SQL)
}

func (f *failedStatements) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	text, _ := ctx.Value(failedKey{}).(string)
	f.faults = append(f.faults, text)
}

func (f *failedStatements) read() (sent, faults []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...), append([]string(nil), f.faults...)
}

type failedKey struct{}

func TestReleaseQuiet(t *testing.T) {
	t.Run("ST-SUP-RP09-LC14-T03 Before a step makes the functions of the reports, the report and Stop send no statement that fails, and the first report after the step calls release_beat", func(t *testing.T) {
		ctx := context.Background()
		d := openDatabase(t)
		role := "lease_quiet_rt"
		d.exec(t, "DROP ROLE IF EXISTS "+role)
		d.exec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'lease-quiet'")
		t.Cleanup(func() {
			_, _ = d.other.Exec("DROP OWNED BY " + role)
			_, _ = d.other.Exec("DROP ROLE IF EXISTS " + role)
		})
		config := mustConfig(t, d.dsn)
		config.User, config.Password = role, "lease-quiet"
		trace := &failedStatements{}
		config.Tracer = trace
		pool := stdlib.OpenDB(*config)
		t.Cleanup(func() { _ = pool.Close() })
		// The interval is the longest that release_beat takes, so the ticker does not run in the test: each report
		// below is one call of beat.
		beat, err := startBeat(ctx, pool, "step_0001_x_expand", "v1", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			beat.beat(ctx)
		}
		// A process that stops before any step made the functions deletes nothing and sends no statement that fails.
		early, err := startBeat(ctx, pool, "step_0001_x_expand", "v1", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := early.Stop(ctx); err != nil {
			t.Fatalf("Stop before the step: %v", err)
		}
		sent, faults := trace.read()
		if len(faults) != 0 {
			t.Fatalf("before the step the reports sent statements that failed: %q", faults)
		}
		for _, statement := range sent {
			if strings.Contains(statement, "release_beat(") || strings.Contains(statement, "release_stop(") {
				t.Fatalf("before the step a report called a function of the reports: %q", statement)
			}
		}
		if len(sent) != 6 {
			t.Errorf("five reports and one Stop sent %d statements, want one catalog read each", len(sent))
		}
		// The step makes the functions; the next report calls release_beat and writes the report.
		text, err := Releases(role)
		if err != nil {
			t.Fatal(err)
		}
		// An overload of release_beat alone does not make the reports start: only the exact signatures count.
		d.exec(t, "CREATE SCHEMA IF NOT EXISTS stego_schema")
		d.exec(t, "GRANT USAGE ON SCHEMA stego_schema TO "+role)
		d.exec(t, "CREATE FUNCTION stego_schema.release_beat(text) RETURNS void LANGUAGE sql AS 'SELECT NULL'")
		d.exec(t, "CREATE FUNCTION stego_schema.release_stop(integer) RETURNS void LANGUAGE sql AS 'SELECT NULL'")
		d.exec(t, "CREATE FUNCTION stego_schema.release_stop(text, text) RETURNS void LANGUAGE sql AS 'SELECT NULL'")
		beat.beat(ctx)
		if sent, faults := trace.read(); len(faults) != 0 || len(sent) != 7 {
			t.Fatalf("with the overloads only, the report sent %d statements and %q failed, want one catalog read and none failed", len(sent), faults)
		}
		// The text makes the exact signatures; with the overloads still there, the next report calls release_beat.
		d.exec(t, text)
		beat.beat(ctx)
		if _, ok := readRelease(t, d, beat.Holder()); !ok {
			t.Fatal("the first report after the step wrote no report")
		}
		if err := beat.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := readRelease(t, d, beat.Holder()); ok {
			t.Error("the report is there after Stop")
		}
		if _, faults := trace.read(); len(faults) != 0 {
			t.Fatalf("the reports sent statements that failed: %q", faults)
		}
	})
}

func TestReleaseFirstBeat(t *testing.T) {
	t.Run("ST-SUP-RP09-LC14-T04 A first report that fails while the function is there stops the start", func(t *testing.T) {
		ctx := context.Background()
		d := openDatabase(t)
		role := "lease_first_beat_rt"
		d.exec(t, "DROP ROLE IF EXISTS "+role)
		d.exec(t, "CREATE ROLE "+role+" LOGIN PASSWORD 'lease-first'")
		t.Cleanup(func() {
			_, _ = d.other.Exec("DROP OWNED BY " + role)
			_, _ = d.other.Exec("DROP ROLE IF EXISTS " + role)
		})
		config, err := pgx.ParseConfig(d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.User, config.Password = role, "lease-first"
		pool := stdlib.OpenDB(*config)
		t.Cleanup(func() { _ = pool.Close() })
		// No step has made the function: the start goes on, and the report is a quiet no-op.
		beat, err := startBeat(ctx, pool, "100_tenancy_row_security", "v1", time.Second)
		if err != nil {
			t.Fatalf("a start with no report function gives %v", err)
		}
		beat.Stop(ctx)
		text, err := Releases(role)
		if err != nil {
			t.Fatal(err)
		}
		d.exec(t, text)
		// The function is there and its call fails: the start fails, so the process restarts.
		d.exec(t, "REVOKE EXECUTE ON FUNCTION stego_schema.release_beat(text, text, text, integer) FROM "+role)
		if beat, err := startBeat(ctx, pool, "100_tenancy_row_security", "v1", time.Second); err == nil {
			beat.Stop(ctx)
			t.Fatal("a start whose first report fails gives no error")
		}
		d.exec(t, "GRANT EXECUTE ON FUNCTION stego_schema.release_beat(text, text, text, integer) TO "+role)
		beat, err = startBeat(ctx, pool, "100_tenancy_row_security", "v1", time.Second)
		if err != nil {
			t.Fatalf("a start whose first report works gives %v", err)
		}
		beat.Stop(ctx)
	})
}
