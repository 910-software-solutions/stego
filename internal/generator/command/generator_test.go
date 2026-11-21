package command

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/domainprobe"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// testModule is the module of the generated test service.
const testModule = "example.com/orders"

// ordersProto is the proto file of the command messages of the test service.
const ordersProto = `syntax = "proto3";

package orders.v1;

// ReserveAndChargeRequest is the request of reserve-and-charge.
message ReserveAndChargeRequest {
  string order_id = 1;
  string sku = 2;
  int32 qty = 3;
  int64 amount_cents = 4;
  string card_last4 = 5;
}

// ReserveAndChargeResponse is the response of reserve-and-charge.
message ReserveAndChargeResponse {
  string reservation_id = 1;
  string charge_id = 2;
}
`

// serviceYAML is the service declaration of the RC32 test records: the owned entities Order (resource type order),
// Hold (resource type hold, a reference to Order, the relationship creator from the caller and owner from the field
// owner_id), Reservation and Charge (amount_cents, card_last4 with sensitivity pii), the outbox destination billing,
// and the command reserve-and-charge that writes Reservation and Charge, needs reserve on order, may write order#holder and
// may stage effects for billing, bound to the fill reserve-and-charge.
const serviceYAML = `name: orders
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
  - name: Order
    resource_type: order
    permissions:
      read: view
      list: view
    fields:
      - name: sku
        type: string
  - name: Hold
    resource_type: hold
    permissions:
      read: view
    references:
      order_id: {relation: order, permission: reserve}
    relationships:
      - relation: creator
        subject_source: caller
        subject_type: user
      - relation: owner
        subject_source: field
        subject_field: owner_id
        subject_type: user
    fields:
      - name: order_id
        type: ref
        to: Order
      - name: owner_id
        type: string
  - name: Reservation
    fields:
      - name: order_id
        type: string
      - name: sku
        type: string
      - name: qty
        type: int32
  - name: Charge
    fields:
      - name: order_id
        type: string
      - name: amount_cents
        type: int64
      - name: card_last4
        type: string
        sensitivity: pii
commands:
  - name: reserve-and-charge
    writes: [Reservation, Charge, Order, Hold]
    resource_type: order
    permission: reserve
    resource_member: orderId
    route: POST /orders/{orderId}/reservations
    fill: reserve-and-charge
    relationships_written: [order#holder]
    effects:
      - destination: billing
        name: charge-requested
    refusals:
      - name: short_stock
        status: 409
        code: reservation.short_stock
    retry_on_conflict: "1"
    lists:
      - name: of-order
        entity: Reservation
        filter:
          - {field: order_id, op: equals, param: order_id}
          - {field: qty, op: greater_than, value: 0}
observations:
  - name: reservations_made
    kind: counter
    severity: info
    emitted_by: [reserve-and-charge]
    fields:
      - name: size
        type: enum(small | large)
`

// declaration reads serviceYAML.
func declaration(t testing.TB) *types.ServiceDeclaration {
	t.Helper()
	var svc types.ServiceDeclaration
	if err := yaml.Unmarshal([]byte(serviceYAML), &svc); err != nil {
		t.Fatal(err)
	}
	return &svc
}

// commandContext is the context that the compiler gives the command generator for the declaration, with the fill
// reserve-and-charge implementing slot.
// yes is the true of a declared delivery property.
var yes = true

func commandContext(t testing.TB, svc *types.ServiceDeclaration, slot string) gen.Context {
	t.Helper()
	var commands []gen.Command
	for _, c := range svc.Commands {
		commands = append(commands, gen.Command{Command: c, FillSlot: slot})
	}
	return gen.Context{
		ModuleName:      testModule,
		OutDirName:      "out",
		OutputNamespace: "commands",
		ServiceName:     svc.Name,
		BasePath:        svc.BasePath,
		Entities:        svc.Entities,
		Commands:        commands,
		Authorization:   svc.Authorization,
		Tenancy:         &gen.Tenancy{Key: "tenant_id"},
		Destinations:    []gen.Destination{{Name: "billing", Component: "outbox", MustLand: &yes, Idempotent: &yes}},
		Observations:    svc.Observations,
		PeerNamespaces:  map[string]string{"domain-probe": "probes", "command": "commands"},
		ComponentConfig: map[string]any{"proto_files": []any{map[string]any{"path": "proto/orders.proto", "import_path": "orders/v1/orders.proto"}}},
		Inputs:          map[string][]byte{"proto/orders.proto": []byte(ordersProto), "authz/schema.zed": []byte(schemaZed)},
	}
}

// schemaZed is the schema of the test declaration, the one that the service check writes to SpiceDB.
const schemaZed = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition order {
	relation owner: user
	relation viewer: user
	relation holder: user
	permission reserve = owner
	permission view = owner + viewer
}

