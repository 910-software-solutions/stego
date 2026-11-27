package declaration

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/910-software-solutions/stego/internal/types"
)

// authorSuppliedSchemas gives the config schemas of two active components, decoded through the yaml tags of the
// registry's config schema, so the mark is read as a registry reads it: grpc-application marks proto_files as
// author supplied, and postgres-adapter marks none of its settings, also not connection_string, which only the
// running service gets.
func authorSuppliedSchemas(t *testing.T) map[string]map[string]types.ConfigField {
	t.Helper()
	schemas := map[string]map[string]types.ConfigField{}
	for name, config := range map[string]string{
		"grpc-application": "proto_files:\n  type: list\n  author_supplied: true\n  items:\n    path: {type: string}\nreflection:\n  type: bool\n  optional: true\n",
		"postgres-adapter": "schema_generation:\n  type: string\nconnection_string:\n  type: string\n  secret: true\n",
	} {
		var schema map[string]types.ConfigField
		if err := yaml.Unmarshal([]byte(config), &schema); err != nil {
			t.Fatal(err)
		}
		schemas[name] = schema
	}
	return schemas
}

func TestAuthorSuppliedSettings(t *testing.T) {
	t.Run("ST-DECL-INV90-T A declaration that does not give an author-supplied setting is refused by name, and one that gives it is accepted", func(t *testing.T) {
		schemas := authorSuppliedSchemas(t)
		if got, want := AuthorSuppliedSettings(schemas), []string{"overrides.grpc-application.proto_files"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("author-supplied settings %v, want %v", got, want)
		}
		for name, overrides := range map[string]map[string]any{
			"no overrides":            nil,
			"no grpc-application":     {"postgres-adapter": map[string]any{"schema_generation": "tables-v1"}},
			"other settings only":     {"grpc-application": map[string]any{"reflection": true}},
			"a setting with no value": {"grpc-application": map[string]any{"proto_files": nil}},
			"an empty text":           {"grpc-application": map[string]any{"proto_files": ""}},
			"an empty list":           {"grpc-application": map[string]any{"proto_files": []any{}}},
			"an empty map":            {"grpc-application": map[string]any{"proto_files": map[string]any{}}},
		} {
			faults := ValidateAuthorSupplied(&types.ServiceDeclaration{Overrides: overrides}, schemas)
			if len(faults) != 1 || faults[0].Rule != "author-supplied" || faults[0].Path != "overrides.grpc-application.proto_files" {
				t.Errorf("%s: want one author-supplied fault at overrides.grpc-application.proto_files, got %+v", name, faults)
			}
		}
		given := map[string]any{"grpc-application": map[string]any{"proto_files": []any{map[string]any{"path": "api.proto"}}}}
		if faults := ValidateAuthorSupplied(&types.ServiceDeclaration{Overrides: given}, schemas); len(faults) != 0 {
			t.Errorf("a declaration that gives proto_files is refused: %+v", faults)
		}
		delete(schemas, "grpc-application")
		if faults := ValidateAuthorSupplied(&types.ServiceDeclaration{}, schemas); len(faults) != 0 {
			t.Errorf("a setting of a component that is not active is asked for: %+v", faults)
		}
	})
}
