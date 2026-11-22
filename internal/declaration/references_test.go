package declaration

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jsell-rh/stego/internal/types"
)

// referencesSchema is the schema of the reference test: an order refers to its customer and its product.
const referencesSchema = `definition user {}
definition customer {
	relation owner: user
	permission place_order = owner
	permission view = owner
}
definition product {
	relation viewer: user
	permission view = viewer
}
definition order {
	relation owner: user
	relation customer: customer
	relation product: product
	permission view = owner
}
`

// referencesService is a pooled declaration with authorization whose Order refers to Customer and Product.
const referencesService = `kind: service
name: shop
archetype: rest-crud
language: go
base_path: /api/shop/v1
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Customer
    resource_type: customer
    permissions: {read: view, list: view}
    fields:
      - name: name
        type: string
  - name: Product
    resource_type: product
    permissions: {read: view, list: view}
    fields:
      - name: title
        type: string
  - name: Order
    resource_type: order
    permissions: {read: view, list: view}
    references:
      customer_id: {relation: customer, permission: place_order}
      product_id: {relation: product, permission: view}
    fields:
      - name: customer_id
        type: ref
        to: Customer
      - name: product_id
        type: ref
        to: Product
`

func TestReferences(t *testing.T) {
	t.Run("ST-DECL-INV85-T A reference to a protected entity with no relation, no permission or an undeclared name is refused, naming the field", func(t *testing.T) {
		check := func(service string) []ValidationError {
			t.Helper()
			decl := readTD03(t, service)
			return validateReferences(decl.Entities, compileTD03(t, referencesSchema))
		}
		expectAccepted(t, check(referencesService))
		// Through the whole authorization check, the accepted declaration gets no reference fault either.
		decl := readTD03(t, referencesService)
		for _, fault := range CheckAuthorization(decl, fstest.MapFS{"authz/schema.zed": {Data: []byte(referencesSchema)}}) {
			if fault.Rule == "entity-references" {
				t.Fatalf("the accepted declaration got %+v", fault)
			}
		}
		expectOneFault(t, check(change(t, referencesService, "      product_id: {relation: product, permission: view}\n", "")),
			"entity-references", "product_id")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{permission: view}")),
			"entity-references", "product_id", "relation")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: product}")),
			"entity-references", "product_id", "permission")
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: maker, permission: view}")),
			"entity-references", "product_id", "maker")
		expectOneFault(t, check(change(t, referencesService, "{relation: customer, permission: place_order}", "{relation: customer, permission: refund}")),
			"entity-references", "customer_id", "refund")
		// A relation of the schema for another subject type is not the reference's relation.
		expectOneFault(t, check(change(t, referencesService, "{relation: product, permission: view}", "{relation: customer, permission: view}")),
			"entity-references", "product_id", "customer")
		// References of a field that is no reference to a protected entity are refused too.
		expectOneFault(t, check(change(t, referencesService, "      product_id: {relation: product, permission: view}\n",
			"      product_id: {relation: product, permission: view}\n      note: {relation: product, permission: view}\n")),
			"entity-references", "note")
	})
}

