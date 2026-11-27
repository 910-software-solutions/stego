package postgresadapter_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/generator/tenancy"
	"github.com/910-software-solutions/stego/internal/types"
)

// boardEntities are the entities of the release before the add steps of ST-SUP-RC01-OC22-T02: the owned Board with a
// title, which a collection lists.
func boardEntities() []types.Entity {
	return []types.Entity{{Name: "Board", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}}
}

// boardCollections list the boards, and with withCard the cards, so that each listed table has search indexes.
func boardCollections(withCard bool) []types.Collection {
	collections := []types.Collection{{Name: "boards", Entity: "Board", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}}
	if withCard {
		collections = append(collections, types.Collection{Name: "cards", Entity: "Card", Operations: []types.Operation{types.OpCreate, types.OpList}})
	}
	return collections
}

// boardSteps is the release with three add steps on boardEntities: the owned entity Card, which refers to a Board
// and has an optional unique code; the optional field Board.note with a maximum length, which the list of boards
// searches; and the field Board.rank with a default.
func boardSteps(ns, tenancyNS string) gen.Context {
	n := func(v int) *int { return &v }
	entities := boardEntities()
	entities[0].Fields = append(entities[0].Fields,
		types.Field{Name: "note", Type: types.FieldTypeString, Optional: true, MinLength: n(1), MaxLength: n(40)},
		types.Field{Name: "rank", Type: types.FieldTypeInt32, Default: 0})
	entities = append(entities, types.Entity{Name: "Card", Fields: []types.Field{
		{Name: "board_id", Type: types.FieldTypeRef, To: "Board"},
		{Name: "code", Type: types.FieldTypeString, Optional: true, Unique: true},
	}})
	ctx := runtimeService(ns, tenancyNS, entities, boardCollections(true))
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "add-card", Kind: "add", Entity: "Card"},
		{ID: "add-board-note", Kind: "add", Entity: "Board", Field: "note"},
		{ID: "add-board-rank", Kind: "add", Entity: "Board", Field: "rank"},
	}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "add-card", Phase: "expand"}, {Change: "add-board-note", Phase: "expand"}, {Change: "add-board-rank", Phase: "expand"}}
	return ctx
}

// boardNoDefault is boardSteps with only its adds that give no default: the table Card and the optional field
// Board.note, and not Board.rank; an add of the optional reference Board.parent_id, whose foreign key the release
// before accepts in its exact form (ST-SUP-RP15-LC03); and adds of the optional fields Board.level with integer bounds,
// Board.score with float bounds and so a finite value, and Board.mood with the values of an enum, whose CHECK
// constraints the release before accepts in their exact forms (ST-SUP-RP15-LC01-T11). Board.note has a minimum
// length, so its add gives a CHECK too.
func boardNoDefault(ns, tenancyNS string) gen.Context {
	f := func(v float64) *float64 { return &v }
	ctx := boardSteps(ns, tenancyNS)
	fields := ctx.Entities[0].Fields
	ctx.Entities[0].Fields = append(fields[:len(fields)-1:len(fields)-1], types.Field{Name: "parent_id", Type: types.FieldTypeRef, To: "Board", Optional: true},
		types.Field{Name: "level", Type: types.FieldTypeInt32, Optional: true, Min: f(1), Max: f(9)},
		types.Field{Name: "score", Type: types.FieldTypeDouble, Optional: true, Min: f(0), Max: f(1.5)},
		types.Field{Name: "mood", Type: types.FieldTypeEnum, Optional: true, Values: []string{"calm", "busy"}})
	ctx.SchemaChanges = append(ctx.SchemaChanges[:2:2], types.SchemaChange{ID: "add-board-parent-id", Kind: "add", Entity: "Board", Field: "parent_id"},
		types.SchemaChange{ID: "add-board-level", Kind: "add", Entity: "Board", Field: "level"},
		types.SchemaChange{ID: "add-board-score", Kind: "add", Entity: "Board", Field: "score"},
		types.SchemaChange{ID: "add-board-mood", Kind: "add", Entity: "Board", Field: "mood"})
	ctx.SchemaHistory = append(ctx.SchemaHistory[:2:2], types.SchemaStep{Change: "add-board-parent-id", Phase: "expand"},
		types.SchemaStep{Change: "add-board-level", Phase: "expand"}, types.SchemaStep{Change: "add-board-score", Phase: "expand"},
		types.SchemaStep{Change: "add-board-mood", Phase: "expand"})
	return ctx
}

// boardSlug is the release after boardBase with one add step: the optional unique field Board.slug, whose unique
// index the step builds CONCURRENTLY on the table boards, which has rows (ST-SUP-RC01-OC38-T02).
func boardSlug(ns, tenancyNS string) gen.Context {
	entities := boardEntities()
	entities[0].Fields = append(entities[0].Fields, types.Field{Name: "slug", Type: types.FieldTypeString, Optional: true, Unique: true})
	ctx := runtimeService(ns, tenancyNS, entities, boardCollections(false))
	ctx.SchemaChanges = []types.SchemaChange{{ID: "add-board-slug", Kind: "add", Entity: "Board", Field: "slug"}}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "add-board-slug", Phase: "expand"}}
	return ctx
}

// boardBase is the release before boardSteps, as the base ref holds it.
func boardBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, boardEntities(), boardCollections(false))
}

// boardRenameBase is the release before a rename: the owned entity Board with the unique field title of 1 to 40
// characters, which the list of boards searches, and the optional field note. title is unique, so the release
// before accepts the sync trigger of its rename only with the unique key of heading (decision on ST-GEN-Q18 (a)).
func boardRenameBase(ns, tenancyNS string) gen.Context {
	n, one := 40, 1
	entities := []types.Entity{{Name: "Board", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString, MinLength: &one, MaxLength: &n, Unique: true},
		{Name: "note", Type: types.FieldTypeString, Optional: true}}}}
	return runtimeService(ns, tenancyNS, entities, boardCollections(false))
}

