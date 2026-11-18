package compiler

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
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
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, SecretStorageVersion)}, parseHead(t, head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var found bool
			for _, fault := range check.Faults {
				named := strings.Contains(fault.Message, "Organization.description") && strings.Contains(fault.Message, "seal")
				if c.name == "a stored field made secret" {
					// The remedy of a field in clear made a readable secret is the seal step (ST-SUP-RC01-OC49).
					named = named && len(fault.Declare) == 1 && fault.Declare[0].Kind == "seal" && fault.Declare[0].Field == "description"
				}
				found = found || named
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
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: secretBase, Record: adapterRecord(t, secretBase, SecretStorageVersion)}, parseHead(t, retyped))
		if err != nil {
			t.Fatal(err)
		}
		var retypeRefused bool
		for _, fault := range check.Faults {
			retypeRefused = retypeRefused || strings.Contains(fault.Message, "secret form")
		}
		if !retypeRefused {
			t.Errorf("a retype of a compare-only secret to a public type is not refused: %+v", check.Faults)
		}
		required := strings.Replace(td01, description, description+"\n      - { name: api_key, type: string, sensitivity: secret }", 1)
		check, err = CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: []byte(td01), Record: adapterRecord(t, []byte(td01), SecretStorageVersion)}, parseHead(t, required))
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
		if check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, SecretStorageVersion)}, parseHead(t, td01)); err != nil || check.HasFaults() {
			t.Fatalf("the same declaration is refused: %v %+v", err, check.Faults)
		}
	})
}

func TestBaseSeal(t *testing.T) {
	t.Run("ST-SUP-RC01-OC49-T03 A head that seals a stored field with a seal change is accepted against the base at expand and at contract", func(t *testing.T) {
		td01 := string(userManagementBase(t))
		const description = "{ name: description, type: string, optional: true, max_length: 1000 }"
		const secret = "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }"
		change := "  - {id: organization-description-seal, kind: seal, entity: Organization, field: description}\n"
		expand := withChanges(strings.Replace(td01, description, secret, 1), change, "  - {change: organization-description-seal, phase: expand}\n")
		base := []byte(td01)
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, SecretStorageVersion)}, parseHead(t, expand))
		if err != nil || check.HasFaults() {
			t.Fatalf("the seal at expand is refused against the base in clear: %v %+v", err, check.Faults)
		}
		contract := withChanges(strings.Replace(td01, description, secret, 1), change,
			"  - {change: organization-description-seal, phase: expand}\n  - {change: organization-description-seal, phase: contract}\n")
		check, err = CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: []byte(expand), Record: adapterRecord(t, []byte(expand), SecretStorageVersion)}, parseHead(t, contract))
		if err != nil || check.HasFaults() {
			t.Fatalf("the contract of the seal is refused against its expand: %v %+v", err, check.Faults)
		}
		// A seal that also changes the bounds of the field is refused: the field before the seal is the field of the
		// head in clear (ST-DECL-INV102).
		wider := withChanges(strings.Replace(td01, description, "{ name: description, type: string, optional: true, max_length: 2000, sensitivity: secret }", 1), change,
			"  - {change: organization-description-seal, phase: expand}\n")
		check, err = CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, SecretStorageVersion)}, parseHead(t, wider))
		if err != nil || !check.HasFaults() {
			t.Fatalf("a seal that also changes max_length is accepted: %v", err)
		}
	})
}

// adapterRecord is the record of a release of the declaration whose postgres-adapter is version, with the tenancy
// component when the declaration selects it.
func adapterRecord(t *testing.T, declaration []byte, version string) []byte {
	t.Helper()
	record := "format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(declaration) + "\n  components:\n    postgres-adapter:\n      version: " + version + "\n"
	for _, s := range selectComponents(parseHead(t, string(declaration))) {
		if s.Component == gen.TenancyComponent {
			record += "    " + gen.TenancyComponent + ":\n      version: 1.0.0\n"
		}
	}
	return []byte(record)
}

