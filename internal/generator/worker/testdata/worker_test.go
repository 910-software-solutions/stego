// Package check runs the generated runners of the workers of the test service, as the worker mode of the service
// runtime starts them (service.TickerEnv), on a local PostgreSQL server (STEGO_TEST_POSTGRES_DSN), with a test clock
// that moves only when the check moves it, the tenant list of the service and an in-memory probe recorder. The
// decision port allows each request: the decisions of a worker are proved by the command runtime's own tests.
package check

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/service"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	applyreplies "example.com/orders/fills/apply-replies"
	syncorders "example.com/orders/fills/sync-orders"
	workers "example.com/orders/out/workers"
	"example.com/orders/out/workers/runners"
)

// tablesSQL is the migration 001_initial of the PostgreSQL adapter for the declaration (ST-SUP-RC01-OC41).
//
//go:embed tables.sql
var tablesSQL string

const (
	password = "worker-check"
	// order is the ID of the one order of each tenant, which the fill of sync-partner-orders changes.
	order = "0b6f3f5e-0b7c-4b4e-9a53-3f2c1f0e2d11"
	// worker is the declared worker with an every trigger, every 1m, single.
	worker   = "sync-partner-orders"
	interval = time.Minute
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// allow is the decision port of the check: it allows each request.
type allow struct{}

func (allow) Evaluate(context.Context, authz.EvaluationRequest) (authz.EvaluationResult, error) {
	return authz.EvaluationResult{Allowed: true}, nil
}

func (allow) EvaluateBatch(_ context.Context, request authz.BatchRequest) ([]authz.EvaluationResult, error) {
	out := make([]authz.EvaluationResult, len(request.Evaluations))
	for i := range out {
		out[i].Allowed = true
	}
	return out, nil
}

func (allow) Search(context.Context, authz.SearchRequest) (authz.SearchResult, error) {
	return authz.SearchResult{}, authz.ErrNoSearch
}

func (allow) Searches() bool { return false }

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

// clock is the test clock: it stands still and moves only with advance, which fires each wait that is due. It
// counts the waits, so a check knows when the scheduler waits for a due time.
type clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []wait
	waits   atomic.Int64
	// changed is closed and made again at each new wait, so a check can wait for a wait with no deadline.
	changed chan struct{}
}

type wait struct {
	at time.Time
	ch chan time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, wait{at: c.now.Add(d), ch: ch})
	c.waits.Add(1)
	if c.changed != nil {
		close(c.changed)
	}
	c.changed = make(chan struct{})
	return ch
}

// waitedAt tells whether a wait ends exactly at at, and gives the channel that the next new wait closes.
func (c *clock) waitedAt(at time.Time) (bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	for _, w := range c.waiters {
		if w.at.Equal(at) {
			return true, c.changed
		}
	}
	return false, c.changed
}

// first blocks until one of the clocks has a wait that ends at its time, and gives the index of each wait that is
// there then, in the order given, so a check that both waits are there sees both. It has no deadline: a party that
// never waits ends the check at the timeout of go test, and a missing result is not a pass.
func first(waits ...clockWait) []int {
	for {
		var signals []<-chan struct{}
		var present []int
		for i, w := range waits {
			found, changed := w.clock.waitedAt(w.at)
			if found {
				present = append(present, i)
			}
			signals = append(signals, changed)
		}
		if len(present) > 0 {
			return present
		}
		cases := make([]reflect.SelectCase, len(signals))
		for i, ch := range signals {
			cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)}
		}
		reflect.Select(cases)
	}
}

// clockWait is a wait of a party on a clock that ends at a time.
type clockWait struct {
	clock *clock
	at    time.Time
}

// advance moves the clock on by d and fires each wait that is due, the earliest first.
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].at.Before(c.waiters[j].at) })
	var keep []wait
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
}

// move moves the clock on by d in steps of at most 2 s, with a pause after each, as time passes: the lease client of
// a single worker renews its lease at each 10 s of the clock and gives it up when the clock passes its expiry, 15 s
// after the last renew, so a single jump of a minute would lose the lease.
func (c *clock) move(d time.Duration) {
	for d > 0 {
		step := min(d, 2*time.Second)
		c.advance(step)
		d -= step
		time.Sleep(30 * time.Millisecond)
	}
}

// due tells whether a wait ends exactly at at.
func (c *clock) due(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if w.at.Equal(at) {
			return true
		}
	}
	return false
}

// newClock is a clock one second before the next multiple of the interval since the Unix epoch after the system
// time: a run under a lease is bounded by the lease's expiry on this clock and by the system clock, so this clock is
// never far behind the system clock.
func newClock() *clock {
	step := interval.Nanoseconds()
	return &clock{now: time.Unix(0, (time.Now().UnixNano()/step+1)*step).Add(-time.Second)}
}

// newLeaseClock is the process clock of the lease clients of a check, apart from the clock of the ticks: it stands
// still at the system time until the check moves it, so a holder renews, and a holder that waits for the lease tries
// again, only when the check says so. A lease's expiry on this clock is never before the system time while the check
// runs, so the deadline of a run under the lease is the one that the lease TTL gives.
func newLeaseClock() *clock { return &clock{now: time.Now()} }

// nextDue is the next due time of the worker after the clock's time.
func (c *clock) nextDue() time.Time {
	n := c.Now().UnixNano()
	step := interval.Nanoseconds()
	return time.Unix(0, (n/step+1)*step)
}

// env is the database of the service with its runtime role and the command runtime of the workers on it, which the
// checks share: the generated entities register with one runtime only.
type env struct {
	t     *testing.T
	ctx   context.Context
	admin *sql.DB
	pool  *sql.DB
	rt    *command.Runtime
	// workerDB is a pool of the worker role, the role of the outbox worker that runs the consuming workers.
	workerDB *sql.DB
}

