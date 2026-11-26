package postgresadapter_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// dropEntities are the entities of the release before the drops (ST-SUP-RC01-OC34, ST-SUP-RC01-OC47): the owned
// entity Board with the title, the required string legacy of at most 40 characters, the required int32 rank and the
// optional note, and the owned entity Pin, which refers to a Board.
func dropEntities() []types.Entity {
	n := 40
	return []types.Entity{
		{Name: "Board", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString},
			{Name: "legacy", Type: types.FieldTypeString, MaxLength: &n}, {Name: "rank", Type: types.FieldTypeInt32},
			{Name: "note", Type: types.FieldTypeString, Optional: true}}},
		{Name: "Pin", Fields: []types.Field{{Name: "board_id", Type: types.FieldTypeRef, To: "Board"}, {Name: "label", Type: types.FieldTypeString}}},
	}
}

func dropCollections() []types.Collection {
	return []types.Collection{{Name: "boards", Entity: "Board", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}},
		{Name: "pins", Entity: "Pin", Operations: []types.Operation{types.OpCreate, types.OpList}}}
}

// dropBase is the release before the drops.
func dropBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, dropEntities(), dropCollections())
}

// dropHead is dropBase with three drops at expand: board-legacy-drop of Board.legacy with no fill value,
// board-rank-drop of Board.rank with the fill value 7, and pin-drop of the entity Pin, which the declaration keeps
// with dropped_by, as the compiler gives it (ST-GEN-Q47).
func dropHead(ns, tenancyNS string) gen.Context {
	ctx := dropBase(ns, tenancyNS)
	all := dropEntities()
	board, pin := all[0], all[1]
	board.Fields = []types.Field{board.Fields[0], board.Fields[3]}
	pin.DroppedBy = "pin-drop"
	ctx.Entities = []types.Entity{board}
	ctx.Collections = dropCollections()[:1]
	ctx.DroppedEntities = []types.Entity{pin}
	ctx.DroppedCollections = dropCollections()[1:]
	ctx.EntityOrder, ctx.CollectionOrder = []string{"Board", "Pin"}, []string{"boards", "pins"}
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "board-legacy-drop", Kind: "drop", Entity: "Board", Field: "legacy", Definition: "string, max_length 40, after title"},
		{ID: "board-rank-drop", Kind: "drop", Entity: "Board", Field: "rank", FillValue: 7, Definition: "int32, after title"},
		{ID: "pin-drop", Kind: "drop", Entity: "Pin"},
	}
	for _, change := range ctx.SchemaChanges {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "expand"})
	}
	return ctx
}

// dropContract is dropHead with the contract step of each drop.
func dropContract(ns, tenancyNS string) gen.Context {
	ctx := dropHead(ns, tenancyNS)
	for _, change := range ctx.SchemaChanges {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "contract"})
	}
	return ctx
}

func TestDropSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC47-T01 The migrations of a drop keep the old column or table at expand and drop it at contract with short statements", func(t *testing.T) {
		roles := map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "drop_runtime", "owner_role": "drop_owner"}}
		base, head, contract := dropBase("dropbase", "dropbasetenancy"), dropHead("drop", "droptenancy"), dropContract("dropc", "dropctenancy")
		for _, ctx := range []*gen.Context{&base, &head, &contract} {
			ctx.PeerConfigs = roles
		}
		baseFiles, _, err := new(postgresadapter.Generator).Generate(base)
		if err != nil {
			t.Fatal(err)
		}
		headFiles, _, err := new(postgresadapter.Generator).Generate(head)
		if err != nil {
			t.Fatal(err)
		}
		contractFiles, _, err := new(postgresadapter.Generator).Generate(contract)
		if err != nil {
			t.Fatal(err)
		}
		// The baseline of each release is the baseline of the release before: the dropped field and entity come
		// back from the declaration, in their places (ST-GEN-INV33).
		initial := generated(t, baseFiles, "dropbase/migrations/001_initial.sql")
		for name, files := range map[string][]gen.File{"drop": headFiles, "dropc": contractFiles} {
			if got := generated(t, files, name+"/migrations/001_initial.sql"); got != initial {
				t.Errorf("the 001_initial of %s is not the one of the release before:\n%s\nwant\n%s", name, got, initial)
			}
		}
		legacy := generated(t, headFiles, "drop/migrations/step_0001_board-legacy-drop_expand.sql")
		for _, want := range []string{
			`ALTER TABLE public."boards" ALTER COLUMN "legacy" DROP NOT NULL, ALTER COLUMN "legacy" SET DEFAULT '';`,
			`COMMENT ON COLUMN public."boards"."legacy" IS 'stego relax step_0001_board-legacy-drop_expand` + "\n" + `nullable` + "\n" + `default ''''::character varying(40)';`,
			"CREATE TABLE IF NOT EXISTS stego_schema.releases",
		} {
			if !strings.Contains(legacy, want) {
				t.Errorf("the expand of the drop of legacy does not hold %s:\n%s", want, legacy)
			}
		}
		if rank := generated(t, headFiles, "drop/migrations/step_0002_board-rank-drop_expand.sql"); !strings.Contains(rank, `ALTER COLUMN "rank" DROP NOT NULL, ALTER COLUMN "rank" SET DEFAULT 7;`) {
			t.Errorf("the expand of the drop of rank does not give the fill value as its default:\n%s", rank)
		}
		if pin := generated(t, headFiles, "drop/migrations/step_0003_pin-drop_expand.sql"); strings.Contains(pin, "ALTER TABLE") || strings.Contains(pin, "DROP") || !strings.Contains(pin, "stego_schema.releases") {
			t.Errorf("the expand of the drop of Pin changes a table, or makes no table of the release reports:\n%s", pin)
		}
		// The release of the drops does not name the old column or table in its manifest.
		manifest := generated(t, headFiles, "drop/schema_manifest.go")
		for _, gone := range []string{`"legacy"`, `"rank"`, `"pins"`} {
			if strings.Contains(manifest, gone) {
				t.Errorf("the manifest of the release of the drops names %s:\n%s", gone, manifest)
			}
		}
		// The last statement of each contract deletes the counts of wrong attempts of the dropped field or entity
		// (decision ST-SUP-Q216 (a)); the body before it is the body of an adapter before that delete, whose digest
		// the ledger accepts as an earlier digest of the step.
		purge := func(where string) string {
			return "DO $purge$ DECLARE t text; BEGIN IF pg_catalog.to_regclass('public.attempt_counts') IS NULL OR pg_catalog.to_regclass('public.stego_tenants') IS NULL THEN RETURN; END IF; " +
				"FOR t IN SELECT s.tenant_id FROM public.stego_tenants s ORDER BY s.tenant_id COLLATE \"C\" LOOP PERFORM pg_catalog.set_config('app.tenant', t, true); " +
				"DELETE FROM public.attempt_counts WHERE tenant_id = t AND " + where + "; END LOOP; PERFORM pg_catalog.set_config('app.tenant', '', true); END $purge$;\n"
		}
		migrate := generated(t, contractFiles, "dropc/migrate.go")
		for file, before := range map[string][2]string{
			"step_0004_board-legacy-drop_contract": {"ALTER TABLE public.\"boards\" DROP COLUMN IF EXISTS \"legacy\";\n", "counter = 'Board.legacy'"},
			"step_0005_board-rank-drop_contract":   {"ALTER TABLE public.\"boards\" DROP COLUMN IF EXISTS \"rank\";\n", "counter = 'Board.rank'"},
			"step_0006_pin-drop_contract": {"ALTER TABLE IF EXISTS public.\"pins\" DROP CONSTRAINT IF EXISTS \"pins_board_id_fkey\";\n" +
				"ALTER TABLE IF EXISTS public.\"pins\" DROP CONSTRAINT IF EXISTS \"pins_tenant_id_fkey\";\nDROP TABLE IF EXISTS public.\"pins\";\n", "pg_catalog.split_part(counter, '.', 1) = 'Pin'"},
		} {
			if got, want := generated(t, contractFiles, "dropc/migrations/"+file+".sql"), before[0]+purge(before[1]); got != want {
				t.Errorf("%s.sql is\n%s\nwant\n%s", file, got, want)
			}
			sum := sha256.Sum256([]byte(before[0]))
			if want := fmt.Sprintf("RegisterEarlierDigests(%q, %q)", file, hex.EncodeToString(sum[:])); !strings.Contains(migrate, want) {
				t.Errorf("migrate.go of the contract does not register the digest of the body before the delete of the counts: %s", want)
			}
		}
		for _, want := range []string{
			`{SQL: "DROP TABLE IF EXISTS public.\"pins\"", When: "", Long: false, Guard: true},`,
			`{SQL: "ALTER TABLE public.\"boards\" DROP COLUMN IF EXISTS \"legacy\"", When: "", Long: false, Guard: true},`,
			`{SQL: "ALTER TABLE IF EXISTS public.\"pins\" DROP CONSTRAINT IF EXISTS \"pins_board_id_fkey\"", When: "", Long: false, Guard: true},`,
			`{SQL: "ALTER TABLE IF EXISTS public.\"pins\" DROP CONSTRAINT IF EXISTS \"pins_tenant_id_fkey\"", When: "", Long: false, Guard: true},`,
			`}, Drop: true})`,
		} {
			if !strings.Contains(migrate, want) {
				t.Errorf("migrate.go of the contract does not hold %s", want)
			}
		}
	})

	t.Run("ST-SUP-RC01-OC47-T01 A drop that generation does not carry is refused with its reason", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			change types.SchemaChange
			fields []types.Field
			want   string
		}{
			{"a reference", types.SchemaChange{ID: "pin-board-drop", Kind: "drop", Entity: "Pin", Field: "board_id", Definition: "ref, first"},
				[]types.Field{{Name: "label", Type: types.FieldTypeString}}, "is of a reference"},
			{"a required unique field", types.SchemaChange{ID: "pin-label-drop", Kind: "drop", Entity: "Pin", Field: "label", Definition: "string, unique, after board_id"},
				[]types.Field{{Name: "board_id", Type: types.FieldTypeRef, To: "Board"}}, "unique field"},
			{"the column created_time that STEGO adds itself", types.SchemaChange{ID: "pin-time-drop", Kind: "drop", Entity: "Pin", Field: "created_time", Definition: "timestamp, after label"},
				[]types.Field{{Name: "board_id", Type: types.FieldTypeRef, To: "Board"}, {Name: "label", Type: types.FieldTypeString}}, "adds to each table itself"},
			{"a timestamp with no fill value", types.SchemaChange{ID: "pin-label-drop", Kind: "drop", Entity: "Pin", Field: "label", Definition: "timestamp, after board_id"},
				[]types.Field{{Name: "board_id", Type: types.FieldTypeRef, To: "Board"}}, "has no zero value"},
		} {
			base := dropEntities()
			if c.name == "a reference" {
				base[1].Fields = []types.Field{{Name: "board_id", Type: types.FieldTypeRef, To: "Board"}, {Name: "label", Type: types.FieldTypeString}}
			}
			ctx := runtimeService("dropref", "dropreftenancy", base, dropCollections())
			ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "drop_runtime", "owner_role": "drop_owner"}}
			ctx.Entities[1].Fields = c.fields
			ctx.SchemaChanges = []types.SchemaChange{c.change}
			ctx.SchemaHistory = []types.SchemaStep{{Change: c.change.ID, Phase: "expand"}}
			if _, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: the drop gives %v, want an error that says %q", c.name, err, c.want)
			}
		}
	})
}
