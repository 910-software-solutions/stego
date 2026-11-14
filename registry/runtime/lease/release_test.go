package lease

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
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
		// A report older than one day goes at the next report.
		d.exec(t, "INSERT INTO stego_schema.releases VALUES ('old', 'step_0001_x_expand', 'v0', 10000, now() - interval '25 hours')")
		beat, err := startBeat(ctx, pool, "step_0002_x_expand", "v2\x01é", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		first, ok := readRelease(t, d, beat.Holder())
		if !ok || first.release != "step_0002_x_expand" || first.version != "v2" || first.every != 1000 {
			t.Fatalf("after the start the report is %+v (there %v), want the release, the label v2 and 1000 ms", first, ok)
		}
		if _, old := readRelease(t, d, "old"); old {
			t.Error("the report older than one day is still there after a report")
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

func mustConfig(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return config
}
