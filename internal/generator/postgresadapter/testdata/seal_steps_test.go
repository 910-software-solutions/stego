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

	"example.com/tables-check/seal"
	"example.com/tables-check/sealbase"
	"example.com/tables-check/sealc"
	"example.com/tables-check/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	runtimetenant "github.com/jsell-rh/stego/registry/runtime/tenant"
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
		if err == nil || !strings.Contains(err.Error(), "vaults_code_sealed_complete") {
			t.Fatalf("the contract over a vault that is not sealed ends with %v, want the failed VALIDATE of the completeness CHECK of code", err)
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
