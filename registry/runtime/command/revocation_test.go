package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/910-software-solutions/stego/registry/runtime/audit"
	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/idempotency"
	"github.com/910-software-solutions/stego/registry/runtime/internal/authzclock"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
	"github.com/910-software-solutions/stego/registry/runtime/outbox"
	"github.com/910-software-solutions/stego/registry/runtime/probe"
	"github.com/910-software-solutions/stego/registry/runtime/query"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// deliveredPort is a relationships port of the relay that records the resources of the tuples it wrote, and a
// decision port that allows a resource only when a tuple of it was written: a check sees a create only after the
// relay delivered its tuples, as at SpiceDB.
type deliveredPort struct {
	mu        sync.Mutex
	delivered map[string]bool
	n         int
}

func (d *deliveredPort) write(touches []authz.Tuple) (authz.Token, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delivered == nil {
		d.delivered = map[string]bool{}
	}
	for _, tuple := range touches {
		d.delivered[tuple.Resource.ID] = true
	}
	d.n++
	return authz.NewToken("t-" + strings.Repeat("9", d.n))
}

func (d *deliveredPort) Touch(_ context.Context, tuples []authz.Tuple, _ []authz.Precondition) (authz.Token, error) {
	return d.write(tuples)
}

func (d *deliveredPort) Delete(context.Context, []authz.Filter, []authz.Precondition) (authz.Token, error) {
	return d.write(nil)
}

func (d *deliveredPort) Write(_ context.Context, touches []authz.Tuple, _ []authz.Filter, _ []authz.Precondition) (authz.Token, error) {
	return d.write(touches)
}

func (d *deliveredPort) allowed(r authz.Resource) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delivered[r.ID] || r.Type == "service"
}

func (d *deliveredPort) Evaluate(_ context.Context, r authz.EvaluationRequest) (authz.EvaluationResult, error) {
	return authz.EvaluationResult{Allowed: d.allowed(r.Resource)}, nil
}

func (d *deliveredPort) EvaluateBatch(_ context.Context, b authz.BatchRequest) ([]authz.EvaluationResult, error) {
	results := make([]authz.EvaluationResult, len(b.Evaluations))
	for i, r := range b.Evaluations {
		results[i] = authz.EvaluationResult{Allowed: d.allowed(r.Resource)}
	}
	return results, nil
}

func (d *deliveredPort) Search(context.Context, authz.SearchRequest) (authz.SearchResult, error) {
	return authz.SearchResult{}, authz.ErrNoSearch
}

func (d *deliveredPort) Searches() bool { return false }

// revocationEnv is the env of the revocation tests: the authorization runtime with the wait cap and the waiter
// limit of the test over the delivered port, the command runtime over it, and the verified identities of two
// principals in two tenants.
type revocationEnv struct {
	*env
	port     *deliveredPort
	az       *authz.Runtime
	rt       *Runtime
	orders   *Entity[placedOrder]
	place    *Command[testProbe]
	identity map[string]context.Context // "subject@tenant": a context with the verified identity
}

func newRevocationEnv(t *testing.T, waitCap time.Duration, waiters int) *revocationEnv {
	t.Helper()
	return newRevocationEnvOf(t, waitCap, waiters, 0)
}

// newRevocationEnvOf is newRevocationEnv with the waiter limit of a tenant tenantWaiters.
func newRevocationEnvOf(t *testing.T, waitCap time.Duration, waiters, tenantWaiters int) *revocationEnv {
	t.Helper()
	return newRevocationEnvClock(t, waitCap, waiters, tenantWaiters, nil)
}

// newRevocationEnvClock is newRevocationEnvOf with the wait clock of the authorization runtime; nil is the real clock.
func newRevocationEnvClock(t *testing.T, waitCap time.Duration, waiters, tenantWaiters int, clock authzclock.WaitClock) *revocationEnv {
	t.Helper()
	e := newEnv(t)
	r := &revocationEnv{env: e, port: &deliveredPort{}, identity: map[string]context.Context{}}
	cfg := authz.Config{Provider: r.port, DB: e.pool, Schema: "authz", Outbox: e.queue,
		Poll: 10 * time.Millisecond, WaitCap: waitCap, Waiters: waiters, TenantWaiters: tenantWaiters}
	// The wait clock reaches the configuration through the internal test hook of the module (review of PR #387, L1).
	if !authzclock.Set(&cfg, authzclock.Clocks{Wait: clock}) {
		t.Fatal("the authz package registers no clock setter")
	}
	var err error
	if r.az, err = authz.New(cfg); err != nil {
		t.Fatal(err)
	}
	r.rt = e.runtime(func(c *Config) { c.Authz = r.az })
	_, orders, _ := referenceEntities(e)
	s := orders.spec
	s.Parent = nil
	s.Relationships = []Relationship[placedOrder]{{Relation: "owner", SubjectType: "user"}}
	if r.orders, err = NewEntity(s); err != nil {
		t.Fatal(err)
	}
	r.place = e.command("place", func(s *CommandSpec[testProbe]) {
		s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
	}, r.orders)
	key := authntest.NewKey(t, "k1")
	set, err := authn.ParseKeys(authntest.Document(key.JWK()))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS",
		Keys: set, TenantClaim: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(verifier.Stop)
	for _, who := range []string{"u-7@t1", "u-8@t1", "u-7@t2"} {
		subject, tenantID, _ := strings.Cut(who, "@")
		claims := authntest.Claims("https://issuer.example", "orders", subject, time.Now())
		claims["tenant"] = tenantID
		ctx, err := verifier.Authenticate(context.Background(), "Bearer "+authntest.Mint(t, authntest.Header("k1"), claims, key))
		if err != nil {
			t.Fatal(err)
		}
		r.identity[who] = ctx
	}
	return r
}

// waitClock is a wait clock of WaitFor that the test moves itself (ST-SUP-RP06-LC24). Each wait bound that it gives
// is sent on bounds and ends when the test moves the clock to its end, or when its parent ends. Its poll timers never
// fire, so a wait reads again only when the test lets it; a wait that asks for a poll timer before its bound fails the
// test, because it waits with no bound.
type waitClock struct {
	t      *testing.T
	mu     sync.Mutex
	now    time.Duration
	open   []*waitBound
	bounds chan *waitBound
	// asked counts the bounds that the clock gave; a poll timer asked when the clock gave no bound has no bound, and
	// is sent on unbounded.
	asked     int
	unbounded chan struct{}
	// polls receives one value each time a bounded wait asks for its poll timer: its read ended and it waits.
	polls chan struct{}
}

// waitBound is one wait bound that the clock gave.
type waitBound struct {
	d, at time.Duration
	ctx   context.Context
	end   context.CancelFunc
}

