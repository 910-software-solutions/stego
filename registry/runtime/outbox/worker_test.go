package outbox

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// testConfig is a worker configuration for tests: one step at a time and short waits. newWorker puts the attempt
// deadline on a hand clock, so no attempt of a test ends by the real clock.
func testConfig() WorkerConfig {
	config := DefaultWorkerConfig()
	config.Concurrency = 1
	config.PollInterval = 10 * time.Millisecond
	config.AttemptTimeout = 300 * time.Millisecond
	config.LeaseDuration = 30 * time.Second
	config.RetryMin = 100 * time.Millisecond
	config.RetryMax = 10 * time.Second
	return config
}

func newWorker(t testing.TB, q *Queue, edit func(*WorkerConfig)) *Worker {
	t.Helper()
	config := testConfig()
	if edit != nil {
		edit(&config)
	}
	w, err := NewWorker(q, config)
	if err != nil {
		t.Fatal(err)
	}
	useHandClock(t, w)
	return w
}

// useHandClock gives the attempts and status lookups of w their deadlines from a new hand clock, and returns it. The
// lease left stays on the real clock unless the test also sets w.now to the clock's now.
func useHandClock(t testing.TB, w *Worker) *handClock {
	t.Helper()
	clock := newHandClock(t)
	w.deadline = clock.deadline
	return clock
}

// handClock is a clock that the test moves itself, for the attempt deadline and the lease left of
// ST-SUP-RP01-LC02. Its time starts at the real time when it is made, with the monotonic reading, so a lease end
// that a claim measured on the real clock after that is not short on it. Each attempt context it gives ends when
// the clock reaches the attempt's bound, with context.DeadlineExceeded as a timeout context does, or when its parent
// ends. The fake destination that holds a request moves the clock to the bound of the attempt that sent it.
type handClock struct {
	mu       sync.Mutex
	base     time.Time
	offset   time.Duration
	attempts []*handAttempt
	made     chan *handAttempt
}

func newHandClock(t testing.TB) *handClock {
	c := &handClock{base: time.Now(), made: make(chan *handAttempt, 64)}
	t.Cleanup(func() {
		c.mu.Lock()
		attempts := c.attempts
		c.mu.Unlock()
		for _, a := range attempts {
			a.end(context.Canceled)
		}
	})
	return c
}

func (c *handClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.offset)
}

// deadline gives an attempt context that ends at the clock's time plus d. It sends the attempt on made when the
// buffer holds it, so a test can read the bound that the worker asked for.
func (c *handClock) deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c.mu.Lock()
	a := &handAttempt{clock: c, parent: parent, d: d, at: c.base.Add(c.offset + d), done: make(chan struct{})}
	c.attempts = append(c.attempts, a)
	c.mu.Unlock()
	a.stop = context.AfterFunc(parent, func() { a.end(parent.Err()) })
	select {
	case c.made <- a:
	default:
	}
	return a, func() {
		a.stop()
		a.end(context.Canceled)
	}
}

// advance moves the clock and ends each attempt whose bound it reached.
func (c *handClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	now := c.base.Add(c.offset)
	var due []*handAttempt
	for _, a := range c.attempts {
		if !a.at.After(now) {
			due = append(due, a)
		}
	}
	c.mu.Unlock()
	for _, a := range due {
		a.end(context.DeadlineExceeded)
	}
}

// next returns the attempt that the clock gave first and that the test did not read yet. The worker asks for the
// bound before it calls the handler, so a missing bound fails the test and does not wait.
func (c *handClock) next(t testing.TB) *handAttempt {
	t.Helper()
	select {
	case a := <-c.made:
		return a
	default:
		t.Fatal("the attempt has no bound on the hand clock")
		return nil
	}
}

// handAttempt is one attempt context of the hand clock.
type handAttempt struct {
	clock  *handClock
	parent context.Context
	d      time.Duration
	at     time.Time
	stop   func() bool
	once   sync.Once
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// Deadline gives the parent's deadline: the bound is on the hand clock, and a deadline here would put it back on
// the real clock of the network and database code that reads it.
func (a *handAttempt) Deadline() (time.Time, bool) { return a.parent.Deadline() }
func (a *handAttempt) Done() <-chan struct{}       { return a.done }
func (a *handAttempt) Value(key any) any           { return a.parent.Value(key) }
func (a *handAttempt) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *handAttempt) end(err error) {
	a.once.Do(func() {
		a.mu.Lock()
		a.err = err
		a.mu.Unlock()
		close(a.done)
	})
}

// expire moves the clock to the bound of the attempt.
func (a *handAttempt) expire() {
	if left := a.at.Sub(a.clock.now()); left > 0 {
		a.clock.advance(left)
		return
	}
	a.clock.advance(0)
}

func register(t testing.TB, w *Worker, d Destination) {
	t.Helper()
	if err := w.Register(d); err != nil {
		t.Fatal(err)
	}
}

// cycle runs one claim and the step of what it claimed, and returns how many messages it claimed.
func cycle(t testing.TB, w *Worker) int {
	t.Helper()
	n, err := w.cycle(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func startWorker(t *testing.T, w *Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- w.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, result
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// metricReader is a manual reader of the meter provider that the worker gets.
func metricReader() (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	reader := sdkmetric.NewManualReader()
	return reader, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var found []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				found = append(found, m)
			}
		}
	}
	return found
}

// gaugeValues reads outbox.oldest.age by destination.
func gaugeValues(t *testing.T, reader *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	values := map[string]float64{}
	for _, m := range collect(t, reader, "outbox.oldest.age") {
		gauge, ok := m.Data.(metricdata.Gauge[float64])
		if !ok {
			t.Fatalf("outbox.oldest.age is a %T", m.Data)
		}
		for _, point := range gauge.DataPoints {
			destination, _ := point.Attributes.Value("destination")
			values[destination.AsString()] = point.Value
		}
	}
	return values
}