// newEnv makes a database of its own and starts the command runtime of the workers on it, with the telemetry
// runtime given.
func newEnv(t *testing.T, telemetry *otel.Runtime) *env {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN: a missing result is not a pass")
	}
	e := &env{t: t, ctx: context.Background()}
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	defer root.Close()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	name := "wk_" + tag
	roles := [3]string{"wk_rt_" + tag, "wk_worker_" + tag, "wk_relay_" + tag}
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
	queueDDL, err := outbox.Migration("wk_check"+outbox.RelaySuffix, roles[0], roles[1])
	must(t, err)
	authzDDL, err := authz.Migration("authz", roles[0], roles[2], "wk_check"+outbox.RelaySuffix, roles[1])
	must(t, err)
	auditDDL, err := audit.DDL(audit.Config{RuntimeRole: roles[0], AnchorPeriod: time.Hour, RetentionMonths: 12})
	must(t, err)
	tenantsDDL, err := tenant.Migration("public", roles[0])
	must(t, err)
	// The key table of the idempotency runtime, which RunConsumer writes the message identity to.
	keysDDL, err := idempotency.Migration("wk_purge_"+tag, roles[0])
	must(t, err)
	setup := []string{
		queueDDL,
		"CREATE SCHEMA authz",
		"GRANT USAGE ON SCHEMA authz TO " + roles[0] + ", " + roles[1] + ", " + roles[2],
		authzDDL,
		auditDDL,
		tenantsDDL,
		lease.LeasesTable,
		"GRANT SELECT, UPDATE ON leases TO " + roles[0],
		"INSERT INTO leases (name) VALUES ('" + service.WorkerLeasePrefix + worker + "')",
		tablesSQL,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON orders TO " + roles[0],
		"ALTER TABLE orders ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE orders FORCE ROW LEVEL SECURITY",
		"CREATE POLICY orders_tenant ON orders USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
		keysDDL,
		"GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO " + roles[0],
		// The outbox worker runs the consumers in its own transactions, as the worker role, with the runtime role's
		// grants; the grants of the worker role itself are proved by the runtime wiring test (ST-SUP-Q141).
		"GRANT " + roles[0] + " TO " + roles[1],
	}
	for _, statement := range setup {
		if _, err := e.admin.ExecContext(e.ctx, statement); err != nil {
			t.Fatalf("%.80s: %v", statement, err)
		}
	}
	u.User = url.UserPassword(roles[1], password)
	e.workerDB, err = sql.Open("pgx", u.String())
	must(t, err)
	u.User = url.UserPassword(roles[0], password)
	runtimeDSN := u.String()
	e.pool, err = tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	auditDB, err := tenant.Open(e.ctx, tenant.Config{InsecureLoopback: true, DSN: runtimeDSN})
	must(t, err)
	maintainAudit(t, e.ctx, e.pool)
	queue, err := outbox.New(e.pool, outbox.Options{})
	must(t, err)
	writer, err := audit.NewWriter(auditOutbox{q: queue}, nil)
	must(t, err)
	az, err := authz.New(authz.Config{Provider: allow{}, DB: e.pool, Schema: "authz", Outbox: queue})
	must(t, err)
	e.rt, err = command.New(e.ctx, command.Config{DB: e.pool, AuditDB: auditDB, Outbox: queue, Authz: az, Audit: writer,
		Service: "orders", ServiceVersion: "1.0.0", DeclarationVersion: "d1", Entities: runners.Entities(),
		Probes: newRecorder(t), Telemetry: telemetry})
	must(t, err)
	t.Cleanup(func() {
		e.pool.Close()
		auditDB.Close()
		e.workerDB.Close()
		e.admin.Close()
	})
	return e
}

// newRecorder is a probe recorder of the built-in observations of the declared workers.
func newRecorder(t *testing.T) *probe.Recorder {
	t.Helper()
	builtins, err := probe.NewBuiltins(runners.Names(), nil)
	must(t, err)
	return probe.NewRecorder(builtins)
}

func (e *env) exec(statement string, args ...any) {
	e.t.Helper()
	if _, err := e.admin.ExecContext(e.ctx, statement, args...); err != nil {
		e.t.Fatalf("%.80s: %v", statement, err)
	}
}