// boardRename is boardRenameBase with the rename board-title-rename of Board.title to heading at expand, with the
// entities as the compiler gives them (gen.StoredNames): the field keeps its API name title, in the column heading.
func boardRename(ns, tenancyNS string) gen.Context {
	ctx := boardRenameBase(ns, tenancyNS)
	ctx.Entities[0].Fields[0].Name = "heading"
	ctx.SchemaChanges = []types.SchemaChange{{ID: "board-title-rename", Kind: "rename", Entity: "Board", Field: "heading", From: "title"}}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "board-title-rename", Phase: "expand"}}
	entities, err := gen.StoredNames(ctx.Entities, nil, ctx.Collections, ctx.SchemaChanges)
	if err != nil {
		panic(err)
	}
	ctx.Entities = entities
	return ctx
}

// boardRenameContract is boardRename with the contract step of board-title-rename after its expand step: the
// release after the rename, which drops the old column title and the sync trigger (ST-GEN-UC15).
func boardRenameContract(ns, tenancyNS string) gen.Context {
	ctx := boardRename(ns, tenancyNS)
	ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "board-title-rename", Phase: "contract"})
	return ctx
}

// boardRetypeBase is the release before a retype on each of two tables and a require: the owned entity Board with
// the title and the required field revision, a string, and the owned entity Tag with the title and the required field
// level, an int64.
func boardRetypeBase(ns, tenancyNS string) gen.Context {
	entities := []types.Entity{{Name: "Board", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "revision", Type: types.FieldTypeString}}},
		{Name: "Tag", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "level", Type: types.FieldTypeInt64}}}}
	return runtimeService(ns, tenancyNS, entities, boardCollections(false))
}

// boardRetype is boardRetypeBase with three changes at expand: the retype board-revision-retype of Board.revision from
// string to int64 with text_to_int64 and int64_to_text and the fill value 0, whose conversion can fail; the retype
// tag-level-retype of Tag.level from int64 to string with the pattern ^-?[0-9]{1,18}$ and the default 0, with
// int64_to_text and text_to_int64, which is total both ways (a field with a default, walkthrough 14 W14-B3); and
// the require board-locale-require of the new field Board.locale, a string of at most 10, with the fill value en. The entities are as the compiler gives them (gen.StoredNames): each
// retyped field keeps its API name, in the column <field>_<type>.
func boardRetype(ns, tenancyNS string) gen.Context {
	ten := 10
	ctx := boardRetypeBase(ns, tenancyNS)
	ctx.Entities[0].Fields[1].Type = types.FieldTypeInt64
	ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "locale", Type: types.FieldTypeString, MaxLength: &ten})
	ctx.Entities[1].Fields[1] = types.Field{Name: "level", Type: types.FieldTypeString, Pattern: "^-?[0-9]{1,18}$", Default: "0"}
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "board-revision-retype", Kind: "retype", Entity: "Board", Field: "revision", From: "string", Conversion: "text_to_int64", InverseConversion: "int64_to_text", FillValue: 0},
		{ID: "tag-level-retype", Kind: "retype", Entity: "Tag", Field: "level", From: "int64", Conversion: "int64_to_text", InverseConversion: "text_to_int64"},
		{ID: "board-locale-require", Kind: "require", Entity: "Board", Field: "locale", FillValue: "en"},
	}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "board-revision-retype", Phase: "expand"}, {Change: "tag-level-retype", Phase: "expand"},
		{Change: "board-locale-require", Phase: "expand"}}
	entities, err := gen.StoredNames(ctx.Entities, nil, ctx.Collections, ctx.SchemaChanges)
	if err != nil {
		panic(err)
	}
	ctx.Entities = entities
	return ctx
}

// boardRetypeContract is boardRetype with the contract step of each of its three changes after the expand steps.
func boardRetypeContract(ns, tenancyNS string) gen.Context {
	ctx := boardRetype(ns, tenancyNS)
	for _, change := range []string{"board-revision-retype", "tag-level-retype", "board-locale-require"} {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change, Phase: "contract"})
	}
	return ctx
}

