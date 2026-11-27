package compiler

import (
	"slices"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestBaseResourceTypes(t *testing.T) {
	t.Parallel()
	// typed gives an entity with the resource type, and dropped by the change when dropped is not empty.
	typed := func(name, resourceType, dropped string) types.Entity {
		return types.Entity{Name: name, EntityAuthorization: types.EntityAuthorization{ResourceType: resourceType}, DroppedBy: dropped}
	}
	declaration := func(entities ...types.Entity) *types.ServiceDeclaration {
		sd := &types.ServiceDeclaration{}
		for _, e := range entities {
			sd.EntityOrder = append(sd.EntityOrder, e.Name)
			if e.DroppedBy != "" {
				sd.DroppedEntities = append(sd.DroppedEntities, e)
				continue
			}
			sd.Entities = append(sd.Entities, e)
		}
		return sd
	}
	// names gives the entity and the two quoted names of each fault.
	names := func(faults []BaseFault) []string {
		var out []string
		for _, f := range faults {
			if f.Rule != ResourceTypeRule {
				t.Fatalf("a fault of another rule: %+v", f)
			}
			out = append(out, f.Entity)
		}
		return out
	}
	t.Run("ST-GEN-INV41-T A head that gives a resource type of the base to another entity, or changes the type of an entity of the base, is refused", func(t *testing.T) {
		// Security review of PR #21, M1: Todo moves from document to task and a new Note takes document in one release;
		// the document tuples of the Todo rows stay and would grant a Note with the same ID.
		base := declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"))
		cases := []struct {
			name  string
			head  *types.ServiceDeclaration
			want  []string
			words []string
		}{
			{"the same head", declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop")), nil, nil},
			{"a new entity with a type of its own", declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"), typed("Note", "note", "")), nil, nil},
			{"a new entity with no resource type", declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"), typed("Setting", "", "")), nil, nil},
			{"Todo moves to task and a new Note takes document", declaration(typed("Todo", "task", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"), typed("Note", "document", "")),
				[]string{"Todo", "Note"}, []string{`gives Todo the resource type "task", and the base gives it the resource type "document"`, `gives Note the resource type "document", which the entity Todo has in the base`}},
			{"a new entity takes the type of an entity of the base that keeps it", declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"), typed("Note", "folder", "")),
				[]string{"Note"}, []string{`which the entity Folder has in the base`}},
			{"a new entity takes the type of a dropped entity of the base", declaration(typed("Todo", "document", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop"), typed("Panel", "board", "")),
				[]string{"Panel"}, []string{`which the entity Board has in the base`}},
			{"an entity of the base loses its resource type", declaration(typed("Todo", "", ""), typed("Folder", "folder", ""), typed("Board", "board", "board-drop")),
				[]string{"Todo"}, []string{`gives Todo no resource type`}},
			{"two entities swap their types", declaration(typed("Todo", "folder", ""), typed("Folder", "document", ""), typed("Board", "board", "board-drop")),
				[]string{"Todo", "Folder"}, []string{`gives Todo the resource type "folder"`, `gives Folder the resource type "document"`}},
		}
		for _, c := range cases {
			faults := resourceTypeFaults(base, c.head)
			if got := names(faults); !slices.Equal(got, c.want) {
				t.Errorf("%s: faults for %q, want %q: %+v", c.name, got, c.want, faults)
				continue
			}
			for i, words := range c.words {
				if !strings.Contains(faults[i].Message, words) || !strings.Contains(faults[i].Message, "(ST-GEN-INV41)") {
					t.Errorf("%s: the fault %q does not say %q", c.name, faults[i].Message, words)
				}
			}
		}
		// Two entities of the base that share a type and keep it get no fault here: the declaration check refuses
		// them (ST-DECL-INV107), and their move needs no fault of a reuse.
		shared := declaration(typed("Todo", "document", ""), typed("Note", "document", ""))
		if faults := resourceTypeFaults(shared, declaration(typed("Todo", "document", ""), typed("Note", "document", ""))); len(faults) != 0 {
			t.Errorf("a base type that two entities keep gives the faults %+v", faults)
		}
	})
	t.Run("ST-GEN-INV41-T The check against the base refuses the release of the security review of PR #21", func(t *testing.T) {
		// The base of examples/user-management has User on member; the head moves User to person and adds Team on
		// member, with its add step.
		base := []byte(userManagementBase(t))
		head := strings.Replace(withChanges(addTeam(string(base)), teamAdd, teamAddExpand), "  - name: Team\n    resource_type: team\n", "  - name: Team\n    resource_type: member\n", 1)
		head = strings.Replace(head, "  - name: User\n    resource_type: member\n", "  - name: User\n    resource_type: person\n", 1)
		if !strings.Contains(head, "resource_type: person") || !strings.Contains(head, "  - name: Team\n    resource_type: member\n") {
			t.Fatal("the head does not move User and give Team the type member")
		}
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, head))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range check.Faults {
			if f.Rule == ResourceTypeRule {
				got = append(got, f.Entity)
			}
		}
		if !slices.Equal(got, []string{"User", "Team"}) {
			t.Fatalf("the faults of %s are for %q, want User and Team: %+v", ResourceTypeRule, got, check.Faults)
		}
	})
}
