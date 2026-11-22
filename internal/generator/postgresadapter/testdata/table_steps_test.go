package check_test

// The migration runner of the generated migrate.go under load, and the step forms that the generator emits against
// the manifests of the release before and of the release of the step, from the independent verification of PR #270.
// The host test TestTableSteps of internal/generator/postgresadapter/tables_test.go runs it in the generated module.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/tables-check/boards"
	"example.com/tables-check/boardsbase"
	"example.com/tables-check/boardsnote"
	"example.com/tables-check/boardsslug"
	"example.com/tables-check/tenancy"
	"example.com/tables-check/uniquebase"
	"example.com/tables-check/uniquestep"
	"example.com/tables-check/within"
	"example.com/tables-check/withinbase"
	"example.com/tables-check/withinfresh"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
)

func TestTableSteps(t *testing.T) {
	cfg, admin := server(t)
	ctx := context.Background()

	// base makes a database that the Board release before migrated, with two boards of tenant-a, and gives its
	// config, its superuser pool and the session of a migration job as the owner role.
	base := func(t *testing.T) (*pgx.ConnConfig, *sql.DB, *sql.Conn) {
		t.Helper()
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return boardsbase.Migrate(ctx, conn) }, "boardsbase_access.json")
		if _, err := db.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title") VALUES ('tenant-a', gen_random_uuid(), 'a'), ('tenant-a', gen_random_uuid(), 'b')`); err != nil {
			t.Fatal(err)
		}
		job := stdlib.OpenDB(*own)
		t.Cleanup(func() { _ = job.Close() })
		conn, err := job.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.ExecContext(ctx, "SET ROLE "+pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		return own, db, conn
	}
	ledgerHolds := func(t *testing.T, db *sql.DB, version string) bool {
		t.Helper()
		var found bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM stego_schema.migrations WHERE version = $1)`, version).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	indexState := func(t *testing.T, db *sql.DB, name string) (bool, bool) {
		t.Helper()
		var present, valid bool
		if err := db.QueryRow(`SELECT count(*) > 0, coalesce(bool_and(i.indisvalid), false) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
  WHERE c.relname = $1`, name).Scan(&present, &valid); err != nil {
			t.Fatal(err)
		}
		return present, valid
	}
	const slugSearch, slugUnique = "step_0001_add-board-slug_expand_index_01", "step_0001_add-board-slug_expand_index_02"

	t.Run("ST-SUP-RC01-OC22-T08 A step waits behind a long reader, and each reader still gets its rows", func(t *testing.T) {
		_, db, conn := base(t)
		var first string
		if err := db.QueryRow(`SELECT "id"::text FROM public."boards" ORDER BY "title" LIMIT 1`).Scan(&first); err != nil {
			t.Fatal(err)
		}
		// A long reader: one SELECT of boards that runs for 4 s, which holds its ACCESS SHARE lock.
		reader := make(chan error, 1)
		go func() {
			_, err := db.Exec(`SELECT "title", pg_catalog.pg_sleep(4) FROM public."boards" LIMIT 1`)
			reader <- err
		}()
		time.Sleep(200 * time.Millisecond)
		done := make(chan error, 1)
		started := time.Now()
		go func() { done <- boards.Migrate(ctx, conn) }()
		// Readers that come while the step waits get their row in a bounded time.
		var slowest time.Duration
		for i := 0; i < 6; i++ {
			time.Sleep(400 * time.Millisecond)
			at := time.Now()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`SET LOCAL statement_timeout = '2500ms'`); err != nil {
				t.Fatal(err)
			}
			var title string
			err = tx.QueryRow(`SELECT "title" FROM public."boards" WHERE "id" = $1`, first).Scan(&title)
			_ = tx.Rollback()
			if err != nil || title != "a" {
				t.Errorf("read %d while the step waits gives %q, %v", i, title, err)
			}
			slowest = max(slowest, time.Since(at))
		}
		t.Logf("the slowest read while the step waits took %v", slowest)
		if err := <-reader; err != nil {
			t.Fatalf("the long reader: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the migration job: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the migration job did not end")
		}
		if time.Since(started) < 3*time.Second {
			t.Errorf("the job ended after %v, before the long reader ended", time.Since(started))
		}
		var rows int
		if err := db.QueryRow(`SELECT count(*) FROM stego_schema.migrations WHERE version LIKE 'step_%'`).Scan(&rows); err != nil || rows != 6 {
			t.Errorf("the ledger holds %d step rows (error %v), want 6", rows, err)
		}
	})

	t.Run("ST-SUP-RC01-OC38-T06 An index build succeeds while an older transaction is open, and both indexes are valid", func(t *testing.T) {
		_, db, conn := base(t)
		// A transaction of the same database that holds a snapshot for 3 s and then commits by itself.
		holder := make(chan error, 1)
		go func() {
			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
			if err != nil {
				holder <- err
				return
			}
			if _, err := tx.Exec(`SELECT count(*) FROM pg_catalog.pg_class`); err != nil {
				holder <- err
				return
			}
			time.Sleep(3 * time.Second)
			holder <- tx.Commit()
		}()
		time.Sleep(200 * time.Millisecond)
		started := time.Now()
		err := boardsslug.Migrate(ctx, conn)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("the migration job with a 3 s transaction open: %v", err)
		}
		if err := <-holder; err != nil {
			t.Fatal(err)
		}
		t.Logf("the job took %v", elapsed)
		if elapsed < 2500*time.Millisecond {
			t.Errorf("the job took %v; the build did not wait for the older transaction", elapsed)
		}
		for _, name := range []string{"boards_slug_search", "boards_slug_key"} {
			if present, valid := indexState(t, db, name); !present || !valid {
				t.Errorf("%s: present %v valid %v", name, present, valid)
			}
		}
		if !ledgerHolds(t, db, slugSearch) || !ledgerHolds(t, db, slugUnique) {
			t.Error("the ledger does not hold both builds")
		}
	})

	t.Run("ST-SUP-RC01-OC38-T07 A unique build that a concurrent duplicate fails leaves no index and no ledger row, and a rerun builds it", func(t *testing.T) {
		_, db, conn := base(t)
		// A writer of the release of the step writes two boards of one tenant with one slug while the build waits for
		// an open write transaction on boards; no event trigger is used.
		done := make(chan error, 1)
		go func() { done <- boardsslug.Migrate(ctx, conn) }()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var column bool
			if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'public.boards'::pg_catalog.regclass AND attname = 'slug')`).Scan(&column); err != nil {
				t.Fatal(err)
			}
			if column {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the step never adds slug")
			}
			time.Sleep(2 * time.Millisecond)
		}
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if _, err := holder.ExecContext(ctx, `BEGIN; LOCK TABLE public."boards" IN ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		var uniqueReady bool
		_ = db.QueryRow(`SELECT coalesce(bool_or(i.indisready), false) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'boards_slug_key'`).Scan(&uniqueReady)
		if uniqueReady {
			t.Skip("inconclusive: the unique build was past its first phase before the holder took its lock")
		}
		_, dupErr := db.Exec(`INSERT INTO public."boards" ("tenant_id", "id", "title", "slug") VALUES ('tenant-a', gen_random_uuid(), 'c', 'same'), ('tenant-a', gen_random_uuid(), 'd', 'same')`)
		if dupErr != nil {
			t.Fatalf("the duplicate write: %v", dupErr)
		}
		if _, err := holder.ExecContext(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("the migration job did not end")
		}
		if err == nil || !strings.Contains(err.Error(), slugUnique) {
			t.Fatalf("the job with a duplicate gives %v, want the failure of %s", err, slugUnique)
		}
		t.Logf("the job fails with: %v", err)
		if present, _ := indexState(t, db, "boards_slug_key"); present {
			t.Error("the failed build left its index")
		}
		var invalid int
		if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_index WHERE NOT indisvalid`).Scan(&invalid); err != nil || invalid != 0 {
			t.Errorf("the database holds %d invalid indexes (%v)", invalid, err)
		}
		if ledgerHolds(t, db, slugUnique) || !ledgerHolds(t, db, slugSearch) {
			t.Errorf("the ledger holds the unique build %v and the search build %v, want false and true", ledgerHolds(t, db, slugUnique), ledgerHolds(t, db, slugSearch))
		}
		// The release before still accepts the database, and a second job after the duplicate is gone builds it.
		if _, err := db.Exec(`UPDATE public."boards" SET "slug" = NULL WHERE "title" = 'd'`); err != nil {
			t.Fatal(err)
		}
		if err := boardsslug.Migrate(ctx, conn); err != nil {
			t.Fatalf("the second job: %v", err)
		}
		if present, valid := indexState(t, db, "boards_slug_key"); !present || !valid || !ledgerHolds(t, db, slugUnique) {
			t.Errorf("after the second job the index is present %v valid %v and in the ledger %v", present, valid, ledgerHolds(t, db, slugUnique))
		}
	})

	t.Run("ST-SUP-RC01-OC38-T08 Each relation that takes the name of a step index fails the step with no ledger row", func(t *testing.T) {
		for _, c := range []struct{ name, setup, failed string }{
			{"a sequence of the name of the unique index", `CREATE SEQUENCE public."boards_slug_key"`, slugUnique},
			{"a view of the name of the search index", `CREATE VIEW public."boards_slug_search" AS SELECT 1 AS x`, slugSearch},
			{"a unique index of the name on another table", `CREATE TABLE public.other ("tenant_id" text, "slug" text); CREATE UNIQUE INDEX "boards_slug_key" ON public.other ("tenant_id", "slug")`, slugUnique},
			{"a plain index of the name and the columns", `ALTER TABLE public."boards" ADD COLUMN IF NOT EXISTS "x" text; CREATE INDEX "boards_slug_key" ON public."boards" ("tenant_id", "x")`, slugUnique},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, db, conn := base(t)
				if _, err := db.Exec(c.setup); err != nil {
					t.Fatal(err)
				}
				err := boardsslug.Migrate(ctx, conn)
				if err == nil || !strings.Contains(err.Error(), c.failed) {
					t.Fatalf("the job gives %v, want the failure of %s", err, c.failed)
				}
				t.Logf("%s: %v", c.name, err)
				if ledgerHolds(t, db, c.failed) {
					t.Errorf("the ledger holds %s", c.failed)
				}
			})
		}
	})

	t.Run("ST-SUP-RC01-OC22-T07 A retry after a lock timeout commits each statement once and applies no step twice", func(t *testing.T) {
		_, db, conn := base(t)
		// Each DDL command that commits writes one row with its transaction; each one that starts takes a number,
		// which a rollback does not give back.
		if _, err := db.Exec(`CREATE TABLE public.ddl_log (xid xid8, tag text, obj text);
CREATE SEQUENCE public.ddl_starts;
CREATE FUNCTION public.ddl_end() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN INSERT INTO public.ddl_log SELECT pg_catalog.pg_current_xact_id(), command_tag, object_identity FROM pg_catalog.pg_event_trigger_ddl_commands(); END $$;
CREATE FUNCTION public.ddl_start() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN PERFORM pg_catalog.nextval('public.ddl_starts'); END $$;
CREATE EVENT TRIGGER ddl_end ON ddl_command_end EXECUTE FUNCTION public.ddl_end();
CREATE EVENT TRIGGER ddl_start ON ddl_command_start EXECUTE FUNCTION public.ddl_start();`); err != nil {
			t.Fatal(err)
		}
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		if _, err := holder.ExecContext(ctx, `BEGIN; LOCK TABLE public."boards" IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- boards.Migrate(ctx, conn) }()
		time.Sleep(3500 * time.Millisecond)
		if _, err := holder.ExecContext(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the migration job: %v", err)
			}
		case <-time.After(90 * time.Second):
			t.Fatal("the migration job did not end")
		}
		var starts, committed int64
		if err := db.QueryRow(`SELECT last_value FROM public.ddl_starts`).Scan(&starts); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT count(DISTINCT xid) FROM public.ddl_log`).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		t.Logf("DDL commands started %d; transactions that committed DDL %d", starts, committed)
		// Each transaction that committed DDL on public is the transaction of exactly one ledger row, and each step
		// ledger row has its own transaction: no DDL committed outside its ledger row, and none twice.
		var stray, shared int
		// A CREATE INDEX CONCURRENTLY of an index step commits outside the ledger transaction; it must commit once.
		if err := db.QueryRow(`SELECT count(DISTINCT d.xid) FROM public.ddl_log d WHERE coalesce(d.obj, '') NOT LIKE 'stego_schema.%' AND d.tag <> 'CREATE INDEX'
  AND NOT EXISTS (SELECT 1 FROM stego_schema.migrations m WHERE m.xmin::text::bigint = (d.xid::text::bigint % 4294967296))`).Scan(&stray); err != nil {
			t.Fatal(err)
		}
		var twice int
		if err := db.QueryRow(`SELECT count(*) FROM (SELECT obj FROM public.ddl_log WHERE tag = 'CREATE INDEX' GROUP BY obj HAVING count(DISTINCT xid) > 1) x`).Scan(&twice); err != nil || twice != 0 {
			t.Errorf("%d indexes were built in more than one transaction (%v)", twice, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM (SELECT m.xmin FROM stego_schema.migrations m WHERE m.version LIKE 'step_%' GROUP BY m.xmin HAVING count(*) > 1) x`).Scan(&shared); err != nil {
			t.Fatal(err)
		}
		if stray != 0 || shared != 0 {
			t.Errorf("%d DDL transactions committed with no ledger row, and %d transactions hold more than one ledger row", stray, shared)
		}
		if strayRows, err := db.Query(`SELECT d.xid::text, d.tag, coalesce(d.obj, '') FROM public.ddl_log d WHERE NOT EXISTS (SELECT 1 FROM stego_schema.migrations m WHERE m.xmin::text::bigint = (d.xid::text::bigint % 4294967296)) ORDER BY 1`); err == nil {
			for strayRows.Next() {
				var x, tag, obj string
				_ = strayRows.Scan(&x, &tag, &obj)
				t.Logf("no ledger row: xid %s %s %s", x, tag, obj)
			}
			strayRows.Close()
		}
		if sharedRows, err := db.Query(`SELECT string_agg(version, ', ') FROM stego_schema.migrations GROUP BY xmin HAVING count(*) > 1`); err == nil {
			for sharedRows.Next() {
				var v string
				_ = sharedRows.Scan(&v)
				t.Logf("one transaction: %s", v)
			}
			sharedRows.Close()
		}
		var logRows *sql.Rows
		logRows, err = db.Query(`SELECT m.version, d.tag, coalesce(d.obj, '') FROM public.ddl_log d JOIN stego_schema.migrations m ON m.xmin::text::bigint = (d.xid::text::bigint % 4294967296) ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		for logRows.Next() {
			var v, tag, obj string
			_ = logRows.Scan(&v, &tag, &obj)
			t.Logf("ledger %s: %s %s", v, tag, obj)
		}
		logRows.Close()
		var versions, distinct int
		if err := db.QueryRow(`SELECT count(*), count(DISTINCT version) FROM stego_schema.migrations`).Scan(&versions, &distinct); err != nil || versions != distinct {
			t.Errorf("the ledger holds %d rows of %d versions (%v)", versions, distinct, err)
		}
		var ledger int
		if err := db.QueryRow(`SELECT count(*) FROM stego_schema.migrations WHERE version LIKE 'step_%'`).Scan(&ledger); err != nil || ledger != 6 {
			t.Errorf("the ledger holds %d step rows (%v), want 6", ledger, err)
		}
		if starts <= committed {
			t.Errorf("no attempt was retried: %d starts, %d commits", starts, committed)
		}
		// A second job applies nothing.
		publicDDL := func() (n int64) {
			if err := db.QueryRow(`SELECT count(DISTINCT xid) FROM public.ddl_log WHERE coalesce(obj, '') NOT LIKE 'stego_schema.%'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := publicDDL()
		if err := boards.Migrate(ctx, conn); err != nil {
			t.Fatal(err)
		}
		if committed = publicDDL(); committed != before {
			t.Errorf("a second job committed DDL: %d, was %d", committed, before)
		}
	})

	t.Run("ST-SUP-RP15-LC01-T15 The release before accepts each emitted step form, NOT VALID and validated, and the release of the step refuses NOT VALID", func(t *testing.T) {
		own, db, conn := base(t)
		if err := boardsnote.Migrate(ctx, conn); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'tables-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := own.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "tables-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		var steps []string
		rows, err := db.Query(`SELECT conname, pg_catalog.pg_get_constraintdef(oid), convalidated FROM pg_catalog.pg_constraint WHERE conrelid = 'public.boards'::pg_catalog.regclass AND contype IN ('c', 'f') ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		type con struct {
			name, def string
		}
		var cons []con
		for rows.Next() {
			var c con
			var valid bool
			_ = rows.Scan(&c.name, &c.def, &valid)
			t.Logf("emitted: %s %s validated %v", c.name, c.def, valid)
			cons = append(cons, c)
			steps = append(steps, c.name)
		}
		rows.Close()
		check := func(what string, m manifest.Manifest, refused bool) {
			t.Helper()
			err := manifest.Check(ctx, reader, owned(m))
			switch {
			case refused && err == nil:
				t.Errorf("%s: Check accepts", what)
			case !refused && err != nil:
				t.Errorf("%s: Check refuses: %v", what, err)
			}
			if err != nil {
				t.Logf("%s: %v", what, err)
			}
		}
		check("validated, the release before", boardsbase.SchemaManifest, false)
		check("validated, the release of the step", boardsnote.SchemaManifest, false)
		// The state between the expand step and its VALIDATE: each emitted CHECK and foreign key NOT VALID.
		for _, c := range cons {
			if c.name == "boards_pkey" || !strings.Contains(c.name, "level") && !strings.Contains(c.name, "score") && !strings.Contains(c.name, "mood") &&
				!strings.Contains(c.name, "parent_id") && !strings.Contains(c.name, "note") {
				continue
			}
			if _, err := db.Exec(`ALTER TABLE public."boards" DROP CONSTRAINT ` + pgx.Identifier{c.name}.Sanitize() + `; ALTER TABLE public."boards" ADD CONSTRAINT ` +
				pgx.Identifier{c.name}.Sanitize() + " " + c.def + " NOT VALID"); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if _, err := db.Exec(`ALTER TABLE public."boards" OWNER TO ` + pgx.Identifier{tenancy.OwnerRole}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			check(c.name+" NOT VALID, the release before", boardsbase.SchemaManifest, false)
			check(c.name+" NOT VALID, the release of the step", boardsnote.SchemaManifest, true)
			if _, err := db.Exec(`ALTER TABLE public."boards" VALIDATE CONSTRAINT ` + pgx.Identifier{c.name}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
		_ = uuid.Nil
	})

	// keyed runs one statement as the runtime role of tenant-a and gives the SQLSTATE and the constraint of its error,
	// or two empty strings.
	keyed := func(t *testing.T, db *sql.DB, statement string, args ...any) (string, string) {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, setup := range []string{"SET LOCAL ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize(), "SELECT pg_catalog.set_config('app.tenant', 'tenant-a', true)"} {
			if _, err := tx.Exec(setup); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(statement, args...); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				return pgErr.Code, pgErr.ConstraintName
			}
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return "", ""
	}
	keyState := func(t *testing.T, db *sql.DB, table, name string) (present, validated bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT count(*) > 0, coalesce(bool_and(convalidated), false) FROM pg_catalog.pg_constraint
  WHERE conrelid = ('public.' || pg_catalog.quote_ident($1))::pg_catalog.regclass AND conname = $2`, table, name).Scan(&present, &validated); err != nil {
			t.Fatal(err)
		}
		return present, validated
	}
	const withinKey, withinValidate = "tickets_sprint_id_within_fkey", "step_0001_ticket-sprint-within_expand_validate"
	// withinCheck holds the optional within field project_id set while sprint_id is set, which MATCH SIMPLE does not
	// (security review of PR #413, L1).
	const withinCheck = "tickets_sprint_id_wcheck"
	// unscoped writes a ticket in the sprint s with no project, as a writer outside the collection whose scope field
	// is project_id can, and expects the CHECK to refuse it.
	unscoped := func(t *testing.T, db *sql.DB, s string) {
		t.Helper()
		if code, key := keyed(t, db, `INSERT INTO public."tickets" ("id", "project_id", "sprint_id", "title") VALUES ($1, NULL, $2, 'no project')`, uuid.NewString(), s); code != "23514" || key != withinCheck {
			t.Errorf("a ticket in a sprint with no project gives %q on %q, want 23514 on %s", code, key, withinCheck)
		}
	}
	// tickets checks the rule of the key on a database whose tables hold it: a ticket in a sprint of its own project
	// and a ticket with no sprint are written, a ticket in a sprint of another project is refused by the key, and a
	// move of a sprint to another project while a ticket of the first project refers to it is refused too.
	tickets := func(t *testing.T, db *sql.DB) {
		t.Helper()
		p1, p2, s1, s2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO public."projects" ("id", "name") VALUES ($1, 'core'), ($2, 'web')`, []any{p1, p2}},
			{`INSERT INTO public."sprints" ("id", "project_id", "name") VALUES ($1, $2, 's1'), ($3, $4, 's2')`, []any{s1, p1, s2, p2}},
			{`INSERT INTO public."tickets" ("id", "project_id", "sprint_id", "title") VALUES ($1, $2, $3, 'in its project')`, []any{uuid.NewString(), p1, s1}},
			{`INSERT INTO public."tickets" ("id", "project_id", "sprint_id", "title") VALUES ($1, $2, NULL, 'no sprint')`, []any{uuid.NewString(), p2}},
		} {
			if code, key := keyed(t, db, statement.sql, statement.args...); code != "" {
				t.Fatalf("%s gives %s on %s", statement.sql, code, key)
			}
		}
		if code, key := keyed(t, db, `INSERT INTO public."tickets" ("id", "project_id", "sprint_id", "title") VALUES ($1, $2, $3, 'other project')`, uuid.NewString(), p2, s1); code != "23503" || key != withinKey {
			t.Errorf("a ticket of web in a sprint of core gives %q on %q, want 23503 on %s", code, key, withinKey)
		}
		if code, key := keyed(t, db, `UPDATE public."sprints" SET "project_id" = $1 WHERE "id" = $2`, p2, s1); code != "23503" || key != withinKey {
			t.Errorf("a move of a sprint with a ticket to another project gives %q on %q, want 23503 on %s", code, key, withinKey)
		}
		if code, key := keyed(t, db, `UPDATE public."sprints" SET "project_id" = $1 WHERE "id" = $2`, p1, s2); code != "" {
			t.Errorf("a move of a sprint with no ticket gives %q on %q, want it written", code, key)
		}
		unscoped(t, db, s2)
	}

	t.Run("ST-SUP-RC01-OC46-T01 A first release holds the composite key of a reference held within a field in 001_initial", func(t *testing.T) {
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return withinfresh.Migrate(ctx, conn) }, "withinfresh_access.json")
		if present, validated := keyState(t, db, "tickets", withinKey); !present || !validated {
			t.Fatalf("the key %s is present %v and validated %v, want both", withinKey, present, validated)
		}
		tickets(t, db)
	})

	t.Run("ST-SUP-RC01-OC46-T02 A within step adds the key NOT VALID, validates it in its own migration, and stops on a row that breaks it", func(t *testing.T) {
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return withinbase.Migrate(ctx, conn) }, "withinbase_access.json")
		p1, p2, s1, bad := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO public."projects" ("tenant_id", "id", "name") VALUES ('tenant-a', $1, 'core'), ('tenant-a', $2, 'web')`, []any{p1, p2}},
			{`INSERT INTO public."sprints" ("tenant_id", "id", "project_id", "name") VALUES ('tenant-a', $1, $2, 's1')`, []any{s1, p1}},
			{`INSERT INTO public."tickets" ("tenant_id", "id", "project_id", "sprint_id", "title") VALUES ('tenant-a', gen_random_uuid(), $1, $2, 'good'), ('tenant-a', $3, $4, $2, 'bad')`, []any{p1, s1, bad, p2}},
		} {
			if _, err := db.Exec(statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
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
		// The release before let a ticket of web into a sprint of core: the job stops at the validate migration with a
		// refusal that names the step, the table and the key, and no row value.
		err = within.Migrate(ctx, conn)
		var refused interface{ Refusal() string }
		if !errors.As(err, &refused) {
			t.Fatalf("the job over a ticket outside its project gives %v, want a refusal", err)
		}
		text := refused.Refusal()
		// The failure record names the fault, the key and the remedy, fixed texts with no row value (walkthrough 23,
		// D-f).
		var named interface {
			FaultClass() string
			FaultObject() string
			FaultRemedy() string
		}
		if !errors.As(err, &named) || named.FaultClass() != "schema_change.foreign_key_broken" ||
			(named.FaultObject() != withinKey && named.FaultObject() != "foreign_key") || named.FaultRemedy() == "" {
			t.Errorf("the refusal %v names no fault of the foreign key", err)
		}
		for _, part := range []string{withinValidate, withinKey, "public.tickets"} {
			if !strings.Contains(text, part) {
				t.Errorf("the refusal %q does not name %s", text, part)
			}
		}
		for _, value := range []string{p1, p2, s1, bad, "tenant-a"} {
			if strings.Contains(text, value) || strings.Contains(err.Error(), value) {
				t.Errorf("the refusal %q holds the row value %s", err, value)
			}
		}
		if present, validated := keyState(t, db, "tickets", withinKey); !present || validated {
			t.Errorf("after the refusal the key is present %v and validated %v, want present and NOT VALID", present, validated)
		}
		if ledgerHolds(t, db, withinValidate) {
			t.Errorf("after the refusal the ledger holds %s", withinValidate)
		}
		// The key migration comes after the index migration that builds the unique key it refers to.
		for _, name := range []string{"step_0001_ticket-sprint-within_expand_index_01", "step_0001_ticket-sprint-within_expand_keys"} {
			if !ledgerHolds(t, db, name) {
				t.Errorf("after the refusal the ledger does not hold %s", name)
			}
		}
		if present, valid := indexState(t, db, "sprints_project_id_id_key"); !present || !valid {
			t.Errorf("the unique key of sprints is present %v and valid %v", present, valid)
		}
		// Once the row is corrected, the job validates the key, and the tables hold the rule.
		if _, err := db.Exec(`UPDATE public."tickets" SET "sprint_id" = NULL WHERE "id" = $1`, bad); err != nil {
			t.Fatal(err)
		}
		if err := within.Migrate(ctx, conn); err != nil {
			t.Fatalf("the job after the row is corrected: %v", err)
		}
		if present, validated := keyState(t, db, "tickets", withinKey); !present || !validated {
			t.Errorf("after the job the key is present %v and validated %v", present, validated)
		}
		if !ledgerHolds(t, db, withinValidate) {
			t.Errorf("after the job the ledger does not hold %s", withinValidate)
		}
		if code, key := keyed(t, db, `INSERT INTO public."tickets" ("id", "project_id", "sprint_id", "title") VALUES ($1, $2, $3, 'other project')`, uuid.NewString(), p2, s1); code != "23503" || key != withinKey {
			t.Errorf("after the job a ticket of web in a sprint of core gives %q on %q, want 23503 on %s", code, key, withinKey)
		}
		if present, validated := keyState(t, db, "tickets", withinCheck); !present || !validated {
			t.Errorf("after the job the CHECK %s is present %v and validated %v", withinCheck, present, validated)
		}
		unscoped(t, db, s1)
	})

	t.Run("ST-SUP-RC01-OC38-T10 A unique of a field stops the job on two rows with one value with a refusal that names no value, builds the index once they are gone, and the release before still starts", func(t *testing.T) {
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return uniquebase.Migrate(ctx, conn) }, "uniquebase_access.json")
		dup := uuid.NewString()
		for _, row := range []struct{ tenant, id, name string }{
			{"tenant-a", uuid.NewString(), "alpha"}, {"tenant-a", uuid.NewString(), "beta"}, {"tenant-a", dup, "alpha"}, {"tenant-b", uuid.NewString(), "alpha"},
		} {
			if code := write(t, db, row.tenant, `INSERT INTO public."sites" ("id", "name", "region") VALUES ($1, $2, 'eu')`, row.id, row.name); code != "" {
				t.Fatalf("the release before writes no site: %s", code)
			}
		}
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
		const index, key = "step_0001_site-name-unique_expand_index_01", "sites_name_key"
		// Two sites of tenant-a named alpha: the index migration stops the job with a refusal that names the migration,
		// the index and the table, and no row value; the error keeps SQLSTATE 23505, and no index is left.
		err = uniquestep.Migrate(ctx, conn)
		var refused interface{ Refusal() string }
		if !errors.As(err, &refused) {
			t.Fatalf("the job over two sites with one name gives %v, want a refusal", err)
		}
		text := refused.Refusal()
		// The failure record names the fault, the index and the remedy, fixed texts with no row value (walkthrough 23,
		// D-f).
		var named interface {
			FaultClass() string
			FaultObject() string
			FaultRemedy() string
		}
		if !errors.As(err, &named) || named.FaultClass() != "schema_change.unique_duplicates" || named.FaultObject() != key ||
			named.FaultRemedy() == "" || strings.Contains(named.FaultRemedy(), "alpha") {
			t.Errorf("the refusal %v names no fault of the unique index", err)
		}
		for _, part := range []string{index, key, "public.sites", "two or more rows", "run the migration job again"} {
			if !strings.Contains(text, part) {
				t.Errorf("the refusal %q does not name %s", text, part)
			}
		}
		for _, value := range []string{"alpha", "tenant-a", dup} {
			if strings.Contains(text, value) {
				t.Errorf("the refusal %q holds the row value %s", text, value)
			}
		}
		var coded *pgconn.PgError
		if !errors.As(err, &coded) || coded.Code != "23505" {
			t.Errorf("the refusal %v does not keep SQLSTATE 23505", err)
		}
		if present, _ := indexState(t, db, key); present {
			t.Error("after the refusal the unique index is still there")
		}
		if ledgerHolds(t, db, index) {
			t.Errorf("after the refusal the ledger holds %s", index)
		}
		// Once the second alpha is gone, the job builds the index, and the key holds within one tenant only.
		if _, err := db.Exec(`DELETE FROM public."sites" WHERE "id" = $1`, dup); err != nil {
			t.Fatal(err)
		}
		if err := uniquestep.Migrate(ctx, conn); err != nil {
			t.Fatalf("the job after the duplicate is gone: %v", err)
		}
		if present, valid := indexState(t, db, key); !present || !valid {
			t.Errorf("after the job the unique index is present %v and valid %v", present, valid)
		}
		if !ledgerHolds(t, db, index) {
			t.Errorf("after the job the ledger does not hold %s", index)
		}
		if code, constraint := keyed(t, db, `INSERT INTO public."sites" ("id", "name", "region") VALUES ($1, 'alpha', 'us')`, uuid.NewString()); code != "23505" || constraint != key {
			t.Errorf("after the job a second alpha of tenant-a gives %q on %q, want 23505 on %s", code, constraint, key)
		}
		if code := write(t, db, "tenant-b", `INSERT INTO public."sites" ("id", "name", "region") VALUES ($1, 'beta', 'us')`, uuid.NewString()); code != "" {
			t.Errorf("tenant-b with the name of a site of tenant-a is refused: %s", code)
		}
		// The release before and the release of the step each accept the database, so the pods of the release before
		// restart after the index migration (ST-SUP-RP15-LC01, ST-GEN-Q48).
		if _, err := db.Exec("ALTER ROLE " + pgx.Identifier{tenancy.RuntimeRole}.Sanitize() + " LOGIN PASSWORD 'unique-check'"); err != nil {
			t.Fatal(err)
		}
		runtime := own.Copy()
		runtime.User, runtime.Password = tenancy.RuntimeRole, "unique-check"
		reader := stdlib.OpenDB(*runtime)
		defer reader.Close()
		for name, m := range map[string]manifest.Manifest{"the release before": uniquebase.SchemaManifest, "the release of the step": uniquestep.SchemaManifest} {
			if err := manifest.Check(ctx, reader, owned(m)); err != nil {
				t.Errorf("after the index migration the schema manifest of %s refuses the database: %v", name, err)
			}
		}
	})
}
