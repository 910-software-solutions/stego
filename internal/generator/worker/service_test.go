package worker

import (
	_ "embed"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/localtest"
)

// TestMain removes the PostgreSQL container that localtest started for the tests of the package, if any
// (ST-STD-LOCAL-R04, R05).
func TestMain(m *testing.M) { os.Exit(localtest.Main(m)) }

//go:embed testdata/worker_test.go
var serviceChecks []byte

// serviceSyncFill is the fill sync-orders of the generated service for the checks: it does what Behavior does, which
// a check sets. It imports the worker slot contract and the standard library fill allow list only.
var serviceSyncFill = []byte(`// Package syncorders is the fill sync-orders of the generated test service.
package syncorders

import (
	"context"
	"time"

	workers "example.com/orders/out/workers"
)

// Behavior is what the fill does; a check of the service sets it.
var Behavior workers.SyncPartnerOrdersFill = func(context.Context, time.Time, *workers.SyncPartnerOrdersHandle) error { return nil }

// New gives the fill of the worker sync-partner-orders.
func New() workers.SyncPartnerOrdersFill {
	return func(ctx context.Context, tick time.Time, h *workers.SyncPartnerOrdersHandle) error {
		return Behavior(ctx, tick, h)
	}
}
`)

// serviceApplyFill is the fill apply-replies of the generated service for the checks: it does what Behavior does,
// which a check sets, and nothing when it is nil. It imports the worker slot contract and the standard library fill
// allow list only.
var serviceApplyFill = []byte(`// Package applyreplies is the fill apply-replies of the generated test service.
package applyreplies

import (
	"context"
	"encoding/json"

	workers "example.com/orders/out/workers"
)

// Behavior is what the fill does; a check of the service sets it.
var Behavior workers.ApplyPartnerRepliesFill

// New gives the fill of the worker apply-partner-replies.
func New() workers.ApplyPartnerRepliesFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.ApplyPartnerRepliesHandle) error {
		if Behavior == nil {
			return nil
		}
		return Behavior(ctx, message, effect, h)
	}
}
`)

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

// checkRun is the one run of the checks of the generated service: one go test of the check package with every
// check, so the service is built and its test binary run once.
type checkRun struct {
	once   sync.Once
	output string
	err    error
}

// runCheck tells whether the check id of the generated service ran and passed, against the local PostgreSQL server
// of STEGO_TEST_POSTGRES_DSN. The first call runs every check. A missing server fails the test: a missing result is
// not a pass.
func runCheck(t *testing.T, p *project, run *checkRun, id string) {
	t.Helper()
	postgres := "STEGO_TEST_POSTGRES_DSN=" + localtest.PostgresDSN(t)
	run.once.Do(func() {
		cmd := exec.Command("go", "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestWorker$", "./check")
		cmd.Dir = p.dir
		cmd.Env = localtest.GoEnv("GOFLAGS=-mod=mod", postgres)
		output, err := cmd.CombinedOutput()
		run.output, run.err = string(output), err
	})
	if !ownPass(run.output, "TestWorker", id) {
		t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", id, run.err, run.output)
	}
}

// ownPass tells whether output, the text of go test -v, holds the pass line of the subtest of id itself, directly
// under a test whose name matches parent. A subtest one level down has the same name prefix, but go test indents
// its line four more spaces, so its pass never counts as the pass of its parent (ST-STD-CODE-R07).
func ownPass(output, parent, id string) bool {
	return regexp.MustCompile(`(?m)^    --- PASS: ` + parent + `/` + regexp.QuoteMeta(id) + `_`).MatchString(output)
}

// TestService generates the service of the workers sync-partner-orders and apply-partner-replies, builds it, and
// runs the generated runners as the worker mode starts them, against PostgreSQL and a test clock.
func TestService(t *testing.T) {
	ctx := workerContext(workerSlot)
	p := newProject(t, ctx, map[string][]byte{
		"fills/sync-orders/fill.go":   serviceSyncFill,
		"fills/apply-replies/fill.go": serviceApplyFill,
		"check/worker_test.go":        serviceChecks,
		"check/tables.sql":            tablesMigration(t, ctx),
	})
	p.mustGo("vet", "./...")
	run := &checkRun{}
	t.Run("ST-SUP-RC35-OC04-T01 An interval worker runs once for each tenant a tick and skips an overlapping tick", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC04-T01") })
	t.Run("ST-SUP-RC35-OC05-T01 Of two instances only the lease holder runs a single worker", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC05-T01") })
	t.Run("ST-SUP-RC35-OC06-T01 A staged message reaches its consuming worker once, and a failed run is retried", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC06-T01") })
	t.Run("ST-SUP-RC35-OC07-T01 A slow tenant ends at its own deadline and the next tenant gets its own", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC07-T01") })
	t.Run("ST-SUP-RC35-OC08-T01 A panic in one worker run does not stop the service or the next run", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC08-T01") })
	t.Run("ST-SUP-RC35-OC09-T01 A worker run is one span with a fixed outcome, and a failure is one built-in event", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC09-T01") })
	t.Run("ST-SUP-RC35-OC15-T02 A worker fill reads and locks through its generated list, also after an added column", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC15-T02") })
	t.Run("ST-SUP-RC35-OC10-T01 Shutdown starts no run, waits for the drain bound and rolls back the rest", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC10-T01") })
	t.Run("ST-SUP-RC35-OC12-T01 A tick that runs out of time counts the unreached tenants, and the next starts from them", func(t *testing.T) { runCheck(t, p, run, "ST-SUP-RC35-OC12-T01") })
}