// tenancyFiles generates the tenancy component of the service ctx into its tenancy namespace.
func tenancyFiles(t *testing.T, ctx gen.Context) []gen.File {
	t.Helper()
	own := ctx
	own.OutputNamespace = own.PeerNamespaces[gen.TenancyComponent]
	own.ComponentConfig = map[string]any{}
	files, _, err := new(tenancy.Generator).Generate(own)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSchemaSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC22-T02 Each add step gives one migration after the baseline, in order, from the declaration alone", func(t *testing.T) {
		files, _, err := new(postgresadapter.Generator).Generate(boardSteps("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		names := []string{"step_0001_add-card_expand", "step_0002_add-board-note_expand", "step_0003_add-board-rank_expand"}
		migrate := generated(t, files, "boards/migrate.go")
		registered := regexp.MustCompile(`Register(?:SQL|Step|IndexStep|ValidateStep)\("([^"]+)"`).FindAllStringSubmatch(migrate, -1)
		var order []string
		for _, match := range registered {
			order = append(order, match[1])
		}
		// A step on the table boards, which has rows, is its own migration, one migration for each index that it
		// builds CONCURRENTLY, and one that validates its constraints (decision on finding F2).
		all := []string{names[0], names[1], names[1] + "_index_01", names[1] + "_validate", names[2], names[2] + "_index_01"}
		if want := append([]string{"001_initial", "100_tenancy_row_security"}, all...); !slices.Equal(order, want) {
			t.Errorf("migrate.go registers %v, want %v", order, want)
		}
		if !slices.IsSorted(order) {
			t.Errorf("the byte order of the names %v is not the order of the steps", order)
		}

		// 001_initial is the baseline: the body of the release before, so its ledger digest does not change.
		before, _, err := new(postgresadapter.Generator).Generate(boardBase("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := generated(t, files, "boards/migrations/001_initial.sql"), generated(t, before, "boards/migrations/001_initial.sql"); got != want {
			t.Errorf("001_initial of the release with the add steps is not the baseline:\n got %s\nwant %s", got, want)
		}
		if got, want := generated(t, tenancyFiles(t, boardSteps("boards", "boardtenancy")), "boardtenancy/migrations/row_security.sql"),
			generated(t, tenancyFiles(t, boardBase("boards", "boardtenancy")), "boardtenancy/migrations/row_security.sql"); got != want {
			t.Errorf("the row-level security of the tenancy component is not that of the baseline:\n got %s\nwant %s", got, want)
		}

		card := generated(t, files, "boards/migrations/"+names[0]+".sql")
		// The step that adds Card gives its table the text that 001_initial and the tenancy component give a Card of
		// the baseline, byte for byte.
		baseline := runtimeService("boards", "boardtenancy", boardSteps("boards", "boardtenancy").Entities[1:], nil)
		baseline.Entities = append(boardEntities(), baseline.Entities...)
		baseline.Collections = boardCollections(true)
		together, _, err := new(postgresadapter.Generator).Generate(baseline)
		if err != nil {
			t.Fatal(err)
		}
		initial := generated(t, together, "boards/migrations/001_initial.sql")
		cardTable := initial[strings.Index(initial, `CREATE TABLE public."cards"`):]
		security := generated(t, tenancyFiles(t, baseline), "boardtenancy/migrations/row_security.sql")
		cardSecurity := security[strings.Index(security, `ALTER TABLE public."cards"`):]
		if card != cardTable+cardSecurity {
			t.Errorf("the step of Card is not the table and the row-level security of a Card of the baseline:\n got %s\nwant %s%s", card, cardTable, cardSecurity)
		}
		if !strings.Contains(migrate, `RegisterStep("step_0001_add-card_expand", `) {
			t.Errorf("the step of Card is not a step migration with its compatible_with set:\n%s", migrate)
		}

		for name, want := range map[string]string{
			names[1]: "ALTER TABLE public.\"boards\" ADD COLUMN \"note\" character varying(40);\n" +
				"ALTER TABLE public.\"boards\" ADD CONSTRAINT \"boards_note_check\" CHECK (char_length(\"note\") >= 1) NOT VALID;\n",
			names[1] + "_index_01": "CREATE INDEX CONCURRENTLY IF NOT EXISTS \"boards_note_search\" ON public.\"boards\" (\"tenant_id\", \"note\");\n",
			names[1] + "_validate": "ALTER TABLE public.\"boards\" VALIDATE CONSTRAINT \"boards_note_check\";\n",
			names[2]:               "ALTER TABLE public.\"boards\" ADD COLUMN \"rank\" integer NOT NULL DEFAULT 0;\n",
			names[2] + "_index_01": "CREATE INDEX CONCURRENTLY IF NOT EXISTS \"boards_rank_search\" ON public.\"boards\" (\"tenant_id\", \"rank\");\n",
		} {
			if got := generated(t, files, "boards/migrations/"+name+".sql"); got != want {
				t.Errorf("%s is\n%s\nwant\n%s", name, got, want)
			}
		}
		for call, name := range map[string]string{"RegisterStep": names[1], "RegisterIndexStep": names[1] + "_index_01", "RegisterValidateStep": names[1] + "_validate"} {
			if !strings.Contains(migrate, call+`("`+name+`", `) {
				t.Errorf("migrate.go does not register %s by %s", name, call)
			}
		}
		if !strings.Contains(migrate, `RegisterIndexStep("`+names[1]+`_index_01", "public.\"boards_note_search\"", `) {
			t.Errorf("migrate.go does not name the index of %s_index_01", names[1])
		}

		again, _, err := new(postgresadapter.Generator).Generate(boardSteps("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if got := generated(t, again, file.Path); got != string(file.Content) {
				t.Errorf("the same declaration gives another %s", file.Path)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC22-T06 A rename adds its sync trigger in a migration after the indexes of its new column", func(t *testing.T) {
		files, _, err := new(postgresadapter.Generator).Generate(boardRename("boards", "boardtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		migrate := generated(t, files, "boards/migrate.go")
		registered := regexp.MustCompile(`Register(?:SQL|Step|IndexStep|ValidateStep)\("([^"]+)"`).FindAllStringSubmatch(migrate, -1)
		var order []string
		for _, match := range registered {
			order = append(order, match[1])
		}
		name := "step_0001_board-title-rename_expand"
		want := []string{"001_initial", "100_tenancy_row_security", name, name + "_index_01", name + "_index_02", postgresadapter.SyncMigrationName(name), name + "_validate"}
		if !slices.Equal(order, want) || !slices.IsSorted(order) {
			t.Errorf("migrate.go registers %v, want %v in byte order", order, want)
		}
		// The unique index of heading is built before the trigger copies title into it (decision on ST-GEN-Q18 (a)).
		indexes := generated(t, files, "boards/migrations/"+name+"_index_01.sql") + generated(t, files, "boards/migrations/"+name+"_index_02.sql")
		if !strings.Contains(indexes, `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "boards_heading_key" ON public."boards" ("tenant_id", "heading")`) {
			t.Errorf("the index migrations of the rename do not build the unique key of heading:\n%s", indexes)
		}
		body, sync := generated(t, files, "boards/migrations/"+name+".sql"), generated(t, files, "boards/migrations/"+name+"_sync.sql")
		if strings.Contains(body, "CREATE TRIGGER") || strings.Contains(body, "stego_steps") || strings.Contains(body, "stego_schema") {
			t.Errorf("the migration %s adds the sync trigger or the backfill before the indexes:\n%s", name, body)
		}
		if !strings.Contains(sync, `CREATE TRIGGER "boards_heading_sync" BEFORE INSERT OR UPDATE ON public."boards"`) || !strings.Contains(sync, "INSERT INTO stego_schema.backfills") {
			t.Errorf("the migration %s_sync does not add the sync trigger and the backfill row:\n%s", name, sync)
		}
		if !strings.Contains(migrate, `RegisterStep("`+name+`_sync", `) {
			t.Errorf("migrate.go does not register %s_sync as a step migration", name)
		}
	})

	t.Run("ST-SUP-RC01-OC32-T02 Generation refuses a runtime role that is the owner role, whose writes the sync trigger passes", func(t *testing.T) {
		for name, roles := range map[string]map[string]any{
			"both set to one role":                      {"runtime_role": "boards_owner", "owner_role": "boards_owner"},
			"the owner role set to the default runtime": {"owner_role": "boards_runtime"},
		} {
			ctx := boardRename("boards", "boardtenancy")
			// With no runtime_role the runtime role is the service name with _runtime.
			ctx.ServiceName = "boards"
			ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: roles}
			if _, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "is also the owner role") {
				t.Errorf("%s: the rename gives %v, want a refusal of a runtime role that is the owner role", name, err)
			}
		}
		ctx := boardRename("boards", "boardtenancy")
		ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "boards_runtime", "owner_role": "boards_owner"}}
		if _, _, err := new(postgresadapter.Generator).Generate(ctx); err != nil {
			t.Errorf("the rename with two roles gives %v, want no refusal", err)
		}
	})

	t.Run("ST-SUP-RC01-OC39-T02 The contract migration of a rename is its statements in their order, and the release after it holds no old column", func(t *testing.T) {
		ctx := boardRenameContract("boards", "boardtenancy")
		ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "boards_runtime", "owner_role": "boards_owner"}}
		files, _, err := new(postgresadapter.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Join([]string{
			`ALTER TABLE public."boards" ADD CONSTRAINT "boards_heading_not_null" CHECK ("heading" IS NOT NULL) NOT VALID`,
			`ALTER TABLE public."boards" VALIDATE CONSTRAINT "boards_heading_not_null"`,
			`ALTER TABLE public."boards" ALTER COLUMN "heading" SET NOT NULL`,
			`ALTER TABLE public."boards" DROP CONSTRAINT IF EXISTS "boards_heading_not_null"`,
			`ALTER TABLE public."boards" ALTER COLUMN "title" DROP NOT NULL`,
			`DROP TRIGGER IF EXISTS "boards_heading_sync" ON public."boards"`,
			`ALTER TABLE public."boards" DROP CONSTRAINT IF EXISTS "boards_heading_complete"`,
			`ALTER TABLE public."boards" DROP COLUMN IF EXISTS "title"`,
			`DROP FUNCTION IF EXISTS stego_steps."boards_heading_sync"()`,
		}, ";\n") + ";\n"
		if got := generated(t, files, "boards/migrations/step_0002_board-title-rename_contract.sql"); got != want {
			t.Errorf("the contract migration is\n%s\nwant\n%s", got, want)
		}
		migrate := generated(t, files, "boards/migrate.go")
		for _, text := range []string{`RegisterContractStep("step_0002_board-title-rename_contract", `, `Expand: "step_0001_board-title-rename_expand"`,
			`Check: "boards_heading_complete", Definition: "CHECK (((heading IS NOT NULL) OR (title IS NULL)))", Trigger: "boards_heading_sync"`, `From: "title", Old: "\"title\""`,
			// The drop of the trigger, its check again, the drop of the completeness CHECK and of the old column commit
			// in one transaction (security review of PR #316, H1).
			`{SQL: "DROP TRIGGER IF EXISTS \"boards_heading_sync\" ON public.\"boards\"", When: "", Long: false, Guard: true, Also: []string{"ALTER TABLE public.\"boards\" DROP CONSTRAINT IF EXISTS \"boards_heading_complete\"", "ALTER TABLE public.\"boards\" DROP COLUMN IF EXISTS \"title\""}}`,
			`{SQL: "DROP FUNCTION IF EXISTS stego_steps.\"boards_heading_sync\"()", When: "", Long: false, Guard: false}`,
			`{SQL: "ALTER TABLE public.\"boards\" VALIDATE CONSTRAINT \"boards_heading_not_null\"", When: "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = 'public.\"boards\"'::pg_catalog.regclass AND c.conname = 'boards_heading_not_null')", Long: true, Guard: false}`} {
			if !strings.Contains(migrate, text) {
				t.Errorf("migrate.go does not hold %s", text)
			}
		}
		manifest := generated(t, files, "boards/schema_manifest.go")
		if strings.Contains(manifest, `{Name: "title", Type:`) || strings.Contains(manifest, "OptionalTriggers") || !strings.Contains(manifest, `{Name: "heading", Type: "character varying(40)", NotNull: true}`) ||
			!strings.Contains(manifest, `const SchemaRelease = "step_0002_board-title-rename_contract"`) {
			t.Errorf("the manifest of the release after the rename is\n%s", manifest)
		}
		// The release of the rename names the NOT NULL CHECK of the contract step, which may be NOT VALID.
		ctx = boardRename("boards", "boardtenancy")
		ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "boards_runtime", "owner_role": "boards_owner"}}
		files, _, err = new(postgresadapter.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if manifest := generated(t, files, "boards/schema_manifest.go"); !strings.Contains(manifest, `{Name: "boards_heading_not_null", Kind: manifest.KindCheck, Definition: "CHECK ((heading IS NOT NULL))", NotValid: true}`) {
			t.Errorf("the manifest of the release of the rename does not name the NOT NULL CHECK of the contract step:\n%s", manifest)
		}
		// The contract of a rename of a field with a default first gives the new column the default, with the comment
		// of a relaxed form that states it, in one transaction, while the column has no default (walkthrough 14,
		// W14-B3).
		ctx = boardRenameContract("boards", "boardtenancy")
		ctx.Entities[0].Fields[0].Default = "untitled"
		files, _, err = new(postgresadapter.Generator).Generate(ctx)
		if err != nil {
			t.Fatalf("the contract of a rename of a field with a default: %v", err)
		}
		contract := generated(t, files, "boards/migrations/step_0002_board-title-rename_contract.sql")
		first := "ALTER TABLE public.\"boards\" ALTER COLUMN \"heading\" SET DEFAULT 'untitled';\n" +
			"COMMENT ON COLUMN public.\"boards\".\"heading\" IS 'stego relax step_0001_board-title-rename_expand\ndefault ''untitled''::character varying(40)';\n"
		if !strings.HasPrefix(contract, first) {
			t.Errorf("the contract migration of a field with a default is\n%s\nwant it to start with\n%s", contract, first)
		}
		if migrate := generated(t, files, "boards/migrate.go"); !strings.Contains(migrate, `{SQL: "ALTER TABLE public.\"boards\" ALTER COLUMN \"heading\" SET DEFAULT 'untitled'", When: "SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum WHERE d.adrelid = 'public.\"boards\"'::pg_catalog.regclass AND a.attname = 'heading' AND NOT a.attisdropped)", Long: false, Guard: false, Also: []string{"COMMENT ON COLUMN public.\"boards\".\"heading\" IS 'stego relax step_0001_board-title-rename_expand\ndefault ''untitled''::character varying(40)'"}}`) {
			t.Errorf("migrate.go does not give the default with its comment while the column has none:\n%s", migrate)
		}
		if manifest := generated(t, files, "boards/schema_manifest.go"); !strings.Contains(manifest, `{Name: "heading", Type: "character varying(40)", NotNull: true, Default: "'untitled'::character varying(40)"}`) {
			t.Errorf("the manifest of the release after the rename of a field with a default is\n%s", manifest)
		}
	})
	t.Run("ST-SUP-RC01-OC23-T02 The completeness CHECK and the backfill of a retype with no fill value allow a value that does not convert", func(t *testing.T) {
		ctx := boardRetype("boards", "boardtenancy")
		ctx.SchemaChanges[0].FillValue = nil
		files, _, err := new(postgresadapter.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		manifest := generated(t, files, "boards/schema_manifest.go")
		for _, want := range []string{`Definition: "CHECK (((revision_int64 IS NOT NULL) OR (revision IS NULL) OR (NOT pg_input_is_valid(revision, 'pg_catalog.int8'::text))))"`,
			`Guard: "pg_catalog.pg_input_is_valid({K}, 'pg_catalog.int8')"`, `ELSE NULL END)"`} {
			if !strings.Contains(manifest, want) {
				t.Errorf("the manifest of a retype with no fill value does not hold %s:\n%s", want, manifest)
			}
		}
		sync := generated(t, files, "boards/migrations/step_0001_board-revision-retype_expand_sync.sql")
		if want := `CHECK ((revision_int64 IS NOT NULL) OR (revision IS NULL) OR (NOT pg_catalog.pg_input_is_valid(revision, 'pg_catalog.int8'))) NOT VALID`; !strings.Contains(sync, want) {
			t.Errorf("the sync migration of a retype with no fill value does not hold %s:\n%s", want, sync)
		}
	})
	t.Run("ST-SUP-RC01-OC22-T02 Generation refuses a step that is not the expand step of an add or a rename, or an add that the rows cannot take", func(t *testing.T) {
		refused := map[string]func(*gen.Context){
			"drop": func(ctx *gen.Context) {
				ctx.Entities[0].Fields = ctx.Entities[0].Fields[:2]
				ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: "board-rank-drop", Kind: "drop", Entity: "Board", Field: "rank", Definition: "int32"})
				ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "board-rank-drop", Phase: "expand"})
			},
			"unique after it": func(ctx *gen.Context) {
				*ctx = boardRetype(ctx.OutputNamespace, ctx.PeerNamespaces[gen.TenancyComponent])
				ctx.Entities[0].Fields[1].Unique = true
			},
			"is of a unique field": func(ctx *gen.Context) {
				*ctx = boardRetype(ctx.OutputNamespace, ctx.PeerNamespaces[gen.TenancyComponent])
				ctx.Entities[0].Fields[2].Unique = true
			},
			"one sync trigger on a table": func(ctx *gen.Context) {
				*ctx = boardRetype(ctx.OutputNamespace, ctx.PeerNamespaces[gen.TenancyComponent])
				ctx.Entities[1].Fields[1] = types.Field{Name: "level", Type: types.FieldTypeString, Pattern: "^-?[0-9]{1,18}$", Default: "0"}
				ctx.SchemaChanges[1].Entity, ctx.Entities[0].Fields = "Board", append(ctx.Entities[0].Fields, types.Field{Name: "level", Type: types.FieldTypeString, Column: "level_string"})
				ctx.Entities[1].Fields = ctx.Entities[1].Fields[:1]
			},
			"is not built": func(ctx *gen.Context) {
				ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "add-card", Phase: "contract"})
			},
			"NOT NULL with no default": func(ctx *gen.Context) { ctx.Entities[0].Fields[2].Default = nil },
			"which a later step adds": func(ctx *gen.Context) {
				ctx.Entities[1].Fields = ctx.Entities[1].Fields[1:]
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "card_id", Type: types.FieldTypeRef, To: "Card", Optional: true})
				ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: "add-board-card-id", Kind: "add", Entity: "Board", Field: "card_id"})
				ctx.SchemaHistory = append([]types.SchemaStep{{Change: "add-board-card-id", Phase: "expand"}}, ctx.SchemaHistory...)
			},
			"has no step": func(ctx *gen.Context) { ctx.SchemaHistory = ctx.SchemaHistory[1:] },
		}
		for want, change := range refused {
			ctx := boardSteps("boards", "boardtenancy")
			change(&ctx)
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err == nil || !strings.Contains(err.Error(), want) || files != nil {
				t.Errorf("%s: files %d, error %v; want a refusal that says %q", want, len(files), err, want)
			}
		}
	})
}