func TestBaseClearSecret(t *testing.T) {
	t.Run("ST-SUP-RC01-OC49-T04 A base whose older generator stored a secret field in clear is refused with the seal step, and a seal from secret is accepted", func(t *testing.T) {
		td01 := string(userManagementBase(t))
		const description = "{ name: description, type: string, optional: true, max_length: 1000 }"
		const secret = "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }"
		declared := strings.Replace(td01, description, secret, 1)
		base := []byte(declared)
		// The release of the base ran postgres-adapter 6.12.0, which stored description in clear.
		check, err := CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, "6.12.0")}, parseHead(t, declared))
		if err != nil {
			t.Fatal(err)
		}
		var sealed bool
		for _, fault := range check.Faults {
			for _, change := range fault.Declare {
				sealed = sealed || (change.Kind == "seal" && change.Field == "description" && strings.HasPrefix(change.Needs, "from: secret") &&
					strings.Contains(fault.Message, "seal step for description"))
			}
		}
		if !sealed {
			t.Fatalf("the same declaration over a base that stored the secret in clear gives %+v, want a refusal with the seal step", check.Faults)
		}
		// The head with a seal from secret keeps the baseline of the base, so 001_initial is not written again.
		head := withChanges(declared, "  - {id: organization-description-seal, kind: seal, entity: Organization, field: description, from: secret}\n",
			"  - {change: organization-description-seal, phase: expand}\n")
		check, err = CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, "6.12.0")}, parseHead(t, head))
		if err != nil || check.HasFaults() {
			t.Fatalf("the seal from secret over a base that stored the secret in clear is refused: %v %+v", err, check.Faults)
		}
		// A base whose generator sealed the field needs no seal.
		check, err = CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, SecretStorageVersion)}, parseHead(t, declared))
		if err != nil || check.HasFaults() {
			t.Fatalf("the same declaration over a base that sealed the secret is refused: %v %+v", err, check.Faults)
		}
	})
}

func TestBaseSealReports(t *testing.T) {
	t.Run("ST-SUP-RC01-OC49-T06 A seal over a base whose processes do not report themselves is refused, and a pin that does not parse counts as stored in clear", func(t *testing.T) {
		td01 := string(userManagementBase(t))
		const description = "{ name: description, type: string, optional: true, max_length: 1000 }"
		const secret = "{ name: description, type: string, optional: true, max_length: 1000, sensitivity: secret }"
		declared := strings.Replace(td01, description, secret, 1)
		base := []byte(declared)
		head := withChanges(declared, "  - {id: organization-description-seal, kind: seal, entity: Organization, field: description, from: secret}\n",
			"  - {change: organization-description-seal, phase: expand}\n")
		for _, c := range []struct {
			name, record string
			refused      bool
		}{
			{"postgres-adapter 6.3.0", string(adapterRecord(t, base, "6.3.0")), true},
			{"no pin", string(recordFor(base)), true},
			{"postgres-adapter 6.4.0", string(adapterRecord(t, base, ReportAdapterVersion)), false},
		} {
			check, err := CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: base, Record: []byte(c.record)}, parseHead(t, head))
			if err != nil {
				t.Fatal(err)
			}
			refused := false
			for _, fault := range check.Faults {
				refused = refused || (strings.Contains(fault.Message, "do not report themselves") && strings.Contains(fault.Message, ReportRuntimeVersion))
			}
			if refused != c.refused {
				t.Errorf("%s: the seal gives %+v, want refused %t", c.name, check.Faults, c.refused)
			}
		}
		// A pin that does not parse counts as a release that stored the secret in clear: the same declaration is
		// refused with the seal step (I3).
		check, err := CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, "dev")}, parseHead(t, declared))
		if err != nil || !check.HasFaults() {
			t.Fatalf("the same declaration over a base with a pin that does not parse is accepted: %v", err)
		}
	})
}
