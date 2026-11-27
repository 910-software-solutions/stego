// Package check runs the generated CRUD handlers of the test service over HTTP on the command and the query runtime
// (ST-SUP-RC20-OC37), against a local PostgreSQL server (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container
// (STEGO_TEST_SPICEDB), with the runtime library wired as the service wires it.
package check

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/910-software-solutions/stego/registry/runtime/audit"
	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/command"
	"github.com/910-software-solutions/stego/registry/runtime/idempotency"
	"github.com/910-software-solutions/stego/registry/runtime/outbox"
	"github.com/910-software-solutions/stego/registry/runtime/probe"
	"github.com/910-software-solutions/stego/registry/runtime/query"
	"github.com/910-software-solutions/stego/registry/runtime/sealed"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"

	"example.com/crud/out/api"
	"example.com/crud/out/slots"
)

// tablesSQL is the migration 001_initial of the PostgreSQL adapter for the declaration (ST-SUP-RC01-OC41).
//
//go:embed tables.sql
var tablesSQL string

const (
	issuer   = "https://issuer.authn.test/realms/r1"
	audience = "crud"
	password = "crud-check"
	schema   = `definition user {}

// The operators who resolve a parked outbox message (ST-SUP-RC29-OC20).
definition outbox_destination {
	relation operator: user
	permission resolve = operator
}

definition order {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition customer {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition meeting {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition todo {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition folder {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition book {
	relation folder: folder
	relation owner: user
	permission view = owner + folder->view
	permission edit = owner + folder->edit
}

definition setting {
	relation folder: folder
	relation owner: user
	permission view = owner + folder->view
	permission edit = owner + folder->edit
}

definition quota {
	relation folder: folder
	relation owner: user
	permission view = owner + folder->view
	permission edit = owner + folder->edit
}

definition rack {
	relation parent: folder
	relation owner: user
	relation viewer: user
	permission view = owner + viewer + parent->view
	permission edit = owner + parent->edit
}

definition card {
	relation parent: rack
	relation owner: user
	relation viewer: user
	permission view = owner + viewer + parent->view
	permission edit = owner + parent->edit
}

definition mark {
	relation parent: card
	relation owner: user
	relation viewer: user
	permission view = owner + viewer + parent->view
	permission edit = owner + parent->edit
}

definition event {
	relation owner: user
	permission view = owner
	permission edit = owner
}

definition label {
	relation owner: user
	permission view = owner
	permission edit = owner
}

definition ticket {
	relation folder: folder
	relation owner: user
	permission view = owner + folder->view
	permission edit = owner + folder->edit
}

definition memo {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}
`
)

var tables = []string{"orders", "customers", "todos", "folders", "books", "settings", "events", "quotas", "meetings", "labels", "racks", "cards", "marks", "tickets", "memos"}

// env is the running service: its database with the runtime role, its runtimes and its HTTP server.
type env struct {
	// ring is the key ring of the sealed secrets, which a check opens a stored secret with.
	ring     *sealed.Ring
	t        *testing.T
	ctx      context.Context
	admin    *sql.DB
	server   *httptest.Server
	verifier *authn.Verifier
	key      *authntest.Key
	spice    *grpc.ClientConn
	// decision is the decision provider of the runtimes: SpiceDB, or a SpiceDB that fails while a test sets a fault.
	decision *faultyDecision
	// orderGate and customerGate are the decisions of the gate fills of orders.before_create and
	// customers.validate: nil, or a nil result, accepts.
	mu           sync.Mutex
	orderGate    func(*slots.BeforeCreateRequest) *slots.SlotResult
	customerGate func(*slots.ValidateRequest) *slots.SlotResult
	// settingGate is the decision of the gate fill of folder-settings.validate.
	settingGate func(*slots.ValidateRequest) *slots.SlotResult
	// orderAfter is the fill of orders.after_create: it records each call, and fails the first call for the sku
	// after-fails and the first two for the sku after-flaky.
	afterCalls []string
	// metrics reads the metrics of the after slots, and logs holds the log lines of the service.
	metrics *sdkmetric.ManualReader
	logs    *lockedBuffer
}

// lockedBuffer is the log of the service, which the slog handler and the checks share.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// counter gives the sum of the counter name of the after slots for the slot.
func (e *env) counter(name, slot string) int64 {
	var data metricdata.ResourceMetrics
	must(e.t, e.metrics.Collect(e.ctx, &data))
	var n int64
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != name {
				continue
			}
			for _, point := range sum.DataPoints {
				if value, _ := point.Attributes.Value("slot"); value.AsString() == slot {
					n += point.Value
				}
			}
		}
	}
	return n
}

// eventually waits until ok holds, at most 30 s.
func (e *env) eventually(what string, ok func() bool) {
	e.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	e.t.Fatalf("after 30 s: %s", what)
}

var (
	shared  *env
	setupMu sync.Mutex
)

// service starts the service once for the test binary.
func service(t *testing.T) *env {
	t.Helper()
	setupMu.Lock()
	defer setupMu.Unlock()
	if shared != nil {
		shared.t = t
		return shared
	}
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	if dsn == "" || endpoint == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN and STEGO_TEST_SPICEDB: a missing result is not a pass")
	}
	e := &env{t: t, ctx: context.Background()}
	e.start(dsn, endpoint, os.Getenv("STEGO_TEST_SPICEDB_KEY"))
	shared = e
	return e
}

// faultyDecision decides through the SpiceDB provider live, and while a test sets a fault, through a SpiceDB provider
// whose every call fails with that error of SpiceDB, so the test sees how the emitted handlers answer a decision that
// the provider cannot make now.
type faultyDecision struct {
	live  authz.Decision
	mu    sync.Mutex
	fault authz.Decision
	// calls counts the calls to the provider by the ID of their subject: one for each evaluation, batch and search
	// (ST-SUP-RC20-OC24).
	calls map[string]int
}

// count adds one call of the subject id.
func (d *faultyDecision) count(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[id]++
}

// callsOf gives the number of calls to the provider for the subject of the caller c.
func (d *faultyDecision) callsOf(c caller) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[c.principal.Subject().ID]
}

// failingConn is a SpiceDB connection whose every call fails with err.
type failingConn struct{ err error }

func (c failingConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return c.err
}

func (c failingConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, c.err
}

// fail makes each decision fail with cause, an error of SpiceDB, until fail(nil).
func (d *faultyDecision) fail(t *testing.T, cause error) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fault = nil
	if cause != nil {
		failing, err := authz.NewSpiceDB(failingConn{err: cause})
		must(t, err)
		d.fault = failing
	}
}

func (d *faultyDecision) now() authz.Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fault != nil {
		return d.fault
	}
	return d.live
}

func (d *faultyDecision) Evaluate(ctx context.Context, r authz.EvaluationRequest) (authz.EvaluationResult, error) {
	d.count(r.Subject.ID)
	return d.now().Evaluate(ctx, r)
}

func (d *faultyDecision) EvaluateBatch(ctx context.Context, r authz.BatchRequest) ([]authz.EvaluationResult, error) {
	if len(r.Evaluations) > 0 {
		d.count(r.Evaluations[0].Subject.ID)
	}
	return d.now().EvaluateBatch(ctx, r)
}

func (d *faultyDecision) Search(ctx context.Context, r authz.SearchRequest) (authz.SearchResult, error) {
	d.count(r.Subject.ID)
	return d.now().Search(ctx, r)
}

func (d *faultyDecision) Searches() bool { return d.live.Searches() }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// start makes a database of its own with the runtime, outbox worker and relay roles, the runtime tables and the
// entity tables with row-level security, writes the schema to SpiceDB, and serves the generated handlers behind the
// verifier on the command and the query runtime.
func (e *env) start(dsn, endpoint, spiceKey string) {
	t := e.t
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	name := "crud_" + tag
	roles := [3]string{"crud_rt_" + tag, "crud_worker_" + tag, "crud_relay_" + tag}
	_, err = root.ExecContext(e.ctx, "CREATE DATABASE "+name)
	must(t, err)
	for _, role := range roles {
		_, err := root.ExecContext(e.ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'")
		must(t, err)
	}
	u, err := url.Parse(dsn)
	must(t, err)
	u.Path = "/" + name
	e.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	// The outbox relay role of the test service, one role of the server, which the outbox migration makes.
	queueDDL, err := outbox.Migration("crud_check"+outbox.RelaySuffix, roles[0], roles[1])
	must(t, err)
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], "crud_check"+outbox.RelaySuffix, roles[1])
	must(t, err)
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	must(t, err)
	// The idempotency purge role of the test service, one role of the server, which the migration makes.
	keysDDL, err := idempotency.Migration("crud_check_idempotency_purge", roles[0])
	must(t, err)
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		keysDDL,
		"GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO " + roles[0],
		tablesSQL,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON " + strings.Join(tables, ", ") + " TO " + roles[0],
		"GRANT " + roles[0] + " TO " + roles[1],
	}
	for _, table := range tables {
		setup = append(setup,
			"ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	}
	for _, statement := range setup {
		if _, err := e.admin.ExecContext(e.ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	u.User = url.UserPassword(roles[0], password)
	runtimeDSN := u.String()
	pool, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	auditDB, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	maintainAudit(t, e.ctx, pool)
	queue, err := outbox.New(pool, outbox.Options{})
	must(t, err)
	writer, err := audit.NewWriter(auditOutbox{q: queue}, nil)
	must(t, err)

	if spiceKey == "" {
		spiceKey = "crud-check"
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	e.spice = conn
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	for {
		_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	must(t, err)
	provider, err := authz.NewSpiceDB(conn)
	must(t, err)
	e.decision = &faultyDecision{live: provider}
	az, err := authz.New(authz.Config{Provider: e.decision, DB: pool, Schema: "authz", Outbox: queue})
	must(t, err)

	settings := audit.Settings{Level: audit.LevelChange}
	probes := probe.NewRecorder(nil)
	// A throwaway key ring of the secret fields of Customer, in memory only (ST-SUP-RP16-LC02).
	ringKey := make([]byte, 32)
	_, _ = rand.Read(ringKey)
	ring, err := sealed.ParseRing([]byte("1 " + base64.StdEncoding.EncodeToString(ringKey)))
	must(t, err)
	e.ring = ring
	commands, err := command.New(e.ctx, command.Config{DB: pool, AuditDB: auditDB, Outbox: queue, Authz: az, Audit: writer,
		AuditSettings: settings, Service: "crud", ServiceVersion: "1.0.0", DeclarationVersion: "d1",
		Entities: api.Entities(), Probes: probes, Ring: ring})
	must(t, err)
	cursorKey := make([]byte, query.MinCursorKey)
	_, _ = rand.Read(cursorKey)
	queries, err := query.New(query.Config{DB: pool, AuditDB: auditDB, Audit: writer, AuditSettings: settings, Authz: az,
		Service: "crud", CursorKey: cursorKey})
	must(t, err)
	// The after slots run in the outbox worker on the worker role, as the service runtime runs them
	// (ST-SUP-RC20-OC34), with a metric reader and the log of the service.
	e.metrics = sdkmetric.NewManualReader()
	after := api.NewAfterSlots()
	must(t, after.Meter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(e.metrics))))
	e.logs = &lockedBuffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(e.logs, nil)))
	u.User = url.UserPassword(roles[1], password)
	workerPool, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: u.String(), Claims: true})
	must(t, err)
	workerQueue, err := outbox.New(workerPool, outbox.Options{})
	must(t, err)
	config := outbox.DefaultWorkerConfig()
	config.PollInterval, config.RetryMin, config.RetryMax = 20*time.Millisecond, 50*time.Millisecond, 200*time.Millisecond
	worker, err := outbox.NewWorker(workerQueue, config)
	must(t, err)
	must(t, worker.Register(outbox.Destination{Name: api.AfterSlotsDestination, Idempotent: true, MustLand: true,
		Handler: func(ctx context.Context, _ *sql.Tx, d outbox.Delivery) error { return after.Deliver(ctx, d) }}))
	go func() { _ = worker.Run(context.Background()) }()
	rt := &api.Runtime{Commands: commands, Queries: queries, Probes: probes, AfterSlots: after}

	e.key = authntest.NewKey(t, "k1")
	keys, err := authn.ParseKeys(authntest.Document(e.key.JWK()))
	must(t, err)
	e.verifier, err = authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CRUD", Keys: keys})
	must(t, err)

	orders := api.NewOrdersHandler(rt, slots.NewBeforeCreateChain(true, orderGate{e}), slots.NewAfterCreateGate(orderAfter{e}))
	customers := api.NewCustomersHandler(rt, slots.NewValidateGate(customerGate{e}))
	todos := api.NewTodosHandler(rt)
	meetings := api.NewMeetingsHandler(rt)
	folders, books, allBooks := api.NewFoldersHandler(rt), api.NewFolderBooksHandler(rt), api.NewBooksHandler(rt)
	redEvents, blueEvents := api.NewRedEventsHandler(rt), api.NewBlueEventsHandler(rt)
	folderSettings := api.NewFolderSettingsHandler(rt, slots.NewValidateGate(settingGate{e}), slots.NewOnEntityChangedGate(settingChanged{e}))
	folderFlags := api.NewFolderFlagsHandler(rt)
	folderQuotas := api.NewFolderQuotasHandler(rt)
	greenLabels := api.NewGreenLabelsHandler(rt)
	folderRacks, rackCards, cardMarks := api.NewFolderRacksHandler(rt), api.NewRackCardsHandler(rt), api.NewCardMarksHandler(rt)
	tickets := api.NewTicketsHandler(rt)
	mux := http.NewServeMux()
	// The fields of a ticket that a collection write cannot change (ST-SUP-RC20-OC44 to ST-SUP-RC20-OC46).
	mux.HandleFunc("POST /api/v1/tickets", tickets.Create)
	mux.HandleFunc("GET /api/v1/tickets/{id}", tickets.Read)
	mux.HandleFunc("PUT /api/v1/tickets/{id}", tickets.Update)
	mux.HandleFunc("PATCH /api/v1/tickets/{id}", tickets.Patch)
	mux.HandleFunc("PUT /api/v1/tickets", tickets.Upsert)
	memos := api.NewMemosHandler(rt)
	mux.HandleFunc("POST /api/v1/memos", memos.Create)
	mux.HandleFunc("GET /api/v1/memos", memos.List)
	mux.HandleFunc("GET /api/v1/memos/{id}", memos.Read)
	mux.HandleFunc("PUT /api/v1/memos/{id}", memos.Update)
	mux.HandleFunc("DELETE /api/v1/memos/{id}", memos.Delete)
	// The routes of the parent chain of three levels, as the wiring of the generator gives them (ST-SUP-RC20-OC24).
	mux.HandleFunc("POST /api/v1/folders/{folder_id}/racks", folderRacks.Create)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/racks/{id}", folderRacks.Read)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/racks", folderRacks.List)
	mux.HandleFunc("POST /api/v1/folders/{folder_id}/racks/{rack_id}/cards", rackCards.Create)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/racks/{rack_id}/cards/{id}", rackCards.Read)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/racks/{rack_id}/cards", rackCards.List)
	marks := "/api/v1/folders/{folder_id}/racks/{rack_id}/cards/{card_id}/marks"
	mux.HandleFunc("POST "+marks, cardMarks.Create)
	mux.HandleFunc("GET "+marks+"/{id}", cardMarks.Read)
	mux.HandleFunc("GET "+marks, cardMarks.List)
	mux.HandleFunc("PUT "+marks+"/{id}", cardMarks.Update)
	mux.HandleFunc("PATCH "+marks+"/{id}", cardMarks.Patch)
	mux.HandleFunc("DELETE "+marks+"/{id}", cardMarks.Delete)
	mux.HandleFunc("PUT "+marks, cardMarks.Upsert)
	mux.HandleFunc("POST /api/v1/folders", folders.Create)
	mux.HandleFunc("GET /api/v1/folders/{id}", folders.Read)
	mux.HandleFunc("GET /api/v1/folders", folders.List)
	mux.HandleFunc("DELETE /api/v1/folders/{id}", folders.Delete)
	mux.HandleFunc("POST /api/v1/folders/{folder_id}/books", books.Create)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/books/{id}", books.Read)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/books", books.List)
	mux.HandleFunc("PUT /api/v1/folders/{folder_id}/books/{id}", books.Update)
	mux.HandleFunc("PATCH /api/v1/folders/{folder_id}/books/{id}", books.Patch)
	mux.HandleFunc("DELETE /api/v1/folders/{folder_id}/books/{id}", books.Delete)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/settings", folderSettings.List)
	mux.HandleFunc("PUT /api/v1/folders/{folder_id}/settings", folderSettings.Upsert)
	mux.HandleFunc("PUT /api/v1/folders/{folder_id}/flags", folderFlags.Upsert)
	mux.HandleFunc("PUT /api/v1/folders/{folder_id}/quotas", folderQuotas.Upsert)
	mux.HandleFunc("GET /api/v1/folders/{folder_id}/quotas", folderQuotas.List)
	mux.HandleFunc("POST /api/v1/books", allBooks.Create)
	mux.HandleFunc("GET /api/v1/books/{id}", allBooks.Read)
	mux.HandleFunc("PUT /api/v1/books/{id}", allBooks.Update)
	mux.HandleFunc("POST /api/v1/red-events", redEvents.Create)
	mux.HandleFunc("GET /api/v1/red-events/{id}", redEvents.Read)
	mux.HandleFunc("GET /api/v1/red-events", redEvents.List)
	mux.HandleFunc("PUT /api/v1/red-events/{id}", redEvents.Update)
	mux.HandleFunc("PATCH /api/v1/red-events/{id}", redEvents.Patch)
	mux.HandleFunc("DELETE /api/v1/red-events/{id}", redEvents.Delete)
	mux.HandleFunc("POST /api/v1/blue-events", blueEvents.Create)
	mux.HandleFunc("GET /api/v1/blue-events", blueEvents.List)
	mux.HandleFunc("POST /api/v1/green-labels", greenLabels.Create)
	mux.HandleFunc("GET /api/v1/green-labels/{id}", greenLabels.Read)
	mux.HandleFunc("POST /api/v1/orders", orders.Create)
	mux.HandleFunc("GET /api/v1/orders/{id}", orders.Read)
	mux.HandleFunc("GET /api/v1/orders", orders.List)
	mux.HandleFunc("PATCH /api/v1/orders/{id}", orders.Patch)
	mux.HandleFunc("DELETE /api/v1/orders/{id}", orders.Delete)
	mux.HandleFunc("POST /api/v1/customers", customers.Create)
	mux.HandleFunc("GET /api/v1/customers/{id}", customers.Read)
	mux.HandleFunc("GET /api/v1/customers", customers.List)
	mux.HandleFunc("PUT /api/v1/customers/{id}", customers.Update)
	mux.HandleFunc("DELETE /api/v1/customers/{id}", customers.Delete)
	mux.HandleFunc("POST /api/v1/todos", todos.Create)
	mux.HandleFunc("GET /api/v1/todos/{id}", todos.Read)
	mux.HandleFunc("GET /api/v1/todos", todos.List)
	mux.HandleFunc("PATCH /api/v1/todos/{id}", todos.Patch)
	mux.HandleFunc("PUT /api/v1/todos/{id}", todos.Update)
	mux.HandleFunc("DELETE /api/v1/todos/{id}", todos.Delete)
	mux.HandleFunc("POST /api/v1/meetings", meetings.Create)
	mux.HandleFunc("GET /api/v1/meetings/{id}", meetings.Read)
	mux.HandleFunc("PUT /api/v1/meetings/{id}", meetings.Update)
	mux.HandleFunc("PATCH /api/v1/meetings/{id}", meetings.Patch)
	e.server = httptest.NewServer(e.verifier.Middleware(mux))
}

// orderGate is the fill of the short-circuit chain of orders.before_create.
type orderGate struct{ e *env }

func (g orderGate) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	decide := g.e.orderGate
	g.e.mu.Unlock()
	if decide != nil {
		if result := decide(req); result != nil {
			return result, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}

// orderAfter is the fill of orders.after_create.
type orderAfter struct{ e *env }

func (g orderAfter) Evaluate(_ context.Context, req *slots.AfterCreateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	defer g.e.mu.Unlock()
	sku := req.PersistedFields["sku"]
	g.e.afterCalls = append(g.e.afterCalls, sku)
	calls := 0
	for _, s := range g.e.afterCalls {
		if s == sku {
			calls++
		}
	}
	if (sku == "after-fails" && calls <= 1) || (sku == "after-flaky" && calls <= 2) {
		return nil, errors.New("after slot failed: secret internal text")
	}
	return &slots.SlotResult{Ok: true}, nil
}

// afterCallsOf counts the calls of orders.after_create for the sku.
func (e *env) afterCallsOf(sku string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.afterCalls {
		if s == sku {
			n++
		}
	}
	return n
}

// customerGate is the gate fill of customers.validate.
type customerGate struct{ e *env }

func (g customerGate) Evaluate(_ context.Context, req *slots.ValidateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	decide := g.e.customerGate
	g.e.mu.Unlock()
	if decide != nil {
		if result := decide(req); result != nil {
			return result, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}

// settingGate is the fill of folder-settings.validate.
type settingGate struct{ e *env }

func (g settingGate) Evaluate(_ context.Context, req *slots.ValidateRequest) (*slots.SlotResult, error) {
	g.e.mu.Lock()
	decide := g.e.settingGate
	g.e.mu.Unlock()
	if decide != nil {
		if result := decide(req); result != nil {
			return result, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}

// settingChanged is the fill of folder-settings.on_entity_changed, which accepts each call.
type settingChanged struct{ e *env }

func (settingChanged) Evaluate(context.Context, *slots.OnEntityChangedRequest) (*slots.SlotResult, error) {
	return &slots.SlotResult{Ok: true}, nil
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// auditOutbox is the audit writer's outbox: it enqueues each sink message on the queue.
type auditOutbox struct{ q *outbox.Queue }

func (o auditOutbox) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit outbox needs a *sql.Tx")
	}
	return o.q.Enqueue(ctx, sqlTx, outbox.Message{ID: uuid.New(), Destination: destination, ResourceKey: key,
		Kind: "audit.event", Payload: payload, Idempotent: true, MustLand: true})
}

// maintainAudit makes the audit partitions of this month and the next, as the controller's maintenance does.
func maintainAudit(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	for range 64 {
		var more bool
		err := func() error {
			tx, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := tx.QueryRowContext(ctx, `SELECT step_left FROM `+audit.MaintainFunction+`()`).Scan(&more); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			t.Fatalf("audit_maintain: %v", err)
		}
		if !more {
			return
		}
	}
	t.Fatal("audit_maintain did not finish")
}

// caller is one verified caller of a tenant, with a token.
type caller struct {
	tenant    tenant.Tenant
	principal authz.Principal
	token     string
}

// as mints a token for subject in tenantID and verifies it.
func (e *env) as(subject, tenantID string) caller {
	t := e.t
	t.Helper()
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, e.key)
	id, err := e.verifier.Verify(e.ctx, token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	c.principal, err = authz.Person("user", id)
	must(t, err)
	return c
}

// newTenant gives a tenant ID that no other check uses.
// cli gives the apply command of the generated CLI program of STEGO_TEST_CLI, logged in as c. The CLI calls only an
// HTTPS URL, so the service gets a TLS listener that passes each request to its handler through serve.
func (e *env) cli(t *testing.T, c caller, serve func(w http.ResponseWriter, r *http.Request, next http.Handler)) func(name string) ([]map[string]string, error) {
	t.Helper()
	apply := e.cliWarn(t, c, serve)
	return func(name string) ([]map[string]string, error) {
		t.Helper()
		results, _, err := apply(name)
		return results, err
	}
}

// cliWarn is cli whose apply also gives what the CLI wrote to standard error.
func (e *env) cliWarn(t *testing.T, c caller, serve func(w http.ResponseWriter, r *http.Request, next http.Handler)) func(name string) ([]map[string]string, string, error) {
	t.Helper()
	program := os.Getenv("STEGO_TEST_CLI")
	if program == "" {
		t.Fatal("the check needs STEGO_TEST_CLI, the generated CLI program: a missing result is not a pass")
	}
	listener := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, e.server.Config.Handler) }))
	t.Cleanup(listener.Close)
	dir := t.TempDir()
	// The CLI keeps its configuration only in a folder that no other user can write.
	must(t, os.Chmod(dir, 0o700))
	ca, token, config := dir+"/ca.pem", dir+"/token", dir+"/config.json"
	must(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: listener.Certificate().Raw}), 0o600))
	must(t, os.WriteFile(token, []byte(c.token), 0o600))
	run := func(args ...string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(program, args...)
		cmd.Env = append(os.Environ(), "CRUD_CLI_CONFIG="+config)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, stderr.String())
		}
		return stdout.String(), stderr.String(), err
	}
	if out, _, err := run("login", "--url", listener.URL, "--token-file", token, "--ca-file", ca); err != nil {
		t.Fatalf("the CLI login failed: %v %s", err, out)
	}
	return func(name string) ([]map[string]string, string, error) {
		t.Helper()
		file := dir + "/" + name + ".yaml"
		must(t, os.WriteFile(file, []byte("apiVersion: crud/v1\nkind: Folder\nmetadata: {name: label}\nspec: {name: "+name+"}\n"), 0o600))
		out, warned, err := run("apply", "-f", file, "-o", "json")
		var results []map[string]string
		if out != "" {
			must(t, json.Unmarshal([]byte(out), &results))
		}
		return results, warned, err
	}
}

