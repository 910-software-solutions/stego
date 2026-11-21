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
			{"a require of a secret field with a fill value", "schema_changes:\n  - {id: token-require, kind: require, entity: Todo, field: token, fill_value: " + secret + "}\nschema_history:\n  - {change: token-require, phase: expand}\n", []string{title, "- {name: token, type: string, sensitivity: secret}"}},
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

// entityRuleFaults reads the declaration of ST-DECL-TD02 with the fields of Todo, the lines of Todo after its fields and the
// declaration tail given, and gives the faults of the rule.
func entityRuleFaults(t *testing.T, rule, entityTail, tail string, fields ...string) []ValidationError {
	t.Helper()
	body := "      " + strings.Join(fields, "\n      ")
	if entityTail != "" {
		body += "\n    " + entityTail
	}
	data := strings.Replace(td02Fields, "%s", body, 1) + tail
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == rule {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestAttemptsCounter(t *testing.T) {
	const title = "- {name: title, type: string}"
	pin := func(settings string) string {
		return "- {name: pin, type: string, " + settings + "}"
	}
	t.Run("ST-DECL-INV104-T A limit of wrong attempts on a compare-only secret is accepted", func(t *testing.T) {
		for _, limit := range []string{"1", "5", "100"} {
			if faults := entityRuleFaults(t, "attempts-limit", "", "", title, pin("sensitivity: secret, compare_only: true, attempts: {limit: "+limit+"}")); len(faults) != 0 {
				t.Fatalf("limit %s: want no fault, got %v", limit, faults)
			}
		}
	})
	refused := map[string]string{
		"a limit of 0":                  "sensitivity: secret, compare_only: true, attempts: {limit: 0}",
		"a limit of 101":                "sensitivity: secret, compare_only: true, attempts: {limit: 101}",
		"attempts on a readable secret": "sensitivity: secret, attempts: {limit: 5}",
		"attempts on a public field":    "attempts: {limit: 5}",
		"attempts on a pii field":       "sensitivity: pii, attempts: {limit: 5}",
	}
	for name, settings := range refused {
		t.Run("ST-DECL-INV104-T "+name+" is refused", func(t *testing.T) {
			faults := entityRuleFaults(t, "attempts-limit", "", "", title, pin(settings))
			if len(faults) == 0 {
				t.Fatal("want a fault of attempts-limit, got none")
			}
			for _, fault := range faults {
				if !strings.Contains(fault.Message, "ST-DECL-INV104") || !strings.Contains(fault.Message, `"pin"`) {
					t.Errorf("the fault does not name the field and the rule: %s", fault.Message)
				}
			}
		})
	}
}

func TestEventFields(t *testing.T) {
	fields := []string{"- {name: title, type: string}", "- {name: done, type: bool, default: false}",
		"- {name: email, type: string, sensitivity: pii}", "- {name: token, type: string, sensitivity: secret}"}
	const mixins = "mixins:\n  - durable-events\n  - kafka-events\n"
	accepted := map[string]string{
		"of title and done":                          "events: {fields: [title, done]}",
		"of title and email with a reason for email": "events: {fields: [title, email], version: 2, pii_reasons: {email: the ledger sends the receipt}}",
	}
	for name, events := range accepted {
		t.Run("ST-DECL-INV105-T Event fields "+name+" are accepted", func(t *testing.T) {
			if faults := entityRuleFaults(t, "event-fields", events, mixins, fields...); len(faults) != 0 {
				t.Fatalf("want no fault, got %v", faults)
			}
		})
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "title"
	}
	refused := []struct{ name, events, tail, names string }{
		{"with no field", "events: {fields: []}", mixins, "0"},
		{"with 65 fields", "events: {fields: [" + strings.Join(many, ", ") + "]}", mixins, "65"},
		{"of a field that Todo does not declare", "events: {fields: [colour]}", mixins, `"colour"`},
		{"of title twice", "events: {fields: [title, title]}", mixins, `"title"`},
		{"of the secret token", "events: {fields: [token]}", mixins, `"token"`},
		{"of email with no reason", "events: {fields: [email]}", mixins, `"email"`},
		{"with a reason for title", "events: {fields: [title], pii_reasons: {title: no reason}}", mixins, `"title"`},
		{"of the version 0", "events: {fields: [title], version: 0}", mixins, "version"},
		{"in a declaration that publishes no change events", "events: {fields: [title]}", "", "kafka-events"},
	}
	for _, c := range refused {
		t.Run("ST-DECL-INV105-T Event fields "+c.name+" are refused", func(t *testing.T) {
			faults := entityRuleFaults(t, "event-fields", c.events, c.tail, fields...)
			if len(faults) == 0 {
				t.Fatal("want a fault of event-fields, got none")
			}
			found := false
			for _, fault := range faults {
				if !strings.Contains(fault.Message, "ST-DECL-INV105") || !strings.Contains(fault.Message, `"Todo"`) {
					t.Errorf("the fault does not name Todo and the rule: %s", fault.Message)
				}
				found = found || strings.Contains(fault.Message, c.names) || strings.Contains(fault.Found, c.names)
			}
			if !found {
				t.Errorf("no fault names %s: %v", c.names, faults)
			}
		})
	}
}
