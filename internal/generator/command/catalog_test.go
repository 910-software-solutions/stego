package command

import (
	_ "embed"
	"os/exec"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/localtest"
	"github.com/910-software-solutions/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// catalogProto holds the messages of the commands add-cost-item and show-cost-item of the catalog service.
const catalogProto = `syntax = "proto3";

package orders.v1;

// AddCostItemRequest is the request of add-cost-item.
message AddCostItemRequest {
  string org_id = 1;
  string name = 2;
  int64 price_cents = 3;
}

// AddCostItemResponse is the response of add-cost-item.
message AddCostItemResponse {
  string cost_item_id = 1;
  string price_history_id = 2;
}

// ShowCostItemRequest is the request of show-cost-item.
message ShowCostItemRequest {
  string cost_item_id = 1;
  string price_history_id = 2;
}

// ShowCostItemResponse is the response of show-cost-item.
message ShowCostItemResponse {
  string name = 1;
  int64 price_cents = 2;
  string cost_item_id = 3;
}
`

// catalogYAML is a service shaped like the catalog of erp-demo: an Org, a CostItem under the Org and a PriceHistory
// under the CostItem. The command add-cost-item is decided on the Org, creates a CostItem under it and the first
// PriceHistory of that CostItem in the same attempt, and declares creates_with_children (ST-DECL-INV108,
// ST-SUP-RP10-LC59). The command show-cost-item is decided on the CostItem and reads the CostItem and a PriceHistory.
const catalogYAML = `name: orders
archetype: rest-crud
language: go
base_path: /api/v1
tenancy:
  kind: pooled
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
  - name: show-cost-item
    writes: [CostItem, PriceHistory]
    resource_type: cost_item
    permission: view
    resource_member: costItemId
    route: POST /cost-items/{costItemId}/prices:show
    fill: show-cost-item
`

// catalogSchema is the schema of catalogYAML, the one that the check of the catalog service writes to SpiceDB and
// gives the schema gate of the command runtime. The relay raises its floor on the marker of stego_floor.
const catalogSchema = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition stego_floor {
	relation marker: stego_floor
}

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

// addCostItemFill is the fill add-cost-item: it loads the Org of the request, creates a CostItem under it and then
// the first PriceHistory of the CostItem under the loaded value that the create gave.
var addCostItemFill = []byte(`// Package addcostitem is the fill add-cost-item of the catalog test service.
package addcostitem

import (
	"context"

	commands "example.com/orders/out/commands"
)

// New gives the fill of the command add-cost-item.
func New() commands.AddCostItemFill {
	return func(ctx context.Context, request *commands.AddCostItemRequest, h *commands.AddCostItemHandle) (*commands.AddCostItemResponse, error) {
		orgs, err := commands.OrgRepository(h)
		if err != nil {
			return nil, err
		}
		org, err := orgs.Get(ctx, request.OrgId)
		if err != nil {
			return nil, err
		}
		items, err := commands.CostItemRepository(h)
		if err != nil {
			return nil, err
		}
		item, err := items.Create(ctx, commands.CostItem{OrgID: request.OrgId, Name: request.Name}, org)
		if err != nil {
			return nil, err
		}
		prices, err := commands.PriceHistoryRepository(h)
		if err != nil {
			return nil, err
		}
		price, err := prices.Create(ctx, commands.PriceHistory{CostItemID: item.ID(), PriceCents: request.PriceCents}, item)
		if err != nil {
			return nil, err
		}
		return &commands.AddCostItemResponse{CostItemId: item.ID(), PriceHistoryId: price.ID()}, nil
	}
}
`)

// showCostItemFill is the fill show-cost-item: it reads the CostItem and the PriceHistory of the request, and gives
// the name of the CostItem, the price and the CostItem of the PriceHistory.
var showCostItemFill = []byte(`// Package showcostitem is the fill show-cost-item of the catalog test service.
package showcostitem

import (
	"context"

	commands "example.com/orders/out/commands"
)

// New gives the fill of the command show-cost-item.
func New() commands.ShowCostItemFill {
	return func(ctx context.Context, request *commands.ShowCostItemRequest, h *commands.ShowCostItemHandle) (*commands.ShowCostItemResponse, error) {
		items, err := commands.CostItemRepository(h)
		if err != nil {
			return nil, err
		}
		item, err := items.Get(ctx, request.CostItemId)
		if err != nil {
			return nil, err
		}
		prices, err := commands.PriceHistoryRepository(h)
		if err != nil {
			return nil, err
		}
		price, err := prices.Get(ctx, request.PriceHistoryId)
		if err != nil {
			return nil, err
		}
		return &commands.ShowCostItemResponse{Name: item.Value().Name, PriceCents: price.Value().PriceCents, CostItemId: price.Value().CostItemID}, nil
	}
}
`)

//go:embed testdata/catalog_test.go
var catalogChecks []byte

// catalogContext is the context that the compiler gives the command generator for catalogYAML.
func catalogContext(t *testing.T) gen.Context {
	t.Helper()
	var svc types.ServiceDeclaration
	if err := yaml.Unmarshal([]byte(catalogYAML), &svc); err != nil {
		t.Fatal(err)
	}
	ctx := commandContext(t, &svc, commandSlot)
	ctx.Destinations = nil
	ctx.Observations = nil
	ctx.Inputs = map[string][]byte{"proto/orders.proto": []byte(catalogProto), "authz/schema.zed": []byte(catalogSchema)}
	return ctx
}

// TestCatalogService generates the catalog service, whose command add-cost-item declares creates_with_children,
// builds it with the pinned Go and runs its check against the local PostgreSQL server and SpiceDB, as
// TestCommandService does.
func TestCatalogService(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T09 The generated command add-cost-item answers 201 for a CostItem with its first PriceHistory", func(t *testing.T) {
		ctx := catalogContext(t)
		files := generated(t, ctx)
		// The handlers are gofmt'd, so the test reads them with each run of spaces as one space.
		handlers := strings.Join(strings.Fields(string(files["commands/handlers/handlers.go"])), " ")
		witness := `{Entity: "PriceHistory", Field: "cost_item_id", Subject: "caller", Permission: "edit", Kind: "reference", Relation: "org", Decided: "catalog_edit"},`
		if n := strings.Count(handlers, witness); n != 1 {
			t.Errorf("the handlers hold the witness of PriceHistory.cost_item_id %d times, want once:\n%s", n, handlers)
		}
		if n := strings.Count(handlers, "{Entity: "); n != 1 {
			t.Errorf("the handlers hold %d witnesses, want only the witness of PriceHistory.cost_item_id", n)
		}
		if n := strings.Count(handlers, "CreatesWithChildren: true,"); n != 1 {
			t.Errorf("%d commands of the handlers declare creates_with_children, want only add-cost-item", n)
		}
		postgres := "STEGO_TEST_POSTGRES_DSN=" + localtest.PostgresDSN(t)
		p := newProject(t, ctx, map[string][]byte{
			"fills/add-cost-item/fill.go":  addCostItemFill,
			"fills/show-cost-item/fill.go": showCostItemFill,
			"check/catalog_test.go":        []byte(strings.Replace(string(catalogChecks), "const schema = ``", "const schema = `"+catalogSchema+"`", 1)),
			"check/tables.sql":             tablesMigration(t, ctx),
		})
		p.mustGo("vet", "./...")
		endpoint, key := startSpiceDB(t)
		cmd := exec.Command(pinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestCatalog$", "./check")
		cmd.Dir = p.dir
		cmd.Env = localtest.GoEnv("GOFLAGS=-mod=mod", postgres, "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key)
		output, err := cmd.CombinedOutput()
		if !ownPass(string(output), "TestCatalog", "ST-SUP-RP10-LC59-T09") {
			t.Fatalf("the check of the catalog service did not run and pass (go test: %v):\n%s", err, output)
		}
	})
}
