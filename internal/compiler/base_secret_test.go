package compiler

import (
	"strings"
	"testing"
)

func TestBaseSecretForm(t *testing.T) {
	t.Run("ST-SUP-RC01-OC48-T02 A head that changes the secret form of a stored field is refused against the base with the seal step as the remedy", func(t *testing.T) {
		td01 := string(userManagementBase(t))
		const description = "{ name: description, type: string, optional: true, max_length: 1000 }"
		for _, c := range []struct{ name, base, head string }{
			{"a stored field made secret", description, "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }"},
			{"a secret made compare-only", "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }",
				"{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret, compare_only: true }"},
			{"a secret made public", "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }", description},
		} {
			base := []byte(strings.Replace(td01, description, c.base, 1))
			head := strings.Replace(td01, description, c.head, 1)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var found bool
			for _, fault := range check.Faults {
				found = found || (strings.Contains(fault.Message, "Organization.description") && strings.Contains(fault.Message, "kind seal"))
			}
			if !found {
				t.Errorf("%s: the faults %+v do not refuse the change with the seal step", c.name, check.Faults)
			}
		}
		// A retype of a compare-only secret to a public type is refused too, and a new required secret field gets
		// an optional add as its suggestion, never a require with a fill value (security re-review of PR #441, N2, N3).
		secretBase := []byte(strings.Replace(td01, description, "{ name: description, type: string, optional: true, sensitivity: secret, compare_only: true }", 1))
		retyped := withChanges(strings.Replace(td01, description, "{ name: description, type: bool, optional: true }", 1),
			"  - {id: organization-description-retype, kind: retype, entity: Organization, field: description, from: string, conversion: text_to_bool, inverse_conversion: bool_to_text}\n",
			"  - {change: organization-description-retype, phase: expand}\n")
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: secretBase, Record: recordFor(secretBase)}, parseHead(t, retyped))
		if err != nil {
			t.Fatal(err)
		}
		var retypeRefused bool
		for _, fault := range check.Faults {
			retypeRefused = retypeRefused || strings.Contains(fault.Message, "kind seal")
		}
		if !retypeRefused {
			t.Errorf("a retype of a compare-only secret to a public type is not refused: %+v", check.Faults)
		}
		required := strings.Replace(td01, description, description+"\n      - { name: api_key, type: string, sensitivity: secret }", 1)
		check, err = CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: []byte(td01), Record: recordFor([]byte(td01))}, parseHead(t, required))
		if err != nil {
			t.Fatal(err)
		}
		suggested := 0
		for _, fault := range check.Faults {
			for _, change := range fault.Declare {
				if change.Field != "api_key" {
					continue
				}
				suggested++
				if change.Kind != "add" || !strings.Contains(change.Needs, "optional") {
					t.Errorf("a new required secret field gets the suggestion %s (%s), want an optional add", change.Kind, change.Needs)
				}
			}
		}
		if suggested != 1 {
			t.Errorf("a new required secret field gets %d suggestions, want one add: %+v", suggested, check.Faults)
		}
		base := []byte(td01)
		if check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, td01)); err != nil || check.HasFaults() {
			t.Fatalf("the same declaration is refused: %v %+v", err, check.Faults)
		}
	})
}
