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

// nestedTickets is a declaration whose Ticket is scoped by a Project, and the Project by an Organization, so the
// path of the tickets holds the organization and the project.
func nestedTickets() *types.ServiceDeclaration {
	entity := func(name, resourceType string) types.Entity {
		e := types.Entity{Name: name}
		e.ResourceType = resourceType
		e.Permissions = map[types.Operation]string{types.OpRead: "view"}
		return e
	}
	project, ticket := entity("Project", "project"), entity("Ticket", "ticket")
	project.References = map[string]types.Reference{"org_id": {Relation: "organization"}}
	ticket.References = map[string]types.Reference{"project_id": {Relation: "project"}}
	return &types.ServiceDeclaration{
		Entities: []types.Entity{entity("Organization", "organization"), project, ticket},
		Collections: []types.Collection{
			{Name: "org-projects", Entity: "Project", Operations: []types.Operation{types.OpRead}, Scope: map[string]string{"org_id": "Organization"}},
			{Name: "project-tickets", Entity: "Ticket", Operations: []types.Operation{types.OpRead}, Scope: map[string]string{"project_id": "Project"}},
		},
	}
}

func TestScopedAccessWarningsAncestors(t *testing.T) {
	t.Run("ST-SUP-RC20-OC24-T16 The scoped-access warning also checks that the read of each ancestor needs the read of the ancestor above it", func(t *testing.T) {
		const top = "definition user {}\n" +
			"definition organization {\n\trelation member: user\n\tpermission view = member\n}\n"
		const ticket = "definition ticket {\n\trelation project: project\n\trelation owner: user\n\tpermission view = owner & project->view\n}\n"

		// The view of a ticket needs the view of its project, but the view of the project does not need the view of
		// its organization: the routes of project-tickets open a ticket to a caller that may not read the organization.
		got := scopedWarnings(t, nestedTickets(), top+ticket+
			"definition project {\n\trelation organization: organization\n\trelation member: user\n\tpermission view = member\n}\n")
		var ancestor []string
		for _, line := range got {
			if strings.Contains(line, "an ancestor in the path of collections.project-tickets") {
				ancestor = append(ancestor, line)
			}
		}
		if len(ancestor) != 1 || !strings.HasPrefix(ancestor[0], "entities[1].permissions.read: ") ||
			!strings.Contains(ancestor[0], "of its parent Organization through the relation organization") ||
			!strings.Contains(ancestor[0], "permission view = (member) & organization->view") || !strings.Contains(ancestor[0], "opens the Ticket there") {
			t.Fatalf("want one warning on the read of Project for the path of project-tickets, got %q", got)
		}

		// A project view that needs the view of its organization guards the path: no warning at all.
		if got := scopedWarnings(t, nestedTickets(), top+ticket+
			"definition project {\n\trelation organization: organization\n\trelation member: user\n\tpermission view = member & organization->view\n}\n"); len(got) != 0 {
			t.Fatalf("the guarded path was warned about: %q", got)
		}
	})
}

func TestScopeFieldDefault(t *testing.T) {
	t.Run("ST-DECL-INV106-T Checking refuses a default on a field that a collection names in its scope, and accepts the field with no default", func(t *testing.T) {
		entities := func(def any) []types.Entity {
			return []types.Entity{{Name: "Organization", Fields: []types.Field{{Name: "name", Type: "string"}}},
				{Name: "Project", Fields: []types.Field{{Name: "org_id", Type: types.FieldTypeRef, Default: def}, {Name: "size", Type: "int32", Default: 1}}}}
		}
		collections := []types.Collection{{Name: "org-projects", Entity: "Project", Operations: []types.Operation{types.OpCreate}, Scope: map[string]string{"org_id": "Organization"}}}
		faults := validateCollectionReferences(collections, entities("org-1"))
		if len(faults) != 1 || faults[0].Path != "collections.org-projects.scope.org_id" || faults[0].Found != "org-1" ||
			!strings.Contains(faults[0].Message, `collection "org-projects" has scope field "org_id", which declares the default org-1`) {
			t.Fatalf("want one fault for the default of the scope field, got %+v", faults)
		}
		if faults := validateCollectionReferences(collections, entities(nil)); len(faults) != 0 {
			t.Fatalf("a scope field with no default is refused: %+v", faults)
		}
		// A scope of two fields: the default of each scope field is refused at its own path, the second too
		// (security review of PR #474, I2).
		composite := []types.Entity{{Name: "Organization", Fields: []types.Field{{Name: "name", Type: "string"}}},
			{Name: "Team", Fields: []types.Field{{Name: "name", Type: "string"}}},
			{Name: "Project", Fields: []types.Field{{Name: "org_id", Type: types.FieldTypeRef}, {Name: "team_id", Type: types.FieldTypeRef, Default: "team-1"}}}}
		scoped := []types.Collection{{Name: "team-projects", Entity: "Project", Operations: []types.Operation{types.OpCreate},
			Scope: map[string]string{"org_id": "Organization", "team_id": "Team"}}}
		faults = validateCollectionReferences(scoped, composite)
		if len(faults) != 1 || faults[0].Path != "collections.team-projects.scope.team_id" || faults[0].Found != "team-1" {
			t.Fatalf("want one fault for the default of the second scope field, got %+v", faults)
		}
		composite[2].Fields[0].Default = "org-1"
		faults = validateCollectionReferences(scoped, composite)
		if len(faults) != 2 || faults[0].Path != "collections.team-projects.scope.org_id" || faults[1].Path != "collections.team-projects.scope.team_id" {
			t.Fatalf("want a fault for the default of each scope field, in the order of the fields, got %+v", faults)
		}
	})
}
