package declaration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// databaseConversions reads the conversions that the postgres-adapter component of the common registry declares.
func databaseConversions(t *testing.T) map[string]types.Conversion {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "registry", "components", "postgres-adapter", "component.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		Conversions []types.Conversion `yaml:"conversions"`
	}
	if err := yaml.Unmarshal(data, &component); err != nil {
		t.Fatal(err)
	}
	out := map[string]types.Conversion{}
	for _, c := range component.Conversions {
		out[c.Name] = c
	}
	return out
}

// priorityRetype is ST-DECL-TD02's Todo with priority retyped from string to the type typ with the pattern after
// the change, by the conversion and the inverse conversion, with the Definition of the field before the change and
// the fill value.
func priorityRetype(typ types.FieldType, after, conversion, inverse, definition string, fill any) *types.ServiceDeclaration {
	return &types.ServiceDeclaration{
		Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "priority", Type: typ, Pattern: after}}}},
		SchemaChanges: []types.SchemaChange{{ID: "todo-priority-retype", Kind: "retype", Entity: "Todo", Field: "priority", From: "string",
			Conversion: conversion, InverseConversion: inverse, Definition: definition, FillValue: fill}},
		SchemaHistory: []types.SchemaStep{{Change: "todo-priority-retype", Phase: "expand"}},
	}
}