definition hold {
	relation owner: user
	relation creator: user
	relation order: order
	permission view = owner + creator
	permission reserve = owner + creator
}
`

// generated generates ctx and gives the files by path.
func generated(t testing.TB, ctx gen.Context) map[string][]byte {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		out[f.Path] = f.Bytes()
	}
	return out
}

// TestDump writes the generated files of the test declaration to STEGO_COMMAND_DUMP, for a reader.
func TestDump(t *testing.T) {
	dir := os.Getenv("STEGO_COMMAND_DUMP")
	if dir == "" {
		t.Skip("STEGO_COMMAND_DUMP is not set")
	}
	for name, data := range generated(t, commandContext(t, declaration(t), commandSlot)) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// exportImporter is a go/types importer that reads the export data that go list -export gives for each package of
// the generated module.
func exportImporter(t *testing.T, p *project, fset *token.FileSet, pattern string) gotypes.Importer {
	t.Helper()
	out := p.mustGo("list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", pattern)
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if path, file, ok := strings.Cut(line, "="); ok && file != "" {
			files[path] = file
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
}

func TestContract(t *testing.T) {
	t.Run("ST-SUP-RC32-OC12-T01 The generated CommandSpec holds the declared delivery properties and effect names", func(t *testing.T) {
		svc := declaration(t)
		svc.Commands[0].Effects = append(svc.Commands[0].Effects, types.Effect{Destination: "notify", Name: "reserved"})
		ctx := commandContext(t, svc, commandSlot)
		no := false
		ctx.Destinations = append(ctx.Destinations, gen.Destination{Name: "notify", Component: "outbox", MustLand: &no, Idempotent: &no})
		handlers := string(generated(t, ctx)["commands/handlers/handlers.go"])
		for _, want := range []string{
			`{Name: "billing", MustLand: true, Idempotent: true, Effects: []string{"charge-requested"}}`,
			`{Name: "notify", MustLand: false, Idempotent: false, Effects: []string{"reserved"}}`,
		} {
			if !strings.Contains(handlers, want) {
				t.Errorf("the CommandSpec of reserve-and-charge does not hold %s:\n%s", want, handlers)
			}
		}
		ctx.Destinations[1].MustLand = nil
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "notify") || !strings.Contains(err.Error(), "must_land") {
			t.Fatalf("a destination with no must_land gave %v, want a refusal that names notify and must_land", err)
		}
		ctx.Destinations = ctx.Destinations[:1]
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "notify") {
			t.Fatalf("an effect for a destination that no component declares gave %v", err)
		}
	})

	t.Run("ST-SUP-RC32-OC16-T01 The contract holds each declared list of a command with its With function, and the CommandSpec declares it", func(t *testing.T) {
		ctx := commandContext(t, declaration(t), commandSlot)
		files := generated(t, ctx)
		contract := strings.Join(strings.Fields(string(files["commands/commands.go"])), " ")
		handlers := strings.Join(strings.Fields(string(files["commands/handlers/handlers.go"])), " ")
		for _, want := range []string{
			`var ReserveAndChargeOfOrder = command.Must(command.NewList(ReservationEntity, command.ListSpec{Name: "of-order",`,
			`Read: "SELECT \"id\"::text, \"order_id\", \"sku\", \"qty\", \"version\", ''::text FROM public.\"reservations\" WHERE \"tenant_id\" = $1 AND \"order_id\" = $3 AND \"qty\" > $4 ORDER BY \"id\" LIMIT $2",`,
			`Values: command.DeclaredValues(command.ListValue{Param: "order_id"}, command.ListValue{Constant: int32(0)})}))`,
			`func ReserveAndChargeOfOrderWith(orderID string) *command.List[Reservation] { return ReserveAndChargeOfOrder.With(orderID) }`,
			// The spec takes the lists that the contract package held when it started, before a fill could set the
			// list variable to its own list (security review of PR #416, L3).
			`var reserveAndChargeDeclaredLists = command.Snapshot(ReserveAndChargeOfOrder)`,
			`func ReserveAndChargeDeclaredLists() []command.AnyList { return append([]command.AnyList(nil), reserveAndChargeDeclaredLists...) }`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the contract does not hold %s:\n%s", want, contract)
			}
		}
		if !strings.Contains(handlers, `Lists: commands.ReserveAndChargeDeclaredLists(),`) {
			t.Errorf("the CommandSpec of reserve-and-charge does not declare its list:\n%s", handlers)
		}
		// The release that adds a field and renames another builds the list from its own entity.
		svc := declaration(t)
		for i := range svc.Entities {
			if svc.Entities[i].Name == "Reservation" {
				svc.Entities[i].Fields = append(svc.Entities[i].Fields, types.Field{Name: "due_at", Type: types.FieldTypeTimestamp})
			}
		}
		svc.Commands[0].Lists[0].Filter = append(svc.Commands[0].Lists[0].Filter, types.ListTerm{Field: "due_at", Op: types.ListLessThan, Param: "due_before"})
		next := strings.Join(strings.Fields(string(generated(t, commandContext(t, svc, commandSlot))["commands/commands.go"])), " ")
		for _, want := range []string{`\"qty\", \"due_at\", \"version\"`, `AND \"due_at\" < $5 ORDER BY`,
			`func ReserveAndChargeOfOrderWith(orderID string, dueBefore time.Time) *command.List[Reservation]`} {
			if !strings.Contains(next, want) {
				t.Errorf("the release with due_at does not hold %s:\n%s", want, next)
			}
		}
		for _, c := range []struct {
			name  string
			edit  func(*types.ServiceDeclaration)
			wants []string
		}{
			{"an entity that the command does not write", func(s *types.ServiceDeclaration) { s.Commands[0].Lists[0].Entity = "Customer" }, []string{"of-order", "reserve-and-charge", "does not write"}},
			{"a field that the entity does not declare", func(s *types.ServiceDeclaration) { s.Commands[0].Lists[0].Filter[0].Field = "colour" }, []string{"of-order", "colour"}},
			{"a lock with no Lock at most", func(s *types.ServiceDeclaration) { s.Commands[0].Lists[0].Lock = true }, []string{"of-order", "Lock at most"}},
			{"a parameter twice", func(s *types.ServiceDeclaration) {
				s.Commands[0].Lists[0].Filter[1] = types.ListTerm{Field: "qty", Op: types.ListGreaterThan, Param: "order_id"}
			}, []string{"of-order", "order_id"}},
			{"a list name twice", func(s *types.ServiceDeclaration) {
				s.Commands[0].Lists = append(s.Commands[0].Lists, s.Commands[0].Lists[0])
			}, []string{"of-order", "twice"}},
		} {
			svc := declaration(t)
			c.edit(svc)
			_, _, err := new(Generator).Generate(commandContext(t, svc, commandSlot))
			if err == nil {
				t.Errorf("%s: the generator accepted the list", c.name)
				continue
			}
			for _, want := range c.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the refusal %q does not name %q", c.name, err, want)
				}
			}
		}
		// A command on the service-level resource has no list (ST-SUP-RC32-OC15).
		_, _, err := new(Generator).Generate(placeContext(t, func(s *types.ServiceDeclaration) {
			s.Commands[0].Lists = []types.WorkerList{{Name: "orders", Entity: "Order", Filter: []types.ListTerm{{Field: "sku", Op: types.ListIsSet}}}}
		}))
		if err == nil || !strings.Contains(err.Error(), "service-level resource declares the list orders") {
			t.Errorf("a list of a command on the service-level resource gave %v", err)
		}
	})

	t.Run("ST-SUP-RC32-OC16-T03 A list of a child entity takes its loaded parent row, and any other list of a child is refused", func(t *testing.T) {
		// Reservation becomes a child of Order: it has no resource type and a reference to Order.
		child := func(edit func(*types.ServiceDeclaration)) gen.Context {
			svc := declaration(t)
			for i := range svc.Entities {
				if svc.Entities[i].Name == "Reservation" {
					svc.Entities[i].Fields[0] = types.Field{Name: "order_id", Type: types.FieldTypeRef, To: "Order"}
				}
			}
			svc.Commands[0].Lists[0].Filter[0] = types.ListTerm{Field: "order_id", Op: types.ListEquals, Parent: true}
			if edit != nil {
				edit(svc)
			}
			return commandContext(t, svc, commandSlot)
		}
		ctx := child(nil)
		contract := strings.Join(strings.Fields(string(generated(t, ctx)["commands/commands.go"])), " ")
		for _, want := range []string{
			`Values: command.DeclaredValues(command.ListValue{Parent: "Order"}, command.ListValue{Constant: int32(0)})}))`,
			`func ReserveAndChargeOfOrderWith(order LoadedOrder) *command.List[Reservation] { return ReserveAndChargeOfOrder.With(order) }`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the contract does not hold %s:\n%s", want, contract)
			}
		}
		// The contract with the parent row compiles.
		newProject(t, ctx, map[string][]byte{"fills/reserve-and-charge/fill.go": serviceFill}).mustGo("vet", "./out/commands/...")
		// A key parameter of a reference is checked with the key check (security review of PR #416, L1).
		key := strings.Join(strings.Fields(string(generated(t, child(func(s *types.ServiceDeclaration) {
			s.Commands[0].Lists = append(s.Commands[0].Lists, types.WorkerList{Name: "holds", Entity: "Hold",
				Filter: []types.ListTerm{{Field: "order_id", Op: types.ListEquals, Param: "order_id"}}})
		}))["commands/commands.go"])), " ")
		if !strings.Contains(key, `command.ListValue{Param: "order_id", Key: true, KeyOf: command.KeyUUID}`) {
			t.Errorf("a reference parameter is not a key:\n%s", key)
		}
		for name, c := range map[string]struct {
			edit func(*types.ServiceDeclaration)
			want string
		}{
			"a free order ID": {func(s *types.ServiceDeclaration) {
				s.Commands[0].Lists[0].Filter[0] = types.ListTerm{Field: "order_id", Op: types.ListEquals, Param: "order_id"}
			}, "needs exactly one parent term"},
			"a parent term of an entity with a resource type": {func(s *types.ServiceDeclaration) {
				s.Commands[0].Lists = append(s.Commands[0].Lists, types.WorkerList{Name: "holds", Entity: "Hold",
					Filter: []types.ListTerm{{Field: "order_id", Op: types.ListEquals, Parent: true}}})
			}, "Hold is not a child entity"},
			"a parent term on a reference to an entity with no resource type": {func(s *types.ServiceDeclaration) {
				for i := range s.Entities {
					if s.Entities[i].Name == "Reservation" {
						s.Entities[i].Fields = append(s.Entities[i].Fields, types.Field{Name: "charge_id", Type: types.FieldTypeRef, To: "Charge"})
					}
				}
				s.Commands[0].Lists[0].Filter[0] = types.ListTerm{Field: "charge_id", Op: types.ListEquals, Parent: true}
			}, "a parent term needs a reference to an entity with a resource type, and Charge has none"},
			"a parent that the command does not write": {func(s *types.ServiceDeclaration) {
				s.Commands[0].Writes = []string{"Reservation", "Charge", "Hold"}
				s.Commands[0].RelationshipsWritten = nil
			}, "does not write"},
		} {
			if _, _, err := new(Generator).Generate(child(c.edit)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s gave %v, want a refusal that holds %q", name, err, c.want)
			}
		}
	})

	t.Run("ST-SUP-RC32-OC01-T01 The fill gets the request and one command handle", func(t *testing.T) {
		p := newProject(t, commandContext(t, declaration(t), commandSlot), map[string][]byte{"fills/reserve-and-charge/fill.go": serviceFill})
		fset := token.NewFileSet()
		imp := exportImporter(t, p, fset, "./out/commands")
		pkg, err := imp.Import(testModule + "/out/commands")
		if err != nil {
			t.Fatal(err)
		}
		obj := pkg.Scope().Lookup("ReserveAndChargeFill")
		if obj == nil {
			t.Fatal("the contract has no slot type ReserveAndChargeFill")
		}
		sig, ok := obj.Type().Underlying().(*gotypes.Signature)
		if !ok {
			t.Fatalf("ReserveAndChargeFill is %s, not a function type", obj.Type())
		}
		params := sig.Params()
		if params.Len() != 3 {
			t.Fatalf("the slot takes %d parameters, want the context, the request and the handle", params.Len())
		}
		if got := params.At(0).Type().String(); got != "context.Context" {
			t.Errorf("the first parameter is %s, want context.Context", got)
		}
		if got := params.At(1).Type().String(); got != "*"+testModule+"/out/commands.ReserveAndChargeRequest" {
			t.Errorf("the second parameter is %s, want the decoded request message", got)
		}
		handle, ok := params.At(2).Type().(*gotypes.Pointer)
		named, _ := gotypes.Unalias(handle.Elem()).(*gotypes.Named)
		if !ok || named == nil || named.Obj().Pkg().Path() != "github.com/jsell-rh/stego/registry/runtime/command" || named.Obj().Name() != "Handle" {
			t.Errorf("the third parameter is %s, want *command.Handle", params.At(2).Type())
		}
		for i := 0; i < params.Len(); i++ {
			for _, refused := range []string{"database/sql", "net", "net/http", "github.com/jsell-rh/stego/registry/runtime/authz.Runtime", "github.com/authzed", "github.com/jackc"} {
				if strings.Contains(params.At(i).Type().String(), refused) {
					t.Errorf("the parameter %s has a type of %s", params.At(i).Type(), refused)
				}
			}
		}
	})

	t.Run("ST-SUP-RC32-OC02-T01 One handler is emitted per command, bound to its fill", func(t *testing.T) {
		files := generated(t, commandContext(t, declaration(t), commandSlot))
		fset := token.NewFileSet()
		bound, err := parser.ParseFile(fset, "fills.go", files["commands/handlers/fills.go"], 0)
		if err != nil {
			t.Fatal(err)
		}
		var fills []string
		for _, spec := range bound.Imports {
			if strings.Contains(spec.Path.Value, "/fills/") {
				fills = append(fills, strings.Trim(spec.Path.Value, `"`))
			}
		}
		if len(fills) != 1 || fills[0] != testModule+"/fills/reserve-and-charge" {
			t.Errorf("the handlers bind the fills %q, want only %s/fills/reserve-and-charge", fills, testModule)
		}
		source, err := parser.ParseFile(fset, "handlers.go", files["commands/handlers/handlers.go"], 0)
		if err != nil {
			t.Fatal(err)
		}
		var handlers, registered []string
		ast.Inspect(source, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && ast.IsExported(n.Name.Name) && n.Type.Results != nil && len(n.Type.Results.List) == 2 {
					if sel, ok := n.Type.Results.List[0].Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Handler" {
						handlers = append(handlers, n.Name.Name)
					}
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Handle" && len(n.Args) == 2 {
					if lit, ok := n.Args[0].(*ast.BasicLit); ok {
						registered = append(registered, lit.Value)
					}
				}
			}
			return true
		})
		if len(handlers) != 1 || handlers[0] != "ReserveAndCharge" {
			t.Errorf("the HTTP handlers are %q, want one for reserve-and-charge", handlers)
		}
		if len(registered) != 1 || registered[0] != `"POST /api/v1/orders/{orderId}/reservations"` {
			t.Errorf("the registered routes are %q, want the one route of reserve-and-charge", registered)
		}
		if !strings.Contains(string(files["commands/handlers/fills.go"]), "ReserveAndCharge: fillReserveAndCharge.New()") {
			t.Error("the handler of reserve-and-charge is not bound to the fill reserve-and-charge")
		}
	})

	t.Run("ST-SUP-RC32-OC06-T02 Each reference of a row to an entity with a resource type is in its Parent list", func(t *testing.T) {
		files := generated(t, commandContext(t, declaration(t), commandSlot))
		want := `{Entity: "Order", ResourceType: "order", ID: func(v Hold) string { return v.OrderID }, Permission: "reserve", Relation: "order"}`
		if !strings.Contains(string(files["commands/commands.go"]), want) {
			t.Errorf("the entity Hold, which refers to Order, does not have the Parent %s:\n%s", want, files["commands/commands.go"])
		}
		// A second reference and an optional one are references of the list too; the optional one is not set when
		// its field is nil.
		svc := declaration(t)
		for i := range svc.Entities {
			if svc.Entities[i].Name == "Hold" {
				svc.Entities[i].Fields = append(svc.Entities[i].Fields, types.Field{Name: "other_id", Type: types.FieldTypeRef, To: "Order", Optional: true})
				svc.Entities[i].References["other_id"] = types.Reference{Relation: "order", Permission: "view"}
			}
		}
		contract := string(generated(t, commandContext(t, svc, commandSlot))["commands/commands.go"])
		for _, want := range []string{want, `{Entity: "Order", ResourceType: "order", ID: func(v Hold) string {`, `if v.OtherID == nil {`, `Permission: "view"`} {
			if !strings.Contains(contract, want) {
				t.Errorf("the Parent list of Hold with two references does not hold %s:\n%s", want, contract)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC05-T25 An entity whose schema type has the relation tenant of the subject type tenant is marked Tenant", func(t *testing.T) {
		ctx := commandContext(t, declaration(t), commandSlot)
		schema := strings.Replace(schemaZed, "definition hold {\n", "definition tenant {}\n\ndefinition hold {\n\trelation tenant: tenant\n", 1)
		schema = strings.Replace(schema, "\trelation holder: user\n", "\trelation holder: user\n\trelation tenant: user\n", 1)
		ctx.Inputs = map[string][]byte{"proto/orders.proto": []byte(ordersProto), "authz/schema.zed": []byte(schema)}
		contract := string(generated(t, ctx)["commands/commands.go"])
		hold := contract[strings.Index(contract, "HoldEntity = "):]
		hold = hold[:strings.Index(hold, "}))")]
		marked := regexp.MustCompile(`\n\tTenant:\s+true,\n`)
		if !marked.MatchString(hold) {
			t.Errorf("the entity Hold, whose type has the relation tenant: tenant, is not marked Tenant:\n%s", hold)
		}
		// The relation tenant of another subject type writes no tuple to the tenant.
		if order := contract[strings.Index(contract, "OrderEntity = "):]; marked.MatchString(order[:strings.Index(order, "}))")]) {
			t.Errorf("the entity Order, whose relation tenant takes a user, is marked Tenant:\n%s", order)
		}
		// No command may relate the tenant relation, a worker relation or the tenant resource: the generator refuses
		// each, so it emits no Relate or Unrelate helper for it (ST-SUP-RC35-OC13).
		for relation, why := range map[string]string{"order#tenant": "the runtime writes it alone",
			"order#worker_sync": "only the tenant provisioning writes", "tenant#worker_sync": "only the tenant provisioning writes"} {
			svc := declaration(t)
			svc.Commands[0].RelationshipsWritten = []string{relation}
			if _, _, err := new(Generator).Generate(commandContext(t, svc, commandSlot)); err == nil || !strings.Contains(err.Error(), relation) || !strings.Contains(err.Error(), why) {
				t.Errorf("a command that writes %s gave %v, want a refusal that names it and says %q", relation, err, why)
			}
		}
		// With the schema of the test declaration, no entity is marked.
		if marked.Match(generated(t, commandContext(t, declaration(t), commandSlot))["commands/commands.go"]) {
			t.Error("an entity is marked Tenant with a schema that has no relation tenant")
		}
	})

	t.Run("ST-SUP-RC29-OC05-T05 Each declared relationship of an entity is in its Relationships list", func(t *testing.T) {
		files := generated(t, commandContext(t, declaration(t), commandSlot))
		contract := string(files["commands/commands.go"])
		for _, want := range []string{
			"\tRelationships: []command.Relationship[Hold]{\n",
			"\tRelations: []string{\"creator\", \"order\", \"owner\"},\n",
			`{Relation: "creator", SubjectType: "user"},`,
			`{Relation: "owner", SubjectType: "user", Field: func(v Hold) string { return v.OwnerID }, FieldName: "owner_id"},`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the entity Hold does not have %s in its Relationships list:\n%s", want, contract)
			}
		}
		// A subject from an optional field reads its pointer, and a subject of a resource type of this service is
		// marked as a resource, so the runtime qualifies its ID with the tenant.
		svc := declaration(t)
		for i := range svc.Entities {
			if svc.Entities[i].Name == "Hold" {
				svc.Entities[i].Fields = append(svc.Entities[i].Fields,
					types.Field{Name: "backup_id", Type: types.FieldTypeString, Optional: true},
					types.Field{Name: "linked_id", Type: types.FieldTypeRef, To: "Order"})
				svc.Entities[i].Relationships = append(svc.Entities[i].Relationships,
					types.Relationship{Relation: "backup", SubjectSource: types.SubjectSourceField, SubjectField: "backup_id", SubjectType: "user"},
					types.Relationship{Relation: "linked", SubjectSource: types.SubjectSourceField, SubjectField: "linked_id", SubjectType: "order"})
			}
		}
		contract = string(generated(t, commandContext(t, svc, commandSlot))["commands/commands.go"])
		for _, want := range []string{
			`{Relation: "backup", SubjectType: "user", Field: func(v Hold) string {`, "if v.BackupID == nil {", "return *v.BackupID", `}, FieldName: "backup_id"},`,
			`{Relation: "linked", SubjectType: "order", Field: func(v Hold) string { return v.LinkedID }, FieldName: "linked_id", Resource: true},`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the Relationships list of Hold does not hold %s:\n%s", want, contract)
			}
		}
		// An entity without relationships gets no list, and a subject field that is not a string or ref is refused.
		if strings.Contains(contract, "Relationship[Order]") {
			t.Error("the entity Order, which declares no relationship, has a Relationships list")
		}
		svc.Entities[1].Relationships = append(svc.Entities[1].Relationships,
			types.Relationship{Relation: "count", SubjectSource: types.SubjectSourceField, SubjectField: "missing_id", SubjectType: "user"})
		if _, _, err := new(Generator).Generate(commandContext(t, svc, commandSlot)); err == nil || !strings.Contains(err.Error(), "missing_id") {
			t.Errorf("a relationship whose subject field is not on the entity gave %v, want a refusal that names it", err)
		}
	})

	t.Run("ST-SUP-RC32-OC02-T02 A command bound to a fill of another slot is refused", func(t *testing.T) {
		files, wiring, err := new(Generator).Generate(commandContext(t, declaration(t), "rest-api.before_create"))
		if err == nil || !strings.Contains(err.Error(), "reserve-and-charge") || !strings.Contains(err.Error(), "the fill reserve-and-charge") {
			t.Fatalf("generation gave %v, want a fault that names the command and the fill", err)
		}
		if files != nil || wiring != nil {
			t.Errorf("a refused generation gave %d files", len(files))
		}
		if err := new(Generator).ValidateContext(commandContext(t, declaration(t), "rest-api.before_create")); err == nil {
			t.Error("the preflight check accepts a fill of another slot")
		}
	})

	t.Run("ST-SUP-RC32-OC02-T04 A service that declares no command gets no command output", func(t *testing.T) {
		// The archetypes hold the command component (ST-SUP-RC20-OC39), so a service with collections only has it.
		ctx := commandContext(t, declaration(t), commandSlot)
		ctx.Commands = nil
		ctx.ComponentConfig = map[string]any{}
		ctx.Inputs = nil
		ctx.Tenancy, ctx.Authorization = nil, nil
		inputs, err := new(Generator).InputFiles(ctx.ComponentConfig)
		if err != nil || len(inputs) != 0 {
			t.Fatalf("InputFiles with no proto_files gave %v, %v; want no input and no fault", inputs, err)
		}
		if err := new(Generator).ValidateContext(ctx); err != nil {
			t.Fatalf("ValidateContext refuses a service with no command: %v", err)
		}
		files, wiring, err := new(Generator).Generate(ctx)
		if err != nil || len(files) != 0 || wiring == nil || len(wiring.GoModRequires) != 0 {
			t.Fatalf("Generate with no command gave %d files, wiring %+v and %v; want no file, an empty wiring and no fault", len(files), wiring, err)
		}
		// The proto files hold the messages of the declared commands: with no command they serve nothing.
		with := commandContext(t, declaration(t), commandSlot)
		with.Commands = nil
		if _, _, err := new(Generator).Generate(with); err == nil || !strings.Contains(err.Error(), "proto_files") {
			t.Fatalf("Generate with proto_files and no command gave %v, want a fault that names proto_files", err)
		}
	})
}