// status reads the status of the order of the tenant.
func (e *env) status(tenantID string) string {
	e.t.Helper()
	var status string
	must(e.t, e.admin.QueryRowContext(e.ctx, `SELECT status FROM orders WHERE tenant_id = $1 AND id = $2`, tenantID, order).Scan(&status))
	return status
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

// call is one call of the fill of sync-partner-orders.
type call struct {
	tenant string
	due    time.Time
}

// body is what the fill of the check does for one tenant after it reported its call.
type body func(ctx context.Context, tenantID string, h *workers.SyncPartnerOrdersHandle) error

// instance is the runner of sync-partner-orders of one instance of the service: the ticker that the generated
// runners give for the worker mode's TickerEnv, running until the check ends it.
type instance struct {
	t      *testing.T
	e      *env
	clock  *clock
	ticker *command.Ticker
	rec    *probe.Recorder
	// signals gets each skipped tick and each failed run of the instance, after the recorder got it, so a check
	// waits for them with no deadline and no poll.
	signals *signalled
	done    chan struct{}
}

// signalled holds the channels that a signalledObserver sends to: one value for each skipped tick and each failed
// run. A full channel drops the value, as no check reads more than 64.
type signalled struct {
	skipped chan struct{}
	failed  chan probe.WorkerOutcome
}

// signalledObserver is the observer of the recorder that also signals each skipped tick and each failed run.
type signalledObserver struct {
	command.WorkerObserver
	signals *signalled
}

func (o signalledObserver) RunFailed(ctx context.Context, outcome probe.WorkerOutcome) {
	o.WorkerObserver.RunFailed(ctx, outcome)
	select {
	case o.signals.failed <- outcome:
	default:
	}
}

// RunFailedCause passes the cause class of a failed run on to the recorder (ST-SUP-RC35-OC06).
func (o signalledObserver) RunFailedCause(ctx context.Context, outcome probe.WorkerOutcome, cause string) {
	o.WorkerObserver.(command.CauseObserver).RunFailedCause(ctx, outcome, cause)
	select {
	case o.signals.failed <- outcome:
	default:
	}
}

func (o signalledObserver) TickSkipped() {
	o.WorkerObserver.TickSkipped()
	select {
	case o.signals.skipped <- struct{}{}:
	default:
	}
}

// calls gets each call of the fill, from every instance of the check. The check binds the fill once for the test
// binary through the fill folder (runners.BoundFills); each instance changes only what the fill does.
var (
	calls    = make(chan call, 256)
	bodyMu   sync.Mutex
	bodyOf   = map[string]body{}
	bindOnce sync.Once
)

// setBody makes the fill run b for the tenants of the database of e.
func setBody(prefix string, b body) {
	bodyMu.Lock()
	defer bodyMu.Unlock()
	bodyOf[prefix] = b
}

func bodyFor(tenantID string) body {
	bodyMu.Lock()
	defer bodyMu.Unlock()
	for prefix, b := range bodyOf {
		if strings.HasPrefix(tenantID, prefix) {
			return b
		}
	}
	return nil
}

// start starts one instance of the worker mode's tickers on e, as service.Start builds the TickerEnv, and gives the
// ticker of sync-partner-orders. With a holder, the ticker runs under the lease stego.worker.sync-partner-orders as
// that holder. With none, the TickerEnv gives no lease client, so the ticker runs without a lease: a run under a lease
// holds the lease row and is bounded by the system clock, which the test clock does not move.
func start(t *testing.T, e *env, c *clock, holder string) *instance {
	t.Helper()
	return startWith(t, e, c, func(name string, ttl time.Duration) (*lease.Client, error) {
		if holder == "" {
			return nil, nil
		}
		return lease.New(e.pool, service.WorkerLeasePrefix+name, holder, lease.Settings{TTL: ttl, Renew: ttl * 2 / 3, Clock: c})
	})
}

// leaseTTL, leaseRenew and leaseRetry are the lease settings of a holder of startLeased: a lease that the database
// holds for an hour, so it never expires while a check runs, and a renew and a retry that happen only when the check
// moves the lease clock.
const (
	leaseTTL   = time.Hour
	leaseRenew = 2 * time.Second
	leaseRetry = time.Second
)

// startLeased starts one instance whose ticker of sync-partner-orders runs under the lease
// stego.worker.sync-partner-orders as holder, with the ticks on the clock tick and the lease on the clock leaseClock.
func startLeased(t *testing.T, e *env, tick, leaseClock *clock, holder string) *instance {
	t.Helper()
	return startWith(t, e, tick, func(name string, _ time.Duration) (*lease.Client, error) {
		return lease.New(e.pool, service.WorkerLeasePrefix+name, holder, lease.Settings{TTL: leaseTTL, Renew: leaseRenew, Retry: leaseRetry, Clock: leaseClock})
	})
}

// startWith starts one instance of the worker mode's tickers on e with the clock c and the lease clients of
// leaseOf, as service.Start builds the TickerEnv, and gives the ticker of sync-partner-orders.
func startWith(t *testing.T, e *env, c *clock, leaseOf func(name string, ttl time.Duration) (*lease.Client, error)) *instance {
	t.Helper()
	bindOnce.Do(func() {
		syncorders.Behavior = func(ctx context.Context, tick time.Time, h *workers.SyncPartnerOrdersHandle) error {
			id := h.Tenant().ID()
			calls <- call{tenant: id, due: tick}
			if b := bodyFor(id); b != nil {
				return b(ctx, id, h)
			}
			return nil
		}
	})
	env := service.TickerEnv{Runtime: e.rt, Clock: c,
		Tenants: func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, e.pool, "public") },
		Lease:   leaseOf,
	}
	rec := newRecorder(t)
	signals := &signalled{skipped: make(chan struct{}, 64), failed: make(chan probe.WorkerOutcome, 64)}
	env.Observe = func(name string) command.WorkerObserver {
		return signalledObserver{WorkerObserver: rec.Worker(name), signals: signals}
	}
	tickers, err := runners.Tickers(runners.BoundFills())(env)
	must(t, err)
	if len(tickers) != 1 || tickers[0].Name() != worker {
		t.Fatalf("the runners give %d tickers, want the one of %s", len(tickers), worker)
	}
	in := &instance{t: t, e: e, clock: c, ticker: tickers[0], rec: rec, signals: signals, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(e.ctx)
	go func() {
		defer close(in.done)
		_ = in.ticker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		finish, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = in.ticker.Finish(finish)
		<-in.done
		drain()
	})
	return in
}

// drain forgets the calls that no check read.
func drain() {
	for {
		select {
		case <-calls:
		default:
			return
		}
	}
}

// armed waits until the instance's scheduler waits for the next due time of the clock.
func (in *instance) armed() {
	in.t.Helper()
	at := in.clock.nextDue()
	for deadline := time.Now().Add(10 * time.Second); !in.clock.due(at); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			in.t.Fatalf("the scheduler does not wait for the due time %v", at)
		}
	}
}