// markerOf gives the two marker digests that the schema_generation.go of the namespace ns holds.
func markerOf(t *testing.T, files []gen.File, ns string) (string, string) {
	t.Helper()
	source := generated(t, files, ns+"/schema_generation.go")
	definition := regexp.MustCompile(`const SchemaDefinition = "([0-9a-f]{64})"`).FindStringSubmatch(source)
	before := regexp.MustCompile(`const schemaDefinitionBefore = "([0-9a-f]{64})"`).FindStringSubmatch(source)
	if definition == nil || before == nil {
		t.Fatalf("schema_generation.go holds no two marker digests:\n%s", source)
	}
	return definition[1], before[1]
}

func TestSchemaMarker(t *testing.T) {
	t.Run("ST-SUP-RC01-OC42-T01 The marker digest changes with the stored shape and not with a collection or a permission", func(t *testing.T) {
		generate := func(ctx gen.Context) (string, string) {
			t.Helper()
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return markerOf(t, files, "boards")
		}
		definition, before := generate(boardBase("boards", "boardtenancy"))
		if definition == before {
			t.Error("the digest of the stored shape is the digest that a release before wrote")
		}
		if steps, _ := generate(boardSteps("boards", "boardtenancy")); steps != definition {
			t.Error("the add steps change the marker digest")
		}
		listed := boardBase("boards", "boardtenancy")
		listed.Collections = append(listed.Collections, types.Collection{Name: "boards-by-title", Entity: "Board", Operations: []types.Operation{types.OpList, types.OpUpsert}, UpsertKey: []string{"title"}, Implicit: map[string]string{"title": "x"}})
		listed.Entities[0].ResourceType, listed.Entities[0].Permissions = "", map[types.Operation]string{types.OpRead: "public"}
		if got, gotBefore := generate(listed); got != definition || gotBefore == before {
			t.Errorf("a collection and a permission change the digest of the stored shape (%v) or do not change the digest of a release before (%v)", got != definition, gotBefore == before)
		}
		for name, change := range map[string]func(*gen.Context){
			"a new column": func(ctx *gen.Context) {
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "note", Type: types.FieldTypeString, Optional: true})
			},
			"a NOT NULL":     func(ctx *gen.Context) { ctx.Entities[0].Fields[0].Optional = true },
			"a unique field": func(ctx *gen.Context) { ctx.Entities[0].Fields[0].Unique = true },
			"a protected entity": func(ctx *gen.Context) {
				ctx.Entities[0].ResourceType = "board"
			},
		} {
			ctx := boardBase("boards", "boardtenancy")
			change(&ctx)
			if got, _ := generate(ctx); got == definition {
				t.Errorf("%s does not change the digest of the stored shape", name)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC43-T01 Generation refuses a ledgered migration that is not the baseline, the tenancy row-level security or a step", func(t *testing.T) {
		for _, name := range []string{"001_initial", "100_tenancy_row_security", "step_0001_add-card_expand", "step_0012_note-title-rename_contract",
			"step_0002_add-board-note_expand_index_01", "step_0002_add-board-note_expand_validate", "step_0003_note-rename_expand_sync"} {
			if err := postgresadapter.CheckLedgerName(name); err != nil {
				t.Errorf("%s is refused: %v", name, err)
			}
		}
		for _, name := range []string{"101_x", "002_audit", "200_outbox", "step_1_add-card_expand", "step_0001_add-card_backfill", "step_0001_add-card_expand_index_1", "step_0001_add-card_expand_valid", "step_0001_add-card_validate", "step_0001_add-card_sync", "step_0001_add-card_expand_synced", "step_0001_Add_expand", "zz_late", ""} {
			if err := postgresadapter.CheckLedgerName(name); err == nil || !strings.Contains(err.Error(), "ST-SUP-RC01-OC43") {
				t.Errorf("%s is accepted, or its refusal does not name ST-SUP-RC01-OC43: %v", name, err)
			}
		}
		// A step name that the grammar refuses stops generation: generation checks each name that migrate.go registers.
		ctx := boardSteps("boards", "boardtenancy")
		ctx.SchemaChanges[0].ID = "Add_Card"
		ctx.SchemaHistory[0].Change = "Add_Card"
		if files, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "ST-SUP-RC01-OC43") || files != nil {
			t.Errorf("a step named step_0001_Add_Card_expand gives files %d and error %v, want the refusal", len(files), err)
		}
	})
}