func TestSchemaConversions(t *testing.T) {
	conversions := databaseConversions(t)
	// The declaration format has no integer type, so the integer of the test record is an int32.
	for _, c := range []struct {
		name, refused string
		service       *types.ServiceDeclaration
	}{
		{"a conversion that is total under the pattern of the field before the change", "",
			priorityRetype(types.FieldTypeInt32, "", "text_to_integer", "integer_to_text", "string, pattern ^[0-9]{1,9}$", nil)},
		{"the same field with no pattern", "text_to_integer is not total",
			priorityRetype(types.FieldTypeInt32, "", "text_to_integer", "integer_to_text", "", nil)},
		{"no pattern and a fill value 0", "",
			priorityRetype(types.FieldTypeInt32, "", "text_to_integer", "integer_to_text", "", 0)},
		{"an inverse conversion that is not total over the field after the change", "inverse conversion text_to_integer is not total",
			&types.ServiceDeclaration{
				Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "priority", Type: types.FieldTypeString}}}},
				SchemaChanges: []types.SchemaChange{{ID: "todo-priority-retype", Kind: "retype", Entity: "Todo", Field: "priority", From: "int32",
					Conversion: "integer_to_text", InverseConversion: "text_to_integer"}},
				SchemaHistory: []types.SchemaStep{{Change: "todo-priority-retype", Phase: "expand"}},
			}},
	} {
		t.Run("ST-DECL-INV63-T "+c.name, func(t *testing.T) {
			faults := ValidateSchemaConversions(c.service, conversions, true)
			if c.refused == "" {
				if len(faults) != 0 {
					t.Errorf("checking refuses the retype: %v", faults)
				}
				return
			}
			if len(faults) != 1 || !strings.Contains(faults[0].Message, c.refused) || !strings.Contains(faults[0].Message, "todo-priority-retype") || faults[0].Rule != "schema-change-total" {
				t.Errorf("checking gives %v, want one fault of the rule schema-change-total that names the change and says %q", faults, c.refused)
			}
		})
	}
	f := func(v float64) *float64 { return &v }
	n := func(v int) *int { return &v }
	bounded := func(before string, after types.Field) *types.ServiceDeclaration {
		s := priorityRetype(after.Type, after.Pattern, "", "", before, nil)
		s.Entities[0].Fields[1] = after
		s.SchemaChanges[0].From = strings.SplitN(before, ",", 2)[0]
		return s
	}
	for _, c := range []struct {
		name, refused, conversion, inverse string
		service                            *types.ServiceDeclaration
	}{
		{"an int64 of at most 1000000 to an int32 of at most 100, whose CHECK refuses a converted 500", "bounds min and max", "int64_to_integer", "integer_to_int64",
			bounded("int64, min 0, max 1000000", types.Field{Name: "priority", Type: types.FieldTypeInt32, Min: f(0), Max: f(100)})},
		{"an int32 of at most 100 to an int64 of at most 1000000, whose back value breaks the CHECK of the old column", "bounds min and max", "integer_to_int64", "int64_to_integer",
			&types.ServiceDeclaration{Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "priority", Type: types.FieldTypeInt64, Min: f(-2147483648), Max: f(1000000)}}}},
				SchemaChanges: []types.SchemaChange{{ID: "todo-priority-retype", Kind: "retype", Entity: "Todo", Field: "priority", From: "int32", Definition: "int32, min -2147483648, max 100", Conversion: "integer_to_int64", InverseConversion: "int64_to_integer"}},
				SchemaHistory: []types.SchemaStep{{Change: "todo-priority-retype", Phase: "expand"}}}},
		{"a text of at least 3 characters to an int64, whose back value 5 breaks the length of the old column", "min_length", "text_to_int64", "int64_to_text",
			bounded("string, min_length 3, pattern ^[0-9]{1,9}$", types.Field{Name: "priority", Type: types.FieldTypeInt64})},
		{"an int64 of at least 0 to a text, whose back value -5 breaks the CHECK of the old column", "a min, a max", "int64_to_text", "text_to_int64",
			bounded("int64, min 0", types.Field{Name: "priority", Type: types.FieldTypeString, Pattern: "^-?[0-9]{1,18}$"})},
		{"a text to an int64 with a max, whose converted value breaks the CHECK of the new column", "a min, a max", "text_to_int64", "int64_to_text",
			bounded("string, pattern ^[0-9]{1,9}$", types.Field{Name: "priority", Type: types.FieldTypeInt64, Max: f(10)})},
		{"a text with a max_length after an int64", "max_length", "int64_to_text", "text_to_int64",
			bounded("int64", types.Field{Name: "priority", Type: types.FieldTypeString, MaxLength: n(5), Pattern: "^-?[0-9]{1,18}$"})},
		{"an int64 to an int32 with the same bounds", "", "int64_to_integer", "integer_to_int64",
			bounded("int64, min -10, max 10", types.Field{Name: "priority", Type: types.FieldTypeInt32, Min: f(-10), Max: f(10)})},
	} {
		t.Run("ST-DECL-INV63-T "+c.name, func(t *testing.T) {
			c.service.SchemaChanges[0].Conversion, c.service.SchemaChanges[0].InverseConversion = c.conversion, c.inverse
			faults := ValidateSchemaConversions(c.service, conversions, true)
			if c.refused == "" {
				if len(faults) != 0 {
					t.Errorf("checking refuses the retype: %v", faults)
				}
				return
			}
			if len(faults) != 1 || !strings.Contains(faults[0].Message, c.refused) || faults[0].Rule != "schema-change-total" {
				t.Errorf("checking gives %v, want one fault of the rule schema-change-total that says %q", faults, c.refused)
			}
		})
	}
	t.Run("ST-DECL-INV63-T a retype of two integer types with no definition names the definition to give", func(t *testing.T) {
		// The walkthrough 7 case K12: Line.discount_pct, an int32 of 0 to 100, becomes an int64 of 0 to 100, and the
		// change leaves its definition out.
		service := &types.ServiceDeclaration{Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "priority", Type: types.FieldTypeInt64, Min: f(0), Max: f(100)}}}},
			SchemaChanges: []types.SchemaChange{{ID: "todo-priority-retype", Kind: "retype", Entity: "Todo", Field: "priority", From: "int32", Conversion: "integer_to_int64", InverseConversion: "int64_to_integer"}},
			SchemaHistory: []types.SchemaStep{{Change: "todo-priority-retype", Phase: "expand"}}}
		faults := ValidateSchemaConversions(service, conversions, true)
		if len(faults) != 1 || !strings.Contains(faults[0].Message, "the retype of Todo.priority gives no definition, so the field before the change is taken as int32 with no min and no max, and the field after the change has min 0 and max 100") ||
			!strings.Contains(faults[0].Message, `give the field before the change in definition, such as definition: "int32, min 0, max 100"`) || faults[0].Path != "schema_changes[0].definition" {
			t.Fatalf("checking gives %v, want one fault at the definition that names the definition to give", faults)
		}
		// The fault does not say that the bounds differ, which they do not in the declaration (walkthrough 8 and 9,
		// B9).
		if strings.Contains(faults[0].Message, "bounds min and max") {
			t.Fatalf("the fault of a missing definition speaks of the bounds: %s", faults[0].Message)
		}
		// With the definition, the retype passes.
		service.SchemaChanges[0].Definition = "int32, min 0, max 100"
		if faults := ValidateSchemaConversions(service, conversions, true); len(faults) != 0 {
			t.Fatalf("checking refuses the retype with its definition: %v", faults)
		}
		// A retype that gives a definition with other bounds is not told to give one.
		service.SchemaChanges[0].Definition = "int32, min 0, max 50"
		if faults := ValidateSchemaConversions(service, conversions, true); len(faults) != 1 || strings.Contains(faults[0].Message, "no definition") {
			t.Fatalf("checking gives %v, want one fault on the bounds that names no missing definition", faults)
		}
	})
	for _, c := range []struct{ name, conversion, inverse, want string }{
		{"a conversion that the database component does not declare", "text_to_money", "integer_to_text", "text_to_money is not one that the database component declares from string to int32"},
		{"a conversion of other types", "text_to_int64", "integer_to_text", "text_to_int64 is not one that the database component declares from string to int32"},
		{"an inverse conversion of other types", "text_to_integer", "int64_to_text", "inverse conversion int64_to_text is not one"},
	} {
		t.Run("ST-DECL-INV62-T "+c.name, func(t *testing.T) {
			faults := ValidateSchemaConversions(priorityRetype(types.FieldTypeInt32, "", c.conversion, c.inverse, "", 0), conversions, true)
			if len(faults) != 1 || !strings.Contains(faults[0].Message, c.want) || faults[0].Rule != "schema-change-value" {
				t.Errorf("checking gives %v, want one fault of the rule schema-change-value that says %q", faults, c.want)
			}
		})
	}
	t.Run("ST-DECL-INV62-T a retype with no database component that declares conversions", func(t *testing.T) {
		if faults := ValidateSchemaConversions(priorityRetype(types.FieldTypeInt32, "", "text_to_integer", "integer_to_text", "", 0), nil, false); len(faults) != 1 {
			t.Errorf("checking gives %v, want one fault", faults)
		}
	})
}

