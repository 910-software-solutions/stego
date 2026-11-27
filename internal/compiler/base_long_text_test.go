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
		// With the relax the head is accepted, and a base of 6.33.0, which built no such index, needs no relax.
		relaxed := withChanges(td01, descriptionRelax, descriptionRelaxExpand)
		if faults := longTextFaults(t, td01, relaxed, "6.32.0"); len(faults) != 0 {
			t.Errorf("a head of a 6.32.0 base with the relax of description: got %+v, want no fault", faults)
		}
		if faults := longTextFaults(t, td01, td01, "6.33.0"); len(faults) != 0 {
			t.Errorf("a head of a 6.33.0 base with no relax: got %+v, want no fault", faults)
		}
		if faults := longTextFaults(t, td01, relaxed, "6.33.0"); len(faults) == 0 {
			t.Error("a relax with the item searchable of a field that a 6.33.0 base built with no search index is accepted")
		}
		// From a 6.33.0 base: searchable: false on a searched field needs the relax; a longer max_length past 500
		// characters names the item searchable; with the item each is accepted.
		const name = "{ name: display_name, type: string, min_length: 1, max_length: 100 }"
		off := strings.Replace(td01, name, "{ name: display_name, type: string, min_length: 1, max_length: 100, searchable: false }", 1)
		faults = longTextFaults(t, td01, off, "6.33.0")
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
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.33.0"); len(faults) != 0 {
			t.Errorf("searchable: false on User.display_name with the relax: got %+v, want no fault", faults)
		}
		longer := strings.Replace(td01, name, "{ name: display_name, type: string, min_length: 1, max_length: 4000 }", 1)
		faults = longTextFaults(t, td01, withChanges(longer, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}\n",
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.33.0")
		named := false
		for _, fault := range faults {
			named = named || (fault.Field == "display_name" && strings.Contains(fault.Message, "add the item searchable") && strings.Contains(fault.Message, "max_length 100, searchable"))
		}
		if !named {
			t.Errorf("a relax of User.display_name to 4000 characters with no item searchable: got %+v, want the fault that names the item", faults)
		}
		if faults := longTextFaults(t, td01, withChanges(longer, "  - {id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: \"max_length 100, searchable\"}\n",
			"  - {change: user-display-name-relax, phase: expand}\n"), "6.33.0"); len(faults) != 0 {
			t.Errorf("a relax of User.display_name to 4000 characters with the item searchable: got %+v, want no fault", faults)
		}
	})
}
