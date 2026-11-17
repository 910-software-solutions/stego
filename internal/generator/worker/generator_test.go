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
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/domainprobe"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

const testModule = "example.com/orders"

var yes = true

// schemaZed is the schema of the test declaration: the type tenant holds one relation for each worker.
const schemaZed = `definition user {}

definition service_account {}

definition tenant {
	relation worker_sync_partner_orders: service_account
	relation worker_apply_partner_replies: service_account
}

definition order {
	relation owner: user | service_account
	relation syncer: service_account
	permission sync = syncer
	permission apply = syncer
	permission reserve = owner
}
`

// orderEntity is the owned entity Order with the fields status and synced.
const orderEntity = `name: Order
resource_type: order
permissions: {create: public, read: sync}
public_reasons: {create: a worker creates orders}
fields:
  - {name: status, type: string}
  - {name: synced, type: bool}
`

// workerContext is the context of a service with the entity Order, the command reserve that stages for
// partner-replies, and the workers sync-partner-orders (every 1m) and apply-partner-replies (consumes
// partner-replies), each with the fill of the slot given.
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
				Permission: "sync", Fill: types.FillNames{"sync-orders"}}, FillSlot: slot},
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
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
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
		"\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.2.0\n\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n"))
	p.mustGo("mod", "tidy")
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
	return named != nil && named.Obj().Pkg().Path() == "github.com/jsell-rh/stego/registry/runtime/command" && named.Obj().Name() == "Handle"
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

	t.Run("ST-SUP-RC35-OC02-T03 The runners and the contract of a service whose workers all have one trigger import only what they use", func(t *testing.T) {
		for _, keep := range []int{0, 1} {
			ctx := workerContext(workerSlot)
			ctx.Workers = ctx.Workers[keep : keep+1]
			if keep == 0 {
				ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.Replace(schemaZed, "\trelation worker_apply_partner_replies: service_account\n", "", 1))}
			} else {
				ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.Replace(schemaZed, "\trelation worker_sync_partner_orders: service_account\n", "", 1))}
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

	t.Run("ST-SUP-RC35-OC13-T02 Generation refuses a schema without the relation of a worker on tenant", func(t *testing.T) {
		ctx := workerContext(workerSlot)
		ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte(strings.Replace(schemaZed, "\trelation worker_apply_partner_replies: service_account\n", "", 1))}
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
