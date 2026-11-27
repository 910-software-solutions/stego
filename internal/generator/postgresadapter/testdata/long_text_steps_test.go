package check_test

// The long text fields of notesfresh, a new service, and the upgrade of notesbase, as postgres-adapter 6.32.0 built
// it, to notesup, whose relax steps drop the search indexes of the fields out of the search (erp-demo G10, decision
// ST-DECL-Q50, ST-SUP-RC01-OC53). The host test TestTables of internal/generator/postgresadapter/tables_test.go runs
// it in the generated module.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"example.com/tables-check/notesbase"
	"example.com/tables-check/notesfresh"
	"example.com/tables-check/notesup"
	"github.com/google/uuid"
)

// longText gives n random characters: letters and digits, or, when wide, characters of four bytes each in UTF-8
// (U+1F600 to U+1F64F). Random text does not compress, so the index entry of a value has its full size.
func longText(n int, wide bool) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	var b strings.Builder
	for _, c := range raw {
		if wide {
			b.WriteRune(rune(0x1F600 + int(c)%0x50))
		} else {
			b.WriteByte(letters[int(c)%len(letters)])
		}
	}
	return b.String()
}

// noteIndexes gives the names of the search indexes of the table notes, in order.
func noteIndexes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT indexname FROM pg_catalog.pg_indexes WHERE schemaname = 'public' AND tablename = 'notes' AND indexname LIKE '%\_search' ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// writeLong writes a note of tenant-a with a body of 4000 random letters and a summary of 1000 random characters of
// four bytes, 4000 bytes each, and gives the SQLSTATE of the write, or "".
func writeLong(t *testing.T, db *sql.DB) string {
	t.Helper()
	return write(t, db, "tenant-a", `INSERT INTO public."notes" ("id", "title", "body", "summary", "tag") VALUES ($1, 'long', $2, $3, 'x')`,
		uuid.Must(uuid.NewV7()).String(), longText(4000, false), longText(1000, true))
}

func TestLongTextSteps(t *testing.T) {
	cfg, admin := server(t)
	ctx := context.Background()

	t.Run("ST-SUP-RC01-OC53-T03 A new service stores long text, and the upgrade from postgres-adapter 6.32.0 drops the search indexes of the fields out of the search and keeps each row", func(t *testing.T) {
		// A new service: no search index of body, summary or tag, and a long body and summary are stored.
		freshCfg, fresh := database(t, cfg, admin)
		migrate(t, freshCfg, fresh, func(ctx context.Context, conn *sql.Conn) error { return notesfresh.Migrate(ctx, conn) }, "notesfresh_access.json")
		if err := notesfresh.VerifySchema(ctx, fresh); err != nil {
			t.Fatalf("the new service refuses its database: %v", err)
		}
		if got := noteIndexes(t, fresh); !slices.Equal(got, []string{"notes_title_search"}) {
			t.Errorf("the new service has the search indexes %v, want only notes_title_search", got)
		}
		if code := writeLong(t, fresh); code != "" {
			t.Errorf("the new service does not store a body of 4000 letters and a summary of 1000 four-byte characters: SQLSTATE %s", code)
		}

		// The release before, as postgres-adapter 6.32.0 built it, with 500 notes of tenant-a: each string field has
		// its search index, and a long body fails its index entry (54000, program_limit_exceeded).
		own, db := database(t, cfg, admin)
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return notesbase.Migrate(ctx, conn) }, "notesbase_access.json")
		if _, err := db.Exec(`INSERT INTO public."notes" ("tenant_id", "id", "title", "body", "summary", "tag")
  SELECT 'tenant-a', gen_random_uuid(), 'note ' || g, 'body ' || g, 'summary ' || g, 'tag' FROM generate_series(1, 500) AS g`); err != nil {
			t.Fatal(err)
		}
		if got, want := noteIndexes(t, db), []string{"notes_body_search", "notes_summary_search", "notes_tag_search", "notes_title_search"}; !slices.Equal(got, want) {
			t.Errorf("the release before has the search indexes %v, want %v", got, want)
		}
		if code := writeLong(t, db); code != "54000" {
			t.Errorf("the release before gives %q for a body of 4000 letters, want 54000 (index row size exceeds the b-tree maximum)", code)
		}

		// The migration job of the release after: its steps drop the three indexes, the job and the schema check
		// accept the database, the rows stay, and the long note is stored.
		migrate(t, own, db, func(ctx context.Context, conn *sql.Conn) error { return notesup.Migrate(ctx, conn) }, "notesup_access.json")
		if err := notesup.VerifySchema(ctx, db); err != nil {
			t.Errorf("the release after refuses the database after its job: %v", err)
		}
		if got := noteIndexes(t, db); !slices.Equal(got, []string{"notes_title_search"}) {
			t.Errorf("after the upgrade the search indexes are %v, want only notes_title_search", got)
		}
		var rows, bodies int
		if err := db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE "body" = 'body ' || substr("title", 6)) FROM public."notes"`).Scan(&rows, &bodies); err != nil || rows != 500 || bodies != 500 {
			t.Errorf("after the upgrade the table holds %d notes, %d with their body (error %v), want 500 and 500", rows, bodies, err)
		}
		if code := writeLong(t, db); code != "" {
			t.Errorf("after the upgrade a body of 4000 letters and a summary of 1000 four-byte characters give SQLSTATE %s, want none", code)
		}
		// A new database of the release after has the same schema as the upgraded one (ST-GEN-INV35).
		nextCfg, next := database(t, cfg, admin)
		migrate(t, nextCfg, next, func(ctx context.Context, conn *sql.Conn) error { return notesup.Migrate(ctx, conn) }, "notesup_access.json")
		if a, b := strings.Join(catalog(t, next), "\n"), strings.Join(catalog(t, db), "\n"); a != b {
			t.Errorf("the upgraded database and a new one differ:\n%s\n----\n%s", b, a)
		}
	})
}
