package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

// scopedProjects is a declaration whose entity Project is scoped by an organization and by a team, each through
// its own relation, as the schema states.
func scopedProjects(teamRelation string) *types.ServiceDeclaration {
	entity := func(name, resourceType string) types.Entity {
		e := types.Entity{Name: name}
		e.ResourceType = resourceType
		e.Permissions = map[types.Operation]string{types.OpRead: "view"}
		return e
	}
	project := entity("Project", "project")
	project.References = map[string]types.Reference{"org_id": {Relation: "organization"}, "team_id": {Relation: teamRelation}}
	return &types.ServiceDeclaration{
		Entities: []types.Entity{entity("Organization", "organization"), entity("Team", "team"), project},
		Collections: []types.Collection{
			{Name: "org-projects", Entity: "Project", Operations: []types.Operation{types.OpRead}, Scope: map[string]string{"org_id": "Organization"}},
			{Name: "team-projects", Entity: "Project", Operations: []types.Operation{types.OpRead}, Scope: map[string]string{"team_id": "Team"}},
		},
	}
}

func scopedWarnings(t *testing.T, service *types.ServiceDeclaration, schema string) []string {
	t.Helper()
	compiled, err := CompileAuthzSchema("authz/schema.zed", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, w := range ScopedAccessWarnings(service, compiled) {
		lines = append(lines, w.Path+": "+w.Message)
	}
	return lines
}

func TestScopedAccessWarningsEachScope(t *testing.T) {
	t.Run("ST-SUP-RC20-OC24-T15 The scoped-access warning checks a permission once for each scope, and not through a relation that admits another type", func(t *testing.T) {
		const parents = "definition user {}\n" +
			"definition organization {\n\trelation member: user\n\tpermission view = member\n}\n" +
			"definition team {\n\trelation member: user\n\tpermission view = member\n}\n"

		// The view of a project needs the view of its organization, but not that of its team. The collection that
		// the team scopes is warned about, though the collection that the organization scopes reads the same view.
		got := scopedWarnings(t, scopedProjects("team"), parents+
			"definition project {\n\trelation organization: organization\n\trelation team: team\n\trelation owner: user\n"+
			"\tpermission view = (owner + team->view) & organization->view\n}\n")
		if len(got) != 1 || !strings.Contains(got[0], "collections.team-projects") || !strings.Contains(got[0], "of its parent Team through the relation team") {
			t.Fatalf("want one warning for the collection team-projects, got %q", got)
		}

		// The relation organization also admits a team, so an arrow through it grants through a team too; that is no
		// guard for the collection that the organization scopes.
		got = scopedWarnings(t, scopedProjects("organization"), parents+
			"definition project {\n\trelation organization: organization | team\n\trelation owner: user\n"+
			"\tpermission view = owner & organization->view\n}\n")
		found := false
		for _, line := range got {
			found = found || strings.Contains(line, "collections.org-projects") && strings.Contains(line, "of its parent Organization through the relation organization")
		}
		if !found {
			t.Fatalf("want a warning for the collection org-projects, whose relation admits a team, got %q", got)
		}

		// The same relation with the organization as its only type guards the view: no warning for org-projects.
		got = scopedWarnings(t, scopedProjects("organization"), parents+
			"definition project {\n\trelation organization: organization\n\trelation owner: user\n"+
			"\tpermission view = owner & organization->view\n}\n")
		for _, line := range got {
			if strings.Contains(line, "collections.org-projects") {
				t.Fatalf("the guarded view of org-projects was warned about: %q", got)
			}
		}
	})
}
