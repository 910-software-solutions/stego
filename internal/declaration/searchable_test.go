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
	})
}