func TestWorkerDelivery(t *testing.T) {
	t.Run("ST-SUP-RP01-LC02-T03 A result after the attempt deadline is not acknowledged", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		clock := useHandClock(t, w)
		var calls atomic.Int32
		entered := make(chan context.Context, 1)
		release := make(chan struct{})
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(ctx context.Context, _ *sql.Tx, d Delivery) error {
			if calls.Add(1) == 1 {
				entered <- ctx
				select {
				case <-ctx.Done():
				case <-release:
				}
			}
			return nil
		}})
		var cycleErr error
		cycled := make(chan struct{})
		go func() {
			defer close(cycled)
			_, cycleErr = w.cycle(context.Background(), context.Background())
		}()
		// If the test fails while the handler waits, the handler is let go and the cycle ends before the database
		// is dropped.
		t.Cleanup(func() {
			close(release)
			<-cycled
		})
		var ctx context.Context
		select {
		case ctx = <-entered:
		case <-cycled:
			t.Fatalf("the handler was not called: %v", cycleErr)
		}
		attempt := clock.next(t)
		if attempt.d != w.config.AttemptTimeout || ctx != context.Context(attempt) {
			t.Fatalf("the handler got a bound of %v, want the attempt deadline %v, or not the attempt context", attempt.d, w.config.AttemptTimeout)
		}
		clock.advance(w.config.AttemptTimeout - time.Nanosecond)
		if ctx.Err() != nil {
			t.Fatal("the attempt ended one nanosecond before its deadline")
		}
		clock.advance(time.Nanosecond)
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("the attempt did not end at its deadline: %v", ctx.Err())
		}
		<-cycled
		if cycleErr != nil {
			t.Fatal(cycleErr)
		}
		row := readRow(t, db, m.ID)
		if w.Stats().Delivered != 0 || row.State != OutcomeUnknown || row.FailureCode != "delivery-timeout" {
			t.Fatalf("a late success counted: delivered %d, row %+v", w.Stats().Delivered, row)
		}
		cycle(t, w)
		if calls.Load() != 2 || readRow(t, db, m.ID).State != Done || w.Stats().Delivered != 1 {
			t.Fatalf("the message was not sent again: calls %d, row %+v", calls.Load(), readRow(t, db, m.ID))
		}
		// NewWorker puts the attempt deadline and the lease left on the real clock.
		real, err := NewWorker(q, testConfig())
		if err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		bounded, stop := real.deadline(context.Background(), time.Hour)
		read := real.now()
		after := time.Now()
		defer stop()
		end, ok := bounded.Deadline()
		if !ok || end.Before(before.Add(time.Hour)) || end.After(after.Add(time.Hour)) || read.Before(before) || read.After(after) {
			t.Fatalf("NewWorker does not use the real clock: deadline %v %v, now %v, between %v and %v", end, ok, read, before, after)
		}
	})

	t.Run("ST-SUP-RP01-LC03-T03 An unknown destination stays in the queue", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("nowhere", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}})
		cycle(t, w)
		row := readRow(t, db, m.ID)
		if row.State != Pending || row.FailureCode != "destination-unknown" || row.Attempts != 0 || calls.Load() != 0 ||
			w.Stats().UnknownDestinations == 0 {
			t.Fatalf("the unknown destination: row %+v, calls %d, stats %+v", row, calls.Load(), w.Stats())
		}
	})

	t.Run("ST-SUP-RP01-LC06-T01 The emitted worker stores no handler error text", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		const secret = "password=hunter2"
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return errors.New("the sink refused " + secret)
		}})
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.FailureCode != "delivery-failed" {
			t.Fatalf("the failure code is %q", row.FailureCode)
		}
		var text string
		if err := admin(t, db).QueryRow(`SELECT row_to_json(m)::text FROM stego_outbox.messages AS m WHERE id = $1`, m.ID).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "hunter2") || strings.Contains(text, "refused") {
			t.Fatalf("the row holds the handler's error text: %s", text)
		}
	})

	t.Run("ST-SUP-RP01-LC06-T03 A failure code that the handler gives is kept only as a short label", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		w := newWorker(t, q, nil)
		var answer atomic.Value
		register(t, w, Destination{Name: "consumer", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return answer.Load().(func() error)()
		}})
		ctx := context.Background()
		for _, c := range []struct {
			name, code, state string
			answer            func() error
		}{
			{"a transient refusal with a code", "fill.rolled_back.not_found.1", "pending",
				func() error {
					return WithFailureCode(TransientRefusal(errors.New("x")), "fill.rolled_back.not_found.1")
				}},
			{"a permanent refusal with a code", "poison", "parked",
				func() error { return WithFailureCode(PermanentRefusal(errors.New("x")), "poison") }},
			{"a code that is not a label", "delivery-refused", "pending",
				func() error { return WithFailureCode(TransientRefusal(errors.New("x")), "password=hunter2 refused") }},
			{"a not sent with a code", "not-sent.1", "pending",
				func() error { return WithFailureCode(NotSent(errors.New("x")), "not-sent.1") }},
			{"a conflict keeps its own code", "delivery-conflict", "pending",
				func() error { return WithFailureCode(Conflict(errors.New("x")), "fill.conflict") }},
		} {
			answer.Store(c.answer)
			m := message("consumer", "key:"+c.name)
			enqueue(t, db, q, m)
			got, err := q.ClaimDestination(ctx, "consumer", 1, time.Minute)
			if err != nil || len(got) != 1 {
				t.Fatalf("%s: the claim gave %v, %v", c.name, got, err)
			}
			w.step(ctx, got[0])
			if row := readRow(t, db, m.ID); row.State != State(c.state) || row.FailureCode != c.code {
				t.Fatalf("%s: the message is %s with %q, want %s with %q", c.name, row.State, row.FailureCode, c.state, c.code)
			}
			clock.Advance(time.Hour)
			if c.state == "pending" {
				again, err := q.ClaimDestination(ctx, "consumer", 1, time.Minute)
				if err != nil || len(again) != 1 || again[0].FailureCode != c.code {
					t.Fatalf("%s: the next claim gave %+v, %v", c.name, again, err)
				}
				answer.Store(func() error { return nil })
				w.step(ctx, again[0])
			}
		}
	})
}

// faultConnector passes each statement to the real database and then loses a result, as a client that loses the
// answer of a statement that committed. With loseCommit false it loses the result of every statement after its
// first row, such as a claim; with loseCommit true it passes every statement through whole, and loses the result of
// each COMMIT after the database committed, such as the commit of an acknowledgement.
type faultConnector struct {
	database   *sql.DB
	loseCommit bool
	failBegin  bool
}

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	return &faultConn{faultConnector: c}, nil
}
func (faultConnector) Driver() driver.Driver { return faultDriver{} }

type faultDriver struct{}

func (faultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the fault connector")
}

// faultConn routes its statements to the real database, or to the real transaction that it holds.
type faultConn struct {
	faultConnector
	tx *sql.Tx
}

func (*faultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *faultConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if c.failBegin {
		return nil, errors.New("the database refuses a transaction")
	}
	tx, err := c.database.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return faultTx{c}, nil
}
func (*faultConn) Close() error { return nil }

type faultTx struct{ c *faultConn }

func (f faultTx) Commit() error {
	err := f.c.tx.Commit()
	f.c.tx = nil
	if err == nil && f.c.loseCommit {
		return errors.New("the connection broke after the commit")
	}
	return err
}