// A chain of retypes is checked step by step: each retype from its From to the type that its own step gives the
// field, with each later step undone (decision on ST-DECL-Q17 (a), orchestrator under the owner's delegation,
// 2026-11-14).
func TestWideningRetype(t *testing.T) {
	conversions := databaseConversions(t)
	f := func(v float64) *float64 { return &v }
	t.Run("ST-DECL-INV63-T a widening from int32 to int64 with min 0 and no max is accepted, and a narrowing with no max is not", func(t *testing.T) {
		// Walkthrough 26, P3-RT: Item.lead_days, an int32 with min 0 and no max, becomes an int64 with min 0 and no
		// max. The int32 column holds the one side, and the API of the release of the change holds the other at
		// most 2147483647 while the change is open, so the inverse conversion is total and the bounds agree.
		widen := func(definition string, min, max *float64) *types.ServiceDeclaration {
			return &types.ServiceDeclaration{Entities: []types.Entity{{Name: "Item", Fields: []types.Field{{Name: "lead_days", Type: types.FieldTypeInt64, Min: min, Max: max}}}},
				SchemaChanges: []types.SchemaChange{{ID: "item-lead-days-retype", Kind: "retype", Entity: "Item", Field: "lead_days", From: "int32", Definition: definition,
					Conversion: "integer_to_int64", InverseConversion: "int64_to_integer"}},
				SchemaHistory: []types.SchemaStep{{Change: "item-lead-days-retype", Phase: "expand"}}}
		}
		for name, service := range map[string]*types.ServiceDeclaration{
			"min 0 and no max":                       widen("int32, min 0, default 0", f(0), nil),
			"no bounds":                              widen("int32", nil, nil),
			"max 2147483647 on both sides":           widen("int32, min 0, max 2147483647", f(0), f(2147483647)),
			"max 2147483647 before and none after":   widen("int32, min 0, max 2147483647", f(0), nil),
			"no max before and 2147483647 after":     widen("int32, min 0", f(0), f(2147483647)),
			"a max above the int32 range after":      widen("int32, min 0", f(0), f(9000000000)),
			"the int32 bounds before and none after": widen("int32, min -2147483648, max 2147483647", nil, nil),
		} {
			if faults := ValidateSchemaConversions(service, conversions, true); len(faults) != 0 {
				t.Errorf("%s: checking refuses the widening: %v", name, faults)
			}
		}
		// Bounds that differ within the int32 range still break the CHECK of one column.
		if faults := ValidateSchemaConversions(widen("int32, min 0", f(1), nil), conversions, true); len(faults) != 1 || !strings.Contains(faults[0].Message, "bounds min and max") {
			t.Errorf("a widening with min 0 before and min 1 after gives %v, want the fault of the bounds", faults)
		}
		if faults := ValidateSchemaConversions(widen("int32, min 0, max 100", f(0), nil), conversions, true); len(faults) != 1 || !strings.Contains(faults[0].Message, "bounds min and max") {
			t.Errorf("a widening with max 100 before and no max after gives %v, want the fault of the bounds", faults)
		}
		// A narrowing from int64 to int32 is not held: the release before writes the whole int64 range.
		narrow := &types.ServiceDeclaration{Entities: []types.Entity{{Name: "Item", Fields: []types.Field{{Name: "lead_days", Type: types.FieldTypeInt32, Min: f(0)}}}},
			SchemaChanges: []types.SchemaChange{{ID: "item-lead-days-retype", Kind: "retype", Entity: "Item", Field: "lead_days", From: "int64", Definition: "int64, min 0",
				Conversion: "int64_to_integer", InverseConversion: "integer_to_int64"}},
			SchemaHistory: []types.SchemaStep{{Change: "item-lead-days-retype", Phase: "expand"}}}
		if faults := ValidateSchemaConversions(narrow, conversions, true); len(faults) == 0 || !strings.Contains(faults[0].Message, "int64_to_integer is not total") {
			t.Errorf("a narrowing with no max gives %v, want the conversion that is not total", faults)
		}
	})
}

