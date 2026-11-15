package declaration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
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