// pinnedGo is the Go command that builds the generated service: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// project is a generated service module in a temporary folder.
type project struct {
	t   *testing.T
	dir string
}

func (p *project) write(name string, data []byte) {
	p.t.Helper()
	full := filepath.Join(p.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// goCommand runs the pinned go command in the project, with no workspace, and gives its output.
func (p *project) goCommand(args ...string) (string, error) {
	p.t.Helper()
	cmd := exec.Command(pinnedGo(), args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (p *project) mustGo(args ...string) string {
	p.t.Helper()
	output, err := p.goCommand(args...)
	if err != nil {
		p.t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	return output
}

// newProject generates the service of ctx with the command and the domain-probe generators under out/, and
// extra files beside them, into a module that requires the runtime library from this repository's
// registry/runtime folder.
func newProject(t *testing.T, ctx gen.Context, extra map[string][]byte) *project {
	t.Helper()
	p := &project{t: t, dir: t.TempDir()}
	for name, data := range generated(t, ctx) {
		p.write("out/"+name, data)
	}
	probes := ctx
	probes.OutputNamespace = "probes"
	probes.ComponentConfig = map[string]any{}
	probes.Emitters = nil
	for _, c := range ctx.Commands {
		probes.Emitters = append(probes.Emitters, gen.Emitter{Name: c.Name, Kind: gen.EmitterCommand})
	}
	files, _, err := new(domainprobe.Generator).Generate(probes)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p.write("out/"+f.Path, f.Bytes())
	}
	for name, data := range extra {
		p.write(name, data)
	}
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("go.sum", sums)
	p.write("go.mod", []byte("module "+ctx.ModuleName+"\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.2.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
	return p
}

func TestRelationHelpers(t *testing.T) {
	t.Run("ST-SUP-RC32-OC13-T02 The contract gives typed helpers for each declared relation, and refuses a relation with no written entity of its type", func(t *testing.T) {
		contract := string(generated(t, commandContext(t, declaration(t), commandSlot))["commands/commands.go"])
		for _, want := range []string{
			"func ReserveAndChargeRelateHolder(ctx context.Context, h *ReserveAndChargeHandle, on LoadedOrder, subjectType, subjectID string) error {",
			"func ReserveAndChargeUnrelateHolder(ctx context.Context, h *ReserveAndChargeHandle, on LoadedOrder, subjectType, subjectID string) error {",
			`authz.NewResource(h.Tenant(), "order", on.ID())`,
			`h.Authz().Relate(ctx, on, authz.Tuple{Resource: resource, Relation: "holder",`,
			`h.Authz().Unrelate(ctx, on, authz.Tuple{Resource: resource, Relation: "holder",`,
			"\"github.com/jsell-rh/stego/registry/runtime/authz\"",
		} {
			if !strings.Contains(contract, want) {
				t.Fatalf("the command contract does not hold %q", want)
			}
		}
		svc := declaration(t)
		svc.Commands[0].RelationshipsWritten = []string{"order#holder", "folder#viewer"}
		if _, _, err := new(Generator).Generate(commandContext(t, svc, commandSlot)); err == nil || !strings.Contains(err.Error(), "folder#viewer") {
			t.Fatalf("a relation of a resource type that the command writes no entity of gave %v, want a refusal that names it", err)
		}
		// The relation of a declared relationship, of the subject source field or caller, and of a reference is the
		// runtime's own: no helper may relate or unrelate it (ST-SUP-Q118).
		for relation, why := range map[string]string{
			"hold#owner":   "the relationship owner of the entity Hold, with the subject source field",
			"hold#creator": "the relationship creator of the entity Hold, with the subject source caller",
			"hold#order":   "the relation of the reference order_id of the entity Hold",
		} {
			svc := declaration(t)
			svc.Commands[0].RelationshipsWritten = []string{"order#holder", relation}
			_, _, err := new(Generator).Generate(commandContext(t, svc, commandSlot))
			if err == nil || !strings.Contains(err.Error(), relation) || !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), "runtime writes it alone") {
				t.Errorf("the relation %s gave %v, want a refusal that names it and says the runtime writes it alone", relation, err)
			}
		}
	})
}

// contractRuntime gives the packages of the runtime library that the Go files of the contract package namespace import,
// in the generated files given; the files of its sub-packages are not part of the contract.
func contractRuntime(t *testing.T, files map[string][]byte, namespace string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for name, data := range files {
		if filepath.Dir(name) != namespace || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if path := strings.Trim(spec.Path.Value, `"`); strings.HasPrefix(path, "github.com/jsell-rh/stego/registry/runtime/") {
				found[path] = true
			}
		}
	}
	return found
}

func TestContractRuntimeImports(t *testing.T) {
	t.Run("ST-SUP-RC32-OC03-T05 The command contract imports exactly the runtime packages that the generator gives the fill check, for each declaration", func(t *testing.T) {
		// The test declaration has the pii field card_last4 and a command that writes a relation. The variants take
		// away the relations, the pii and secret fields, or both, so each runtime package is in and out once.
		noRelations := func(svc *types.ServiceDeclaration) {
			for i := range svc.Commands {
				svc.Commands[i].RelationshipsWritten = nil
			}
		}
		noSensitive := func(svc *types.ServiceDeclaration) {
			for i := range svc.Entities {
				for j := range svc.Entities[i].Fields {
					svc.Entities[i].Fields[j].Sensitivity = ""
				}
			}
		}
		for _, c := range []struct {
			name    string
			changes []func(*types.ServiceDeclaration)
			want    int
		}{
			{"pii and relations", nil, 3},
			{"pii and no relations", []func(*types.ServiceDeclaration){noRelations}, 2},
			{"relations and no pii", []func(*types.ServiceDeclaration){noSensitive}, 2},
			{"no pii and no relations", []func(*types.ServiceDeclaration){noRelations, noSensitive}, 1},
		} {
			svc := declaration(t)
			for _, change := range c.changes {
				change(svc)
			}
			found := contractRuntime(t, generated(t, commandContext(t, svc, commandSlot)), "commands")
			listed := ContractRuntimeImports(svc.Entities, svc.Commands)
			if len(listed) != c.want {
				t.Errorf("%s: ContractRuntimeImports gives %v, want %d packages", c.name, listed, c.want)
			}
			for _, path := range listed {
				if !found[path] {
					t.Errorf("%s: ContractRuntimeImports gives %s, which the contract does not import, so the fill check admits more than the contract reaches", c.name, path)
				}
			}
			for path := range found {
				if !slices.Contains(listed, path) {
					t.Errorf("%s: the contract imports %s, which ContractRuntimeImports does not give, so the fill check refuses a fill that imports it", c.name, path)
				}
			}
		}
	})
}

func TestWithinEntities(t *testing.T) {
	t.Run("ST-SUP-RP10-LC37-T04 The generated entities declare each reference held within a field, the readers of the field and the keys of the references to them", func(t *testing.T) {
		entities := []types.Entity{
			{Name: "Project", EntityAuthorization: types.EntityAuthorization{ResourceType: "project", Permissions: map[types.Operation]string{types.OpRead: "view"}},
				Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}},
			{Name: "Sprint", EntityAuthorization: types.EntityAuthorization{ResourceType: "sprint", Permissions: map[types.Operation]string{types.OpRead: "view"},
				References: map[string]types.Reference{"project_id": {Relation: "project", Permission: "edit"}}},
				Fields: []types.Field{{Name: "project_id", Type: types.FieldTypeRef, To: "Project"}}},
			{Name: "Ticket", EntityAuthorization: types.EntityAuthorization{ResourceType: "ticket", Permissions: map[types.Operation]string{types.OpRead: "view"},
				References: map[string]types.Reference{"project_id": {Relation: "project", Permission: "edit"}, "sprint_id": {Relation: "sprint", Permission: "edit", Within: "project_id"}}},
				Fields: []types.Field{{Name: "project_id", Type: types.FieldTypeRef, To: "Project"}, {Name: "sprint_id", Type: types.FieldTypeRef, To: "Sprint", Optional: true}}},
		}
		byName := map[string]types.Entity{}
		for _, e := range entities {
			byName[e.Name] = e
		}
		render := func(name string) string {
			t.Helper()
			p, err := planEntity(byName[name], byName, "tenant_id", Relations{})
			if err != nil {
				t.Fatal(err)
			}
			w := &writer{}
			renderEntity(w, p, "tenant_id")
			return w.String()
		}
		ticket, sprint, project := render("Ticket"), render("Sprint"), render("Project")
		for _, want := range []string{`Field: "sprint_id", Within: "project_id", Key: "tickets_sprint_id_within_fkey", Check: "tickets_sprint_id_wcheck"`, `Within: map[string]func(v Ticket) string{`,
			`"project_id": func(v Ticket) string { return v.ProjectID },`} {
			if !strings.Contains(ticket, want) {
				t.Errorf("the entity Ticket does not declare %s:\n%s", want, ticket)
			}
		}
		if strings.Count(ticket, "Within: \"") != 1 {
			t.Errorf("the entity Ticket holds more than its one reference held within a field:\n%s", ticket)
		}
		for _, want := range []string{`"project_id": func(v Sprint) string { return v.ProjectID },`,
			`{Key: "tickets_sprint_id_within_fkey", Field: "Ticket.sprint_id", Within: "project_id"},`, "before.ProjectID != after.ProjectID"} {
			if !strings.Contains(sprint, want) {
				t.Errorf("the entity Sprint does not declare %s:\n%s", want, sprint)
			}
		}
		if strings.Contains(project, "Within") || strings.Contains(project, "Referrers") {
			t.Errorf("the entity Project declares a within that no reference holds:\n%s", project)
		}
	})
}

func TestFieldCheckEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC18-T01 The generated entity gives the declared constraints of its fields as Check and the field of each constraint as Constraints", func(t *testing.T) {
		two, forty, ten := 2, 40, 10
		zero, top := 0.0, 150.0
		project := types.Entity{Name: "Project", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}}}
		patient := types.Entity{Name: "Patient", Fields: []types.Field{
			{Name: "name", Type: types.FieldTypeString, MinLength: &two, MaxLength: &forty, Pattern: `^[A-Za-z ]+$`},
			{Name: "nick", Type: types.FieldTypeString, MaxLength: &ten, Optional: true},
			{Name: "age", Type: types.FieldTypeInt32, Min: &zero, Max: &top},
			{Name: "score", Type: types.FieldTypeDouble, Min: &zero},
			{Name: "status", Type: types.FieldTypeEnum, Values: []string{"open", "closed"}},
			{Name: "project_id", Type: types.FieldTypeRef, To: "Project"},
			{Name: "note", Type: types.FieldTypeString},
		}}
		byName := map[string]types.Entity{"Project": project, "Patient": patient}
		p, err := planEntity(patient, byName, "tenant_id", Relations{})
		if err != nil {
			t.Fatal(err)
		}
		w := &writer{}
		renderEntity(w, p, "tenant_id")
		text := w.String()
		for _, want := range []string{
			"var patternPatientName = command.MustPattern(\"^[A-Za-z ]+$\")",
			"Check: func(v Patient) (string, string, bool) {",
			"if !command.MinLength(v.Name, 2) {\n\t\t\treturn \"name\", \"shorter than min_length 2\", false",
			"if !command.MaxLength(v.Name, 40) {",
			"if !patternPatientName.MatchString(v.Name) {",
			"if v.Nick != nil {\n\t\t\tif !command.MaxLength(*v.Nick, 10) {",
			"if float64(v.Age) < 0 {\n\t\t\treturn \"age\", \"less than min 0\", false",
			"if float64(v.Age) > 150 {",
			"if !command.Finite(float64(v.Score)) {\n\t\t\treturn \"score\", \"is not a finite number\", false",
			"if !command.OneOf(v.Status, \"open\", \"closed\") {",
			"Constraints: map[string]string{",
			"\"patients_name_check\": \"name\",",
			"\"patients_age_check\": \"age\",",
			"\"patients_status_check\": \"status\",",
			"\"patients_project_id_fkey\": \"project_id\",",
			"\"note\": \"note\",",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the entity Patient does not hold %q:\n%s", want, text)
			}
		}
		// A field with no constraint has no rule, and the pattern of a field has no CHECK of the table.
		if strings.Contains(text, "command.Finite(float64(v.Age))") {
			t.Errorf("the entity checks an integer for a finite value:\n%s", text)
		}
		if strings.Contains(text, "v.Note)") || strings.Contains(text, "patients_note_check") {
			t.Errorf("the entity gives a rule or a CHECK for note, which declares no constraint:\n%s", text)
		}
		if _, err := format.Source([]byte("package commands\n\n" + text)); err != nil {
			t.Fatalf("the entity is not valid Go: %v", err)
		}
		// An entity with no constraint gives no Check; a pattern that is not RE2 is refused.
		if w := (&writer{}); true {
			q, err := planEntity(project, byName, "tenant_id", Relations{})
			if err != nil {
				t.Fatal(err)
			}
			renderEntity(w, q, "tenant_id")
			if strings.Contains(w.String(), "Check:") {
				t.Errorf("an entity with no constraint gives a Check:\n%s", w.String())
			}
		}
		bad := patient
		bad.Fields = []types.Field{{Name: "code", Type: types.FieldTypeString, Pattern: `(?<=a)b`}}
		if _, err := planEntity(bad, map[string]types.Entity{"Patient": bad}, "tenant_id", Relations{}); err == nil || !strings.Contains(err.Error(), "RE2") {
			t.Fatalf("a pattern that is not RE2 gave %v, want a refusal", err)
		}
	})
}