func newWaitClock(t *testing.T) *waitClock {
	c := &waitClock{t: t, bounds: make(chan *waitBound, 64), unbounded: make(chan struct{}, 64), polls: make(chan struct{}, 1024)}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, b := range c.open {
			b.end()
		}
	})
	return c
}

func (c *waitClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, end := context.WithCancel(parent)
	c.mu.Lock()
	b := &waitBound{d: d, at: c.now + d, ctx: ctx, end: end}
	c.open = append(c.open, b)
	c.asked++
	c.mu.Unlock()
	c.bounds <- b
	return ctx, end
}

func (c *waitClock) After(time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asked == 0 {
		c.unbounded <- struct{}{}
		return nil
	}
	c.polls <- struct{}{}
	return nil
}

// advance moves the clock and ends each bound whose end it reached.
func (c *waitClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	for _, b := range c.open {
		if b.at <= c.now {
			b.end()
		}
	}
}

// statusAnswer is the answer of one status read.
type statusAnswer struct {
	code    int
	state   string
	applied string
}

// statusAsync starts a status read as who and sends its answer on the channel it returns.
func (r *revocationEnv) statusAsync(who, path, prefer string) <-chan statusAnswer {
	answers := make(chan statusAnswer, 1)
	go func() {
		answer, body, _ := r.status(who, path, prefer)
		answers <- statusAnswer{answer.Code, body["state"], answer.Header().Get("Preference-Applied")}
	}()
	return answers
}

// waitBound reads the next bound that the clock gave, or fails when the read answers first: a read that waits asks
// for its bound before it waits, and a read over a limit answers and asks for none.
func (c *waitClock) waitBound(t *testing.T, answers <-chan statusAnswer) *waitBound {
	t.Helper()
	select {
	case b := <-c.bounds:
		return b
	case a := <-answers:
		t.Fatalf("the read answered %+v and did not wait", a)
		return nil
	case <-c.unbounded:
		t.Fatal("the read waits for its poll timer with no wait bound")
		return nil
	}
}

// polled waits until a wait asks for its poll timer, so its read found the revocation pending and it waits now; it
// fails when the read answers first.
func (c *waitClock) polled(t *testing.T, answers <-chan statusAnswer) {
	t.Helper()
	select {
	case <-c.polls:
	case a := <-answers:
		t.Fatalf("the read answered %+v before it waited", a)
	}
}

// pending fails when the read answered: its bound has not ended.
func pending(t *testing.T, answers <-chan statusAnswer, why string) {
	t.Helper()
	select {
	case a := <-answers:
		t.Fatalf("%s: the read answered %+v before its bound ended", why, a)
	default:
	}
}

// bound is the verified tenant and principal of who.
func (r *revocationEnv) bound(who string) Bound {
	r.t.Helper()
	b, err := BindIdentity(r.identity[who], "user")
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// create runs the place command as who with key, which creates the order id when id is not empty, and returns the
// result.
func (r *revocationEnv) create(who, key, id string, fill func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error)) Result {
	r.t.Helper()
	b := r.bound(who)
	req := r.request(b.Tenant.ID(), b.Principal, key, `{}`)
	if fill == nil {
		fill = fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(r.t, h, r.orders).Create(ctx, placedOrder{ID: id, CustomerID: "c1", ProductID: "p1"})
			return err
		})
	}
	result, err := r.runOn(r.rt, r.place, req, fill)
	if err != nil {
		r.t.Fatal(err)
	}
	return result
}

// startRelay runs the relay of the delivered port in an outbox worker of the worker role until the test ends.
func (r *revocationEnv) startRelay() {
	r.t.Helper()
	relay, err := authz.NewRelay(authz.RelayConfig{Name: "authz-relay", Relations: relayRelations, Relationships: r.port,
		DB: r.openPool(r.dsn(r.roles[2])), Schema: "authz"})
	if err != nil {
		r.t.Fatal(err)
	}
	config := outbox.DefaultWorkerConfig()
	config.PollInterval = 10 * time.Millisecond
	config.RetryMin = 10 * time.Millisecond
	config.RetryMax = 50 * time.Millisecond
	config.AttemptTimeout = 2 * time.Second
	worker, err := outbox.NewWorker(r.wqueue, config)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := worker.Register(relay.Destination()); err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	r.t.Cleanup(func() { cancel(); <-done })
}