func newTenant() string { return "t-" + uuid.NewString()[:8] }

// randomText gives n random characters: letters and digits, or, when wide, characters of four bytes each in UTF-8
// (U+1F600 to U+1F64F).
func randomText(n int, wide bool) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	var b strings.Builder
	for _, c := range raw {
		if wide {
			b.WriteRune(rune(0x1F600 + int(c)%0x50))
		} else {
			b.WriteByte(letters[int(c)%len(letters)])
		}
	}
	return b.String()
}

// failInsert makes the database fail each insert into table whose column holds value, until the check ends, as a
// fault of the database does: the service then answers the write with a 5xx.
func (e *env) failInsert(t *testing.T, table, column, value string) {
	t.Helper()
	name := "fail_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := e.admin.ExecContext(e.ctx, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.`+column+` = '`+value+`' THEN
    RAISE EXCEPTION 'the check fails this insert';
  END IF;
  RETURN NEW;
END $$`)
	must(t, err)
	_, err = e.admin.ExecContext(e.ctx, `CREATE TRIGGER `+name+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
	must(t, err)
	t.Cleanup(func() {
		_, _ = e.admin.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON `+table)
		_, _ = e.admin.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
	})
}

// own writes the owner relation of the caller on a resource of the caller's tenant in SpiceDB.
func (e *env) own(c caller, resourceType, id string) { e.relate(c, "owner", resourceType, id) }

// relate writes the relation of the caller on a resource of the caller's tenant in SpiceDB.
func (e *env) relate(c caller, relation, resourceType, id string) {
	t := e.t
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.principal.Subject().ID}}}}}})
	must(t, err)
}

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// object gives the JSON object of the body.
func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("the body %s is not a JSON object: %v", r.body, err)
	}
	return v
}

// member gives the string member name of a JSON object body.
func (r reply) member(t *testing.T, name string) string {
	t.Helper()
	s, _ := r.object(t)[name].(string)
	return s
}

// send sends one request with the caller's token and, when key is not empty, the idempotency key.
func (e *env) send(c caller, method, path, key, body string) reply {
	e.t.Helper()
	return e.sendWith(c, method, path, key, body, nil)
}

// sendWith is send with the request headers given.
func (e *env) sendWith(c caller, method, path, key, body string, headers map[string]string) reply {
	t := e.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, e.server.URL+path, reader)
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+c.token)
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, header: response.Header, body: data}
}

// create creates one resource of the collection with a new key and gives its ID; the caller owns it.
func (e *env) create(c caller, collection, resourceType, body string) string {
	t := e.t
	t.Helper()
	created := e.send(c, http.MethodPost, "/api/v1/"+collection, "k-"+uuid.NewString(), body)
	if created.status != http.StatusCreated {
		t.Fatalf("the create of %s answered %d %s, want 201", collection, created.status, created.body)
	}
	id := created.member(t, "id")
	e.own(c, resourceType, id)
	return id
}

// createUnder creates one resource of resourceType under the path below /api/v1 with a new key and gives its ID; it
// writes no relation of the caller, so the caller reaches it through its parent. The relay of the check does not
// run, so createUnder relays the parent relation of the row (relayParent).
func (e *env) createUnder(c caller, path, resourceType, parentType, parentID, body string) string {
	t := e.t
	t.Helper()
	created := e.send(c, http.MethodPost, "/api/v1"+path, "k-"+uuid.NewString(), body)
	if created.status != http.StatusCreated {
		t.Fatalf("the create under %s answered %d %s, want 201", path, created.status, created.body)
	}
	id := created.member(t, "id")
	e.relayParent(c, resourceType, id, parentType, parentID)
	return id
}

// relayParent finds the one tuple <resourceType>:<id>#parent that the write of the row enqueued for the relay, checks
// that its subject is the parent parentType:parentID of the caller's tenant, and writes it to SpiceDB, as the relay
// does (ST-SUP-RP10-LC05).
func (e *env) relayParent(c caller, resourceType, id, parentType, parentID string) {
	t := e.t
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	parent, err := authz.ResourceID(c.tenant, parentID)
	must(t, err)
	rows, err := e.admin.QueryContext(e.ctx, `SELECT convert_from(payload, 'UTF8') FROM stego_outbox.messages WHERE destination = 'authz'
AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,resource_type}' = $1 AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,resource_id}' = $2
AND convert_from(payload, 'UTF8')::jsonb #>> '{tuple,relation}' = 'parent'`, resourceType, resourceID)
	must(t, err)
	defer rows.Close()
	type relayed struct {
		Op    string `json:"op"`
		Tuple struct {
			SubjectType string `json:"subject_type"`
			SubjectID   string `json:"subject_id"`
		} `json:"tuple"`
	}
	var tuples []relayed
	for rows.Next() {
		var text string
		var tuple relayed
		must(t, rows.Scan(&text))
		must(t, json.Unmarshal([]byte(text), &tuple))
		tuples = append(tuples, tuple)
	}
	must(t, rows.Err())
	if len(tuples) != 1 || tuples[0].Op != "touch" || tuples[0].Tuple.SubjectType != parentType || tuples[0].Tuple.SubjectID != parent {
		t.Fatalf("the write of %s:%s enqueued the parent tuples %+v, want one touch of the subject %s:%s", resourceType, id, tuples, parentType, parent)
	}
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: "parent",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: parentType, ObjectId: parent}}}}}})
	must(t, err)
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// checkZone is the local time zone of the checks, 5 hours west of UTC, so a time that the database driver gives in
// the local zone is not in UTC (ST-SUP-RC20-OC37-T10). It is set before any check starts a goroutine.
var checkZone = time.FixedZone("check-zone", -5*60*60)

func init() { time.Local = checkZone }

