package command

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/types"
)

//go:embed testdata/command_test.go
var serviceChecks []byte

// serviceFill is the fill reserve-and-charge of the generated service, which the service writes at
// fills/reserve-and-charge/fill.go.
var serviceFill = []byte(`// Package reservecharge is the fill reserve-and-charge of the generated test service: it reserves the stock of an
// order and records a charge, through its command handle only. It imports the command slot contract and the
// standard library fill allow list only (ST-EXT-UC01-R03).
package reservecharge

import (
	"context"
	"encoding/json"

	commands "example.com/orders/out/commands"
)

// Behavior is what the fill does; a test of the service changes it.
var Behavior commands.ReserveAndChargeFill = ReserveAndCharge

// New gives the fill of the command reserve-and-charge.
func New() commands.ReserveAndChargeFill {
	return func(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
		return Behavior(ctx, request, h)
	}
}

// ReserveAndCharge writes one Reservation and one Charge of the order, stages one billing message for the charge
// and counts the reservation. It refuses more than ten items as short stock.
func ReserveAndCharge(ctx context.Context, request *commands.ReserveAndChargeRequest, h *commands.ReserveAndChargeHandle) (*commands.ReserveAndChargeResponse, error) {
	if request.Qty > 10 {
		return nil, commands.ReserveAndChargeShortStock
	}
	reservations, err := commands.ReservationRepository(h)
	if err != nil {
		return nil, err
	}
	reservation, err := reservations.Create(ctx, commands.Reservation{OrderID: request.OrderId, Sku: request.Sku, Qty: request.Qty}, nil)
	if err != nil {
		return nil, err
	}
	charges, err := commands.ChargeRepository(h)
	if err != nil {
		return nil, err
	}
	charge, err := charges.Create(ctx, commands.Charge{OrderID: request.OrderId, AmountCents: request.AmountCents, CardLast4: request.CardLast4}, nil)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"charge": charge.ID(), "amount_cents": request.AmountCents})
	if err != nil {
		return nil, err
	}
	if err := h.Effects().Stage(ctx, "billing", "charge-requested", charge, payload); err != nil {
		return nil, err
	}
	if request.Qty > 5 {
		h.Probe().ReservationsMade("large")
	} else {
		h.Probe().ReservationsMade("small")
	}
	return &commands.ReserveAndChargeResponse{ReservationId: reservation.ID(), ChargeId: charge.ID()}, nil
}
`)

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and gives
// its endpoint and preshared key. It removes the container when the test ends.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if endpoint := os.Getenv("STEGO_TEST_SPICEDB"); endpoint != "" {
		return endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-command-spicedb-"+hex.EncodeToString(suffix), "command-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", spiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	var port string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			port = strings.TrimSpace(strings.Split(string(out), "\n")[0])
			break
		}
	}
	if port == "" {
		t.Fatal("the SpiceDB container publishes no port")
	}
	return port, key
}

// tablesMigration is the migration 001_initial that the PostgreSQL adapter creates for the declaration on the
// runtime data path (ST-SUP-RC01-OC41).
func tablesMigration(t *testing.T, ctx gen.Context) []byte {
	t.Helper()
	adapter := gen.Context{OutputNamespace: "storage", ModuleName: ctx.ModuleName, ServiceName: ctx.ServiceName,
		Tenancy: ctx.Tenancy, Entities: ctx.Entities, PeerNamespaces: map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy", "command": "commands", "query": "queries"},
		ComponentConfig: map[string]any{"schema_generation": "tables-v1", "writer_lease": "off"}}
	files, _, err := new(postgresadapter.Generator).Generate(adapter)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "storage/migrations/001_initial.sql" {
			return f.Content
		}
	}
	t.Fatal("the PostgreSQL adapter gave no migration 001_initial")
	return nil
}