// status reads path through the status route as who, or with no identity when who is empty, with the Prefer header
// prefer when it is not empty.
func (r *revocationEnv) status(who, path, prefer string) (*httptest.ResponseRecorder, map[string]string, time.Duration) {
	r.t.Helper()
	ctx := context.Background()
	if who != "" {
		ctx = r.identity[who]
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if prefer != "" {
		request.Header.Set("Prefer", prefer)
	}
	answer := httptest.NewRecorder()
	start := time.Now()
	ServeRevocationStatus(r.rt, "user").ServeHTTP(answer, request)
	took := time.Since(start)
	body := map[string]string{}
	if answer.Code == http.StatusOK {
		if err := json.Unmarshal(answer.Body.Bytes(), &body); err != nil {
			r.t.Fatalf("the status answer is not JSON: %v: %s", err, answer.Body)
		}
	}
	return answer, body, took
}

// holdPool keeps the one connection of the main pool in a transaction for hold, so no read of the pool can start
// before then, and gives the pool back its limit when the test ends.
func (r *revocationEnv) holdPool(hold time.Duration) {
	r.t.Helper()
	r.pool.SetMaxOpenConns(1)
	tx, err := r.pool.BeginTx(context.Background(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { defer close(released); time.Sleep(hold); _ = tx.Rollback() }()
	r.t.Cleanup(func() { <-released; r.pool.SetMaxOpenConns(0) })
}

// queries is the query runtime over the authorization runtime of the env.
func (r *revocationEnv) queries() *query.Runtime {
	r.t.Helper()
	queries, err := query.New(query.Config{DB: r.pool, AuditDB: r.auditDB, Audit: r.writer,
		AuditSettings: audit.Settings{Level: audit.LevelChange}, Authz: r.az, Service: "orders"})
	if err != nil {
		r.t.Fatal(err)
	}
	return queries
}

// await runs AwaitWrite as who with the Authz-Revocation-After values after and the Prefer header prefer when it is
// not empty, and gives the response, the error and the time it took.
func (r *revocationEnv) await(queries *query.Runtime, who, prefer string, after ...string) (*httptest.ResponseRecorder, error, time.Duration) {
	r.t.Helper()
	b := r.bound(who)
	request := httptest.NewRequestWithContext(r.identity[who], http.MethodGet, "/orders", nil)
	for _, value := range after {
		request.Header.Add(authz.RevocationAfterHeader, value)
	}
	if prefer != "" {
		request.Header.Set("Prefer", prefer)
	}
	answer := httptest.NewRecorder()
	start := time.Now()
	err := queries.AwaitWrite(answer, request, b.Tenant, b.Principal)
	return answer, err, time.Since(start)
}

func TestRevocationAnswers(t *testing.T) {
	t.Run("ST-SUP-RP10-LC35-T01 A committed write that enqueued a tuple answers its revocation, and its replay is the same", func(t *testing.T) {
		r := newRevocationEnv(t, 0, 0)
		first := r.create("u-7@t1", "k-lc36-1", "o1", nil)
		location := first.Response.Header.Get(authz.RevocationLocationHeader)
		if first.Response.Header.Get(authz.RevocationHeader) != "pending" || !strings.HasPrefix(location, authz.RevocationPath) {
			t.Fatalf("the create answered %v, want Authz-Revocation: pending and a location under %s", first.Response.Header, authz.RevocationPath)
		}
		if _, err := uuid.Parse(strings.TrimPrefix(location, authz.RevocationPath)); err != nil {
			t.Fatalf("the location %s does not end in a revocation identity: %v", location, err)
		}
		// The relay delivers, and the same key gives the stored answer: still pending, byte for byte.
		r.startRelay()
		if answer, body, _ := r.status("u-7@t1", location, "wait=5"); answer.Code != http.StatusOK || body["state"] != "complete" {
			t.Fatalf("the status after the delivery is %d %v, want complete", answer.Code, body)
		}
		replay := r.create("u-7@t1", "k-lc36-1", "o1", nil)
		if !replay.Replayed || replay.Response.Header.Get(authz.RevocationHeader) != "pending" ||
			replay.Response.Header.Get(authz.RevocationLocationHeader) != location {
			t.Fatalf("the replay answered %v (replayed %v), want the stored pending answer with %s", replay.Response.Header, replay.Replayed, location)
		}
		// A write that enqueues no tuple sends neither header, also when the fill sets one.
		none := r.create("u-7@t1", "k-lc36-2", "", func(context.Context, *Handle[testProbe]) (idempotency.Response, error) {
			return idempotency.Response{Status: http.StatusOK, Header: http.Header{authz.RevocationHeader: {"pending"},
				authz.RevocationLocationHeader: {authz.RevocationPath + uuid.NewString()}}, Body: []byte("{}")}, nil
		})
		if none.Response.Header.Get(authz.RevocationHeader) != "" || none.Response.Header.Get(authz.RevocationLocationHeader) != "" {
			t.Fatalf("a write with no tuple answered %v, want no revocation header", none.Response.Header)
		}
	})
	t.Run("ST-SUP-RP10-LC35-T02 The status route answers only the writer, pending and then complete with no token", func(t *testing.T) {
		r := newRevocationEnv(t, 0, 0)
		location := r.create("u-7@t1", "k-lc36-3", "o3", nil).Response.Header.Get(authz.RevocationLocationHeader)
		if answer, body, _ := r.status("u-7@t1", location, ""); answer.Code != http.StatusOK || body["state"] != "pending" ||
			body["token"] != "" || answer.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("the status before the delivery is %d %v %v, want 200 pending with no token and no-store", answer.Code, body, answer.Header())
		}
		for _, who := range []string{"u-8@t1", "u-7@t2"} {
			if answer, _, _ := r.status(who, location, ""); answer.Code != http.StatusNotFound {
				t.Fatalf("%s read the status of u-7@t1's revocation: %d, want 404", who, answer.Code)
			}
		}
		for _, path := range []string{authz.RevocationPath + uuid.NewString(), authz.RevocationPath + "not-an-id",
			authz.RevocationPath + strings.ToUpper(strings.TrimPrefix(location, authz.RevocationPath))} {
			if answer, _, _ := r.status("u-7@t1", path, ""); answer.Code != http.StatusNotFound {
				t.Fatalf("the status of %s is %d, want 404", path, answer.Code)
			}
		}
		if answer, _, _ := r.status("", location, ""); answer.Code != http.StatusUnauthorized {
			t.Fatalf("the status with no identity is %d, want 401", answer.Code)
		}
		r.startRelay()
		answer, body, _ := r.status("u-7@t1", location, "wait=5")
		// The body holds no token of the provider, which would tell the write count of a datastore that all tenants
		// share (ST-SUP-Q119).
		if _, token := body["token"]; answer.Code != http.StatusOK || body["state"] != "complete" || token ||
			strings.Contains(answer.Body.String(), "t-9") {
			t.Fatalf("the status after the delivery is %d %s, want complete with no token", answer.Code, answer.Body)
		}
		if answer.Header().Get("Preference-Applied") != "wait=5" {
			t.Fatalf("Preference-Applied is %q, want wait=5", answer.Header().Get("Preference-Applied"))
		}
	})
}

func TestRevocationNotFound(t *testing.T) {
	t.Run("ST-SUP-RP10-LC35-T03 A wait that ends before a read found the caller's revocation answers 404, not pending", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		other := r.create("u-7@t2", "k-lc35-4", "o6", nil).Response.Header.Get(authz.RevocationLocationHeader)
		// The pool is held for 1.5 s, so the first read of a wait of 1 s cannot start before the wait ends.
		r.holdPool(1500 * time.Millisecond)
		for _, path := range []string{other, authz.RevocationPath + uuid.NewString()} {
			answer, body, took := r.status("u-7@t1", path, "wait=1")
			if answer.Code != http.StatusNotFound || took > 1400*time.Millisecond {
				t.Fatalf("u-7@t1 read %s under a held pool: %d %v after %s, want 404 when the wait ends", path, answer.Code, body, took)
			}
		}
	})
}

func TestRevocationWait(t *testing.T) {
	t.Run("ST-SUP-RP06-LC24-T01 WaitFor waits at most the wait cap and at most the waiter limit of calls wait", func(t *testing.T) {
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 400*time.Millisecond, 2, 0, clock)
		location := r.create("u-7@t1", "k-lc24-1", "o4", nil).Response.Header.Get(authz.RevocationLocationHeader)
		// The relay does not run: the cap cuts a longer wait, and Preference-Applied gives the cap.
		answers := r.statusAsync("u-7@t1", location, "wait=30")
		b := clock.waitBound(t, answers)
		if b.d != 400*time.Millisecond {
			t.Fatalf("a wait of 30 s with a cap of 0.4 s is bounded at %s, want the cap", b.d)
		}
		clock.polled(t, answers)
		clock.advance(400*time.Millisecond - time.Nanosecond)
		if b.ctx.Err() != nil {
			t.Fatal("the wait ended before the cap")
		}
		pending(t, answers, "a wait of 30 s at 1 ns before the cap")
		clock.advance(time.Nanosecond)
		a := <-answers
		if a.state != "pending" || a.applied != "wait=0" {
			t.Fatalf("a wait of 30 s with a cap of 0.4 s answered %+v, want pending with the cap in whole seconds, wait=0", a)
		}
		// Three waits at one time with a limit of two: one answers at once with no Preference-Applied, and two wait
		// for the bound of their wait.
		merged := make(chan statusAnswer, 3)
		for range 3 {
			answers := r.statusAsync("u-7@t1", location, "wait=1")
			go func() { merged <- <-answers }()
		}
		var quick []statusAnswer
		var waits []*waitBound
		for range 3 {
			select {
			case b := <-clock.bounds:
				waits = append(waits, b)
			case a := <-merged:
				quick = append(quick, a)
			}
		}
		if len(quick) != 1 || len(waits) != 2 || quick[0].applied != "" || quick[0].state != "pending" {
			t.Fatalf("three waits with a limit of two gave %v at once and %d bounds, want one pending with no Preference-Applied and two bounds", quick, len(waits))
		}
		for _, b := range waits {
			if b.d != 400*time.Millisecond {
				t.Fatalf("a wait of 1 s with a cap of 0.4 s is bounded at %s", b.d)
			}
		}
		clock.polled(t, merged)
		clock.polled(t, merged)
		pending(t, merged, "two waits before the cap")
		clock.advance(400 * time.Millisecond)
		for range 2 {
			if a := <-merged; a.applied != "wait=0" || a.state != "pending" {
				t.Fatalf("a wait answered %+v after the cap, want pending with wait=0", a)
			}
		}
	})
	t.Run("ST-SUP-RP06-LC24-T02 A waiting call holds no pooled connection", func(t *testing.T) {
		// The cap of 2 s keeps every wait open while the test samples the pool, from 0.2 s to 0.7 s.
		r := newRevocationEnv(t, 2*time.Second, 10)
		location := r.create("u-7@t1", "k-lc24-2", "o5", nil).Response.Header.Get(authz.RevocationLocationHeader)
		var wg sync.WaitGroup
		for range 5 {
			wg.Add(1)
			go func() { defer wg.Done(); r.status("u-7@t1", location, "wait=5") }()
		}
		time.Sleep(200 * time.Millisecond)
		// Between two reads a wait holds no connection. The five waits poll at the same times, so a sample can see
		// their five short reads at once; but a wait that held its connection would keep one in use at every
		// sample. So the pool must have no connection in use at some samples.
		idle, busy := 0, 0
		for range 50 {
			if n := r.pool.Stats().InUse; n == 0 {
				idle++
			} else {
				busy = max(busy, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
		wg.Wait()
		if idle == 0 {
			t.Fatalf("five waits kept a pooled connection in use at each of 50 samples (up to %d), want none between two reads", busy)
		}
	})
}

func TestTenantWaiters(t *testing.T) {
	t.Run("ST-SUP-RP06-LC24-T03 A tenant that holds its waiter places does not block the waits of another tenant", func(t *testing.T) {
		// The waiter limit of all is 100 and that of a tenant 2; no relay runs, so each wait lasts until its bound
		// ends on the clock of the test.
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 2*time.Second, 100, 2, clock)
		hog := r.create("u-7@t2", "k-lc24-3", "o7", nil).Response.Header.Get(authz.RevocationLocationHeader)
		own := r.create("u-7@t1", "k-lc24-4", "o8", nil).Response.Header.Get(authz.RevocationLocationHeader)
		var hogs []<-chan statusAnswer
		for range 2 {
			answers := r.statusAsync("u-7@t2", hog, "wait=5")
			hogs = append(hogs, answers)
			if b := clock.waitBound(t, answers); b.d != 2*time.Second {
				t.Fatalf("a wait of 5 s of t2 with a cap of 2 s is bounded at %s", b.d)
			}
			clock.polled(t, answers)
		}
		// A third wait of t2 is over the limit of its tenant: it answers at once and does not wait.
		third := r.statusAsync("u-7@t2", hog, "wait=5")
		select {
		case a := <-third:
			if a.applied != "" || a.state != "pending" {
				t.Fatalf("a third wait of t2 answered %+v, want pending at once with no Preference-Applied", a)
			}
		case b := <-clock.bounds:
			t.Fatalf("a third wait of t2 waits with a bound of %s, want an answer at once", b.d)
		}
		// A wait of t1 waits: t2 holds no place of t1.
		answers := r.statusAsync("u-7@t1", own, "wait=1")
		b := clock.waitBound(t, answers)
		if b.d != time.Second {
			t.Fatalf("a wait of 1 s of t1 is bounded at %s, want 1 s", b.d)
		}
		clock.polled(t, answers)
		clock.advance(time.Second - time.Nanosecond)
		pending(t, answers, "a wait of t1 at 1 ns before 1 s")
		clock.advance(time.Nanosecond)
		if a := <-answers; a.applied != "wait=1" || a.state != "pending" {
			t.Fatalf("a wait of t1 answered %+v, want pending after 1 s with Preference-Applied: wait=1", a)
		}
		clock.advance(time.Second)
		for _, answers := range hogs {
			if a := <-answers; a.applied != "wait=2" || a.state != "pending" {
				t.Fatalf("a wait of t2 answered %+v after its cap, want pending with wait=2", a)
			}
		}
	})
}

func TestReadYourWritesRefusals(t *testing.T) {
	t.Run("ST-SUP-RP11-LC21-T02 A revocation of another tenant or principal is unknown, and a bad header is a bad request", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		queries := r.queries()
		location := r.create("u-7@t1", "k-ryw-3", "ryw3", nil).Response.Header.Get(authz.RevocationLocationHeader)
		// A real revocation of u-7@t1 is unknown to another tenant, at once. Another principal of t1 waits for it and
		// gets unknown too (T04).
		for _, who := range []string{"u-7@t2"} {
			answer, err, took := r.await(queries, who, "wait=5", location)
			if err != nil || answer.Header().Get(authz.RevocationHeader) != query.RevocationUnknown || took > 2*time.Second {
				t.Fatalf("%s waited for u-7@t1's revocation: %v %q after %s, want unknown at once", who, err,
					answer.Header().Get(authz.RevocationHeader), took)
			}
		}
		// Two headers, and an identity in upper case, are bad requests.
		id := strings.TrimPrefix(location, authz.RevocationPath)
		for name, after := range map[string][]string{"two headers": {location, location}, "upper case": {strings.ToUpper(id)}} {
			if _, err, _ := r.await(queries, "u-7@t1", "", after...); !errors.Is(err, query.ErrBadRequest) {
				t.Fatalf("%s gave %v, want ErrBadRequest", name, err)
			}
		}
		// When the wait ends before a read found the revocation, the revocation of another tenant is unknown, not
		// pending.
		other := r.create("u-7@t2", "k-ryw-4", "ryw4", nil).Response.Header.Get(authz.RevocationLocationHeader)
		r.holdPool(1500 * time.Millisecond)
		answer, err, _ := r.await(queries, "u-7@t1", "wait=1", other)
		if err != nil || answer.Header().Get(authz.RevocationHeader) != query.RevocationUnknown {
			t.Fatalf("u-7@t1 waited for u-7@t2's revocation under a held pool: %v %q, want unknown", err, answer.Header().Get(authz.RevocationHeader))
		}
	})
	t.Run("ST-SUP-RP11-LC21-T04 A second user of the tenant who has the revocation of another user's create waits for it and gets unknown, never its state", func(t *testing.T) {
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 5*time.Second, 0, 0, clock)
		queries := r.queries()
		location := r.create("u-7@t1", "k-ryw-5", "ryw5", nil).Response.Header.Get(authz.RevocationLocationHeader)
		type awaited struct {
			answer *httptest.ResponseRecorder
			err    error
		}
		answers := make(chan awaited, 1)
		go func() {
			answer, err, _ := r.await(queries, "u-8@t1", "wait=5", location)
			answers <- awaited{answer, err}
		}()
		select {
		case <-clock.bounds:
		case a := <-answers:
			t.Fatalf("the read of u-8 with the revocation of u-7 answered %v %v and did not wait", a.err, a.answer.Header())
		}
		select {
		case <-clock.polls:
		case a := <-answers:
			t.Fatalf("the read of u-8 with the revocation of u-7 answered %v %v before it waited, want a wait for the revocation", a.err, a.answer.Header())
		}
		r.startRelay()
		maker := r.bound("u-7@t1")
		id := uuid.MustParse(strings.TrimPrefix(location, authz.RevocationPath))
		for {
			state, err := r.az.Status(context.Background(), maker.Tenant, maker.Principal, id)
			if err != nil {
				t.Fatal(err)
			}
			if state.State == authz.RevocationComplete {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		clock.advance(5 * time.Second)
		a := <-answers
		if a.err != nil || a.answer.Header().Get(authz.RevocationHeader) != query.RevocationUnknown {
			t.Fatalf("the read of u-8 after the wait gave %v and %s %q, want unknown", a.err, authz.RevocationHeader, a.answer.Header().Get(authz.RevocationHeader))
		}
		// The maker reads the state of its own revocation.
		go func() {
			answer, err, _ := r.await(queries, "u-7@t1", "wait=5", location)
			answers <- awaited{answer, err}
		}()
		<-clock.bounds
		if a := <-answers; a.err != nil || a.answer.Header().Get(authz.RevocationHeader) != string(authz.RevocationComplete) {
			t.Fatalf("the read of u-7 gave %v and %q, want complete", a.err, a.answer.Header().Get(authz.RevocationHeader))
		}
	})
	t.Run("ST-SUP-RP11-LC21-T03 A hostile Authz-Revocation-After value appears in no error, header or log", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		queries := r.queries()
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(previous) })
		for _, value := range []string{"hostile-<script>alert(1)</script>", "hostile-' OR 1=1 --", "hostile-../../etc/passwd",
			"hostile-" + strings.Repeat("A", 8000), authz.RevocationPath + "hostile-%0d%0aX-Injected:1"} {
			answer, err, _ := r.await(queries, "u-7@t1", "", value)
			if !errors.Is(err, query.ErrBadRequest) {
				t.Fatalf("the value %.40q gave %v, want ErrBadRequest", value, err)
			}
			for where, text := range map[string]string{"the error": err.Error(), "the log": logs.String(),
				"the headers": fmt.Sprint(answer.Header()), "the body": answer.Body.String()} {
				if strings.Contains(text, "hostile") {
					t.Fatalf("%s holds the hostile value %.40q: %.200s", where, value, text)
				}
			}
		}
	})
}

