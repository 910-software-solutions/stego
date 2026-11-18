package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
)

// secretFaults reads the declaration of ST-DECL-TD02 with the fields of Todo and the declaration tail given, and
// gives the faults of the rule secret-field (ST-DECL-INV101).
func secretFaults(t *testing.T, tail string, fields ...string) []ValidationError {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      "+strings.Join(fields, "\n      "), 1) + tail
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "secret-field" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestSecretFields(t *testing.T) {
	const title = "- {name: title, type: string}"
	t.Run("ST-DECL-INV101-T A secret field that is not a string, or that is a key, an index, a reference or a default, is refused", func(t *testing.T) {
		const secret = "s3cr3t-default-0042"
		refused := []struct {
			name, tail string
			fields     []string
		}{
			{"an int32 secret", "", []string{title, "- {name: token, type: int32, sensitivity: secret}"}},
			{"a bytes secret", "", []string{title, "- {name: token, type: bytes, sensitivity: secret}"}},
			{"a compare-only jsonb secret", "", []string{title, "- {name: token, type: jsonb, sensitivity: secret, compare_only: true}"}},
			{"a unique secret", "", []string{title, "- {name: token, type: string, sensitivity: secret, unique: true}"}},
			{"a secret unique when live", "", []string{title, "- {name: token, type: string, sensitivity: secret, unique_when_live: true}"}},
			{"a secret with a unique_composite", "", []string{title, "- {name: token, type: string, sensitivity: secret, unique_composite: [title]}"}},
			{"a secret in the unique_composite of another field", "", []string{"- {name: title, type: string, unique_composite: [token]}", "- {name: token, type: string, sensitivity: secret}"}},
			{"a computed secret", "", []string{title, "- {name: token, type: string, sensitivity: secret, computed: true}"}},
			{"a secret with a default", "", []string{title, "- {name: token, type: string, sensitivity: secret, default: " + secret + "}"}},
			{"compare_only on a public field", "", []string{title, "- {name: token, type: string, compare_only: true}"}},
			{"a rename of a secret field", "schema_changes:\n  - {id: token-rename, kind: rename, entity: Todo, field: token, from: old_token}\nschema_history:\n  - {change: token-rename, phase: expand}\n", []string{title, "- {name: token, type: string, sensitivity: secret}"}},
			{"a retype of a secret field", "schema_changes:\n  - {id: token-retype, kind: retype, entity: Todo, field: token, from: jsonb, conversion: jsonb_to_text, inverse_conversion: text_to_jsonb}\nschema_history:\n  - {change: token-retype, phase: expand}\n", []string{title, "- {name: token, type: string, sensitivity: secret}"}},
			{"a declared unique invariant by a secret", "invariants:\n  - {name: one-token, kind: unique, on: Todo, by: [token]}\n", []string{title, "- {name: token, type: string, sensitivity: secret}"}},
		}
		for _, c := range refused {
			faults := secretFaults(t, c.tail, c.fields...)
			if len(faults) == 0 {
				t.Errorf("%s: want a secret-field fault, got none", c.name)
				continue
			}
			for _, fault := range faults {
				if !strings.Contains(fault.Message, "ST-DECL-INV101") || strings.Contains(fault.Message, secret) || !strings.Contains(fault.Message, `"token"`) {
					t.Errorf("%s: the fault does not name the field and the rule, or names a value: %s", c.name, fault.Message)
				}
			}
		}
	})
	t.Run("ST-DECL-INV101-T An implicit value of a secret field is refused", func(t *testing.T) {
		data := strings.Replace(strings.Replace(td02Fields, "%s", "      "+title+"\n      - {name: token, type: string, sensitivity: secret}", 1),
			"    operations: [create, read, update, delete, list]\n", "    operations: [create, read, update, delete, list]\n    implicit: {token: s3cr3t}\n", 1)
		service, _, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, fault := range validateSecretFields(service) {
			found = found || (strings.Contains(fault.Message, "implicit value") && !strings.Contains(fault.Message, "s3cr3t"))
		}
		if !found {
			t.Fatal("an implicit value of a secret field is not refused, or the fault names the value")
		}
	})
	t.Run("ST-DECL-INV101-T A readable and a compare-only secret string and a readable secret jsonb are accepted", func(t *testing.T) {
		if faults := secretFaults(t, "", title, "- {name: token, type: string, sensitivity: secret, optional: true, min_length: 8}",
			"- {name: pin, type: string, sensitivity: secret, compare_only: true, max_length: 64}",
			"- {name: vault, type: jsonb, sensitivity: secret, optional: true}"); len(faults) != 0 {
			t.Fatalf("want no secret-field fault, got %+v", faults)
		}
	})
}
