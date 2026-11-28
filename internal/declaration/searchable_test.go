package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/parser"
)

// searchableFaults reads ST-DECL-TD02 with the fields given and gives its searchable faults.
func searchableFaults(t *testing.T, fields ...string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      "+strings.Join(fields, "\n      "), 1)
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "searchable" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestValidateSearchable(t *testing.T) {
	t.Run("ST-DECL-INV109-T A searchable setting that a field cannot hold is refused with its path, and searchable false is taken on each string field", func(t *testing.T) {
		title := "- {name: title, type: string}"
		refused := []struct {
			name, found, field, says string
			fields                   []string
		}{
			{"searchable true on a max_length of 501", "true", "notes", "max_length 501",
				[]string{title, "- {name: notes, type: string, max_length: 501, searchable: true}"}},
			{"searchable true on a max_length of 4000", "true", "notes", "2000 bytes",
				[]string{title, "- {name: notes, type: string, optional: true, max_length: 4000, searchable: true}"}},
			// 2^62 characters: a product with the 4 bytes of a character overflows to 0 (security review of PR #22, I1).
			{"searchable true on a max_length of 2^62", "true", "notes", "max_length 4611686018427387904",
				[]string{title, "- {name: notes, type: string, optional: true, max_length: 4611686018427387904, searchable: true}"}},
			{"searchable on an int32", "false", "rank", "string field",
				[]string{title, "- {name: rank, type: int32, searchable: false}"}},
			{"searchable on an enum", "true", "state", "string field",
				[]string{title, "- {name: state, type: enum, values: [open, closed], searchable: true}"}},
			{"searchable true on a pii field", "true", "email", "pii",
				[]string{title, "- {name: email, type: string, max_length: 255, sensitivity: pii, searchable: true}"}},
		}
		for _, c := range refused {
			faults := searchableFaults(t, c.fields...)
			if len(faults) != 1 || faults[0].Path != "entities[0].fields[1].searchable" || faults[0].Found != c.found ||
				!strings.Contains(faults[0].Message, `"`+c.field+`"`) || !strings.Contains(faults[0].Message, c.says) {
				t.Errorf("%s: got %+v, want one searchable fault at entities[0].fields[1].searchable that names %s and says %q", c.name, faults, c.field, c.says)
			}
		}
		// The controls: searchable false on a short and on a long string, searchable true at the bound of 500
		// characters and with no max_length, and no searchable setting on a long string.
		for _, fields := range [][]string{
			{title, "- {name: code, type: string, max_length: 40, searchable: false}"},
			{title, "- {name: notes, type: string, optional: true, max_length: 4000, searchable: false}"},
			{title, "- {name: notes, type: string, optional: true, max_length: 500, searchable: true}"},
			{"- {name: title, type: string, searchable: true}"},
			{title, "- {name: notes, type: string, optional: true, max_length: 4000}"},
			{title, "- {name: secret_notes, type: string, optional: true, max_length: 4000, sensitivity: secret, searchable: false}"},
		} {
			if faults := searchableFaults(t, fields...); len(faults) != 0 {
				t.Errorf("%v: got %+v, want no searchable fault", fields, faults)
			}
		}
		// A dropped entity is checked too, and takes searchable: true on long text, which keeps the search index that
		// a release before postgres-adapter 6.34.0 built (security review of PR #22, M1).
		dropped := func(field string) []ValidationError {
			data := td02 + "  - name: Memo\n    dropped_by: memo-drop\n    fields:\n      - " + field + "\n" +
				"schema_changes:\n  - {id: memo-drop, kind: drop, entity: Memo}\nschema_history:\n  - {change: memo-drop, phase: expand}\n"
			data = strings.Replace(data, "collections:\n", "", 1)
			data = strings.Replace(data, "  todos:\n    entity: Todo\n    operations: [create, read, update, delete, list]\n", "", 1)
			data = strings.Replace(data, "schema_changes:", "collections:\n  todos:\n    entity: Todo\n    operations: [create, read, update, delete, list]\nschema_changes:", 1)
			service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
			if err != nil || len(undeclared) != 0 {
				t.Fatalf("read the declaration: %v %v", err, undeclared)
			}
			var faults []ValidationError
			for _, fault := range Validate(service).All() {
				if fault.Rule == "searchable" {
					faults = append(faults, fault)
				}
			}
			return faults
		}
		if faults := dropped("{name: body, type: string, optional: true, max_length: 4000, searchable: true}"); len(faults) != 0 {
			t.Errorf("searchable true on long text of a dropped entity: got %+v, want no fault", faults)
		}
		for field, says := range map[string]string{
			"{name: body, type: string, optional: true, max_length: 4000, sensitivity: pii, searchable: true}": "pii",
			"{name: rank, type: int32, optional: true, searchable: true}":                                      "string field",
		} {
			if faults := dropped(field); len(faults) != 1 || faults[0].Path != "entities.Memo.fields[0].searchable" || !strings.Contains(faults[0].Message, says) {
				t.Errorf("%s on a dropped entity: got %+v, want one searchable fault at entities.Memo.fields[0].searchable that says %q", field, faults, says)
			}
		}
		// The item searchable of the definition of a drop is only for a public long text field.
		drop := func(definition string) []ValidationError {
			return schemaFaults(t, todoFields([]string{"{name: title, type: string, min_length: 1, max_length: 255}"}, "",
				[]string{"{id: todo-notes-drop, kind: drop, entity: Todo, field: notes, definition: \"" + definition + "\"}"}, "todo-notes-drop expand"))
		}
		if faults := drop("string, optional, max_length 4000, after title, searchable"); len(faults) != 0 {
			t.Errorf("a drop with the item searchable of long text: got %+v, want no fault", faults)
		}
		for definition, says := range map[string]string{
			"string, optional, max_length 200, after title, searchable":       "max_length over 500",
			"string, optional, max_length 4000, pii, after title, searchable": "pii",
			"int32, optional, after title, searchable":                        "only for a string field",
			"string, optional, max_length 4000, after title, searchable yes":  "takes no value",
		} {
			faults := drop(definition)
			if len(faults) != 1 || !strings.Contains(faults[0].Message, says) {
				t.Errorf("a drop with the definition %q: got %+v, want one fault that says %q", definition, faults, says)
			}
		}
	})
}
