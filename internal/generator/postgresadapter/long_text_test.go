package postgresadapter_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/types"
)

// noteEntities gives the owned entity Note of the long text fixtures (erp-demo G10): title of at most 200
// characters, the optional body of at most 4000, the optional summary of at most 1000 and the optional tag of at most
// 40. body and summary can pass the size of a b-tree entry in UTF-8 (4 bytes a character).
func noteEntities() []types.Entity {
	n := func(v int) *int { return &v }
	return []types.Entity{{Name: "Note", Fields: []types.Field{
		{Name: "title", Type: types.FieldTypeString, MaxLength: n(200)},
		{Name: "body", Type: types.FieldTypeString, Optional: true, MaxLength: n(4000)},
		{Name: "summary", Type: types.FieldTypeString, Optional: true, MaxLength: n(1000)},
		{Name: "tag", Type: types.FieldTypeString, Optional: true, MaxLength: n(40)},
	}}}
}

// noteCollections lists the notes, so each searched field of Note has its search index.
func noteCollections() []types.Collection {
	return []types.Collection{{Name: "notes", Entity: "Note", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}}
}

// notesBefore is the release of the notes as postgres-adapter 6.32.0 generated it: each public string field of the
// listed Note has its search index, also body and summary. Since 6.34.0 the same output comes from the fields marked
// searchable, which is how the check against such a base reads it (ST-GEN-INV40); ST-SUP-RC01-OC53-T02 holds the
// output to the golden file that 6.32.0 wrote.
func notesBefore(ns, tenancyNS string) gen.Context {
	ctx := runtimeService(ns, tenancyNS, noteEntities(), noteCollections())
	yes := true
	for i := range ctx.Entities[0].Fields {
		ctx.Entities[0].Fields[i].Searchable = &yes
	}
	return ctx
}

// notesFresh is a new service of the notes from postgres-adapter 6.34.0: body and summary are long text, so they are
// out of the search, and tag sets searchable: false.
func notesFresh(ns, tenancyNS string) gen.Context {
	ctx := runtimeService(ns, tenancyNS, noteEntities(), noteCollections())
	no := false
	ctx.Entities[0].Fields[3].Searchable = &no
	return ctx
}

// notesUpgrade is notesFresh as the release after notesBefore: a relax with the item searchable for each of body,
// summary and tag at expand, whose steps keep their search indexes (decision ST-DECL-Q50).
func notesUpgrade(ns, tenancyNS string) gen.Context {
	ctx := notesFresh(ns, tenancyNS)
	for _, field := range []string{"body", "summary", "tag"} {
		id := "note-" + field + "-relax"
		ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: id, Kind: "relax", Entity: "Note", Field: field, Definition: "searchable"})
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: id, Phase: "expand"})
	}
	return ctx
}

// notesContract is notesUpgrade as the release after it: the contract step of each relax, which drops its search
// index (security review of PR #22, M2).
func notesContract(ns, tenancyNS string) gen.Context {
	ctx := notesUpgrade(ns, tenancyNS)
	for _, field := range []string{"body", "summary", "tag"} {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "note-" + field + "-relax", Phase: "contract"})
	}
	return ctx
}

// generatedFiles generates ctx and gives the content of each file by its path.
func generatedFiles(t *testing.T, ctx gen.Context) map[string]string {
	t.Helper()
	files, _, err := new(postgresadapter.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.Path] = string(f.Content)
	}
	return out
}

