package worker

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	gotypes "go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/domainprobe"
	"github.com/910-software-solutions/stego/internal/localtest"
	"github.com/910-software-solutions/stego/internal/types"
	"gopkg.in/yaml.v3"
)

const testModule = "example.com/orders"

var yes = true

// schemaZed is the schema of the test declaration: the type tenant holds one relation for each worker.
const schemaZed = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition service_account {}

definition tenant {
	relation worker_sync_partner_orders: service_account
	relation worker_apply_partner_replies: service_account
	permission sync = worker_sync_partner_orders
	permission apply = worker_apply_partner_replies
}

definition order {
	relation owner: user | service_account
	relation syncer: service_account
	permission sync = syncer
	permission apply = syncer
	permission reserve = owner
}
`

// orderEntity is the owned entity Order with the fields status and synced, and Lock at most 10.
const orderEntity = `name: Order
resource_type: order
lock_at_most: 10
permissions: {create: public, read: sync}
public_reasons: {create: a worker creates orders}
fields:
  - {name: status, type: string}
  - {name: synced, type: bool}
`

// unsyncedList is the list unsynced of sync-partner-orders: the orders that are not synced and whose status is not
// done, which the fill locks.
var unsyncedList = types.WorkerList{Name: "unsynced", Entity: "Order", Lock: true, Filter: []types.ListTerm{
	{Field: "synced", Op: types.ListEquals, Value: false}, {Field: "status", Op: types.ListNotEquals, Value: "done"}}}

// workerContext is the context of a service with the entity Order, the command reserve that stages for
// partner-replies, and the workers sync-partner-orders (every 1m, with the list unsynced) and apply-partner-replies
// (consumes partner-replies), each with the fill of the slot given.
func workerContext(slot string) gen.Context {
	var order types.Entity
	if err := yaml.Unmarshal([]byte(orderEntity), &order); err != nil {
		panic(err)
	}
	return gen.Context{
		ModuleName:      testModule,
		OutDirName:      "out",
		OutputNamespace: "workers",
		ServiceName:     "orders",
		Entities:        []types.Entity{order},
		Commands: []gen.Command{{Command: types.Command{Name: "reserve", Permission: "reserve", ResourceType: "order",
			Effects: []types.Effect{{Destination: "partner-replies", Name: "reserved"}}}}},
		Workers: []gen.Worker{
			{Worker: types.Worker{Name: "sync-partner-orders", Trigger: "every 1m", Writes: []string{"Order"}, ResourceType: "order",
				Permission: "sync", Fill: types.FillNames{"sync-orders"}, Lists: []types.WorkerList{unsyncedList}}, FillSlot: slot},
			{Worker: types.Worker{Name: "apply-partner-replies", Trigger: "consumes partner-replies", Writes: []string{"Order"},
				ResourceType: "order", Permission: "apply", Fill: types.FillNames{"apply-replies"}}, FillSlot: slot},
		},
		Authorization:  &types.Authorization{SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Tenancy:        &gen.Tenancy{Key: "tenant_id"},
		Destinations:   []gen.Destination{{Name: "partner-replies", Component: "worker", MustLand: &yes, Idempotent: &yes}},
		PeerNamespaces: map[string]string{"domain-probe": "probes"},
		Inputs:         map[string][]byte{"authz/schema.zed": []byte(schemaZed)},
	}
}

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

// The fills of the test service, one of each trigger.
var (
	syncFill = []byte(`package syncorders

import (
	"context"
	"time"

	workers "example.com/orders/out/workers"
)

func New() workers.SyncPartnerOrdersFill {
	return func(ctx context.Context, tick time.Time, h *workers.SyncPartnerOrdersHandle) error { return nil }
}
`)
	applyFill = []byte(`package applyreplies

import (
	"context"
	"encoding/json"

	workers "example.com/orders/out/workers"
)

func New() workers.ApplyPartnerRepliesFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.ApplyPartnerRepliesHandle) error {
		return nil
	}
}
`)
)

// project is a generated module in a temporary folder.
type project struct {
	t   *testing.T
	dir string
}

func (p *project) write(name string, data []byte) {
	p.t.Helper()
	full := filepath.Join(p.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *project) mustGo(args ...string) string {
	p.t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = p.dir
	cmd.Env = localtest.GoEnv("GOFLAGS=-mod=mod")
	output, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	return string(output)
}

// newProject generates the service of ctx with the worker and the domain-probe generators under out/, with the fills
// of both workers and the extra files, which may take the place of a fill, into a module that requires the runtime
// library from this repository's registry/runtime folder.
func newProject(t *testing.T, ctx gen.Context, extra ...map[string][]byte) *project {
	t.Helper()
	p := &project{t: t, dir: t.TempDir()}
	for name, data := range generated(t, ctx) {
		p.write("out/"+name, data)
	}
	probes := ctx
	probes.OutputNamespace = "probes"
	probes.Emitters = nil
	for _, w := range ctx.Workers {
		probes.Emitters = append(probes.Emitters, gen.Emitter{Name: w.Name, Kind: gen.EmitterWorker})
	}
	files, _, err := new(domainprobe.Generator).Generate(probes)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p.write("out/"+f.Path, f.Bytes())
	}
	p.write("fills/sync-orders/fill.go", syncFill)
	p.write("fills/apply-replies/fill.go", applyFill)
	for _, files := range extra {
		for name, data := range files {
			p.write(name, data)
		}
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
	p.write("go.mod", []byte("module "+testModule+"\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire github.com/910-software-solutions/stego/registry/runtime v0.2.0\n\nreplace github.com/910-software-solutions/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
	localtest.CheckSums(t, p.dir)
	return p
}

// exportImporter is a go/types importer that reads the export data that go list -export gives for each package of
// the generated module.
func exportImporter(p *project, fset *token.FileSet, pattern string) gotypes.Importer {
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

// handleOf tells whether typ is *command.Handle of the runtime library.
func handleOf(typ gotypes.Type) bool {
	pointer, ok := typ.(*gotypes.Pointer)
	if !ok {
		return false
	}
	named, _ := gotypes.Unalias(pointer.Elem()).(*gotypes.Named)
	return named != nil && named.Obj().Pkg().Path() == "github.com/910-software-solutions/stego/registry/runtime/command" && named.Obj().Name() == "Handle"
}

// forbidden tells whether typ names a type of database/sql, net, os or an authorization provider client.
func forbidden(typ gotypes.Type) bool {
	text := typ.String()
	for _, prefix := range []string{"database/sql.", "net.", "net/", "os.", "github.com/authzed/"} {
		if strings.Contains(text, prefix) {
			return true
		}
	}
	return false
}

func TestContract(t *testing.T) {
	t.Run("ST-SUP-RC35-OC01-T01 The worker fill gets its input and one command handle", func(t *testing.T) {
		p := newProject(t, workerContext(workerSlot))
		// The runners bind each fill: the whole module builds.
		p.mustGo("build", "./...")
		fset := token.NewFileSet()
		pkg, err := exportImporter(p, fset, "./out/workers").Import(testModule + "/out/workers")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{
			"SyncPartnerOrdersFill":   {"context.Context", "time.Time", "handle"},
			"ApplyPartnerRepliesFill": {"context.Context", "encoding/json.RawMessage", "string", "handle"},
		}
		for name, params := range want {
			obj := pkg.Scope().Lookup(name)
			if obj == nil {
				t.Fatalf("the contract has no slot type %s", name)
			}
			sig, ok := obj.Type().Underlying().(*gotypes.Signature)
			if !ok || sig.Params().Len() != len(params) {
				t.Fatalf("%s is %s, want a function of %v", name, obj.Type().Underlying(), params)
			}
			for i, param := range params {
				got := sig.Params().At(i).Type()
				if forbidden(got) {
					t.Errorf("%s takes %s, which reaches storage, the network or the authorization provider", name, got)
				}
				if param == "handle" {
					if !handleOf(got) {
						t.Errorf("parameter %d of %s is %s, want *command.Handle", i, name, got)
					}
					continue
				}
				if got.String() != param {
					t.Errorf("parameter %d of %s is %s, want %s", i, name, got, param)
				}
			}
		}
	})

	t.Run("ST-SUP-RC35-OC02-T01 Each worker gets one runner, and a fill of the wrong slot is refused", func(t *testing.T) {
		runners := string(generated(t, workerContext(workerSlot))["workers/runners/runners.go"])
		for _, want := range []string{
			`var syncPartnerOrdersWorker = command.Must(command.NewWorker(`,
			`var applyPartnerRepliesWorker = command.Must(command.NewWorker(`,
			`command.TickFill[probes.SyncPartnerOrdersProbe](fills.SyncPartnerOrders)`,
			`command.MessageFill[json.RawMessage, probes.ApplyPartnerRepliesProbe](fills.ApplyPartnerReplies)`,
			`worker.Register(outbox.Destination{Name: "partner-replies", Idempotent: true, MustLand: true, ActiveTenants: true,`,
			`map[string]string{"reserve": "reserve"}`,
			`env.Lease("sync-partner-orders", 15*time.Second)`,
		} {
			if !strings.Contains(runners, want) {
				t.Errorf("the runners do not hold %s:\n%s", want, runners)
			}
		}
		if n := strings.Count(runners, "command.NewTicker(") + strings.Count(runners, "worker.Register("); n != 2 {
			t.Errorf("the runners start %d runners, want one for each of the two workers", n)
		}
		ctx := workerContext(workerSlot)
		ctx.Workers[0].FillSlot = "command.command"
		files, _, err := new(Generator).Generate(ctx)
		if err == nil || !strings.Contains(err.Error(), "sync-partner-orders") || !strings.Contains(err.Error(), "sync-orders") || files != nil {
			t.Fatalf("a fill of the command slot gave %v and %d files, want a refusal that names the worker and the fill", err, len(files))
		}
	})

	t.Run("ST-SUP-RC35-OC15-T01 The contract holds each declared list with the statements of the entity of the release", func(t *testing.T) {
		files := generated(t, workerContext(workerSlot))
		// gofmt aligns the keys of the literal, so the check reads the source with one space between words.
		spaces := regexp.MustCompile(`[ \t]+`)
		contract, runners := spaces.ReplaceAllString(string(files["workers/workers.go"]), " "), spaces.ReplaceAllString(string(files["workers/runners/runners.go"]), " ")
		columns := `"id"::text, "status", "synced", "version", "authz_token"`
		from := `SELECT ` + columns + ` FROM public."orders" WHERE "tenant_id" = $1`
		for _, want := range []string{
			`var SyncPartnerOrdersUnsynced = command.Must(command.NewList(OrderEntity, command.ListSpec{Name: "unsynced",`,
			`Read: ` + strconv.Quote(from+` AND "synced" = $3 AND "status" <> $4 ORDER BY "id" LIMIT $2`),
			`Lock: ` + strconv.Quote(from+` AND "id" = ANY($2::text[]::uuid[]) AND "synced" = $3 AND "status" <> $4 ORDER BY "id" FOR NO KEY UPDATE`),
			`Values: command.DeclaredValues(command.ListValue{Constant: false}, command.ListValue{Constant: "done"})}))`,
			// The spec takes the lists that the contract package held when it started (security review of PR #416, L3).
			`var syncPartnerOrdersDeclaredLists = command.Snapshot(SyncPartnerOrdersUnsynced)`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the contract does not hold %s:\n%s", want, contract)
			}
		}
		if !strings.Contains(spaces.ReplaceAllString(string(files["workers/entities.go"]), " "), "LockAtMost: 10,") {
			t.Error("the entity declaration does not hold the Lock at most of Order")
		}
		if !strings.Contains(runners, `Lists: workers.SyncPartnerOrdersDeclaredLists(),`) || strings.Count(runners, "Lists:") != 1 {
			t.Errorf("the runners do not give sync-partner-orders, and only it, its list:\n%s", runners)
		}

		// The release that adds a field generates the list with its column, and a term on the time of the run.
		ctx := workerContext(workerSlot)
		ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "due_at", Type: types.FieldTypeTimestamp, ClientSet: true},
			types.Field{Name: "estimate", Type: types.FieldTypeInt64})
		list := unsyncedList
		list.Lock = false
		list.Filter = append(slices.Clone(list.Filter), types.ListTerm{Field: "due_at", Op: types.ListLessThan, Now: true},
			types.ListTerm{Field: "estimate", Op: types.ListGreaterThanOrEquals, Value: 3}, types.ListTerm{Field: "due_at", Op: types.ListIsSet})
		ctx.Workers[0].Lists = []types.WorkerList{list}
		contract = spaces.ReplaceAllString(string(generated(t, ctx)["workers/workers.go"]), " ")
		wider := `SELECT "id"::text, "status", "synced", "due_at", "estimate", "version", "authz_token" FROM public."orders" WHERE "tenant_id" = $1`
		for _, want := range []string{
			`Read: ` + strconv.Quote(wider+` AND "synced" = $3 AND "status" <> $4 AND "due_at" < $5 AND "estimate" >= $6 AND "due_at" IS NOT NULL ORDER BY "id" LIMIT $2`),
			`Values: command.DeclaredValues(command.ListValue{Constant: false}, command.ListValue{Constant: "done"}, command.ListValue{RunTime: true}, command.ListValue{Constant: int64(3)})}))`,
		} {
			if !strings.Contains(contract, want) {
				t.Errorf("the contract of the release with the new fields does not hold %s:\n%s", want, contract)
			}
		}
		if strings.Contains(contract, "Lock: ") {
			t.Error("a list that does not lock has a Lock statement")
		}

		// Generation refuses a list that checking refuses, naming the worker and the list.
		for name, change := range map[string]func(*types.WorkerList){
			"an entity that the worker does not write": func(l *types.WorkerList) { l.Entity = "Invoice" },
			"a field that the entity does not declare": func(l *types.WorkerList) {
				l.Filter = []types.ListTerm{{Field: "colour", Op: types.ListEquals, Value: "red"}}
			},
			"a value of another type": func(l *types.WorkerList) {
				l.Filter = []types.ListTerm{{Field: "synced", Op: types.ListEquals, Value: "yes"}}
			},
		} {
			ctx := workerContext(workerSlot)
			list := unsyncedList
			change(&list)
			ctx.Workers[0].Lists = []types.WorkerList{list}
			if files, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "sync-partner-orders") || !strings.Contains(err.Error(), "unsynced") || files != nil {
				t.Errorf("%s: generation gave %v, want a refusal that names the worker and the list", name, err)
			}
		}
		ctx = workerContext(workerSlot)
		ctx.Entities[0].LockAtMost = nil
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "Lock at most") {
			t.Errorf("a locked list of an entity with no Lock at most gave %v", err)
		}
	})

	t.Run("ST-SUP-RC35-OC15-T03 A worker list with parameters has a With function that takes them as typed Go arguments", func(t *testing.T) {
		spaces := regexp.MustCompile(`[ \t]+`)
		for _, keep := range []int{0, 1} {
			ctx := workerContext(workerSlot)
			ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "due_at", Type: types.FieldTypeTimestamp, ClientSet: true})
			list := unsyncedList
			list.Lock = false
			list.Filter = []types.ListTerm{{Field: "status", Op: types.ListEquals, Param: "status"}, {Field: "due_at", Op: types.ListLessThan, Param: "type"},
				{Field: "synced", Op: types.ListEquals, Value: false}}
			ctx.Workers = ctx.Workers[keep : keep+1]
			ctx.Workers[0].Lists = []types.WorkerList{list}
			ctx.Workers[0].Writes = []string{"Order"}
			files, _, err := new(Generator).Generate(ctx)
			if err != nil {
				t.Fatalf("worker %s: %v", ctx.Workers[0].Name, err)
			}
			var contract string
			for _, f := range files {
				if f.Path == "workers/workers.go" {
					contract = spaces.ReplaceAllString(string(f.Content), " ")
				}
			}
			name := mixedCase(ctx.Workers[0].Name) + "Unsynced"
			for _, want := range []string{
				`AND \"status\" = $3 AND \"due_at\" < $4 AND \"synced\" = $5 ORDER BY \"id\" LIMIT $2`,
				`Values: command.DeclaredValues(command.ListValue{Param: "status"}, command.ListValue{Param: "type"}, command.ListValue{Constant: false})}))`,
				// A parameter named as a Go keyword takes the suffix Value, and a timestamp takes a time.Time.
				`func ` + name + `With(status string, typeValue time.Time) *command.List[Order] {`,
				`return ` + name + `.With(status, typeValue)`,
				" \"time\"\n",
			} {
				if !strings.Contains(contract, want) {
					t.Errorf("worker %s: the contract does not hold %q:\n%s", ctx.Workers[0].Name, want, contract)
				}
			}
		}
	})

	t.Run("ST-SUP-RC35-OC02-T03 The runners and the contract of a service whose workers all have one trigger import only what they use", func(t *testing.T) {
		for _, keep := range []int{0, 1} {
			ctx := workerContext(workerSlot)
			ctx.Workers = ctx.Workers[keep : keep+1]
			if keep == 0 {
				ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.NewReplacer("\trelation worker_apply_partner_replies: service_account\n", "", "\tpermission apply = worker_apply_partner_replies\n", "").Replace(schemaZed))}
			} else {
				ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.NewReplacer("\trelation worker_sync_partner_orders: service_account\n", "", "\tpermission sync = worker_sync_partner_orders\n", "").Replace(schemaZed))}
			}
			files := generated(t, ctx)
			for _, name := range []string{"workers/runners/runners.go", "workers/workers.go"} {
				if unused := unusedImports(t, name, files[name]); len(unused) > 0 {
					t.Errorf("with the one worker %s, %s imports %v and does not use it", ctx.Workers[0].Name, name, unused)
				}
			}
		}
	})

	t.Run("ST-SUP-RC35-OC06-T03 The runners name the tables of the consumers' writes for the worker role, and report a refused privilege", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		partner := ctx.Entities[0]
		partner.Name = "Partner"
		ctx.Entities = append(ctx.Entities, partner)
		// The ticker writes Partner too; only the consumer's write of Order needs a grant of the worker role.
		ctx.Workers[0].Writes = []string{"Order", "Partner"}
		runners := string(generated(t, ctx)["workers/runners/runners.go"])
		for _, want := range []string{
			"func ConsumerTables() []string {\n\treturn []string{\"orders\"}\n}",
			`return command.ObserveConsumer(ctx, observe("apply-partner-replies"), command.RunConsumer(ctx, rt, applyPartnerRepliesWorker, tx, d, decodeMessage, fill))`,
		} {
			if !strings.Contains(runners, want) {
				t.Errorf("the runners do not hold %s:\n%s", want, runners)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC16-T01 The worker that consumes a subscription takes its messages with the subscription as the stager and its own permission", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		ctx.Commands = nil
		ctx.Workers[1].Trigger = "consumes shop-orders"
		ctx.Destinations = []gen.Destination{{Name: "shop-orders", Component: "worker", MustLand: &yes, Idempotent: &yes}}
		ctx.Subscriptions = []types.Subscription{{Name: "shop-orders", Source: "shop", Events: []string{"shop.order.created"}}}
		runners := string(generated(t, ctx)["workers/runners/runners.go"])
		if !strings.Contains(runners, `map[string]string{"shop-orders": "apply"},`) || !strings.Contains(runners, "Stagers:") {
			t.Errorf("the consuming worker does not take the subscription as its stager with its own permission:\n%s", runners)
		}
	})

	t.Run("ST-SUP-RC35-OC13-T02 Generation refuses a schema without the relation of a worker on tenant", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.NewReplacer("\trelation worker_apply_partner_replies: service_account\n", "", "\tpermission apply = worker_apply_partner_replies\n", "").Replace(schemaZed))}
		files, _, err := new(Generator).Generate(ctx)
		if err == nil || !strings.Contains(err.Error(), "tenant#worker_apply_partner_replies") || files != nil {
			t.Fatalf("a schema without tenant#worker_apply_partner_replies gave %v and %d files, want a refusal that names it", err, len(files))
		}
		ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.Replace(schemaZed, "worker_sync_partner_orders: service_account", "worker_sync_partner_orders: user", 1))}
		if _, _, err := new(Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "tenant#worker_sync_partner_orders") {
			t.Fatalf("a relation of the subject type user gave %v, want a refusal that names tenant#worker_sync_partner_orders", err)
		}
	})
}