func TestReadYourWrites(t *testing.T) {
	t.Run("ST-SUP-RP11-LC21-T01 Two creates, then an immediate list with the revocation of the last shows both", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		first := r.create("u-7@t1", "k-ryw-1", "ryw1", nil)
		second := r.create("u-7@t1", "k-ryw-2", "ryw2", nil)
		location := second.Response.Header.Get(authz.RevocationLocationHeader)
		if location == "" || location == first.Response.Header.Get(authz.RevocationLocationHeader) {
			t.Fatalf("the creates answered %v and %v, want a revocation of their own each", first.Response.Header, second.Response.Header)
		}
		queries, err := query.New(query.Config{DB: r.pool, AuditDB: r.auditDB, Audit: r.writer,
			AuditSettings: audit.Settings{Level: audit.LevelChange}, Authz: r.az, Service: "orders"})
		if err != nil {
			t.Fatal(err)
		}
		entity, err := query.NewEntity(query.Spec{Name: "order", Schema: "public", Table: "placed_orders", Key: "id",
			Columns: []string{"id"}, ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(row query.Row) (string, error) { return row.Text("id") })
		if err != nil {
			t.Fatal(err)
		}
		list, err := query.NewListQuery("list_orders", entity, "view")
		if err != nil {
			t.Fatal(err)
		}
		b := r.bound("u-7@t1")
		listed := func(header string) ([]string, string) {
			t.Helper()
			request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodGet, "/orders", nil)
			if header != "" {
				request.Header.Set(authz.RevocationAfterHeader, header)
			}
			answer := httptest.NewRecorder()
			if err := queries.AwaitWrite(answer, request, b.Tenant, b.Principal); err != nil {
				t.Fatal(err)
			}
			call := probe.NewRecorder(nil).Bind(context.Background(), b.Tenant.ID(), b.Principal.Subject().String(), "list_orders")
			ids, err := query.Run(request.Context(), queries, list, query.Bound[struct{}]{Tenant: b.Tenant, Principal: b.Principal,
				Call: call, Probe: func(*probe.Probe) struct{} { return struct{}{} }, RequestID: NewID()}, query.ListRequest{Limit: 10},
				func(ctx context.Context, request query.ListRequest, h *query.Handle[struct{}]) ([]string, error) {
					repository, err := query.Repo(h, entity)
					if err != nil {
						return nil, err
					}
					page, err := repository.List(ctx, request)
					var out []string
					for _, item := range page.Items {
						out = append(out, item.Value())
					}
					return out, err
				})
			if err != nil {
				t.Fatal(err)
			}
			return ids, answer.Header().Get(authz.RevocationHeader)
		}
		// Before the relay runs, a list does not see the creates: this is the gap that the header closes.
		if ids, _ := listed(""); len(ids) != 0 {
			t.Fatalf("a list before the delivery shows %v, want nothing", ids)
		}
		go func() { time.Sleep(300 * time.Millisecond); r.startRelay() }()
		ids, state := listed(location)
		if state != "complete" || len(ids) != 2 {
			t.Fatalf("a list with %s answered %s and %v, want complete and both creates", authz.RevocationAfterHeader, state, ids)
		}
		// The identity of a revocation that is not the caller's own is unknown, and a value that is no identity is
		// a bad request.
		if _, state := listed(authz.RevocationPath + uuid.NewString()); state != query.RevocationUnknown {
			t.Fatalf("a list with a revocation that is not the caller's answered %q, want unknown", state)
		}
		request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodGet, "/orders", nil)
		request.Header.Set(authz.RevocationAfterHeader, "not-an-id")
		if err := queries.AwaitWrite(httptest.NewRecorder(), request, b.Tenant, b.Principal); !errors.Is(err, query.ErrBadRequest) {
			t.Fatalf("a list with a bad %s gave %v, want ErrBadRequest", authz.RevocationAfterHeader, err)
		}
	})
}

