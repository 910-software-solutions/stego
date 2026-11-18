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
		base := []byte(td01)
		if check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, td01)); err != nil || check.HasFaults() {
			t.Fatalf("the same declaration is refused: %v %+v", err, check.Faults)
		}
	})
}
