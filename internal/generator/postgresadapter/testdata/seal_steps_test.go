package check_test

// The seal steps of seal and sealc against the release before, sealbase, on tables with rows (ST-SUP-RC01-OC49,
// ST-SUP-RC01-OC50, ST-SUP-RP10-LC50; decision ST-DECL-Q44; walkthrough 13, W13-B1). sealbase stores Vault.note, a
// pii field, and Vault.code, a secret field of a generator before postgres-adapter 6.13.0, in clear; seal seals note
// from pii and code from secret at expand, and sealc contracts both. The host test TestTables of
// internal/generator/postgresadapter/tables_test.go runs it in the generated module.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"example.com/tables-check/seal"
	"example.com/tables-check/sealbase"
	"example.com/tables-check/sealc"
	"example.com/tables-check/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/910-software-solutions/stego/registry/runtime/command"
	"github.com/910-software-solutions/stego/registry/runtime/lease"
	"github.com/910-software-solutions/stego/registry/runtime/manifest"
	"github.com/910-software-solutions/stego/registry/runtime/sealed"
	runtimetenant "github.com/910-software-solutions/stego/registry/runtime/tenant"
)

func TestSealSteps(t *testing.T) {
	cfg, admin := server(t)
	ctx := context.Background()
	// A throwaway key ring, which lives only in the memory of the test.
	key := make([]byte, sealed.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := sealed.ParseRing([]byte("1 " + base64.StdEncoding.EncodeToString(key) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	clear(key)

	// older makes the seal checkpoints ten minutes old: a first pass waits three minutes after the expand, so each
	// process of a release before reports itself first (security review of PR #443, L1).
	older := func(t *testing.T, db *sql.DB) {
		t.Helper()
		if _, err := db.Exec(`UPDATE stego_schema.seals SET created_at = created_at - interval '10 minutes'`); err != nil {
			t.Fatal(err)
		}
	}
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
	// base makes a database that the release before migrated, with the tenant list of tenant-a and tenant-b and
	// their vaults in clear, the lease table with the backfill lease of each seal, and gives its config, its superuser
	// pool and a pool of the runtime role.
	base := func(t *testing.T) (*pgx.ConnConfig, *sql.DB, *sql.DB) {
		t.Helper()
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return sealbase.Migrate(ctx, conn) }, "sealbase_access.json")
		list, err := runtimetenant.Migration("public", tenancy.RuntimeRole)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET LOCAL ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize(), list,
			"INSERT INTO public.stego_tenants (tenant_id) VALUES ('tenant-a'), ('tenant-b')", lease.LeasesTable,
			"INSERT INTO public.leases (name) VALUES ('stego.backfill.vault-note-seal'), ('stego.backfill.vault-code-seal')",
			"GRANT SELECT, UPDATE ON TABLE public.leases TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()} {
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, tenant := range []string{"tenant-a", "tenant-b"} {
			// The release before writes 30 vaults of each tenant; each third has no note.
			if code := write(t, db, tenant, `INSERT INTO public."vaults" ("id", "title", "note", "code") SELECT gen_random_uuid(), 't' || g,
  CASE WHEN g % 3 = 0 THEN NULL ELSE 'member-' || g || '@example.com' END, 'code-' || g FROM generate_series(1, 30) g`); code != "" {
				t.Fatalf("the release before writes no vaults: %s", code)
			}
		}
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'seal-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := own.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "seal-check"
		reader := stdlib.OpenDB(*runtime)
		t.Cleanup(func() { _ = reader.Close() })
		return own, db, reader
	}
	expand := func(t *testing.T, own *pgx.ConnConfig, db *sql.DB) {
		t.Helper()
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return seal.Migrate(ctx, conn) }, "seal_access.json")
		older(t, db)
	}
	works := func(t *testing.T, db, reader *sql.DB, name, after string, verify func(context.Context, *sql.DB) error, m manifest.Manifest) {
		t.Helper()
		if err := verify(ctx, db); err != nil {
			t.Errorf("after %s the schema check of %s refuses the database: %v", after, name, err)
		}
		if err := manifest.Check(ctx, reader, owned(m)); err != nil {
			t.Errorf("after %s the schema manifest of %s refuses the database: %v", after, name, err)
		}
	}
	// job runs the seal job of the release of the seals once over the tenant list of public.
	job := func(t *testing.T, reader *sql.DB) []command.SealResult {
		t.Helper()
		j, err := command.NewSealJob(reader, "public", ring, seal.SealColumns, "holder-"+uuid.NewString(), lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		results, err := j.Run(ctx)
		if err != nil {
			t.Fatalf("the seal job: %v", err)
		}
		return results
	}
	// stopped moves each report of the release before back by one minute: the process stopped a minute ago, and its
	// report stays (ST-SUP-RP09-LC15).
	stopped := func(t *testing.T, db *sql.DB) {
		t.Helper()
		if _, err := db.Exec(`UPDATE stego_schema.releases SET seen_at = seen_at - interval '1 minute' WHERE release COLLATE "C" < 'step_0001' COLLATE "C"`); err != nil {
			t.Fatal(err)
		}
	}
	// agree fails when a vault holds a clear value whose sealed value is not set or opens to another text, or a
	// sealed value with no clear value.
	agree := func(t *testing.T, db *sql.DB) int {
		t.Helper()
		rows, err := db.Query(`SELECT "tenant_id", "id"::text, "note", "note_sealed", "code", "code_sealed" FROM public."vaults"`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var tenant, id string
			var note, code sql.NullString
			var noteSealed, codeSealed []byte
			if err := rows.Scan(&tenant, &id, &note, &noteSealed, &code, &codeSealed); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				column string
				clear  sql.NullString
				raw    []byte
			}{{"note_sealed", note, noteSealed}, {"code_sealed", code, codeSealed}} {
				if !c.clear.Valid {
					if c.raw != nil {
						t.Errorf("the row %s holds a sealed %s and no clear value", id, c.column)
					}
					continue
				}
				plain, err := ring.Open(c.raw, sealed.Place{Tenant: tenant, Table: "vaults", Column: c.column, Row: id})
				if err != nil || string(plain) != c.clear.String {
					t.Errorf("the row %s holds a sealed %s that is not its clear value (%v)", id, c.column, err)
				}
			}
			n++
		}
		return n
	}
	beat := func(t *testing.T, reader *sql.DB, holder, release string) {
		t.Helper()
		if _, err := reader.Exec("SELECT stego_schema.release_beat($1, $2, 'v', 10000)", holder, release); err != nil {
			t.Fatal(err)
		}
	}
	old := strings.Repeat("a", 32)

	t.Run("ST-SUP-RC01-OC49-T02 A seal at expand keeps the release before working, the seal job seals each row, and a roll back to the release before loses no write", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		for _, c := range columns(t, db, "vaults") {
			if (c.Name == "note_sealed" || c.Name == "code_sealed") && (c.Type != "bytea" || c.NotNull || c.Default != "") {
				t.Errorf("after the expand %s is %+v, want nullable bytea with no default", c.Name, c)
			}
		}
		// Both releases start and work on the database, side by side: the release before writes the clear columns
		// alone, and the release of the seals writes both.
		works(t, db, reader, "the release before", "the expand", func(ctx context.Context, db *sql.DB) error { return sealbase.VerifySchema(ctx, db) }, sealbase.SchemaManifest)
		works(t, db, reader, "the release of the seals", "the expand", func(ctx context.Context, db *sql.DB) error { return seal.VerifySchema(ctx, db) }, seal.SchemaManifest)
		if code := write(t, db, "tenant-a", `INSERT INTO public."vaults" ("id", "title", "note", "code") VALUES ($1, 'before', 'n@example.com', 'c-before')`, uuid.NewString()); code != "" {
			t.Errorf("the release before writes no vault after the expand: %s", code)
		}
		id := uuid.NewString()
		noteSealed, err := ring.Seal([]byte("both@example.com"), sealed.Place{Tenant: "tenant-b", Table: "vaults", Column: "note_sealed", Row: id})
		if err != nil {
			t.Fatal(err)
		}
		codeSealed, err := ring.Seal([]byte("c-both"), sealed.Place{Tenant: "tenant-b", Table: "vaults", Column: "code_sealed", Row: id})
		if err != nil {
			t.Fatal(err)
		}
		if code := write(t, db, "tenant-b", `INSERT INTO public."vaults" ("id", "title", "note", "note_sealed", "code", "code_sealed") VALUES ($1, 'both', 'both@example.com', $2, 'c-both', $3)`, id, noteSealed, codeSealed); code != "" {
			t.Errorf("the release of the seals writes no vault: %s", code)
		}
		// A process of the release before reports itself: the seal job waits.
		beat(t, reader, old, sealbase.SchemaRelease)
		for _, r := range job(t, reader) {
			if !r.Waits || r.Checked != 0 {
				t.Fatalf("the seal job gave %+v while the release before runs, want it to wait", r)
			}
		}
		// The release before stops; its report stays, a minute old.
		if _, err := reader.Exec("SELECT stego_schema.release_stop($1)", old); err != nil {
			t.Fatal(err)
		}
		var kept int
		if err := db.QueryRow(`SELECT count(*) FROM stego_schema.releases WHERE holder = $1`, old).Scan(&kept); err != nil || kept != 1 {
			t.Fatalf("the stop of the release before deleted its report while the seals are open (%d, %v)", kept, err)
		}
		stopped(t, db)
		for _, r := range job(t, reader) {
			if !r.Completed || r.Checked != 62 {
				t.Fatalf("the seal job gave %+v, want the pass completed over the 62 vaults", r)
			}
		}
		if n := agree(t, db); n != 62 {
			t.Fatalf("%d vaults, want 62", n)
		}
		// A roll back: the release before runs again, writes the clear note of one vault and clears another, and
		// stops cleanly.
		beat(t, reader, old, sealbase.SchemaRelease)
		if code := write(t, db, "tenant-b", `UPDATE public."vaults" SET "note" = 'rolled@example.com' WHERE "id" = $1`, id); code != "" {
			t.Fatalf("the release before writes no vault after the roll back: %s", code)
		}
		if code := write(t, db, "tenant-a", `UPDATE public."vaults" SET "note" = NULL WHERE "note" IS NOT NULL AND "title" = 't1'`); code != "" {
			t.Fatalf("the release before clears no note: %s", code)
		}
		if _, err := reader.Exec("SELECT stego_schema.release_stop($1)", old); err != nil {
			t.Fatal(err)
		}
		// The last pass completed ten minutes ago and the release before stopped a minute ago, after it.
		if _, err := db.Exec(`UPDATE stego_schema.seals SET pass_started = pass_started - interval '10 minutes', completed_at = completed_at - interval '10 minutes'`); err != nil {
			t.Fatal(err)
		}
		stopped(t, db)
		err = release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) })
		var refused interface{ Refusal() string }
		if !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "ran after the start") {
			t.Fatalf("the contract after the roll back ends with %v, want a refusal that names the release before that ran after the pass", err)
		}
		for _, r := range job(t, reader) {
			if !r.Completed {
				t.Fatalf("the seal job after the roll back gave %+v, want a new pass completed", r)
			}
		}
		agree(t, db)
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the contract after the new pass: %v", err)
		}
		// After the contract each value is in its sealed column alone, and the write of the roll back is there.
		for _, c := range columns(t, db, "vaults") {
			switch c.Name {
			case "note", "code":
				t.Errorf("the clear column %s is still there after the contract", c.Name)
			case "code_sealed":
				if !c.NotNull {
					t.Error("the sealed column of the required code is nullable after the contract")
				}
			}
		}
		var raw []byte
		if err := db.QueryRow(`SELECT "note_sealed" FROM public."vaults" WHERE "id" = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if plain, err := ring.Open(raw, sealed.Place{Tenant: "tenant-b", Table: "vaults", Column: "note_sealed", Row: id}); err != nil || string(plain) != "rolled@example.com" {
			t.Errorf("the note that the release before wrote in the roll back is %q after the contract (%v)", plain, err)
		}
		var cleared int
		if err := db.QueryRow(`SELECT count(*) FROM public."vaults" WHERE "title" = 't1' AND "tenant_id" = 'tenant-a' AND "note_sealed" IS NULL`).Scan(&cleared); err != nil || cleared != 1 {
			t.Errorf("the note that the release before cleared has a sealed value after the contract (%d, %v)", cleared, err)
		}
		var open int
		if err := db.QueryRow(`SELECT count(*) FROM stego_schema.seals`).Scan(&open); err != nil || open != 0 {
			t.Errorf("the contract leaves %d seal checkpoints (%v)", open, err)
		}
		works(t, db, reader, "the release of the contract", "the contract", func(ctx context.Context, db *sql.DB) error { return sealc.VerifySchema(ctx, db) }, sealc.SchemaManifest)
	})

	t.Run("ST-SUP-RC01-OC49-T08 The job of a later release on a database from before the seal makes the lease of the seal job with the expand step", func(t *testing.T) {
		// A restore of a backup of the release before: the lease table is there with no lease of a seal job, as the job
		// of the release of the expand step made it (walkthrough 14, W14-B7).
		own, db, reader := base(t)
		if _, err := db.Exec(`DELETE FROM public.leases WHERE name LIKE 'stego.backfill.%'`); err != nil {
			t.Fatal(err)
		}
		// The job of the release of the contract step applies the expand step and refuses at the contract.
		var refused interface{ Refusal() string }
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); !errors.As(err, &refused) ||
			!strings.Contains(refused.Refusal(), "has not completed a pass") || !strings.Contains(refused.Refusal(), "run the migration job of the release of the expand step") {
			t.Fatalf("the job of the contract release on the database before the seal ends with %v, want the refusal of the contract with the remedy", err)
		}
		var leases int
		if err := db.QueryRow(`SELECT count(*) FROM public.leases WHERE name IN ('stego.backfill.vault-note-seal', 'stego.backfill.vault-code-seal')`).Scan(&leases); err != nil {
			t.Fatal(err)
		}
		if leases != 2 {
			t.Fatalf("the expand steps made %d leases of the seal jobs, want 2", leases)
		}
		// The seal job of the release of the expand step runs on that database, and the contract then passes.
		older(t, db)
		for _, result := range job(t, reader) {
			if !result.Completed {
				t.Fatalf("the seal job on the database gives %+v, want a completed pass", result)
			}
		}
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the job of the contract release after the pass: %v", err)
		}
	})

	t.Run("ST-SUP-RC01-OC49-T12 A database whose seal expand ran with postgres-adapter 6.19.0 or 6.20.0 passes the ledger check, gets the functions of this release, and then seals and contracts", func(t *testing.T) {
		earlier := seal.EarlierMigrationDigests()
		for i, version := range []string{"6.19.0", "6.20.0"} {
			t.Run(version, func(t *testing.T) {
				own, db, reader := base(t)
				expand(t, own, db)
				for _, r := range job(t, reader) {
					if !r.Completed {
						t.Fatalf("the seal job gave %+v, want the pass completed", r)
					}
				}
				// The database as the migration job of that version left it: the ledger rows of the expand steps hold
				// the digests of the bodies of that version, and the functions of the seal checkpoints are its own.
				for _, name := range []string{"step_0001_vault-note-seal_expand", "step_0002_vault-code-seal_expand"} {
					if len(earlier[name]) != 2 {
						t.Fatalf("the release gives the earlier digests %v of %s, want 2", earlier[name], name)
					}
					if _, err := db.Exec(`UPDATE stego_schema.migrations SET digest = $1 WHERE version = $2`, earlier[name][i], name); err != nil {
						t.Fatal(err)
					}
				}
				old := []string{
					// 6.20.0: the database instance is the database and the start of its server alone.
					`CREATE OR REPLACE FUNCTION stego_schema.seal_instance() RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
  AS $f$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS') FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$f$`,
					`UPDATE stego_schema.seals SET armed_in = stego_schema.seal_instance()`,
				}
				if version == "6.19.0" {
					// 6.19.0: no record of the database instance at all.
					old = []string{`ALTER TABLE stego_schema.seals DROP COLUMN armed_in, DROP COLUMN armed_at`, `DROP FUNCTION stego_schema.seal_instance()`}
				}
				for _, statement := range old {
					if _, err := db.Exec(statement); err != nil {
						t.Fatalf("%.60s: %v", statement, err)
					}
				}
				// The migration job of this release passes the ledger check and puts the functions of this release in place.
				migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return seal.Migrate(ctx, conn) }, "seal_access.json")
				var current bool
				if err := db.QueryRow(`SELECT pg_catalog.pg_get_functiondef('stego_schema.seal_instance()'::regprocedure) LIKE '%pg_relation_filenode%'`).Scan(&current); err != nil || !current {
					t.Fatalf("after the migration job the database instance is not the one of this release (%v, %v)", current, err)
				}
				// The pass of the earlier version does not count: the first run waits, and the next completes a new pass.
				for _, r := range job(t, reader) {
					if r.Completed || !r.Waits {
						t.Fatalf("the first run of the seal job after the upgrade gave %+v, want it to wait", r)
					}
				}
				if _, err := db.Exec(`UPDATE stego_schema.seals SET armed_at = armed_at - interval '4 minutes'`); err != nil {
					t.Fatal(err)
				}
				for _, r := range job(t, reader) {
					if !r.Completed {
						t.Fatalf("the seal job 3 minutes after the first sight gave %+v, want a new pass completed", r)
					}
				}
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
					t.Fatalf("the contract after the new pass: %v", err)
				}
			})
		}
	})

	t.Run("ST-SUP-RC01-OC50-T05 The contract release as the first release of postgres-adapter 6.21.0 on a database of 6.19.0 or 6.20.0 puts the functions of this release in place, refuses with the remedy, and passes after a pass of the expand release", func(t *testing.T) {
		earlier := seal.EarlierMigrationDigests()
		for i, version := range []string{"6.19.0", "6.20.0"} {
			t.Run(version, func(t *testing.T) {
				own, db, reader := base(t)
				expand(t, own, db)
				for _, r := range job(t, reader) {
					if !r.Completed {
						t.Fatalf("the seal job gave %+v, want the pass completed", r)
					}
				}
				// The database as the migration job of that version left it (as in ST-SUP-RC01-OC49-T12).
				for _, name := range []string{"step_0001_vault-note-seal_expand", "step_0002_vault-code-seal_expand"} {
					if _, err := db.Exec(`UPDATE stego_schema.migrations SET digest = $1 WHERE version = $2`, earlier[name][i], name); err != nil {
						t.Fatal(err)
					}
				}
				old := []string{
					`CREATE OR REPLACE FUNCTION stego_schema.seal_instance() RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
  AS $f$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS') FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$f$`,
					`UPDATE stego_schema.seals SET armed_in = stego_schema.seal_instance()`,
				}
				if version == "6.19.0" {
					old = []string{`ALTER TABLE stego_schema.seals DROP COLUMN armed_in, DROP COLUMN armed_at`, `DROP FUNCTION stego_schema.seal_instance()`}
				}
				for _, statement := range old {
					if _, err := db.Exec(statement); err != nil {
						t.Fatalf("%.60s: %v", statement, err)
					}
				}
				// The migration job of the contract release runs first: it puts the functions of this release in place
				// before the check of the contract step, and refuses with the remedy.
				var refused interface{ Refusal() string }
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); !errors.As(err, &refused) ||
					!strings.Contains(refused.Refusal(), "this database instance") || !strings.Contains(refused.Refusal(), "build it again with postgres-adapter 6.21.0") {
					t.Fatalf("the contract release first ends with %v, want a refusal that names the instance and the remedy", err)
				}
				var current bool
				if err := db.QueryRow(`SELECT pg_catalog.pg_get_functiondef('stego_schema.seal_instance()'::regprocedure) LIKE '%pg_relation_filenode%'`).Scan(&current); err != nil || !current {
					t.Fatalf("after the refused contract the database instance is not the one of this release (%v, %v)", current, err)
				}
				// The release of the expand step, built again with this adapter, completes a pass: its first run waits.
				for _, r := range job(t, reader) {
					if r.Completed || !r.Waits {
						t.Fatalf("the first run of the seal job gave %+v, want it to wait", r)
					}
				}
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "has not completed a pass") {
					t.Fatalf("the contract before the new pass ends with %v, want a refusal that names the pass", err)
				}
				if _, err := db.Exec(`UPDATE stego_schema.seals SET armed_at = armed_at - interval '4 minutes'`); err != nil {
					t.Fatal(err)
				}
				for _, r := range job(t, reader) {
					if !r.Completed {
						t.Fatalf("the seal job 3 minutes after the first sight gave %+v, want a new pass completed", r)
					}
				}
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
					t.Fatalf("the contract after the new pass: %v", err)
				}
			})
		}
	})

	// waits gives the query of the backend of the pool application that waits for a lock of the kind event, or "".
	waits := func(t *testing.T, db *sql.DB, application, event string) string {
		t.Helper()
		var query sql.NullString
		err := db.QueryRow(`SELECT a.query FROM pg_catalog.pg_stat_activity a WHERE a.application_name = $1 AND a.wait_event_type = 'Lock' AND a.wait_event = $2`, application, event).Scan(&query)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return query.String
	}

	t.Run("ST-SUP-RC01-OC50-T06 The check of a seal contract runs the functions of this release in its own transaction, also when another job put older functions in place after the start of the job", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		for _, r := range job(t, reader) {
			if !r.Completed {
				t.Fatalf("the seal job gave %+v, want the pass completed", r)
			}
		}
		// The test holds the ledger lock on a connection of its own. Each time that the job waits for it at a
		// transaction lock, one statement gives the lock to the job and asks for it again, so the test gets it back
		// when that transaction ends. When the job waits at the session lock of the contract step, after the run of
		// the seal checkpoints at its start, the test puts the seal_instance of postgres-adapter 6.20.0 in place, as a
		// job of a release before can do in that time, and gives the lock to the job.
		gate, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Close()
		if _, err := gate.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_lock(1937007983,1935894637)`); err != nil {
			t.Fatal(err)
		}
		named := own.Copy()
		named.RuntimeParams["application_name"] = "seal-contract-t06"
		done := make(chan error, 1)
		jobDB := stdlib.OpenDB(*named)
		defer jobDB.Close()
		go func() {
			conn, err := jobDB.Conn(ctx)
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
				done <- err
				return
			}
			done <- sealc.Migrate(ctx, conn)
		}()
		for {
			query := waits(t, db, "seal-contract-t06", "advisory")
			if query == "" {
				select {
				case err := <-done:
					t.Fatalf("the contract job ended with %v before it waited for the ledger lock of the contract step", err)
				default:
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if !strings.Contains(query, "pg_advisory_xact_lock") {
				break
			}
			if _, err := gate.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_unlock(1937007983,1935894637), pg_catalog.pg_advisory_lock(1937007983,1935894637)`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`CREATE OR REPLACE FUNCTION stego_schema.seal_instance() RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
  AS $f$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS') FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$f$`); err != nil {
			t.Fatal(err)
		}
		if _, err := gate.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_unlock(1937007983,1935894637)`); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("the contract with older functions in place after the start of the job ends with %v, want it to pass", err)
		}
		var current bool
		if err := db.QueryRow(`SELECT pg_catalog.pg_get_functiondef('stego_schema.seal_instance()'::regprocedure) LIKE '%pg_relation_filenode%'`).Scan(&current); err != nil || !current {
			t.Fatalf("after the contract seal_instance is not the one of this release (%v, %v)", current, err)
		}
	})

	t.Run("ST-SUP-RC01-OC49-T13 A migration job again on a database with the seal checkpoints takes no ACCESS EXCLUSIVE lock on their table", func(t *testing.T) {
		own, db, _ := base(t)
		expand(t, own, db)
		before := catalog(t, db)
		// A reader holds an ACCESS SHARE lock on the table of the seal checkpoints while the job runs again. An ALTER
		// TABLE would wait for it past the lock timeout of each attempt, and the job would fail.
		reading, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reading.Rollback() }()
		var rows int
		if err := reading.QueryRow(`SELECT pg_catalog.count(*) FROM stego_schema.seals`).Scan(&rows); err != nil || rows != 2 {
			t.Fatalf("the reader sees %d seal rows (%v), want 2", rows, err)
		}
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return seal.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the migration job again while a reader holds the seal checkpoints: %v", err)
		}
		if err := reading.Rollback(); err != nil {
			t.Fatal(err)
		}
		if after := catalog(t, db); !slices.Equal(before, after) {
			t.Errorf("the migration job again changed the catalog:\n%s", strings.Join(after, "\n"))
		}
		var defaulted bool
		if err := db.QueryRow(`SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid) LIKE '%seal_instance()' FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
  WHERE d.adrelid = 'stego_schema.seals'::regclass AND a.attname = 'armed_in'`).Scan(&defaulted); err != nil || !defaulted {
			t.Fatalf("armed_in has no default of seal_instance after the job again (%v, %v)", defaulted, err)
		}
	})

	t.Run("ST-SUP-RC01-OC50-T01 The contract of a seal refuses before a completed pass, while a release before runs and while a row is not sealed, and a run again recovers", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		contract := func() error {
			return release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) })
		}
		var refused interface{ Refusal() string }
		before := catalog(t, db)
		if err := contract(); !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "has not completed a pass") || !strings.Contains(refused.Refusal(), "vault-note-seal") {
			t.Fatalf("the contract before a pass ends with %v, want a refusal that names the change and the pass", err)
		}
		if after := catalog(t, db); !slices.Equal(before, after) {
			t.Errorf("the refused contract changed the catalog or the ledger:\n%s", strings.Join(after, "\n"))
		}
		job(t, reader)
		// A process of the release before reports itself after the pass: the contract refuses.
		beat(t, reader, old, sealbase.SchemaRelease)
		if err := contract(); !errors.As(err, &refused) {
			t.Fatalf("the contract while the release before reports itself ends with %v, want a refusal", err)
		}
		if _, err := db.Exec(`DELETE FROM stego_schema.releases`); err != nil {
			t.Fatal(err)
		}
		// A vault of a tenant that the tenant list does not hold has a clear code and no sealed one: the job passes
		// over it, and the VALIDATE of the completeness CHECK refuses it.
		if code := write(t, db, "tenant-z", `INSERT INTO public."vaults" ("id", "title", "code") VALUES ($1, 'z', 'c-z')`, uuid.NewString()); code != "" {
			t.Fatalf("no vault of tenant-z: %s", code)
		}
		err := contract()
		if !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "vault-code-seal") || !strings.Contains(refused.Refusal(), "tenant list") {
			t.Fatalf("the contract over a vault that is not sealed ends with %v, want a refusal that names the change and the unlisted tenant", err)
		}
		var clearColumns, ledger int
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM pg_catalog.pg_attribute WHERE attrelid = 'public.vaults'::regclass AND attname IN ('note', 'code') AND NOT attisdropped),
  (SELECT count(*) FROM stego_schema.migrations WHERE version LIKE '%_contract')`).Scan(&clearColumns, &ledger); err != nil {
			t.Fatal(err)
		}
		// The contract of note, whose rows are all sealed, ran in full before the contract of code failed.
		if clearColumns != 1 || ledger != 1 {
			t.Fatalf("after the failed contract the table has %d clear columns and the ledger %d contract rows; want code and the contract of note", clearColumns, ledger)
		}
		// The row goes, and the migration job runs again from the CHECK that the failed run left.
		if code := write(t, db, "tenant-z", `DELETE FROM public."vaults"`); code != "" {
			t.Fatal(code)
		}
		if err := contract(); err != nil {
			t.Fatalf("the contract again: %v", err)
		}
		again := catalog(t, db)
		if err := contract(); err != nil {
			t.Fatalf("the migration job again after the contract: %v", err)
		}
		if after := catalog(t, db); !slices.Equal(again, after) {
			t.Errorf("the migration job again changed the catalog:\n%s", strings.Join(after, "\n"))
		}
	})

	t.Run("ST-SUP-RC01-OC50-T08 A seal over the rows of a tenant retired before or during the seal completes its contract and leaves no clear value", func(t *testing.T) {
		own, db, reader := base(t)
		retire := func(id string) {
			t.Helper()
			if code := write(t, db, id, `SELECT public.stego_retire_tenant($1)`, id); code != "" {
				t.Fatalf("the retire of %s: %s", id, code)
			}
		}
		// tenant-b is retired before the expand step of the seal, and tenant-a while the seal is open. A Retire
		// deletes no row, so the vaults of both stay with their clear values.
		retire("tenant-b")
		expand(t, own, db)
		retire("tenant-a")
		var active int
		if err := db.QueryRow(`SELECT count(*) FROM public.stego_tenants WHERE retired_at IS NULL`).Scan(&active); err != nil || active != 0 {
			t.Fatalf("%d active tenants after the retires (%v), want none", active, err)
		}
		// The clear values that the contract drops, by tenant and row.
		type value struct{ note, code sql.NullString }
		values := map[string]value{}
		rows, err := db.Query(`SELECT "tenant_id" || '/' || "id"::text, "note", "code" FROM public."vaults"`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var key string
			var v value
			if err := rows.Scan(&key, &v.note, &v.code); err != nil {
				t.Fatal(err)
			}
			values[key] = v
		}
		if err := rows.Close(); err != nil || len(values) != 60 {
			t.Fatalf("%d vaults (%v), want the 60 of the two retired tenants", len(values), err)
		}
		for _, r := range job(t, reader) {
			if !r.Completed || r.Checked != 60 {
				t.Fatalf("the seal job gave %+v, want the pass completed over the 60 vaults of the retired tenants", r)
			}
		}
		if n := agree(t, db); n != 60 {
			t.Fatalf("%d vaults agree, want 60", n)
		}
		var clearOnly int
		if err := db.QueryRow(`SELECT count(*) FROM public."vaults" WHERE ("note" IS NOT NULL AND "note_sealed" IS NULL) OR ("code" IS NOT NULL AND "code_sealed" IS NULL)`).Scan(&clearOnly); err != nil || clearOnly != 0 {
			t.Fatalf("%d vaults hold a clear value with no sealed value after the pass (%v), want none", clearOnly, err)
		}
		if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
			t.Fatalf("the contract after a completed pass with retired tenants: %v", err)
		}
		// No clear column stays, and each sealed value of each retired tenant opens to the value that it held in
		// clear.
		for _, c := range columns(t, db, "vaults") {
			if c.Name == "note" || c.Name == "code" {
				t.Errorf("the clear column %s is still there after the contract", c.Name)
			}
		}
		rows, err = db.Query(`SELECT "tenant_id", "id"::text, "note_sealed", "code_sealed" FROM public."vaults"`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var tenant, id string
			var noteSealed, codeSealed []byte
			if err := rows.Scan(&tenant, &id, &noteSealed, &codeSealed); err != nil {
				t.Fatal(err)
			}
			v := values[tenant+"/"+id]
			for _, c := range []struct {
				column string
				clear  sql.NullString
				raw    []byte
			}{{"note_sealed", v.note, noteSealed}, {"code_sealed", v.code, codeSealed}} {
				if !c.clear.Valid {
					if c.raw != nil {
						t.Errorf("the vault %s of %s holds a sealed %s and had no value", id, tenant, c.column)
					}
					continue
				}
				plain, err := ring.Open(c.raw, sealed.Place{Tenant: tenant, Table: "vaults", Column: c.column, Row: id})
				if err != nil || string(plain) != c.clear.String {
					t.Errorf("the vault %s of %s holds a %s that is not its value before the contract (%v)", id, tenant, c.column, err)
				}
			}
			n++
		}
		if err := rows.Err(); err != nil || n != 60 {
			t.Fatalf("%d vaults after the contract (%v), want 60", n, err)
		}
	})

	t.Run("ST-SUP-RC01-OC50-T09 After a pass of a runtime before v0.208.0 that passed a retired tenant, completed or cut, the seal job of this runtime seals its rows and the contract passes", func(t *testing.T) {
		for _, mode := range []string{"completed", "cut"} {
			t.Run(mode, func(t *testing.T) {
				own, db, reader := base(t)
				expand(t, own, db)
				if code := write(t, db, "tenant-a", `SELECT public.stego_retire_tenant('tenant-a')`); code != "" {
					t.Fatalf("the retire of tenant-a: %s", code)
				}
				// The seal job of a runtime before v0.208.0 read the active tenants alone: it runs here over a copy of
				// the tenant list that holds only them.
				for _, statement := range []string{"CREATE SCHEMA oldlist",
					"CREATE TABLE oldlist.stego_tenants AS SELECT * FROM public.stego_tenants WHERE retired_at IS NULL",
					"GRANT USAGE ON SCHEMA oldlist TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize(),
					"GRANT SELECT ON oldlist.stego_tenants TO " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize()} {
					if _, err := db.Exec(statement); err != nil {
						t.Fatalf("%.60s: %v", statement, err)
					}
				}
				old, err := command.NewSealJob(reader, "oldlist", ring, seal.SealColumns, "holder-old", lease.Settings{})
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range func() []command.SealResult {
					results, err := old.Run(ctx)
					if err != nil {
						t.Fatalf("the old seal job: %v", err)
					}
					return results
				}() {
					if !r.Completed || r.Checked != 30 {
						t.Fatalf("the old seal job gave %+v, want the pass completed over the 30 vaults of tenant-b", r)
					}
				}
				if mode == "cut" {
					// The old binary stopped after tenant-b, before the end of its pass, as at the rollout.
					if _, err := db.Exec(`UPDATE stego_schema.seals SET completed_at = NULL`); err != nil {
						t.Fatal(err)
					}
				}
				var clearOnly int
				if err := db.QueryRow(`SELECT count(*) FROM public."vaults" WHERE "tenant_id" = 'tenant-a' AND "code" IS NOT NULL AND "code_sealed" IS NULL`).Scan(&clearOnly); err != nil || clearOnly != 30 {
					t.Fatalf("the old pass left %d codes of tenant-a in clear (%v), want 30", clearOnly, err)
				}
				// The contract refuses while the rows of tenant-a are in clear.
				var refused interface{ Refusal() string }
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); !errors.As(err, &refused) {
					t.Fatalf("the contract after the old pass ends with %v, want a refusal", err)
				}
				// This runtime: the seal job of the tenant list starts a new pass.
				for _, r := range job(t, reader) {
					if !r.Completed || r.Checked != 60 {
						t.Fatalf("the seal job of this runtime gave %+v, want a new pass completed over the 60 vaults", r)
					}
				}
				if n := agree(t, db); n != 60 {
					t.Fatalf("%d vaults agree, want 60", n)
				}
				if err := db.QueryRow(`SELECT count(*) FROM public."vaults" WHERE ("note" IS NOT NULL AND "note_sealed" IS NULL) OR ("code" IS NOT NULL AND "code_sealed" IS NULL)`).Scan(&clearOnly); err != nil || clearOnly != 0 {
					t.Fatalf("%d vaults hold a clear value with no sealed value (%v), want none", clearOnly, err)
				}
				if err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }); err != nil {
					t.Fatalf("the contract after the pass of this runtime: %v", err)
				}
				for _, c := range columns(t, db, "vaults") {
					if c.Name == "note" || c.Name == "code" {
						t.Errorf("the clear column %s is still there after the contract", c.Name)
					}
				}
			})
		}
	})

	t.Run("ST-SUP-RC01-OC50-T07 A contract of a seal that waits for its pass gives the failure record its fault class, the table, the remedy and the cause class", func(t *testing.T) {
		own, db, _ := base(t)
		expand(t, own, db)
		err := release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) })
		// The failure record of the process takes these fixed names and texts, and never the text of the refusal,
		// which names the change (walkthrough 21, U-F).
		var classified interface {
			FaultClass() string
			FaultObject() string
			FaultRemedy() string
		}
		if !errors.As(err, &classified) || classified.FaultClass() != "schema_change.seal_pending" || classified.FaultObject() != "stego_schema.seals" ||
			!strings.Contains(classified.FaultRemedy(), "until the seal job completes a pass") {
			t.Fatalf("the contract before a pass ends with %v, whose failure record names no fault class, table and remedy", err)
		}
		if strings.Contains(classified.FaultRemedy(), "vault-note-seal") {
			t.Errorf("the remedy names the change: %s", classified.FaultRemedy())
		}
		var caused interface{ CauseClass() string }
		if !errors.As(err, &caused) || caused.CauseClass() != "refused" {
			t.Errorf("the contract before a pass ends with %v, whose failure record names no cause class refused", err)
		}
	})

	t.Run("ST-SUP-RC01-OC50-T03 A release before that reports itself while the contract of a seal waits for its lock leaves the clear column", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		job(t, reader)
		// A session holds a lock on vaults, so the first statement of the contract waits for it after the check of the
		// step passed; while it waits, a process of the release before reports itself (security review of PR #443, M1).
		hold, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(`LOCK TABLE public."vaults" IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }) }()
		for waiting := 0; waiting == 0; {
			if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_locks WHERE relation = 'public.vaults'::regclass AND NOT granted`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
		beat(t, reader, strings.Repeat("e", 32), sealbase.SchemaRelease)
		if err := hold.Rollback(); err != nil {
			t.Fatal(err)
		}
		var refused interface{ Refusal() string }
		if err := <-done; !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "vault-note-seal") {
			t.Fatalf("the contract ends with %v, want a refusal of vault-note-seal", err)
		}
		var clearColumns int
		if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_attribute WHERE attrelid = 'public.vaults'::regclass AND attname IN ('note', 'code') AND NOT attisdropped`).Scan(&clearColumns); err != nil {
			t.Fatal(err)
		}
		if clearColumns != 2 {
			t.Errorf("after the refused contract the table has %d clear columns, want 2", clearColumns)
		}
	})

	t.Run("ST-SUP-RC01-OC50-T04 The contract of a seal refuses a completed pass that did not run in this database instance until the seal job completes a new pass", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		for _, r := range job(t, reader) {
			if !r.Completed {
				t.Fatalf("the seal job gave %+v, want the pass completed", r)
			}
		}
		// A restore brings the completed passes from a dump of another database instance (security review of PR #471,
		// L3).
		if _, err := db.Exec(`UPDATE stego_schema.seals SET armed_in = '1/20261101000000000000/1/1', armed_at = clock_timestamp() - interval '9 minutes'`); err != nil {
			t.Fatal(err)
		}
		contract := func() error {
			return release(t, own, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) })
		}
		var refused interface{ Refusal() string }
		before := catalog(t, db)
		if err := contract(); !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "this database instance") || !strings.Contains(refused.Refusal(), "vault-note-seal") {
			t.Fatalf("the contract over a pass of another instance ends with %v, want a refusal that names the change and the instance", err)
		}
		if after := catalog(t, db); !slices.Equal(before, after) {
			t.Errorf("the refused contract changed the catalog or the ledger:\n%s", strings.Join(after, "\n"))
		}
		// The first run of the seal job ends the passes and waits; the contract still refuses.
		for _, r := range job(t, reader) {
			if r.Completed || !r.Waits {
				t.Fatalf("the first run of the seal job after the restore gave %+v, want it to wait", r)
			}
		}
		if err := contract(); !errors.As(err, &refused) || !strings.Contains(refused.Refusal(), "has not completed a pass") {
			t.Fatalf("the contract after the first run ends with %v, want a refusal that names the pass", err)
		}
		if _, err := db.Exec(`UPDATE stego_schema.seals SET armed_at = armed_at - interval '4 minutes'`); err != nil {
			t.Fatal(err)
		}
		for _, r := range job(t, reader) {
			if !r.Completed {
				t.Fatalf("the seal job 3 minutes after the first sight gave %+v, want a new pass completed", r)
			}
		}
		if err := contract(); err != nil {
			t.Fatalf("the contract after the new pass: %v", err)
		}
	})

	t.Run("ST-SUP-RC01-OC50-T02 A database that the three releases of a seal migrated in turn and a new one get the same schema", func(t *testing.T) {
		own, db, reader := base(t)
		expand(t, own, db)
		job(t, reader)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }, "sealc_access.json")
		freshCfg, fresh := database(t, cfg, admin)
		// A new database runs the expand and the contract of each seal on its empty tables, after a pass of the job
		// over no row, which the contract needs.
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return seal.Migrate(ctx, conn) }, "seal_access.json")
		list, err := runtimetenant.Migration("public", tenancy.RuntimeRole)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET ROLE " + pgx.Identifier{tenancy.OwnerRole}.Sanitize() + "; " + list + "; " + lease.LeasesTable +
			"; INSERT INTO public.leases (name) VALUES ('stego.backfill.vault-note-seal'), ('stego.backfill.vault-code-seal'); GRANT SELECT, UPDATE ON TABLE public.leases TO " +
			pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + "; RESET ROLE"} {
			if _, err := fresh.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fresh.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'seal-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := freshCfg.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "seal-check"
		freshReader := stdlib.OpenDB(*runtime)
		defer freshReader.Close()
		older(t, fresh)
		job(t, freshReader)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return sealc.Migrate(ctx, conn) }, "sealc_access.json")
		// The grants of the releases stay on the upgraded database, as a migration job grants only; the catalog
		// compares the tables, the columns, the constraints, the indexes, the triggers, the policies and the ledger.
		strip := func(lines []string) []string {
			var out []string
			for _, line := range lines {
				if strings.HasPrefix(line, "0|") {
					parts := strings.Split(line, "|")
					line = strings.Join(parts[:len(parts)-1], "|")
				}
				if strings.Contains(line, "stego_tenants") || strings.Contains(line, "leases") {
					continue
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
