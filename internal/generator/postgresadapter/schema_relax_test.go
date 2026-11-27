package postgresadapter_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/types"
)

// relaxBase is the release before four relax steps: the owned entity Board with the title of 1 to 40 characters,
// the enum mood of calm and busy, the required size of at most 8 characters, and the optional enum kind of a and b.
func relaxBase(ns, tenancyNS string) gen.Context {
	n := func(v int) *int { return &v }
	entities := []types.Entity{{Name: "Board", Fields: []types.Field{
		{Name: "title", Type: types.FieldTypeString, MinLength: n(1), MaxLength: n(40)},
		{Name: "mood", Type: types.FieldTypeEnum, Values: []string{"calm", "busy"}},
		{Name: "size", Type: types.FieldTypeString, MaxLength: n(8)},
		{Name: "kind", Type: types.FieldTypeEnum, Optional: true, Values: []string{"a", "b"}},
	}}}
	return runtimeService(ns, tenancyNS, entities, boardCollections(false))
}

// relaxHead is relaxBase with one relax step of each kind (decision on finding F12, owner, 2026-11-13): title of at
// most 80 characters, the new mood sleepy, size optional with the default m, and kind a string.
func relaxHead(ns, tenancyNS string) gen.Context {
	n := func(v int) *int { return &v }
	ctx := relaxBase(ns, tenancyNS)
	fields := ctx.Entities[0].Fields
	fields[0].MaxLength = n(80)
	fields[1].Values = []string{"calm", "busy", "sleepy"}
	fields[2].Optional, fields[2].Default = true, "m"
	fields[3].Type, fields[3].Values = types.FieldTypeString, nil
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "board-title-relax", Kind: "relax", Entity: "Board", Field: "title", Definition: "max_length 40"},
		{ID: "board-mood-relax", Kind: "relax", Entity: "Board", Field: "mood", Definition: "values calm busy"},
		{ID: "board-size-relax", Kind: "relax", Entity: "Board", Field: "size", Definition: "required, no default"},
		{ID: "board-kind-relax", Kind: "relax", Entity: "Board", Field: "kind", Definition: "type enum, values a b"},
	}
	for _, change := range ctx.SchemaChanges {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "expand"})
	}
	return ctx
}

func TestRelaxSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC45-T01 Each relax kind is migrations that change the column in place, each with one short ALTER TABLE", func(t *testing.T) {
		files, _, err := new(postgresadapter.Generator).Generate(relaxHead("boardsrx", "boardsrxtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		bodies := map[string]string{}
		for _, f := range files {
			if strings.HasPrefix(f.Path, filepath.Join("boardsrx", "migrations")+string(os.PathSeparator)) {
				bodies[strings.TrimSuffix(filepath.Base(f.Path), ".sql")] = string(f.Content)
			}
		}
		const table = `public."boards"`
		comment := func(column, step string, lines ...string) string {
			text := strings.Join(append([]string{"stego relax " + step}, lines...), "\n")
			return "COMMENT ON COLUMN " + table + `."` + column + `" IS '` + strings.ReplaceAll(text, "'", "''") + "';\n"
		}
		// The ALTER TABLE comes first, so the comment takes no lock beyond the one it holds, and the comment states
		// the exact form of each attribute that the step relaxes (security review of PR #334, M1 and the lock order).
		want := map[string]string{
			"step_0001_board-title-relax_expand": "ALTER TABLE " + table + ` ADD CONSTRAINT "boards_title_relax" CHECK (char_length("title") >= 1) NOT VALID, DROP CONSTRAINT "boards_title_check", ALTER COLUMN "title" TYPE character varying(80);` + "\n" +
				comment("title", "step_0001_board-title-relax_expand", "type character varying(80)"),
			"step_0001_board-title-relax_expand_validate":        "ALTER TABLE " + table + ` VALIDATE CONSTRAINT "boards_title_relax";` + "\n",
			"step_0001_board-title-relax_expand_validate_finish": "ALTER TABLE " + table + ` RENAME CONSTRAINT "boards_title_relax" TO "boards_title_check";` + "\n",
			"step_0002_board-mood-relax_expand": "ALTER TABLE " + table + ` ADD CONSTRAINT "boards_mood_relax" CHECK ("mood" IN ('calm', 'busy', 'sleepy')) NOT VALID;` + "\n" +
				comment("mood", "step_0002_board-mood-relax_expand", "check CHECK ((mood = ANY (ARRAY['calm'::text, 'busy'::text, 'sleepy'::text])))"),
			"step_0002_board-mood-relax_expand_validate":        "ALTER TABLE " + table + ` VALIDATE CONSTRAINT "boards_mood_relax";` + "\n",
			"step_0002_board-mood-relax_expand_validate_finish": "ALTER TABLE " + table + ` DROP CONSTRAINT "boards_mood_check";` + "\nALTER TABLE " + table + ` RENAME CONSTRAINT "boards_mood_relax" TO "boards_mood_check";` + "\n",
			"step_0003_board-size-relax_expand": "ALTER TABLE " + table + ` ALTER COLUMN "size" DROP NOT NULL, ALTER COLUMN "size" SET DEFAULT 'm';` + "\n" +
				comment("size", "step_0003_board-size-relax_expand", "nullable", "default 'm'::character varying(8)"),
			"step_0004_board-kind-relax_expand": "ALTER TABLE " + table + ` DROP CONSTRAINT "boards_kind_check";` + "\n" +
				comment("kind", "step_0004_board-kind-relax_expand", "check none"),
		}
		for name, body := range want {
			if got, ok := bodies[name]; !ok {
				t.Errorf("no migration %s; the migrations are %v", name, keys(bodies))
			} else if got != body {
				t.Errorf("%s is\n%s\nwant\n%s", name, got, body)
			}
		}
		for name := range bodies {
			if strings.HasPrefix(name, "step_") && want[name] == "" {
				t.Errorf("the migration %s is more than the relax steps give", name)
			}
		}
		// The baseline 001_initial is the release before's, so a database with rows takes the steps.
		baseTables, baseMarker, err := postgresadapter.Baseline(relaxBase("boardsrx", "boardsrxtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		headTables, headMarker, err := postgresadapter.Baseline(relaxHead("boardsrx", "boardsrxtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(baseTables, headTables) || baseMarker != headMarker {
			t.Errorf("the baseline of the relax release is not that of the release before:\n%v %s\n%v %s", headTables, headMarker, baseTables, baseMarker)
		}
		// The marker of the step is the text that the schema check of the runtime library accepts.
		runtimeText, err := os.ReadFile(filepath.Join("..", "..", "..", "registry", "runtime", "manifest", "relax.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(runtimeText), "const RelaxMarker = \""+postgresadapter.RelaxMarker+"\"\n") {
			t.Errorf("the marker %q of the relax step is not the runtime library's manifest.RelaxMarker", postgresadapter.RelaxMarker)
		}
		for _, name := range []string{"step_0001_board-title-relax_expand_validate_finish", "step_0002_board-mood-relax_expand_validate_finish"} {
			if err := postgresadapter.CheckLedgerName(name); err != nil {
				t.Errorf("the ledger refuses the name %s: %v", name, err)
			}
		}
	})
}

// keys gives the names of the migrations.
func keys(bodies map[string]string) []string {
	var names []string
	for name := range bodies {
		names = append(names, name)
	}
	return names
}