func TestRPCRevocation(t *testing.T) {
	t.Run("ST-SUP-RC32-OC17-T01 The gRPC handler of each command serves it through the runtime's ServeRPC, which answers the revocation metadata", func(t *testing.T) {
		handlers := string(generated(t, commandContext(t, declaration(t), commandSlot))["commands/handlers/handlers.go"])
		if n := strings.Count(handlers, "return command.ServeRPC(rt, "); n == 0 || strings.Contains(handlers, "grpc.SetHeader") {
			t.Fatalf("the handlers call command.ServeRPC %d times and set metadata themselves %v, want each gRPC handler served by ServeRPC alone:\n%s",
				n, strings.Contains(handlers, "grpc.SetHeader"), handlers)
		}
	})
}

func TestUniqueEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC22-T01 The generated entity names the declared fields of each unique index of its table", func(t *testing.T) {
		member := types.Entity{Name: "Member", Fields: []types.Field{
			{Name: "email", Type: types.FieldTypeString, Unique: true},
			{Name: "team", Type: types.FieldTypeString, UniqueComposite: []string{"team", "handle"}},
			{Name: "handle", Type: types.FieldTypeString, UniqueComposite: []string{"team", "handle"}},
			{Name: "note", Type: types.FieldTypeString},
		}}
		p, err := planEntity(member, map[string]types.Entity{"Member": member}, "tenant_id", Relations{})
		if err != nil {
			t.Fatal(err)
		}
		w := &writer{}
		renderEntity(w, p, "tenant_id")
		text := w.String()
		for _, want := range []string{
			"Unique: map[string]string{",
			"\"members_email_key\": \"email\",",
			"\"members_team_handle_key\": \"team, handle\",",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the entity Member does not hold %q:\n%s", want, text)
			}
		}
		// The names are those of the migration of the postgres adapter.
		for name := range postgresadapter.UniqueIndexes(member) {
			if !strings.Contains(text, "\""+name+"\"") {
				t.Errorf("the entity does not name the unique index %s of the migration:\n%s", name, text)
			}
		}
		// An entity with no unique key gives no Unique.
		plain := types.Entity{Name: "Note", Fields: []types.Field{{Name: "text", Type: types.FieldTypeString}}}
		q, err := planEntity(plain, map[string]types.Entity{"Note": plain}, "tenant_id", Relations{})
		if err != nil {
			t.Fatal(err)
		}
		w = &writer{}
		renderEntity(w, q, "tenant_id")
		if strings.Contains(w.String(), "Unique:") {
			t.Errorf("an entity with no unique key gives Unique:\n%s", w.String())
		}
	})
}

func TestSealingEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC23-T01 A field with an open seal step writes and reads both columns, and after the contract only its sealed column", func(t *testing.T) {
		vault := types.Entity{Name: "Vault", Fields: []types.Field{
			{Name: "title", Type: types.FieldTypeString},
			{Name: "note", Type: types.FieldTypeString, Optional: true, Sensitivity: "secret"},
		}}
		changes := []types.SchemaChange{{ID: "vault-note-seal", Kind: "seal", Entity: "Vault", Field: "note", From: "pii"}}
		render := func(history []types.SchemaStep) string {
			entity := gen.SealedNames([]types.Entity{vault}, changes, history)[0]
			p, err := planEntity(entity, map[string]types.Entity{"Vault": entity}, "tenant_id", Relations{})
			if err != nil {
				t.Fatal(err)
			}
			w := &writer{}
			renderEntity(w, p, "tenant_id")
			return w.String()
		}
		open := render([]types.SchemaStep{{Change: "vault-note-seal", Phase: "expand"}})
		for _, want := range []string{
			`sealingNote := command.SealingAt("vaults", "note_sealed", &v.ID, &v.Note)`,
			`err := row.Scan(&v.ID, &v.Title, sealingNote.Clear(), sealingNote.Sealed(), &m.Version, &m.Token)`,
			`return []any{id, v.Title, command.ClearOf(v.Note), command.SealAt("vaults", "note_sealed", id, v.Note)}`,
			`INSERT INTO public.\"vaults\" (\"tenant_id\", \"id\", \"title\", \"note\", \"note_sealed\") VALUES ($1, $2::uuid, $3, $4, $5)`,
			`\"note\" = $6, \"note_sealed\" = $7`,
			`"note": "note",`, `"note_sealed": "note",`,
			"Sealed: true,",
			// The runtime takes the clear column again only while the checkpoint of its change is there.
			"Clear: []string{\"note\"},\n\t\tChanges: []string{\"vault-note-seal\"},",
		} {
			if !strings.Contains(open, want) {
				t.Errorf("the entity with an open seal does not hold %s:\n%s", want, open)
			}
		}
		closed := render([]types.SchemaStep{{Change: "vault-note-seal", Phase: "expand"}, {Change: "vault-note-seal", Phase: "contract"}})
		if strings.Contains(closed, "SealingAt") || strings.Contains(closed, "ClearOf") || strings.Contains(closed, `\"note\",`) ||
			!strings.Contains(closed, `command.OpenAt("vaults", "note_sealed", &v.ID, &v.Note)`) {
			t.Errorf("the entity after the contract of its seal still names the clear column:\n%s", closed)
		}
	})
}