// unusedImports gives the imports of the Go source that no selector of the file names, as the compiler refuses them.
func unusedImports(t *testing.T, name string, source []byte) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatalf("%s does not parse: %v", name, err)
	}
	used := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok {
				used[ident.Name] = true
			}
		}
		return true
	})
	var unused []string
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		local := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			local = spec.Name.Name
		}
		if local != "_" && !used[local] {
			unused = append(unused, path)
		}
	}
	return unused
}

func TestRootCreateSchema(t *testing.T) {
	t.Run("ST-SUP-RC35-OC17-T01 Generation refuses a schema whose type tenant lacks the permission of a worker that can create a row with no parent", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.Replace(schemaZed, "\tpermission apply = worker_apply_partner_replies\n", "", 1))}
		files, _, err := new(Generator).Generate(ctx)
		if err == nil || files != nil {
			t.Fatalf("a schema whose tenant lacks apply gave %v and %d files, want a refusal", err, len(files))
		}
		for _, want := range []string{"apply-partner-replies", "Order", "permission apply", "tenant", "ST-SUP-RC35-OC17"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
		// A worker whose entity has a required reference to an entity with a resource type needs no permission on
		// tenant; with the permissions of both workers the schema passes.
		generated(t, workerContext(workerSlot))
		ctx = workerContext(workerSlot)
		ctx.Entities = append(ctx.Entities, types.Entity{Name: "Line", EntityAuthorization: types.EntityAuthorization{ResourceType: "order"},
			Fields: []types.Field{{Name: "order_id", Type: types.FieldTypeRef, To: "Order"}}})
		for i := range ctx.Workers {
			ctx.Workers[i].Writes = []string{"Line"}
		}
		ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.NewReplacer("\tpermission apply = worker_apply_partner_replies\n", "",
			"\tpermission sync = worker_sync_partner_orders\n", "").Replace(schemaZed))}
		if _, _, err := new(Generator).Generate(ctx); err != nil && strings.Contains(err.Error(), "ST-SUP-RC35-OC17") {
			t.Fatalf("a worker that writes only an entity with a required parent gave %v, want no refusal of OC17", err)
		}
	})
}

