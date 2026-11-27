package compiler

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

func TestRenamedPatchableField(t *testing.T) {
	t.Run("ST-GEN-UC12-R04-T03 A collection patches a renamed field by its API name, also after two renames", func(t *testing.T) {
		input := snapshotTestInput(t)
		// Widget.label is renamed to title, and title to heading; the collection names heading as patchable, by
		// its declared name (walkthrough 16, W16-N1).
		service := `kind: service
name: test-service
archetype: test-arch
language: go

entities:
  - name: Widget
    fields:
      - { name: heading, type: string }
      - { name: note, type: string }

collections:
  widgets:
    entity: Widget
    operations: [create, read, patch]
    patchable: [heading, note]

schema_changes:
  - {id: widget-label-rename, kind: rename, entity: Widget, field: title, from: label}
  - {id: widget-title-rename, kind: rename, entity: Widget, field: heading, from: title}
schema_history:
  - {change: widget-label-rename, phase: expand}
  - {change: widget-label-rename, phase: contract}
  - {change: widget-title-rename, phase: expand}
`
		writeFile(t, filepath.Join(input.ProjectDir, "service.yaml"), service)
		api := &capturingGenerator{stubGenerator: stubGenerator{files: []gen.File{{Path: "internal/api/handler.go", Content: []byte("package api\n")}}}}
		input.Generators["stub-api"] = api
		if validation, err := Validate(input); err != nil || validation.HasErrors() {
			t.Fatalf("checking refused the declaration: %v %v", FormatValidation(validation), err)
		}
		if _, err := Reconcile(input); err != nil {
			t.Fatalf("generation refused a rename of a patchable field: %v", err)
		}
		ctx := api.capturedCtx
		if len(ctx.Collections) != 1 || !slices.Equal(ctx.Collections[0].Patchable, []string{"label", "note"}) {
			t.Fatalf("the generator got the collections %+v; want widgets with the patchable fields label, the API name, and note", ctx.Collections)
		}
		if len(ctx.Entities) != 1 || ctx.Entities[0].Fields[0].Name != "label" || ctx.Entities[0].Fields[0].ColumnName() != "heading" {
			t.Fatalf("the generator got the entities %+v; want Widget.label in the column heading", ctx.Entities)
		}
		data, err := os.ReadFile(filepath.Join(input.ProjectDir, "service.yaml"))
		if err != nil || string(data) != service {
			t.Fatalf("generation changed the declaration: %v", err)
		}
	})
}

func TestRetypedPatchableField(t *testing.T) {
	t.Run("ST-GEN-UC12-R04-T04 A collection patches a field that an open retype from int32 to int64 widens, and its API holds the int32 bounds until the contract", func(t *testing.T) {
		// Walkthrough 26, P3-RT: Item.lead_days, an int32 with min 0 and no max that the collection names as
		// patchable, becomes an int64 with min 0 and no max. Generation refused it, as the patchable list names it.
		minimum := 0.0
		svc := &types.ServiceDeclaration{
			Entities: []types.Entity{{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString},
				{Name: "lead_days", Type: types.FieldTypeInt64, Min: &minimum}}}},
			Collections: []types.Collection{{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpPatch}, Patchable: []string{"name", "lead_days"}}},
			SchemaChanges: []types.SchemaChange{{ID: "item-lead-days-retype", Kind: "retype", Entity: "Item", Field: "lead_days", From: "int32",
				Definition: "int32, min 0", Conversion: "integer_to_int64", InverseConversion: "int64_to_integer"}},
		}
		for _, contract := range []bool{false, true} {
			svc.SchemaHistory = []types.SchemaStep{{Change: "item-lead-days-retype", Phase: "expand"}}
			if contract {
				svc.SchemaHistory = append(svc.SchemaHistory, types.SchemaStep{Change: "item-lead-days-retype", Phase: "contract"})
			}
			live, _, err := storedEntities(svc, svc.SchemaChanges)
			if err != nil {
				t.Fatalf("generation refused a retype of a patchable field (contract %t): %v", contract, err)
			}
			field := live[0].Fields[1]
			if field.Name != "lead_days" || field.ColumnName() != "lead_days_int64" || field.Max != nil || field.Min == nil || *field.Min != 0 {
				t.Fatalf("the generator gets the field %+v; want lead_days in the column lead_days_int64 with min 0 and no max", field)
			}
			held := field.HeldMax != nil && *field.HeldMax == 2147483647 && field.HeldMin == nil
			if held == contract {
				t.Fatalf("the field lead_days holds the bounds %v and %v with the contract step %t; want at most 2147483647 only until the contract", field.HeldMin, field.HeldMax, contract)
			}
		}
	})
}

func TestRetypedPatchableNarrowing(t *testing.T) {
	t.Run("ST-GEN-UC12-R04-T05 A collection patches a field that a retype which is not a widening changes, and the API holds no int32 bound", func(t *testing.T) {
		// Security review of PR #499, I2: the relaxation that lets a patchable field be retyped holds for each
		// retype, not only for a widening from int32 (walkthrough 26, P3-RT). A retype from string to int64 and a
		// narrowing from int64 to int32 keep the field patchable, in the column of its new type, with no held bound.
		hundred := 100.0
		for _, c := range []struct {
			name, from, definition, conversion, inverse string
			field                                       types.Field
			column                                      string
		}{
			{"string to int64", "string", "string, pattern ^[0-9]{1,9}$", "text_to_int64", "int64_to_text",
				types.Field{Name: "lead_days", Type: types.FieldTypeInt64}, "lead_days_int64"},
			{"int64 to int32", "int64", "int64, max 100", "int64_to_integer", "integer_to_int64",
				types.Field{Name: "lead_days", Type: types.FieldTypeInt32, Max: &hundred}, "lead_days_int32"},
		} {
			svc := &types.ServiceDeclaration{
				Entities: []types.Entity{{Name: "Item", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}, c.field}}},
				Collections: []types.Collection{{Name: "items", Entity: "Item", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpPatch},
					Patchable: []string{"name", "lead_days"}}},
				SchemaChanges: []types.SchemaChange{{ID: "item-lead-days-retype", Kind: "retype", Entity: "Item", Field: "lead_days", From: c.from,
					Definition: c.definition, Conversion: c.conversion, InverseConversion: c.inverse}},
				SchemaHistory: []types.SchemaStep{{Change: "item-lead-days-retype", Phase: "expand"}},
			}
			live, _, err := storedEntities(svc, svc.SchemaChanges)
			if err != nil {
				t.Fatalf("%s: generation refused a retype of a patchable field: %v", c.name, err)
			}
			field := live[0].Fields[1]
			if field.Name != "lead_days" || field.ColumnName() != c.column || field.HeldMin != nil || field.HeldMax != nil {
				t.Fatalf("%s: the generator gets the field %+v; want lead_days in the column %s with no held bound", c.name, field, c.column)
			}
		}
	})
}