// tick waits until the instance's scheduler waits for the next due time and moves the clock to it.
func (in *instance) tick() {
	in.t.Helper()
	in.armed()
	in.clock.move(in.clock.nextDue().Sub(in.clock.Now()))
}

// runTick moves the instance's clock to the next due time once the scheduler waits for it, and gives the call of
// the fill that the tick makes. A tick that comes while the run of the tick before still ends is skipped
// (ST-SUP-RC35-OC04), so it moves the clock to the next due time again. It has no deadline: a scheduler that never
// waits ends the check at the timeout of go test, and a missing result is not a pass.
func (in *instance) runTick() call {
	for {
		due := in.clock.nextDue()
		first(clockWait{in.clock, due})
		in.clock.advance(due.Sub(in.clock.Now()))
		select {
		case r := <-calls:
			return r
		case <-in.signals.skipped:
		}
	}
}

// next gives the next call of the fill.
func next(t *testing.T) call {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("the fill was not called")
		return call{}
	}
}

// none checks that the fill is not called for a moment.
func none(t *testing.T) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the fill was called for %s at %v", c.tenant, c.due)
	case <-time.After(300 * time.Millisecond):
	}
}

// stageReply runs the command reserve as a caller of the tenant, whose fill stages the message payload for
// partner-replies on the order of the tenant, as the command reserve of the declaration does, and gives the ID of
// the message.
func stageReply(t *testing.T, e *env, tenantID, payload string) uuid.UUID {
	t.Helper()
	var target tenant.Tenant
	tenants, err := tenant.List(e.ctx, e.pool, "public")
	must(t, err)
	for _, tn := range tenants {
		if tn.ID() == tenantID {
			target = tn
		}
	}
	if target.IsZero() {
		t.Fatalf("the tenant list does not hold %s", tenantID)
	}
	caller, err := authz.System("reserve-caller")
	must(t, err)
	reserve, err := command.NewCommand(command.CommandSpec[struct{}]{Name: "reserve", Permission: "reserve",
		Resource: &command.Resource{Type: "service", Service: "orders"}, Writes: []command.Declared{workers.OrderEntity},
		Probe:        func(*probe.Probe) struct{} { return struct{}{} },
		Destinations: []command.Destination{{Name: "partner-replies", MustLand: true, Idempotent: true, Consumed: true, Effects: []string{"reserved"}}}})
	must(t, err)
	_, err = command.Run(e.ctx, e.rt, reserve, command.Request{Tenant: target, Principal: caller, RequestID: uuid.NewString()},
		func([]byte) (struct{}, error) { return struct{}{}, nil },
		func(ctx context.Context, _ struct{}, h *command.Handle[struct{}]) (idempotency.Response, error) {
			orders, err := command.Repo(h, workers.OrderEntity)
			if err != nil {
				return idempotency.Response{}, err
			}
			loaded, err := orders.Get(ctx, order)
			if err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{Status: http.StatusOK}, h.Effects().Stage(ctx, "partner-replies", "reserved", loaded, json.RawMessage(payload))
		})
	must(t, err)
	var id uuid.UUID
	must(t, e.admin.QueryRow(`SELECT id FROM stego_outbox.messages WHERE destination = 'partner-replies' AND tenant_id = $1
 ORDER BY sequence DESC LIMIT 1`, tenantID).Scan(&id))
	return id
}

// awaitMessage waits until the message has the state and the attempts, with no deadline; a state that is neither
// on the way nor the one awaited fails the check at once.
func awaitMessage(t *testing.T, e *env, id uuid.UUID, state string, attempts int64) {
	t.Helper()
	for {
		var got string
		var n int64
		must(t, e.admin.QueryRow(`SELECT state, attempts FROM stego_outbox.messages WHERE id = $1`, id).Scan(&got, &n))
		switch {
		case got == state && n == attempts:
			return
		case got != "pending" && got != "dispatching" && got != state:
			t.Fatalf("the message %s is %s after %d attempts, want %s after %d", id, got, n, state, attempts)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// committed waits until the order of the tenant has the status that a committed run wrote. It has no deadline.
func committed(t *testing.T, e *env, tenantID, status string) {
	t.Helper()
	for e.status(tenantID) != status {
		time.Sleep(5 * time.Millisecond)
	}
}

// noCall checks that no call of the fill is waiting to be read. A check calls it only after it proved that no
// party can call the fill: each run of the tick committed, and each other instance waits for a clock that the
// check does not move.
func noCall(t *testing.T) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the fill was called for %s at %v", c.tenant, c.due)
	default:
	}
}

// eventually waits until ok holds.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s: %s", what)
		}
	}
}

// failed gives the outcomes of the worker.run.failed emissions, and fails the check on an emission with other
// fields than the worker, the outcome and a fixed cause class (ST-SUP-RC35-OC06).
func (in *instance) failed(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, em := range in.rec.Emissions("worker.run.failed") {
		if len(em.Fields) != 3 || em.Fields[0].Name != "worker" || em.Fields[0].Value != worker || em.Fields[1].Name != "outcome" ||
			em.Fields[2].Name != "cause" || em.Fields[2].Value == "unknown" || !slices.Contains(probe.WorkerCauses, em.Fields[2].Value) {
			t.Fatalf("worker.run.failed has the fields %v, want the worker %s, the outcome and a fixed cause class only", em.Fields, worker)
		}
		out = append(out, em.Fields[1].Value)
	}
	return out
}

// counted gives the count of the built-in counter name of the worker: each emission adds one.
func (in *instance) counted(name string) int {
	return len(in.rec.Emissions(name))
}