func TestContractRuntimeImports(t *testing.T) {
	t.Run("ST-SUP-RC35-OC03-T04 The worker contract imports exactly the runtime packages that the generator gives the fill check, for each declaration", func(t *testing.T) {
		// The entity Order has no pii field; the second case adds one, and the third a secret field, whose type is of the
		// sealing runtime (ST-SUP-RC32-OC20).
		for _, c := range []struct {
			name, sensitivity string
			want              int
		}{{"no pii field", "", 1}, {"a written entity with a pii field", "pii", 2}, {"a written entity with a secret field", "secret", 3}} {
			ctx := workerContext(workerSlot)
			if c.sensitivity != "" {
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: "email", Type: "string", Sensitivity: c.sensitivity})
			}
			found := map[string]bool{}
			for name, data := range generated(t, ctx) {
				if filepath.Dir(name) != "workers" || !strings.HasSuffix(name, ".go") {
					continue
				}
				file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, spec := range file.Imports {
					if path, _ := strconv.Unquote(spec.Path.Value); strings.HasPrefix(path, "github.com/910-software-solutions/stego/registry/runtime/") {
						found[path] = true
					}
				}
			}
			var workers []types.Worker
			for _, w := range ctx.Workers {
				workers = append(workers, w.Worker)
			}
			listed := ContractRuntimeImports(ctx.Entities, workers)
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

// readsSchema is the schema of the test of the worker reads: the type tenant holds the relation of each worker, and
// the permission record of the workers on wallets and totals.
const readsSchema = `definition user {}

definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition service_account {}

definition tenant {
	relation worker_sum_balances: service_account
	relation worker_record_wallets: service_account
	permission record = worker_sum_balances + worker_record_wallets
}

definition wallet {
	relation tenant: tenant
	permission view = tenant->record
	permission record = tenant->record
}

definition total {
	relation tenant: tenant
	permission view = tenant->record
	permission record = tenant->record
}
`

// readsEntities are Wallet, with a readable secret api_key and a compare-only secret pin, and Total.
const readsEntities = `- name: Wallet
  resource_type: wallet
  permissions: {read: view, list: view}
  fields:
    - {name: label, type: string}
    - {name: balance, type: int64}
    - {name: api_key, type: string, sensitivity: secret}
    - {name: pin, type: string, sensitivity: secret, compare_only: true}
- name: Total
  resource_type: total
  permissions: {read: view, list: view}
  fields:
    - {name: balance, type: int64}
`

// readsContext is a service whose worker sum-balances, every 1h, reads Wallet and writes Total with the list funded
// over Wallet, and whose worker record-wallets consumes wallet-changes and reads Wallet.
func readsContext() gen.Context {
	var entities []types.Entity
	if err := yaml.Unmarshal([]byte(readsEntities), &entities); err != nil {
		panic(err)
	}
	funded := types.WorkerList{Name: "funded", Entity: "Wallet", Filter: []types.ListTerm{{Field: "balance", Op: types.ListGreaterThan, Value: 0}}}
	return gen.Context{
		ModuleName: testModule, OutDirName: "out", OutputNamespace: "workers", ServiceName: "wallets", Entities: entities,
		Workers: []gen.Worker{
			{Worker: types.Worker{Name: "sum-balances", Trigger: "every 1h", Writes: []string{"Total"}, Reads: []string{"Wallet"},
				ResourceType: "total", Permission: "record", Fill: types.FillNames{"sync-orders"}, Lists: []types.WorkerList{funded}}, FillSlot: workerSlot},
			{Worker: types.Worker{Name: "record-wallets", Trigger: "consumes wallet-changes", Reads: []string{"Wallet"},
				ResourceType: "wallet", Permission: "record", Fill: types.FillNames{"apply-replies"}}, FillSlot: workerSlot},
		},
		Authorization:  &types.Authorization{SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"},
		Tenancy:        &gen.Tenancy{Key: "tenant_id"},
		Destinations:   []gen.Destination{{Name: "wallet-changes", Component: "worker", MustLand: &yes, Idempotent: &yes}},
		PeerNamespaces: map[string]string{"domain-probe": "probes"},
		Inputs:         map[string][]byte{"authz/schema.zed": []byte(readsSchema)},
	}
}

func TestWorkerReads(t *testing.T) {
	t.Run("ST-SUP-RC35-OC19-T01 A worker with reads gets a view, a read-only declaration and a reader of each read entity", func(t *testing.T) {
		ctx := readsContext()
		files := generated(t, ctx)
		squash := func(b []byte) string { return regexp.MustCompile(`[ \t]+`).ReplaceAllString(string(b), " ") }
		contract, entities, runners := squash(files["workers/workers.go"]), squash(files["workers/entities.go"]), squash(files["workers/runners/runners.go"])
		view, _, _ := strings.Cut(entities[strings.Index(entities, "type WalletView struct"):], "}")
		for _, want := range []string{"ID string", "Label string", "Balance int64"} {
			if !strings.Contains(view, want) {
				t.Errorf("WalletView does not hold %s:\n%s", want, view)
			}
		}
		if strings.Contains(view, "ApiKey") || strings.Contains(view, "Pin") {
			t.Errorf("WalletView holds a secret field:\n%s", view)
		}
		for _, want := range []string{"type LoadedWalletView = command.Loaded[WalletView]", "var WalletViewEntity = command.Must(command.NewEntity(command.EntitySpec[WalletView]{",
			"ReadOnly: true,", "func WalletReader[P any](h *command.Handle[P]) (*command.Reader[WalletView], error)"} {
			if !strings.Contains(entities, want) {
				t.Errorf("the entity file does not hold %s:\n%s", want, entities)
			}
		}
		selectRow := entities[strings.Index(entities, "ReadOnly: true,"):]
		selectRow, _, _ = strings.Cut(selectRow[strings.Index(selectRow, "Select:"):], "\n")
		if strings.Contains(selectRow, "api_key") || strings.Contains(selectRow, "pin") {
			t.Errorf("the Select of the view reads a secret column: %s", selectRow)
		}
		if strings.Contains(entities, "WalletRepository") || strings.Contains(entities, "\"github.com/910-software-solutions/stego/registry/runtime/sealed\"") {
			t.Errorf("the entity file gives a repository of Wallet, which no worker writes, or imports the sealed package:\n%s", entities)
		}
		list := contract[strings.Index(contract, "var SumBalancesFunded"):]
		list, _, _ = strings.Cut(list, "}))")
		if !strings.Contains(list, "command.NewList(WalletViewEntity") || strings.Contains(list, "Lock:") || strings.Contains(list, "api_key") {
			t.Errorf("the list funded is not a list of the view with no lock and no secret column:\n%s", list)
		}
		for _, want := range []string{"Reads: []command.Declared{workers.WalletViewEntity}", "return []command.Declared{workers.TotalEntity, workers.WalletViewEntity}",
			"func ConsumerReadTables() map[string][]string {\n return map[string][]string{\"wallets\": {\"tenant_id\", \"id\", \"label\", \"balance\", \"version\", \"authz_token\"}}\n}"} {
			if !strings.Contains(runners, want) {
				t.Errorf("the runners do not hold %s:\n%s", want, runners)
			}
		}
		fill := []byte(`package syncorders

import (
	"context"
	"time"

	workers "example.com/orders/out/workers"
)

func New() workers.SumBalancesFill {
	return func(ctx context.Context, tick time.Time, h *workers.SumBalancesHandle) error {
		wallets, err := workers.WalletReader(h)
		if err != nil {
			return err
		}
		if _, err := wallets.Get(ctx, "id"); err != nil {
			return err
		}
		funded, err := wallets.List(ctx, workers.SumBalancesFunded, 10)
		if err != nil {
			return err
		}
		var total int64
		for _, w := range funded {
			total += w.Value().Balance
		}
		totals, err := workers.TotalRepository(h)
		if err != nil {
			return err
		}
		_, err = totals.Create(ctx, workers.Total{Balance: total})
		return err
	}
}
`)
		apply := []byte(`package applyreplies

import (
	"context"
	"encoding/json"

	workers "example.com/orders/out/workers"
)

func New() workers.RecordWalletsFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.RecordWalletsHandle) error {
		_, err := workers.WalletReader(h)
		return err
	}
}
`)
		p := newProject(t, ctx, map[string][]byte{"fills/sync-orders/fill.go": fill, "fills/apply-replies/fill.go": apply})
		p.mustGo("vet", "./...")
	})
}