// withinEntities are the entities of the within cases (ST-SUP-RC01-OC46): the owned Project, the owned Sprint of a
// project, and the owned Ticket of a project with an optional sprint; with within, the sprint of a ticket is held
// within project_id.
func withinEntities(within string) []types.Entity {
	return []types.Entity{
		{Name: "Project", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
		{Name: "Sprint", Fields: []types.Field{{Name: "project_id", Type: types.FieldTypeRef, To: "Project"}, {Name: "name", Type: types.FieldTypeString}}},
		{Name: "Ticket", EntityAuthorization: types.EntityAuthorization{References: map[string]types.Reference{
			"sprint_id": {Relation: "sprint", Permission: "edit", Within: within}}},
			Fields: []types.Field{{Name: "project_id", Type: types.FieldTypeRef, To: "Project", Optional: true}, {Name: "sprint_id", Type: types.FieldTypeRef, To: "Sprint", Optional: true},
				{Name: "title", Type: types.FieldTypeString}}},
	}
}

// withinBase is the release before the within step: the sprint of a ticket has no within.
func withinBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, withinEntities(""), nil)
}

// withinHead is withinBase with within: project_id on the sprint of a ticket, added by the within step
// ticket-sprint-within on the tables with rows (ST-SUP-RC01-OC46, ST-DECL-Q42).
func withinHead(ns, tenancyNS string) gen.Context {
	ctx := runtimeService(ns, tenancyNS, withinEntities("project_id"), nil)
	ctx.SchemaChanges = []types.SchemaChange{{ID: "ticket-sprint-within", Kind: "within", Entity: "Ticket", Field: "sprint_id"}}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "ticket-sprint-within", Phase: "expand"}}
	return ctx
}