// relayRelations are the relations that the relay of the tests may write: those of the orders, and the admin relation
// of the service-level resource that Provision grants.
var relayRelations = []authz.Relation{{ResourceType: "order", Name: "owner"}, {ResourceType: "order", Name: "customer"},
	{ResourceType: "order", Name: "product"}, {ResourceType: "order", Name: "tenant"}, {ResourceType: "service", Name: "admin"}}

// headerStream is the server transport stream of a gRPC call in a test: it keeps the header metadata that the handler
// sets.
type headerStream struct {
	mu sync.Mutex
	md metadata.MD
}

func (h *headerStream) Method() string { return "/test.Orders/Place" }
func (h *headerStream) SetHeader(md metadata.MD) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.md = metadata.Join(h.md, md)
	return nil
}
func (h *headerStream) SendHeader(md metadata.MD) error { return h.SetHeader(md) }
func (h *headerStream) SetTrailer(metadata.MD) error    { return nil }

// header gives the values of the header metadata name.
func (h *headerStream) header(name string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.md.Get(name)
}

// rpcCall gives a gRPC call context of who with the request metadata pairs, and the stream that keeps its header.
func (r *revocationEnv) rpcCall(ctx context.Context, pairs ...string) (context.Context, *headerStream) {
	stream := &headerStream{}
	return metadata.NewIncomingContext(grpc.NewContextWithServerTransportStream(ctx, stream), metadata.Pairs(pairs...)), stream
}

