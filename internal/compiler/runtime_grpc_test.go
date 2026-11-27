package compiler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/localtest"
)

// The proof of the gRPC application on the runtime data path (ST-SUP-RC07-OC19, ST-SUP-RC33-OC08): a service of the
// rpc-service archetype with the command and query components, generated from testdata/runtime_grpc/project/service.yaml
// with stego apply, built with the pinned Go, and run against a local PostgreSQL server and a local SpiceDB container.

// grpcFixture is the folder of the gRPC test service: its project files and the check that runs in its module.
const grpcFixture = "testdata/runtime_grpc"

// rpcRuntimeArchetype is the components list of the rpc-service archetype of the registry copy on the runtime data
// path: the command and query generators with the PostgreSQL adapter, the tenancy, domain-probe and outbox components
// that the runtime wiring calls, and the gRPC application in place of grpc-processes.
const rpcRuntimeArchetype = "components:\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - grpc-application\n  - otel-tracing\n  - health-check\n"

// newGRPCProject copies the registry of this repository with the rpc-service archetype on the runtime data path, each
// alpha component of the test staged as ga, and the project files of the fixture into a new folder.
func newGRPCProject(t *testing.T, binary string) *wiringProject {
	t.Helper()
	p := &wiringProject{t: t, dir: t.TempDir(), binary: binary}
	if err := os.CopyFS(filepath.Join(p.dir, "registry"), os.DirFS(filepath.Join("..", "..", "registry"))); err != nil {
		t.Fatal(err)
	}
	// The shipped rpc-service is on the runtime data path (ST-SUP-Q104 (a)); the copy holds its component list as
	// the test states it, so a later change of the archetype shows here.
	archetype := filepath.Join("registry", "archetypes", "rpc-service", "archetype.yaml")
	p.write(archetype, regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(p.read(archetype), rpcRuntimeArchetype))
	sunset := regexp.MustCompile(`sunset: "?[0-9-]+"?\n`)
	for _, name := range []string{"tenancy", "command", "query", "domain-probe", "audit-log", "idempotency", "spicedb-authz"} {
		file := filepath.Join("registry", "components", name, "component.yaml")
		p.write(file, sunset.ReplaceAllString(strings.Replace(p.read(file), "stage: alpha\n", "stage: ga\n", 1), ""))
	}
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join(grpcFixture, "project"))); err != nil {
		t.Fatal(err)
	}
	p.write("go.mod", "module "+wiringModule+"\n\ngo 1.26.11\n")
	return p
}

// grpcCheckIDs are the checks of the generated service that TestRuntimeGRPC runs, each with the ID of the test record
// it proves.
var grpcCheckIDs = []string{
	"ST-SUP-RC27-OC11-T01 A gRPC call of a tenant that is not provisioned is refused before any handler",
	"ST-SUP-RC07-OC19-T01 A gRPC call runs its command or query fill with the same handling as HTTP",
	"ST-SUP-RC33-OC08-T01 A stream query gets a gRPC stream and no HTTP route",
	"ST-SUP-RC32-OC15-T03 A command on the service-level resource creates a row with no parent over gRPC and HTTP",
}