func TestSchemaConversionChain(t *testing.T) {
	conversions := databaseConversions(t)
	first := types.SchemaChange{ID: "todo-priority-retype", Kind: "retype", Entity: "Todo", Field: "priority", From: "int32", Conversion: "integer_to_text", InverseConversion: "text_to_integer"}
	second := types.SchemaChange{ID: "todo-priority-retype-2", Kind: "retype", Entity: "Todo", Field: "priority", From: "string", Definition: "string, pattern ^-?[0-9]{1,9}$",
		Conversion: "text_to_int64", InverseConversion: "int64_to_text"}
	chain := func(first, second types.SchemaChange, head types.Field) *types.ServiceDeclaration {
		return &types.ServiceDeclaration{
			Entities:      []types.Entity{{Name: "Todo", Fields: []types.Field{head}}},
			SchemaChanges: []types.SchemaChange{first, second},
			SchemaHistory: []types.SchemaStep{{Change: first.ID, Phase: "expand"}, {Change: first.ID, Phase: "contract"}, {Change: second.ID, Phase: "expand"}},
		}
	}
	int64Field := types.Field{Name: "priority", Type: types.FieldTypeInt64}
	t.Run("ST-DECL-INV63-T Two retypes, int32 to string and then string to int64, are each checked at their own step and accepted", func(t *testing.T) {
		if faults := ValidateSchemaConversions(chain(first, second, int64Field), conversions, true); len(faults) != 0 {
			t.Errorf("checking refuses the chain: %v", faults)
		}
	})
	t.Run("ST-DECL-INV63-T A first retype whose conversions go to the declared type and not to the type at its step is refused, naming the step and the shape", func(t *testing.T) {
		wrong := first
		wrong.Conversion, wrong.InverseConversion = "integer_to_int64", "int64_to_integer"
		faults := ValidateSchemaConversions(chain(wrong, second, int64Field), conversions, true)
		want := "integer_to_int64 is not one that the database component declares from int32 to string (the field after the step schema_history[0] is Todo.priority: string, pattern ^-?[0-9]{1,9}$, with each later step undone)"
		if len(faults) != 2 || !strings.Contains(faults[0].Message, want) || !strings.Contains(faults[1].Message, "int64_to_integer is not one") {
			t.Errorf("checking gives %v, want the conversion and the inverse conversion of todo-priority-retype refused, the first saying %q", faults, want)
		}
	})
	t.Run("ST-DECL-INV63-T A middle step whose From is not the type that the step before gives refuses the step before, at the shape that the middle step leaves", func(t *testing.T) {
		wrong := second
		wrong.From, wrong.Definition, wrong.Conversion, wrong.InverseConversion = "int32", "", "integer_to_int64", "int64_to_integer"
		faults := ValidateSchemaConversions(chain(first, wrong, types.Field{Name: "priority", Type: types.FieldTypeInt64}), conversions, true)
		named := false
		for _, fault := range faults {
			named = named || strings.Contains(fault.Message, "integer_to_text is not one that the database component declares from int32 to int32")
		}
		if !named || !strings.Contains(faults[0].Message, `"todo-priority-retype"`) {
			t.Errorf("checking gives %v, want todo-priority-retype refused from int32 to int32, the shape that the middle step leaves", faults)
		}
	})
}
