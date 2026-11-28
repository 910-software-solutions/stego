package declaration

import (
	"slices"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

// catalogService is the declaration of ST-DECL-INV108-T: Org, CostItem with the required reference org_id to Org,
// PriceHistory with the reference cost_item_id to CostItem, and the command add-cost-item with writes [Org, CostItem,
// PriceHistory] and creates_with_children.
func catalogService() *types.ServiceDeclaration {
	ref := func(name, to string, optional bool) types.Field {
		return types.Field{Name: name, Type: types.FieldTypeRef, To: to, Optional: optional}
	}
	return &types.ServiceDeclaration{
		Authorization: &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Entities: []types.Entity{
			{Name: "Org", EntityAuthorization: types.EntityAuthorization{ResourceType: "org"}},
			{Name: "CostItem", Fields: []types.Field{ref("org_id", "Org", false)}, EntityAuthorization: types.EntityAuthorization{ResourceType: "cost_item",
				References: map[string]types.Reference{"org_id": {Relation: "org", Permission: "catalog_edit"}}}},
			{Name: "PriceHistory", Fields: []types.Field{ref("cost_item_id", "CostItem", false)}, EntityAuthorization: types.EntityAuthorization{ResourceType: "price_history",
				References: map[string]types.Reference{"cost_item_id": {Relation: "cost_item", Permission: "edit"}}}},
		},
		Commands: []types.Command{{Name: "add-cost-item", Writes: []string{"Org", "CostItem", "PriceHistory"}, ResourceType: "org",
			Permission: "catalog_edit", CreatesWithChildren: true}},
	}
}

// catalogSchema is the schema of part A with cost_item.edit = edit and the relation org of CostItem as org.
func catalogSchema(t *testing.T, edit, org, owner string) *AuthzSchema {
	t.Helper()
	return compileTD03(t, `use expiration
caveat only_on(day int) {
	day > 0
}
definition user {}
definition group {
	relation member: user
}
definition org {
	relation owner: user
	relation admin: user
	relation member: user
	relation frozen: user
	permission catalog_edit = owner + admin
	permission catalog_view = owner + admin + member
}
definition cost_item {
	relation org: `+org+`
	relation owner: `+owner+`
	permission edit = `+edit+`
}
definition price_history {
	relation cost_item: cost_item
	permission edit = cost_item->edit
}`)
}

// witnessText gives the witnesses as relation kinds, such as org_id:reference:org:catalog_edit.
func witnessText(ws []ChildWitness) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Entity+"."+w.Field+" "+w.Subject+" "+w.Permission+" "+w.Kind+" "+w.Relation+" "+w.Decided)
	}
	return out
}

// probeSchema is the schema of the probe of the security review sec-17 of PR #17 (M4), for part B.
const probeSchema = `caveat only_on(day int) {
	day > 0
}
definition user {}
definition org {
	relation owner: user
	relation admin: user
	relation member: user
	relation frozen: user
	relation parent: org
	relation cav: user with only_on
	permission catalog_edit = owner + admin
	permission catalog_view = owner + admin + member
	permission catalog_read = member
	permission edit2 = catalog_edit + member
	permission view_nested = edit2 + catalog_read
	permission excl = (owner + admin + member) - frozen
	permission excl_sib = owner + admin + (member - frozen)
	permission excl_sib2 = catalog_edit + (member - frozen)
	permission inter = (owner + admin + member) & member
	permission upx = parent->catalog_edit
	permission up_all = parent.all(catalog_edit)
	permission withcav = owner + admin + cav
	permission editcav = owner + cav
	permission view_one = catalog_edit
}`