// withinService is a pooled declaration whose Ticket of a Project refers to an optional Sprint of a Project, with
// the references of sprint_id given, and the schema changes given (ST-DECL-Q42).
func withinService(sprint string, changes ...string) string {
	service := `kind: service
name: tracker
archetype: rest-crud
language: go
base_path: /api/tracker/v1
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Project
    resource_type: project
    permissions: {read: view, list: view}
    fields:
      - {name: name, type: string}
  - name: Label
    fields:
      - {name: name, type: string}
  - name: Sprint
    resource_type: sprint
    permissions: {read: view, list: view}
    references:
      project_id: {relation: project, permission: edit}
    fields:
      - {name: project_id, type: ref, to: Project}
      - {name: label_id, type: ref, to: Label}
      - {name: note_id, type: ref, to: Project, optional: true}
  - name: Ticket
    resource_type: ticket
    permissions: {read: view, list: view}
    references:
      project_id: {relation: project, permission: edit}
      sprint_id: ` + sprint + `
    fields:
      - {name: project_id, type: ref, to: Project}
      - {name: sprint_id, type: ref, to: Sprint, optional: true}
      - {name: label_id, type: ref, to: Label}
      - {name: note_id, type: ref, to: Project, optional: true}
      - {name: board_id, type: ref, to: Project}
`
	if len(changes) > 0 {
		service += "schema_changes:\n"
		var steps []string
		for _, change := range changes {
			service += "  - " + change + "\n"
			id, _, _ := strings.Cut(strings.TrimPrefix(change, "{id: "), ",")
			steps = append(steps, "  - {change: "+id+", phase: expand}\n")
		}
		service += "schema_history:\n" + strings.Join(steps, "")
	}
	return service
}