// commandService generates the test service with its fill and its checks, once for the test binary.
func commandService(t *testing.T) *project {
	t.Helper()
	ctx := commandContext(t, declaration(t), commandSlot)
	return newProject(t, ctx, map[string][]byte{
		"fills/reserve-and-charge/fill.go": serviceFill,
		"check/command_test.go":            serviceChecks,
		"check/tables.sql":                 tablesMigration(t, ctx),
	})
}

// checkRun is the one run of the checks of the generated service: one go test of the check package with every
// check, so the service is built and its test binary run once.
type checkRun struct {
	once   sync.Once
	output string
	err    error
}

// runCheck tells whether the check id of the generated service ran and passed, against the local PostgreSQL server
// of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. The first call runs every check. A missing server
// fails the test: a missing result is not a pass.
func runCheck(t *testing.T, p *project, run *checkRun, endpoint, key, id string) {
	t.Helper()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	run.once.Do(func() {
		cmd := exec.Command(pinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^(TestCommand|TestCommandLists)$", "./check")
		cmd.Dir = p.dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key)
		output, err := cmd.CombinedOutput()
		run.output, run.err = string(output), err
	})
	if !ownPass(run.output, "TestCommand", id) && !ownPass(run.output, "TestCommandLists", id) {
		t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", id, run.err, run.output)
	}
}

