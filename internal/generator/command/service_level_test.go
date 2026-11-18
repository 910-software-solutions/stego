package command

import (
	"go/token"
	gotypes "go/types"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// placeProto holds the messages of the command place-order, which creates an Order with no parent.
const placeProto = `syntax = "proto3";

package orders.v1;

// PlaceOrderRequest is the request of place-order.
message PlaceOrderRequest {
  string sku = 1;
}

// PlaceOrderResponse is the response of place-order.
message PlaceOrderResponse {
  string order_id = 1;
}
`

// placeYAML is a service with the tenant provisioning on the resource type service and the command place-order,
// whose resource type is that one: it names no resource member and is decided on the service-level resource
// (ST-SUP-RC32-OC15).
const placeYAML = `name: orders
archetype: rpc-service
language: go
base_path: /api/v1
tenancy:
  kind: pooled
  provisioning: {resource_type: service, provision: provision, retire: retire}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
entities:
  - name: Order
    resource_type: order
    permissions: {read: view, list: view}
    relationships:
      - {relation: owner, subject_source: caller, subject_type: user}
    fields:
      - {name: sku, type: string}
  - name: Line
    references:
      order_id: {relation: order, permission: view}
    fields:
      - {name: order_id, type: ref, to: Order}
commands:
  - name: place-order
    writes: [Order]
    resource_type: service
    permission: place_order
    route: POST /orders
    fill: place-order
`

// placeSchema is the schema of placeYAML.
const placeSchema = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition service {
	relation operator: user
	relation placer: user
	permission provision = operator
	permission retire = operator
	permission place_order = placer
}

definition order {
	relation owner: user
	permission view = owner
}
`

// placeFill is the fill place-order: it creates one Order through the handle of the command.
var placeFill = []byte(`// Package placeorder is the fill place-order of the test service.
package placeorder

import (
	"context"

	commands "example.com/orders/out/commands"
)

// New gives the fill of the command place-order.
func New() commands.PlaceOrderFill {
	return func(ctx context.Context, request *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		order, err := h.CreateOrder(ctx, commands.Order{Sku: request.Sku})
		if err != nil {
			return nil, err
		}
		return &commands.PlaceOrderResponse{OrderId: order.ID()}, nil
	}
}
`)

// placeContext is the context that the compiler gives the command generator for placeYAML, changed by edit.
func placeContext(t *testing.T, edit func(*types.ServiceDeclaration)) gen.Context {
	t.Helper()
	var svc types.ServiceDeclaration
	if err := yaml.Unmarshal([]byte(placeYAML), &svc); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&svc)
	}
	ctx := commandContext(t, &svc, commandSlot)
	ctx.Tenancy = &gen.Tenancy{Key: "tenant_id", Provisioning: svc.Tenancy.Provisioning}
	ctx.Destinations = nil
	ctx.Collections = svc.Collections
	ctx.Observations = nil
	ctx.Inputs = map[string][]byte{"proto/orders.proto": []byte(placeProto), "authz/schema.zed": []byte(placeSchema)}
	return ctx
}

func TestServiceLevelCommand(t *testing.T) {
	t.Run("ST-SUP-RC32-OC15-T01 A command on the service-level resource is decided on it and its handle offers only the create and the read", func(t *testing.T) {
		ctx := placeContext(t, nil)
		files := generated(t, ctx)
		// The handlers are gofmt'd, so the test reads them with each run of spaces as one space.
		handlers := strings.Join(strings.Fields(string(files["commands/handlers/handlers.go"])), " ")
		for _, want := range []string{
			`Resource: &command.Resource{Type: "service", Service: "orders"},`,
			`Resource: func(*commands.PlaceOrderRequest) string { return "orders" },`,
			`return fill(ctx, request, commands.NewPlaceOrderHandle(h))`,
		} {
			if !strings.Contains(handlers, want) {
				t.Errorf("the handlers of place-order do not hold %s:\n%s", want, handlers)
			}
		}
		if strings.Contains(handlers, "authz_token FROM public.") {
			t.Errorf("the command on the service-level resource reads a row token:\n%s", handlers)
		}
		// The fill compiles against the contract, and the handle has no method that reads a row with a lock, updates
		// or deletes it, or that gives the command handle or a repository.
		p := newProject(t, ctx, map[string][]byte{"fills/place-order/fill.go": placeFill})
		p.mustGo("vet", "./...")
		fset := token.NewFileSet()
		pkg, err := exportImporter(t, p, fset, "./out/commands").Import(testModule + "/out/commands")
		if err != nil {
			t.Fatal(err)
		}
		handle := pkg.Scope().Lookup("PlaceOrderHandle")
		if handle == nil {
			t.Fatal("the contract has no PlaceOrderHandle")
		}
		methods := gotypes.NewMethodSet(gotypes.NewPointer(handle.Type()))
		var names []string
		for i := 0; i < methods.Len(); i++ {
			names = append(names, methods.At(i).Obj().Name())
		}
		sort.Strings(names)
		if got, want := strings.Join(names, " "), "CreateOrder Effects GetOrder Principal Probe Tenant"; got != want {
			t.Errorf("the handle of place-order has the methods %s, want %s", got, want)
		}
		if s, ok := handle.Type().Underlying().(*gotypes.Struct); !ok || s.NumFields() != 1 || s.Field(0).Exported() {
			t.Errorf("the handle of place-order is %s, want a struct with one unexported field", handle.Type().Underlying())
		}
	})

	t.Run("ST-SUP-RC32-OC15-T02 A command on the service-level resource that could act on a row the decision does not cover is refused", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			edit  func(*types.ServiceDeclaration)
			wants []string
		}{
			{"a resource member", func(s *types.ServiceDeclaration) { s.Commands[0].ResourceMember = "sku" }, []string{"place-order", "resource member sku", "names no resource member"}},
			{"the provision permission", func(s *types.ServiceDeclaration) { s.Commands[0].Permission = "provision" }, []string{"place-order", "permission provision", "Provision or Retire"}},
			{"the retire permission", func(s *types.ServiceDeclaration) { s.Commands[0].Permission = "retire" }, []string{"place-order", "permission retire"}},
			{"a written relation", func(s *types.ServiceDeclaration) { s.Commands[0].RelationshipsWritten = []string{"order#owner"} }, []string{"place-order", "order#owner", "only the create"}},
			{"an entity with a parent", func(s *types.ServiceDeclaration) { s.Commands[0].Writes = []string{"Line", "Order"} }, []string{"place-order", "Line", "parent"}},
			{"an entity under a collection scope", func(s *types.ServiceDeclaration) {
				s.Collections = []types.Collection{{Name: "orders", Entity: "Order", Operations: []types.Operation{types.OpCreate}, Scope: map[string]string{"customer_id": "Customer"}}}
			}, []string{"place-order", "Order", "collection orders"}},
		} {
			_, _, err := new(Generator).Generate(placeContext(t, c.edit))
			if err == nil {
				t.Errorf("%s: the generator accepted the command", c.name)
				continue
			}
			for _, want := range c.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the refusal %q does not name %q", c.name, err, want)
				}
			}
		}
		// The same command with its resource type on another type than the provisioning still needs its member.
		_, _, err := new(Generator).Generate(placeContext(t, func(s *types.ServiceDeclaration) { s.Commands[0].ResourceType = "order" }))
		if err == nil || !strings.Contains(err.Error(), "resource member") {
			t.Errorf("a command on order with no resource member gave %v, want a refusal that names the resource member", err)
		}
	})
}