func TestCrud(t *testing.T) {
	t.Run("ST-SUP-RC20-OC37-T01 A collection create runs as a command", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "create-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"s-1","qty":2}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND tenant_id = $2 AND sku = 's-1' AND qty = 2`, id, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows with the caller's tenant, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid`, id); n != 1 {
			t.Errorf("%d order rows of the ID in any tenant, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'create' AND resource_type = 'order' AND resource_id = $2 AND outcome = 'ok' AND idempotency_key = $3`, a.tenant.ID(), id, key); n != 1 {
			t.Errorf("%d ok audit rows of the create, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'completed' AND response_code = 201`, a.tenant.ID(), a.principal.Subject().String(), key); n != 1 {
			t.Errorf("%d completed key rows, want 1", n)
		}
		// The order row, its audit row and its key row are of one transaction: they have one xmin.
		if n := e.count(`SELECT count(DISTINCT x) FROM (SELECT xmin::text AS x FROM orders WHERE id = $1::uuid UNION ALL SELECT xmin::text FROM audit_event WHERE resource_id = $2 AND action = 'create' UNION ALL SELECT xmin::text FROM idempotency_keys WHERE key = $3) s`, id, id, key); n != 1 {
			t.Errorf("the order row, its audit row and its key row are of %d transactions, want 1", n)
		}
		replay := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"s-1","qty":2}`)
		if replay.status != http.StatusCreated || string(replay.body) != string(created.body) || replay.header.Get(idempotency.ReplayedHeader) != "true" {
			t.Errorf("the replay answered %d %s (replayed %q), want the stored 201 %s", replay.status, replay.body, replay.header.Get(idempotency.ReplayedHeader), created.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows after the replay, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T02 A refusing before slot stops the command before any write", func(t *testing.T) {
		e := service(t)
		e.mu.Lock()
		e.orderGate = func(req *slots.BeforeCreateRequest) *slots.SlotResult {
			if req.Input.Fields["sku"] == "refused" {
				return &slots.SlotResult{Ok: false, StatusCode: http.StatusUnprocessableEntity, ErrorMessage: "the sku is refused"}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.orderGate = nil; e.mu.Unlock() }()
		a := e.as("u-1", newTenant())
		key := "refused-" + uuid.NewString()
		refused := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"refused","qty":1}`)
		if refused.status != http.StatusUnprocessableEntity || !strings.HasPrefix(refused.header.Get("Content-Type"), "application/problem+json") {
			t.Fatalf("the refused create answered %d %q %s, want the slot's 422 as a problem", refused.status, refused.header.Get("Content-Type"), refused.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d order rows after the refusal, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`, a.tenant.ID(), key); n != 0 {
			t.Errorf("%d key rows after the refusal, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d audit rows of the tenant, want only the denied row", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'create' AND outcome = 'denied' AND idempotency_key = $2`, a.tenant.ID(), key); n != 1 {
			t.Errorf("%d denied audit rows of the create, want 1", n)
		}
		accepted := e.send(a, http.MethodPost, "/api/v1/orders", "accepted-"+uuid.NewString(), `{"sku":"accepted","qty":1}`)
		if accepted.status != http.StatusCreated {
			t.Errorf("a create that the slot accepts answered %d %s, want 201", accepted.status, accepted.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T03 A write that the caller may not make, or of a missing row, is 404 and changes nothing", func(t *testing.T) {
		e := service(t)
		tenantID := newTenant()
		owner, other := e.as("owner", tenantID), e.as("other", tenantID)
		order := e.create(owner, "orders", "order", `{"sku":"own","qty":1}`)
		customer := e.create(owner, "customers", "customer", `{"name":"Eve","api_token":"tok","plan":"free"}`)
		todo := e.create(owner, "todos", "todo", `{"title":"keep"}`)
		// The other caller may view each row but not edit it, so only the write's own permission refuses the write.
		e.relate(other, "viewer", "order", order)
		e.relate(other, "viewer", "customer", customer)
		e.relate(other, "viewer", "todo", todo)
		writes := []struct{ method, path, key, body string }{
			{http.MethodPatch, "/api/v1/orders/" + order, "k-" + uuid.NewString(), `{"qty":9}`},
			{http.MethodDelete, "/api/v1/orders/" + order, "", ""},
			{http.MethodPut, "/api/v1/customers/" + customer, "", `{"name":"Eve","api_token":"tok","plan":"pro"}`},
			{http.MethodDelete, "/api/v1/customers/" + customer, "", ""},
			{http.MethodPatch, "/api/v1/todos/" + todo, "k-" + uuid.NewString(), `{"title":"changed"}`},
			{http.MethodPut, "/api/v1/todos/" + todo, "", `{"title":"changed"}`},
			{http.MethodDelete, "/api/v1/todos/" + todo, "", ""},
		}
		for _, w := range writes {
			if r := e.send(other, w.method, w.path, w.key, w.body); r.status != http.StatusNotFound {
				t.Errorf("%s %s as a caller who may not edit answered %d %s, want 404", w.method, w.path, r.status, r.body)
			}
		}
		for _, w := range writes {
			missing := w.path[:strings.LastIndex(w.path, "/")+1] + uuid.NewString()
			if r := e.send(owner, w.method, missing, w.key+"-m", w.body); r.status != http.StatusNotFound {
				t.Errorf("%s of a missing row answered %d %s, want 404", w.method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND qty = 1`, order); n != 1 {
			t.Errorf("the order changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND plan = 'free'`, customer); n != 1 {
			t.Errorf("the customer changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND title = 'keep'`, todo); n != 1 {
			t.Errorf("the todo changed: %d rows as created, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok' AND action IN ('update', 'patch', 'delete')`, tenantID); n != 0 {
			t.Errorf("%d ok audit rows of a refused write, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND actor_id = $2 AND outcome = 'denied'`, tenantID, other.principal.Subject().String()); n != 7 {
			t.Errorf("%d denied audit rows of the refused writes, want 7", n)
		}
		// A deleted row whose owner tuple stays is allowed by the decision and then not found: it is 404 too, so the
		// answer does not tell that the row once existed.
		gone := e.create(owner, "orders", "order", `{"sku":"gone","qty":1}`)
		if r := e.send(owner, http.MethodDelete, "/api/v1/orders/"+gone, "", ""); r.status != http.StatusNoContent {
			t.Fatalf("the owner's delete answered %d %s, want 204", r.status, r.body)
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			if r := e.send(owner, method, "/api/v1/orders/"+gone, "k-"+uuid.NewString(), `{"qty":2}`); r.status != http.StatusNotFound {
				t.Errorf("a %s of a deleted row answered %d %s, want 404", method, r.status, r.body)
			}
		}
		if r := e.send(owner, http.MethodPatch, "/api/v1/orders/"+order, "k-"+uuid.NewString(), `{"qty":5}`); r.status != http.StatusOK {
			t.Errorf("the owner's patch answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T05 A before slot that halts writes nothing and fires no after slot", func(t *testing.T) {
		e := service(t)
		e.mu.Lock()
		e.orderGate = func(req *slots.BeforeCreateRequest) *slots.SlotResult {
			if req.Input.Fields["sku"] == "halted" {
				return &slots.SlotResult{Ok: true, Halt: true, StatusCode: http.StatusNoContent}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.orderGate = nil; e.mu.Unlock() }()
		a := e.as("u-1", newTenant())
		key := "halt-" + uuid.NewString()
		before := e.afterCallsOf("")
		for i := 0; i < 2; i++ {
			r := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"halted","qty":1}`)
			if r.status != http.StatusNoContent || r.header.Get("Stego-Slot-Halted") != "" {
				t.Errorf("the halted create (call %d) answered %d with the halt mark %q, want 204 with no mark", i+1, r.status, r.header.Get("Stego-Slot-Halted"))
			}
		}
		if n := e.afterCallsOf("halted") + e.afterCallsOf("") - before; n != 0 {
			t.Errorf("the after slot ran %d times for a halted create and its replay, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d order rows after the halt, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok'`, a.tenant.ID()); n != 0 {
			t.Errorf("%d ok audit rows after the halt, want 0", n)
		}
		e.create(a, "orders", "order", `{"sku":"written","qty":1}`)
		e.eventually("the outbox worker runs the after slot of a create that wrote", func() bool { return e.afterCallsOf("written") == 1 })
		if n := e.afterCallsOf("halted"); n != 0 {
			t.Errorf("the after slot ran %d times for a halted create, want 0", n)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T06 A failed after slot answers the committed response with no error text", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "after-" + uuid.NewString()
		r := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"after-fails","qty":1}`)
		if r.status != http.StatusCreated || strings.Contains(string(r.body), "secret internal text") {
			t.Fatalf("the create whose after slot fails answered %d %s, want the committed 201 with no error text", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = $1 AND sku = 'after-fails'`, a.tenant.ID()); n != 1 {
			t.Errorf("%d order rows, want the committed one", n)
		}
		if replay := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"after-fails","qty":1}`); replay.status != r.status || string(replay.body) != string(r.body) {
			t.Errorf("the replay answered %d %s, want the same answer %d %s", replay.status, replay.body, r.status, r.body)
		}
		e.eventually("the failed after slot is counted and runs again", func() bool { return api.AfterSlotFailures() >= 1 && e.afterCallsOf("after-fails") == 2 })
	})

	t.Run("ST-SUP-RC20-OC34-T03 A failed after slot is retried through the outbox, never reaches the answer, and each failure and retry is visible", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		slot := "orders.after_create"
		failures, retries := e.counter("rest_api.after_slot.failures", slot), e.counter("rest_api.after_slot.retries", slot)
		r := e.send(a, http.MethodPost, "/api/v1/orders", "flaky-"+uuid.NewString(), `{"sku":"after-flaky","qty":1}`)
		if r.status != http.StatusCreated || strings.Contains(string(r.body), "secret internal text") {
			t.Fatalf("the create whose after slot fails twice answered %d %s, want the committed 201 with no error text", r.status, r.body)
		}
		e.eventually("the outbox worker runs the after slot three times", func() bool { return e.afterCallsOf("after-flaky") == 3 })
		e.eventually("the message of the after slot is done", func() bool {
			return e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = $2 AND state <> 'done'`, a.tenant.ID(), api.AfterSlotsDestination) == 0
		})
		time.Sleep(300 * time.Millisecond)
		if n := e.afterCallsOf("after-flaky"); n != 3 {
			t.Errorf("the after slot ran %d times, want 3: two failures and one pass", n)
		}
		if got := e.counter("rest_api.after_slot.failures", slot) - failures; got != 2 {
			t.Errorf("the failure metric of %s grew by %d, want 2", slot, got)
		}
		if got := e.counter("rest_api.after_slot.retries", slot) - retries; got != 2 {
			t.Errorf("the retry metric of %s grew by %d, want 2", slot, got)
		}
		logs := e.logs.String()
		if strings.Contains(logs, "secret internal text") {
			t.Error("a log line holds the error text of the after slot")
		}
		if strings.Count(logs, `"slot":"`+slot+`"`) < 4 {
			t.Errorf("the log has fewer than four lines that name the slot %s, want one for each failure and each retry:\n%s", slot, logs)
		}
	})

	// tenantRoutes sends every route as the caller of tenant t1 with tenant t2 in the header, query or body.
	tenantRoutes := func(e *env, a caller, decorate func(*http.Request)) {
		t := e.t
		t.Helper()
		send := func(method, path, key, body string) reply {
			var reader io.Reader
			if body != "" {
				reader = strings.NewReader(body)
			}
			request, err := http.NewRequest(method, e.server.URL+path, reader)
			must(t, err)
			request.Header.Set("Authorization", "Bearer "+a.token)
			if key != "" {
				request.Header.Set("Idempotency-Key", `"`+key+`"`)
			}
			decorate(request)
			response, err := http.DefaultClient.Do(request)
			must(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			must(t, err)
			return reply{status: response.StatusCode, header: response.Header, body: data}
		}
		for _, collection := range []struct{ name, resourceType, body, change, method string }{
			{"orders", "order", `{"sku":"t","qty":1}`, `{"qty":2}`, http.MethodPatch},
			{"customers", "customer", `{"name":"T","api_token":"x","plan":"free"}`, `{"name":"T","api_token":"x","plan":"pro"}`, http.MethodPut},
			{"todos", "todo", `{"title":"t"}`, `{"title":"u"}`, http.MethodPatch},
		} {
			created := send(http.MethodPost, "/api/v1/"+collection.name, "k-"+uuid.NewString(), collection.body)
			if created.status != http.StatusCreated {
				t.Fatalf("the create of %s answered %d %s", collection.name, created.status, created.body)
			}
			id := created.member(t, "id")
			e.own(a, collection.resourceType, id)
			for _, r := range []reply{
				send(http.MethodGet, "/api/v1/"+collection.name+"/"+id, "", ""),
				send(http.MethodGet, "/api/v1/"+collection.name, "", ""),
				send(collection.method, "/api/v1/"+collection.name+"/"+id, "k-"+uuid.NewString(), collection.change),
				send(http.MethodDelete, "/api/v1/"+collection.name+"/"+id, "", ""),
			} {
				if r.status >= 300 {
					t.Errorf("a route of %s answered %d %s, want it served for the caller's tenant", collection.name, r.status, r.body)
				}
			}
		}
		if n := e.count(`SELECT (SELECT count(*) FROM orders WHERE tenant_id = 't2') + (SELECT count(*) FROM customers WHERE tenant_id = 't2') + (SELECT count(*) FROM todos WHERE tenant_id = 't2')`); n != 0 {
			t.Errorf("%d rows with tenant t2, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't2'`); n != 0 {
			t.Errorf("%d audit rows with tenant t2, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok' AND action IN ('create', 'patch', 'update', 'delete')`, a.tenant.ID()); n != 9 {
			t.Errorf("%d ok audit rows with the caller's tenant, want 9", n)
		}
	}

	t.Run("ST-SUP-RC20-OC35-T01 Every route takes the tenant from the verified identity", func(t *testing.T) {
		e := service(t)
		other := e.as("u-1", newTenant())
		orderOfT2 := e.create(other, "orders", "order", `{"sku":"t2","qty":1}`)
		a := e.as("u-1", newTenant())
		tenantRoutes(e, a, func(*http.Request) {})
		// A row of another tenant is not reached by its ID: the same subject in tenant t1 may not see or change it.
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+orderOfT2, "", ""); r.status != http.StatusNotFound {
			t.Errorf("a read of another tenant's row answered %d %s, want 404", r.status, r.body)
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			if r := e.send(a, method, "/api/v1/orders/"+orderOfT2, "k-"+uuid.NewString(), `{"qty":7}`); r.status != http.StatusNotFound {
				t.Errorf("a %s of another tenant's row answered %d %s, want 404", method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = $1::uuid AND tenant_id = $2 AND qty = 1`, orderOfT2, other.tenant.ID()); n != 1 {
			t.Errorf("the row of tenant t2 changed: %d rows as created, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC35-T02 A tenant in a header is not accepted", func(t *testing.T) {
		e := service(t)
		tenantRoutes(e, e.as("u-1", newTenant()), func(r *http.Request) {
			r.Header.Set("X-Tenant-ID", "t2")
			r.Header.Set("Tenant", "t2")
		})
	})

	t.Run("ST-SUP-RC20-OC35-T03 A tenant in a query parameter is not accepted", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		tenantRoutes(e, a, func(r *http.Request) {
			// A list refuses a query parameter that it does not read (ST-SUP-RC20-OC51), so it is sent below.
			if r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") == 3 {
				return
			}
			q := r.URL.Query()
			q.Set("tenant", "t2")
			q.Set("tenant_id", "t2")
			r.URL.RawQuery = q.Encode()
		})
		for _, collection := range []string{"orders", "customers", "todos"} {
			if r := e.send(a, http.MethodGet, "/api/v1/"+collection+"?tenant=t2&tenant_id=t2", "", ""); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `\"tenant\", \"tenant_id\"`) {
				t.Errorf("a list of %s with a tenant in the query answered %d %s, want 400 that names tenant and tenant_id", collection, r.status, r.body)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC35-T04 A tenant in a body field is not accepted", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// A body member that names a tenant is not a field: each write refuses it with a 400 that names it and holds
		// no value, and stores nothing (ST-SUP-RC20-OC35, ST-SUP-RC20-OC55, ST-SUP-RC20-OC15).
		withTenant := func(body string) string {
			return strings.TrimSuffix(body, "}") + `,"tenant":"t2","tenant_id":"t2"}`
		}
		refused := func(what string, r reply) {
			t.Helper()
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"tenant"`) || strings.Contains(string(r.body), "t2") {
				t.Errorf("%s with a tenant in the body answered %d %s, want 400 that names tenant and holds no value", what, r.status, r.body)
			}
		}
		for _, collection := range []struct{ name, resourceType, body, change, method string }{
			{"orders", "order", `{"sku":"t","qty":1}`, `{"qty":2}`, http.MethodPatch},
			{"customers", "customer", `{"name":"T","api_token":"x","plan":"free"}`, `{"name":"T","api_token":"x","plan":"pro"}`, http.MethodPut},
			{"todos", "todo", `{"title":"t"}`, `{"title":"u"}`, http.MethodPut},
			{"todos", "todo", `{"title":"t"}`, `{"title":"u"}`, http.MethodPatch},
		} {
			refused("the create of "+collection.name, e.send(a, http.MethodPost, "/api/v1/"+collection.name, "k-"+uuid.NewString(), withTenant(collection.body)))
			id := e.create(a, collection.name, collection.resourceType, collection.body)
			refused(collection.method+" of "+collection.name, e.send(a, collection.method, "/api/v1/"+collection.name+"/"+id, "k-"+uuid.NewString(), withTenant(collection.change)))
		}
		if n := e.count(`SELECT (SELECT count(*) FROM orders WHERE tenant_id = 't2') + (SELECT count(*) FROM customers WHERE tenant_id = 't2') + (SELECT count(*) FROM todos WHERE tenant_id = 't2')`); n != 0 {
			t.Errorf("%d rows with tenant t2, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't2'`); n != 0 {
			t.Errorf("%d audit rows with tenant t2, want 0", n)
		}
		// Only the four creates without a tenant member wrote a row, and no refused write changed one.
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND outcome = 'ok' AND action IN ('create', 'patch', 'update', 'delete')`, a.tenant.ID()); n != 4 {
			t.Errorf("%d ok audit rows with the caller's tenant, want 4", n)
		}
	})

	t.Run("ST-SUP-RC20-OC55-T01 A create, an update and an upsert refuse a member that they do not take, also in another letter case, store nothing, and take a read row sent back", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		ticket := func(code, title, extra string) string {
			return `{"folder_id":"` + folder + `","code":"` + code + `","title":"` + title + `","sku":"s-` + code + `"` + extra + `}`
		}
		id := e.create(a, "tickets", "ticket", ticket("c1", "first", ""))
		item := "/api/v1/tickets/" + id
		for member, extra := range map[string]string{
			"Title":  `,"Title":"value-of-the-member"`,
			"titel":  `,"titel":"value-of-the-member"`,
			"tenant": `,"tenant":"value-of-the-member"`,
			"CODE":   `,"CODE":"value-of-the-member"`,
		} {
			for _, w := range []struct{ what, method, path, body string }{
				{"a create", http.MethodPost, "/api/v1/tickets", ticket("c2", "second", extra)},
				{"an update", http.MethodPut, item, ticket("c1", "second", extra)},
				{"an upsert of a new row", http.MethodPut, "/api/v1/tickets", ticket("c3", "second", extra)},
				{"an upsert of the stored row", http.MethodPut, "/api/v1/tickets", ticket("c1", "second", extra)},
			} {
				r := e.send(a, w.method, w.path, "k-"+uuid.NewString(), w.body)
				if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"`+member+`"`) || strings.Contains(string(r.body), "value-of-the-member") {
					t.Errorf("%s with the member %s answered %d %s, want 400 that names the member and holds no value", w.what, member, r.status, r.body)
				}
			}
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d ticket rows, want only the first", n)
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'first'`, id); n != 1 {
			t.Errorf("%d ticket rows as the create left them, want 1", n)
		}
		// A read row sent back, with kind, id, href and each member that the server sets, is taken.
		read := e.send(a, http.MethodGet, item, "", "")
		row := read.object(t)
		row["title"] = "second"
		body, err := json.Marshal(row)
		must(t, err)
		if r := e.send(a, http.MethodPut, item, "k-"+uuid.NewString(), string(body)); r.status != http.StatusOK || r.member(t, "title") != "second" {
			t.Errorf("an update with a read row sent back (%s) answered %d %s, want 200 with the new title", body, r.status, r.body)
		}
		if r := e.send(a, http.MethodPut, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c1", "third", `,"kind":"Ticket"`)); r.status != http.StatusOK || r.member(t, "title") != "third" {
			t.Errorf("an upsert of the stored row with the right names answered %d %s, want 200 with the new title", r.status, r.body)
		}
		if r := e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c4", "fourth", `,"kind":"Ticket"`)); r.status != http.StatusCreated {
			t.Errorf("a create with the right names answered %d %s, want 201", r.status, r.body)
		}
		// An implicit field is taken only with the value of its collection (ST-SUP-RC20-OC23).
		if r := e.send(a, http.MethodPost, "/api/v1/red-events", "k-"+uuid.NewString(), `{"source":"blue","note":"n1"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"source"`) || strings.Contains(string(r.body), "blue") {
			t.Errorf("a create of a red event with the source blue answered %d %s, want 400 that names source and holds no value", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM events WHERE tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d event rows, want 0", n)
		}
	})

	t.Run("ST-SUP-RC20-OC55-T07 A write refuses a member given twice, cuts a long member name, and ignores a server-set member of another type", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		ticket := func(code, rest string) string {
			return `{"folder_id":"` + folder + `","code":"` + code + `","sku":"s-` + code + `"` + rest + `}`
		}
		id := e.create(a, "tickets", "ticket", ticket("c1", `,"title":"first"`))
		item := "/api/v1/tickets/" + id
		twice := func(what string, r reply, member string) {
			t.Helper()
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"`+member+`"`) || !strings.Contains(string(r.body), "more than once") || strings.Contains(string(r.body), "value-of-the-member") {
				t.Errorf("%s answered %d %s, want 400 that names %s as given twice and holds no value", what, r.status, r.body, member)
			}
		}
		twice("a create with title twice", e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c2", `,"title":"value-of-the-member","title":"b"`)), "title")
		twice("an update with title twice, once escaped", e.send(a, http.MethodPut, item, "k-"+uuid.NewString(), ticket("c1", `,"title":"value-of-the-member","\u0074itle":"b"`)), "title")
		twice("an upsert with title twice", e.send(a, http.MethodPut, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c1", `,"title":"value-of-the-member","title":"b"`)), "title")
		twice("a patch with title twice", e.send(a, http.MethodPatch, item, "k-"+uuid.NewString(), `{"title":"value-of-the-member","title":"b"}`), "title")
		// The implicit check cannot be passed by a second member that ends in null (security review of PR #464, LOW).
		twice("a red event with source twice", e.send(a, http.MethodPost, "/api/v1/red-events", "k-"+uuid.NewString(), `{"source":"value-of-the-member","source":null,"note":"n"}`), "source")
		if n := e.count(`SELECT count(*) FROM tickets WHERE tenant_id = $1 AND title = 'first'`, a.tenant.ID()); n != 1 || e.count(`SELECT count(*) FROM tickets WHERE tenant_id = $1`, a.tenant.ID()) != 1 || e.count(`SELECT count(*) FROM events WHERE tenant_id = $1`, a.tenant.ID()) != 0 {
			t.Errorf("the refused writes changed a row: %d tickets with the first title", n)
		}
		// A problem repeats at most 128 bytes of a member name.
		long := strings.Repeat("x", 300)
		r := e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c3", `,"title":"t","`+long+`":1`))
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"`+long[:128]+`"`) || strings.Contains(string(r.body), long[:129]) {
			t.Errorf("a member of 300 bytes answered %d %s, want 400 whose field holds its first 128 bytes", r.status, r.body)
		}
		// A member that the server sets is ignored, also with a value of another JSON type.
		if r := e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), ticket("c4", `,"title":"t","id":5,"href":[1]`)); r.status != http.StatusCreated || r.member(t, "id") == "5" {
			t.Errorf("a create with id 5 and href [1] answered %d %s, want 201 with a new id", r.status, r.body)
		}
		if r := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), `{"title":"m","starts_at":"2026-12-01T08:00:00Z","noted_at":7}`); r.status != http.StatusCreated {
			t.Errorf("a create of a meeting with noted_at 7 answered %d %s, want 201", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC30-OC04-T01 The key row commits with the write", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		key := "todo-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/todos", key, `{"title":"write the check"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE tenant_id = $1 AND title = 'write the check'`, a.tenant.ID()); n != 1 {
			t.Errorf("%d Todo rows, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'completed'`, a.tenant.ID(), a.principal.Subject().String(), key); n != 1 {
			t.Errorf("%d completed key rows scoped by the tenant, the principal and the key, want 1", n)
		}
	})

	t.Run("ST-SUP-RC30-OC04-T03 The same key from another tenant or principal is a new key", func(t *testing.T) {
		e := service(t)
		tenantA, tenantB := newTenant(), newTenant()
		key := "shared-" + uuid.NewString()
		title := "t-" + uuid.NewString()
		body := `{"title":"` + title + `"}`
		for _, c := range []caller{e.as("p1", tenantA), e.as("p2", tenantA), e.as("p1", tenantB)} {
			r := e.send(c, http.MethodPost, "/api/v1/todos", key, body)
			if r.status != http.StatusCreated || r.header.Get(idempotency.ReplayedHeader) != "" {
				t.Errorf("the create as %s in %s answered %d (replayed %q), want a new 201", c.principal.Subject(), c.tenant.ID(), r.status, r.header.Get(idempotency.ReplayedHeader))
			}
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE title = $1`, title); n != 3 {
			t.Errorf("%d Todo rows, want 3", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 3 {
			t.Errorf("%d key rows, want 3", n)
		}
	})

	t.Run("ST-SUP-RC28-OC02-T02 A committed mutation leaves exactly one audit row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Ada","api_token":"tok-1","plan":"free"}`)
		if r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Ada","api_token":"tok-2","plan":"pro"}`); r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		if r := e.send(a, http.MethodDelete, "/api/v1/customers/"+id, "", ""); r.status != http.StatusNoContent {
			t.Fatalf("the delete answered %d %s, want 204", r.status, r.body)
		}
		for _, action := range []string{"create", "update", "delete"} {
			if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = $3 AND outcome = 'ok'`, a.tenant.ID(), id, action); n != 1 {
				t.Errorf("%d ok audit rows with the action %s, want 1", n, action)
			}
			if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = $3`, a.tenant.ID(), id, action); n != 1 {
				t.Errorf("%d audit rows with the action %s, want 1", n, action)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC03-T01 A denied mutation writes one audit row with outcome denied after the rollback", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Bea","api_token":"tok-1","plan":"free"}`)
		e.mu.Lock()
		e.customerGate = func(req *slots.ValidateRequest) *slots.SlotResult {
			if req.Input.Fields["plan"] == "denied" {
				return &slots.SlotResult{Ok: false, StatusCode: http.StatusForbidden, ErrorMessage: "the plan is denied"}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.customerGate = nil; e.mu.Unlock() }()
		r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Bea","api_token":"tok-2","plan":"denied"}`)
		if r.status != http.StatusForbidden {
			t.Fatalf("the denied update answered %d %s, want 403", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND plan = 'free'`, id); n != 1 {
			t.Errorf("the change was not rolled back: %d rows with the old plan, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = 'update'`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d audit rows of the update, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND resource_id = $2 AND action = 'update' AND outcome = 'denied' AND error_code = 'slot_refused'`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d denied audit rows with the code slot_refused, want 1", n)
		}
	})

	t.Run("ST-SUP-RC28-OC04-T01 A single read of an entity with a pii field writes one read audit row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "customers", "customer", `{"name":"Cy","api_token":"tok-1","plan":"free"}`)
		before := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID())
		if r := e.send(a, http.MethodGet, "/api/v1/customers/"+id, "", ""); r.status != http.StatusOK {
			t.Fatalf("the read answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID()) - before; n != 1 {
			t.Errorf("%d new read audit rows, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_type = 'customer' AND resource_id = $2`, a.tenant.ID(), id); n != 1 {
			t.Errorf("%d read audit rows of the customer, want 1", n)
		}
		order := e.create(a, "orders", "order", `{"sku":"s-1","qty":1}`)
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+order, "", ""); r.status != http.StatusOK {
			t.Fatalf("the read of the order answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_id = $2`, a.tenant.ID(), order); n != 0 {
			t.Errorf("%d read audit rows of an entity with no pii field, want 0", n)
		}
	})

	t.Run("ST-SUP-RC28-OC04-T03 A list that returns a pii field writes one read audit row per returned resource, at most one page", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		for i := 0; i < 5; i++ {
			e.create(a, "customers", "customer", `{"name":"N","api_token":"tok","plan":"free"}`)
		}
		ids := func(r reply) []string {
			items, _ := r.object(t)["items"].([]any)
			var out []string
			for _, item := range items {
				m, _ := item.(map[string]any)
				id, _ := m["id"].(string)
				out = append(out, id)
			}
			return out
		}
		reads := func() int {
			return e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read'`, a.tenant.ID())
		}
		start := reads()
		first := e.send(a, http.MethodGet, "/api/v1/customers?pageSize=2", "", "")
		if first.status != http.StatusOK || len(ids(first)) != 2 {
			t.Fatalf("the first list answered %d %s, want 200 with two items", first.status, first.body)
		}
		if n := reads() - start; n != 2 {
			t.Errorf("%d read rows for the first list, want 2", n)
		}
		next := first.member(t, "next")
		if next == "" {
			t.Fatalf("the first list gives no cursor of the next page: %s", first.body)
		}
		second := e.send(a, http.MethodGet, "/api/v1/customers?pageSize=2&cursor="+url.QueryEscape(next), "", "")
		if second.status != http.StatusOK || len(ids(second)) != 2 {
			t.Fatalf("the second list answered %d %s, want 200 with two items", second.status, second.body)
		}
		if n := reads() - start; n != 4 {
			t.Errorf("%d read rows for both lists, want 4", n)
		}
		returned := append(ids(first), ids(second)...)
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND resource_type = 'customer' AND resource_id = ANY($2)`, a.tenant.ID(), returned); n != 4 {
			t.Errorf("%d read rows name a returned customer, want 4", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = $1 AND action = 'read' AND NOT (resource_id = ANY($2))`, a.tenant.ID(), returned); n != 0 {
			t.Errorf("%d read rows name a resource that no list returned, or the collection", n)
		}
	})

	t.Run("ST-SUP-RC20-OC38-T01 A list on the runtime data path pages by cursor in the requested order, with no total", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		type order struct {
			ID  string
			SKU string
			Qty float64
		}
		var created []order
		for _, o := range []order{{SKU: "c", Qty: 3}, {SKU: "a", Qty: 1}, {SKU: "b", Qty: 3}, {SKU: "a", Qty: 2}, {SKU: "d", Qty: 5}} {
			o.ID = e.create(a, "orders", "order", fmt.Sprintf(`{"sku":%q,"qty":%d}`, o.SKU, int(o.Qty)))
			created = append(created, o)
		}
		page := func(path string) ([]order, string) {
			t.Helper()
			r := e.send(a, http.MethodGet, path, "", "")
			if r.status != http.StatusOK {
				t.Fatalf("GET %s answered %d %s, want 200", path, r.status, r.body)
			}
			var body map[string]any
			if err := json.Unmarshal(r.body, &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["total"]; ok {
				t.Fatalf("the page %s holds a total: %s", path, r.body)
			}
			var out []order
			items, _ := body["items"].([]any)
			for _, item := range items {
				m, _ := item.(map[string]any)
				id, _ := m["id"].(string)
				sku, _ := m["sku"].(string)
				qty, _ := m["qty"].(float64)
				out = append(out, order{ID: id, SKU: sku, Qty: qty})
			}
			next, _ := body["next"].(string)
			return out, next
		}
		all := func(query string) []order {
			t.Helper()
			var got []order
			items, next := page("/api/v1/orders?pageSize=2&" + query)
			for pages := 0; ; pages++ {
				if len(items) > 2 {
					t.Fatalf("a page of %s holds %d orders, want at most 2", query, len(items))
				}
				got = append(got, items...)
				if next == "" || pages > 10 {
					break
				}
				items, next = page("/api/v1/orders?pageSize=2&" + query + "&cursor=" + url.QueryEscape(next))
			}
			return got
		}
		want := slices.Clone(created)
		slices.SortStableFunc(want, func(x, y order) int {
			if x.Qty != y.Qty {
				return cmp.Compare(y.Qty, x.Qty)
			}
			return strings.Compare(x.ID, y.ID)
		})
		if got := all("orderBy=" + url.QueryEscape("qty desc")); !slices.Equal(got, want) {
			t.Fatalf("orderBy qty desc gave %v, want %v", got, want)
		}
		two := slices.Clone(created)
		slices.SortStableFunc(two, func(x, y order) int {
			if x.SKU != y.SKU {
				return strings.Compare(x.SKU, y.SKU)
			}
			if x.Qty != y.Qty {
				return cmp.Compare(y.Qty, x.Qty)
			}
			return strings.Compare(x.ID, y.ID)
		})
		if got := all("orderBy=" + url.QueryEscape("sku asc,qty desc")); !slices.Equal(got, two) {
			t.Fatalf("orderBy sku asc,qty desc gave %v, want %v", got, two)
		}
		// With no orderBy, the newest order comes first (ST-SUP-RC20-OC11).
		newest := slices.Clone(created)
		slices.Reverse(newest)
		if got := all(""); !slices.Equal(got, newest) {
			t.Fatalf("the list with no orderBy gave %v, want the newest first %v", got, newest)
		}
		for _, bad := range []string{"secret", "qty up", "qty desc desc"} {
			if r := e.send(a, http.MethodGet, "/api/v1/orders?orderBy="+url.QueryEscape(bad), "", ""); r.status != http.StatusBadRequest {
				t.Errorf("orderBy %q answered %d %s, want 400", bad, r.status, r.body)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC37-T11 A denied create answers 404 with no empty ID, and a bad Idempotency-Key names its fault", func(t *testing.T) {
		e := service(t)
		tenantID := newTenant()
		owner, other := e.as("owner", tenantID), e.as("other", tenantID)
		folder := e.create(owner, "folders", "folder", `{"name":"own"}`)
		e.relate(other, "viewer", "folder", folder)
		// The other caller may view the folder but not edit it, so the create of a book in it is denied on the parent.
		denied := e.send(other, http.MethodPost, "/api/v1/folders/"+folder+"/books", "k-"+uuid.NewString(), `{"title":"t"}`)
		detail, _ := denied.object(t)["detail"].(string)
		if denied.status != http.StatusNotFound || strings.Contains(detail, "''") || !strings.Contains(detail, "Book was not written") {
			t.Errorf("the denied create answered %d %s, want 404 that names the Book that was not written and no empty ID", denied.status, denied.body)
		}
		t.Run("ST-SUP-RC30-OC02-T03 A create with no Idempotency-Key gets idempotency-key-missing, and one with a key that is not a structured string idempotency-key-invalid", func(t *testing.T) {
			for _, c := range []struct{ header, want, slug string }{
				{"", "the Idempotency-Key header is absent", "idempotency-key-missing"},
				{"k-unquoted", "put the key in double quotes", "idempotency-key-invalid"},
				{`""`, "put the key in double quotes", "idempotency-key-invalid"},
			} {
				headers := map[string]string{}
				if c.header != "" {
					headers["Idempotency-Key"] = c.header
				}
				r := e.sendWith(owner, http.MethodPost, "/api/v1/orders", "", `{"sku":"s","qty":1}`, headers)
				problem := r.object(t)
				if detail, _ := problem["detail"].(string); r.status != http.StatusBadRequest || !strings.Contains(detail, c.want) || !strings.HasSuffix(problem["type"].(string), "/"+c.slug) {
					t.Errorf("a create with the Idempotency-Key %q answered %d %s, want 400 of the type %s that says %q", c.header, r.status, r.body, c.slug, c.want)
				}
			}
		})
	})

	t.Run("ST-SUP-RC20-OC37-T13 A list, a read and a write that the provider cannot decide now answer 503 with Retry-After, a refused provider 500, and the log names the cause", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "orders", "order", `{"sku":"s-503","qty":1}`)
		notServed, err := status.New(codes.FailedPrecondition, "object definition `order` not found").WithDetails(
			&errdetails.ErrorInfo{Reason: v1.ErrorReason_ERROR_REASON_UNKNOWN_DEFINITION.String(), Domain: "authzed.com"})
		must(t, err)
		for _, fault := range []struct {
			name  string
			err   error
			cause string
		}{
			{"a definition that SpiceDB does not serve yet", notServed.Err(), authz.CauseSchemaNotServed},
			{"a SpiceDB that does not answer", status.Error(codes.Unavailable, "connection refused"), authz.CauseUnavailable},
		} {
			e.decision.fail(t, fault.err)
			before := len(e.logs.String())
			for _, call := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/v1/orders", ""},
				{http.MethodGet, "/api/v1/orders/" + id, ""},
				{http.MethodPatch, "/api/v1/orders/" + id, `{"qty":2}`},
			} {
				key := ""
				if call.method != http.MethodGet {
					key = "k-" + uuid.NewString()
				}
				r := e.send(a, call.method, call.path, key, call.body)
				problem := r.object(t)
				if r.status != http.StatusServiceUnavailable || !slices.Contains([]string{"1", "2", "3"}, r.header.Get("Retry-After")) || problem["status"] != float64(http.StatusServiceUnavailable) ||
					!strings.HasSuffix(fmt.Sprint(problem["code"]), "-INT-001") {
					t.Errorf("%s: %s %s answered %d with Retry-After %q: %s, want 503 with Retry-After 1 to 3 and an INT code", fault.name, call.method, call.path, r.status, r.header.Get("Retry-After"), r.body)
				}
			}
			logged := e.logs.String()[before:]
			// The query and the command runtime each log a cause once in each interval.
			if strings.Count(logged, `"cause":"`+fault.cause+`"`) < 1 {
				t.Errorf("%s: the log does not name the cause %s: %s", fault.name, fault.cause, logged)
			}
			if strings.Contains(logged, "not found\"") || strings.Contains(logged, "connection refused") || strings.Contains(logged, id) {
				t.Errorf("%s: the log holds provider text or the row key: %s", fault.name, logged)
			}
		}
		// A refusal of the provider, such as for a bad key, is no retry case: 500 with no Retry-After.
		e.decision.fail(t, status.Error(codes.PermissionDenied, "bad key"))
		for _, call := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/orders", ""},
			{http.MethodGet, "/api/v1/orders/" + id, ""},
			{http.MethodPatch, "/api/v1/orders/" + id, `{"qty":3}`},
		} {
			key := ""
			if call.method != http.MethodGet {
				key = "k-" + uuid.NewString()
			}
			if r := e.send(a, call.method, call.path, key, call.body); r.status != http.StatusInternalServerError || r.header.Get("Retry-After") != "" || strings.Contains(string(r.body), "bad key") {
				t.Errorf("a refused provider: %s %s answered %d with Retry-After %q: %s, want 500 with none", call.method, call.path, r.status, r.header.Get("Retry-After"), r.body)
			}
		}
		e.decision.fail(t, nil)
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+id, "", ""); r.status != http.StatusOK || r.header.Get("Retry-After") != "" {
			t.Errorf("after the fault the read answered %d with Retry-After %q, want 200 with none", r.status, r.header.Get("Retry-After"))
		}
		var qty float64
		if r := e.send(a, http.MethodGet, "/api/v1/orders/"+id, "", ""); r.status == http.StatusOK {
			qty, _ = r.object(t)["qty"].(float64)
		}
		if qty != 1 {
			t.Errorf("the 503 patch changed the order: qty %v, want 1", qty)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T08 A jsonb field is written, read and patched as JSON on the runtime data path", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		id := e.create(a, "todos", "todo", `{"title":"with meta","meta":{"tags":["a","b"],"n":1}}`)
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND meta = '{"n":1,"tags":["a","b"]}'::jsonb`, id); n != 1 {
			t.Errorf("%d Todo rows whose jsonb column holds the created value, want 1", n)
		}
		meta := func(r reply) any {
			t.Helper()
			if r.status != http.StatusOK {
				t.Fatalf("the read answered %d %s, want 200", r.status, r.body)
			}
			return r.object(t)["meta"]
		}
		got, _ := json.Marshal(meta(e.send(a, http.MethodGet, "/api/v1/todos/"+id, "", "")))
		if string(got) != `{"n":1,"tags":["a","b"]}` {
			t.Errorf("the read gave meta %s, want the created JSON value", got)
		}
		patched := e.send(a, http.MethodPatch, "/api/v1/todos/"+id, "k-"+uuid.NewString(), `{"meta":[1,2]}`)
		if patched.status != http.StatusOK {
			t.Fatalf("the patch answered %d %s, want 200", patched.status, patched.body)
		}
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND meta = '[1,2]'::jsonb AND title = 'with meta'`, id); n != 1 {
			t.Errorf("%d Todo rows with the patched jsonb value and the title kept, want 1", n)
		}
		// An optional jsonb field that the body does not give is NULL in its column and absent from the answer.
		bare := e.create(a, "todos", "todo", `{"title":"no meta"}`)
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND meta IS NULL`, bare); n != 1 {
			t.Errorf("%d Todo rows with a NULL meta, want 1", n)
		}
		read := e.send(a, http.MethodGet, "/api/v1/todos/"+bare, "", "")
		if _, ok := read.object(t)["meta"]; ok || read.status != http.StatusOK {
			t.Errorf("the read of a Todo with no meta answered %d %s, want 200 with no meta member", read.status, read.body)
		}
		list := e.send(a, http.MethodGet, "/api/v1/todos", "", "")
		if list.status != http.StatusOK || !strings.Contains(string(list.body), `"meta":[1,2]`) {
			t.Errorf("the list answered %d %s, want 200 with the jsonb value of the row", list.status, list.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T12 An optional field that is NULL is absent from the answer, and an empty or false value is answered", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		absent := func(what string, r reply, fields ...string) {
			t.Helper()
			if r.status != http.StatusOK && r.status != http.StatusCreated {
				t.Fatalf("%s answered %d %s, want 200 or 201", what, r.status, r.body)
			}
			for _, field := range fields {
				if value, ok := r.object(t)[field]; ok {
					t.Errorf("%s answered %s %v in %s, want no %s member", what, field, value, r.body, field)
				}
			}
		}
		// A create that sends no note and no done stores NULL, and each answer leaves both out (ST-SUP-Q128 (B)).
		created := e.send(a, http.MethodPost, "/api/v1/todos", "k-"+uuid.NewString(), `{"title":"unset"}`)
		absent("the create", created, "note", "done")
		id := created.member(t, "id")
		e.own(a, "todo", id)
		absent("the read", e.send(a, http.MethodGet, "/api/v1/todos/"+id, "", ""), "note", "done")
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND note IS NULL AND done IS NULL`, id); n != 1 {
			t.Errorf("%d rows with a NULL note and done, want 1", n)
		}
		// A create that sends "" and false answers them: the client tells an unset field from its zero value.
		set := e.send(a, http.MethodPost, "/api/v1/todos", "k-"+uuid.NewString(), `{"title":"zero","note":"","done":false}`)
		if set.status != http.StatusCreated {
			t.Fatalf("the create with an empty note answered %d %s, want 201", set.status, set.body)
		}
		setID := set.member(t, "id")
		e.own(a, "todo", setID)
		for what, r := range map[string]reply{"the create": set, "the read": e.send(a, http.MethodGet, "/api/v1/todos/"+setID, "", "")} {
			note, hasNote := r.object(t)["note"]
			done, hasDone := r.object(t)["done"]
			if !hasNote || note != "" || !hasDone || done != false {
				t.Errorf("%s of a Todo with an empty note and done false answered %s, want note \"\" and done false", what, r.body)
			}
		}
		// A PUT that leaves out the note stores NULL and answers no note; a patch then sets it.
		absent("the update", e.send(a, http.MethodPut, "/api/v1/todos/"+setID, "", `{"title":"put"}`), "note", "done")
		if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND note IS NULL`, setID); n != 1 {
			t.Errorf("%d rows with a NULL note after the PUT, want 1", n)
		}
		patched := e.send(a, http.MethodPatch, "/api/v1/todos/"+setID, "k-"+uuid.NewString(), `{"note":"n"}`)
		if note, _ := patched.object(t)["note"].(string); note != "n" {
			t.Errorf("the patch answered %s, want note n", patched.body)
		}
		// The list leaves out the NULL fields of the first row and answers the note of the second.
		list := e.send(a, http.MethodGet, "/api/v1/todos", "", "")
		if list.status != http.StatusOK || strings.Count(string(list.body), `"note"`) != 1 || strings.Contains(string(list.body), `"done"`) {
			t.Errorf("the list answered %d %s, want one note member and no done member", list.status, list.body)
		}
	})

	t.Run("ST-SUP-RC20-OC06-T03 A create, its replay, a read, a list, an update and a patch answer an int64 above 2^53 and the int64 bounds exactly", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// tally reads the member as its JSON text: a float64 of 9007199254740993 is 9007199254740992 (walkthrough 27,
		// W-PREC).
		tally := func(what string, r reply, want string) {
			t.Helper()
			if r.status != http.StatusOK && r.status != http.StatusCreated {
				t.Fatalf("%s answered %d %s, want 200 or 201", what, r.status, r.body)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(r.body, &members); err != nil || string(members["tally"]) != want {
				t.Errorf("%s answered tally %s in %s, want %s", what, members["tally"], r.body, want)
			}
		}
		for _, value := range []string{"9007199254740993", "9223372036854775807", "-9223372036854775808"} {
			key := "k-" + uuid.NewString()
			created := e.send(a, http.MethodPost, "/api/v1/todos", key, `{"title":"big","tally":`+value+`}`)
			tally("the create of "+value, created, value)
			// The second request of the key gets the stored response, which holds the value exactly too.
			tally("the replay of the create of "+value, e.send(a, http.MethodPost, "/api/v1/todos", key, `{"title":"big","tally":`+value+`}`), value)
			id := created.member(t, "id")
			e.own(a, "todo", id)
			if n := e.count(`SELECT count(*) FROM todos WHERE id = $1::uuid AND tally = $2::bigint`, id, value); n != 1 {
				t.Errorf("%d rows with the tally %s, want 1", n, value)
			}
			tally("the read of "+value, e.send(a, http.MethodGet, "/api/v1/todos/"+id, "", ""), value)
			tally("the update of "+value, e.send(a, http.MethodPut, "/api/v1/todos/"+id, "", `{"title":"put","tally":`+value+`}`), value)
			tally("the patch of "+value, e.send(a, http.MethodPatch, "/api/v1/todos/"+id, "k-"+uuid.NewString(), `{"title":"patched"}`), value)
		}
		list := e.send(a, http.MethodGet, "/api/v1/todos", "", "")
		selected := e.send(a, http.MethodGet, "/api/v1/todos?fields=tally", "", "")
		for what, r := range map[string]reply{"the list": list, "the list with fields=tally": selected} {
			for _, value := range []string{"9007199254740993", "9223372036854775807", "-9223372036854775808"} {
				if r.status != http.StatusOK || !strings.Contains(string(r.body), `"tally":`+value+`,`) && !strings.Contains(string(r.body), `"tally":`+value+`}`) {
					t.Errorf("%s answered %d %s, want the tally %s exactly", what, r.status, r.body, value)
				}
			}
		}
	})

	t.Run("ST-SUP-RC20-OC37-T10 A NULL timestamp is absent from the answer, and each timestamp is answered in UTC", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		if time.Now().Location() != checkZone {
			t.Fatal("the checks do not run in the zone west of UTC")
		}
		utc := func(what string, r reply) {
			t.Helper()
			for _, field := range []string{"due_at", "seen_at"} {
				value, _ := r.object(t)[field].(string)
				at, err := time.Parse(time.RFC3339Nano, value)
				if err != nil || !strings.HasSuffix(value, "Z") || time.Since(at) > time.Hour || time.Since(at) < -time.Minute {
					t.Errorf("%s answered %s %q in %s, want the time of the create in UTC", what, field, value, r.body)
				}
			}
		}
		// A timestamp is server-managed (ST-SUP-RC20-OC17): the create sets it, and each answer gives it in UTC.
		created := e.send(a, http.MethodPost, "/api/v1/todos", "k-"+uuid.NewString(), `{"title":"due"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "todo", id)
		utc("the create", created)
		utc("the read", e.send(a, http.MethodGet, "/api/v1/todos/"+id, "", ""))
		utc("the update", e.send(a, http.MethodPut, "/api/v1/todos/"+id, "", `{"title":"due again"}`))
		utc("the patch", e.send(a, http.MethodPatch, "/api/v1/todos/"+id, "k-"+uuid.NewString(), `{"title":"due patched"}`))
		// A row of an earlier release holds NULL: the read and the list answer no due_at member, and not the zero time.
		if _, err := e.admin.ExecContext(e.ctx, `UPDATE todos SET due_at = NULL WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		read := e.send(a, http.MethodGet, "/api/v1/todos/"+id, "", "")
		if _, ok := read.object(t)["due_at"]; ok || read.status != http.StatusOK {
			t.Errorf("the read of a Todo with a NULL due_at answered %d %s, want 200 with no due_at member", read.status, read.body)
		}
		list := e.send(a, http.MethodGet, "/api/v1/todos", "", "")
		if list.status != http.StatusOK || strings.Contains(string(list.body), "0001-01-01") || strings.Contains(string(list.body), `"due_at"`) {
			t.Errorf("the list answered %d %s, want 200 with no due_at member", list.status, list.body)
		}
	})

	t.Run("ST-SUP-RC20-OC50-T01 A create or an update that breaks a unique key answers 409 that names the key and no value", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		const code = "m-jane.doe@example.com"
		body := `{"title":"first","starts_at":"2026-12-01T10:00:00Z","code":"` + code + `"}`
		first := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), body)
		if first.status != http.StatusCreated {
			t.Fatalf("the first create answered %d %s, want 201", first.status, first.body)
		}
		e.own(a, "meeting", first.member(t, "id"))
		want := "the Meeting was not written: another Meeting has the same code, which must be unique"
		second := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), body)
		if second.status != http.StatusConflict || second.member(t, "detail") != want || strings.Contains(string(second.body), code) {
			t.Fatalf("a second create with the same code answered %d %s, want 409 with %q and no value", second.status, second.body, want)
		}
		other := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), `{"title":"other","starts_at":"2026-12-01T10:00:00Z"}`)
		if other.status != http.StatusCreated {
			t.Fatalf("a create with no code answered %d %s, want 201", other.status, other.body)
		}
		id := other.member(t, "id")
		e.own(a, "meeting", id)
		updated := e.send(a, http.MethodPut, "/api/v1/meetings/"+id, "", body)
		if updated.status != http.StatusConflict || updated.member(t, "detail") != want || strings.Contains(string(updated.body), code) {
			t.Fatalf("an update to the same code answered %d %s, want 409 with %q and no value", updated.status, updated.body, want)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T14 A create or an update that leaves out a field with a default stores the default", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		defaults := map[string]any{"state": "planned", "seats": float64(8), "rate": 1.5, "remote": true}
		answered := func(what string, r reply, want map[string]any) {
			t.Helper()
			object := r.object(t)
			for field, value := range want {
				if object[field] != value {
					t.Errorf("%s answered %s %v in %s, want %v", what, field, object[field], r.body, value)
				}
			}
		}
		// A body that leaves out each field with a default, and one that sends them as null, store the defaults.
		var ids []string
		for _, body := range []string{
			`{"title":"plain","starts_at":"2026-12-01T10:00:00Z"}`,
			`{"title":"nulls","starts_at":"2026-12-01T10:00:00Z","state":null,"seats":null,"rate":null,"remote":null}`,
		} {
			created := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), body)
			if created.status != http.StatusCreated {
				t.Fatalf("the create %s answered %d %s, want 201", body, created.status, created.body)
			}
			id := created.member(t, "id")
			e.own(a, "meeting", id)
			ids = append(ids, id)
			answered("the create "+body, created, defaults)
			if n := e.count(`SELECT count(*) FROM meetings WHERE id = $1::uuid AND state = 'planned' AND seats = 8 AND rate = 1.5 AND remote`, id); n != 1 {
				t.Errorf("%d meeting rows of %s with the defaults, want 1", n, body)
			}
		}
		// A value that the body gives, also the zero value of its type, is stored as given.
		given := map[string]any{"state": "held", "seats": float64(0), "rate": float64(0), "remote": false}
		created := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), `{"title":"given","starts_at":"2026-12-01T10:00:00Z","state":"held","seats":0,"rate":0,"remote":false}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create with the values given answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "meeting", id)
		answered("the create with the values given", created, given)
		updated := e.send(a, http.MethodPut, "/api/v1/meetings/"+id, "", `{"title":"given","starts_at":"2026-12-01T10:00:00Z","seats":3}`)
		answered("the update that leaves out state, rate and remote", updated, map[string]any{"state": "planned", "seats": float64(3), "rate": 1.5, "remote": true})
		if n := e.count(`SELECT count(*) FROM meetings WHERE id = $1::uuid AND state = 'planned' AND seats = 3 AND rate = 1.5 AND remote`, id); n != 1 {
			t.Errorf("%d meeting rows with the update's values and the defaults, want 1", n)
		}
		answered("the read", e.send(a, http.MethodGet, "/api/v1/meetings/"+ids[0], "", ""), defaults)
	})

	t.Run("ST-SUP-RC20-OC17-T05 A client-set timestamp keeps the client's time in UTC and refuses a value that is not an RFC 3339 time", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		answered := func(what string, r reply, want map[string]string) {
			t.Helper()
			object := r.object(t)
			for field, value := range want {
				if got, _ := object[field].(string); got != value {
					t.Errorf("%s answered %s %q in %s, want %q", what, field, got, r.body, value)
				}
			}
		}
		// The create keeps the client's times in UTC, and noted_at, which is not client_set, is the server's time.
		key := "k-" + uuid.NewString()
		body := `{"title":"plan","starts_at":"2026-12-01T10:00:00+02:00","ends_at":"2026-12-01T11:30:00.5+02:00","noted_at":"2000-01-01T00:00:00Z"}`
		created := e.send(a, http.MethodPost, "/api/v1/meetings", key, body)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "meeting", id)
		answered("the create", created, map[string]string{"starts_at": "2026-12-01T08:00:00Z", "ends_at": "2026-12-01T09:30:00.5Z"})
		noted, _ := created.object(t)["noted_at"].(string)
		if at, err := time.Parse(time.RFC3339Nano, noted); err != nil || time.Since(at) > time.Hour {
			t.Errorf("the create answered noted_at %q, want the server's time of the create, not the client's", noted)
		}
		if n := e.count(`SELECT count(*) FROM meetings WHERE id = $1::uuid AND starts_at = '2026-12-01T08:00:00Z' AND ends_at = '2026-12-01T09:30:00.5Z' AND noted_at > now() - interval '1 hour'`, id); n != 1 {
			t.Errorf("%d meeting rows with the client's times and the server's noted_at, want 1", n)
		}
		// The same key with another time is another request (the fingerprint holds the client-set field): 422. A
		// member matches a field only in its exact letter case, so STARTS_AT is refused with a 400 that names it, with
		// the key of the first request or a new key (ST-SUP-RC20-OC55).
		reused := func(what, key, body string) {
			t.Helper()
			r := e.send(a, http.MethodPost, "/api/v1/meetings", key, body)
			if problem, _ := r.object(t)["type"].(string); r.status != http.StatusUnprocessableEntity || !strings.HasSuffix(problem, "idempotency-key-reused") {
				t.Errorf("the same key with %s answered %d %s, want 422 idempotency-key-reused", what, r.status, r.body)
			}
		}
		reused("another starts_at", key, strings.Replace(body, "10:00:00", "10:05:00", 1))
		folded := strings.Replace(body, `"starts_at"`, `"STARTS_AT"`, 1)
		for _, k := range []string{key, "k-" + uuid.NewString()} {
			if r := e.send(a, http.MethodPost, "/api/v1/meetings", k, folded); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"STARTS_AT"`) {
				t.Errorf("a create with STARTS_AT answered %d %s, want 400 that names STARTS_AT", r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM meetings WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d meeting rows after the reused keys, want 1", n)
		}
		answered("the read", e.send(a, http.MethodGet, "/api/v1/meetings/"+id, "", ""), map[string]string{"starts_at": "2026-12-01T08:00:00Z"})
		// A value that is not an RFC 3339 time, the zero time, and a required time left out or null are refused with
		// a 400 problem that names the field, and write no row.
		for _, c := range []struct{ body, field string }{
			{`{"title":"bad","starts_at":"tomorrow"}`, "starts_at"},
			{`{"title":"bad","starts_at":"2026-12-01 10:00:00"}`, "starts_at"},
			{`{"title":"bad","starts_at":"2026-12-01T10:00:00"}`, "starts_at"},
			{`{"title":"bad","starts_at":1764583200}`, "starts_at"},
			{`{"title":"bad","starts_at":"0001-01-01T00:00:00Z"}`, "starts_at"},
			// An offset that moves the UTC year outside 1 to 9999: the answer in UTC cannot encode it.
			{`{"title":"bad","starts_at":"9999-12-31T23:59:59-14:00"}`, "starts_at must be an RFC 3339 time whose UTC year is from 1 to 9999"},
			{`{"title":"bad","starts_at":"0000-01-01T00:00:00+01:00"}`, "starts_at must be an RFC 3339 time whose UTC year is from 1 to 9999"},
			{`{"title":"bad","starts_at":"2026-12-01T10:00:00Z","ends_at":"9999-12-31T23:00:00-01:30"}`, "ends_at must be an RFC 3339 time whose UTC year is from 1 to 9999"},
			{`{"title":"bad"}`, "starts_at is required"},
			{`{"title":"bad","starts_at":null}`, "starts_at is required"},
			{`{"title":"bad","starts_at":"2026-12-01T10:00:00Z","ends_at":"soon"}`, "ends_at"},
			{`{"title":"bad","starts_at":"2026-12-01T10:00:00Z","ends_at":"0001-01-01T00:00:00Z"}`, "ends_at"},
		} {
			r := e.send(a, http.MethodPost, "/api/v1/meetings", "k-"+uuid.NewString(), c.body)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), c.field) || !strings.Contains(string(r.body), "RFC 3339") {
				t.Errorf("the create %s answered %d %s, want 400 naming %s and RFC 3339", c.body, r.status, r.body, c.field)
			}
		}
		if n := e.count(`SELECT count(*) FROM meetings WHERE tenant_id = $1`, a.tenant.ID()); n != 1 {
			t.Errorf("%d meeting rows after the refused creates, want 1", n)
		}
		// An update sets the client's time and keeps the server's noted_at; a patch changes one time.
		updated := e.send(a, http.MethodPut, "/api/v1/meetings/"+id, "", `{"title":"plan","starts_at":"2026-12-02T09:00:00-05:00"}`)
		answered("the update", updated, map[string]string{"starts_at": "2026-12-02T14:00:00Z", "noted_at": noted})
		if _, ok := updated.object(t)["ends_at"]; ok {
			t.Errorf("the update with no ends_at answered %s, want no ends_at member", updated.body)
		}
		if r := e.send(a, http.MethodPut, "/api/v1/meetings/"+id, "", `{"title":"plan"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "starts_at is required") {
			t.Errorf("the update with no starts_at answered %d %s, want 400", r.status, r.body)
		}
		patched := e.send(a, http.MethodPatch, "/api/v1/meetings/"+id, "k-"+uuid.NewString(), `{"ends_at":"2026-12-02T16:00:00+01:00"}`)
		answered("the patch", patched, map[string]string{"starts_at": "2026-12-02T14:00:00Z", "ends_at": "2026-12-02T15:00:00Z"})
		for _, bad := range []string{`{"starts_at":"later"}`, `{"ends_at":"0001-01-01T00:00:00Z"}`, `{"ends_at":"9999-12-31T23:59:59-14:00"}`} {
			if r := e.send(a, http.MethodPatch, "/api/v1/meetings/"+id, "k-"+uuid.NewString(), bad); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "RFC 3339") {
				t.Errorf("the patch %s answered %d %s, want 400", bad, r.status, r.body)
			}
		}
		answered("the read after the refused patches", e.send(a, http.MethodGet, "/api/v1/meetings/"+id, "", ""), map[string]string{"starts_at": "2026-12-02T14:00:00Z", "ends_at": "2026-12-02T15:00:00Z"})
	})

	t.Run("ST-SUP-RC28-OC01-T06 A secret jsonb field reaches neither the audit rows, the answer nor the stored response", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		first, second := "vault-"+uuid.NewString(), "vault-"+uuid.NewString()
		key := "k-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/customers", key, `{"name":"Vic","api_token":"tok","plan":"free","vault":{"pin":"`+first+`"}}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "customer", id)
		r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Vic","api_token":"tok","plan":"pro","vault":{"pin":"`+second+`"}}`)
		if r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		read := e.send(a, http.MethodGet, "/api/v1/customers/"+id, "", "")
		for name, body := range map[string][]byte{"create": created.body, "update": r.body, "read": read.body} {
			if strings.Contains(string(body), first) || strings.Contains(string(body), second) || strings.Contains(string(body), "vault") {
				t.Errorf("the answer of the %s holds the secret jsonb field: %s", name, body)
			}
		}
		rows, err := e.admin.QueryContext(e.ctx, `SELECT row_to_json(a)::text FROM audit_event a WHERE tenant_id = $1`, a.tenant.ID())
		must(t, err)
		defer rows.Close()
		n := 0
		for rows.Next() {
			var row string
			must(t, rows.Scan(&row))
			n++
			if strings.Contains(row, first) || strings.Contains(row, second) || strings.Contains(row, "vault") {
				t.Errorf("an audit row holds the secret jsonb field: %s", row)
			}
		}
		must(t, rows.Err())
		if n < 2 {
			t.Errorf("%d audit rows of the tenant, want the create and the update rows", n)
		}
		// The audit messages of the tenant in the outbox, which the audit sink gets, hold no secret jsonb field.
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND kind = 'audit.event'`, a.tenant.ID()); n < 2 {
			t.Errorf("%d audit messages of the tenant in the outbox, want the create and the update", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND (position(convert_to($2, 'UTF8') in payload) > 0 OR position(convert_to($3, 'UTF8') in payload) > 0 OR position(convert_to('vault', 'UTF8') in payload) > 0)`, a.tenant.ID(), first, second); n != 0 {
			t.Errorf("%d outbox messages of the tenant hold the secret jsonb field, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 AND status = 'completed' AND position(convert_to($3, 'UTF8') in coalesce(response_body, '')) = 0 AND position(convert_to('vault', 'UTF8') in coalesce(response_body, '')) = 0`, a.tenant.ID(), key, first); n != 1 {
			t.Errorf("%d completed key rows whose stored response holds no secret jsonb field, want 1", n)
		}
		// The source row holds the secret jsonb sealed, never in clear (decision ST-DECL-Q44).
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND substring(vault from 1 for 1) = '\x01'::bytea
 AND position(convert_to($2, 'UTF8') in vault) = 0 AND position(convert_to($3, 'UTF8') in vault) = 0`, id, first, second); n != 1 {
			t.Errorf("%d source rows that hold the secret jsonb sealed and not in clear, want 1", n)
		}
	})

	t.Run("ST-SUP-RC28-OC01-T07 A pii jsonb field is the redaction marker in the before, the after and the patch", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		first, second := "mail-"+uuid.NewString(), "mail-"+uuid.NewString()
		id := e.create(a, "customers", "customer", `{"name":"Pia","api_token":"tok","plan":"free","contact":{"email":"`+first+`"}}`)
		if r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Pia","api_token":"tok","plan":"free","contact":{"email":"`+second+`"}}`); r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		var before, after, patch, row string
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT before::text, after::text, patch::text, row_to_json(a)::text FROM audit_event a WHERE tenant_id = $1 AND action = 'update' AND resource_id = $2`, a.tenant.ID(), id).Scan(&before, &after, &patch, &row))
		for name, state := range map[string]string{"before": before, "after": after} {
			var m map[string]any
			must(t, json.Unmarshal([]byte(state), &m))
			if m["contact"] != audit.RedactionMarker {
				t.Errorf("the %s of the update holds contact %v, want the redaction marker", name, m["contact"])
			}
		}
		var ops []map[string]any
		must(t, json.Unmarshal([]byte(patch), &ops))
		found := false
		for _, op := range ops {
			found = found || (op["path"] == "/contact" && op["value"] == audit.RedactionMarker)
		}
		if !found {
			t.Errorf("the patch %s has no change of /contact to the redaction marker", patch)
		}
		if strings.Contains(row, first) || strings.Contains(row, second) {
			t.Errorf("the audit row holds the pii jsonb value: %s", row)
		}
		if n := e.count(`SELECT count(*) FROM customers WHERE id = $1::uuid AND contact->>'email' = $2`, id, second); n != 1 {
			t.Errorf("%d source rows that hold the pii jsonb value, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC24-T05 A scoped collection on the runtime data path checks each row against the parent of the path", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a, other := e.as("u-1", tn), e.as("u-2", tn)
		s1 := e.create(a, "folders", "folder", `{"name":"one"}`)
		s2 := e.create(a, "folders", "folder", `{"name":"two"}`)
		foreign := e.create(other, "folders", "folder", `{"name":"not mine"}`)
		books := func(folder string) string { return "/api/v1/folders/" + folder + "/books" }
		// A body that names another folder than the path is refused with a problem that names the field, and
		// writes nothing; a body that gives the folder of the path is taken.
		empty := e.count(`SELECT count(*) FROM books`)
		if r := e.send(a, http.MethodPost, books(s1), "k-"+uuid.NewString(), `{"title":"t1","folder_id":"`+s2+`"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"folder_id"`) {
			t.Errorf("a create with another folder in the body answered %d %s, want 400 that names folder_id", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM books`); n != empty {
			t.Errorf("%d book rows after the refused create, want %d", n, empty)
		}
		created := e.send(a, http.MethodPost, books(s1), "k-"+uuid.NewString(), `{"title":"t1","folder_id":"`+s1+`"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		if href := created.member(t, "href"); href != books(s1)+"/"+id {
			t.Errorf("the href is %q, want the path of the book under its folder", href)
		}
		e.own(a, "book", id)
		if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id = $2::uuid AND title = 't1'`, id, s1); n != 1 {
			t.Errorf("%d book rows on the folder of the path, want 1", n)
		}
		if r := e.send(a, http.MethodGet, books(s1)+"/"+id, "", ""); r.status != http.StatusOK {
			t.Errorf("the read under its folder answered %d %s, want 200", r.status, r.body)
		}
		// Under another parent each operation on the row is not found and changes nothing.
		for _, c := range []struct{ method, key, body string }{
			{http.MethodGet, "", ""},
			{http.MethodPut, "", `{"title":"moved"}`},
			{http.MethodPatch, "k-" + uuid.NewString(), `{"title":"moved"}`},
			{http.MethodDelete, "", ""},
		} {
			if r := e.send(a, c.method, books(s2)+"/"+id, c.key, c.body); r.status != http.StatusNotFound {
				t.Errorf("%s under another folder answered %d %s, want 404", c.method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id = $2::uuid AND title = 't1'`, id, s1); n != 1 {
			t.Errorf("%d book rows as created after the writes under another folder, want 1", n)
		}
		// An update with another folder in the body is refused and changes nothing; one with no folder keeps the
		// parent of the path.
		if r := e.send(a, http.MethodPut, books(s1)+"/"+id, "", `{"title":"t2","folder_id":"`+s2+`"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"folder_id"`) {
			t.Errorf("an update with another folder in the body answered %d %s, want 400 that names folder_id", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id = $2::uuid AND title = 't1'`, id, s1); n != 1 {
			t.Errorf("%d book rows as created after the refused update, want 1", n)
		}
		if r := e.send(a, http.MethodPut, books(s1)+"/"+id, "", `{"title":"t2"}`); r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id = $2::uuid AND title = 't2'`, id, s1); n != 1 {
			t.Errorf("%d book rows updated on their folder, want 1", n)
		}
		e.create(a, "folders/"+s2+"/books", "book", `{"title":"on two"}`)
		// The list holds the books of the folder of the path only.
		list := e.send(a, http.MethodGet, books(s1), "", "")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		must(t, json.Unmarshal(list.body, &page))
		if list.status != http.StatusOK || len(page.Items) != 1 || page.Items[0]["id"] != id {
			t.Errorf("the list of folder one answered %d %s, want the one book of that folder", list.status, list.body)
		}
		// A create under a folder that the caller may not edit, that does not exist or that is not an ID writes
		// nothing.
		before := e.count(`SELECT count(*) FROM books`)
		for name, folder := range map[string]string{"another owner's folder": foreign, "a missing folder": uuid.NewString(), "not an ID": "x"} {
			if r := e.send(a, http.MethodPost, books(folder), "k-"+uuid.NewString(), `{"title":"no"}`); r.status != http.StatusNotFound {
				t.Errorf("a create under %s answered %d %s, want 404", name, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM books`); n != before {
			t.Errorf("%d book rows after the refused creates, want %d", n, before)
		}
	})

	t.Run("ST-SUP-RC20-OC24-T09 A collection three levels under its top ancestor checks every ancestor of the path, and a wrong, foreign or smuggled ancestor is 404", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a := e.as("u-1", tn)
		// The caller owns the folders only: each rack, card and mark is reached through the parent relation that the
		// runtime writes from its reference, so a read of a mark proves the relations along the chain.
		f1 := e.create(a, "folders", "folder", `{"name":"one"}`)
		f2 := e.create(a, "folders", "folder", `{"name":"two"}`)
		racks := func(f string) string { return "/folders/" + f + "/racks" }
		cards := func(f, r string) string { return racks(f) + "/" + r + "/cards" }
		marks := func(f, r, c string) string { return cards(f, r) + "/" + c + "/marks" }
		rack := func(f, name string) string {
			return e.createUnder(a, racks(f), "rack", "folder", f, `{"name":"`+name+`"}`)
		}
		r1, r2, r3 := rack(f1, "r1"), rack(f2, "r2"), rack(f2, "r3")
		card := func(f, r, title string) string {
			return e.createUnder(a, cards(f, r), "card", "rack", r, `{"title":"`+title+`"}`)
		}
		c1, c2, c3 := card(f1, r1, "c1"), card(f2, r2, "c2"), card(f2, r2, "c3")
		e.createUnder(a, marks(f1, r1, c1), "mark", "card", c1, `{"label":"l1","note":"n1"}`)
		// A body that names another card than the path is refused and writes nothing.
		if r := e.send(a, http.MethodPost, "/api/v1"+marks(f2, r2, c2), "k-"+uuid.NewString(), `{"label":"l2","note":"n2","card_id":"`+c1+`"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"card_id"`) {
			t.Errorf("a create of a mark with another card in the body answered %d %s, want 400 that names card_id", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM marks WHERE label = 'l2'`); n != 0 {
			t.Errorf("%d mark rows after the refused create, want 0", n)
		}
		created := e.send(a, http.MethodPost, "/api/v1"+marks(f2, r2, c2), "k-"+uuid.NewString(), `{"label":"l2","note":"n2"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of a mark answered %d %s, want 201", created.status, created.body)
		}
		m2 := created.member(t, "id")
		// The parent of the mark is the card of the path.
		e.relayParent(a, "mark", m2, "card", c2)
		if href := created.member(t, "href"); href != "/api/v1"+marks(f2, r2, c2)+"/"+m2 {
			t.Errorf("the href is %q, want the path of the mark under each of its ancestors", href)
		}
		if n := e.count(`SELECT count(*) FROM marks WHERE id = $1::uuid AND card_id = $2::uuid`, m2, c2); n != 1 {
			t.Errorf("%d mark rows on the card of the path, want 1", n)
		}
		if r := e.send(a, http.MethodGet, "/api/v1"+marks(f2, r2, c2)+"/"+m2, "", ""); r.status != http.StatusOK || r.member(t, "href") != "/api/v1"+marks(f2, r2, c2)+"/"+m2 {
			t.Fatalf("the read under its ancestors answered %d %s, want 200 with its href", r.status, r.body)
		}
		// The foreign keys of the chain hold the tenant: a card of another tenant cannot name a rack of this one.
		for _, fk := range []struct{ table, to string }{{"racks", "folders"}, {"cards", "racks"}, {"marks", "cards"}} {
			if n := e.count(`SELECT count(*) FROM pg_catalog.pg_constraint WHERE contype = 'f' AND conrelid = ('public.' || $1)::regclass AND confrelid = ('public.' || $2)::regclass AND array_length(conkey, 1) = 2`, fk.table, fk.to); n != 1 {
				t.Errorf("%d foreign keys from %s to %s with the tenant column, want 1", n, fk.table, fk.to)
			}
		}
		other := e.as("u-1", newTenant())
		fx := e.create(other, "folders", "folder", `{"name":"x"}`)
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO cards (tenant_id, id, rack_id, title) VALUES ($1, $2::uuid, $3::uuid, 'x')`, other.tenant.ID(), uuid.NewString(), r2); err == nil || !strings.Contains(err.Error(), "cards_rack_id_fkey") {
			t.Errorf("a card of another tenant under a rack of this tenant gave %v, want the refusal of the foreign key cards_rack_id_fkey", err)
		}
		rx := e.createUnder(other, racks(fx), "rack", "folder", fx, `{"name":"rx"}`)
		cx := e.createUnder(other, cards(fx, rx), "card", "rack", rx, `{"title":"cx"}`)
		beforeMarks := e.count(`SELECT count(*) FROM marks`)
		item := func(base string) string { return "/api/v1" + base + "/" + m2 }
		// Each case is a path to the mark m2 that a check refuses; chain tells that the chain itself is wrong, so the
		// collection operations under it are refused too.
		for _, c := range []struct {
			name  string
			who   caller
			base  string
			chain bool
		}{
			{"a rack of another folder", a, marks(f1, r2, c2), true},
			{"a card of another rack", a, marks(f2, r3, c2), true},
			{"a mark of another card", a, marks(f2, r2, c3), false},
			{"the mark smuggled under another chain", a, marks(f1, r1, c1), false},
			{"a folder that is not an ID", a, marks("x", r2, c2), true},
			{"a rack that is not an ID", a, marks(f2, "x", c2), true},
			{"a card that is not an ID", a, marks(f2, r2, "x"), true},
			{"a missing folder", a, marks(uuid.NewString(), r2, c2), true},
			{"the chain from another tenant", other, marks(f2, r2, c2), true},
			{"a card of another tenant smuggled under the caller's rack", other, marks(fx, rx, c2), true},
			{"the caller's card under a rack of another tenant", a, marks(f2, r2, cx), true},
		} {
			for _, op := range []struct{ method, key, body string }{
				{http.MethodGet, "", ""},
				{http.MethodPut, "", `{"label":"l2","note":"moved"}`},
				{http.MethodPatch, "k-" + uuid.NewString(), `{"note":"moved"}`},
				{http.MethodDelete, "", ""},
			} {
				if r := e.send(c.who, op.method, item(c.base), op.key, op.body); r.status != http.StatusNotFound {
					t.Errorf("%s: %s of the mark answered %d %s, want 404", c.name, op.method, r.status, r.body)
				}
			}
			if !c.chain {
				continue
			}
			for _, op := range []struct{ method, key, body string }{
				{http.MethodGet, "", ""},
				{http.MethodPost, "k-" + uuid.NewString(), `{"label":"new","note":"no"}`},
				{http.MethodPut, "", `{"label":"l2","note":"moved"}`},
			} {
				if r := e.send(c.who, op.method, "/api/v1"+c.base, op.key, op.body); r.status != http.StatusNotFound {
					t.Errorf("%s: %s of the collection answered %d %s, want 404", c.name, op.method, r.status, r.body)
				}
			}
		}
		if n := e.count(`SELECT count(*) FROM marks`); n != beforeMarks {
			t.Errorf("%d mark rows after the refused requests, want %d", n, beforeMarks)
		}
		if n := e.count(`SELECT count(*) FROM marks WHERE id = $1::uuid AND card_id = $2::uuid AND note = 'n2'`, m2, c2); n != 1 {
			t.Errorf("%d mark rows as created after the refused writes, want 1", n)
		}
		// Under its own ancestors each operation runs: the list holds the marks of the card of the path only.
		list := e.send(a, http.MethodGet, "/api/v1"+marks(f2, r2, c2), "", "")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		must(t, json.Unmarshal(list.body, &page))
		if list.status != http.StatusOK || len(page.Items) != 1 || page.Items[0]["id"] != m2 {
			t.Errorf("the list of the card answered %d %s, want its one mark", list.status, list.body)
		}
		if r := e.send(a, http.MethodPut, "/api/v1"+marks(f2, r2, c2), "", `{"label":"l2","note":"upserted"}`); r.status != http.StatusOK {
			t.Errorf("the upsert of the stored mark answered %d %s, want 200", r.status, r.body)
		}
		if r := e.send(a, http.MethodPatch, item(marks(f2, r2, c2)), "k-"+uuid.NewString(), `{"note":"patched"}`); r.status != http.StatusOK {
			t.Errorf("the patch under its ancestors answered %d %s, want 200", r.status, r.body)
		}
		if r := e.send(a, http.MethodDelete, item(marks(f2, r2, c2)), "", ""); r.status != http.StatusNoContent {
			t.Errorf("the delete under its ancestors answered %d %s, want 204", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC24-T10 A grant on a resource of the path reaches the rows below it through the scoped routes, and a caller with no grant gets 404", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a := e.as("u-1", tn)
		// The case of walkthrough 6 (K9): the folder is the workspace, the rack the project that its owner shares with
		// a viewer, the card a ticket and the mark a comment. A card and a mark are viewed through parent->view.
		f := e.create(a, "folders", "folder", `{"name":"f"}`)
		r := e.createUnder(a, "/folders/"+f+"/racks", "rack", "folder", f, `{"name":"r"}`)
		c := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards", "card", "rack", r, `{"title":"c"}`)
		m := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards/"+c+"/marks", "mark", "card", c, `{"label":"l","note":"n"}`)
		bob := e.as("u-bob", tn)
		e.relate(bob, "viewer", "rack", r)
		carol := e.as("u-carol", tn)
		rack := "/api/v1/folders/" + f + "/racks/" + r
		card := rack + "/cards/" + c
		mark := card + "/marks/" + m
		// list gives the IDs of a list that answered 200.
		list := func(who caller, path string) []string {
			got := e.send(who, http.MethodGet, path, "", "")
			if got.status != http.StatusOK {
				t.Errorf("the list %s answered %d %s, want 200", path, got.status, got.body)
				return nil
			}
			var page struct {
				Items []map[string]any `json:"items"`
			}
			must(t, json.Unmarshal(got.body, &page))
			var ids []string
			for _, item := range page.Items {
				ids = append(ids, fmt.Sprint(item["id"]))
			}
			return ids
		}
		// bob may view the rack and the rows below it, and not the folder above it.
		for _, path := range []string{rack, card, mark} {
			if got := e.send(bob, http.MethodGet, path, "", ""); got.status != http.StatusOK {
				t.Errorf("the viewer of the shared rack read %s with %d %s, want 200", path, got.status, got.body)
			}
		}
		if ids := list(bob, rack+"/cards"); len(ids) != 1 || ids[0] != c {
			t.Errorf("the viewer of the shared rack listed the cards %v, want [%s]", ids, c)
		}
		if ids := list(bob, card+"/marks"); len(ids) != 1 || ids[0] != m {
			t.Errorf("the viewer of the shared rack listed the marks %v, want [%s]", ids, m)
		}
		// The read gives the row and its href only, and no field of an ancestor.
		got := e.send(bob, http.MethodGet, mark, "", "")
		var body map[string]any
		must(t, json.Unmarshal(got.body, &body))
		for _, name := range []string{"name", "title"} {
			if _, ok := body[name]; ok {
				t.Errorf("the read of the mark holds the field %s of an ancestor: %s", name, got.body)
			}
		}
		// The folder decides on its own: bob may not read it or list the racks in it.
		for _, path := range []string{"/api/v1/folders/" + f, "/api/v1/folders/" + f + "/racks"} {
			if got := e.send(bob, http.MethodGet, path, "", ""); got.status != http.StatusNotFound {
				t.Errorf("the viewer of the shared rack read %s with %d %s, want 404", path, got.status, got.body)
			}
		}
		// A view is no edit: bob may not change the mark.
		if got := e.send(bob, http.MethodPatch, mark, "k-"+uuid.NewString(), `{"note":"bob"}`); got.status != http.StatusNotFound {
			t.Errorf("the viewer of the shared rack patched the mark with %d %s, want 404", got.status, got.body)
		}
		// carol has no grant: every route of the chain is 404, with no row in the body.
		for _, path := range []string{rack, rack + "/cards", card, card + "/marks", mark} {
			got := e.send(carol, http.MethodGet, path, "", "")
			if got.status != http.StatusNotFound || strings.Contains(string(got.body), `"items"`) || strings.Contains(string(got.body), `"note"`) {
				t.Errorf("a caller with no grant read %s with %d %s, want 404 with no row", path, got.status, got.body)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC24-T12 A missing, wrong or denied path gives the same 404 with the same calls to the authorization port, and changes nothing", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a := e.as("u-1", tn)
		f := e.create(a, "folders", "folder", `{"name":"f"}`)
		r := e.createUnder(a, "/folders/"+f+"/racks", "rack", "folder", f, `{"name":"r"}`)
		other := e.createUnder(a, "/folders/"+f+"/racks", "rack", "folder", f, `{"name":"other"}`)
		c := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards", "card", "rack", r, `{"title":"c"}`)
		c2 := e.createUnder(a, "/folders/"+f+"/racks/"+other+"/cards", "card", "rack", other, `{"title":"c2"}`)
		m := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards/"+c+"/marks", "mark", "card", c, `{"label":"l","note":"n"}`)
		f2 := e.create(a, "folders", "folder", `{"name":"f2"}`)
		marks := func(folder, rack, card string) string {
			return "/api/v1/folders/" + folder + "/racks/" + rack + "/cards/" + card + "/marks"
		}
		// Each path names the mark m, or the marks of a card, in a way that a check refuses for each caller.
		paths := []struct{ name, base, id string }{
			{"the path of the mark", marks(f, r, c), m},
			{"a missing folder", marks(uuid.NewString(), r, c), m},
			{"a folder of another rack", marks(f2, r, c), m},
			{"a missing rack", marks(f, uuid.NewString(), c), m},
			{"a rack of another card", marks(f, other, c), m},
			{"a missing card", marks(f, r, uuid.NewString()), m},
			{"a card of another rack", marks(f, r, c2), m},
			{"a mark of another card", marks(f, other, c2), m},
			{"a missing mark", marks(f, r, c), uuid.NewString()},
		}
		ops := []struct{ name, method, key, body string }{
			{"read", http.MethodGet, "", ""},
			{"update", http.MethodPut, "", `{"label":"l","note":"changed"}`},
			{"patch", http.MethodPatch, "k", `{"note":"changed"}`},
			{"delete", http.MethodDelete, "", ""},
			{"list", http.MethodGet, "", ""},
			{"create", http.MethodPost, "k", `{"label":"new","note":"new"}`},
			{"upsert", http.MethodPut, "", `{"label":"l","note":"changed"}`},
		}
		// grants are the callers by what they may do: nothing; view or edit the mark only; view or edit the card
		// only. For each one, a refused path gives one 404 with one count of calls; the paths that the grant allows
		// are left out. A viewer of the card may read the parent of a create and an upsert, which then decide edit on
		// it through the reference after the chain check, so its refusals on the path of the card are 404 with calls of
		// their own: they tell it nothing that a read of the card does not.
		apart := map[string]bool{"create|the path of the mark": true, "create|a missing mark": true, "upsert|the path of the mark": true, "upsert|a missing mark": true}
		grants := []struct {
			name    string
			grant   func(caller)
			allowed map[string]bool // "op|path" that the grant allows
			apart   map[string]bool // "op|path" that is 404 with calls of its own
		}{
			{"no grant", func(caller) {}, nil, nil},
			{"a viewer of the mark", func(b caller) { e.relate(b, "viewer", "mark", m) }, map[string]bool{"read|the path of the mark": true}, nil},
			{"the owner of the mark", func(b caller) { e.own(b, "mark", m) }, map[string]bool{"read|the path of the mark": true, "update|the path of the mark": true, "patch|the path of the mark": true, "delete|the path of the mark": true}, nil},
			{"a viewer of the card", func(b caller) { e.relate(b, "viewer", "card", c) }, map[string]bool{"read|the path of the mark": true, "list|the path of the mark": true, "list|a missing mark": true}, apart},
			{"the owner of the card", func(b caller) { e.own(b, "card", c) }, map[string]bool{"read|the path of the mark": true, "update|the path of the mark": true, "patch|the path of the mark": true, "delete|the path of the mark": true,
				"list|the path of the mark": true, "list|a missing mark": true, "create|the path of the mark": true, "create|a missing mark": true, "upsert|the path of the mark": true, "upsert|a missing mark": true}, nil},
		}
		n := 0
		for _, g := range grants {
			for _, op := range ops {
				var want *reply
				wantCalls := -1
				for _, p := range paths {
					if g.allowed[op.name+"|"+p.name] {
						continue
					}
					// A new caller for each request, so no decision of an earlier request is cached.
					n++
					b := e.as(fmt.Sprintf("u-t12-%d", n), tn)
					g.grant(b)
					path := p.base
					if op.name != "list" && op.name != "create" && op.name != "upsert" {
						path += "/" + p.id
					}
					key := ""
					if op.key != "" {
						key = "k-" + uuid.NewString()
					}
					before := e.decision.callsOf(b)
					got := e.send(b, op.method, path, key, op.body)
					calls := e.decision.callsOf(b) - before
					if got.status != http.StatusNotFound {
						t.Errorf("%s: the %s under %s answered %d %s, want 404", g.name, op.name, p.name, got.status, got.body)
						continue
					}
					if g.apart[op.name+"|"+p.name] {
						continue
					}
					if want == nil {
						want, wantCalls = &got, calls
						continue
					}
					if got.member(t, "title") != want.member(t, "title") || calls != wantCalls {
						t.Errorf("%s: the %s under %s answered %q with %d calls to the authorization port, want %q with %d calls, as for the first refused path", g.name, op.name, p.name, got.member(t, "title"), calls, want.member(t, "title"), wantCalls)
					}
				}
			}
		}
		if n := e.count(`SELECT count(*) FROM marks WHERE tenant_id = $1`, tn); n != 1 {
			t.Errorf("%d marks in the tenant after the refused writes, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM marks WHERE id = $1::uuid AND note = 'n'`, m); n != 1 {
			t.Errorf("the mark changed after the refused writes")
		}
	})

	t.Run("ST-SUP-RC20-OC24-T13 The parent chain check reads the ancestors only after the decision on the deepest resource, and decides nothing on them", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a := e.as("u-1", tn)
		f := e.create(a, "folders", "folder", `{"name":"f"}`)
		r := e.createUnder(a, "/folders/"+f+"/racks", "rack", "folder", f, `{"name":"r"}`)
		c := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards", "card", "rack", r, `{"title":"c"}`)
		m := e.createUnder(a, "/folders/"+f+"/racks/"+r+"/cards/"+c+"/marks", "mark", "card", c, `{"label":"l","note":"n"}`)
		base := "/api/v1/folders/" + f + "/racks/" + r + "/cards/" + c + "/marks"
		// poison makes each read of a row of the tenant in the racks fail, with no change to the rows of other
		// tenants: a check that reads the rack of the path answers 500, and not 404. The cure drops the policy; the
		// test runs it again when it ends, so a failure part way never leaves a policy behind.
		cure := func() {
			if _, err := e.admin.ExecContext(e.ctx, "DROP POLICY IF EXISTS t13_poison ON racks"); err != nil {
				t.Errorf("drop the poison of racks: %v", err)
			}
		}
		t.Cleanup(cure)
		_, err := e.admin.ExecContext(e.ctx, "CREATE POLICY t13_poison ON racks AS RESTRICTIVE USING (CASE WHEN tenant_id = '"+tn+"' THEN 1 / (length(tenant_id) - length(tenant_id)) = 1 ELSE true END)")
		must(t, err)
		// A caller with no grant is refused on the deepest resource first, so no rack is read: 404, not 500.
		none := e.as("u-t13-none", tn)
		for _, w := range []struct{ name, method, path, key, body string }{
			{"read", http.MethodGet, base + "/" + m, "", ""},
			{"list", http.MethodGet, base, "", ""},
			{"update", http.MethodPut, base + "/" + m, "", `{"label":"l","note":"updated"}`},
			{"patch", http.MethodPatch, base + "/" + m, "k-" + uuid.NewString(), `{"note":"patched"}`},
			{"delete", http.MethodDelete, base + "/" + m, "", ""},
			{"create", http.MethodPost, base, "k-" + uuid.NewString(), `{"label":"new","note":"created"}`},
			{"upsert", http.MethodPut, base, "", `{"label":"l","note":"upserted"}`},
		} {
			if got := e.send(none, w.method, w.path, w.key, w.body); got.status != http.StatusNotFound {
				t.Errorf("the %s by a caller with no grant, with the racks poisoned, answered %d %s, want 404 and no read of a rack", w.name, got.status, got.body)
			}
		}
		// A caller that the deepest resource allows, and that may not read the rack, still has the rack read: the
		// check reads it with no decision, so the poison answers 500.
		viewer := e.as("u-t13-viewer", tn)
		e.relate(viewer, "viewer", "mark", m)
		if got := e.send(viewer, http.MethodGet, base+"/"+m, "", ""); got.status != http.StatusInternalServerError {
			t.Errorf("the read by a viewer of the mark, with the racks poisoned, answered %d %s, want 500", got.status, got.body)
		}
		editor := e.as("u-t13-editor", tn)
		e.own(editor, "mark", m)
		if got := e.send(editor, http.MethodPatch, base+"/"+m, "k-"+uuid.NewString(), `{"note":"patched"}`); got.status != http.StatusInternalServerError {
			t.Errorf("the patch by the owner of the mark, with the racks poisoned, answered %d %s, want 500", got.status, got.body)
		}
		cards := e.as("u-t13-card", tn)
		e.relate(cards, "viewer", "card", c)
		if got := e.send(cards, http.MethodGet, base, "", ""); got.status != http.StatusInternalServerError {
			t.Errorf("the list by a viewer of the card, with the racks poisoned, answered %d %s, want 500", got.status, got.body)
		}
		cure()
		if n := e.count(`SELECT count(*) FROM marks WHERE id = $1::uuid AND note = 'n'`, m); n != 1 {
			t.Errorf("the mark changed while the racks were poisoned")
		}
		// After the poison, each of them gets its row with no read of the folder or the rack.
		if got := e.send(viewer, http.MethodGet, base+"/"+m, "", ""); got.status != http.StatusOK {
			t.Errorf("after the poison, the read by a viewer of the mark answered %d %s, want 200", got.status, got.body)
		}
		if got := e.send(editor, http.MethodPatch, base+"/"+m, "k-"+uuid.NewString(), `{"note":"patched"}`); got.status != http.StatusOK {
			t.Errorf("after the poison, the patch by the owner of the mark answered %d %s, want 200", got.status, got.body)
		}
		if got := e.send(cards, http.MethodGet, base, "", ""); got.status != http.StatusOK {
			t.Errorf("after the poison, the list by a viewer of the card answered %d %s, want 200", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RC33-OC09-T02 A search of an entity of small expected cardinality reads only the rows that the caller may see, so hidden rows that match do not fill its pages", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// Event is of small expected cardinality. Red events that the caller may not see match the search, and sort
		// both before and after its own two: 1100 in the default order (created_time descending) and by key before
		// them, and 100 after them. A list that paged the table would read 1000 hidden rows first and give an empty
		// first page with a cursor.
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO events (tenant_id, id, source, note, created_time, updated_time)
SELECT $1, ('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, 'red', 'match', now() + interval '1 day', now() FROM generate_series(1, 1100) g`, a.tenant.ID()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO events (tenant_id, id, source, note, created_time, updated_time)
SELECT $1, ('ffffffff-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, 'red', 'match', now() - interval '1 day', now() FROM generate_series(1, 100) g`, a.tenant.ID()); err != nil {
			t.Fatal(err)
		}
		var page func(text, cursor string) ([]string, string)
		mine := map[string]bool{}
		for range 2 {
			mine[e.create(a, "red-events", "event", `{"note":"match"}`)] = true
		}
		// search gives the IDs of every page of the search, and the number of pages.
		search := func(text string) (ids []string, pages int) {
			cursor := ""
			for pages < 5 {
				got, next := page(text, cursor)
				ids, pages = append(ids, got...), pages+1
				if cursor = next; cursor == "" {
					break
				}
			}
			return ids, pages
		}
		page = func(text, cursor string) (ids []string, next string) {
			q := url.Values{"search": {text}, "pageSize": {"10"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			r := e.send(a, http.MethodGet, "/api/v1/red-events?"+q.Encode(), "", "")
			if r.status != http.StatusOK {
				t.Fatalf("the search %q answered %d %s, want 200", text, r.status, r.body)
			}
			var page struct {
				Items []map[string]any `json:"items"`
				Next  string           `json:"next"`
			}
			must(t, json.Unmarshal(r.body, &page))
			for _, item := range page.Items {
				ids = append(ids, fmt.Sprint(item["id"]))
			}
			return ids, page.Next
		}
		// The first page holds both: no page is filled with hidden rows first. The search may give a cursor of the
		// provider after them, whose page is empty.
		first, _ := page("note = 'match'", "")
		ids, pages := search("note = 'match'")
		if len(first) != 2 || !mine[first[0]] || !mine[first[1]] || len(ids) != 2 || pages > 2 {
			t.Errorf("the search of the caller's two events gave %v on its first page and %v in %d pages, want its two events on the first page", first, ids, pages)
		}
		if ids, pages := search("note = 'none'"); len(ids) != 0 || pages > 2 {
			t.Errorf("a search that no row matches gave %v in %d pages, want none", ids, pages)
		}
	})

	t.Run("ST-SUP-RC20-OC23-T04 Implicit fields are forced and filtered on the runtime data path, under their path prefixes", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// A body may give an implicit field only with the value of its collection (ST-SUP-RC20-OC23, ST-SUP-RC20-OC55-T01).
		red := e.send(a, http.MethodPost, "/api/v1/red-events", "k-"+uuid.NewString(), `{"source":"red","note":"n1"}`)
		blue := e.send(a, http.MethodPost, "/api/v1/blue-events", "k-"+uuid.NewString(), `{"note":"n2"}`)
		if red.status != http.StatusCreated || blue.status != http.StatusCreated {
			t.Fatalf("the creates answered %d %s and %d %s, want 201", red.status, red.body, blue.status, blue.body)
		}
		redID, blueID := red.member(t, "id"), blue.member(t, "id")
		e.own(a, "event", redID)
		e.own(a, "event", blueID)
		if n := e.count(`SELECT count(*) FROM events WHERE (id = $1::uuid AND source = 'red') OR (id = $2::uuid AND source = 'blue')`, redID, blueID); n != 2 {
			t.Errorf("%d event rows with the implicit source of their collection, want 2", n)
		}
		if href := red.member(t, "href"); href != "/api/v1/red-events/"+redID {
			t.Errorf("the href is %q, want the path under the prefix", href)
		}
		for path, want := range map[string]string{"/api/v1/red-events": redID, "/api/v1/blue-events": blueID} {
			r := e.send(a, http.MethodGet, path, "", "")
			var page struct {
				Items []map[string]any `json:"items"`
			}
			must(t, json.Unmarshal(r.body, &page))
			if r.status != http.StatusOK || len(page.Items) != 1 || page.Items[0]["id"] != want {
				t.Errorf("the list of %s answered %d %s, want only its own event", path, r.status, r.body)
			}
		}
		if r := e.send(a, http.MethodGet, "/api/v1/red-events/"+blueID, "", ""); r.status != http.StatusNotFound {
			t.Errorf("the read of a blue event as red answered %d %s, want 404", r.status, r.body)
		}
		if r := e.send(a, http.MethodGet, "/api/v1/red-events/"+redID, "", ""); r.status != http.StatusOK {
			t.Errorf("the read of a red event answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC23-T07 A read of a row whose optional implicit field is NULL is not found", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		created := e.send(a, http.MethodPost, "/api/v1/green-labels", "k-"+uuid.NewString(), `{"name":"l1"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "label", id)
		if r := e.send(a, http.MethodGet, "/api/v1/green-labels/"+id, "", ""); r.status != http.StatusOK || r.member(t, "source") != "green" {
			t.Fatalf("the read of a green label answered %d %s, want 200 with source green", r.status, r.body)
		}
		// The view holds the optional implicit field as a pointer, nil for NULL: a NULL is not the value of the
		// collection, so the row is not found, and its fields are not answered (ST-SUP-Q128 (B)).
		if _, err := e.admin.ExecContext(e.ctx, `UPDATE labels SET source = NULL WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		if r := e.send(a, http.MethodGet, "/api/v1/green-labels/"+id, "", ""); r.status != http.StatusNotFound || strings.Contains(string(r.body), "l1") {
			t.Errorf("the read of a label with a NULL source answered %d %s, want 404", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC37-T09 A list search on the runtime data path filters on declared fields with bound values", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		for _, title := range []string{"x", "y", "x", "it's"} {
			e.create(a, "todos", "todo", `{"title":"`+title+`"}`)
		}
		search := func(text, cursor string) reply {
			q := url.Values{"search": {text}, "pageSize": {"1"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			return e.send(a, http.MethodGet, "/api/v1/todos?"+q.Encode(), "", "")
		}
		var titles []string
		cursor := ""
		for range 10 {
			r := search("title = 'x'", cursor)
			if r.status != http.StatusOK {
				t.Fatalf("the search answered %d %s, want 200", r.status, r.body)
			}
			var page struct {
				Items []map[string]any `json:"items"`
				Next  string           `json:"next"`
			}
			must(t, json.Unmarshal(r.body, &page))
			for _, item := range page.Items {
				titles = append(titles, fmt.Sprint(item["title"]))
			}
			if cursor = page.Next; cursor == "" {
				break
			}
		}
		if strings.Join(titles, ",") != "x,x" {
			t.Errorf("the search of title x gave %v, want two todos x", titles)
		}
		if r := search("title = 'it''s'", ""); r.status != http.StatusOK || !strings.Contains(string(r.body), `it's`) {
			t.Errorf("the search with a quote answered %d %s, want the todo it's", r.status, r.body)
		}
		// A cursor of one search is refused for another.
		first := search("title = 'x'", "")
		next := first.member(t, "next")
		if r := search("title = 'y'", next); r.status != http.StatusBadRequest {
			t.Errorf("the cursor of another search answered %d %s, want 400", r.status, r.body)
		}
		// A search of a secret field, of a form other than equality or with SQL text is refused, and no row leaves.
		for _, text := range []string{"api_token = 'x'", "title = 'x' or done = true", "title = x' OR '1'='1", "title != 'x'"} {
			path := "/api/v1/todos?" + url.Values{"search": {text}}.Encode()
			if text == "api_token = 'x'" {
				path = "/api/v1/customers?" + url.Values{"search": {text}}.Encode()
			}
			if r := e.send(a, http.MethodGet, path, "", ""); r.status != http.StatusBadRequest || strings.Contains(string(r.body), "items") {
				t.Errorf("the search %q answered %d %s, want 400 with no items", text, r.status, r.body)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC13-T03 An upsert on the runtime data path creates the row of its key with 201 and updates it with 200", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a, other := e.as("u-1", tn), e.as("u-2", tn)
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		foreign := e.create(other, "folders", "folder", `{"name":"not mine"}`)
		settings := "/api/v1/folders/" + folder + "/settings"
		first := e.send(a, http.MethodPut, settings, "", `{"name":"color","value":"red"}`)
		if first.status != http.StatusCreated {
			t.Fatalf("the first upsert answered %d %s, want 201", first.status, first.body)
		}
		id := first.member(t, "id")
		// The relay of the check does not run, so the caller owns the row: its relation to the folder is in the
		// outbox.
		e.own(a, "setting", id)
		// An upsert with another folder in the body is refused and changes nothing.
		if r := e.send(a, http.MethodPut, settings, "", `{"name":"color","value":"blue","folder_id":"`+foreign+`"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"folder_id"`) {
			t.Errorf("an upsert with another folder in the body answered %d %s, want 400 that names folder_id", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE id = $1::uuid AND value = 'red'`, id); n != 1 {
			t.Errorf("%d setting rows as created after the refused upsert, want 1", n)
		}
		second := e.send(a, http.MethodPut, settings, "", `{"name":"color","value":"blue","folder_id":"`+folder+`"}`)
		if second.status != http.StatusOK || second.member(t, "id") != id || second.member(t, "value") != "blue" {
			t.Fatalf("the second upsert answered %d %s, want 200 with the same row and the new value", second.status, second.body)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE folder_id = $1::uuid AND name = 'color'`, folder); n != 1 {
			t.Errorf("%d setting rows of the key, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE id = $1::uuid AND value = 'blue'`, id); n != 1 {
			t.Errorf("%d setting rows with the updated value, want 1", n)
		}
		third := e.send(a, http.MethodPut, settings, "", `{"name":"size","value":"large"}`)
		if third.status != http.StatusCreated || third.member(t, "id") == id {
			t.Errorf("the upsert of another key answered %d %s, want 201 with a new row", third.status, third.body)
		}
		e.own(a, "setting", third.member(t, "id"))
		// A caller who may not edit the folder upserts nothing, under that folder or under the caller's own key.
		for name, r := range map[string]reply{
			"another caller":   e.send(other, http.MethodPut, settings, "", `{"name":"color","value":"green"}`),
			"a foreign folder": e.send(a, http.MethodPut, "/api/v1/folders/"+foreign+"/settings", "", `{"name":"color","value":"green"}`),
		} {
			if r.status != http.StatusNotFound {
				t.Errorf("the upsert of %s answered %d %s, want 404", name, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE value = 'green'`); n != 0 {
			t.Errorf("%d setting rows of the refused upserts, want 0", n)
		}
		list := e.send(a, http.MethodGet, settings, "", "")
		if list.status != http.StatusOK || !strings.Contains(string(list.body), `"blue"`) || !strings.Contains(string(list.body), `"large"`) {
			t.Errorf("the list of the folder's settings answered %d %s, want both settings", list.status, list.body)
		}
	})

	t.Run("ST-SUP-RC20-OC14-T01 An upsert with optimistic concurrency updates only under the If-Match of the stored generation", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a := e.as("u-1", tn)
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		quotas := "/api/v1/folders/" + folder + "/quotas"
		put := func(body string, ifMatch string) reply {
			t.Helper()
			headers := map[string]string{}
			if ifMatch != "" {
				headers["If-Match"] = ifMatch
			}
			return e.sendWith(a, http.MethodPut, quotas, "", body, headers)
		}
		amount := func(id string) int {
			t.Helper()
			return e.count(`SELECT amount FROM quotas WHERE id = $1::uuid`, id)
		}
		// An If-Match on a key with no stored row names a generation that does not exist.
		if r := put(`{"name":"disk","amount":10}`, `"1"`); r.status != http.StatusPreconditionFailed {
			t.Fatalf("an upsert of a new key with If-Match answered %d %s, want 412", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM quotas WHERE name = 'disk'`); n != 0 {
			t.Fatalf("the refused insert left %d rows", n)
		}
		created := put(`{"name":"disk","amount":10}`, "")
		if created.status != http.StatusCreated {
			t.Fatalf("the insert answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "quota", id)
		if g, _ := created.object(t)["generation"].(float64); g != 1 {
			t.Fatalf("the inserted row has the generation %v, want 1", created.object(t)["generation"])
		}
		for name, c := range map[string]struct {
			ifMatch string
			status  int
		}{
			"no If-Match":             {"", http.StatusPreconditionRequired},
			"a stale generation":      {`"7"`, http.StatusPreconditionFailed},
			"a weak tag":              {`W/"1"`, http.StatusPreconditionFailed},
			"any representation":      {`*`, http.StatusPreconditionFailed},
			"a tag that is no number": {`"one"`, http.StatusPreconditionFailed},
		} {
			if r := put(`{"name":"disk","amount":99}`, c.ifMatch); r.status != c.status {
				t.Errorf("an update with %s answered %d %s, want %d", name, r.status, r.body, c.status)
			}
		}
		if got := amount(id); got != 10 {
			t.Fatalf("a refused update changed the amount to %d", got)
		}
		updated := put(`{"name":"disk","amount":20,"generation":50}`, `"1"`)
		if updated.status != http.StatusOK || updated.member(t, "id") != id {
			t.Fatalf("the update with the stored generation answered %d %s, want 200 with the same row", updated.status, updated.body)
		}
		if g, _ := updated.object(t)["generation"].(float64); g != 2 || amount(id) != 20 {
			t.Fatalf("the update has the generation %v and the amount %d, want 2 and 20", updated.object(t)["generation"], amount(id))
		}
		// A caller of the tenant who may not edit the folder gets 404 for a key with no stored row and for a stored
		// key alike, with If-Match and without, so the precondition tells it nothing about which rows exist. The
		// viewer may read the folder and its quotas, and the stranger has no relation at all.
		viewer, stranger := e.as("u-2", tn), e.as("u-3", tn)
		e.relate(viewer, "viewer", "folder", folder)
		for who, c := range map[string]caller{"the viewer": viewer, "the stranger": stranger} {
			for name, r := range map[string]struct{ body, ifMatch string }{
				"a missing key with If-Match":   {`{"name":"cpu","amount":1}`, `"1"`},
				"a stored key with If-Match":    {`{"name":"disk","amount":1}`, `"2"`},
				"a stored key with no If-Match": {`{"name":"disk","amount":1}`, ""},
			} {
				headers := map[string]string{}
				if r.ifMatch != "" {
					headers["If-Match"] = r.ifMatch
				}
				if got := e.sendWith(c, http.MethodPut, quotas, "", r.body, headers); got.status != http.StatusNotFound {
					t.Errorf("the upsert of %s by %s answered %d %s, want 404", name, who, got.status, got.body)
				}
			}
		}
		if n := e.count(`SELECT count(*) FROM quotas WHERE name = 'cpu'`); n != 0 || amount(id) != 20 {
			t.Fatalf("the refused upserts of the other caller left %d cpu rows and the amount %d", n, amount(id))
		}
		// The generation that the client read before the update is stale now.
		if r := put(`{"name":"disk","amount":30}`, `"1"`); r.status != http.StatusPreconditionFailed || amount(id) != 20 {
			t.Errorf("a second update with the old generation answered %d %s and left the amount %d, want 412 and 20", r.status, r.body, amount(id))
		}
		if r := put(`{"name":"disk","amount":30}`, `"2"`); r.status != http.StatusOK || amount(id) != 30 {
			t.Errorf("the update with the new generation answered %d %s and left the amount %d, want 200 and 30", r.status, r.body, amount(id))
		}
	})

	t.Run("ST-SUP-RC20-OC13-T04 An upsert finds its row only under its own parent and implicit value", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		one := e.create(a, "folders", "folder", `{"name":"one"}`)
		two := e.create(a, "folders", "folder", `{"name":"two"}`)
		put := func(path, body string, want int) string {
			t.Helper()
			r := e.send(a, http.MethodPut, "/api/v1/folders/"+path, "", body)
			if r.status != want {
				t.Fatalf("PUT %s %s answered %d %s, want %d", path, body, r.status, r.body, want)
			}
			id := r.member(t, "id")
			e.own(a, "setting", id)
			return id
		}
		first := put(one+"/settings", `{"name":"k","value":"v1"}`, http.StatusCreated)
		// The same key under another folder is another row, and the row of the first folder stays as it is.
		other := put(two+"/settings", `{"name":"k","value":"v2"}`, http.StatusCreated)
		// The same key under the same folder with another implicit value is another row. A body that gives the
		// implicit value of another collection is refused (ST-SUP-RC20-OC23).
		if r := e.send(a, http.MethodPut, "/api/v1/folders/"+one+"/flags", "", `{"name":"k","value":"v3","source":"setting"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"source"`) {
			t.Errorf("an upsert of a flag with the source setting answered %d %s, want 400 that names source", r.status, r.body)
		}
		flag := put(one+"/flags", `{"name":"k","value":"v3","source":"flag"}`, http.StatusCreated)
		if other == first || flag == first || flag == other {
			t.Errorf("the upserts gave the rows %s, %s and %s, want three rows", first, other, flag)
		}
		for _, c := range []struct{ id, folder, source, value string }{{first, one, "setting", "v1"}, {other, two, "setting", "v2"}, {flag, one, "flag", "v3"}} {
			if n := e.count(`SELECT count(*) FROM settings WHERE id = $1::uuid AND folder_id = $2::uuid AND source = $3 AND value = $4`, c.id, c.folder, c.source, c.value); n != 1 {
				t.Errorf("%d rows %s with the folder %s, the source %s and the value %s, want 1", n, c.id, c.folder, c.source, c.value)
			}
		}
		if again := put(one+"/settings", `{"name":"k","value":"v4"}`, http.StatusOK); again != first {
			t.Errorf("the upsert of the key under its own folder answered the row %s, want %s", again, first)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE id = $1::uuid AND value = 'v4'`, first); n != 1 {
			t.Errorf("%d rows of the first folder with the new value, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC13-T05 An upsert calls its validate gate before both branches and on_entity_changed after both", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		settings := "/api/v1/folders/" + folder + "/settings"
		e.mu.Lock()
		e.settingGate = func(req *slots.ValidateRequest) *slots.SlotResult {
			if req.Input.Fields["value"] == "refused" {
				return &slots.SlotResult{Ok: false, StatusCode: http.StatusForbidden, ErrorMessage: "the value is refused"}
			}
			return nil
		}
		e.mu.Unlock()
		defer func() { e.mu.Lock(); e.settingGate = nil; e.mu.Unlock() }()
		if r := e.send(a, http.MethodPut, settings, "", `{"name":"g","value":"refused"}`); r.status != http.StatusForbidden {
			t.Errorf("the refused insert answered %d %s, want 403", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE folder_id = $1::uuid`, folder); n != 0 {
			t.Errorf("%d rows after the refused insert, want 0", n)
		}
		created := e.send(a, http.MethodPut, settings, "", `{"name":"g","value":"ok"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the insert answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "setting", id)
		if r := e.send(a, http.MethodPut, settings, "", `{"name":"g","value":"refused"}`); r.status != http.StatusForbidden {
			t.Errorf("the refused update answered %d %s, want 403", r.status, r.body)
		}
		if r := e.send(a, http.MethodPut, settings, "", `{"name":"g","value":"ok2"}`); r.status != http.StatusOK {
			t.Errorf("the update answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM settings WHERE id = $1::uuid AND value = 'ok2'`, id); n != 1 {
			t.Errorf("%d rows with the value of the accepted update, want 1", n)
		}
		// One on_entity_changed for the insert and one for the update, none for a refused upsert. They are staged in
		// the transaction of the write; the outbox worker runs them after the relations of the row, which the relay
		// writes, and the relay of the check does not run (ST-SUP-RC20-OC34 proves the run).
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = $2 AND kind = 'folder-settings.on_entity_changed' AND resource_key = 'setting:' || $3`, a.tenant.ID(), api.AfterSlotsDestination, id); n != 2 {
			t.Errorf("%d staged on_entity_changed messages of the row, want 2", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND kind = 'folder-settings.on_entity_changed'`, a.tenant.ID()); n != 2 {
			t.Errorf("%d staged on_entity_changed messages of the tenant, want 2", n)
		}
	})

	t.Run("ST-SUP-RC20-OC23-T05 A write of a row of another implicit value is not found and changes nothing, and a write that gives the implicit field another value is refused", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		red := e.send(a, http.MethodPost, "/api/v1/red-events", "k-"+uuid.NewString(), `{"note":"r"}`)
		blue := e.send(a, http.MethodPost, "/api/v1/blue-events", "k-"+uuid.NewString(), `{"note":"b"}`)
		if red.status != http.StatusCreated || blue.status != http.StatusCreated {
			t.Fatalf("the creates answered %d and %d, want 201", red.status, blue.status)
		}
		redID, blueID := red.member(t, "id"), blue.member(t, "id")
		e.own(a, "event", redID)
		e.own(a, "event", blueID)
		for _, c := range []struct{ method, key, body string }{
			{http.MethodPut, "", `{"note":"moved"}`},
			{http.MethodPatch, "k-" + uuid.NewString(), `{"note":"moved"}`},
			{http.MethodDelete, "", ""},
		} {
			if r := e.send(a, c.method, "/api/v1/red-events/"+blueID, c.key, c.body); r.status != http.StatusNotFound {
				t.Errorf("%s of a blue event as red answered %d %s, want 404", c.method, r.status, r.body)
			}
		}
		if n := e.count(`SELECT count(*) FROM events WHERE id = $1::uuid AND source = 'blue' AND note = 'b'`, blueID); n != 1 {
			t.Errorf("%d blue event rows as created, want 1", n)
		}
		// A patch or an update that gives the implicit field another value is refused, and the row stays red
		// (ST-SUP-RC20-OC15, ST-SUP-RC20-OC23).
		for _, method := range []string{http.MethodPatch, http.MethodPut} {
			if r := e.send(a, method, "/api/v1/red-events/"+redID, "k-"+uuid.NewString(), `{"note":"n2","source":"green"}`); r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"source"`) {
				t.Errorf("%s of a red event with the source green answered %d %s, want 400 that names source", method, r.status, r.body)
			}
		}
		if r := e.send(a, http.MethodPut, "/api/v1/red-events/"+redID, "", `{"note":"n3","source":"red"}`); r.status != http.StatusOK {
			t.Errorf("the update of a red event answered %d %s, want 200", r.status, r.body)
		}
		if n := e.count(`SELECT count(*) FROM events WHERE id = $1::uuid AND source = 'red' AND note = 'n3'`, redID); n != 1 {
			t.Errorf("%d red event rows with the updated note and the implicit source, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC24-T06 A list cursor of one parent is refused for another", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		one := e.create(a, "folders", "folder", `{"name":"one"}`)
		two := e.create(a, "folders", "folder", `{"name":"two"}`)
		for range 3 {
			e.create(a, "folders/"+one+"/books", "book", `{"title":"t"}`)
		}
		first := e.send(a, http.MethodGet, "/api/v1/folders/"+one+"/books?pageSize=1", "", "")
		next := first.member(t, "next")
		if first.status != http.StatusOK || next == "" {
			t.Fatalf("the first page answered %d %s, want 200 with a cursor", first.status, first.body)
		}
		if r := e.send(a, http.MethodGet, "/api/v1/folders/"+two+"/books?pageSize=1&cursor="+url.QueryEscape(next), "", ""); r.status != http.StatusBadRequest {
			t.Errorf("the cursor of folder one on folder two answered %d %s, want 400", r.status, r.body)
		}
		if r := e.send(a, http.MethodGet, "/api/v1/folders/"+one+"/books?pageSize=1&cursor="+url.QueryEscape(next), "", ""); r.status != http.StatusOK {
			t.Errorf("the cursor on its own folder answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC24-T07 An upper-case UUID in a path or a ref names the same row as its lower-case form", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		one := e.create(a, "folders", "folder", `{"name":"one"}`)
		two := e.create(a, "folders", "folder", `{"name":"two"}`)
		upper := strings.ToUpper
		books := func(folder string) string { return "/api/v1/folders/" + folder + "/books" }
		// The parent of the path in upper case.
		created := e.send(a, http.MethodPost, books(upper(one)), "k-"+uuid.NewString(), `{"title":"t1"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create under the upper-case folder answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "book", id)
		if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id::text = $2`, id, one); n != 1 {
			t.Errorf("%d book rows on the lower-case folder, want 1", n)
		}
		for _, c := range []struct {
			method, path, key, body string
			want                    int
		}{
			{http.MethodGet, books(upper(one)) + "/" + upper(id), "", "", http.StatusOK},
			{http.MethodGet, books(upper(one)), "", "", http.StatusOK},
			{http.MethodPatch, books(upper(one)) + "/" + upper(id), "k-" + uuid.NewString(), `{"title":"t2"}`, http.StatusOK},
			{http.MethodPut, books(upper(one)) + "/" + id, "", `{"title":"t3"}`, http.StatusOK},
			{http.MethodGet, books(upper(two)) + "/" + upper(id), "", "", http.StatusNotFound},
		} {
			if r := e.send(a, c.method, c.path, c.key, c.body); r.status != c.want {
				t.Errorf("%s %s answered %d %s, want %d", c.method, c.path, r.status, r.body, c.want)
			}
		}
		list := e.send(a, http.MethodGet, books(upper(one)), "", "")
		var page struct {
			Items []map[string]any `json:"items"`
		}
		must(t, json.Unmarshal(list.body, &page))
		if len(page.Items) != 1 || page.Items[0]["id"] != id {
			t.Errorf("the list under the upper-case folder answered %s, want its one book", list.body)
		}
		// A ref of the body in upper case: a create, an update that keeps the ref and an update that moves it.
		free := e.send(a, http.MethodPost, "/api/v1/books", "k-"+uuid.NewString(), `{"title":"f1","folder_id":"`+upper(one)+`"}`)
		if free.status != http.StatusCreated {
			t.Fatalf("the create with an upper-case ref answered %d %s, want 201", free.status, free.body)
		}
		freeID := free.member(t, "id")
		e.own(a, "book", freeID)
		for _, folder := range []string{one, two} {
			if r := e.send(a, http.MethodPut, "/api/v1/books/"+upper(freeID), "", `{"title":"f2","folder_id":"`+upper(folder)+`"}`); r.status != http.StatusOK {
				t.Errorf("the update with the upper-case ref %s answered %d %s, want 200", folder, r.status, r.body)
			}
			if n := e.count(`SELECT count(*) FROM books WHERE id = $1::uuid AND folder_id::text = $2`, freeID, folder); n != 1 {
				t.Errorf("%d book rows on the lower-case folder %s, want 1", n, folder)
			}
		}
		if r := e.send(a, http.MethodGet, "/api/v1/books/"+upper(freeID), "", ""); r.status != http.StatusOK {
			t.Errorf("the read of the upper-case ID answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RP11-LC20-T04 A search of the generated service uses the index of its field, and a sensitive field neither searches nor orders", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO todos (tenant_id, id, title, seen_at) SELECT $1, gen_random_uuid(), 't-' || g, now() FROM generate_series(1, 20000) g`, a.tenant.ID()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.admin.ExecContext(e.ctx, `ANALYZE todos`); err != nil {
			t.Fatal(err)
		}
		// The statement of the first page of a filtered list, as the query runtime writes it.
		rows, err := e.admin.QueryContext(e.ctx, `EXPLAIN SELECT id FROM public.todos WHERE tenant_id = $1 AND title = $2 ORDER BY id LIMIT 101`, a.tenant.ID(), "t-7")
		must(t, err)
		var plan strings.Builder
		for rows.Next() {
			var line string
			must(t, rows.Scan(&line))
			plan.WriteString(line + "\n")
		}
		must(t, rows.Err())
		rows.Close()
		if !strings.Contains(plan.String(), "todos_title_search") {
			t.Errorf("the plan of a search of title uses no index of title:\n%s", plan.String())
		}
		// The rows have no owner, so the caller sees none of them; the search is served.
		if r := e.send(a, http.MethodGet, "/api/v1/todos?search="+url.QueryEscape("title = 't-7'"), "", ""); r.status != http.StatusOK {
			t.Errorf("the search answered %d %.200s, want 200", r.status, r.body)
		}
		// A pii field neither searches nor orders a list; a bad value is 400 and not 500.
		for _, q := range []string{"search=" + url.QueryEscape("name = 'Ada'"), "orderBy=name", "orderBy=api_token"} {
			if r := e.send(a, http.MethodGet, "/api/v1/customers?"+q, "", ""); r.status != http.StatusBadRequest {
				t.Errorf("the list of customers with %s answered %d %s, want 400", q, r.status, r.body)
			}
		}
		for _, q := range []string{"title = 'a\x00b'"} {
			if r := e.send(a, http.MethodGet, "/api/v1/todos?search="+url.QueryEscape(q), "", ""); r.status != http.StatusBadRequest {
				t.Errorf("the search %q answered %d %s, want 400", q, r.status, r.body)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC57-T01 A create stores a long text value of 4000 characters and of 1000 four-byte characters", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// Random text, so the compression of an index entry cannot make it small: 4000 letters and digits in body
		// (4000 bytes), and 1000 characters of four bytes each in summary (4000 bytes). A b-tree entry holds at most
		// about 2700 bytes (erp-demo G10).
		body, summary := randomText(4000, false), randomText(1000, true)
		document, err := json.Marshal(map[string]string{"title": "long", "body": body, "summary": summary})
		must(t, err)
		id := e.create(a, "memos", "memo", string(document))
		var storedBody, storedSummary string
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT body, summary FROM memos WHERE id = $1::uuid`, id).Scan(&storedBody, &storedSummary))
		if storedBody != body || storedSummary != summary || len(storedSummary) != 4000 {
			t.Errorf("the memo stores a body of %d bytes and a summary of %d bytes, want the values of the create", len(storedBody), len(storedSummary))
		}
		if r := e.send(a, http.MethodGet, "/api/v1/memos/"+id, "", ""); r.status != http.StatusOK || r.member(t, "body") != body || r.member(t, "summary") != summary {
			t.Errorf("the read of the memo answered %d, want 200 with the long values", r.status)
		}
	})

	t.Run("ST-SUP-RC20-OC57-T02 A search or an order of a field out of the search answers 400 that names the field, and the table has no search index of it", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		e.create(a, "memos", "memo", `{"title":"first","body":"b","summary":"s","tag":"t"}`)
		for _, c := range []struct{ query, field string }{
			{"search=" + url.QueryEscape("body = 'b'"), "body"},
			{"search=" + url.QueryEscape("title = 'first' and summary = 's'"), "summary"},
			{"search=" + url.QueryEscape("tag = 't'"), "tag"},
			{"orderBy=body", "body"},
			{"orderBy=" + url.QueryEscape("title asc, tag desc"), "tag"},
		} {
			r := e.send(a, http.MethodGet, "/api/v1/memos?"+c.query, "", "")
			if r.status != http.StatusBadRequest || !strings.Contains(r.member(t, "detail"), `"`+c.field+`"`) || !strings.Contains(r.member(t, "detail"), "searchable") {
				t.Errorf("the list of memos with %s answered %d %s, want 400 with a detail that names %s", c.query, r.status, r.body, c.field)
			}
		}
		// A search or an order of the pii field aside, out of the search, answers the 400 that names no field, as for a
		// field that the entity does not declare: the answer does not tell that a pii field is there (security review
		// of PR #22, MK).
		for _, q := range []string{"search=" + url.QueryEscape("aside = 'x'"), "orderBy=aside"} {
			if r := e.send(a, http.MethodGet, "/api/v1/memos?"+q, "", ""); r.status != http.StatusBadRequest || strings.Contains(string(r.body), "aside") || strings.Contains(string(r.body), "searchable") {
				t.Errorf("the list of memos with %s answered %d %s, want 400 that names no field", q, r.status, r.body)
			}
		}
		// A search of a field that the entity does not declare, and one that is not well formed, stay the 400 that
		// names no field; title is searched and orders the list.
		for _, q := range []string{"search=" + url.QueryEscape("colour = 'red'"), "search=" + url.QueryEscape("body = b")} {
			if r := e.send(a, http.MethodGet, "/api/v1/memos?"+q, "", ""); r.status != http.StatusBadRequest || r.member(t, "detail") != "the search is not valid" {
				t.Errorf("the list of memos with %s answered %d %s, want 400 the search is not valid", q, r.status, r.body)
			}
		}
		for _, q := range []string{"search=" + url.QueryEscape("title = 'first'"), "orderBy=title"} {
			if r := e.send(a, http.MethodGet, "/api/v1/memos?"+q, "", ""); r.status != http.StatusOK {
				t.Errorf("the list of memos with %s answered %d %s, want 200", q, r.status, r.body)
			}
		}
		rows, err := e.admin.QueryContext(e.ctx, `SELECT indexname FROM pg_catalog.pg_indexes WHERE tablename = 'memos' AND indexname LIKE '%\_search' ORDER BY indexname`)
		must(t, err)
		var indexes []string
		for rows.Next() {
			var name string
			must(t, rows.Scan(&name))
			indexes = append(indexes, name)
		}
		must(t, rows.Err())
		rows.Close()
		if !slices.Equal(indexes, []string{"memos_title_search"}) {
			t.Errorf("the table memos has the search indexes %v, want only memos_title_search", indexes)
		}
	})

	t.Run("ST-SUP-RC20-OC48-T05 An update that leaves out a required secret, or gives it empty, answers 200 and keeps the stored value", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		token := "secret-" + uuid.NewString()
		id := e.create(a, "customers", "customer", `{"name":"Kai","api_token":"`+token+`","plan":"free"}`)
		path := "/api/v1/customers/" + id
		// No response gives the secret back, so a client sends back a row that it read with no secret, or with the
		// secret empty: each update is written and keeps the stored secret (walkthrough 14, W14-B4).
		for _, body := range []string{`{"name":"Kai","plan":"pro"}`, `{"name":"Kai","api_token":"","plan":"team"}`} {
			if r := e.send(a, http.MethodPut, path, "", body); r.status != http.StatusOK {
				t.Fatalf("the update %s answered %d %s, want 200", body, r.status, r.body)
			}
			var stored []byte
			var plan string
			must(t, e.admin.QueryRowContext(e.ctx, `SELECT api_token, plan FROM customers WHERE id = $1::uuid`, id).Scan(&stored, &plan))
			plain, err := e.ring.Open(stored, sealed.Place{Tenant: a.tenant.ID(), Table: "customers", Column: "api_token", Row: id})
			if err != nil || string(plain) != token {
				t.Errorf("after the update %s the stored secret does not open to the value of the create (%v)", body, err)
			}
			if !strings.Contains(body, plan) {
				t.Errorf("the update %s stored the plan %q", body, plan)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC01-T03 A secret field reaches neither before, after nor the stored row", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		first, second := "secret-"+uuid.NewString(), "secret-"+uuid.NewString()
		id := e.create(a, "customers", "customer", `{"name":"Dee","api_token":"`+first+`","plan":"free"}`)
		r := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Dee","api_token":"`+second+`","plan":"pro"}`)
		if r.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", r.status, r.body)
		}
		if strings.Contains(string(r.body), second) {
			t.Errorf("the response holds the secret value: %s", r.body)
		}
		rows, err := e.admin.QueryContext(e.ctx, `SELECT row_to_json(a)::text, coalesce(before::text, ''), coalesce(after::text, ''), coalesce(patch::text, '') FROM audit_event a WHERE tenant_id = $1`, a.tenant.ID())
		must(t, err)
		defer rows.Close()
		n := 0
		for rows.Next() {
			var row, before, after, patch string
			must(t, rows.Scan(&row, &before, &after, &patch))
			n++
			for _, value := range []string{first, second} {
				if strings.Contains(row, value) {
					t.Errorf("an audit row holds the secret value: %s", row)
				}
			}
			for name, state := range map[string]string{"before": before, "after": after, "patch": patch} {
				if strings.Contains(state, "api_token") {
					t.Errorf("the %s of an audit row holds the key api_token: %s", name, state)
				}
			}
		}
		must(t, rows.Err())
		if n < 2 {
			t.Errorf("%d audit rows of the tenant, want the create and the update rows", n)
		}
		// The source row holds the secret sealed, never in clear (decision ST-DECL-Q44).
		var stored []byte
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT api_token FROM customers WHERE id = $1::uuid`, id).Scan(&stored))
		if len(stored) == 0 || stored[0] != 1 || strings.Contains(string(stored), first) || strings.Contains(string(stored), second) {
			t.Errorf("the source row does not hold the secret sealed, or holds it in clear")
		}
	})

	t.Run("ST-SUP-RC20-OC44-T01 An update refuses a changed create or never field, names each, changes nothing, and keeps a field that the body leaves out or repeats", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a, other := e.as("u-1", tn), e.as("u-2", tn)
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		id := e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"c1","title":"first","sku":"s1","meta":{"a":1},"due_at":"2026-12-01T10:00:00+02:00"}`)
		// A command or a worker changes the never fields: the check writes them as the administrator.
		if _, err := e.admin.ExecContext(e.ctx, `UPDATE tickets SET state = 'closed', rank = 3 WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		kept := func(what string, r reply, title string) {
			t.Helper()
			object := r.object(t)
			if r.status != http.StatusOK || object["title"] != title || object["sku"] != "s1" || object["state"] != "closed" || object["rank"] != float64(3) ||
				object["due_at"] != "2026-12-01T08:00:00Z" || fmt.Sprint(object["meta"]) != "map[a:1]" {
				t.Errorf("%s answered %d %s, want 200 with the title %s and the stored sku, state, rank, meta and due_at", what, r.status, r.body, title)
			}
			if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = $2 AND sku = 's1' AND state = 'closed' AND rank = 3 AND meta = '{"a":1}'::jsonb AND due_at = '2026-12-01T08:00:00Z'`, id, title); n != 1 {
				t.Errorf("%s: %d ticket rows with the title %s and the stored values, want 1", what, n, title)
			}
		}
		path := "/api/v1/tickets/" + id
		kept("an update that leaves out each kept field", e.send(a, http.MethodPut, path, "", `{"folder_id":"`+folder+`","code":"c1","title":"second"}`), "second")
		// A client sends back the row it read, with each member as the read gives it.
		read := e.send(a, http.MethodGet, path, "", "")
		if read.status != http.StatusOK {
			t.Fatalf("the read answered %d %s, want 200", read.status, read.body)
		}
		row := read.object(t)
		row["title"] = "third"
		body, err := json.Marshal(row)
		must(t, err)
		kept("an update that sends back the read row", e.send(a, http.MethodPut, path, "", string(body)), "third")
		kept("an update that repeats each stored value in another form", e.send(a, http.MethodPut, path, "",
			`{"folder_id":"`+folder+`","code":"c1","title":"fourth","sku":"s1","state":"closed","rank":3,"meta":{ "a" : 1.0 },"due_at":"2026-12-01T03:00:00-05:00"}`), "fourth")
		refused := func(what, body string, fields ...string) {
			t.Helper()
			r := e.send(a, http.MethodPut, path, "", body)
			var problem struct {
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"validation_errors"`
			}
			if r.status != http.StatusBadRequest || json.Unmarshal(r.body, &problem) != nil || len(problem.Errors) != len(fields) {
				t.Errorf("%s answered %d %s, want 400 that names %v", what, r.status, r.body, fields)
				return
			}
			for i, field := range fields {
				if problem.Errors[i].Field != field || !strings.Contains(problem.Errors[i].Message, "cannot change") {
					t.Errorf("%s: entry %d is %+v, want the field %s that a collection write cannot change", what, i, problem.Errors[i], field)
				}
			}
			if strings.Contains(string(r.body), "s2") || strings.Contains(string(r.body), "secret") {
				t.Errorf("%s: the problem quotes a value of the request: %s", what, r.body)
			}
		}
		refused("an update that changes each kept field", `{"folder_id":"`+folder+`","code":"c1","title":"changed","sku":"s2","state":"open","rank":4,"meta":{"a":"secret"},"due_at":"2026-12-02T08:00:00Z"}`,
			"sku", "state", "meta", "due_at", "rank")
		refused("an update that clears rank", `{"folder_id":"`+folder+`","code":"c1","title":"changed","rank":null}`, "rank")
		refused("an update that gives sku as null", `{"folder_id":"`+folder+`","code":"c1","title":"changed","sku":null}`, "sku")
		kept("the row after the refused updates", e.send(a, http.MethodGet, path, "", ""), "fourth")
		// A caller who may not edit the row gets 404, as for any update, and not the 400 of a kept field.
		if r := e.send(other, http.MethodPut, path, "", `{"folder_id":"`+folder+`","code":"c1","title":"changed","sku":"s2"}`); r.status != http.StatusNotFound {
			t.Errorf("an update of a caller who may not edit the row answered %d %s, want 404", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC44-T03 A kept jsonb field compares its numbers exactly, so a value that differs only above 2^53 is refused", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		id := e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"c1","title":"first","sku":"s1","meta":{"n":9007199254740993,"list":[1,2.5]}}`)
		path := "/api/v1/tickets/" + id
		stored := func(what string) {
			t.Helper()
			if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND meta = '{"n":9007199254740993,"list":[1,2.5]}'::jsonb`, id); n != 1 {
				t.Errorf("%s: %d ticket rows that keep the created meta, want 1", what, n)
			}
		}
		// A float64 holds 9007199254740992 and 9007199254740993 as one value; the check must not.
		for _, c := range []struct{ method, meta string }{
			{http.MethodPut, `{"n":9007199254740992,"list":[1,2.5]}`},
			{http.MethodPut, `{"n":9007199254740994,"list":[1,2.5]}`},
			{http.MethodPut, `{"n":9007199254740993,"list":[1,2.500000000000000001]}`},
			{http.MethodPut, `{"n":"9007199254740993","list":[1,2.5]}`},
			{http.MethodPatch, `{"n":9007199254740992,"list":[1,2.5]}`},
		} {
			body := `{"folder_id":"` + folder + `","code":"c1","title":"changed","sku":"s1","meta":` + c.meta + `}`
			key := ""
			if c.method == http.MethodPatch {
				body, key = `{"title":"changed","meta":`+c.meta+`}`, "k-"+uuid.NewString()
			}
			r := e.send(a, c.method, path, key, body)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"meta"`) {
				t.Errorf("a %s with meta %s answered %d %s, want 400 that names meta", c.method, c.meta, r.status, r.body)
			}
			stored("after a " + c.method + " with meta " + c.meta)
		}
		// The same value in another form is the stored value.
		for _, meta := range []string{`{"list":[1.0,25e-1],"n":9007199254740993}`, `{"n":9.007199254740993e15,"list":[10e-1,2.50]}`} {
			r := e.send(a, http.MethodPut, path, "", `{"folder_id":"`+folder+`","code":"c1","title":"kept","sku":"s1","meta":`+meta+`}`)
			if r.status != http.StatusOK {
				t.Errorf("an update with meta %s, the stored value in another form, answered %d %s, want 200", meta, r.status, r.body)
			}
			stored("after an update with meta " + meta)
		}
	})

	t.Run("ST-SUP-RC20-OC45-T01 A create and an upsert that inserts store the default of a never field and refuse another value, and an upsert of a stored row keeps each kept field", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		problemNames := func(what string, r reply, fields ...string) {
			t.Helper()
			var problem struct {
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"validation_errors"`
			}
			if r.status != http.StatusBadRequest || json.Unmarshal(r.body, &problem) != nil || len(problem.Errors) != len(fields) {
				t.Errorf("%s answered %d %s, want 400 that names %v", what, r.status, r.body, fields)
				return
			}
			for i, field := range fields {
				if problem.Errors[i].Field != field {
					t.Errorf("%s: entry %d is %+v, want the field %s", what, i, problem.Errors[i], field)
				}
			}
		}
		// A create keeps the client's value of a create field and stores the default of each never field.
		ticket := `{"folder_id":"` + folder + `","code":"%s","title":"t","sku":"s1"%s}`
		problemNames("a create with another state than the default", e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), fmt.Sprintf(ticket, "c1", `,"state":"closed"`)), "state")
		problemNames("a create with a rank", e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), fmt.Sprintf(ticket, "c1", `,"rank":5,"state":"closed"`)), "state", "rank")
		if n := e.count(`SELECT count(*) FROM tickets WHERE code = 'c1' AND tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d ticket rows of the refused creates, want 0", n)
		}
		created := e.send(a, http.MethodPost, "/api/v1/tickets", "k-"+uuid.NewString(), fmt.Sprintf(ticket, "c1", `,"state":"open","rank":null`))
		if object := created.object(t); created.status != http.StatusCreated || object["sku"] != "s1" || object["state"] != "open" || object["rank"] != nil {
			t.Errorf("a create that gives the default state and no rank answered %d %s, want 201 with sku s1, state open and no rank", created.status, created.body)
		}
		// An upsert that inserts acts as a create.
		problemNames("an upsert that inserts with another state", e.send(a, http.MethodPut, "/api/v1/tickets", "", fmt.Sprintf(ticket, "u1", `,"state":"closed"`)), "state")
		if n := e.count(`SELECT count(*) FROM tickets WHERE code = 'u1' AND tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d ticket rows of the refused upsert, want 0", n)
		}
		inserted := e.send(a, http.MethodPut, "/api/v1/tickets", "", fmt.Sprintf(ticket, "u1", `,"meta":{"b":2}`))
		if object := inserted.object(t); inserted.status != http.StatusCreated || object["sku"] != "s1" || object["state"] != "open" {
			t.Fatalf("the upsert that inserts answered %d %s, want 201 with sku s1 and state open", inserted.status, inserted.body)
		}
		id := inserted.member(t, "id")
		e.own(a, "ticket", id)
		if _, err := e.admin.ExecContext(e.ctx, `UPDATE tickets SET state = 'closed' WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		// An upsert of the stored row acts as an update: it keeps each kept field and refuses another value.
		updated := e.send(a, http.MethodPut, "/api/v1/tickets", "", `{"folder_id":"`+folder+`","code":"u1","title":"second","sku":"s1"}`)
		if object := updated.object(t); updated.status != http.StatusOK || object["id"] != id || object["title"] != "second" || object["state"] != "closed" || fmt.Sprint(object["meta"]) != "map[b:2]" {
			t.Errorf("the upsert of the stored row answered %d %s, want 200 with the new title and the stored state and meta", updated.status, updated.body)
		}
		problemNames("an upsert of the stored row that changes sku and state", e.send(a, http.MethodPut, "/api/v1/tickets", "", `{"folder_id":"`+folder+`","code":"u1","title":"third","sku":"s2","state":"open"}`), "sku", "state")
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'second' AND sku = 's1' AND state = 'closed' AND meta = '{"b":2}'::jsonb`, id); n != 1 {
			t.Errorf("%d ticket rows as the first upsert of the stored row left them, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC46-T01 A patch refuses a create or never field that the body names with another value, and changes nothing", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		id := e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"c1","title":"first","sku":"s1"}`)
		path := "/api/v1/tickets/" + id
		if r := e.send(a, http.MethodPatch, path, "k-"+uuid.NewString(), `{"title":"second","sku":"s1","state":"open"}`); r.status != http.StatusOK || r.member(t, "title") != "second" {
			t.Errorf("a patch that repeats the stored sku and state answered %d %s, want 200 with the new title", r.status, r.body)
		}
		for name, body := range map[string]string{
			"sku":   `{"title":"third","sku":"s2"}`,
			"state": `{"title":"third","state":"closed"}`,
			// A member matches a field only in its exact letter case (ST-SUP-RC20-OC15).
			"State": `{"title":"third","State":"open"}`,
			"rank":  `{"title":"third","rank":1}`,
			"meta":  `{"title":"third","meta":{}}`,
		} {
			r := e.send(a, http.MethodPatch, path, "k-"+uuid.NewString(), body)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"`+name+`"`) {
				t.Errorf("a patch %s answered %d %s, want 400 that names %s", body, r.status, r.body, name)
			}
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'second' AND sku = 's1' AND state = 'open' AND rank IS NULL AND meta IS NULL`, id); n != 1 {
			t.Errorf("%d ticket rows as the first patch left them, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC15-T03 A patch refuses a member that is not kind, a patchable field or a kept field, also in another letter case, and changes nothing", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		id := e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"c1","title":"first","sku":"s1"}`)
		path := "/api/v1/tickets/" + id
		for member, body := range map[string]string{
			"Title":     `{"Title":"value-of-the-member"}`,
			"titel":     `{"titel":"value-of-the-member"}`,
			"folder_id": `{"title":"second","folder_id":"value-of-the-member"}`,
			"href":      `{"title":"second","href":"value-of-the-member"}`,
		} {
			r := e.send(a, http.MethodPatch, path, "k-"+uuid.NewString(), body)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"field":"`+member+`"`) || strings.Contains(string(r.body), "value-of-the-member") {
				t.Errorf("a patch %s answered %d %s, want 400 that names %s and holds no value", body, r.status, r.body, member)
			}
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'first'`, id); n != 1 {
			t.Errorf("%d ticket rows as the create left them, want 1", n)
		}
		if r := e.send(a, http.MethodPatch, path, "k-"+uuid.NewString(), `{"kind":"Ticket","title":"second"}`); r.status != http.StatusOK || r.member(t, "title") != "second" {
			t.Errorf("a patch with kind and a patchable field answered %d %s, want 200 with the new title", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC44-T02 A secret kept field is refused whenever the body gives it, so the answer tells nothing of its value", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		// A create may set a secret with writable create.
		id := e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"c1","title":"first","sku":"s1","pin":"1234"}`)
		path := "/api/v1/tickets/" + id
		// The secret is sealed at rest: the check compares the stored column with itself before and after.
		sealedPin := func() string {
			t.Helper()
			var v string
			must(t, e.admin.QueryRowContext(e.ctx, `SELECT coalesce(pin::text, '') FROM tickets WHERE id = $1::uuid`, id).Scan(&v))
			return v
		}
		storedPin := sealedPin()
		if storedPin == "" {
			t.Fatal("the create stored no pin")
		}
		entries := func(r reply) string {
			t.Helper()
			var problem struct {
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"validation_errors"`
			}
			if json.Unmarshal(r.body, &problem) != nil {
				return ""
			}
			return fmt.Sprint(problem.Errors)
		}
		full := func(pin string) string {
			return `{"folder_id":"` + folder + `","code":"c1","title":"t","sku":"s1",` + pin + `}`
		}
		// The right value, a wrong value and null, through the update, the patch and the upsert of the stored row.
		first := ""
		for _, c := range []struct{ method, path, key, body string }{
			{http.MethodPut, path, "", full(`"pin":"1234"`)},
			{http.MethodPut, path, "", full(`"pin":"9999"`)},
			{http.MethodPut, path, "", full(`"pin":null`)},
			{http.MethodPatch, path, "k-" + uuid.NewString(), `{"title":"t","pin":"1234"}`},
			{http.MethodPatch, path, "k-" + uuid.NewString(), `{"title":"t","pin":"9999"}`},
			{http.MethodPut, "/api/v1/tickets", "", full(`"pin":"1234"`)},
			{http.MethodPut, "/api/v1/tickets", "", full(`"pin":null`)},
		} {
			r := e.send(a, c.method, c.path, c.key, c.body)
			got := entries(r)
			if r.status != http.StatusBadRequest || !strings.Contains(got, "pin") || !strings.Contains(got, "leave it out") || strings.Contains(string(r.body), "1234") {
				t.Errorf("%s %s %s answered %d %s, want 400 that names pin and says to leave it out", c.method, c.path, c.body, r.status, r.body)
			}
			if first == "" {
				first = got
			} else if got != first {
				t.Errorf("%s %s %s answered the entries %s, want the same entries %s as for the right value", c.method, c.path, c.body, got, first)
			}
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'first'`, id); n != 1 || sealedPin() != storedPin {
			t.Errorf("%d ticket rows with the first title, and the pin changed %v, after the refused writes; want 1 and the stored pin", n, sealedPin() != storedPin)
		}
		// A body that leaves the secret out, as a read row sent back does, keeps it.
		read := e.send(a, http.MethodGet, path, "", "")
		row := read.object(t)
		if _, ok := row["pin"]; ok {
			t.Fatalf("the read answers the secret: %s", read.body)
		}
		row["title"] = "second"
		body, err := json.Marshal(row)
		must(t, err)
		if r := e.send(a, http.MethodPut, path, "", string(body)); r.status != http.StatusOK {
			t.Errorf("an update that sends back the read row answered %d %s, want 200", r.status, r.body)
		}
		// The update seals the kept secret again, so the check opens it.
		var held []byte
		must(t, e.admin.QueryRowContext(e.ctx, `SELECT pin FROM tickets WHERE id = $1::uuid AND title = 'second'`, id).Scan(&held))
		plain, err := e.ring.Open(held, sealed.Place{Tenant: a.tenant.ID(), Table: "tickets", Column: "pin", Row: id})
		if err != nil || string(plain) != "1234" {
			t.Errorf("the pin after the update opens to %q (%v), want the stored 1234", plain, err)
		}
	})

	t.Run("ST-SUP-RC20-OC48-T03 A compare-only secret with writable never is refused whenever a body gives it, and no write stores it", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		entries := func(r reply) string {
			t.Helper()
			var problem struct {
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
				} `json:"validation_errors"`
			}
			if json.Unmarshal(r.body, &problem) != nil {
				return ""
			}
			return fmt.Sprint(problem.Errors)
		}
		ticket := func(code, title, recovery string) string {
			return `{"folder_id":"` + folder + `","code":"` + code + `","title":"` + title + `","sku":"s1"` + recovery + `}`
		}
		id := e.create(a, "tickets", "ticket", ticket("c1", "first", ""))
		path := "/api/v1/tickets/" + id
		// A create, an upsert that inserts, an update, a patch and an upsert of the stored row, each with a value,
		// another value and null: no write compares the secret, so each answer is the same.
		first := ""
		for _, c := range []struct{ method, path, key, body string }{
			{http.MethodPost, "/api/v1/tickets", "k-" + uuid.NewString(), ticket("c2", "t", `,"recovery":"1234"`)},
			{http.MethodPost, "/api/v1/tickets", "k-" + uuid.NewString(), ticket("c2", "t", `,"recovery":null`)},
			{http.MethodPut, "/api/v1/tickets", "", ticket("c3", "t", `,"recovery":"1234"`)},
			{http.MethodPut, path, "", ticket("c1", "t", `,"recovery":"1234"`)},
			{http.MethodPut, path, "", ticket("c1", "t", `,"recovery":null`)},
			{http.MethodPatch, path, "k-" + uuid.NewString(), `{"title":"t","recovery":"9999"}`},
			{http.MethodPut, "/api/v1/tickets", "", ticket("c1", "t", `,"recovery":"1234"`)},
		} {
			r := e.send(a, c.method, c.path, c.key, c.body)
			got := entries(r)
			if r.status != http.StatusBadRequest || !strings.Contains(got, "recovery") || !strings.Contains(got, "leave it out") || strings.Contains(string(r.body), "1234") || strings.Contains(string(r.body), "9999") {
				t.Errorf("%s %s %s answered %d %s, want 400 that names recovery and says to leave it out", c.method, c.path, c.body, r.status, r.body)
			}
			if first == "" {
				first = got
			} else if got != first {
				t.Errorf("%s %s %s answered the entries %s, want the same entries %s as for the first write", c.method, c.path, c.body, got, first)
			}
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE folder_id = $1::uuid AND code IN ('c2', 'c3')`, folder); n != 0 {
			t.Errorf("%d ticket rows of the refused create and upsert, want 0", n)
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE id = $1::uuid AND title = 'first' AND recovery IS NULL`, id); n != 1 {
			t.Errorf("%d ticket rows with the first title and no recovery after the refused writes, want 1", n)
		}
		// The read gives no recovery, and a body that leaves it out is written.
		read := e.send(a, http.MethodGet, path, "", "")
		if _, ok := read.object(t)["recovery"]; ok {
			t.Errorf("the read answers the secret: %s", read.body)
		}
		if r := e.send(a, http.MethodPut, path, "", ticket("c1", "second", "")); r.status != http.StatusOK {
			t.Errorf("an update that leaves recovery out answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC20-OC48-T04 No answer holds a member of a secret field, also not of a required one", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		// noSecret fails when the object holds a member of the required secret api_token or the optional secret vault.
		noSecret := func(what string, object map[string]any) {
			t.Helper()
			for _, name := range []string{"api_token", "vault"} {
				if _, ok := object[name]; ok {
					t.Errorf("%s holds the member %s of a secret field: %v", what, name, object)
				}
			}
		}
		created := e.send(a, http.MethodPost, "/api/v1/customers", "k-"+uuid.NewString(), `{"name":"Sam","api_token":"tok-1","plan":"free","vault":{"pin":"4321"}}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		noSecret("the create answer", created.object(t))
		id := created.member(t, "id")
		e.own(a, "customer", id)
		noSecret("the read answer", e.send(a, http.MethodGet, "/api/v1/customers/"+id, "", "").object(t))
		updated := e.send(a, http.MethodPut, "/api/v1/customers/"+id, "", `{"name":"Sam","plan":"pro"}`)
		if updated.status != http.StatusOK {
			t.Fatalf("the update answered %d %s, want 200", updated.status, updated.body)
		}
		noSecret("the update answer", updated.object(t))
		list := e.send(a, http.MethodGet, "/api/v1/customers", "", "")
		items, _ := list.object(t)["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("the list answered %d %s, want the customer", list.status, list.body)
		}
		for _, item := range items {
			object, _ := item.(map[string]any)
			noSecret("a list item", object)
		}
		// A replay of the create gives the stored answer, which holds no secret either (security review of PR #442,
		// I2).
		key := "k-" + uuid.NewString()
		body := `{"name":"Kim","api_token":"tok-2","plan":"free","vault":{"pin":"8765"}}`
		first := e.send(a, http.MethodPost, "/api/v1/customers", key, body)
		replay := e.send(a, http.MethodPost, "/api/v1/customers", key, body)
		if first.status != http.StatusCreated || replay.status != http.StatusCreated || replay.header.Get(idempotency.ReplayedHeader) != "true" {
			t.Fatalf("the create and its replay answered %d and %d (replayed %q), want 201 and the replayed 201", first.status, replay.status, replay.header.Get(idempotency.ReplayedHeader))
		}
		noSecret("the replayed create answer", replay.object(t))
		// An upsert that inserts and a patch answer the ticket with no member of its secret pin or of its
		// compare-only secret recovery.
		ticketSecret := func(what string, r reply) {
			t.Helper()
			for _, name := range []string{"pin", "recovery"} {
				if _, ok := r.object(t)[name]; ok {
					t.Errorf("%s holds the member %s of a secret field: %s", what, name, r.body)
				}
			}
		}
		folder := e.create(a, "folders", "folder", `{"name":"secrets"}`)
		upserted := e.send(a, http.MethodPut, "/api/v1/tickets", "", `{"folder_id":"`+folder+`","code":"sec-1","title":"t","sku":"s1","pin":"2468"}`)
		if upserted.status != http.StatusCreated && upserted.status != http.StatusOK {
			t.Fatalf("the upsert answered %d %s, want 200 or 201", upserted.status, upserted.body)
		}
		ticketSecret("the upsert answer", upserted)
		ticket := upserted.member(t, "id")
		e.own(a, "ticket", ticket)
		patched := e.send(a, http.MethodPatch, "/api/v1/tickets/"+ticket, "k-"+uuid.NewString(), `{"title":"t2"}`)
		if patched.status != http.StatusOK {
			t.Fatalf("the patch answered %d %s, want 200", patched.status, patched.body)
		}
		ticketSecret("the patch answer", patched)
	})

	t.Run("ST-SUP-RC20-OC51-T01 A list refuses a query parameter that it does not read, names it, and quotes no value", func(t *testing.T) {
		e := service(t)
		a := e.as("u-1", newTenant())
		e.create(a, "todos", "todo", `{"title":"t1"}`)
		detail := func(r reply) string {
			t.Helper()
			var problem struct {
				Detail string `json:"detail"`
			}
			if json.Unmarshal(r.body, &problem) != nil {
				return ""
			}
			return problem.Detail
		}
		many := ""
		for i := range 12 {
			many += fmt.Sprintf("&x%02d=v", i)
		}
		long := strings.Repeat("p", 100)
		for _, c := range []struct{ query, want, absent string }{
			{"limit=SECRET-VALUE", `"limit"`, "SECRET-VALUE"},
			{"PageSize=1", `"PageSize"`, ""},
			{"zz=1&page_size=1&pageSize=1", `"page_size", "zz"`, ""},
			{"tenant=t2", `"tenant"`, ""},
			{many[1:], `"x09" and 2 more`, `"x10"`},
			{long + "=1", `"` + strings.Repeat("p", 64) + `"`, strings.Repeat("p", 65)},
		} {
			r := e.send(a, http.MethodGet, "/api/v1/todos?"+c.query, "", "")
			got := detail(r)
			if r.status != http.StatusBadRequest || !strings.Contains(got, c.want) || (c.absent != "" && strings.Contains(string(r.body), c.absent)) {
				t.Errorf("a list with %.60s answered %d %s, want 400 whose detail names %s", c.query, r.status, r.body, c.want)
			}
			if strings.HasPrefix(c.query, "limit") && !strings.Contains(got, "pageSize sets the page size") {
				t.Errorf("a list with limit answered the detail %q, want it to name pageSize", got)
			}
		}
		// A query string that does not parse is refused, not read in part: a ';', a bad escape, and more parameters
		// than the url package takes, which r.URL.Query() gives as no parameter at all (security review of PR #442, L1).
		var past strings.Builder
		past.WriteString("pageSize=1")
		for range 10001 {
			past.WriteString("&k=1")
		}
		for _, query := range []string{"a;pageSize=1", "%zz=1&pageSize=1", past.String()} {
			r := e.send(a, http.MethodGet, "/api/v1/todos?"+query, "", "")
			if got := detail(r); r.status != http.StatusBadRequest || got != "the query string is not valid" || r.header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("a list with the query string %.30s (%d bytes) answered %d %q, want 400 with nosniff that the query string is not valid", query, len(query), r.status, got)
			}
		}
		// The parameters that the list reads are taken.
		if r := e.send(a, http.MethodGet, "/api/v1/todos?pageSize=1&orderBy=title&fields=title&search=&cursor=", "", ""); r.status != http.StatusOK {
			t.Errorf("a list with only the parameters that it reads answered %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("ST-SUP-RC04-OC23-T06 The emitted apply command finds a resource through the list of a generated service", func(t *testing.T) {
		e := service(t)
		a := e.as("cli-1", newTenant())
		var gets atomic.Int32
		apply := e.cli(t, a, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method == http.MethodGet {
				gets.Add(1)
			}
			next.ServeHTTP(w, r)
		})
		folders := func(name string) int {
			return e.count(`SELECT count(*) FROM folders WHERE tenant_id = $1 AND name = $2`, a.tenant.ID(), name)
		}
		// A new name: the lookup lists no folder, so the CLI creates one.
		results, err := apply("alpha")
		if err != nil || len(results) != 1 || results[0]["status"] != "created" || results[0]["id"] == "" {
			t.Fatalf("the first apply of alpha gave %v %v, want the status created with an ID", results, err)
		}
		if gets.Load() == 0 {
			t.Error("the apply of alpha listed no folder before its create")
		}
		e.own(a, "folder", results[0]["id"])
		// The same name again: the lookup lists the folder, so the CLI writes nothing.
		again, err := apply("alpha")
		if err != nil || len(again) != 1 || again[0]["status"] != "unchanged" || again[0]["id"] != results[0]["id"] {
			t.Errorf("the second apply of alpha gave %v %v, want the status unchanged with the ID %s", again, err, results[0]["id"])
		}
		if n := folders("alpha"); n != 1 {
			t.Errorf("%d folders alpha after two applies, want 1", n)
		}
		// Two folders of one name: the lookup lists both, so the CLI stops before any write.
		e.create(a, "folders", "folder", `{"name":"twin"}`)
		e.create(a, "folders", "folder", `{"name":"twin"}`)
		if results, err := apply("twin"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("the apply of an ambiguous name gave %v %v, want the error that the identity is ambiguous", results, err)
		}
		if n := folders("twin"); n != 2 {
			t.Errorf("%d folders twin after the ambiguous apply, want 2", n)
		}
	})

	t.Run("ST-SUP-RC04-OC31-T02 The emitted CLI writes to a generated service that requires a key, and the second request of a write gets the stored answer", func(t *testing.T) {
		e := service(t)
		a := e.as("cli-2", newTenant())
		// The listener of the service passes each request as it is. It keeps the key of each POST, and for the first
		// POST of the folder lost it lets the service do the write, then closes the connection with no answer, as a
		// network fault after the commit does.
		var mu sync.Mutex
		var keys []string
		var replayed []string
		dropped := false
		apply := e.cli(t, a, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			drop := !dropped && strings.Contains(string(body), `"lost"`)
			dropped = dropped || drop
			mu.Unlock()
			if drop {
				next.ServeHTTP(httptest.NewRecorder(), r)
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			next.ServeHTTP(w, r)
			mu.Lock()
			replayed = append(replayed, w.Header().Get("Idempotent-Replayed"))
			mu.Unlock()
		})
		folders := func(name string) int {
			return e.count(`SELECT count(*) FROM folders WHERE tenant_id = $1 AND name = $2`, a.tenant.ID(), name)
		}
		// Each apply write has a key of its own, so the service, which requires a key on a create, does the write.
		for _, name := range []string{"one", "two"} {
			if results, err := apply(name); err != nil || len(results) != 1 || results[0]["status"] != "created" {
				t.Fatalf("the apply of %s gave %v %v, want the status created", name, results, err)
			}
		}
		if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] {
			t.Errorf("the creates of one and two carried the keys %q, want two keys that differ", keys)
		}
		// The first answer of lost does not come back. The CLI sends the create again with the same key, and the
		// service gives the stored answer of the write that it did, so there is one folder lost.
		keys, replayed = nil, nil
		results, err := apply("lost")
		if err != nil || len(results) != 1 || results[0]["status"] != "created" || results[0]["id"] == "" {
			t.Fatalf("the apply of lost gave %v %v, want the status created", results, err)
		}
		if n := folders("lost"); n != 1 {
			t.Errorf("%d folders lost after a create that the CLI sent two times, want 1", n)
		}
		if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
			t.Errorf("the creates of lost carried the keys %q, want one key two times", keys)
		}
		if len(replayed) != 1 || replayed[0] != "true" {
			t.Errorf("the answer of the second create of lost had Idempotent-Replayed %q, want true", replayed)
		}
	})

	t.Run("ST-SUP-RC30-OC08-T02 Each answer of the generated service to a create or a patch with a key carries Idempotency-Kept: 1, a problem and a 503 too, and no other answer carries it", func(t *testing.T) {
		e := service(t)
		a := e.as("kept-1", newTenant())
		kept := func(r reply) string { return strings.Join(r.header.Values("Idempotency-Kept"), ",") }
		key := "k-" + uuid.NewString()
		created := e.send(a, http.MethodPost, "/api/v1/orders", key, `{"sku":"s-kept","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		e.own(a, "order", id)
		patchKey := "k-" + uuid.NewString()
		for _, c := range []struct {
			name, method, path, key, body string
			status                        int
			kept                          bool
		}{
			{"the create", "", "", "", "", 0, true},
			{"the create given again", http.MethodPost, "/api/v1/orders", key, `{"sku":"s-kept","qty":1}`, http.StatusCreated, true},
			{"a create with a used key and another body", http.MethodPost, "/api/v1/orders", key, `{"sku":"s-kept","qty":2}`, http.StatusUnprocessableEntity, true},
			{"a create whose body is not valid", http.MethodPost, "/api/v1/orders", "k-" + uuid.NewString(), `{"sku":`, http.StatusBadRequest, true},
			{"a patch", http.MethodPatch, "/api/v1/orders/" + id, patchKey, `{"qty":2}`, http.StatusOK, true},
			{"the patch given again", http.MethodPatch, "/api/v1/orders/" + id, patchKey, `{"qty":2}`, http.StatusOK, true},
			{"a patch of a row that is not there", http.MethodPatch, "/api/v1/orders/" + uuid.NewString(), "k-" + uuid.NewString(), `{"qty":2}`, http.StatusNotFound, true},
			{"a create with no key", http.MethodPost, "/api/v1/orders", "", `{"sku":"s-kept","qty":1}`, http.StatusBadRequest, false},
			{"a read with a key", http.MethodGet, "/api/v1/orders/" + id, "k-" + uuid.NewString(), "", http.StatusOK, false},
			{"a list with a key", http.MethodGet, "/api/v1/orders", "k-" + uuid.NewString(), "", http.StatusOK, false},
			{"an update with a key", http.MethodPut, "/api/v1/customers/" + uuid.NewString(), "k-" + uuid.NewString(), `{"name":"n"}`, 0, false},
			{"a delete with a key", http.MethodDelete, "/api/v1/orders/" + id, "k-" + uuid.NewString(), "", http.StatusNoContent, false},
		} {
			r := created
			if c.method != "" {
				r = e.send(a, c.method, c.path, c.key, c.body)
			}
			if c.status != 0 && r.status != c.status {
				t.Errorf("%s answered %d %s, want %d", c.name, r.status, r.body, c.status)
			}
			if want := map[bool]string{true: "1", false: ""}[c.kept]; kept(r) != want {
				t.Errorf("%s answered %d with Idempotency-Kept %q, want %q", c.name, r.status, kept(r), want)
			}
		}
		// A patch that the provider cannot decide now answers 503, and the answer tells that the key is kept, so a
		// second request with the key is safe.
		other := e.create(a, "orders", "order", `{"sku":"s-kept","qty":1}`)
		e.decision.fail(t, status.Error(codes.Unavailable, "connection refused"))
		unavailable := e.send(a, http.MethodPatch, "/api/v1/orders/"+other, "k-"+uuid.NewString(), `{"qty":3}`)
		e.decision.fail(t, nil)
		if unavailable.status != http.StatusServiceUnavailable || kept(unavailable) != "1" {
			t.Errorf("a patch that the provider could not decide answered %d with Idempotency-Kept %q, want 503 with 1", unavailable.status, kept(unavailable))
		}
		// A create that the database fails answers 500, and the answer tells that the key is kept too.
		name := "s-fail-" + uuid.NewString()[:8]
		e.failInsert(t, "orders", "sku", name)
		failed := e.send(a, http.MethodPost, "/api/v1/orders", "k-"+uuid.NewString(), `{"sku":"`+name+`","qty":1}`)
		if failed.status < 500 || kept(failed) != "1" {
			t.Errorf("a create that the database failed answered %d %s with Idempotency-Kept %q, want a 5xx with 1", failed.status, failed.body, kept(failed))
		}
	})

	t.Run("ST-SUP-RC04-OC34-T02 The emitted CLI sends a write again after a 5xx of the service with Idempotency-Kept, not after a 5xx with none, and warns when a 2xx has none", func(t *testing.T) {
		e := service(t)
		a := e.as("cli-kept", newTenant())
		// The listener passes each request as it is, except a POST, as the mode says: proxy answers 502 itself as a
		// proxy does, and strip takes Idempotency-Kept out of the answer of the service, as a service of another
		// release gives it. The database fails each insert of the folder busy, so the service answers its create with
		// a 5xx.
		busy := "busy-" + uuid.NewString()[:8]
		e.failInsert(t, "folders", "name", busy)
		var mu sync.Mutex
		mode := ""
		var posts []string
		apply := e.cliWarn(t, a, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			mu.Lock()
			current := mode
			if r.Method == http.MethodPost {
				posts = append(posts, r.Header.Get("Idempotency-Key"))
			}
			mu.Unlock()
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			switch current {
			case "proxy":
				w.WriteHeader(http.StatusBadGateway)
			case "strip":
				answer := httptest.NewRecorder()
				next.ServeHTTP(answer, r)
				for name, values := range answer.Header() {
					if name != "Idempotency-Kept" {
						w.Header()[name] = values
					}
				}
				w.WriteHeader(answer.Code)
				_, _ = w.Write(answer.Body.Bytes())
			default:
				next.ServeHTTP(w, r)
			}
		})
		folders := func(name string) int {
			return e.count(`SELECT count(*) FROM folders WHERE tenant_id = $1 AND name = $2`, a.tenant.ID(), name)
		}
		run := func(m, name string) ([]map[string]string, string, error, []string) {
			t.Helper()
			mu.Lock()
			mode, posts = m, nil
			mu.Unlock()
			results, warned, err := apply(name)
			mu.Lock()
			defer mu.Unlock()
			return results, warned, err, append([]string(nil), posts...)
		}
		// A service of this release: the create succeeds with the header, and the CLI warns of nothing.
		if results, warned, err, sent := run("", "plain"); err != nil || len(results) != 1 || results[0]["status"] != "created" || len(sent) != 1 || warned != "" {
			t.Fatalf("the apply of plain gave %v %v after %d creates and the warning %q, want created after one and no warning", results, err, len(sent), warned)
		}
		// A 5xx of the service carries the header: the CLI sends the create again with the same key, the second
		// answer is a 5xx too, and the outcome is unknown.
		results, _, err, sent := run("", busy)
		if err == nil || len(results) != 1 || results[0]["status"] != "unknown" || len(sent) != 2 || sent[0] == "" || sent[0] != sent[1] {
			t.Errorf("the apply of busy with a 5xx of the service gave %v %v after the creates %q, want unknown after two creates with one key", results, err, sent)
		}
		if n := folders(busy); n != 0 {
			t.Errorf("%d folders busy after two creates that the database failed, want 0", n)
		}
		// A 502 of a proxy carries no header: the CLI does not send the create again.
		results, _, err, sent = run("proxy", "gate")
		if err == nil || len(results) != 1 || results[0]["status"] != "unknown" || len(sent) != 1 {
			t.Errorf("the apply of gate with a 502 of a proxy gave %v %v after %d creates, want unknown after one", results, err, len(sent))
		}
		// A 2xx with no header: the create is done, and the CLI warns on standard error that the releases can differ.
		results, warned, err, sent := run("strip", "old")
		if err != nil || len(results) != 1 || results[0]["status"] != "created" || len(sent) != 1 || !strings.Contains(warned, "different releases") {
			t.Errorf("the apply of old with no header gave %v %v after %d creates and the warning %q, want created after one and the warning", results, err, len(sent), warned)
		}
		if n := folders("old"); n != 1 {
			t.Errorf("%d folders old, want 1", n)
		}
	})

	t.Run("ST-SUP-RC20-OC45-T03 An upsert by a caller who may not write under the parent gets the same 404 whether a row of the key exists or not", func(t *testing.T) {
		e := service(t)
		tn := newTenant()
		a, other := e.as("u-1", tn), e.as("u-2", tn)
		folder := e.create(a, "folders", "folder", `{"name":"one"}`)
		e.create(a, "tickets", "ticket", `{"folder_id":"`+folder+`","code":"held","title":"t","sku":"s1"}`)
		// Each body breaks a kept field: a state other than the default.
		var answers []reply
		for _, code := range []string{"held", "free"} {
			r := e.send(other, http.MethodPut, "/api/v1/tickets", "", `{"folder_id":"`+folder+`","code":"`+code+`","title":"x","sku":"s2","state":"closed"}`)
			if r.status != http.StatusNotFound {
				t.Errorf("the upsert of the key %s by a caller who may not edit the folder answered %d %s, want 404", code, r.status, r.body)
			}
			answers = append(answers, r)
		}
		if len(answers) == 2 && fmt.Sprint(answers[0].object(t)["title"], answers[0].object(t)["detail"]) != fmt.Sprint(answers[1].object(t)["title"], answers[1].object(t)["detail"]) {
			t.Errorf("the answers differ: %s and %s", answers[0].body, answers[1].body)
		}
		if n := e.count(`SELECT count(*) FROM tickets WHERE code = 'free' AND tenant_id = $1`, a.tenant.ID()); n != 0 {
			t.Errorf("%d ticket rows of the refused upsert, want 0", n)
		}
	})
}