func TestRevocationRPC(t *testing.T) {
	t.Run("ST-SUP-RP10-LC40-T01 A gRPC command that enqueued a tuple answers its revocation in the header metadata, the same on a replay, and no other answer does", func(t *testing.T) {
		r := newRevocationEnv(t, 0, 0)
		var create bool
		var fail error
		spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
			Command: r.place, ResourceType: "order", SubjectType: "user", Status: http.StatusCreated,
			NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
			NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
			Resource:    func(*structpb.Struct) string { return "" },
			Fill: func(ctx context.Context, request *structpb.Struct, h *Handle[testProbe]) (*structpb.Struct, error) {
				if fail != nil {
					return nil, fail
				}
				if create {
					id := request.GetFields()["id"].GetStringValue()
					if _, err := repo(t, h, r.orders).Create(ctx, placedOrder{ID: id, CustomerID: "c1", ProductID: "p1"}); err != nil {
						return nil, err
					}
				}
				return structpb.NewStruct(map[string]any{"done": true})
			},
		}
		rpc, err := ServeRPC(r.rt, spec)
		if err != nil {
			t.Fatal(err)
		}
		call := func(key, id string) (*headerStream, error) {
			ctx, stream := r.rpcCall(r.identity["u-7@t1"], RPCKeyMetadata, `"`+key+`"`)
			request, _ := structpb.NewStruct(map[string]any{"id": id})
			_, err := rpc(ctx, request)
			return stream, err
		}
		create = true
		first, err := call("k-lc40-1", "rpc1")
		if err != nil {
			t.Fatal(err)
		}
		location := first.header(authz.RevocationLocationMetadata)
		if !slices.Equal(first.header(authz.RevocationMetadata), []string{"pending"}) || len(location) != 1 ||
			!strings.HasPrefix(location[0], authz.RevocationPath) {
			t.Fatalf("the create answered the header metadata %v, want authz-revocation: pending and one location", first.md)
		}
		if _, err := uuid.Parse(strings.TrimPrefix(location[0], authz.RevocationPath)); err != nil {
			t.Fatalf("the location %s does not end in a revocation identity", location[0])
		}
		// The location is the one that the HTTP status route answers for the writer.
		if answer, body, _ := r.status("u-7@t1", location[0], ""); answer.Code != http.StatusOK || body["state"] != "pending" {
			t.Fatalf("the status of the gRPC revocation is %d %v, want pending", answer.Code, body)
		}
		replay, err := call("k-lc40-1", "rpc1")
		if err != nil || !slices.Equal(replay.header(authz.RevocationLocationMetadata), location) ||
			!slices.Equal(replay.header(authz.RevocationMetadata), []string{"pending"}) {
			t.Fatalf("the replay answered %v, %v, want the same metadata", replay.md, err)
		}
		// A command that enqueues no tuple, and one that fails, send neither key.
		create = false
		if none, err := call("k-lc40-2", ""); err != nil || len(none.md) != 0 {
			t.Fatalf("a command with no tuple answered the metadata %v, %v, want none", none.md, err)
		}
		create, fail = true, NewRefusal("place", "closed", http.StatusConflict, "place.closed")
		if failed, err := call("k-lc40-3", "rpc3"); err == nil || len(failed.md) != 0 {
			t.Fatalf("a failed command answered the metadata %v, %v, want an error and none", failed.md, err)
		}
	})
	t.Run("ST-SUP-RP11-LC22-T01 A gRPC query waits for the revocation that its metadata names, within the prefer wait, the cap and the call deadline, and answers the state", func(t *testing.T) {
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 30*time.Second, 0, 0, clock)
		queries := r.queries()
		location := r.create("u-7@t1", "k-lc22-1", "q1", nil).Response.Header.Get(authz.RevocationLocationHeader)
		id := strings.TrimPrefix(location, authz.RevocationPath)
		await := func(ctx context.Context, who string, pairs ...string) (*headerStream, <-chan error) {
			ctx, stream := r.rpcCall(ctx, pairs...)
			b := r.bound(who)
			done := make(chan error, 1)
			go func() { done <- queries.AwaitWriteRPC(ctx, b.Tenant, b.Principal) }()
			return stream, done
		}
		answer := func(done <-chan error) error {
			select {
			case err := <-done:
				return err
			case b := <-clock.bounds:
				t.Fatalf("the call waits with a bound of %s, want an answer at once", b.d)
				return nil
			}
		}
		// No metadata waits for nothing and sends nothing; a bad value and two values are bad requests.
		if stream, done := await(r.identity["u-7@t1"], "u-7@t1", "x-other", "1"); answer(done) != nil || len(stream.md) != 0 {
			t.Fatalf("a call with no revocation metadata sent %v", stream.md)
		}
		for name, pairs := range map[string][]string{"a bad value": {authz.RevocationAfterMetadata, "not-an-id"},
			"two values": {authz.RevocationAfterMetadata, id, authz.RevocationAfterMetadata, id},
			"upper case": {authz.RevocationAfterMetadata, strings.ToUpper(id)}} {
			if _, done := await(r.identity["u-7@t1"], "u-7@t1", pairs...); !errors.Is(answer(done), query.ErrBadRequest) {
				t.Fatalf("%s is no bad request", name)
			}
		}
		// The revocation of u-7@t1 is unknown to another tenant, at once. Another principal of t1 waits for it, and
		// then gets unknown, never its state (decision ST-SUP-Q206 (a), recommended).
		for _, who := range []string{"u-8@t1", "u-7@t2"} {
			stream, done := await(r.identity[who], who, authz.RevocationAfterMetadata, location, authz.PreferMetadata, "wait=5")
			b := clock.waitBound(t, nil)
			if who == "u-8@t1" {
				clock.polled(t, nil)
				b.end()
			}
			if err := <-done; err != nil || !slices.Equal(stream.header(authz.RevocationMetadata), []string{query.RevocationUnknown}) {
				t.Fatalf("%s waited for u-7@t1's revocation: %v %v, want unknown", who, err, stream.md)
			}
			b.end()
		}
		// The prefer wait bounds the wait: no relay runs, so it ends pending at its end.
		stream, done := await(r.identity["u-7@t1"], "u-7@t1", authz.RevocationAfterMetadata, id, authz.PreferMetadata, "wait=2")
		if b := clock.waitBound(t, nil); b.d != 2*time.Second {
			t.Fatalf("a wait=2 call is bounded at %s, want 2 s", b.d)
		}
		clock.polled(t, nil)
		clock.advance(2 * time.Second)
		if err := <-done; err != nil || !slices.Equal(stream.header(authz.RevocationMetadata), []string{"pending"}) {
			t.Fatalf("the wait answered %v %v, want pending", err, stream.md)
		}
		// The call deadline bounds the wait to half the time that it leaves, below the cap of 30 s.
		ctx, cancel := context.WithTimeout(r.identity["u-7@t1"], 20*time.Second)
		defer cancel()
		stream, done = await(ctx, "u-7@t1", authz.RevocationAfterMetadata, location)
		b := clock.waitBound(t, nil)
		if b.d > 10*time.Second || b.d < 9*time.Second {
			t.Fatalf("a call with a deadline 20 s away is bounded at %s, want half of it, below the cap", b.d)
		}
		clock.polled(t, nil)
		// A client that cancels during the wait ends it with the error of its context: no internal error (review
		// of PR #428, L1).
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled wait gave %v, want the cancel", err)
		}
		_ = stream
	})
	t.Run("ST-SUP-RP11-LC22-T02 ServeStream waits before the read, refuses a bad value as InvalidArgument, and a stream with no runtime that names a revocation is Internal", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		queries := r.queries()
		location := r.create("u-7@t1", "k-lc22-2", "q2", nil).Response.Header.Get(authz.RevocationLocationHeader)
		r.startRelay()
		var seen []string
		var stream *headerStream
		serve := func(rt *query.Runtime) func(context.Context, *structpb.Struct, func(*structpb.Struct) error) error {
			h, err := query.ServeStream(query.StreamRPC[*structpb.Struct, *structpb.Struct, struct{}, string, struct{}]{
				Bind: func(ctx context.Context) (query.Bound[struct{}], error) {
					b, err := BindIdentity(ctx, "user")
					return query.Bound[struct{}]{Tenant: b.Tenant, Principal: b.Principal}, err
				},
				Decode: func(*structpb.Struct) (struct{}, error) { return struct{}{}, nil },
				Stream: func(context.Context, query.Bound[struct{}], struct{}, func(string) error) (uint64, error) {
					seen = append(seen, strings.Join(stream.header(authz.RevocationMetadata), ","))
					return 0, nil
				},
				NewMessage: func() *structpb.Struct { return &structpb.Struct{} },
				Runtime:    rt,
			})
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		send := func(*structpb.Struct) error { return nil }
		var ctx context.Context
		ctx, stream = r.rpcCall(r.identity["u-7@t1"], authz.RevocationAfterMetadata, location, authz.PreferMetadata, "wait=5")
		if err := serve(queries)(ctx, &structpb.Struct{}, send); err != nil || !slices.Equal(seen, []string{"complete"}) {
			t.Fatalf("the stream ran with the state %v (%v), want the read after complete", seen, err)
		}
		ctx, stream = r.rpcCall(r.identity["u-7@t1"], authz.RevocationAfterMetadata, "not-an-id")
		if err := serve(queries)(ctx, &structpb.Struct{}, send); status.Code(err) != codes.InvalidArgument || len(seen) != 1 {
			t.Fatalf("a bad value gave %v and ran the stream %d times, want InvalidArgument and no read", err, len(seen)-1)
		}
		ctx, stream = r.rpcCall(r.identity["u-7@t1"], authz.RevocationAfterMetadata, location)
		if err := serve(nil)(ctx, &structpb.Struct{}, send); status.Code(err) != codes.Internal || len(seen) != 1 {
			t.Fatalf("a stream with no runtime gave %v, want Internal and no read", err)
		}
		ctx, stream = r.rpcCall(r.identity["u-7@t1"])
		if err := serve(nil)(ctx, &structpb.Struct{}, send); err != nil || len(seen) != 2 {
			t.Fatalf("a stream with no runtime and no revocation gave %v, want the read", err)
		}
	})
}

