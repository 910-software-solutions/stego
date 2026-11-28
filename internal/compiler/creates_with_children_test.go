package compiler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// catalogDeclaration is a catalog shaped like erp-demo: the command add-cost-item writes an Org, a CostItem under
// the Org and a PriceHistory under the CostItem, and declares creates_with_children. The schema proves the decision
// of edit on the new CostItem from the decision of catalog_edit on its Org, but nothing proves the decision of
// catalog_edit on the Org: the Org has no caller relationship and no reference.
const catalogDeclaration = `name: catalog
archetype: rest-crud
language: go
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Org
    resource_type: org
    permissions: {read: catalog_view}
    fields:
      - {name: name, type: string}
  - name: CostItem
    resource_type: cost_item
    permissions: {read: view}
    references:
      org_id: {relation: org, permission: catalog_edit}
    fields:
      - {name: org_id, type: ref, to: Org}
      - {name: name, type: string}
  - name: PriceHistory
    resource_type: price_history
    permissions: {read: view}
    references:
      cost_item_id: {relation: cost_item, permission: edit}
    fields:
      - {name: cost_item_id, type: ref, to: CostItem}
      - {name: price_cents, type: int64}
commands:
  - name: add-cost-item
    writes: [Org, CostItem, PriceHistory]
    resource_type: org
    permission: catalog_edit
    resource_member: orgId
    route: POST /orgs/{orgId}/cost-items
    fill: add-cost-item
    creates_with_children: true
`

// catalogDeclarationSchema is the schema of catalogDeclaration.
const catalogDeclarationSchema = `definition user {}

definition org {
	relation editor: user
	relation viewer: user
	permission catalog_edit = editor
	permission catalog_view = viewer + editor
}

definition cost_item {
	relation org: org
	permission edit = org->catalog_edit
	permission view = org->catalog_view
}

definition price_history {
	relation cost_item: cost_item
	permission edit = cost_item->edit
	permission view = cost_item->view
}
`

func TestNewReferenceWarning(t *testing.T) {
	t.Run("ST-DECL-INV108-T E: validate warns new-reference-decided for the one decision of a command with creates_with_children that the schema does not prove", func(t *testing.T) {
		project := t.TempDir()
		if err := os.MkdirAll(filepath.Join(project, "authz"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(project, "authz", "schema.zed"), catalogDeclarationSchema)
		var service types.ServiceDeclaration
		if err := yaml.Unmarshal([]byte(catalogDeclaration), &service); err != nil {
			t.Fatal(err)
		}
		// scopedAccessWarnings gives the warnings of the schema that stego validate prints (Validate).
		var got []ValidationError
		for _, w := range scopedAccessWarnings(&service, project) {
			if w.Rule == "new-reference-decided" {
				got = append(got, w)
			}
		}
		if len(got) != 1 {
			t.Fatalf("validate gave %d warnings new-reference-decided, want one for CostItem.org_id: %v", len(got), got)
		}
		w := got[0]
		if w.Found != "CostItem.org_id" || w.Path != "commands[0].creates_with_children" {
			t.Errorf("the warning is at %s on %s, want commands[0].creates_with_children on CostItem.org_id", w.Path, w.Found)
		}
		for _, want := range []string{`"add-cost-item"`, "CostItem.org_id", "subject kind caller", "decision of catalog_edit", "ST-DECL-INV108"} {
			if !strings.Contains(w.Message, want) {
				t.Errorf("the warning %q does not name %s", w.Message, want)
			}
		}
		// The proved decision, edit on the new CostItem for PriceHistory.cost_item_id, gives no warning.
		if strings.Contains(w.Message, "cost_item_id") {
			t.Errorf("the warning names the proved reference PriceHistory.cost_item_id: %s", w.Message)
		}
	})
}

func TestCreatesWithChildrenWiring(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T13 The runtime wiring gives the schema text to the parts and to the migration job only in a service with creates_with_children", func(t *testing.T) {
		opted, err := renderRuntimeWiring(runtimeWiringData{CreatesWithChildren: true})
		if err != nil {
			t.Fatal(err)
		}
		// gofmt aligns the values of a composite literal, so the check takes any space after the key.
		for _, key := range []string{"SchemaText", "Schema"} {
			if !regexp.MustCompile(`\b` + key + `:\s+svcauthz\.Schema,`).Match(opted) {
				t.Errorf("the wiring of a service with creates_with_children does not give %s: svcauthz.Schema", key)
			}
		}
		plain, err := renderRuntimeWiring(runtimeWiringData{})
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`:\s+svcauthz\.Schema,`).Match(plain) {
			t.Error("the wiring of a service with no creates_with_children gives the schema text, so the generated code of each existing service changes")
		}
	})
}