// write sets the status of the order of the tenant through the handle of a fill of either worker.
func write[P any](ctx context.Context, h *command.Handle[P], status string) error {
	orders, err := workers.OrderRepository(h)
	if err != nil {
		return err
	}
	loaded, err := orders.Get(ctx, order)
	if err != nil {
		return err
	}
	value := loaded.Value()
	value.Status = status
	_, err = orders.Update(ctx, loaded, value)
	return err
}

// tenants adds n tenants with a prefix of their own to the tenant list, each with the order of the check, so the fill
// can tell the tenants of one check apart, and retires them when the check ends, so no later check's tick visits
// them. The check runs with t, and the lease of the worker is free again after it.
func (e *env) tenants(t *testing.T, n int) (string, []string) {
	t.Helper()
	e.t = t
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	prefix := "w" + hex.EncodeToString(suffix) + "-"
	ids := make([]string, n)
	for i := range ids {
		ids[i] = prefix + "t" + string(rune('1'+i))
		e.exec(`INSERT INTO stego_tenants (tenant_id) VALUES ($1)`, ids[i])
		e.exec(`INSERT INTO orders (tenant_id, id, status, synced) VALUES ($1, $2, 'new', false)`, ids[i], order)
	}
	t.Cleanup(func() {
		e.exec(`UPDATE stego_tenants SET retired_at = now() WHERE starts_with(tenant_id, $1)`, prefix)
		e.exec(`UPDATE leases SET holder = '', expires_at = '-infinity' WHERE name = $1`, service.WorkerLeasePrefix+worker)
		setBody(prefix, nil)
	})
	return prefix, ids
}

// reset sets the order of each tenant of the prefix back to the status new.
func (e *env) reset(prefix string) {
	e.exec(`UPDATE orders SET status = 'new' WHERE starts_with(tenant_id, $1)`, prefix)
}

// collector is a fake OTLP trace collector on TLS, to which the telemetry runtime of a check exports.
type collector struct {
	tracecollector.UnimplementedTraceServiceServer
	mu    sync.Mutex
	spans []*tracepb.Span
}

func (c *collector) Export(_ context.Context, request *tracecollector.ExportTraceServiceRequest) (*tracecollector.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			c.spans = append(c.spans, scope.Spans...)
		}
	}
	return &tracecollector.ExportTraceServiceResponse{}, nil
}

// startTelemetry starts the fake collector and the telemetry runtime of the service that exports to it.
func startTelemetry(t *testing.T) (*otel.Runtime, *collector) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "STEGO_OTEL_") {
			t.Setenv(name, "")
		}
	}
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	must(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	c := &collector{}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: server.TLS.Certificates})))
	tracecollector.RegisterTraceServiceServer(grpcServer, c)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	telemetry, err := otel.New("orders", command.SpanNames()...)
	must(t, err)
	return telemetry, c
}

// workerSpans gives the attributes of each exported span of the name that started at since or later, in the order
// of their start, as key=value texts.
func (c *collector) workerSpans(name string, since uint64) [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	spans := append([]*tracepb.Span(nil), c.spans...)
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartTimeUnixNano < spans[j].StartTimeUnixNano })
	var out [][]string
	for _, span := range spans {
		if span.Name != name || span.StartTimeUnixNano < since {
			continue
		}
		var attrs []string
		for _, kv := range span.Attributes {
			attrs = append(attrs, kv.Key+"="+value(kv.Value))
		}
		sort.Strings(attrs)
		out = append(out, attrs)
	}
	return out
}

func value(v *commonpb.AnyValue) string {
	switch x := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	}
	return v.String()
}