// TestRuntimeGRPC generates the gRPC test service with stego apply, reads its routes and its gRPC registration, builds
// it with the pinned Go, and runs the check of testdata/runtime_grpc/check in its module once against the local
// PostgreSQL server of localtest and a local SpiceDB container. A missing server fails the test: a
// missing result is not a pass.
func TestRuntimeGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	// The check turns TLS on in the PostgreSQL server with a certificate of its own and resets it at its end, as the
	// check of TestRuntimeWiring does, so the two must not run at the same time: the test runs in parallel, and its
	// check holds postgresTLS (runTLSCheck).
	t.Parallel()
	postgres := "STEGO_TEST_POSTGRES_DSN=" + localtest.PostgresDSN(t)
	p := newGRPCProject(t, stegoBinary(t))
	t.Run("ST-DECL-INV92-T stego validate refuses a collection of the rpc-service, which no component serves", func(t *testing.T) {
		declared := p.read("service.yaml")
		p.write("service.yaml", declared+"collections:\n  orders:\n    entity: Order\n    operations: [create, read, list]\n")
		defer p.write("service.yaml", declared)
		output, err := p.stegoRun("validate")
		if err == nil || !strings.Contains(output, "collection-served") || !strings.Contains(output, `collection "orders"`) || !strings.Contains(output, "rpc-service") {
			t.Fatalf("stego validate with a collection of the rpc-service gave %v:\n%s", err, output)
		}
	})
	p.stego("apply")
	t.Run("ST-SUP-RC33-OC08-T01 The stream query is marked from the proto file and gets no HTTP route", func(t *testing.T) {
		main, server, serve := p.read("out/main.go"), p.read("out/grpcapi/server.go"), p.read("out/queries/serve/serve.go")
		if strings.Contains(main, "/orders/watch") || strings.Contains(serve, `"GET /api/shop/v1/orders/watch"`) {
			t.Error("the stream query watch-orders has an HTTP route")
		}
		if !strings.Contains(main, `"GET /api/shop/v1/orders/{orderId}/summary"`) {
			t.Error("the unary query get-order-summary has no HTTP route")
		}
		for _, want := range []string{"queryserve.ServeWatchOrdersStream[", "pb0.RegisterShopServer(registrar, srv0)", "pb0.UnimplementedShopServer"} {
			if !strings.Contains(server, want) {
				t.Errorf("the gRPC registration lacks %q:\n%s", want, server)
			}
		}
		if !strings.Contains(serve, "query.RunStream(") || !strings.Contains(serve, "func ServeWatchOrdersStream[") {
			t.Error("the stream query is not served through its Stream function on query.RunStream")
		}
	})
	check, err := os.ReadFile(filepath.Join(grpcFixture, "check", "grpc_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/grpccheck/grpc_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	p.goCommand("vet", "./out/...")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	output, runErr := runTLSCheck(p.dir, "./out/grpccheck", localtest.GoEnv(wiringPath(), postgres, "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_GRPC_BINARY="+binary),
		"-test.count=1", "-test.timeout=300s", "-test.v", "-test.run", "^TestGRPC$")
	for _, id := range grpcCheckIDs {
		t.Run(id, func(t *testing.T) {
			name := strings.ReplaceAll(id, " ", "_")
			if !strings.Contains(string(output), "--- PASS: TestGRPC/"+name+" ") {
				t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(id, " ", 2)[0], runErr, output)
			}
		})
	}
}

func TestGRPCSecretPathParameter(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the stego binary")
	}
	t.Parallel()
	stego := stegoBinary(t)
	t.Run("ST-SUP-RC20-OC54-T05 A service of the rpc-service archetype, with no rest-api, refuses a path parameter that the proto marks debug_redact", func(t *testing.T) {
		plain := newGRPCProject(t, stego)
		if output, err := plain.stegoRun("plan"); err != nil {
			t.Fatalf("the plan of the fixture with a plain path parameter gave %v:\n%s", err, output)
		}
		marked := newGRPCProject(t, stego)
		proto := marked.read("proto/shop.proto")
		changed := strings.Replace(proto, "message AddNoteRequest {\n  string order_id = 1;", "message AddNoteRequest {\n  string order_id = 1 [debug_redact = true];", 1)
		if changed == proto {
			t.Fatal("the fixture proto has no member order_id of AddNoteRequest")
		}
		marked.write("proto/shop.proto", changed)
		output, err := marked.stegoRun("plan")
		if err == nil || !strings.Contains(output, "a secret member cannot be a path parameter") || !strings.Contains(output, "orderId") {
			t.Errorf("the plan with a debug_redact path parameter gave %v, want the refusal that names orderId:\n%s", err, output)
		}
		if _, err := os.Stat(filepath.Join(marked.dir, "out")); !os.IsNotExist(err) {
			t.Errorf("the refused plan wrote the output folder: %v", err)
		}
	})
}
