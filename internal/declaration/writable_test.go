package declaration

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/parser"
)

// writableFaults reads ST-DECL-TD02 with the fields given and the collection todos with patch and the patchable list
// given, and gives its writable faults.
func writableFaults(t *testing.T, patchable string, fields ...string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      "+strings.Join(fields, "\n      "), 1)
	data = strings.Replace(data, "operations: [create, read, update, delete, list]", "operations: [create, read, update, delete, list, patch]\n    patchable: "+patchable, 1)
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "writable" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestValidateWritable(t *testing.T) {
	t.Run("ST-DECL-INV100-T A writable setting outside its values or that disagrees with the collections is refused with its path", func(t *testing.T) {
		title := "- {name: title, type: string}"
		refused := []struct {
			name, patchable, path, found, field string
			fields                              []string
			allowed                             bool
		}{
			{"a value that is not always, create or never", "[title]", "entities[0].fields[1].writable", "sometimes", "sku",
				[]string{title, "- {name: sku, type: string, writable: sometimes}"}, true},
			{"a value in upper case", "[title]", "entities[0].fields[1].writable", "Create", "sku",
				[]string{title, "- {name: sku, type: string, writable: Create}"}, true},
			{"a computed field with create", "[title]", "entities[0].fields[1].writable", "create", "total",
				[]string{title, "- {name: total, type: int64, computed: true, filled_by: summer, writable: create}"}, false},
			{"a server timestamp with never", "[title]", "entities[0].fields[1].writable", "never", "closed_at",
				[]string{title, "- {name: closed_at, type: timestamp, optional: true, writable: never}"}, false},
			{"a required never field with no default", "[title]", "entities[0].fields[1].writable", "never", "state",
				[]string{title, "- {name: state, type: enum, values: [open, closed], writable: never}"}, false},
			{"a patchable create field", "[title, sku]", "collections.todos.patchable[1]", "sku", "sku",
				[]string{title, "- {name: sku, type: string, writable: create}"}, false},
			{"a patchable never field", "[state, title]", "collections.todos.patchable[0]", "state", "state",
				[]string{title, "- {name: state, type: enum, values: [open, closed], default: open, writable: never}"}, false},
		}
		for _, c := range refused {
			faults := writableFaults(t, c.patchable, c.fields...)
			if len(faults) != 1 || faults[0].Path != c.path || faults[0].Found != c.found || (len(faults[0].Allowed) == 3) != c.allowed ||
				!strings.Contains(faults[0].Message, "writable") || !strings.Contains(faults[0].Message, `"`+c.field+`"`) {
				t.Errorf("%s: got %+v, want one writable fault at %s", c.name, faults, c.path)
			}
		}
		// A field gets one fault: a computed never field with no default that a patch names is refused at its setting.
		if faults := writableFaults(t, "[title, total]", title, "- {name: total, type: int64, computed: true, filled_by: summer, writable: never}"); len(faults) != 1 ||
			faults[0].Path != "entities[0].fields[1].writable" {
			t.Errorf("a computed never field in the patchable list: got %+v, want one fault at its setting", faults)
		}
		accepted := [][]string{
			{title, "- {name: sku, type: string, writable: create}", "- {name: state, type: enum, values: [open, closed], default: open, writable: never}",
				"- {name: note, type: string, optional: true, writable: never}", "- {name: due_at, type: timestamp, client_set: true, writable: create}",
				"- {name: label, type: string, writable: always}"},
			{title},
		}
		for _, fields := range accepted {
			if faults := writableFaults(t, "[title]", fields...); len(faults) != 0 {
				t.Errorf("%v got writable faults: %+v", fields, faults)
			}
		}
		if faults := writableFaults(t, "[title, label]", title, "- {name: label, type: string, writable: always}"); len(faults) != 0 {
			t.Errorf("a patchable always field got writable faults: %+v", faults)
		}
	})
}