func TestWorker(t *testing.T) {
	telemetry, spans := startTelemetry(t)
	e := newEnv(t, telemetry)
	t.Run("ST-SUP-RC35-OC04-T01 An interval worker runs once for each tenant a tick and skips an overlapping tick", func(t *testing.T) {
		prefix, ids := e.tenants(t, 2)
		c := newClock()
		// The blocked run does not end with its context, so the tick lasts past the next due time.
		release := make(chan struct{})
		var releaseOnce sync.Once
		var blocking atomic.Bool
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			if blocking.Load() && id == ids[0] {
				<-release
			}
			return write(ctx, h, "tick")
		})
		in := start(t, e, c, "")
		// The cleanup of the instance waits for the blocked run, so the release comes first.
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		in.tick()
		first, second := next(t), next(t)
		if first.tenant != ids[0] || second.tenant != ids[1] || first.due.UnixNano()%interval.Nanoseconds() != 0 || !first.due.Equal(second.due) {
			t.Fatalf("the first tick called the fill with %+v and %+v, want each tenant once at a multiple of the interval", first, second)
		}
		eventually(t, "each tenant's run commits on its own", func() bool { return e.status(ids[0]) == "tick" && e.status(ids[1]) == "tick" })
		// The next tick starts with the first tenant, the one after the last tenant reached; its run blocks, so the
		// due time after it comes while the tick runs, and is skipped.
		blocking.Store(true)
		in.tick()
		if r := next(t); r.tenant != ids[0] {
			t.Fatalf("the second tick called the fill for %s first, want %s", r.tenant, ids[0])
		}
		in.tick()
		in.armed()
		none(t)
		if n := in.counted("worker.tick.skipped"); n != 1 {
			t.Fatalf("worker.tick.skipped counted %d, want 1", n)
		}
		releaseOnce.Do(func() { close(release) })
	})

	t.Run("ST-SUP-RC35-OC05-T01 Of two instances only the lease holder runs a single worker", func(t *testing.T) {
		// The check does not depend on time: the ticks of each instance are on a clock of its own, which the check
		// moves, and the lease of both instances is on one more clock, which only the check moves, so no renew, retry
		// or expiry happens between two steps of the check. Each step waits for the wait that the next one needs,
		// with no deadline (ST-SUP-RC35-OC05).
		prefix, ids := e.tenants(t, 1)
		setBody(prefix, func(ctx context.Context, _ string, h *workers.SyncPartnerOrdersHandle) error {
			return write(ctx, h, "synced")
		})
		ticks1, ticks2, leases := newClock(), newClock(), newLeaseClock()
		both := func(d time.Duration) {
			ticks1.advance(d)
			ticks2.advance(d)
		}
		startLeased(t, e, ticks1, leases, "h1")
		// h1 holds the lease: its scheduler waits for the due time only after h1 took the lease row.
		due, renew := ticks1.nextDue(), leases.Now().Add(leaseRenew)
		first(clockWait{ticks1, due})
		var holder string
		must(t, e.admin.QueryRow(`SELECT holder FROM leases WHERE name = $1`, service.WorkerLeasePrefix+worker).Scan(&holder))
		if holder != "h1" {
			t.Fatalf("the lease stego.worker.%s is held by %q while h1 waits for a tick, want h1", worker, holder)
		}
		// h2 cannot take the lease: it waits for its retry on the lease clock, and it never waits for a tick. A
		// single worker that ticked without its lease would wait for the due time instead.
		startLeased(t, e, ticks2, leases, "h2")
		if got := first(clockWait{leases, leases.Now().Add(leaseRetry)}, clockWait{ticks2, due}); !slices.Equal(got, []int{0}) {
			t.Fatalf("the waits of h2 are %v of [its retry of the lease, a tick]: h2 waits for a tick without the lease", got)
		}
		both(due.Sub(ticks1.Now()))
		if r := <-calls; r.tenant != ids[0] || !r.due.Equal(due) {
			t.Fatalf("the fill was called for %s at %v, want %s at %v", r.tenant, r.due, ids[0], due)
		}
		// The run of h1 commits, and its tick of one tenant ends. h2 never waited on its tick clock, so it is still
		// in the wait for its retry of the lease, which the lease clock has not reached, and it can call no fill.
		committed(t, e, ids[0], "synced")
		if n := ticks2.waits.Load(); n != 0 {
			t.Fatalf("the instance h2 waited %d times on its tick clock without the lease", n)
		}
		noCall(t)

		// h2 takes the row: when the lease clock moves, the renew of h1 fails, so h1 stops ticking and waits to take
		// the lease again, and h2 takes the lease at its retry and waits for the next due time.
		e.reset(prefix)
		e.exec(`UPDATE leases SET holder = 'h2', token = token + 1, expires_at = now() + interval '1 hour' WHERE name = $1`, service.WorkerLeasePrefix+worker)
		first(clockWait{leases, renew})
		leases.advance(leaseRenew)
		next := due.Add(interval)
		first(clockWait{ticks1, ticks1.Now().Add(leaseRetry)})
		first(clockWait{ticks2, next})
		must(t, e.admin.QueryRow(`SELECT holder FROM leases WHERE name = $1`, service.WorkerLeasePrefix+worker).Scan(&holder))
		if holder != "h2" {
			t.Fatalf("the lease stego.worker.%s is held by %q after the renew of h1 failed, want h2", worker, holder)
		}
		both(next.Sub(ticks1.Now()))
		if r := <-calls; r.tenant != ids[0] || !r.due.Equal(next) {
			t.Fatalf("the fill was called for %s at %v, want %s at %v", r.tenant, r.due, ids[0], next)
		}
		// The run of h2 commits. h1 tried the lease again at its retry and waits for the next retry on the lease
		// clock, which the check does not move, so it can call no fill.
		committed(t, e, ids[0], "synced")
		first(clockWait{leases, leases.Now().Add(leaseRetry)})
		noCall(t)
	})

	t.Run("ST-SUP-RC35-OC06-T01 A staged message reaches its consuming worker once, and a failed run is retried", func(t *testing.T) {
		prefix, ids := e.tenants(t, 2)
		setBody(prefix, nil)
		// The queue of the outbox worker is on a test clock a minute after the system time, so each staged message is
		// due; a retry is due only when the check moves the clock.
		queueClock := &clock{now: time.Now().Add(time.Minute)}
		queue, err := outbox.New(e.workerDB, outbox.Options{Clock: queueClock})
		must(t, err)
		config := outbox.DefaultWorkerConfig()
		config.Concurrency, config.PollInterval = 1, 10*time.Millisecond
		config.RetryMin, config.RetryMax, config.AttemptTimeout, config.LeaseDuration = time.Second, 2*time.Second, 10*time.Second, time.Minute
		worker, err := outbox.NewWorker(queue, config)
		must(t, err)
		rec := newRecorder(t)
		must(t, runners.Consumers(runners.BoundFills())(e.rt, worker, func(name string) command.WorkerObserver { return rec.Worker(name) }))
		// The generated code registers one handler of partner-replies, idempotent and must land.
		var registered []outbox.Registration
		for _, r := range worker.Registrations() {
			if r.Name == "partner-replies" {
				registered = append(registered, r)
			}
		}
		if len(registered) != 1 || !registered[0].Idempotent || !registered[0].MustLand {
			t.Fatalf("the consumers register %+v for partner-replies, want one handler, idempotent and must land", registered)
		}
		// The other destinations of the check's commands complete at once.
		for _, name := range []string{"audit-sink", "authz"} {
			must(t, worker.Register(outbox.Destination{Name: name, Idempotent: true,
				Handler: func(context.Context, *sql.Tx, outbox.Delivery) error { return nil }}))
		}
		type applied struct{ tenant, message string }
		got := make(chan applied, 16)
		var failing atomic.Bool
		applyreplies.Behavior = func(ctx context.Context, message json.RawMessage, _ string, h *workers.ApplyPartnerRepliesHandle) error {
			id := h.Tenant().ID()
			if !strings.HasPrefix(id, prefix) {
				return nil
			}
			got <- applied{tenant: id, message: string(message)}
			if err := write(ctx, h, "applied "+string(message)); err != nil {
				return err
			}
			if failing.Load() {
				return errors.New("the partner refused the reply")
			}
			return nil
		}
		defer func() { applyreplies.Behavior = nil }()
		ctx, cancel := context.WithCancel(e.ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = worker.Run(ctx) }()
		defer func() { cancel(); <-done }()

		// The command reserve stages one message for partner-replies in t1.
		first := stageReply(t, e, ids[0], `{"n":1}`)
		if r := <-got; r.tenant != ids[0] || r.message != `{"n":1}` {
			t.Fatalf("the consumer got %+v, want the message of t1", r)
		}
		awaitMessage(t, e, first, "done", 1)
		if e.status(ids[0]) != `applied {"n":1}` || e.status(ids[1]) != "new" {
			t.Fatalf("the orders hold %q and %q, want the write of the run in t1 only", e.status(ids[0]), e.status(ids[1]))
		}
		noApply := func() {
			t.Helper()
			select {
			case r := <-got:
				t.Fatalf("the consumer got %+v again", r)
			default:
			}
		}
		noApply()

		// A run that fails rolls back: the message is pending again, not acted and never outcome unknown, and it is
		// not delivered before its retry is due on the queue clock.
		failing.Store(true)
		second := stageReply(t, e, ids[0], `{"n":2}`)
		if r := <-got; r.message != `{"n":2}` {
			t.Fatalf("the consumer got %+v, want the second message", r)
		}
		awaitMessage(t, e, second, "pending", 1)
		var due time.Time
		must(t, e.admin.QueryRow(`SELECT available_at FROM stego_outbox.messages WHERE id = $1`, second).Scan(&due))
		if !due.After(queueClock.Now()) {
			t.Fatalf("the failed message is due again at %v, not after the queue time %v: no backoff", due, queueClock.Now())
		}
		if e.status(ids[0]) != `applied {"n":1}` {
			t.Fatalf("the failed run left the order at %q, want the write of the first run only", e.status(ids[0]))
		}
		noApply()
		// When its retry is due, the run commits and the message is delivered.
		failing.Store(false)
		queueClock.advance(due.Sub(queueClock.Now()))
		if r := <-got; r.message != `{"n":2}` {
			t.Fatalf("the retry got %+v, want the second message", r)
		}
		awaitMessage(t, e, second, "done", 2)
		if e.status(ids[0]) != `applied {"n":2}` {
			t.Fatalf("the retried run left the order at %q, want its write", e.status(ids[0]))
		}
		var unknown int
		must(t, e.admin.QueryRow(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'partner-replies' AND state = 'outcome_unknown'`).Scan(&unknown))
		if unknown != 0 {
			t.Fatalf("%d messages of partner-replies are in outcome unknown, want none", unknown)
		}
		noApply()
	})

	t.Run("ST-SUP-RC35-OC07-T01 A slow tenant ends at its own deadline and the next tenant gets its own", func(t *testing.T) {
		prefix, ids := e.tenants(t, 2)
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			if err := write(ctx, h, "slow"); err != nil {
				return err
			}
			if id == ids[0] {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
		c := newClock()
		in := start(t, e, c, "")
		in.tick()
		if r := next(t); r.tenant != ids[0] {
			t.Fatalf("the tick started with %s", r.tenant)
		}
		none(t)
		c.move(31 * time.Second)
		if r := next(t); r.tenant != ids[1] {
			t.Fatalf("the second run of the tick was %s", r.tenant)
		}
		eventually(t, "the run of the second tenant commits", func() bool { return e.status(ids[1]) == "slow" })
		eventually(t, "the run of the first tenant is reported", func() bool { return len(in.failed(t)) == 1 })
		if got := in.failed(t); got[0] != "rolled_back" || e.status(ids[0]) != "new" {
			t.Fatalf("the slow run is reported %v and left %s, want rolled_back and no change", got, e.status(ids[0]))
		}
	})

	t.Run("ST-SUP-RC35-OC08-T01 A panic in one worker run does not stop the service or the next run", func(t *testing.T) {
		prefix, ids := e.tenants(t, 2)
		var panicking atomic.Bool
		panicking.Store(true)
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			if err := write(ctx, h, "panic"); err != nil {
				return err
			}
			if panicking.Load() && id == ids[0] {
				panic("secret-7")
			}
			return nil
		})
		c := newClock()
		in := start(t, e, c, "")
		in.tick()
		next(t)
		next(t)
		eventually(t, "the run of the second tenant commits", func() bool { return e.status(ids[1]) == "panic" })
		eventually(t, "the panicked run is reported", func() bool { return len(in.failed(t)) == 1 })
		if got := in.failed(t); got[0] != "panicked" || e.status(ids[0]) != "new" {
			t.Fatalf("the panicked run is reported %v and left %s, want panicked and no change", got, e.status(ids[0]))
		}
		panicking.Store(false)
		e.reset(prefix)
		in.tick()
		next(t)
		next(t)
		eventually(t, "the next tick commits for both tenants", func() bool { return e.status(ids[0]) == "panic" && e.status(ids[1]) == "panic" })
	})

	t.Run("ST-SUP-RC35-OC10-T01 Shutdown starts no run, waits for the drain bound and rolls back the rest", func(t *testing.T) {
		prefix, ids := e.tenants(t, 2)
		release := make(chan struct{})
		var blockAll atomic.Bool
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			if err := write(ctx, h, "drain"); err != nil {
				return err
			}
			if blockAll.Load() {
				<-ctx.Done()
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
		c := newClock()
		in := start(t, e, c, "")
		in.tick()
		next(t)
		finished := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished <- in.ticker.Finish(ctx)
		}()
		time.Sleep(100 * time.Millisecond)
		close(release)
		if err := <-finished; err != nil {
			t.Fatalf("the drain gave %v, want the running run to finish in the bound", err)
		}
		none(t)
		if e.status(ids[0]) != "drain" || e.status(ids[1]) != "new" || in.counted("worker.tenant.unreached") != 1 {
			t.Fatalf("after the drain the tenants hold %s and %s, with %d unreached, want the run of %s committed and no run of %s",
				e.status(ids[0]), e.status(ids[1]), in.counted("worker.tenant.unreached"), ids[0], ids[1])
		}
		c.move(interval)
		none(t)

		blockAll.Store(true)
		e.reset(prefix)
		blocked := start(t, e, c, "")
		blocked.tick()
		next(t)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := blocked.ticker.Finish(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the drain gave %v, want the end of its bound", err)
		}
		if e.status(ids[0]) != "new" {
			t.Fatal("the run that outlived the drain bound committed")
		}
		c.move(interval)
		none(t)
	})

	t.Run("ST-SUP-RC35-OC12-T01 A tick that runs out of time counts the unreached tenants, and the next starts from them", func(t *testing.T) {
		prefix, ids := e.tenants(t, 3)
		c := newClock()
		var slow atomic.Bool
		slow.Store(true)
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			if slow.Load() {
				switch id {
				case ids[0]:
					c.move(40 * time.Second)
				case ids[1]:
					c.move(25 * time.Second)
				}
			}
			return nil
		})
		in := start(t, e, c, "")
		in.tick()
		first := []string{next(t).tenant, next(t).tenant}
		eventually(t, "the tick ends", func() bool { return in.counted("worker.tenant.unreached") > 0 })
		none(t)
		if n := in.counted("worker.tenant.unreached"); n != 1 || first[0] != ids[0] || first[1] != ids[1] {
			t.Fatalf("the first tick ran %v and counted %d unreached, want %s and %s and one", first, n, ids[0], ids[1])
		}
		slow.Store(false)
		in.tick()
		second := []string{next(t).tenant, next(t).tenant, next(t).tenant}
		if second[0] != ids[2] {
			t.Fatalf("the second tick ran %v, want %s first", second, ids[2])
		}
		in.tick()
		if third := next(t).tenant; third != ids[2] {
			t.Fatalf("the third tick started with %s, want %s, the tenant after the last one the second tick reached", third, ids[2])
		}
	})
	t.Run("ST-SUP-RC35-OC09-T01 A worker run is one span with a fixed outcome, and a failure is one built-in event", func(t *testing.T) {
		prefix, ids := e.tenants(t, 1)
		since := uint64(time.Now().UnixNano())
		var end atomic.Value
		setBody(prefix, func(ctx context.Context, id string, h *workers.SyncPartnerOrdersHandle) error {
			switch end.Load() {
			case "error":
				return errors.New("secret-7")
			case "panic":
				panic("secret-7")
			}
			return write(ctx, h, "secret-7")
		})
		// The check does not depend on time (as ST-SUP-RC35-OC05-T01): the ticks are on a clock that only the check
		// moves, in one step to each due time, and the lease is on a clock of its own that the check never moves, so
		// no renew, retry or expiry happens while the check runs. Each step waits for the event that the next one
		// needs, with no deadline: the scheduler's wait for the due time, the call of the fill, the commit or the
		// failed run of the instance.
		ticks, leases := newClock(), newLeaseClock()
		in := startLeased(t, e, ticks, leases, "h1")
		for _, ending := range []string{"nil", "error", "panic"} {
			end.Store(ending)
			if r := in.runTick(); r.tenant != ids[0] {
				t.Fatalf("the fill was called for %s, want %s", r.tenant, ids[0])
			}
			switch ending {
			case "nil":
				committed(t, e, ids[0], "secret-7")
			default:
				<-in.signals.failed
			}
		}
		if got := in.failed(t); len(got) != 2 || got[0] != "rolled_back" || got[1] != "panicked" {
			t.Fatalf("worker.run.failed reported %v, want rolled_back and panicked", got)
		}
		in.ticker.Stop()
		// Close exports the spans that the runtime holds; the spans of the earlier checks started before this one.
		telemetry.Close()
		var outcomes []string
		for _, attrs := range spans.workerSpans(command.WorkerSpanName, since) {
			text := strings.Join(attrs, " ")
			if strings.Contains(text, "secret-7") || !strings.Contains(text, "stego.worker="+worker) {
				t.Errorf("the span of a run has the attributes %s, want stego.worker=%s and no value of the fill", text, worker)
			}
			for _, kv := range attrs {
				if after, ok := strings.CutPrefix(kv, "stego.outcome="); ok {
					outcomes = append(outcomes, after)
				}
			}
		}
		if strings.Join(outcomes, " ") != "committed rolled_back panicked" {
			t.Fatalf("the spans %s of the runs have the outcomes %v, want committed, rolled_back and panicked", command.WorkerSpanName, outcomes)
		}
	})
}