func TestCreatesWithChildren(t *testing.T) {
	t.Run("ST-DECL-INV108-T A: the witness shapes that the union-subset rule proves, and those it refuses", func(t *testing.T) {
		const arrow = "org->catalog_edit"
		for _, c := range []struct {
			name, edit, org, owner string
			relationship, optional bool
			want                   []string
		}{
			{"an arrow", arrow, "org", "user", false, false, []string{"org_id"}},
			{"a caller relationship", "owner", "org", "user", true, false, []string{"owner"}},
			{"both", "owner + " + arrow, "org", "user", true, false, []string{"owner", "org_id"}},
			{"an arrow any", "org.any(catalog_edit)", "org", "user", false, false, []string{"org_id"}},
			{"an intersection", "(owner + " + arrow + ") & org->catalog_view", "org", "user", true, false, nil},
			{"no caller relationship", "owner", "org", "user", false, false, nil},
			{"an exclusion", arrow + " - org->frozen", "org", "user", false, false, nil},
			{"an arrow all", "org.all(catalog_edit)", "org", "user", false, false, nil},
			{"two subject types", arrow, "org | user", "user", false, false, nil},
			{"a subject relation", arrow, "org | group#member", "user", false, false, nil},
			// The subject relation of the referenced type itself, alone or beside the plain type (security review of
			// PR #30, L3): org#member names the members of an org, not the org, so the arrow proves nothing.
			{"a subject relation of the referenced type", arrow, "org#member", "user", false, false, nil},
			{"the referenced type and a subject relation of it", arrow, "org | org#member", "user", false, false, nil},
			{"a wildcard", arrow, "org | org:*", "user", false, false, nil},
			{"a caveat", arrow, "org with only_on", "user", false, false, nil},
			{"an expiration", arrow, "org with expiration", "user", false, false, nil},
			{"an optional reference", arrow, "org", "user", false, true, nil},
			{"a caveated owner", "owner", "org", "user with only_on", true, false, nil},
		} {
			service := catalogService()
			if c.relationship {
				service.Entities[1].Relationships = []types.Relationship{{Relation: "owner", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}}
			}
			service.Entities[1].Fields[0].Optional = c.optional
			schema := catalogSchema(t, c.edit, c.org, c.owner)
			var got []string
			for _, w := range ChildWitnesses(service, schema, "add-cost-item") {
				if w.Entity == "PriceHistory" {
					if w.Kind == WitnessReference {
						got = append(got, "org_id")
					} else {
						got = append(got, w.Relation)
					}
				}
			}
			faults := CreatesWithChildrenFaults(service, schema)
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s: the witnesses are %v, want %v", c.name, got, c.want)
			}
			if refused := len(faults) == 1 && strings.Contains(faults[0].Message, "add-cost-item") && strings.Contains(faults[0].Message, "ST-DECL-INV108"); refused != (c.want == nil) {
				t.Fatalf("%s: checking gave %v, want a refusal only when no decision has a witness", c.name, faults)
			}
		}
		// The witness of the first shape, as the generator gives it.
		if got := witnessText(ChildWitnesses(catalogService(), catalogSchema(t, arrow, "org", "user"), "add-cost-item")); !slices.Equal(got,
			[]string{"PriceHistory.cost_item_id caller edit reference org catalog_edit"}) {
			t.Fatalf("the witnesses are %v", got)
		}
	})

	t.Run("ST-DECL-INV108-T B: the union-subset rule on the Org, and where it differs from implies", func(t *testing.T) {
		schema := compileTD03(t, probeSchema)
		for _, c := range []struct {
			d, x           string
			proof, implies bool
		}{
			{"catalog_edit", "catalog_edit", true, true},
			{"catalog_edit", "view_one", true, true},
			{"catalog_edit", "edit2", true, true},
			{"catalog_edit", "view_nested", true, false},
			{"catalog_edit", "excl_sib2", true, true},
			{"catalog_edit", "catalog_view", false, true},
			{"catalog_edit", "excl", false, false},
			{"catalog_edit", "excl_sib", false, true},
			{"catalog_edit", "inter", false, false},
			{"catalog_edit", "upx", false, false},
			{"upx", "up_all", false, false},
			{"editcav", "withcav", false, true},
		} {
			if got := schema.HoldsTerm("org", c.d, c.x); got != c.proof {
				t.Fatalf("%s => %s by the rule is %t, want %t", c.d, c.x, got, c.proof)
			}
			if got := schema.implies("org", c.d, c.x, 0); got != c.implies {
				t.Fatalf("implies(%s, %s) is %t, want its known answer %t", c.d, c.x, got, c.implies)
			}
		}
	})

	t.Run("ST-DECL-INV108-T C: each mode proves from the permission that the runtime decides, with a witness of its own subject kind", func(t *testing.T) {
		worker := func(catalogEdit string) (*types.ServiceDeclaration, *AuthzSchema) {
			service := catalogService()
			service.Commands = nil
			service.Workers = []types.Worker{{Name: "sync", Trigger: "every 1m", Writes: []string{"Org", "CostItem", "PriceHistory"}, Permission: "sync", CreatesWithChildren: true}}
			return service, compileTD03(t, `definition user {}
definition service_account {}
definition org {
	relation owner: user
	relation admin: user
	relation worker_sync: service_account
	permission catalog_edit = `+catalogEdit+`
	permission sync = worker_sync
}
definition cost_item {
	relation org: org
	permission sync = org->catalog_edit
}
definition price_history {
	relation cost_item: cost_item
	permission sync = cost_item->sync
}`)
		}
		if s, schema := worker("owner + admin"); len(CreatesWithChildrenFaults(s, schema)) != 1 {
			t.Fatal("a worker whose sync on the Org is no term of catalog_edit was not refused: validate decides with sync, not the named catalog_edit")
		}
		if s, schema := worker("owner + worker_sync + sync"); len(CreatesWithChildrenFaults(s, schema)) != 0 ||
			!slices.Equal(witnessText(ChildWitnesses(s, schema, "sync")), []string{"PriceHistory.cost_item_id worker sync reference org sync"}) {
			t.Fatalf("the worker with catalog_edit = owner + sync gave %v", witnessText(ChildWitnesses(s, schema, "sync")))
		}
		noPermission := catalogService()
		noPermission.Commands[0].Permission, noPermission.Commands[0].ResourceType = "", ""
		noPermission.Entities[1].References["org_id"] = types.Reference{Relation: "org"}
		schema := catalogSchema(t, "org->catalog_edit", "org", "user")
		if len(CreatesWithChildrenFaults(noPermission, schema)) != 1 {
			t.Fatal("a command with no permission whose org_id names none was not refused")
		}
		noPermission.Entities[1].References["org_id"] = types.Reference{Relation: "org", Permission: "catalog_edit"}
		if faults := CreatesWithChildrenFaults(noPermission, schema); len(faults) != 0 {
			t.Fatalf("the command with no permission whose org_id names catalog_edit gave %v", faults)
		}
		consumer := func(stage string) (*types.ServiceDeclaration, *AuthzSchema) {
			service := catalogService()
			service.Entities[1].Fields, service.Entities[1].References = nil, nil
			service.Commands = []types.Command{{Name: "place-costs", Permission: "stage", ResourceType: "service", Writes: []string{"Org"},
				Effects: []types.Effect{{Destination: "cost-requests", Name: "requested"}}}}
			service.Workers = []types.Worker{{Name: "apply-costs", Trigger: "consumes cost-requests", Writes: []string{"CostItem", "PriceHistory"}, Permission: "sync", CreatesWithChildren: true}}
			return service, compileTD03(t, `definition user {}
definition service_account {}
definition tenant {
	relation worker_apply_costs: service_account
	relation stager: user
	permission sync = worker_apply_costs
	permission stage = stager
}
definition org {
	relation owner: user
}
definition cost_item {
	relation tenant: tenant
	relation owner: user
	permission sync = tenant->sync
	permission stage = `+stage+`
}
definition price_history {
	relation cost_item: cost_item
	permission sync = cost_item->sync
	permission stage = cost_item->stage
}`)
		}
		s, schema := consumer("owner")
		warnings := NewReferenceWarnings(s, schema)
		if len(CreatesWithChildrenFaults(s, schema)) != 0 || len(warnings) != 1 || warnings[0].Rule != "new-reference-decided" ||
			!strings.Contains(warnings[0].Message, "cost_item_id") || !strings.Contains(warnings[0].Message, "stager") || !strings.Contains(warnings[0].Message, "stage") {
			t.Fatalf("the consumer with no stager proof gave %v and %v, want a pass with one warning for the stager", CreatesWithChildrenFaults(s, schema), warnings)
		}
		// A caller relationship of the CostItem is no witness of a stager or a worker decision: the subject of the
		// stage is the stager, not a caller of this run.
		s, schema = consumer("owner")
		s.Entities[1].Relationships = []types.Relationship{{Relation: "owner", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}}
		if warnings := NewReferenceWarnings(s, schema); len(warnings) != 1 || !strings.Contains(warnings[0].Message, "stager") {
			t.Fatalf("the consumer with a caller relationship owner and stage = owner gave %v, want the warning for the stager", warnings)
		}
		s, schema = consumer("tenant->stage")
		if len(CreatesWithChildrenFaults(s, schema)) != 0 || len(NewReferenceWarnings(s, schema)) != 0 {
			t.Fatalf("the consumer with the stager proof gave %v", NewReferenceWarnings(s, schema))
		}
		if got := witnessText(ChildWitnesses(s, schema, "apply-costs")); !slices.Equal(got, []string{
			"PriceHistory.cost_item_id worker sync tenant tenant sync", "PriceHistory.cost_item_id stager stage tenant tenant stage"}) {
			t.Fatalf("the consumer has the witnesses %v", got)
		}
	})

	t.Run("ST-DECL-INV108-T D: creates_with_children on a query, and on a command whose writes hold no reference, is refused", func(t *testing.T) {
		service := catalogService()
		service.Queries = []types.Query{{Name: "price-list", CreatesWithChildren: true}}
		service.Commands[0].Writes = []string{"Org"}
		faults := CreatesWithChildrenFaults(service, catalogSchema(t, "org->catalog_edit", "org", "user"))
		if len(faults) != 2 || !strings.Contains(faults[0].Message, "ST-DECL-INV108") || !strings.Contains(faults[1].Message, "ST-DECL-INV108") ||
			!strings.Contains(faults[0].Message, "price-list") || !strings.Contains(faults[1].Message, "add-cost-item") {
			t.Fatalf("checking gave %v, want the query and the command refused, each with ST-DECL-INV108", faults)
		}
	})
}