// walletEntities are the entities of the tests of the counter and the event fields: Wallet with a compare-only secret
// pin with the limit of 5 wrong attempts and no event fields, and Transaction with the event fields wallet_id,
// kind and amount at version 2.
func walletEntities() []types.Entity {
	two := 2
	return []types.Entity{
		{Name: "Wallet", Fields: []types.Field{
			{Name: "label", Type: types.FieldTypeString},
			{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret", CompareOnly: true, Attempts: &types.FieldAttempts{Limit: 5}},
		}},
		{Name: "Transaction", Fields: []types.Field{
			{Name: "wallet_id", Type: types.FieldTypeString},
			{Name: "kind", Type: types.FieldTypeEnum, Values: []string{"deposit", "withdrawal"}},
			{Name: "amount", Type: types.FieldTypeInt64},
			{Name: "note", Type: types.FieldTypeString, Optional: true},
		}, Events: &types.EntityEvents{Fields: []string{"wallet_id", "kind", "amount"}, Version: &two}},
	}
}

// buildEntityFile compiles the entity file of the package data against the runtime library of this repository.
func buildEntityFile(t *testing.T, source []byte) {
	t.Helper()
	p := &project{t: t, dir: t.TempDir()}
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("go.sum", sums)
	p.write("go.mod", []byte("module example.com/wallets\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.2.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.write("data/entities.go", source)
	p.mustGo("mod", "tidy")
	p.mustGo("vet", "./...")
}

func TestCounterEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC24-T01 A compare-only field with a limit gets its counter, CountedAt in the scan and HashCountedOf in the values", func(t *testing.T) {
		source, err := EntityFile("data", []string{"Wallet", "Transaction"}, walletEntities(), "tenant_id", Relations{})
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		for _, want := range []string{
			`var walletPinCounter = command.Must(command.NewCounter(command.CounterSpec{Entity: "Wallet", Field: "pin", Limit: 5}))`,
			`command.CountedAt(walletPinCounter, &v.ID, &v.Pin)`,
			`command.HashCountedOf(walletPinCounter, id, v.Pin)`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the entity file does not hold %s:\n%s", want, text)
			}
		}
		buildEntityFile(t, source)
	})
}

func TestResetEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC24-T02 A command that declares the reset of a counted field gets its typed reset helper and its Resets, and no other command does", func(t *testing.T) {
		svc := declaration(t)
		svc.Entities[0].Fields = append(svc.Entities[0].Fields, types.Field{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret",
			CompareOnly: true, Attempts: &types.FieldAttempts{Limit: 3, UnlockAfter: "1h"}})
		unlock := types.Command{Name: "unlock-pin", Writes: []string{"Order"}, ResourceType: "order", Permission: "unlock_pin",
			ResourceMember: "orderId", Route: "POST /orders/{orderId}/pin:unlock", Fill: types.FillNames{"unlock-pin"},
			ResetsAttempts: []string{"Order.pin"}}
		svc.Commands = append(svc.Commands, unlock)
		ctx := commandContext(t, svc, commandSlot)
		ctx.Inputs["proto/orders.proto"] = []byte(ordersProto + "\nmessage UnlockPinRequest {\n  string order_id = 1;\n}\n\nmessage UnlockPinResponse {}\n")
		var all strings.Builder
		for _, data := range generated(t, ctx) {
			all.Write(data)
		}
		text := all.String()
		for _, want := range []string{
			`var orderPinCounter = command.Must(command.NewCounter(command.CounterSpec{Entity: "Order", Field: "pin", Limit: 3, UnlockAfter: 3600 * time.Second}))`,
			"func UnlockPinResetOrderPinAttempts(ctx context.Context, h *UnlockPinHandle, on LoadedOrder) error {",
			"return command.ResetAttempts(ctx, h, orderPinCounter, on)",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the generated code does not hold %s", want)
			}
		}
		if n := len(regexp.MustCompile(`Resets:\s+\[\]string\{"Order.pin"\},`).FindAllString(text, -1)); n != 1 || strings.Count(text, "Resets:") != 1 || strings.Contains(text, "func ReserveAndChargeReset") {
			t.Errorf("the generated code holds %d declarations of resets, or a reset helper of reserve-and-charge, want one and none", n)
		}
		const head = "package %s\n\nimport (\n\t\"context\"\n\n\tcommands \"example.com/orders/out/commands\"\n)\n\n"
		reset := fmt.Sprintf(head, "unlockpin") + "func New() commands.UnlockPinFill {\n\treturn func(ctx context.Context, req *commands.UnlockPinRequest, h *commands.UnlockPinHandle) (*commands.UnlockPinResponse, error) {\n" +
			"\t\torders, err := commands.OrderRepository(h)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n" +
			"\t\torder, err := orders.GetForUpdate(ctx, req.OrderId)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n" +
			"\t\treturn &commands.UnlockPinResponse{}, commands.UnlockPinResetOrderPinAttempts(ctx, h, order)\n\t}\n}\n"
		// The fill of reserve-and-charge, which may check the PIN, cannot call the reset of unlock-pin.
		wrong := fmt.Sprintf(head, "wrongreset") + "func Run(ctx context.Context, h *commands.ReserveAndChargeHandle, order commands.LoadedOrder) error {\n" +
			"\treturn commands.UnlockPinResetOrderPinAttempts(ctx, h, order)\n}\n"
		p := newProject(t, ctx, map[string][]byte{"fills/reserve-and-charge/fill.go": serviceFill, "fills/unlock-pin/fill.go": []byte(reset),
			"fills/wrongreset/fill.go": []byte(wrong)})
		if output, err := p.goCommand("build", "./out/...", "./fills/unlock-pin"); err != nil {
			t.Errorf("the code with the reset does not build: %v\n%s", err, output)
		}
		if output, err := p.goCommand("build", "./fills/wrongreset"); err == nil || !strings.Contains(output, "cannot use h") {
			t.Errorf("the reset of unlock-pin in the fill of reserve-and-charge gave %v, want a refusal of the handle:\n%s", err, output)
		}
		for name, resets := range map[string][]string{"a field with no attempts": {"Order.sku"}, "an entity it does not write": {"Hold.pin"}, "the field twice": {"Order.pin", "Order.pin"}} {
			bad := declaration(t)
			bad.Entities[0].Fields = svc.Entities[0].Fields
			c := unlock
			c.ResetsAttempts = resets
			bad.Commands = append(bad.Commands, c)
			ctx := commandContext(t, bad, commandSlot)
			ctx.Inputs["proto/orders.proto"] = []byte(ordersProto + "\nmessage UnlockPinRequest {\n  string order_id = 1;\n}\n\nmessage UnlockPinResponse {}\n")
			if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "ST-DECL-INV104") {
				t.Errorf("a reset of %s gave %v, want a refusal that names ST-DECL-INV104", name, err)
			}
		}
	})
}

