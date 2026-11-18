package check_test

// The drop steps of dropc and drop against the release before, dropbase, on tables with rows (ST-SUP-RC01-OC34,
// ST-SUP-RC01-OC44, ST-SUP-RC01-OC47; ST-GEN-Q47). The host test TestTables of
// internal/generator/postgresadapter/tables_test.go runs it in the generated module.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"example.com/tables-check/drop"
	"example.com/tables-check/dropbase"
	"example.com/tables-check/dropc"
	"example.com/tables-check/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	runtimetenant "github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestDropSteps(t *testing.T) {
	cfg, admin := server(t)
	ctx := context.Background()

	// release runs the migration job of one release as the owner role and gives its error.
	release := func(t *testing.T, own *pgx.ConnConfig, run func(context.Context, *sql.Conn) error) error {
		t.Helper()
		job := stdlib.OpenDB(*own)
		defer job.Close()
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		return run(ctx, conn)
	}
	// base makes a database that the release before migrated, with the tenant list, 50 boards and 10 pins of
	// tenant-a, and gives its config, its superuser pool and a pool of the runtime role.
	base := func(t *testing.T) (*pgx.ConnConfig, *sql.DB, *sql.DB) {
		t.Helper()
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return dropbase.Migrate(ctx, conn) }, "dropbase_access.json")
		list, err := runtimetenant.Migration("public", tenancy.RuntimeRole)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize(), list, "INSERT INTO public.stego_tenants (tenant_id) VALUES ('tenant-a')"} {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title", "legacy", "rank", "note") SELECT gen_random_uuid(), 'b' || g, 'old', 3, 'n' FROM generate_series(1, 50) g`); code != "" {
			t.Fatalf("the release before writes no boards: %s", code)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."pins" ("id", "board_id", "label") SELECT gen_random_uuid(), "id", 'p' FROM public."boards" LIMIT 10`); code != "" {
			t.Fatalf("the release before writes no pins: %s", code)
		}
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'drop-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := own.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "drop-check"
		reader := stdlib.OpenDB(*runtime)
		t.Cleanup(func() { _ = reader.Close() })
		return own, db, reader
	}
	// expand migrates the release of the drops at expand.
	expand := func(t *testing.T, own *pgx.ConnConfig, db *sql.DB) {
		t.Helper()
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return drop.Migrate(ctx, conn) }, "drop_access.json")
	}
	// works checks that a release starts and works on the database: its schema check and its manifest accept it.
	works := func(t *testing.T, db, reader *sql.DB, name, after string, verify func(context.Context, *sql.DB) error, m manifest.Manifest) {
		t.Helper()
		if err := verify(ctx, db); err != nil {
			t.Errorf("after %s the schema check of %s refuses the database: %v", after, name, err)
		}
		if err := manifest.Check(ctx, reader, owned(m)); err != nil {
			t.Errorf("after %s the schema manifest of %s refuses the database: %v", after, name, err)
		}
	}

	t.Run("ST-SUP-RC01-OC34-T01 After a drop at expand the old column has a default and no NOT NULL", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		for _, c := range columns(t, db, "boards") {
			switch {
			case c.Name == "legacy" && (c.NotNull || c.Default != "''::character varying"):
				t.Errorf("after the expand legacy is %+v, want nullable with the default ''", c)
			case c.Name == "rank" && (c.NotNull || c.Default != "7"):
				t.Errorf("after the expand rank is %+v, want nullable with the default 7", c)
			}
		}
		// The release of the drops writes a board with neither column, and the release before reads it.
		id := uuid.Must(uuid.NewV7()).String()
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'new')`, id); code != "" {
			t.Fatalf("the release of the drops writes no board: %s", code)
		}
		var legacy sql.NullString
		var rank sql.NullInt32
		if err := db.QueryRow(`SELECT "legacy", "rank" FROM public."boards" WHERE "id" = $1`, id).Scan(&legacy, &rank); err != nil {
			t.Fatal(err)
		}
		if !legacy.Valid || legacy.String != "" || !rank.Valid || rank.Int32 != 7 {
			t.Errorf("the release before reads legacy %v and rank %v of the new board, want the empty string and 7", legacy, rank)
		}
		var nulls int
		if err := db.QueryRow(`SELECT count(*) FROM public."boards" WHERE "legacy" IS NULL OR "rank" IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
			t.Errorf("%d boards hold NULL in an old column (error %v)", nulls, err)
		}
		// Both releases start and work on the database; the release before still writes its own shape.
		works(t, db, reader, "the release before", "the expand", func(ctx context.Context, db *sql.DB) error { return dropbase.VerifySchema(ctx, db) }, dropbase.SchemaManifest)
		works(t, db, reader, "the release of the drops", "the expand", func(ctx context.Context, db *sql.DB) error { return drop.VerifySchema(ctx, db) }, drop.SchemaManifest)
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title", "legacy", "rank") VALUES ($1, 'v1', 'still', 4)`, uuid.Must(uuid.NewV7()).String()); code != "" {
			t.Errorf("the release before writes no board of its shape after the expand: %s", code)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."pins" ("id", "board_id", "label") VALUES ($1, $2, 'v1')`, uuid.Must(uuid.NewV7()).String(), id); code != "" {
			t.Errorf("the release before writes no pin after the expand: %s", code)
		}
	})

	t.Run("ST-SUP-RC01-OC44-T03 The contract of a drop refuses while the release before reports itself, and then drops the column and the table", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		// A process of the release before and one of the release of the drops report themselves.
		old, current := strings.Repeat("a", 32), strings.Repeat("b", 32)
		for _, beat := range [][2]string{{old, "100_tenancy_row_security"}, {current, "step_0003_pin-drop_expand"}} {
			if _, err := reader.Exec("SELECT stego_schema.release_beat($1, $2, 'v', 10000)", beat[0], beat[1]); err != nil {
				t.Fatal(err)
			}
		}
		before := catalog(t, db)
		err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) })
		var refused interface{ Refusal() string }
		if !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "board-legacy-drop") || !strings.Contains(refused.Refusal(), "step_0001_board-legacy-drop_expand") {
			t.Fatalf("the contract while the release before reports ends with %v, want a refusal that names the change and its expand migration", err)
		}
		if after := catalog(t, db); !slices.Equal(before, after) {
			t.Errorf("the refused contract changed the catalog or the ledger:\n%s", strings.Join(after, "\n"))
		}
		// The release before stops: its process deletes its report.
		if _, err := reader.Exec("SELECT stego_schema.release_stop($1)", old); err != nil {
			t.Fatal(err)
		}
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the contract after the release before stopped: %v", err)
		}
		for _, c := range columns(t, db, "boards") {
			if c.Name == "legacy" || c.Name == "rank" {
				t.Errorf("the old column %s is still there after the contract", c.Name)
			}
		}
		var pins bool
		if err := db.QueryRow(`SELECT pg_catalog.to_regclass('public.pins') IS NOT NULL`).Scan(&pins); err != nil || pins {
			t.Errorf("the table pins is still there after the contract (error %v)", err)
		}
		var last, set string
		if err := db.QueryRow(`SELECT version, compatible_with::text FROM stego_schema.migrations ORDER BY version COLLATE "C" DESC LIMIT 1`).Scan(&last, &set); err != nil {
			t.Fatal(err)
		}
		if last != "step_0006_pin-drop_contract" || set != "{step_0003_pin-drop_expand}" {
			t.Errorf("the last ledger row is %s with the set %s, want the contract of pin-drop with its expand migration", last, set)
		}
		// The release of the drops and the release of the contract start and work after the contract; a run again
		// changes nothing.
		works(t, db, reader, "the release of the drops", "the contract", func(ctx context.Context, db *sql.DB) error { return drop.VerifySchema(ctx, db) }, drop.SchemaManifest)
		works(t, db, reader, "the release of the contract", "the contract", func(ctx context.Context, db *sql.DB) error { return dropc.VerifySchema(ctx, db) }, dropc.SchemaManifest)
		if code := write(t, db, "tenant-a", `INSERT INTO public."boards" ("id", "title") VALUES ($1, 'v3')`, uuid.Must(uuid.NewV7()).String()); code != "" {
			t.Errorf("the release of the contract writes no board: %s", code)
		}
		again := catalog(t, db)
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the migration job again after the contract: %v", err)
		}
		if after := catalog(t, db); !slices.Equal(again, after) {
			t.Errorf("the migration job again changed the catalog:\n%s", strings.Join(after, "\n"))
		}
	})

	t.Run("ST-SUP-RC01-OC44-T04 A release before that reports itself while the contract of an entity drop waits for its lock leaves the table with its keys", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		// A session holds a lock on pins, so the first statement of the contract of pin-drop waits for it after the
		// check of the step passed; while it waits, a process of the release before reports itself.
		hold, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(`LOCK TABLE public."pins" IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- release(t, own, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) }) }()
		for waiting := 0; waiting == 0; {
			if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_locks WHERE relation = 'public.pins'::regclass AND NOT granted`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := reader.Exec("SELECT stego_schema.release_beat($1, '100_tenancy_row_security', 'v1', 10000)", strings.Repeat("d", 32)); err != nil {
			t.Fatal(err)
		}
		if err := hold.Rollback(); err != nil {
			t.Fatal(err)
		}
		var refused interface{ Refusal() string }
		if err := <-done; !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "pin-drop") || !strings.Contains(refused.Refusal(), "the old table") {
			t.Fatalf("the contract ends with %v, want a refusal of pin-drop that names the old table", err)
		}
		var keys, ledger int
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM pg_catalog.pg_constraint WHERE conrelid = 'public.pins'::regclass AND contype = 'f'),
  (SELECT count(*) FROM stego_schema.migrations WHERE version = 'step_0006_pin-drop_contract')`).Scan(&keys, &ledger); err != nil {
			t.Fatal(err)
		}
		if keys != 1 || ledger != 0 {
			t.Errorf("after the refused contract pins has %d foreign keys and the ledger %d rows of the step; want the key pins_board_id_fkey and no row", keys, ledger)
		}
		if code := write(t, db, "tenant-a", `INSERT INTO public."pins" ("id", "board_id", "label") VALUES ($1, $2, 'v1')`, uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()); code != "23503" {
			t.Errorf("a pin of the release before with no board gives %q after the refused contract, want 23503 from its kept key", code)
		}
	})

	t.Run("ST-SUP-RC01-OC47-T02 A database that the three releases migrated in turn and a new one get the same schema", func(t *testing.T) {
		own, db, _ := base(t)
		expand(t, own, db)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) }, "dropc_access.json")
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return dropc.Migrate(ctx, conn) }, "dropc_access.json")
		list, err := runtimetenant.Migration("public", tenancy.RuntimeRole)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := fresh.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize(), list} {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// The grants of the release before stay on the upgraded database, as a migration job grants only; the
		// catalog compares the tables, the columns, the constraints, the indexes, the triggers, the policies and the
		// ledger.
		strip := func(lines []string) []string {
			var out []string
			for _, line := range lines {
				if strings.HasPrefix(line, "0|") {
					parts := strings.Split(line, "|")
					line = strings.Join(parts[:len(parts)-1], "|")
				}
				out = append(out, line)
			}
			return out
		}
		upgraded, created := strip(catalog(t, db)), strip(catalog(t, fresh))
		if !slices.Equal(upgraded, created) {
			t.Errorf("the upgraded database and the new one differ:\nupgraded:\n%s\nnew:\n%s", strings.Join(upgraded, "\n"), strings.Join(created, "\n"))
		}
	})
}
