package check_test

// The relax steps of boardsrx against the release before, boardsrxbase, on a table with rows (ST-SUP-RC01-OC45;
// decision on finding F12 of the schema-change research, owner, 2026-11-13). The host test TestTables of
// internal/generator/postgresadapter/tables_test.go runs it in the generated module.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"example.com/tables-check/boardsrx"
	"example.com/tables-check/boardsrxbase"
	"example.com/tables-check/tenancy"
	"github.com/910-software-solutions/stego/registry/runtime/manifest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestRelaxSteps(t *testing.T) {
	cfg, admin := server(t)
	ctx := context.Background()

	// base makes a database that the release before migrated, with 2,000 boards of tenant-a, and gives its config,
	// its superuser pool and a pool of the runtime role.
	base := func(t *testing.T) (*pgx.ConnConfig, *sql.DB, *sql.DB) {
		t.Helper()
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return boardsrxbase.Migrate(ctx, conn) }, "boardsrxbase_access.json")
		if _, err := db.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title", "mood", "size", "kind")
  SELECT 'tenant-a', gen_random_uuid(), 'board ' || g, 'calm', 's', 'a' FROM generate_series(1, 2000) AS g`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'relax-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := own.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "relax-check"
		reader := stdlib.OpenDB(*runtime)
		t.Cleanup(func() { _ = reader.Close() })
		return own, db, reader
	}
	// before checks that the release before starts and works on the database: its schema check and its manifest
	// accept it, it writes a board of its own shape, and it reads every board, those of the new release too.
	before := func(t *testing.T, db, reader *sql.DB, after string) {
		t.Helper()
		if err := boardsrxbase.VerifySchema(ctx, db); err != nil {
			t.Errorf("after %s the schema check of the release before refuses the database: %v", after, err)
		}
		if err := manifest.Check(ctx, reader, owned(boardsrxbase.SchemaManifest)); err != nil {
			t.Errorf("after %s the schema manifest of the release before refuses the database: %v", after, err)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title", "mood", "size", "kind") VALUES ($1, $2, 'busy', 'xl', 'b')`,
			uuid.Must(uuid.NewV7()).String(), strings.Repeat("t", 40)); code != "" {
			t.Errorf("after %s the release before writes no board of its shape: %s", after, code)
		}
		var rows int
		if err := db.QueryRow(`SELECT count(*) FROM (SELECT "title", "mood", "size", "kind" FROM public."boards") AS b`).Scan(&rows); err != nil || rows < 2000 {
			t.Errorf("after %s the release before reads %d boards (error %v)", after, rows, err)
		}
	}

	t.Run("ST-SUP-RC01-OC45-T03 Each relax migration scans no row under its strong lock, and the release before starts and works after each one", func(t *testing.T) {
		_, db, reader := base(t)
		before(t, db, reader, "the migrations of the release before")
		job, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer job.Close()
		if _, err := job.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		names, err := os.ReadDir("../boardsrx/migrations")
		if err != nil {
			t.Fatal(err)
		}
		var applied []string
		for _, entry := range names {
			name := strings.TrimSuffix(entry.Name(), ".sql")
			if !strings.HasPrefix(name, "step_") {
				continue
			}
			body, err := os.ReadFile("../boardsrx/migrations/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			applied = append(applied, name)
			tx, err := job.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			const scanned = `SELECT coalesce((SELECT seq_scan + coalesce(idx_scan, 0) FROM pg_catalog.pg_stat_xact_user_tables WHERE relid = 'public.boards'::pg_catalog.regclass), 0)`
			var start int64
			if err := tx.QueryRowContext(ctx, scanned).Scan(&start); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				_ = tx.Rollback()
				t.Fatalf("%s: %v", name, err)
			}
			var scans int64
			var modes string
			if err := tx.QueryRowContext(ctx, scanned+`,
  coalesce((SELECT pg_catalog.string_agg(DISTINCT mode, ',') FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid() AND relation = 'public.boards'::pg_catalog.regclass), '')`).Scan(&scans, &modes); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			strong := strings.Contains(modes, "AccessExclusiveLock")
			switch scans -= start; {
			case strings.HasSuffix(name, "_validate"):
				// VALIDATE reads the rows under SHARE UPDATE EXCLUSIVE, which blocks no read and no write.
				if strong {
					t.Errorf("%s validates under the locks %s; want no ACCESS EXCLUSIVE lock", name, modes)
				}
			case scans != 0:
				t.Errorf("%s scans boards %d times while it holds the locks %s; want no scan", name, scans, modes)
			case !strong:
				t.Errorf("%s holds the locks %s; want the one ALTER TABLE of the step", name, modes)
			}
			if !strings.HasSuffix(name, "_validate") && strings.Count(string(body), "ALTER TABLE") > 2 {
				t.Errorf("%s runs more than two ALTER TABLE statements: %s", name, body)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			before(t, db, reader, name)
		}
		if len(applied) != 8 {
			t.Errorf("the relax steps of boardsrx are the migrations %v; want 8", applied)
		}
		// After every migration the release of the steps accepts the database, its constraints are validated, and it
		// writes what the release before could not.
		if err := manifest.Check(ctx, reader, owned(boardsrx.SchemaManifest)); err != nil {
			t.Errorf("the schema manifest of the release with the steps refuses the database: %v", err)
		}
		var invalid int
		if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_constraint WHERE conrelid = 'public.boards'::pg_catalog.regclass AND NOT convalidated`).Scan(&invalid); err != nil || invalid != 0 {
			t.Errorf("%d constraints of boards are not validated after the steps (error %v)", invalid, err)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title", "mood", "kind") VALUES ($1, $2, 'sleepy', 'free text')`,
			uuid.Must(uuid.NewV7()).String(), strings.Repeat("t", 80)); code != "" {
			t.Errorf("the release with the steps writes no relaxed board: %s", code)
		}
		var size string
		if err := db.QueryRow(`SELECT "size" FROM public."boards" WHERE "mood" = 'sleepy'`).Scan(&size); err != nil || size != "m" {
			t.Errorf("a board with no size gets %q, want the new default m (error %v)", size, err)
		}
		// The relaxed values are still bounded: a longer title and another mood are refused.
		for _, statement := range []string{
			`INSERT INTO public."boards" ("id", "title", "mood") VALUES ('` + uuid.NewString() + `', '` + strings.Repeat("t", 81) + `', 'calm')`,
			`INSERT INTO public."boards" ("id", "title", "mood") VALUES ('` + uuid.NewString() + `', 'x', 'angry')`,
			`INSERT INTO public."boards" ("id", "title", "mood") VALUES ('` + uuid.NewString() + `', '', 'calm')`,
		} {
			if code := write(t, db, "tenant-a", statement); code != "22001" && code != "23514" {
				t.Errorf("%s gives %q, want 22001 or 23514", statement, code)
			}
		}
		before(t, db, reader, "the boards of the release with the steps")
	})

	t.Run("ST-SUP-RC01-OC45-T04 The migration job takes a database with rows through the relax steps to the schema of a new database, and the release before migrates again on it", func(t *testing.T) {
		own, db, reader := base(t)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return boardsrx.Migrate(ctx, conn) }, "boardsrx_access.json")
		if err := boardsrx.VerifySchema(ctx, db); err != nil {
			t.Errorf("the release with the steps refuses the database after its job: %v", err)
		}
		before(t, db, reader, "the migration job of the release with the steps")
		// A rollback: the job of the release before runs again on the relaxed database and changes nothing.
		was := catalog(t, db)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return boardsrxbase.Migrate(ctx, conn) }, "boardsrxbase_access.json")
		if now := catalog(t, db); strings.Join(was, "\n") != strings.Join(now, "\n") {
			t.Errorf("the migration job of the release before changed the relaxed database")
		}
		before(t, db, reader, "the migration job of the release before")
		// A new database that the release with the steps migrates has the same schema (ST-GEN-INV35).
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return boardsrx.Migrate(ctx, conn) }, "boardsrx_access.json")
		if a, b := strings.Join(catalog(t, fresh), "\n"), strings.Join(catalog(t, db), "\n"); a != b {
			t.Errorf("the stepped database and a new one differ:\n%s\n----\n%s", b, a)
		}
		var marked int
		if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_attribute WHERE attrelid = 'public.boards'::pg_catalog.regclass AND pg_catalog.starts_with(pg_catalog.col_description(attrelid, attnum), 'stego relax step_')`).Scan(&marked); err != nil || marked != 4 {
			t.Errorf("%d columns of boards have the relax marker, want 4 (error %v)", marked, err)
		}
	})
}