// withinFresh is a first release with within: project_id on the sprint of a ticket, so 001_initial holds the key.
func withinFresh(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, withinEntities("project_id"), nil)
}

func TestWithinKeys(t *testing.T) {
	t.Run("ST-SUP-RC01-OC46-T03 The within step builds the unique key, then adds the foreign key NOT VALID, then validates it, each in its own migration", func(t *testing.T) {
		files, _, err := new(postgresadapter.Generator).Generate(withinHead("within", "withintenancy"))
		if err != nil {
			t.Fatal(err)
		}
		bodies := map[string]string{}
		var names []string
		for _, f := range files {
			if dir, name := filepath.Split(f.Path); dir == filepath.Join("within", "migrations")+string(os.PathSeparator) {
				bodies[strings.TrimSuffix(name, ".sql")] = string(f.Content)
				names = append(names, strings.TrimSuffix(name, ".sql"))
			}
		}
		want := []string{"001_initial", "step_0001_ticket-sprint-within_expand_index_01", "step_0001_ticket-sprint-within_expand_keys", "step_0001_ticket-sprint-within_expand_validate"}
		for _, name := range want {
			if _, ok := bodies[name]; !ok {
				t.Fatalf("no migration %s among %v", name, names)
			}
		}
		if initial := bodies["001_initial"]; strings.Contains(initial, "within") || strings.Contains(initial, "sprints_project_id_id_key") {
			t.Errorf("001_initial holds the key that the step adds:\n%s", initial)
		}
		if body := bodies[want[1]]; !strings.Contains(body, `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "sprints_project_id_id_key" ON public."sprints" ("tenant_id", "project_id", "id")`) {
			t.Errorf("the index migration does not build the unique key of sprints:\n%s", body)
		}
		if body := bodies[want[2]]; body != `ALTER TABLE public."tickets" ADD CONSTRAINT "tickets_sprint_id_within_fkey" FOREIGN KEY ("tenant_id", "project_id", "sprint_id") REFERENCES public."sprints" ("tenant_id", "project_id", "id") NOT VALID;`+"\n" {
			t.Errorf("the keys migration is not the foreign key NOT VALID:\n%s", body)
		}
		if body := bodies[want[3]]; body != `ALTER TABLE public."tickets" VALIDATE CONSTRAINT "tickets_sprint_id_wcheck";`+"\n"+
			`ALTER TABLE public."tickets" VALIDATE CONSTRAINT "tickets_sprint_id_within_fkey";`+"\n" {
			t.Errorf("the validate migration does not validate the CHECK and the key:\n%s", body)
		}
		// The step's own migration runs first and adds only the CHECK of the optional within field NOT VALID: the key
		// waits for the unique key it refers to.
		if body := bodies["step_0001_ticket-sprint-within_expand"]; body != `ALTER TABLE public."tickets" ADD CONSTRAINT "tickets_sprint_id_wcheck" CHECK ("sprint_id" IS NULL OR "project_id" IS NOT NULL) NOT VALID;`+"\n" {
			t.Errorf("the step's own migration is not the CHECK of the within field NOT VALID:\n%s", body)
		}
		fresh, _, err := new(postgresadapter.Generator).Generate(withinFresh("withinfresh", "withinfreshtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		initial := generated(t, fresh, "withinfresh/migrations/001_initial.sql")
		for _, part := range []string{`CREATE UNIQUE INDEX "sprints_project_id_id_key" ON public."sprints" ("tenant_id", "project_id", "id")`,
			`CONSTRAINT "tickets_sprint_id_within_fkey" FOREIGN KEY ("tenant_id", "project_id", "sprint_id") REFERENCES public."sprints" ("tenant_id", "project_id", "id")`} {
			if !strings.Contains(initial, part) {
				t.Errorf("001_initial of a first release does not hold %s:\n%s", part, initial)
			}
		}
		if strings.Index(initial, "sprints_project_id_id_key") > strings.Index(initial, "tickets_sprint_id_within_fkey") {
			t.Errorf("001_initial adds the key before the unique key it refers to:\n%s", initial)
		}
	})

	t.Run("ST-SUP-RC01-OC46-T05 A reference held within an optional field gets a CHECK that the field is set when the reference is set, and one held within a required field gets none", func(t *testing.T) {
		const check = `CONSTRAINT "tickets_sprint_id_wcheck" CHECK ("sprint_id" IS NULL OR "project_id" IS NOT NULL)`
		fresh, _, err := new(postgresadapter.Generator).Generate(withinFresh("withinopt", "withinopttenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if initial := generated(t, fresh, "withinopt/migrations/001_initial.sql"); !strings.Contains(initial, check) {
			t.Errorf("001_initial of an optional within field does not hold %s:\n%s", check, initial)
		}
		if manifest := generated(t, fresh, "withinopt/schema_manifest.go"); !strings.Contains(manifest, `CHECK (((sprint_id IS NULL) OR (project_id IS NOT NULL)))`) {
			t.Errorf("the schema manifest does not hold the CHECK as pg_get_constraintdef writes it:\n%s", manifest)
		}
		required := withinFresh("withinreq", "withinreqtenancy")
		required.Entities[2].Fields[0].Optional = false
		files, _, err := new(postgresadapter.Generator).Generate(required)
		if err != nil {
			t.Fatal(err)
		}
		if initial := generated(t, files, "withinreq/migrations/001_initial.sql"); strings.Contains(initial, "wcheck") || !strings.Contains(initial, "tickets_sprint_id_within_fkey") {
			t.Errorf("001_initial of a required within field holds a CHECK of the field, or no key:\n%s", initial)
		}
	})

	t.Run("ST-SUP-RC01-OC46-T04 The composite key of a reference to its own entity, or across the tenant column, is refused", func(t *testing.T) {
		self := withinFresh("withinself", "withinselftenancy")
		self.Entities[2].Fields = append(self.Entities[2].Fields, types.Field{Name: "parent_id", Type: types.FieldTypeRef, To: "Ticket", Optional: true})
		self.Entities[2].References["parent_id"] = types.Reference{Relation: "parent", Permission: "edit", Within: "project_id"}
		if _, _, err := new(postgresadapter.Generator).Generate(self); err == nil || !strings.Contains(err.Error(), "parent_id refers to its own entity") {
			t.Errorf("a within of a reference to its own entity gives %v", err)
		}
		shared := withinFresh("withinshared", "withinsharedtenancy")
		shared.Entities[0].Tenant, shared.Entities[1].Tenant = "shared", "shared"
		if _, _, err := new(postgresadapter.Generator).Generate(shared); err == nil || !strings.Contains(err.Error(), "only one of Ticket and Sprint holds the tenant column") {
			t.Errorf("a within across the tenant column gives %v", err)
		}
	})
}

func TestNamesApart(t *testing.T) {
	t.Run("ST-SUP-RC01-OC46-T06 A field whose foreign key takes the name of the composite key of a within is refused at generation, naming both", func(t *testing.T) {
		// The field sprint_id_within is a reference, so its plain foreign key is tickets_sprint_id_within_fkey: the
		// name of the composite key of the within on sprint_id.
		clash := withinFresh("withinclash", "withinclashtenancy")
		clash.Entities[2].Fields = append(clash.Entities[2].Fields, types.Field{Name: "sprint_id_within", Type: types.FieldTypeRef, To: "Sprint", Optional: true})
		_, _, err := new(postgresadapter.Generator).Generate(clash)
		if err == nil || !strings.Contains(err.Error(), "are both tickets_sprint_id_within_fkey") || !strings.Contains(err.Error(), "(tenant_id, sprint_id_within)") ||
			!strings.Contains(err.Error(), "(tenant_id, project_id, sprint_id)") {
			t.Errorf("a field whose key takes the name of the within key gives %v", err)
		}
		// The within key keeps its name, so a database that holds it needs no rename.
		fresh, _, err := new(postgresadapter.Generator).Generate(withinFresh("withinkept", "withinkepttenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if initial := generated(t, fresh, "withinkept/migrations/001_initial.sql"); !strings.Contains(initial, `CONSTRAINT "tickets_sprint_id_within_fkey"`) {
			t.Errorf("the within key lost its name:\n%s", initial)
		}
	})
}

// siteEntities are the entities of the unique cases of a field (ST-SUP-RC01-OC38, ST-GEN-Q48): the owned Site with
// a name and a region, with the unique of the field given to name, or none.
func siteEntities(name types.Field) []types.Entity {
	name.Name, name.Type = "name", types.FieldTypeString
	return []types.Entity{{Name: "Site", Fields: []types.Field{name, {Name: "region", Type: types.FieldTypeString}}}}
}

// uniqueBase is the release before the unique step: the name of a site is not unique.
func uniqueBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, siteEntities(types.Field{}), nil)
}

// uniqueHead is uniqueBase with name made unique by the unique of the field site-name-unique on the tables with
// rows (ST-SUP-RC01-OC38, ST-GEN-Q48).
func uniqueHead(ns, tenancyNS string) gen.Context {
	ctx := runtimeService(ns, tenancyNS, siteEntities(types.Field{Unique: true}), nil)
	ctx.SchemaChanges = []types.SchemaChange{{ID: "site-name-unique", Kind: "unique", Entity: "Site", Field: "name"}}
	ctx.SchemaHistory = []types.SchemaStep{{Change: "site-name-unique", Phase: "expand"}}
	return ctx
}

func TestUniqueOfFieldStep(t *testing.T) {
	t.Run("ST-SUP-RC01-OC38-T09 A unique of a field builds the unique index of the field in its index migration at expand, and has no contract step", func(t *testing.T) {
		migrations := func(ctx gen.Context) map[string]string {
			t.Helper()
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			bodies := map[string]string{}
			for _, f := range files {
				if dir, name := filepath.Split(f.Path); dir == filepath.Join(ctx.OutputNamespace, "migrations")+string(os.PathSeparator) {
					bodies[strings.TrimSuffix(name, ".sql")] = string(f.Content)
				}
			}
			return bodies
		}
		bodies := migrations(uniqueHead("uniquestep", "uniquesteptenancy"))
		const index = "step_0001_site-name-unique_expand_index_01"
		if want := `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "sites_name_key" ON public."sites" ("tenant_id", "name");` + "\n"; bodies[index] != want {
			t.Errorf("the index migration %s is %q, want %q (all: %v)", index, bodies[index], want, bodies)
		}
		if body, ok := bodies["step_0001_site-name-unique_expand"]; !ok || body != "" {
			t.Errorf("the step's own migration is %q (present %v), want an empty body: the index is its own migration", body, ok)
		}
		if initial := bodies["001_initial"]; strings.Contains(initial, "sites_name_key") {
			t.Errorf("001_initial holds the unique index that the step builds:\n%s", initial)
		}
		for name := range bodies {
			if strings.Contains(name, "_validate") || strings.Contains(name, "_sync") || strings.Contains(name, "_keys") {
				t.Errorf("the unique of a field has the migration %s, want only its own and its index migration", name)
			}
		}
		// A unique_composite: the index of the group, in the order of the group, after the tenant column.
		composite := uniqueHead("uniquegroup", "uniquegrouptenancy")
		composite.Entities = siteEntities(types.Field{UniqueComposite: []string{"region", "name"}})
		if body := migrations(composite)["step_0001_site-name-unique_expand_index_01"]; !strings.Contains(body, `"sites_region_name_key" ON public."sites" ("tenant_id", "region", "name")`) {
			t.Errorf("the index migration of the unique_composite is %q", body)
		}
		// A contract step is not built, and the release of the unique step names its index in its manifest.
		contract := uniqueHead("uniquec", "uniquectenancy")
		contract.SchemaHistory = append(contract.SchemaHistory, types.SchemaStep{Change: "site-name-unique", Phase: "contract"})
		if _, _, err := new(postgresadapter.Generator).Generate(contract); err == nil || !strings.Contains(err.Error(), "is not built") {
			t.Errorf("a contract step of the unique of a field gives %v, want the refusal", err)
		}
		files, _, err := new(postgresadapter.Generator).Generate(uniqueHead("uniquem", "uniquemtenancy"))
		if err != nil {
			t.Fatal(err)
		}
		if source := generated(t, files, "uniquem/schema_manifest.go"); !strings.Contains(source, `{Name: "sites_name_key", Columns: []string{"tenant_id", "name"}}`) {
			t.Errorf("the manifest of the release of the step does not name its unique index:\n%s", source)
		}
		// The manifest marks the column of a secret Secret, so the schema check of the release before takes no
		// unique index of a later step on it (ST-SUP-RP15-LC01; security review of PR #484, L2).
		sealed := uniqueBase("uniques", "uniquestenancy")
		sealed.Entities[0].Fields = append(sealed.Entities[0].Fields, types.Field{Name: "token", Type: types.FieldTypeString, Sensitivity: "secret"})
		files, _, err = new(postgresadapter.Generator).Generate(sealed)
		if err != nil {
			t.Fatal(err)
		}
		source := generated(t, files, "uniques/schema_manifest.go")
		if !strings.Contains(source, `{Name: "token", Type: "bytea", NotNull: true, Secret: true}`) || strings.Contains(source, `{Name: "region", Type: "text", NotNull: true, Secret: true}`) {
			t.Errorf("the manifest does not mark only the secret column Secret:\n%s", source)
		}
		// A unique change that names no field of the entity is a unique of an invariant, which generation does not
		// build yet.
		invariant := uniqueHead("uniquei", "uniqueitenancy")
		invariant.SchemaChanges[0].Field = "site_name_once"
		if _, _, err := new(postgresadapter.Generator).Generate(invariant); err == nil {
			t.Error("a unique of an invariant is built")
		}
	})
}
