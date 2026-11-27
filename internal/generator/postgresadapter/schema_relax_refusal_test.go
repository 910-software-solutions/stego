package postgresadapter

import (
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

func TestRelaxRefusals(t *testing.T) {
	t.Run("ST-SUP-RC01-OC45-T02 The generator refuses a relax step that PostgreSQL cannot make in place, or that changes more than its column", func(t *testing.T) {
		n := func(v int) *int { return &v }
		f := func(v float64) *float64 { return &v }
		models := func(fields ...types.Field) []tableModel {
			tables, err := tableModels(gen.Context{OutputNamespace: "boardsrx", Tenancy: &gen.Tenancy{Key: "tenant_id"}, Entities: []types.Entity{{Name: "Board", Fields: fields}}})
			if err != nil {
				t.Fatal(err)
			}
			return tables
		}
		title := types.Field{Name: "title", Type: types.FieldTypeString, MaxLength: n(40)}
		change := types.SchemaChange{ID: "board-title-relax", Kind: "relax", Entity: "Board", Field: "title", Definition: "max_length 40"}
		with := func(edit func(*types.Field)) types.Field {
			field := title
			edit(&field)
			return field
		}
		other := types.Field{Name: "other", Type: types.FieldTypeString, Optional: true}
		for _, c := range []struct {
			name          string
			before, after []tableModel
		}{
			{"a shorter character varying", models(title), models(with(func(f *types.Field) { f.MaxLength = n(20) }))},
			{"character varying made text", models(title), models(with(func(f *types.Field) { f.MaxLength = nil }))},
			{"a retype that needs a rewrite", models(types.Field{Name: "title", Type: types.FieldTypeInt32}), models(types.Field{Name: "title", Type: types.FieldTypeInt64})},
			{"an optional field made required", models(with(func(f *types.Field) { f.Optional = true })), models(title)},
			{"a longer character varying with the field made required", models(with(func(f *types.Field) { f.Optional = true })), models(with(func(f *types.Field) { f.MaxLength = n(80) }))},
			{"a default changed", models(with(func(f *types.Field) { f.Default = "a" })), models(with(func(f *types.Field) { f.Default = "b" }))},
			{"a default taken away", models(with(func(f *types.Field) { f.Default = "a" })), models(title)},
			{"a length CHECK changed", models(with(func(f *types.Field) { f.MinLength = n(2) })), models(with(func(f *types.Field) { f.MinLength = n(1) }))},
			{"a numeric bound changed", models(types.Field{Name: "title", Type: types.FieldTypeInt32, Max: f(5)}), models(types.Field{Name: "title", Type: types.FieldTypeInt32, Max: f(9)})},
			{"an enum value taken away", models(types.Field{Name: "title", Type: types.FieldTypeEnum, Values: []string{"a", "b"}}), models(types.Field{Name: "title", Type: types.FieldTypeEnum, Values: []string{"a"}})},
			{"another column changed too", models(title, other), models(with(func(f *types.Field) { f.MaxLength = n(80) }), with(func(f *types.Field) { f.Name, f.MaxLength = "other", n(9) }))},
			{"another column added", models(title), models(with(func(f *types.Field) { f.MaxLength = n(80) }), other)},
			{"nothing changed", models(title), models(title)},
		} {
			if _, err := relaxSteps(gen.Context{}, 1, c.before, c.after, change); err == nil {
				t.Errorf("%s: relaxSteps gives no error", c.name)
			}
		}
		// The controls: a longer character varying, more enum values and an enum made a string are relax steps.
		for _, c := range []struct {
			name          string
			before, after []tableModel
		}{
			{"a longer character varying", models(title), models(with(func(f *types.Field) { f.MaxLength = n(80) }))},
			{"an enum value added", models(types.Field{Name: "title", Type: types.FieldTypeEnum, Values: []string{"a"}}), models(types.Field{Name: "title", Type: types.FieldTypeEnum, Values: []string{"a", "b"}})},
			{"an enum made a string", models(types.Field{Name: "title", Type: types.FieldTypeEnum, Values: []string{"a"}}), models(types.Field{Name: "title", Type: types.FieldTypeString})},
			{"a field made optional with a default", models(title), models(with(func(f *types.Field) { f.Optional, f.Default = true, "x" }))},
		} {
			if _, err := relaxSteps(gen.Context{}, 1, c.before, c.after, change); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	})
}