func TestEventDataEmission(t *testing.T) {
	t.Run("ST-SUP-RC32-OC25-T01 An entity with Events gets EventData and EventVersion, and one with none gets neither", func(t *testing.T) {
		source, err := EntityFile("data", []string{"Wallet", "Transaction"}, walletEntities(), "tenant_id", Relations{})
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		wallet, transaction, _ := strings.Cut(text, "// TransactionEntity is")
		for _, want := range []string{
			`EventData: func(v Transaction) map[string]any {`,
			`return map[string]any{"wallet_id": v.WalletID, "kind": v.Kind, "amount": v.Amount}`,
			"EventVersion: 2,",
		} {
			if !strings.Contains(transaction, want) {
				t.Errorf("the declaration of Transaction does not hold %s:\n%s", want, transaction)
			}
		}
		if strings.Contains(wallet, "EventData") || strings.Contains(wallet, "EventVersion") {
			t.Errorf("the declaration of Wallet gives event data:\n%s", wallet)
		}
		buildEntityFile(t, source)
	})
}

func TestTypedRepository(t *testing.T) {
	t.Run("ST-SUP-RC32-OC26-T01 A parent where the runtime refuses one does not compile, and a move takes its parents", func(t *testing.T) {
		const head = "package %s\n\nimport (\n\t\"context\"\n\n\tcommands \"example.com/orders/out/commands\"\n)\n\n" +
			"func Run(ctx context.Context, h *commands.ReserveAndChargeHandle, id string) error {\n" +
			"\torders, err := commands.OrderRepository(h)\n\tif err != nil {\n\t\treturn err\n\t}\n" +
			"\tholds, err := commands.HoldRepository(h)\n\tif err != nil {\n\t\treturn err\n\t}\n" +
			"\torder, err := orders.Get(ctx, id)\n\tif err != nil {\n\t\treturn err\n\t}\n" +
			"\tfrom, err := holds.GetForUpdate(ctx, id)\n\tif err != nil {\n\t\treturn err\n\t}\n" +
			"\t_, _ = order, from\n%s\treturn nil\n}\n"
		cases := []struct{ pkg, body, refusal string }{
			// The calls of a fill with no wrong parent, and a move of the reference of a Hold, compile.
			{"good", "\tif _, err := holds.Update(ctx, from, from.Value()); err != nil {\n\t\treturn err\n\t}\n" +
				"\tif _, err := holds.Move(ctx, from, from.Value(), order); err != nil {\n\t\treturn err\n\t}\n" +
				"\tif _, err := orders.GetForDelete(ctx, id); err != nil {\n\t\treturn err\n\t}\n" +
				"\tif err := holds.Delete(ctx, from); err != nil {\n\t\treturn err\n\t}\n", ""},
			// Walkthrough 15, W15-B3: the parent of an update that keeps its references.
			{"update", "\tif _, err := holds.Update(ctx, from, from.Value(), order); err != nil {\n\t\treturn err\n\t}\n", "too many arguments in call to holds.Update"},
			{"lock", "\tif _, err := holds.GetForUpdate(ctx, id, order); err != nil {\n\t\treturn err\n\t}\n", "too many arguments in call to holds.GetForUpdate"},
			{"locked", "\tif _, err := orders.GetForDelete(ctx, id, order); err != nil {\n\t\treturn err\n\t}\n", "too many arguments in call to orders.GetForDelete"},
			{"delete", "\tif err := holds.Delete(ctx, from, order); err != nil {\n\t\treturn err\n\t}\n", "too many arguments in call to holds.Delete"},
			// An entity with no reference has no move.
			{"move", "\tif _, err := orders.Move(ctx, from, from.Value()); err != nil {\n\t\treturn err\n\t}\n", "orders.Move undefined"},
		}
		extra := map[string][]byte{"fills/reserve-and-charge/fill.go": serviceFill}
		for _, c := range cases {
			extra["fills/"+c.pkg+"/fill.go"] = []byte(fmt.Sprintf(head, c.pkg, c.body))
		}
		p := newProject(t, commandContext(t, declaration(t), commandSlot), extra)
		for _, c := range cases {
			output, err := p.goCommand("build", "./fills/"+c.pkg)
			switch {
			case c.refusal == "" && err != nil:
				t.Errorf("%s: go build gave %v:\n%s", c.pkg, err, output)
			case c.refusal != "" && (err == nil || !strings.Contains(output, c.refusal)):
				t.Errorf("%s: go build gave %v, want the refusal %q:\n%s", c.pkg, err, c.refusal, output)
			}
		}
	})
}

func TestRouteDocumentFile(t *testing.T) {
	t.Run("ST-SUP-RC32-OC29-T02 With no rest-api the command component writes the route document that it gets, also with no command, and with rest-api none", func(t *testing.T) {
		document := []byte(`{"openapi":"3.0.3","paths":{}}`)
		ctx := commandContext(t, declaration(t), commandSlot)
		ctx.ServiceOpenAPI = document
		if got := generated(t, ctx)["commands/openapi.json"]; !bytes.Equal(got, document) {
			t.Errorf("with commands, commands/openapi.json is %q, want the route document", got)
		}
		// A service whose routes are queries alone, such as the rpc-service scaffold.
		ctx.Commands, ctx.ComponentConfig = nil, map[string]any{}
		if got := generated(t, ctx)["commands/openapi.json"]; !bytes.Equal(got, document) {
			t.Errorf("with no command, commands/openapi.json is %q, want the route document", got)
		}
		// With rest-api the compiler gives the command component no document, and it writes none.
		ctx = commandContext(t, declaration(t), commandSlot)
		if _, written := generated(t, ctx)["commands/openapi.json"]; written {
			t.Error("with no route document the command component writes commands/openapi.json")
		}
	})
}