func TestRevocationCancel(t *testing.T) {
	t.Run("ST-SUP-RP10-LC35-T04 A client that cancels during the wait of the status route gets no answer and no internal error code", func(t *testing.T) {
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 5*time.Second, 0, 0, clock)
		location := r.create("u-7@t1", "k-lc35-5", "o9", nil).Response.Header.Get(authz.RevocationLocationHeader)
		ctx, cancel := context.WithCancel(otel.WithErrorCode(r.identity["u-7@t1"]))
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		request.Header.Set("Prefer", "wait=5")
		answer := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); ServeRevocationStatus(r.rt, "user").ServeHTTP(answer, request) }()
		clock.waitBound(t, nil)
		clock.polled(t, nil)
		cancel()
		<-done
		code, _ := otel.ErrorCode(ctx)
		if answer.Body.Len() != 0 || strings.Contains(answer.Body.String(), ErrorCodeInternal) || code != "" {
			t.Fatalf("the cancelled wait answered %d %q with the log code %q, want no answer and no code", answer.Code, answer.Body, code)
		}
	})
}

func TestProvisionWaitLimit(t *testing.T) {
	t.Run("ST-SUP-RP10-LC41-T02 One principal holds at most MaxProvisionWaits Provision waits over all the tenants it provisions", func(t *testing.T) {
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 10*time.Second, 0, 0, clock)
		migration, err := tenant.Migration("public", r.roles[0])
		if err != nil {
			t.Fatal(err)
		}
		r.exec(migration)
		h, err := ServeTenant(r.rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
			Schema: "public", SubjectType: "user", AdminRelation: "admin", TrustedIssuers: []string{"https://issuer.example"}})
		if err != nil {
			t.Fatal(err)
		}
		send := func(id string) <-chan *httptest.ResponseRecorder {
			answers := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodPost, "/stego/tenants",
					strings.NewReader(`{"tenant_id":"`+id+`","admins":[{"subject":"a-1"}]}`))
				request.Header.Set("Idempotency-Key", `"k-lc41-t02-`+id+`"`)
				request.Header.Set("Prefer", "wait=5")
				answer := httptest.NewRecorder()
				h.ServeHTTP(answer, request)
				answers <- answer
			}()
			return answers
		}
		var waiting []<-chan *httptest.ResponseRecorder
		for i := range MaxProvisionWaits {
			answers := send(fmt.Sprintf("w%d", i))
			waiting = append(waiting, answers)
			select {
			case <-clock.bounds:
			case a := <-answers:
				t.Fatalf("Provision %d answered %d %v before it waited", i, a.Code, a.Header())
			}
			clock.polled(t, nil)
		}
		// One more Provision of the same principal, of another tenant, answers pending at once and does not wait.
		select {
		case a := <-send("w-over"):
			if a.Code != http.StatusCreated || a.Header().Get(authz.RevocationHeader) != "pending" || a.Header().Get("Preference-Applied") != "" {
				t.Fatalf("the Provision over the limit answered %d %v, want 201 pending with no Preference-Applied", a.Code, a.Header())
			}
		case b := <-clock.bounds:
			t.Fatalf("the Provision over the limit waits with a bound of %s", b.d)
		}
		clock.advance(5 * time.Second)
		for _, answers := range waiting {
			if a := <-answers; a.Header().Get("Preference-Applied") != "wait=5" {
				t.Fatalf("a waiting Provision answered %v, want wait=5", a.Header())
			}
		}
	})
}