// migrationBodies generates ctx and gives the SQL body of each migration of its namespace by its name.
func migrationBodies(t *testing.T, ctx gen.Context) map[string]string {
	t.Helper()
	files, _, err := new(postgresadapter.Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for _, f := range files {
		if strings.HasPrefix(f.Path, filepath.Join(ctx.OutputNamespace, "migrations")+string(os.PathSeparator)) {
			bodies[strings.TrimSuffix(filepath.Base(f.Path), ".sql")] = string(f.Content)
		}
	}
	return bodies
}

func TestLongText(t *testing.T) {
	t.Run("ST-SUP-RC01-OC53-T01 001_initial gives no search index to a string field out of the search, and one to each other searched field", func(t *testing.T) {
		initial := migrationBodies(t, notesFresh("notes", "notestenancy"))["001_initial"]
		if !strings.Contains(initial, `CREATE INDEX "notes_title_search" ON public."notes" ("tenant_id", "title");`) {
			t.Errorf("001_initial has no search index of title, of at most 200 characters:\n%s", initial)
		}
		for _, field := range []string{"body", "summary", "tag"} {
			if strings.Contains(initial, `"notes_`+field+`_search"`) {
				t.Errorf("001_initial gives a search index to %s, which is out of the search:\n%s", field, initial)
			}
		}
		// The bound is 500 characters (2000 bytes in UTF-8): 500 keeps its index and 501 has none. A string with
		// no max_length and an enum keep theirs, and searchable: true at the bound keeps its index.
		n := func(v int) *int { return &v }
		yes := true
		ctx := runtimeService("bounds", "boundstenancy", []types.Entity{{Name: "Item", Fields: []types.Field{
			{Name: "at_bound", Type: types.FieldTypeString, MaxLength: n(500)},
			{Name: "over_bound", Type: types.FieldTypeString, MaxLength: n(501)},
			{Name: "free", Type: types.FieldTypeString},
			{Name: "kind", Type: types.FieldTypeEnum, Values: []string{"a", "b"}},
			{Name: "marked", Type: types.FieldTypeString, MaxLength: n(500), Searchable: &yes},
		}}}, []types.Collection{{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpCreate, types.OpList}}})
		initial = migrationBodies(t, ctx)["001_initial"]
		var indexed []string
		for _, line := range strings.Split(initial, "\n") {
			if name, ok := strings.CutPrefix(line, `CREATE INDEX "items_`); ok {
				indexed = append(indexed, strings.SplitN(name, `_search"`, 2)[0])
			}
		}
		if want := []string{"at_bound", "free", "kind", "marked"}; !slices.Equal(indexed, want) {
			t.Errorf("001_initial gives the search indexes of %v, want %v:\n%s", indexed, want, initial)
		}
	})

	t.Run("ST-SUP-RC01-OC53-T02 The upgrade from postgres-adapter 6.32.0 keeps 001_initial and drops each search index of a field out of the search in a relax step", func(t *testing.T) {
		golden, err := os.ReadFile(filepath.Join("testdata", "golden", "notes_6.32.0_001_initial.sql"))
		if err != nil {
			t.Fatal(err)
		}
		before := migrationBodies(t, notesBefore("notesbase", "notesbasetenancy"))
		if before["001_initial"] != string(golden) {
			t.Errorf("001_initial of the notes marked searchable is not the one of postgres-adapter 6.32.0:\n%s", before["001_initial"])
		}
		bodies := migrationBodies(t, notesUpgrade("notesup", "notesuptenancy"))
		// The digest of 001_initial is in the ledger of each database of the release before: its body stays.
		if strings.ReplaceAll(bodies["001_initial"], "notesup", "notesbase") != string(golden) {
			t.Errorf("the upgrade changes 001_initial of the release before:\n%s", bodies["001_initial"])
		}
		// The expand step of each relax keeps the search index, so the release before can restart and a rollback to
		// it is possible; its migration makes the table of the release reports for the contract step.
		var steps []string
		for name := range bodies {
			if strings.HasPrefix(name, "step_") {
				steps = append(steps, name)
			}
		}
		slices.Sort(steps)
		expands := []string{"step_0001_note-body-relax_expand", "step_0002_note-summary-relax_expand", "step_0003_note-tag-relax_expand"}
		if !slices.Equal(steps, expands) {
			t.Errorf("the upgrade has the steps %v, want %v", steps, expands)
		}
		for _, name := range expands {
			if strings.Contains(bodies[name], "DROP INDEX") || !strings.HasPrefix(bodies[name], "CREATE TABLE IF NOT EXISTS stego_schema.releases (") {
				t.Errorf("%s is %q, want the table of the release reports and no DROP INDEX", name, bodies[name])
			}
		}
		// The release after it drops each index at the contract step of its relax, while the index is there, after
		// the check of the running releases.
		contract := generatedFiles(t, notesContract("notesupc", "notesupctenancy"))
		migrations := filepath.Join("notesupc", "migrations")
		for i, field := range []string{"body", "summary", "tag"} {
			name := fmt.Sprintf("step_%04d_note-%s-relax_contract", i+4, field)
			body, want := contract[filepath.Join(migrations, name+".sql")], "DROP INDEX IF EXISTS public.\"notes_"+field+"_search\";\n"
			if body != want {
				t.Errorf("%s is %q, want %q", name, body, want)
			}
			index := fmt.Sprintf(`"DROP INDEX IF EXISTS public.\"notes_%s_search\"", When: "SELECT pg_catalog.to_regclass('public.\"notes_%s_search\"') IS NOT NULL", Long: false, Guard: true},`, field, field)
			if register := contract[filepath.Join("notesupc", "migrate.go")]; !strings.Contains(register, index) || !strings.Contains(register, `}, Drop: true, Index: "notes_`+field+`_search"})`) {
				t.Errorf("migrate.go does not register %s as a contract step of a drop of the index notes_%s_search with the check of the running releases", name, field)
			}
		}
		// A relax with the item searchable and a longer max_length changes the column at expand and keeps the index.
		n := func(v int) *int { return &v }
		ctx := runtimeService("notesln", "noteslntenancy", noteEntities(), noteCollections())
		ctx.Entities[0].Fields[3].MaxLength = n(4000)
		ctx.SchemaChanges = []types.SchemaChange{{ID: "note-tag-relax", Kind: "relax", Entity: "Note", Field: "tag", Definition: "max_length 40, searchable"}}
		ctx.SchemaHistory = []types.SchemaStep{{Change: "note-tag-relax", Phase: "expand"}}
		bodies = migrationBodies(t, ctx)
		step := bodies["step_0001_note-tag-relax_expand"]
		if !strings.HasPrefix(step, "ALTER TABLE public.\"notes\" ALTER COLUMN \"tag\" TYPE character varying(4000);\n") || strings.Contains(step, "DROP INDEX") ||
			!strings.Contains(step, "CREATE TABLE IF NOT EXISTS stego_schema.releases (") {
			t.Errorf("the relax of the tag to 4000 characters is %q, want its ALTER TABLE and the table of the release reports, and no DROP INDEX", step)
		}
		if !strings.Contains(bodies["001_initial"], `"notes_tag_search"`) {
			t.Errorf("001_initial of the relax to 4000 characters gives tag no search index:\n%s", bodies["001_initial"])
		}
		// A contract step of a relax with no item searchable is refused.
		ctx = runtimeService("notesrc", "notesrctenancy", noteEntities(), noteCollections())
		ctx.Entities[0].Fields[0].MaxLength = n(300)
		ctx.SchemaChanges = []types.SchemaChange{{ID: "note-title-relax", Kind: "relax", Entity: "Note", Field: "title", Definition: "max_length 200"}}
		ctx.SchemaHistory = []types.SchemaStep{{Change: "note-title-relax", Phase: "expand"}, {Change: "note-title-relax", Phase: "contract"}}
		if _, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "is not built") {
			t.Errorf("a contract step of a relax with no item searchable gives %v, want the refusal that it is not built", err)
		}
		ctx = runtimeService("notesin", "notesintenancy", noteEntities(), noteCollections())
		ctx.SchemaChanges = []types.SchemaChange{{ID: "note-title-relax", Kind: "relax", Entity: "Note", Field: "title", Definition: "searchable"}}
		ctx.SchemaHistory = []types.SchemaStep{{Change: "note-title-relax", Phase: "expand"}}
		if _, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil {
			t.Error("a relax with the item searchable of title, which is in the search, is generated")
		}
	})
}