func TestReferenceWithin(t *testing.T) {
	within := func(t *testing.T, sprint string) []ValidationError {
		t.Helper()
		decl := readTD03(t, withinService(sprint))
		return validateWithin(decl.Entities, decl.Collections)
	}
	t.Run("ST-DECL-INV93-T A within that both rows declare as a required reference to the same entity, or none, is accepted", func(t *testing.T) {
		expectAccepted(t, within(t, "{relation: sprint, permission: edit, within: project_id}"))
		expectAccepted(t, within(t, "{relation: sprint, permission: edit, within: none}"))
		expectAccepted(t, within(t, "{relation: sprint, permission: edit}"))
		// A required reference to an entity with no resource type is a within too.
		expectAccepted(t, within(t, "{relation: sprint, permission: edit, within: label_id}"))
	})
	for _, c := range []struct{ name, within, names string }{
		{"the reference itself", "sprint_id", "held within itself"},
		{"a field that the referenced entity does not declare", "board_id", `"Sprint" declares no required reference "board_id"`},
		{"a field that neither entity declares", "ticket_label", `"Ticket" declares no required reference "ticket_label"`},
		{"a field that is no reference", "name", `"Ticket" declares no required reference "name"`},
		{"an optional reference on both rows that no collection scopes", "note_id", `"Ticket" declares no required reference "note_id"`},
	} {
		t.Run("ST-DECL-INV93-T A within of "+c.name+" is refused, naming the entity, the reference and the field", func(t *testing.T) {
			faults := within(t, "{relation: sprint, permission: edit, within: "+c.within+"}")
			if len(faults) != 1 || faults[0].Rule != "reference-within" || faults[0].Path != "entities[3].references.sprint_id.within" || faults[0].Found != c.within ||
				!strings.Contains(faults[0].Message, `"Ticket"`) || !strings.Contains(faults[0].Message, `"sprint_id"`) || !strings.Contains(faults[0].Message, c.names) {
				t.Fatalf("want one reference-within fault that names Ticket, sprint_id and %s, got %+v", c.names, faults)
			}
		})
	}
	t.Run("ST-DECL-INV93-T A within whose two fields refer to different entities is refused", func(t *testing.T) {
		decl := readTD03(t, withinService("{relation: sprint, permission: edit, within: label_id}"))
		for i := range decl.Entities[2].Fields {
			if decl.Entities[2].Fields[i].Name == "label_id" {
				decl.Entities[2].Fields[i].To = "Project"
			}
		}
		faults := validateWithin(decl.Entities, decl.Collections)
		if len(faults) != 1 || !strings.Contains(faults[0].Message, `"label_id" refers to Label on "Ticket" and to Project on "Sprint"`) {
			t.Fatalf("want one fault that names both entities, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV93-T An optional reference that a collection scopes is a within", func(t *testing.T) {
		decl := readTD03(t, withinService("{relation: sprint, permission: edit, within: note_id}"))
		expectAccepted(t, func() []ValidationError {
			decl.Collections = []types.Collection{{Name: "project-tickets", Entity: "Ticket", Scope: map[string]string{"note_id": "Project"}},
				{Name: "project-sprints", Entity: "Sprint", Scope: map[string]string{"note_id": "Project"}}}
			return validateWithin(decl.Entities, decl.Collections)
		}())
	})
	t.Run("ST-DECL-INV93-T A reference with no within between two entities that refer to the same entity is warned about, and not refused", func(t *testing.T) {
		warnings := WithinWarnings(readTD03(t, withinService("{relation: sprint, permission: edit}")))
		if len(warnings) != 1 || warnings[0].Rule != "reference-within" || warnings[0].Path != "entities[3].references.sprint_id" ||
			!strings.Contains(warnings[0].Message, "within: project_id") || !strings.Contains(warnings[0].Message, "within: none") {
			t.Fatalf("want one warning on Ticket.sprint_id that tells within: project_id and within: none, got %+v", warnings)
		}
		for _, sprint := range []string{"{relation: sprint, permission: edit, within: project_id}", "{relation: sprint, permission: edit, within: none}"} {
			if warnings := WithinWarnings(readTD03(t, withinService(sprint))); len(warnings) != 0 {
				t.Errorf("a reference that states its within %s is warned about: %+v", sprint, warnings)
			}
		}
		// The warning is among the warnings of checking.
		found := false
		for _, w := range Warnings(readTD03(t, withinService("{relation: sprint, permission: edit}"))) {
			found = found || w.Rule == "reference-within"
		}
		if !found {
			t.Error("checking does not give the within warning")
		}
	})
}

func TestWithinSchemaChange(t *testing.T) {
	kept := func(t *testing.T, service string, rules ...string) []ValidationError {
		t.Helper()
		return compositionFaults(t, service, Facts{}, rules...)
	}
	held := "{relation: sprint, permission: edit, within: project_id}"
	t.Run("ST-DECL-INV57-T A within with its field and its expand step is accepted", func(t *testing.T) {
		wantNoFault(t, kept(t, withinService(held, "{id: ticket-sprint-within, kind: within, entity: Ticket, field: sprint_id}"), "schema-change-kind", "schema-change-entity", "schema-step"))
	})
	for _, c := range []struct{ name, change, names string }{
		{"a within with no Field", "{id: ticket-sprint-within, kind: within, entity: Ticket}", "no field"},
		{"a within with a From", "{id: ticket-sprint-within, kind: within, entity: Ticket, field: sprint_id, from: x}", "from"},
	} {
		t.Run("ST-DECL-INV57-T A schema change with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, kept(t, withinService(held, c.change), "schema-change-kind"), c.names)
		})
	}
	for _, c := range []struct{ name, sprint, field string }{
		{"a reference that declares no within", "{relation: sprint, permission: edit}", "sprint_id"},
		{"a reference that declares within: none", "{relation: sprint, permission: edit, within: none}", "sprint_id"},
		{"a field that is no reference", held, "label"},
	} {
		t.Run("ST-DECL-INV58-T A within of "+c.name+" is refused", func(t *testing.T) {
			change := "{id: ticket-sprint-within, kind: within, entity: Ticket, field: " + c.field + "}"
			wantFault(t, kept(t, withinService(c.sprint, change), "schema-change-entity"), `"ticket-sprint-within"`, `"Ticket"`)
		})
	}
	t.Run("ST-DECL-INV59-T A within with a contract step is refused", func(t *testing.T) {
		service := withinService(held, "{id: ticket-sprint-within, kind: within, entity: Ticket, field: sprint_id}") + "  - {change: ticket-sprint-within, phase: contract}\n"
		wantFault(t, kept(t, service, "schema-step"), "within has an expand step only")
	})
}

func TestRelateSchemaChange(t *testing.T) {
	kept := func(t *testing.T, service string, rules ...string) []ValidationError {
		t.Helper()
		return compositionFaults(t, service, Facts{}, rules...)
	}
	held := "{relation: sprint, permission: edit}"
	for _, change := range []string{"{id: ticket-sprint-relate, kind: relate, entity: Ticket, relation: sprint}",
		"{id: ticket-tenant-relate, kind: relate, entity: Ticket, relation: tenant}"} {
		t.Run("ST-DECL-INV57-T A relate of a relation that the runtime writes, with its expand step, is accepted", func(t *testing.T) {
			wantNoFault(t, kept(t, withinService(held, change), "schema-change-kind", "schema-change-entity", "schema-step", "schema-change-mode"))
		})
	}
	for _, c := range []struct{ name, change, names string }{
		{"a relate with no relation", "{id: ticket-sprint-relate, kind: relate, entity: Ticket}", "no relation"},
		{"a relate with a field", "{id: ticket-sprint-relate, kind: relate, entity: Ticket, relation: sprint, field: sprint_id}", "field"},
		{"a relation on another kind", "{id: ticket-sprint-within, kind: within, entity: Ticket, field: sprint_id, relation: sprint}", "relation"},
	} {
		t.Run("ST-DECL-INV57-T A schema change with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, kept(t, withinService("{relation: sprint, permission: edit, within: project_id}", c.change), "schema-change-kind"), c.names)
		})
	}
	for _, c := range []struct{ name, change, names string }{
		{"a relation that the runtime does not write", "{id: ticket-watcher-relate, kind: relate, entity: Ticket, relation: watcher}", "no relationship with a field subject"},
		{"an entity with no resource type", "{id: label-tenant-relate, kind: relate, entity: Label, relation: tenant}", "no resource type"},
	} {
		t.Run("ST-DECL-INV58-T A relate of "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, kept(t, withinService(held, c.change), "schema-change-entity"), c.names)
		})
	}
	t.Run("ST-DECL-INV59-T A relate with a contract step is refused", func(t *testing.T) {
		service := withinService(held, "{id: ticket-sprint-relate, kind: relate, entity: Ticket, relation: sprint}") + "  - {change: ticket-sprint-relate, phase: contract}\n"
		wantFault(t, kept(t, service, "schema-step"), "relate has an expand step only")
	})
	t.Run("ST-DECL-INV61-T A relate in a service that does not run the controller is refused", func(t *testing.T) {
		service := withinService(held, "{id: ticket-sprint-relate, kind: relate, entity: Ticket, relation: sprint}") + "modes: [api]\n"
		wantFault(t, kept(t, service, "schema-change-mode"), "ticket-sprint-relate", "controller")
	})
}

func TestRelateSchema(t *testing.T) {
	t.Run("ST-DECL-INV58-T A relate of tenant needs the relation tenant of the subject type tenant in the schema", func(t *testing.T) {
		decl := readTD03(t, withinService("{relation: sprint, permission: edit}", "{id: ticket-tenant-relate, kind: relate, entity: Ticket, relation: tenant}"))
		for schema, refused := range map[string]bool{
			"definition user {}\ndefinition tenant {}\ndefinition ticket {\n\trelation tenant: tenant\n}\n": false,
			"definition user {}\ndefinition tenant {}\ndefinition ticket {\n\trelation owner: user\n}\n":    true,
			"definition user {}\ndefinition tenant {}\ndefinition ticket {\n\trelation tenant: user\n}\n":   true,
		} {
			compiled, err := CompileAuthzSchema("authz/schema.zed", []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			faults := relateSchema(decl, compiled)
			if refused != (len(faults) == 1 && strings.Contains(faults[0].Message, "ticket#tenant")) || (!refused && len(faults) != 0) {
				t.Errorf("the relate of tenant with the schema\n%s\ngave %+v, want refused %v", schema, faults, refused)
			}
		}
	})
}