func TestProvisionRevocation(t *testing.T) {
	t.Run("ST-SUP-RP10-LC41-T01 Provision answers the revocation of its grants pending, waits for it with Prefer, and a replay does not wait", func(t *testing.T) {
		provision := func(r *revocationEnv) func(key, body, prefer string) *httptest.ResponseRecorder {
			migration, err := tenant.Migration("public", r.roles[0])
			if err != nil {
				t.Fatal(err)
			}
			r.exec(migration)
			h, err := ServeTenant(r.rt, TenantSpec{Action: tenant.ProvisionAction, ResourceType: "service", Permission: "provision",
				Schema: "public", SubjectType: "user", AdminRelation: "admin", TrustedIssuers: []string{"https://issuer.example"}})
			if err != nil {
				t.Fatal(err)
			}
			return func(key, body, prefer string) *httptest.ResponseRecorder {
				request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodPost, "/stego/tenants", strings.NewReader(body))
				request.Header.Set("Idempotency-Key", `"`+key+`"`)
				if prefer != "" {
					request.Header.Set("Prefer", prefer)
				}
				answer := httptest.NewRecorder()
				h.ServeHTTP(answer, request)
				return answer
			}
		}
		// With the clock of the test and no relay: no Prefer answers pending at once; Prefer waits to its end.
		clock := newWaitClock(t)
		r := newRevocationEnvClock(t, 10*time.Second, 0, 0, clock)
		send := provision(r)
		first := send("k-lc41-1", `{"tenant_id":"t9","admins":[{"subject":"a-1"}]}`, "")
		if first.Code != http.StatusCreated || first.Header().Get(authz.RevocationHeader) != "pending" ||
			first.Header().Get(authz.RevocationLocationHeader) != "" || first.Header().Get("Preference-Applied") != "" {
			t.Fatalf("Provision answered %d %v, want 201 with Authz-Revocation: pending and no location", first.Code, first.Header())
		}
		replay := send("k-lc41-1", `{"tenant_id":"t9","admins":[{"subject":"a-1"}]}`, "wait=5")
		select {
		case b := <-clock.bounds:
			t.Fatalf("the replay waits with a bound of %s, want no wait", b.d)
		default:
		}
		if replay.Code != http.StatusCreated || replay.Header().Get(authz.RevocationHeader) != "pending" || replay.Header().Get("Preference-Applied") != "" {
			t.Fatalf("the replay answered %d %v, want the stored pending answer", replay.Code, replay.Header())
		}
		answers := make(chan *httptest.ResponseRecorder, 1)
		go func() { answers <- send("k-lc41-2", `{"tenant_id":"t8","admins":[{"subject":"a-1"}]}`, "wait=3") }()
		b := <-clock.bounds
		if b.d != 3*time.Second {
			t.Fatalf("a Provision with wait=3 is bounded at %s", b.d)
		}
		clock.polled(t, nil)
		clock.advance(3 * time.Second)
		if a := <-answers; a.Code != http.StatusCreated || a.Header().Get(authz.RevocationHeader) != "pending" || a.Header().Get("Preference-Applied") != "wait=3" {
			t.Fatalf("a Provision whose grant no relay delivers answered %d %v, want 201 pending with wait=3", a.Code, a.Header())
		}
		// With the relay: the wait ends complete, and the grant is at the floor that a later decision reads.
		real := newRevocationEnv(t, 10*time.Second, 0)
		sendReal := provision(real)
		real.startRelay()
		if a := sendReal("k-lc41-3", `{"tenant_id":"t7","admins":[{"subject":"a-1"}]}`, "wait=10"); a.Code != http.StatusCreated ||
			a.Header().Get(authz.RevocationHeader) != "complete" {
			t.Fatalf("a Provision with the relay answered %d %v, want 201 complete", a.Code, a.Header())
		}
		// A Provision with no admin and no worker stages no tuple, and answers no revocation.
		plain := sendReal("k-lc41-4", `{"tenant_id":"t6"}`, "wait=10")
		if plain.Code != http.StatusBadRequest || plain.Header().Get(authz.RevocationHeader) != "" {
			t.Fatalf("a Provision with no admins answered %d %v, want 400 and no revocation", plain.Code, plain.Header())
		}
	})
}
