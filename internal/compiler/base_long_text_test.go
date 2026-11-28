package compiler

import (
	"strings"
	"testing"
)

// longTextFaults runs the check against the base of TD01 at the record of postgres-adapter adapter on runtime
// v0.206.0, with the head given, and gives its faults.
func longTextFaults(t *testing.T, base, head, adapter string) []BaseFault {
	t.Helper()
	data := []byte(base)
	check, err := CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: data, Record: runtimeRecord(t, data, adapter, "v0.206.0")}, parseHead(t, head))
	if err != nil {
		t.Fatal(err)
	}
	return check.Faults
}

func TestBaseLongText(t *testing.T) {
	t.Run("ST-GEN-INV40-T A base whose generator gave a long text field a search index needs a relax with the item searchable, and a new step that takes a field out of the search names it", func(t *testing.T) {
		td01 := withoutDescriptionRelax(t, string(userManagementBase(t)))
		// A base of postgres-adapter 6.32.0 gave Organization.description (max_length 1000) its search index. The
		// same declaration as the head is refused, with the relax to declare.
		faults := longTextFaults(t, td01, td01, "6.32.0")
		suggested := false
		for _, fault := range faults {
			for _, change := range fault.Declare {
				suggested = suggested || (fault.Entity == "Organization" && fault.Field == "description" && change.Kind == "relax" &&
					change.Field == "description" && change.Needs == "the definition searchable" && change.Phase == "expand")
			}
		}
		if !suggested {
			t.Errorf("a head of a 6.32.0 base with no relax of description: got %+v, want the fault that asks for the relax with the definition searchable", faults)
		}
		// With the relax the head is accepted, and a base of 6.34.0, which built no such index, needs no relax.
		relaxed := withChanges(td01, descriptionRelax, descriptionRelaxExpand)
		if faults := longTextFaults(t, td01, relaxed, "6.32.0"); len(faults) != 0 {
			t.Errorf("a head of a 6.32.0 base with the relax of description: got %+v, want no fault", faults)
		}
		if faults := longTextFaults(t, td01, td01, "6.34.0"); len(faults) != 0 {
			t.Errorf("a head of a 6.34.0 base with no relax: got %+v, want no fault", faults)
		}
		if faults := longTextFaults(t, td01, relaxed, "6.34.0"); len(faults) == 0 {
			t.Error("a relax with the item searchable of a field that a 6.34.0 base built with no search index is accepted")
		}
		// From a 6.34.0 base: searchable: false on a searched field needs the relax; a longer max_length past 500
		// characters names the item searchable; with the item each is accepted.
		const name = "{ name: display_name, type: string, min_length: 1, max_length: 100 }"
		off := strings.Replace(td01, name, "{ name: display_name, type: string, min_length: 1, max_length: 100, searchable: false }", 1)
		faults = longTextFaults(t, td01, off, "6.34.0")
		suggested = false
		for _, fault := range faults {
			for _, change := range fault.Declare {
				suggested = suggested || (fault.Field == "display_name" && change.Kind == "relax" && change.Needs == "the definition searchable")
			}
		}
		if !suggested {
			t.Errorf("searchable: false on User.display_name with no relax: got %+v, want the fault that asks for the relax with the definition searchable", faults)
		}
		if faults := longTextFaults(t, td01, withChanges(off, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: searchable}\n",
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.34.0"); len(faults) != 0 {
			t.Errorf("searchable: false on User.display_name with the relax: got %+v, want no fault", faults)
		}
		longer := strings.Replace(td01, name, "{ name: display_name, type: string, min_length: 1, max_length: 4000 }", 1)
		faults = longTextFaults(t, td01, withChanges(longer, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}\n",
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.34.0")
		named := false
		for _, fault := range faults {
			named = named || (fault.Field == "display_name" && strings.Contains(fault.Message, "add the item searchable") && strings.Contains(fault.Message, "max_length 100, searchable"))
		}
		if !named {
			t.Errorf("a relax of User.display_name to 4000 characters with no item searchable: got %+v, want the fault that names the item", faults)
		}
		if faults := longTextFaults(t, td01, withChanges(longer, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: \"max_length 100, searchable\"}\n",
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.34.0"); len(faults) != 0 {
			t.Errorf("a relax of User.display_name to 4000 characters with the item searchable: got %+v, want no fault", faults)
		}
		// A relax with the item searchable of a field that is still in the search after it is refused by the check,
		// not only by generation (security review of PR #22, MF).
		searched := withChanges(td01, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: searchable}\n",
			"  - {change: user-display-name-relax, phase: expand}\n")
		for _, adapter := range []string{"6.32.0", "6.34.0"} {
			refused := false
			for _, fault := range longTextFaults(t, td01, searched, adapter) {
				refused = refused || fault.Field == "display_name"
			}
			if !refused {
				t.Errorf("a relax with the item searchable of User.display_name, which stays in the search, on a %s base is accepted", adapter)
			}
		}
		// A long text field of an entity that no collection lists has no search index, so a 6.32.0 base needs no relax
		// for it (security review of PR #22, MG).
		unlisted := strings.Replace(td01, "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete]\n", 1)
		if unlisted == td01 {
			t.Fatal("the base has no list of organizations to take away")
		}
		if faults := longTextFaults(t, unlisted, unlisted, "6.32.0"); len(faults) != 0 {
			t.Errorf("a 6.32.0 base with Organization.description on an entity that no collection lists: got %+v, want no fault", faults)
		}
		// A drop of a long text field, or of an entity with one, keeps the search index that a 6.32.0 base built: the
		// definition of the drop names the item searchable, and a dropped entity sets searchable: true (security
		// review of PR #22, M1). The check asks for each, and accepts the head with it.
		gone := strings.Replace(td01, "      - { name: description, type: string, optional: true, max_length: 1000 }\n", "", 1)
		drop := func(definition string) string {
			return withChanges(gone, "  - {id: organization-description-drop, kind: drop, entity: Organization, field: description, definition: \""+definition+"\"}\n",
				"  - {change: organization-description-drop, phase: expand}\n")
		}
		plain, kept := drop("string, optional, max_length 1000, after name"), drop("string, optional, max_length 1000, after name, searchable")
		asks := func(faults []BaseFault, entity, field, text string) bool {
			for _, fault := range faults {
				if fault.Entity == entity && fault.Field == field && strings.Contains(fault.Message, text) {
					return true
				}
			}
			return false
		}
		for name, c := range map[string][2]string{"a new drop": {td01, plain}, "a drop of the base": {plain, plain}} {
			if faults := longTextFaults(t, c[0], c[1], "6.32.0"); !asks(faults, "Organization", "description", "add the item searchable to the definition: string, optional, max_length 1000, after name, searchable") {
				t.Errorf("%s of Organization.description with no item searchable on a 6.32.0 base: got %+v, want the fault that asks for the item", name, faults)
			}
		}
		for name, c := range map[string][2]string{"a new drop": {td01, kept}, "a drop of the base": {plain, kept}} {
			if faults := longTextFaults(t, c[0], c[1], "6.32.0"); len(faults) != 0 {
				t.Errorf("%s of Organization.description with the item searchable on a 6.32.0 base: got %+v, want no fault", name, faults)
			}
		}
		if faults := longTextFaults(t, td01, kept, "6.34.0"); len(faults) == 0 {
			t.Error("a drop with the item searchable of a field that a 6.34.0 base built with no search index is accepted")
		}
		const action = "      - { name: action, type: string, max_length: 100 }\n"
		noted := withChanges(strings.Replace(td01, action, action+"      - { name: note, type: string, optional: true, max_length: 1000 }\n", 1), descriptionRelax, descriptionRelaxExpand)
		dropped := withChanges(strings.Replace(noted, "  - name: ActivityEntry\n", "  - name: ActivityEntry\n    dropped_by: activity-entry-drop\n", 1),
			"  - {id: activity-entry-drop, kind: drop, entity: ActivityEntry}\n", "  - {change: activity-entry-drop, phase: expand}\n")
		if faults := longTextFaults(t, noted, dropped, "6.32.0"); !asks(faults, "ActivityEntry", "note", "set searchable: true on the field") {
			t.Errorf("a drop of ActivityEntry with note of 1000 characters and no searchable setting on a 6.32.0 base: got %+v, want the fault that asks for searchable: true", faults)
		}
		on := strings.Replace(dropped, "{ name: note, type: string, optional: true, max_length: 1000 }", "{ name: note, type: string, optional: true, max_length: 1000, searchable: true }", 1)
		if faults := longTextFaults(t, noted, on, "6.32.0"); len(faults) != 0 {
			t.Errorf("a drop of ActivityEntry with note set searchable: true on a 6.32.0 base: got %+v, want no fault", faults)
		}
		// A drop of the base stays as it is: only the item searchable that the base marking adds is not a change, and a
		// change of a word that holds "searchable" is (security review sec-22c, N1).
		changed := "the head changes the schema change organization-description-drop"
		for _, c := range []struct{ base, head, adapter string }{
			{"string, optional, max_length 1000, after name, pattern ^searchable", "string, optional, max_length 1000, after name, pattern ^x", "6.34.0"},
			{"string, optional, max_length 1000, after name, pattern ^searchable", "string, optional, max_length 1000, after name, pattern ^x", "6.32.0"},
			{"string, optional, max_length 1000, after name, searchable", "string, optional, max_length 1000, after name", "6.34.0"},
		} {
			if faults := longTextFaults(t, drop(c.base), drop(c.head), c.adapter); !asks(faults, "Organization", "description", changed) {
				t.Errorf("a base drop %q changed to %q on a %s base: got %+v, want the fault that the head changes it", c.base, c.head, c.adapter, faults)
			}
		}
		if faults := longTextFaults(t, drop("string, optional, max_length 1000, after name, pattern ^searchable"), drop("string, optional, max_length 1000, after name, searchable, pattern ^searchable"), "6.32.0"); len(faults) != 0 {
			t.Errorf("a base drop with a pattern given the item searchable on a 6.32.0 base: got %+v, want no fault", faults)
		}
		// A record with no postgres-adapter pin, or a pin that does not parse, counts as a base that gave long text its
		// search index, so the check fails closed (security review of PR #22, L2).
		data := []byte(td01)
		archetype := "  archetype:\n    name: rest-crud\n    version: 3.0.0\n    runtime:\n      version: v0.207.0\n"
		records := map[string][]byte{
			"a record with no postgres-adapter pin and a runtime": append(recordFor(data), []byte(archetype)...),
			"a record with no pin":                                recordFor(data),
			"a postgres-adapter pin that does not parse":          runtimeRecord(t, data, "6.x", "v0.207.0"),
		}
		for name, record := range records {
			check, err := CheckBase(BaseSource{Ref: "r1", Commit: "c0ffee", Declaration: data, Record: record}, parseHead(t, td01))
			if err != nil {
				t.Fatal(err)
			}
			asked := false
			for _, fault := range check.Faults {
				for _, change := range fault.Declare {
					asked = asked || (fault.Entity == "Organization" && fault.Field == "description" && change.Needs == "the definition searchable")
				}
			}
			if !asked {
				t.Errorf("%s: got %+v, want the fault that asks for the relax of Organization.description with the definition searchable", name, check.Faults)
			}
		}
	})
}