func (f faultTx) Rollback() error {
	err := f.c.tx.Rollback()
	f.c.tx = nil
	return err
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, value := range args {
		values[i] = value.Value
	}
	var rows *sql.Rows
	var err error
	if c.tx != nil {
		rows, err = c.tx.QueryContext(ctx, query, values...)
	} else {
		rows, err = c.database.QueryContext(ctx, query, values...)
	}
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	return &faultRows{rows: rows, columns: columns, whole: c.loseCommit || c.failBegin}, nil
}

// faultRows gives the first row and then fails, so the client never reads the whole result.
type faultRows struct {
	rows    *sql.Rows
	columns []string
	read    bool
	whole   bool
}

func (r *faultRows) Columns() []string { return r.columns }
func (r *faultRows) Close() error      { return r.rows.Close() }
func (r *faultRows) Next(dest []driver.Value) error {
	if r.read && !r.whole {
		_ = r.rows.Close()
		return errors.New("the connection broke")
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	values := make([]any, len(dest))
	for i := range dest {
		values[i] = &dest[i]
	}
	if err := r.rows.Scan(values...); err != nil {
		return err
	}
	r.read = true
	return nil
}

func TestWorkerFailures(t *testing.T) {
	t.Run("ST-SUP-RP01-LC04-T01 A database failure drops no message", func(t *testing.T) {
		db, dsn := testDatabase(t)
		m := message("audit", "one")
		enqueue(t, db, newQueue(t, db, nil), m)
		pool, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		w := newWorker(t, newQueue(t, pool, nil), func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		entered, release := make(chan struct{}), make(chan struct{})
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			close(entered)
			<-release
			return nil
		}})
		cancel, result := startWorker(t, w)
		// The database ends every session of the pool while Run runs and a handler is in flight, and the pool closes:
		// the acknowledgement in the handler's transaction and the next claims fail.
		<-entered
		if _, err := admin(t, db).Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE usename = $1`, runtimeUser(t, dsn)); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		close(release)
		await(t, func() bool { return w.Stats().DatabaseFailures > 0 })
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, not the cancellation", err)
		}
		if w.Stats().Delivered != 0 {
			t.Fatalf("a database failure counted as a delivery: %+v", w.Stats())
		}
		if row := readRow(t, db, m.ID); row.State != Dispatching || !row.Token.Valid {
			t.Fatalf("the message lost its claim: %+v", row)
		}

		// The acknowledgement commits with the handler's transaction, but the result of the COMMIT is lost: the worker
		// counts a database failure, not a delivery.
		n := message("audit", "two")
		enqueue(t, db, newQueue(t, db, nil), n)
		faulty := sql.OpenDB(faultConnector{database: db, loseCommit: true})
		t.Cleanup(func() { _ = faulty.Close() })
		lost := newWorker(t, newQueue(t, faulty, nil), nil)
		register(t, lost, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		if got := cycle(t, lost); got != 1 {
			t.Fatalf("the cycle claimed %d messages", got)
		}
		if stats := lost.Stats(); stats.Delivered != 0 || stats.DatabaseFailures != 1 {
			t.Fatalf("a lost acknowledgement result counted as %+v", stats)
		}
		if row := readRow(t, db, n.ID); row.State != Done {
			t.Fatalf("the acknowledgement did not commit: %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC04-T02 Recovery after an unknown claim result keeps the delivery contract", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		m, n := message("audit", "one"), message("audit", "two")
		enqueue(t, db, newQueue(t, db, clock), m, n)
		faulty := sql.OpenDB(faultConnector{database: db})
		faulty.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = faulty.Close() })
		lost, err := newQueue(t, faulty, clock).Claim(context.Background(), 2, time.Minute)
		if err == nil || lost != nil {
			t.Fatalf("a claim with a lost result returned %v, %v", lost, err)
		}
		old := readRow(t, db, m.ID)
		if old.State != Dispatching || !old.Token.Valid || old.Attempts != 1 {
			t.Fatalf("the lost claim did not keep its lease: %+v", old)
		}
		q := newQueue(t, db, clock)
		if next, err := q.Claim(context.Background(), 2, time.Minute); err != nil || len(next) != 0 {
			t.Fatalf("a message under a live lease was claimed again: %v %v", next, err)
		}
		clock.Advance(2 * time.Minute)
		w := newWorker(t, q, nil)
		var got []Delivery
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			got = append(got, d)
			return nil
		}})
		for i := 0; i < 4; i++ {
			cycle(t, w)
		}
		if len(got) != 2 {
			t.Fatalf("the recovery delivered %d messages", len(got))
		}
		for _, d := range got {
			want := m
			if d.ID == n.ID {
				want = n
			}
			if d.ID != want.ID || d.Kind != want.Kind || string(d.Payload) != `{"version":1}` || d.Attempts != 2 ||
				d.Receipt.Token == old.Token.UUID {
				t.Fatalf("the recovered delivery %+v does not keep the contract", d)
			}
		}
		stale := Receipt{ID: m.ID, Token: old.Token.UUID, State: Dispatching}
		if ok, _ := q.acknowledge(context.Background(), stale); ok {
			t.Fatal("the lost token had an effect")
		}
		if ok, _ := q.retry(context.Background(), stale, 0, "late"); ok || readRow(t, db, m.ID).State != Done {
			t.Fatal("the lost token moved the message")
		}
	})
}

func TestWorkerBounds(t *testing.T) {
	t.Run("ST-SUP-RP01-LC05-T01 The worker holds its concurrency limit and copies its handlers", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		for i := 0; i < 8; i++ {
			enqueue(t, db, q, message("audit", uuid.NewString()))
		}
		var active, maximum atomic.Int32
		release := make(chan struct{})
		d := Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			now := active.Add(1)
			defer active.Add(-1)
			for current := maximum.Load(); now > current && !maximum.CompareAndSwap(current, now); current = maximum.Load() {
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		w := newWorker(t, q, func(c *WorkerConfig) { c.Concurrency = 2; c.AttemptTimeout = 5 * time.Second })
		register(t, w, d)
		d.Handler = nil
		_, result := startWorker(t, w)
		await(t, func() bool { return active.Load() == 2 })
		// Twenty poll intervals give an idle worker time to claim a third message while two handlers block.
		time.Sleep(200 * time.Millisecond)
		if n := active.Load(); n != 2 {
			t.Fatalf("%d handlers are active at one time, want 2", n)
		}
		if err := w.Run(context.Background()); err == nil {
			t.Fatal("the worker ran twice")
		}
		close(release)
		await(t, func() bool { return w.Stats().Delivered == 8 })
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatalf("Run after StopClaiming returned %v", err)
		}
		if err := w.Finish(context.Background()); err != nil {
			t.Fatal(err)
		}
		if maximum.Load() != 2 || active.Load() != 0 {
			t.Fatalf("the concurrency was %d and %d handlers are still active", maximum.Load(), active.Load())
		}
	})

	t.Run("ST-SUP-RP01-LC05-T04 Shutdown acknowledges a delivery that completed", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)

		// StopClaiming does not cancel a handler that runs: a handler that honours its context ends done.
		m := message("audit", "one")
		enqueue(t, db, q, m)
		entered, stopped := make(chan struct{}), make(chan struct{})
		w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		var cancelled atomic.Bool
		register(t, w, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered)
			<-stopped
			select {
			case <-ctx.Done():
				cancelled.Store(true)
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return nil
			}
		}})
		_, result := startWorker(t, w)
		<-entered
		w.StopClaiming()
		close(stopped)
		bound, stopBound := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopBound()
		if err := w.Finish(bound); err != nil {
			t.Fatal(err)
		}
		if cancelled.Load() {
			t.Fatal("StopClaiming cancelled the handler's context")
		}
		if err := <-result; err != nil {
			t.Fatalf("Run after StopClaiming returned %v", err)
		}
		if w.Stats().Delivered != 1 || readRow(t, db, m.ID).State != Done {
			t.Fatalf("StopClaiming lost a delivery: %+v, %+v", w.Stats(), readRow(t, db, m.ID))
		}

		// The end of the parent context cancels the handler; a success that it returns then is still recorded.
		n := message("audit", "two")
		enqueue(t, db, q, n)
		entered2, exited := make(chan struct{}), make(chan struct{})
		w2 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		register(t, w2, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered2)
			<-ctx.Done()
			close(exited)
			return nil
		}})
		cancel, result2 := startWorker(t, w2)
		<-entered2
		cancel()
		if err := <-result2; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, not the cancellation", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("the worker returned while its handler ran")
		}
		if w2.Stats().Delivered != 1 || readRow(t, db, n.ID).State != Done {
			t.Fatalf("shutdown lost a completed delivery: %+v", w2.Stats())
		}

		// The drain bound of Finish cancels a handler that waits; Finish waits for Run even when Run starts late.
		o := message("audit", "three")
		enqueue(t, db, q, o)
		w3 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 30 * time.Second; c.LeaseDuration = time.Minute })
		entered3 := make(chan struct{})
		register(t, w3, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered3)
			<-ctx.Done()
			return ctx.Err()
		}})
		finished := make(chan error, 1)
		bound, stop := context.WithCancel(context.Background())
		defer stop()
		go func() { finished <- w3.Finish(bound) }()
		select {
		case err := <-finished:
			t.Fatalf("Finish returned %v before Run started", err)
		case <-time.After(50 * time.Millisecond):
		}
		_, result3 := startWorker(t, w3)
		<-entered3
		w3.StopClaiming()
		stop()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Finish at its drain bound returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Finish did not cancel the handler at its drain bound")
		}
		if err := <-result3; err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if row := readRow(t, db, o.ID); row.State != OutcomeUnknown || w3.Stats().Delivered != 0 {
			t.Fatalf("the canceled handler left %+v", row)
		}
		if count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'dispatching'") != 0 {
			t.Fatal("a message stays in dispatching")
		}
	})
}

func TestWorkerMetrics(t *testing.T) {
	t.Run("ST-SUP-RP01-LC07-T01 The delivery delay of a message is recorded under its destination", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		enqueue(t, db, q, message("billing", "one"))
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider })
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		clock.Advance(3 * time.Second)
		cycle(t, w)
		found := collect(t, reader, "outbox.delivery.delay")
		if len(found) != 1 || found[0].Unit != "s" {
			t.Fatalf("outbox.delivery.delay is %+v", found)
		}
		histogram, ok := found[0].Data.(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) != 1 {
			t.Fatalf("outbox.delivery.delay holds %+v", found[0].Data)
		}
		point := histogram.DataPoints[0]
		want := attribute.NewSet(attribute.String("destination", "billing"))
		if point.Count != 1 || point.Sum != 3 || !point.Attributes.Equals(&want) {
			t.Fatalf("the observation is count %d, sum %v, labels %v", point.Count, point.Sum, point.Attributes.ToSlice())
		}
	})

	t.Run("ST-SUP-RP01-LC07-T01 A message resolved as acted records its delay", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("billing", "one")
		enqueue(t, db, q, m)
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) {
			c.Meter = provider
			c.Decider = &fakeDecider{allow: true}
			c.Auditor = tableAuditor{}
		})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		clock.Advance(5 * time.Second)
		// A resolution that the caller rolls back records no delay.
		tx, err := tenant.Begin(context.Background(), db, testTenant(t, "tenant-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Resolve(context.Background(), tx, ResolveRequest{ID: m.ID, Destination: m.Destination,
			Principal: person("user:ops-1"), Resolution: Acted}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if found := collect(t, reader, "outbox.delivery.delay"); len(found) != 0 {
			t.Fatalf("a rolled back resolution recorded %+v", found)
		}
		resolve(t, db, w, m.ID, Acted)
		found := collect(t, reader, "outbox.delivery.delay")
		if len(found) != 1 {
			t.Fatalf("outbox.delivery.delay is %+v", found)
		}
		histogram := found[0].Data.(metricdata.Histogram[float64])
		if len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 || histogram.DataPoints[0].Sum != 5 {
			t.Fatalf("the observation is %+v", histogram.DataPoints)
		}
	})

	t.Run("ST-SUP-RP01-LC07-T02 The oldest-age gauge shows a destination that does not deliver", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		enqueue(t, db, q, message("authz", "document:1"), message("authz", "document:1"), message("billing", "one"))
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider; c.AttemptTimeout = 5 * time.Second })
		entered, release := make(chan struct{}, 2), make(chan struct{})
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			entered <- struct{}{}
			<-release
			return nil
		}})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		blocked := make(chan int, 1)
		go func() { n, _ := w.cycle(context.Background(), context.Background()); blocked <- n }()
		<-entered
		if n := cycle(t, w); n != 1 || readRowByKey(t, db, "billing", "one").State != Done {
			t.Fatal("the billing message was not delivered")
		}
		clock.Advance(7 * time.Second)
		if ages := gaugeValues(t, reader); ages["authz"] != 7 || ages["billing"] != 0 || len(ages) != 2 {
			t.Fatalf("the gauge holds %v while the handler blocks", ages)
		}
		close(release)
		<-blocked
		cycle(t, w)
		if open := count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE state <> 'done'"); open != 0 {
			t.Fatalf("%d messages are not done", open)
		}
		if ages := gaugeValues(t, reader); ages["authz"] != 0 || ages["billing"] != 0 {
			t.Fatalf("the gauge holds %v after the delivery", ages)
		}
	})
}

func TestWorkerOldestAge(t *testing.T) {
	t.Run("ST-SUP-RP01-LC29-T01 OldestAge gives the age of the oldest message that is not done, and an error on a failed read", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		older := message("authz", "document:1")
		enqueue(t, db, q, older)
		clock.Advance(time.Second)
		enqueue(t, db, q, message("billing", "one"))
		clock.Advance(time.Second)
		enqueue(t, db, q, message("authz", "document:2"))
		clock.Advance(time.Second)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return nil
		}})
		ctx := context.Background()
		if age, err := w.OldestAge(ctx, "authz"); err != nil || age != 3*time.Second {
			t.Fatalf("OldestAge of authz before the delivery gave %v, %v", age, err)
		}
		if n := cycle(t, w); n != 1 || readRow(t, db, older.ID).State != Done {
			t.Fatal("the older authz message was not delivered")
		}
		for destination, want := range map[string]time.Duration{"authz": time.Second, "billing": 2 * time.Second, "mail": 0} {
			if age, err := w.OldestAge(ctx, destination); err != nil || age != want {
				t.Fatalf("OldestAge of %s gave %v, %v; want %v", destination, age, err, want)
			}
		}
		var role string
		if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
			t.Fatal(err)
		}
		if _, err := admin(t, db).ExecContext(ctx, `REVOKE EXECUTE ON FUNCTION stego_outbox.oldest(timestamptz) FROM "`+role+`"`); err != nil {
			t.Fatal(err)
		}
		age, err := w.OldestAge(ctx, "authz")
		if err == nil || age != 0 || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("OldestAge with the relay function revoked gave %v, %v", age, err)
		}
	})
}

func readRowByKey(t testing.TB, db *sql.DB, destination, key string) stored {
	t.Helper()
	var id uuid.UUID
	if err := admin(t, db).QueryRow(`SELECT id FROM stego_outbox.messages WHERE destination = $1 AND resource_key = $2
		ORDER BY sequence LIMIT 1`, destination, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return readRow(t, db, id)
}

// lazyQueue is a queue on a database handle that never connects, for the tests that need no server.
func lazyQueue(t *testing.T) *Queue {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newQueue(t, db, nil)
}

func TestWorkerRegistration(t *testing.T) {
	nop := func(context.Context, *sql.Tx, Delivery) error { return nil }
	lookup := func(context.Context, LookupRequest) (LookupStatus, error) { return StatusActed, nil }

	t.Run("ST-SUP-RP01-LC14-T01 A destination that does not say it is idempotent counts as not idempotent", func(t *testing.T) {
		w := newWorker(t, lazyQueue(t), nil)
		register(t, w, Destination{Name: "billing", Handler: nop})
		register(t, w, Destination{Name: "mail", Handler: nop, Idempotent: true, Lookup: lookup, SettleTime: time.Second})
		register(t, w, Destination{Name: "partner", Handler: nop, Lookup: lookup, SettleTime: time.Minute})
		register(t, w, Destination{Name: "authz", Handler: nop, Idempotent: true, MustLand: true})
		got := map[string]Registration{}
		for _, r := range w.Registrations() {
			got[r.Name] = r
		}
		if b := got["billing"]; b.Idempotent || b.MustLand || b.HasLookup || len(got) != 4 {
			t.Fatalf("billing is registered as %+v", b)
		}
		if m := got["mail"]; !m.Idempotent || !m.HasLookup || m.MustLand || m.SettleTime != time.Second {
			t.Fatalf("mail is registered as %+v", m)
		}
		if p, a := got["partner"], got["authz"]; p.Idempotent || !p.HasLookup || a.HasLookup || !a.Idempotent || !a.MustLand {
			t.Fatalf("partner and authz are registered as %+v and %+v", p, a)
		}
	})

	t.Run("ST-SUP-RP01-LC14-T02 A destination with no idempotency and no lookup is accepted", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		if err := w.Register(Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		_, result := startWorker(t, w)
		await(t, func() bool { return w.Stats().Delivered == 1 })
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatalf("the worker did not start and stop: %v", err)
		}
		if calls.Load() != 1 || readRow(t, db, m.ID).State != Done {
			t.Fatalf("the worker claimed no work: %d calls, %+v", calls.Load(), readRow(t, db, m.ID))
		}
	})

	t.Run("ST-SUP-RP01-LC14-T03 A status lookup without a positive settle time is refused", func(t *testing.T) {
		w := newWorker(t, lazyQueue(t), nil)
		for _, settle := range []time.Duration{0, -time.Second} {
			if err := w.Register(Destination{Name: "billing", Handler: nop, Lookup: lookup, SettleTime: settle}); err == nil {
				t.Fatalf("a status lookup with the settle time %v was accepted", settle)
			}
		}
		if got := w.Registrations(); len(got) != 0 {
			t.Fatalf("a refused destination is registered: %+v", got)
		}
	})
}

// recordingLookup is a status lookup that answers from a list and records each request.
type recordingLookup struct {
	mu       sync.Mutex
	answers  []lookupAnswer
	requests []LookupRequest
	clock    *fakeClock
	times    []time.Time
}

type lookupAnswer struct {
	status LookupStatus
	err    error
}

func (l *recordingLookup) lookup(_ context.Context, request LookupRequest) (LookupStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, request)
	if l.clock != nil {
		l.times = append(l.times, l.clock.Now())
	}
	if len(l.answers) == 0 {
		return StatusUnknown, nil
	}
	answer := l.answers[0]
	if len(l.answers) > 1 {
		l.answers = l.answers[1:]
	}
	return answer.status, answer.err
}

func (l *recordingLookup) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// runtimeUser is the role name of a runtime DSN.
func runtimeUser(t testing.TB, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Username()
}

func TestWorkerLimits(t *testing.T) {
	t.Run("ST-SUP-RP01-LC23-T01 The attempts limit defaults to 25 and is bounded", func(t *testing.T) {
		q := lazyQueue(t)
		if DefaultWorkerConfig().MaxAttempts != 25 {
			t.Fatalf("the default attempts limit is %d", DefaultWorkerConfig().MaxAttempts)
		}
		if w := newWorker(t, q, nil); w.config.MaxAttempts != 25 {
			t.Fatalf("a worker with no attempts limit has %d", w.config.MaxAttempts)
		}
		for limit, ok := range map[int64]bool{0: false, 1001: false, 1000: true, 1: true} {
			config := testConfig()
			config.MaxAttempts = limit
			if _, err := NewWorker(q, config); (err == nil) != ok {
				t.Fatalf("the attempts limit %d gave %v", limit, err)
			}
		}
	})
}

func TestWorkerDrain(t *testing.T) {
	t.Run("ST-SUP-RP01-LC26-T01 A handler in flight during StopClaiming ends in done", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m, next := message("audit", "one"), message("audit", "two")
		enqueue(t, db, q, m)
		entered := make(chan struct{})
		var calls atomic.Int32
		w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		register(t, w, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		}})
		_, result := startWorker(t, w)
		<-entered
		w.StopClaiming()
		enqueue(t, db, q, next)
		bound, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := w.Finish(bound); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if readRow(t, db, m.ID).State != Done || readRow(t, db, next.ID).State != Pending || calls.Load() != 1 {
			t.Fatalf("after StopClaiming: %+v, %+v, %d calls", readRow(t, db, m.ID), readRow(t, db, next.ID), calls.Load())
		}

		// A second worker gets a drain bound of 50 ms while its handler runs.
		w2 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 30 * time.Second; c.LeaseDuration = time.Minute })
		entered2 := make(chan struct{})
		var cancelled atomic.Bool
		register(t, w2, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered2)
			<-ctx.Done()
			cancelled.Store(true)
			return ctx.Err()
		}})
		_, result2 := startWorker(t, w2)
		<-entered2
		w2.StopClaiming()
		short, stopShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer stopShort()
		if err := w2.Finish(short); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Finish at its bound returned %v", err)
		}
		if err := <-result2; err != nil {
			t.Fatal(err)
		}
		if !cancelled.Load() || readRow(t, db, next.ID).State != OutcomeUnknown {
			t.Fatalf("the drain bound left %+v, cancelled %v", readRow(t, db, next.ID), cancelled.Load())
		}
	})
}

func TestWorkerLease(t *testing.T) {
	t.Run("ST-SUP-RP01-LC02-T04 A handler is not called when the lease left is short", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		clock := useHandClock(t, w)
		w.now = clock.now
		var calls atomic.Int32
		var handled context.Context
		reg := Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			calls.Add(1)
			handled = ctx
			return nil
		}}
		register(t, w, reg)
		claimed, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		d := claimed[0]
		// The lease on the local clock holds one nanosecond less than the attempt and the two statements after it.
		need := w.config.AttemptTimeout + 2*operationTimeout
		d.leaseEnd = clock.now().Add(need - time.Nanosecond)
		w.step(context.Background(), d)
		if row := readRow(t, db, m.ID); calls.Load() != 0 || row.State != Pending || row.Attempts != 0 || row.FailureCode != "lease-short" {
			t.Fatalf("a short lease gave %d calls and %+v", calls.Load(), row)
		}
		// A lease that holds exactly the attempt and the two statements calls the handler, with the attempt
		// deadline as its bound: the lease left less the two statements is not shorter.
		claimed, err = q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		d = claimed[0]
		d.leaseEnd = clock.now().Add(need)
		w.step(context.Background(), d)
		if calls.Load() != 1 || readRow(t, db, m.ID).State != Done {
			t.Fatalf("a lease of the attempt and two statements gave %d calls and %+v", calls.Load(), readRow(t, db, m.ID))
		}
		attempt := clock.next(t)
		if attempt.d != w.config.AttemptTimeout || handled != context.Context(attempt) {
			t.Fatalf("the handler got a bound of %v for a lease of %v, want %v", attempt.d, need, w.config.AttemptTimeout)
		}
	})

	t.Run("ST-SUP-RP01-LC02-T05 A message that another holder finished is a lost lease", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		reg := Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }}
		register(t, w, reg)
		claimed, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if ok, err := q.acknowledge(context.Background(), claimed[0].Receipt); !ok || err != nil {
			t.Fatalf("acknowledge: %v %v", ok, err)
		}
		if w.apply(context.Background(), nil, claimed[0], reg, Done, change{}) {
			t.Fatal("a done message was moved again")
		}
		if s := w.Stats(); s.LostLeases != 1 || s.DatabaseFailures != 0 {
			t.Fatalf("a done row counted as %+v", s)
		}
	})
}

func TestWorkerPool(t *testing.T) {
	t.Run("ST-SUP-RP01-LC05-T05 A pool with fewer than Concurrency + 2 connections is refused", func(t *testing.T) {
		q := lazyQueue(t)
		q.db.SetMaxOpenConns(3)
		config := testConfig()
		config.Concurrency = 2
		if _, err := NewWorker(q, config); err == nil {
			t.Fatal("a pool of 3 was accepted for a concurrency of 2")
		}
		q.db.SetMaxOpenConns(4)
		if _, err := NewWorker(q, config); err != nil {
			t.Fatalf("a pool of 4: %v", err)
		}
		q.db.SetMaxOpenConns(0)
		if _, err := NewWorker(q, config); err != nil {
			t.Fatalf("a pool with no limit: %v", err)
		}
	})
}

func TestWorkerBeginFailure(t *testing.T) {
	t.Run("ST-SUP-RP01-LC22-T01 A failed begin takes back no attempt", func(t *testing.T) {
		db, _ := testDatabase(t)
		m := message("audit", "one")
		enqueue(t, db, newQueue(t, db, nil), m)
		faulty := sql.OpenDB(faultConnector{database: db, failBegin: true})
		t.Cleanup(func() { _ = faulty.Close() })
		w := newWorker(t, newQueue(t, faulty, nil), nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}})
		if n := cycle(t, w); n != 1 {
			t.Fatalf("the cycle claimed %d", n)
		}
		if row := readRow(t, db, m.ID); calls.Load() != 0 || row.Attempts != 1 || w.Stats().DatabaseFailures == 0 {
			t.Fatalf("a failed begin left %+v with %d calls", row, calls.Load())
		}
	})
}

// tenantList is a fake tenant list: the tenants of active are active, and a read for failing fails.
type tenantList struct {
	active  map[string]bool
	failing string
	reads   atomic.Int64
}

func (l *tenantList) Active(_ context.Context, t tenant.Tenant) (bool, error) {
	l.reads.Add(1)
	if t.ID() == l.failing {
		return false, errors.New("the tenant list cannot be read")
	}
	return l.active[t.ID()], nil
}

func TestWorkerTenants(t *testing.T) {
	t.Run("ST-SUP-RC27-OC12-T03 A destination that serves only active tenants parks the message of a retired tenant before its handler", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		list := &tenantList{active: map[string]bool{"tenant-a": true}, failing: "tenant-c"}
		w := newWorker(t, q, func(c *WorkerConfig) { c.Tenants = list })
		var handled sync.Map
		handler := func(_ context.Context, _ *sql.Tx, d Delivery) error {
			handled.Store(d.Destination+" "+d.Tenant.ID(), true)
			return nil
		}
		register(t, w, Destination{Name: "after-slots", Idempotent: true, MustLand: true, ActiveTenants: true, Handler: handler})
		register(t, w, Destination{Name: "audit", Idempotent: true, MustLand: true, Handler: handler})
		enqueueAs(t, db, q, "tenant-a", message("after-slots", "a:1"))
		enqueueAs(t, db, q, "tenant-b", message("after-slots", "b:1"), message("audit", "b:2"))
		enqueueAs(t, db, q, "tenant-c", message("after-slots", "c:1"))
		for cycle(t, w) > 0 {
		}
		if row := readRowByKey(t, db, "after-slots", "a:1"); row.State != Done {
			t.Errorf("the message of the active tenant is %s, want done", row.State)
		}
		// The retired tenant b: its message for the destination of active tenants is parked before the handler,
		// also though the destination must land; a destination that does not ask still gets its message.
		if row := readRowByKey(t, db, "after-slots", "b:1"); row.State != Parked || row.FailureCode != "tenant-not-active" {
			t.Errorf("the message of the retired tenant is %s %s, want parked with tenant-not-active", row.State, row.FailureCode)
		}
		if _, ran := handled.Load("after-slots tenant-b"); ran {
			t.Error("the handler ran for the message of the retired tenant")
		}
		if row := readRowByKey(t, db, "audit", "b:2"); row.State != Done {
			t.Errorf("the message of the retired tenant for a destination that serves every tenant is %s, want done", row.State)
		}
		// A failed read of the list runs no handler and parks nothing: the message goes back to pending.
		if row := readRowByKey(t, db, "after-slots", "c:1"); row.State != Pending || row.FailureCode != "tenant-list-failed" {
			t.Errorf("the message whose tenant list read failed is %s %s, want pending with tenant-list-failed", row.State, row.FailureCode)
		}
		if _, ran := handled.Load("after-slots tenant-c"); ran {
			t.Error("the handler ran for a message whose tenant list read failed")
		}
		// With no tenant list in the configuration, nothing is checked.
		open := newWorker(t, q, nil)
		register(t, open, Destination{Name: "after-slots", Idempotent: true, MustLand: true, ActiveTenants: true, Handler: handler})
		enqueueAs(t, db, q, "tenant-d", message("after-slots", "d:1"))
		reads := list.reads.Load()
		for cycle(t, open) > 0 {
		}
		if row := readRowByKey(t, db, "after-slots", "d:1"); row.State != Done || list.reads.Load() != reads {
			t.Errorf("with no tenant list the message is %s after %d reads, want done after none", row.State, list.reads.Load()-reads)
		}
	})
}

func TestWorkerBulkhead(t *testing.T) {
	t.Run("ST-SUP-RP01-LC34-T01 A destination whose attempts block holds only its own slots, and the authz relay delivers", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		w := newWorker(t, q, func(c *WorkerConfig) { c.Concurrency = 4 })
		// Each event reports one handler call: the destination and, for kafka, how many of its calls are in flight.
		type event struct {
			destination string
			inFlight    int64
		}
		events := make(chan event, 64)
		release := make(chan struct{})
		var kafkaInFlight atomic.Int64
		register(t, w, Destination{Name: "kafka", Idempotent: true, MustLand: true,
			Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
				events <- event{destination: "kafka", inFlight: kafkaInFlight.Add(1)}
				defer kafkaInFlight.Add(-1)
				// The broker holds each request until the test releases it: the attempt deadline is on the hand clock,
				// which the test does not move.
				select {
				case <-release:
				case <-ctx.Done():
				}
				return TransientRefusal(errors.New("the broker refused"))
			}})
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true,
			Handler: func(context.Context, *sql.Tx, Delivery) error {
				events <- event{destination: "authz"}
				return nil
			}})
		// The kafka messages come first, each of its own key, so one pool for all destinations would claim them and
		// fill every slot.
		var authz []Message
		for i := 0; i < 8; i++ {
			enqueue(t, db, q, message("kafka", fmt.Sprintf("ticket:%d", i)))
		}
		if got := w.slots(); strings.Join(got, ",") != "authz,kafka,authz,kafka" {
			t.Fatalf("the slots are %v, not two of authz and two of kafka", got)
		}
		cancel, result := startWorker(t, w)
		blocked := 0
		check := func(e event) {
			blocked++
			if e.inFlight > 2 || blocked > 2 {
				t.Fatalf("kafka started attempt %d, with %d in flight, while its two slots are blocked", blocked, e.inFlight)
			}
		}
		for blocked < 2 {
			e := <-events
			if e.destination != "kafka" {
				t.Fatalf("a call of %s came while the queue held kafka messages only", e.destination)
			}
			check(e)
		}
		// Both slots of kafka are blocked now; the authz relay gets its tuples after that.
		for i := 0; i < 3; i++ {
			authz = append(authz, message("authz", fmt.Sprintf("tuple:%d", i)))
		}
		enqueue(t, db, q, authz...)
		for delivered := 0; delivered < 3; {
			if e := <-events; e.destination == "authz" {
				delivered++
			} else {
				check(e)
			}
		}
		// The handler returned; the move to done commits after it. The wait has no deadline: only the timeout of go
		// test bounds it, and the kafka slots stay blocked all the time.
		for w.Stats().Delivered < 3 {
			time.Sleep(time.Millisecond)
		}
		if n := kafkaInFlight.Load(); n != 2 {
			t.Fatalf("kafka holds %d slots, not its two", n)
		}
		for _, m := range authz {
			if got := readRow(t, db, m.ID); got.State != Done || got.Attempts != 1 {
				t.Fatalf("the authz message %s is %s after %d attempts while kafka blocks, not done after one", m.ResourceKey, got.State, got.Attempts)
			}
		}
		close(release)
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		cancel()
	})

	t.Run("ST-SUP-RP01-LC34-T02 Each destination has a slot of its own, and the slots stay within the checked limit", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		w := newWorker(t, q, nil) // Concurrency 1
		noop := func(context.Context, *sql.Tx, Delivery) error { return nil }
		for _, name := range []string{"kafka", "authz", "audit"} {
			register(t, w, Destination{Name: name, Handler: noop})
		}
		if got := w.slots(); strings.Join(got, ",") != "audit,authz,kafka" {
			t.Fatalf("with Concurrency 1 the slots of three destinations are %v, not one each", got)
		}
		w.config.Concurrency = 7
		if got := w.slots(); strings.Join(got, ",") != "audit,authz,kafka,audit,authz,kafka,audit" {
			t.Fatalf("with Concurrency 7 the slots are %v", got)
		}
		full := newWorker(t, q, nil)
		for i := 0; i < MaxBatchSize; i++ {
			register(t, full, Destination{Name: fmt.Sprintf("d%02d", i), Handler: noop})
		}
		if err := full.Register(Destination{Name: "one-more", Handler: noop}); err == nil {
			t.Fatalf("the worker registered a destination past %d", MaxBatchSize)
		}
		// A pool too small for a slot of each destination is refused at Run, with no claim.
		db.SetMaxOpenConns(4)
		t.Cleanup(func() { db.SetMaxOpenConns(12) })
		m := message("audit", "row:1")
		enqueue(t, db, q, m)
		// The context has ended already, so a Run that skips the check returns the cancellation and claims nothing.
		ended, end := context.WithCancel(context.Background())
		end()
		err := w.Run(ended)
		if err == nil || !strings.Contains(err.Error(), "the 7 slots of the destinations need 9") {
			t.Fatalf("Run on a pool of 4 connections for 7 slots gave %v", err)
		}
		if got := readRow(t, db, m.ID); got.State != Pending || got.Attempts != 0 {
			t.Fatalf("the refused Run claimed the message: %s after %d attempts", got.State, got.Attempts)
		}
	})

	t.Run("ST-SUP-RP01-LC34-T04 The claim of one destination reads the index of its destination's open messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		var others []Message
		for i := 0; i < 300; i++ {
			others = append(others, message("kafka", fmt.Sprintf("ticket:%d", i)))
		}
		for i := 0; i < len(others); i += MaxBatchSize {
			enqueue(t, db, q, others[i:min(i+MaxBatchSize, len(others))]...)
		}
		enqueue(t, db, q, message("authz", "tuple:1"))
		a := admin(t, db)
		ctx := context.Background()
		conn, err := a.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		// The plan of the read part of claim_destination, on statistics of a backlog of another destination.
		var plan []string
		for _, statement := range []string{`ANALYZE stego_outbox.messages`, `SET enable_seqscan = off`} {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := conn.QueryContext(ctx, `EXPLAIN SELECT m.id FROM stego_outbox.messages AS m
 WHERE m.destination = 'authz' AND m.state IN ('pending', 'outcome_unknown', 'reconciling') AND m.available_at <= now()
 ORDER BY m.sequence LIMIT 1`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		if text := strings.Join(plan, "\n"); !strings.Contains(text, "messages_destination_due") {
			t.Fatalf("the claim of one destination does not read the index messages_destination_due:\n%s", text)
		}
		if got, err := q.ClaimDestination(ctx, "authz", 1, time.Minute); err != nil || len(got) != 1 || got[0].Destination != "authz" {
			t.Fatalf("the claim of authz behind the kafka backlog gave %v, %v", got, err)
		}
	})

	t.Run("ST-SUP-RP01-LC34-T05 The lease of a destination that no worker registers expires", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		dropped := message("dropped", "row:1")
		enqueue(t, db, q, dropped)
		// A worker of an earlier release claimed the message and stopped; this release no longer registers its
		// destination.
		if got, err := q.Claim(context.Background(), 1, time.Minute); err != nil || len(got) != 1 {
			t.Fatalf("the claim gave %v, %v", got, err)
		}
		clock.Advance(2 * time.Minute)
		w := newWorker(t, q, func(c *WorkerConfig) { c.PurgeEvery = time.Second })
		register(t, w, Destination{Name: "authz", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		_, result := startWorker(t, w)
		// The purge timer moves the expired leases first and then purges, in the same tick. The wait has no deadline:
		// only the timeout of go test bounds it.
		for w.Stats().PurgeBatches == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if row := readRow(t, db, dropped.ID); row.State != OutcomeUnknown || row.FailureCode != "lease-expired" || row.Token.Valid {
			t.Fatalf("the expired lease of the dropped destination is %s with %q", row.State, row.FailureCode)
		}
	})

	t.Run("ST-SUP-RP01-LC34-T03 The claim of one destination leases and moves only its own messages", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		ctx := context.Background()
		first, second, other := message("kafka", "ticket:1"), message("kafka", "ticket:1"), message("authz", "tuple:1")
		unknown := message("retired", "row:1")
		enqueue(t, db, q, first, second, other, unknown)
		// The authz message is claimed and its lease expires.
		if got, err := q.ClaimDestination(ctx, "authz", 32, time.Minute); err != nil || len(got) != 1 || got[0].ID != other.ID {
			t.Fatalf("the claim of authz gave %v, %v", got, err)
		}
		clock.Advance(2 * time.Minute)
		got, err := q.ClaimDestination(ctx, "kafka", 32, time.Minute)
		if err != nil || len(got) != 1 || got[0].ID != first.ID || got[0].Destination != "kafka" || got[0].FailureCode != "" {
			t.Fatalf("the claim of kafka gave %v, %v; it takes the first message of the key only", got, err)
		}
		if s := readRow(t, db, other.ID); s.State != Dispatching {
			t.Fatalf("the claim of kafka moved the expired lease of authz to %s", s.State)
		}
		if s := readRow(t, db, unknown.ID); s.State != Pending || s.Attempts != 0 {
			t.Fatalf("the claim of kafka took the message of another destination: %s after %d attempts", s.State, s.Attempts)
		}
		// A refused attempt keeps its failure code, which the next claim of the destination gives.
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "kafka", Idempotent: true, MustLand: true,
			Handler: func(context.Context, *sql.Tx, Delivery) error { return TransientRefusal(errors.New("refused")) }})
		w.step(ctx, got[0])
		clock.Advance(time.Hour)
		again, err := q.ClaimDestination(ctx, "kafka", 32, time.Minute)
		if err != nil || len(again) != 1 || again[0].ID != first.ID || again[0].FailureCode != "delivery-refused" || again[0].Attempts != 2 {
			t.Fatalf("the claim after a refusal gave %+v, %v", again, err)
		}
		if _, err := q.ClaimDestination(ctx, "not a label", 1, time.Minute); err == nil {
			t.Fatal("the claim took a destination that is not a label")
		}
		// The claim of authz moves its own expired lease.
		if got, err := q.ClaimDestination(ctx, "authz", 32, time.Minute); err != nil || len(got) != 0 {
			t.Fatalf("the claim of authz after the expiry gave %v, %v", got, err)
		}
		if s := readRow(t, db, other.ID); s.State != OutcomeUnknown || s.FailureCode != "lease-expired" {
			t.Fatalf("the expired lease of authz is %s with %q", s.State, s.FailureCode)
		}
	})
}