// TestCommandService generates a service with the command reserve-and-charge, builds it with the pinned Go and runs
// its HTTP and gRPC handlers against PostgreSQL and SpiceDB.
func TestCommandService(t *testing.T) {
	p := commandService(t)
	p.mustGo("vet", "./...")
	endpoint, key := startSpiceDB(t)
	run := &checkRun{}
	t.Run("ST-SUP-RC32-OC04-T01 The fill runs in one transaction with the caller's tenant", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC04-T01") })
	t.Run("ST-SUP-RC32-OC04-T02 An unauthenticated command never reaches the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC04-T02") })
	t.Run("ST-SUP-RC32-OC05-T01 A fill error leaves nothing behind", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC05-T01") })
	t.Run("ST-SUP-RC32-OC05-T02 A successful command commits its writes and effects together", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC05-T02") })
	t.Run("ST-SUP-RC29-OC05-T01 Create, update and delete each enqueue the declared tuples", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC29-OC05-T01") })
	t.Run("ST-SUP-RC29-OC05-T02 The tuples commit and roll back with the row", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC29-OC05-T02") })
	t.Run("ST-SUP-RC29-OC05-T03 The tuples of a row join the transaction's revocation", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC29-OC05-T03") })
	t.Run("ST-SUP-RC32-OC06-T01 A denied command answers 404 and does not call the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC06-T01") })
	t.Run("ST-SUP-RC32-OC06-T02 A write under a parent that the principal may not act on is refused", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC06-T02") })
	t.Run("ST-SUP-RC32-OC07-T01 A committed, a denied and a failed command each leave one command audit row", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC07-T01") })
	t.Run("ST-SUP-RC32-OC08-T01 A retried command replays and does not call the fill again", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC08-T01") })
	t.Run("ST-SUP-RC32-OC08-T02 A command without an Idempotency-Key gets 400 and does not call the fill", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC08-T02") })
	t.Run("ST-SUP-RC32-OC09-T01 A command fill's error text reaches no response, audit row or completion log", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC09-T01") })
	t.Run("ST-SUP-RC32-OC10-T01 A command that loses a conflict gets 409 conflict, an audit row, and runs again on a retry with the same key", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC10-T01") })
	t.Run("ST-SUP-RC32-OC11-T01 HTTP and gRPC take one request message", func(t *testing.T) { runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC11-T01") })
	t.Run("ST-SUP-RC32-OC16-T02 A command fill reads its generated list with a parameter, also after an added column", func(t *testing.T) {
		runCheck(t, p, run, endpoint, key, "ST-SUP-RC32-OC16-T02")
	})
}

// ownPass tells whether output, the text of go test -v, holds the pass line of the subtest of id itself, directly
// under a test whose name matches parent, a regular expression. A subtest one level down has the same name prefix,
// but go test indents its line four more spaces, so its pass never counts as the pass of its parent
// (ST-STD-CODE-R07).
func ownPass(output, parent, id string) bool {
	return regexp.MustCompile(`(?m)^    --- PASS: ` + parent + `/` + regexp.QuoteMeta(id) + `_`).MatchString(output)
}

func TestOwnPass(t *testing.T) {
	t.Run("ST-STD-CODE-R07-T01 In the command wrapper, a pass of a subtest under the subtest of a record is not its pass", func(t *testing.T) {
		output := "=== RUN   TestCommand/ST-X-T01_a\n        --- PASS: TestCommand/ST-X-T01_a/child (0.00s)\n    --- FAIL: TestCommand/ST-X-T01_a (0.00s)\n" +
			"    --- PASS: TestCommand/ST-X-T02_b (0.00s)\n        --- FAIL: TestCommand/ST-X-T02_b/child (0.00s)\n    --- PASS: TestCommand/ST-X-T03_c/d (0.00s)\n"
		if ownPass(output, "TestCommand", "ST-X-T01") {
			t.Fatal("a failed subtest counts as passed because a subtest under it passed")
		}
		if !ownPass(output, "TestCommand", "ST-X-T02") || !ownPass(output, "TestCommand", "ST-X-T03") {
			t.Fatal("a passed subtest, also one whose title holds a slash, does not count as passed")
		}
	})
}

//go:embed testdata/counter_test.go
var counterChecks []byte

// releaseHoldFill is the fill release-hold of the counter service: it reads the order of the request, with no lock
// or locked as the request says, and checks its counted PIN, so a check of the PIN from the resource type hold runs
// as a fill writes it.
var releaseHoldFill = []byte(`// Package releasehold is the fill release-hold of the counter test service: it reads the order of the request and
// checks its PIN (decision ST-SUP-Q209 (d)).
package releasehold

import (
	"context"

	commands "example.com/orders/out/commands"
)

// New gives the fill of the command release-hold.
func New() commands.ReleaseHoldFill {
	return func(ctx context.Context, request *commands.ReleaseHoldRequest, h *commands.ReleaseHoldHandle) (*commands.ReleaseHoldResponse, error) {
		orders, err := commands.OrderRepository(h)
		if err != nil {
			return nil, err
		}
		var order commands.LoadedOrder
		if request.Locked {
			order, err = orders.GetForUpdate(ctx, request.OrderId)
		} else {
			order, err = orders.Get(ctx, request.OrderId)
		}
		if err != nil {
			return nil, err
		}
		if !order.Value().Pin.Matches(request.Pin) {
			return nil, commands.ReleaseHoldPinWrong
		}
		return &commands.ReleaseHoldResponse{}, nil
	}
}
`)

// unlockPinFill is the fill unlock-pin of the counter service: it resets the count of the PIN of the order by its ID.
var unlockPinFill = []byte(`// Package unlockpin is the fill unlock-pin of the counter test service.
package unlockpin

import (
	"context"

	commands "example.com/orders/out/commands"
)

// New gives the fill of the command unlock-pin.
func New() commands.UnlockPinFill {
	return func(ctx context.Context, request *commands.UnlockPinRequest, h *commands.UnlockPinHandle) (*commands.UnlockPinResponse, error) {
		return &commands.UnlockPinResponse{}, commands.UnlockPinResetOrderPinAttemptsByID(ctx, h, request.OrderId)
	}
}
`)

// counterSchema is the part of the schema of the counter service that the test declaration does not have: who may
// unlock the PIN of an order, and who may release on an order and on a hold.
const counterSchema = `	permission view = owner + viewer
	relation support: user
	relation releaser: user
	permission unlock_pin = support
	permission release = releaser
}`

// counterService generates the test service with the counted PIN of Order, which the command unlock-pin resets, and
// the command release-hold of the resource type hold, which writes Order and checks its PIN (decision ST-SUP-Q209
// (d)), with their fills and the checks of the service and of the counter.
func counterService(t *testing.T) *project {
	t.Helper()
	svc := declaration(t)
	svc.Entities[0].Fields = append(svc.Entities[0].Fields, types.Field{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret",
		CompareOnly: true, Attempts: &types.FieldAttempts{Limit: 3, UnlockAfter: "1h"}})
	svc.Commands = append(svc.Commands,
		types.Command{Name: "unlock-pin", Writes: []string{"Order"}, ResourceType: "order", Permission: "unlock_pin",
			ResourceMember: "orderId", Route: "POST /orders/{orderId}/pin:unlock", Fill: types.FillNames{"unlock-pin"}, ResetsAttempts: []string{"Order.pin"}},
		types.Command{Name: "release-hold", Writes: []string{"Hold", "Order"}, ResourceType: "hold", Permission: "release",
			ResourceMember: "holdId", Route: "POST /holds/{holdId}/releases", Fill: types.FillNames{"release-hold"},
			Refusals: []types.Refusal{{Name: "pin_wrong", Status: 422, Code: "hold.pin_wrong"}}})
	ctx := commandContext(t, svc, commandSlot)
	ctx.Inputs["proto/orders.proto"] = []byte(ordersProto + "\nmessage UnlockPinRequest {\n  string order_id = 1;\n}\n\nmessage UnlockPinResponse {}\n" +
		"\nmessage ReleaseHoldRequest {\n  string hold_id = 1;\n  string order_id = 2;\n  string pin = 3;\n  bool locked = 4;\n}\n\nmessage ReleaseHoldResponse {}\n")
	schema := strings.Replace(schemaZed, "\tpermission view = owner + viewer\n}", counterSchema, 1)
	schema = strings.Replace(schema, "\tpermission reserve = owner + creator\n}", "\tpermission reserve = owner + creator\n\tpermission release = owner + creator\n}", 1)
	ctx.Inputs["authz/schema.zed"] = []byte(schema)
	checks := strings.Replace(string(serviceChecks), "\tpermission view = owner + viewer\n}", counterSchema, 1)
	checks = strings.Replace(checks, "\tpermission reserve = owner + creator\n}", "\tpermission reserve = owner + creator\n\tpermission release = owner + creator\n}", 1)
	if checks == string(serviceChecks) || schema == schemaZed {
		t.Fatal("the schema of the counter service is the schema of the test declaration")
	}
	return newProject(t, ctx, map[string][]byte{
		"fills/reserve-and-charge/fill.go": serviceFill,
		"fills/unlock-pin/fill.go":         unlockPinFill,
		"fills/release-hold/fill.go":       releaseHoldFill,
		"check/command_test.go":            []byte(checks),
		"check/counter_test.go":            counterChecks,
		"check/tables.sql":                 tablesMigration(t, ctx),
	})
}

// TestCounterService generates the service with the counted PIN and runs its check of the counter against a local
// PostgreSQL server and a local SpiceDB container. CI runs it; a host with neither fails it, as a missing result is
// not a pass.
func TestCounterService(t *testing.T) {
	t.Run("ST-SUP-RC32-OC24-T03 A generated service answers 500 and charges nothing for a check of a counted PIN that a command resets on a row on which the command did not decide its permission, and checks it on a row that it locked", func(t *testing.T) {
		if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
			t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
		}
		p := counterService(t)
		endpoint, key := startSpiceDB(t)
		cmd := exec.Command(pinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestCounterReset$", "./check")
		cmd.Dir = p.dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key)
		output, err := cmd.CombinedOutput()
		if !ownPass(string(output), "TestCounterReset", "ST-SUP-RC32-OC24-T03") {
			t.Fatalf("the check of the counter of the generated service did not run and pass (go test: %v):\n%s", err, output)
		}
	})
}
