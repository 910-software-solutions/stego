package command

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// tick is the tick of the worker tests: due at 1793966400 seconds since the Unix epoch, a multiple of 1m.
var tick = time.Unix(1793966400, 0)

func (e *env) worker(name, permission string, change func(*WorkerSpec[testProbe]), writes ...Declared) *Worker[testProbe] {
	e.t.Helper()
	spec := WorkerSpec[testProbe]{Name: name, Permission: permission, Interval: time.Minute, Writes: writes, Probe: newTestProbe,
		Destinations: []Destination{{Name: "partner-orders", Idempotent: true}, {Name: "billing", Idempotent: true}}}
	if change != nil {
		change(&spec)
	}
	w, err := NewWorker(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// tickFill adapts a tick fill to RunWorker.
// handRunClock is a clock of the time left before a deadline that the test moves itself (ST-SUP-RP10-LC22,
// ST-SUP-RP10-LC28). Each deadline that it gives is sent on made and ends with context.DeadlineExceeded when the test
// moves the clock to it. A wait that the runtime asks of it passes at once: it moves the clock by the wait.
type handRunClock struct {
	mu   sync.Mutex
	now  time.Time
	ends []*handDeadline
	made chan *handDeadline
}

func newHandRunClock(start time.Time) *handRunClock {
	return &handRunClock{now: start, made: make(chan *handDeadline, 64)}
}

func (c *handRunClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *handRunClock) WithDeadline(parent context.Context, at time.Time) (context.Context, context.CancelFunc) {
	d := &handDeadline{Context: parent, at: at, done: make(chan struct{})}
	stop := context.AfterFunc(parent, func() { d.end(parent.Err()) })
	c.mu.Lock()
	c.ends = append(c.ends, d)
	reached := !c.now.Before(at)
	c.mu.Unlock()
	if reached {
		d.end(context.DeadlineExceeded)
	}
	select {
	case c.made <- d:
	default:
	}
	return d, func() { stop(); d.end(context.Canceled) }
}

// After moves the clock by d and gives a channel that holds the time then.
func (c *handRunClock) After(d time.Duration) <-chan time.Time {
	now := c.Now().Add(d)
	c.advance(now)
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

// add moves the clock by d.
func (c *handRunClock) add(d time.Duration) { c.advance(c.Now().Add(d)) }

// advance moves the clock to now and ends each deadline that it reached.
func (c *handRunClock) advance(now time.Time) {
	c.mu.Lock()
	c.now = now
	ends := append([]*handDeadline(nil), c.ends...)
	c.mu.Unlock()
	for _, d := range ends {
		if !now.Before(d.at) {
			d.end(context.DeadlineExceeded)
		}
	}
}

// handDeadline is a context of the hand clock: it ends at its deadline on the clock, or when its parent ends.
type handDeadline struct {
	context.Context
	at   time.Time
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func (d *handDeadline) end(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
		close(d.done)
	}
}

func (d *handDeadline) Deadline() (time.Time, bool) { return d.at, true }
func (d *handDeadline) Done() <-chan struct{}       { return d.done }
func (d *handDeadline) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func tickFill(fill func(ctx context.Context, h *Handle[testProbe]) error) TickFill[testProbe] {
	return func(ctx context.Context, _ time.Time, h *Handle[testProbe]) error { return fill(ctx, h) }
}

// updateOrder sets the status of the order id through the handle.
func updateOrder(ctx context.Context, h *Handle[testProbe], e *Entity[order], id, status string) (Loaded[order], error) {
	r, err := Repo(h, e)
	if err != nil {
		return nil, err
	}
	o, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v := o.Value()
	v.Status = status
	return r.Update(ctx, o, v)
}

// testClock is the injected clock of the outbox in the consumer tests: the system clock, which the test moves on
// with add. While the test holds it, it stands still and moves only with add, so a message that waits on the clock
// is not dispatched again until the test moves it.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
	held   bool
	at     time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held {
		return c.at.Add(c.offset)
	}
	return time.Now().Add(c.offset)
}

// hold stops the clock at its time now. The function it gives lets the clock run on from the time it shows.
func (c *testClock) hold() (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held, c.at = true, time.Now()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.held {
			c.held, c.offset = false, c.at.Add(c.offset).Sub(time.Now())
		}
	}
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// consumer runs the outbox worker of the worker role with the destination partner-replies, whose handler calls
// RunConsumer for w with fill, until the test ends. The other destinations complete at once.
func (e *env) consumer(w *Worker[testProbe], change func(*outbox.WorkerConfig),
	fill MessageFill[map[string]string, testProbe]) {
	e.t.Helper()
	config := outbox.DefaultWorkerConfig()
	config.Concurrency, config.PollInterval = 1, 10*time.Millisecond
	config.RetryMin, config.RetryMax, config.AttemptTimeout = 10*time.Millisecond, 50*time.Millisecond, 5*time.Second
	if change != nil {
		change(&config)
	}
	worker, err := outbox.NewWorker(e.wqueue, config)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.consumed == nil {
		e.consumed = &observed{}
	}
	decode := func(payload []byte) (map[string]string, error) {
		var m map[string]string
		return m, json.Unmarshal(payload, &m)
	}
	// As the generated code does, the consumed destination is idempotent and must land (ST-SUP-RC35-OC06).
	destinations := []outbox.Destination{{Name: "partner-replies", Idempotent: true, MustLand: true,
		Handler: func(ctx context.Context, tx *sql.Tx, d outbox.Delivery) error {
			return ObserveConsumer(ctx, e.consumed, RunConsumer(ctx, e.rt, w, tx, d, decode, fill))
		}}}
	for _, name := range []string{"audit-sink", "billing", "partner-orders"} {
		destinations = append(destinations, outbox.Destination{Name: name, Idempotent: true,
			Handler: func(context.Context, *sql.Tx, outbox.Delivery) error { return nil }})
	}
	for _, d := range destinations {
		if err := worker.Register(d); err != nil {
			e.t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

// stage runs the command reserve-and-charge as user:u-7 in tenant tenantID, whose fill stages a message for
// partner-replies for the order orderID with payload, and returns the message's ID.
func (e *env) stage(tenantID, orderID string, payload map[string]string) uuid.UUID {
	e.t.Helper()
	cmd := e.command("reserve-and-charge", nil, e.orders)
	body, _ := json.Marshal(payload)
	if _, err := e.run(cmd, e.request(tenantID, u7(e.t), "k-stage-"+uuid.NewString(), `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
		o, err := repo(e.t, h, e.orders).Get(ctx, orderID)
		if err != nil {
			return err
		}
		return h.Effects().Stage(ctx, "partner-replies", "reply-received", o, body)
	})); err != nil {
		e.t.Fatal(err)
	}
	var id uuid.UUID
	if err := e.admin.QueryRowContext(e.ctx, `SELECT id FROM stego_outbox.messages WHERE destination = 'partner-replies'
 AND tenant_id = $1 ORDER BY sequence DESC LIMIT 1`, tenantID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// waitState waits until the message id is in state, and fails the test after 10 s.
func (e *env) waitState(t *testing.T, id uuid.UUID, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND state = $2`, id, state) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got string
	_ = e.admin.QueryRowContext(e.ctx, `SELECT state || ' ' || failure_code || ' attempts ' || attempts || ' conflicts ' || conflicts
 || ' available ' || available_at FROM stego_outbox.messages WHERE id = $1`, id).Scan(&got)
	t.Fatalf("the message %s is %s, not %s", id, got, state)
}

// redeliver makes a done message pending again, as a dispatch again after a crash before its state was recorded.
func (e *env) redeliver(id uuid.UUID) {
	e.t.Helper()
	e.exec(fmt.Sprintf(`UPDATE stego_outbox.messages SET state = 'pending', available_at = now(), done_at = NULL
 WHERE id = '%s'`, id))
}

func TestWorkers(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-1', 'new'), ('t1', 'o-2', 'new'), ('t2', 'o-1', 'new'),
 ('t2', 'o-2', 'new')`,
		`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 1, '0000')`,
		`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r-w2', 'o-2', 's'), ('t1', 'r-w1', 'o-1', 's')`)
	allOrders, err := NewList(e.orders, ListSpec{Name: "all",
		Read: `SELECT id, status, version, authz_token FROM orders WHERE tenant_id = $1 ORDER BY id LIMIT $2`})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ST-SUP-RP10-LC17-T01 A worker handle reads only allowed rows and writes only allowed resources", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", func(s *WorkerSpec[testProbe]) { s.Lists = []AnyList{allOrders} },
			e.orders, e.underOrder, e.typedRes)
		var o2 Loaded[order]
		var listed []string
		var writeCalls []evaluation
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			var err error
			// The value of o-2 is loaded while the port still allows it, so the fill holds one to write.
			if o2, err = r.Get(ctx, "o-2"); err != nil {
				return err
			}
			e.decision.setAllow(func(subject, action, resource string) bool {
				return !(resource == "order:o-2" && action == "sync")
			})
			list, err := r.List(ctx, allOrders, 10)
			if err != nil {
				return err
			}
			for _, o := range list {
				listed = append(listed, o.ID())
			}
			e.decision.taken()
			if _, err := updateOrderFrom(ctx, r, list[0], "synced"); err != nil {
				return err
			}
			if _, err := updateOrderFrom(ctx, r, o2, "synced"); !errors.Is(err, ErrRefused) {
				t.Errorf("the update of o-2 gave %v, want the refused error", err)
			}
			if _, err := repo(t, h, e.underOrder).Create(ctx, reservation{ID: "r-new", OrderID: "o-2", SKU: "s"}, o2); !errors.Is(err, ErrRefused) {
				t.Errorf("the create under o-2 gave %v, want the refused error", err)
			}
			writeCalls = e.decision.taken()
			// A child row takes the worker's decision on its parent too (ST-SUP-RP10-LC05).
			if _, err := repo(t, h, e.underOrder).GetForUpdate(ctx, "r-w2", o2); !errors.Is(err, ErrRefused) {
				t.Errorf("the locked read of a child of o-2 gave %v, want the refused error", err)
			}
			child, err := repo(t, h, e.underOrder).Get(ctx, "r-w2")
			if err != nil {
				return err
			}
			if err := repo(t, h, e.underOrder).Delete(ctx, child, o2); !errors.Is(err, ErrRefused) {
				t.Errorf("the delete of a child of o-2 gave %v, want the refused error", err)
			}
			if err := h.Effects().Stage(ctx, "billing", "noted", child, []byte(`{}`), o2); !errors.Is(err, ErrRefused) {
				t.Errorf("an effect for a child of o-2 gave %v, want the refused error", err)
			}
			// A row with a resource type of its own moves under o-2 only with the worker's decision on o-2.
			typed := repo(t, h, e.typedRes)
			r1, err := typed.GetForUpdate(ctx, "r-w1")
			if err != nil {
				return err
			}
			if _, err := typed.Update(ctx, r1, reservation{ID: "r-w1", OrderID: "o-2", SKU: "s"}, o2); !errors.Is(err, ErrRefused) {
				t.Errorf("moving r-w1 under o-2 gave %v, want the refused error", err)
			}
			return nil
		}))
		e.decision.setAllow(nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(listed, ",") != "o-1" {
			t.Fatalf("the list holds %v, want o-1 only", listed)
		}
		if e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND status = 'synced'`) != 1 ||
			e.count(`SELECT count(*) FROM orders WHERE id = 'o-new'`) != 0 {
			t.Fatal("the writes are not o-1 alone")
		}
		if len(writeCalls) != 3 {
			t.Fatalf("the three writes made the decisions %+v, want one each", writeCalls)
		}
		for _, c := range writeCalls {
			if c.Subject != "service_account:sync-partner-orders" || c.Action != "sync" {
				t.Fatalf("a write was decided as %+v", c)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC18-T01 A worker tick commits its writes and effects together or not at all", func(t *testing.T) {
		w := e.worker("tick-orders", "sync", nil, e.orders)
		for i, end := range []string{"nil", "error", "panic"} {
			e.exec(`UPDATE orders SET status = 'new' WHERE tenant_id = 't2' AND id = 'o-1'`)
			outbox, audits := e.outboxRows("t2", "partner-orders"), e.auditRows("t2", "tick-orders")
			calls := 0
			err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Duration(i)*time.Minute), mustTenant(t, "t2"), nil,
				tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
					calls++
					o, err := updateOrder(ctx, h, e.orders, "o-1", "ticked")
					if err != nil {
						return err
					}
					if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
						return err
					}
					switch end {
					case "error":
						return errors.New("refused")
					case "panic":
						panic("secret-42")
					}
					return nil
				}))
			wrote := e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't2' AND id = 'o-1' AND status = 'ticked'`) +
				e.outboxRows("t2", "partner-orders") - outbox + e.auditRows("t2", "tick-orders") - audits
			if calls != 1 {
				t.Fatalf("%s: the fill ran %d times", end, calls)
			}
			if end == "nil" && (err != nil || wrote != 3) {
				t.Fatalf("the tick that returned nil gave %v and left %d of the 3 rows", err, wrote)
			}
			if end != "nil" && (err == nil || wrote != 0 || strings.Contains(err.Error(), "secret-42")) {
				t.Fatalf("the tick that ended with %s gave %v and left %d rows", end, err, wrote)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC21-T01 Two replicas that run one tick stage one key for each entity", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		stageBoth := tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			for _, id := range []string{"o-1", "o-2"} {
				o, err := r.Get(ctx, id)
				if err != nil {
					return err
				}
				if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
					return err
				}
			}
			return nil
		})
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'partner-orders'`)
		var replicas sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			replicas.Add(1)
			go func() {
				defer replicas.Done()
				errs <- RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, stageBoth)
			}()
		}
		replicas.Wait()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		o1 := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-1")
		o2 := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-2")
		want := idempotency.Derive("t1", "service_account:sync-partner-orders", lp("sync-partner-orders", "1793966400", "o-1"),
			"partner-orders", "order-changed")
		if len(o1) != 2 || o1[0] != want || o1[1] != want || len(o2) != 2 || o2[0] != o2[1] || o2[0] == want {
			t.Fatalf("the replicas' keys are %v for o-1 and %v for o-2, want %s twice and another key twice", o1, o2, want)
		}
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, e.orders).Get(ctx, "o-1")
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err == nil {
				t.Error("a second order-changed for o-1 in one run was staged")
			}
			return errors.New("roll back")
		}))
		if err == nil {
			t.Fatal("the run committed")
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), nil, stageBoth); err != nil {
			t.Fatal(err)
		}
		next := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-1")
		if len(next) != 3 || next[2] == want {
			t.Fatalf("the next tick gave the keys %v for o-1, want a new key", next)
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Second), mustTenant(t, "t1"), nil, stageBoth); err == nil {
			t.Fatal("a tick that is not due at a multiple of the interval ran")
		}
	})

	t.Run("ST-SUP-RP10-LC20-T01 A run with a stale lease token is fenced, and a live run holds back a new holder", func(t *testing.T) {
		const name = "stego.worker.sync-partner-orders"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE leases SET token = 3 WHERE name = '` + name + `'`)
		h1, err := lease.New(e.pool, name, "h1", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		held, err := h1.Acquire(e.ctx)
		if err != nil || held.Token() != 4 {
			t.Fatalf("h1 acquired token %v, %v; want 4", held, err)
		}
		// The lease expires in the row, while h1's run holds it.
		e.exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = '` + name + `'`)
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		running, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				close(running)
				<-release
				_, err := updateOrder(ctx, h, e.orders, "o-1", "fenced-run")
				return err
			}))
		}()
		<-running
		h2, err := lease.New(e.pool, name, "h2", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		var committed atomic.Bool
		acquired := make(chan *lease.Held, 1)
		go func() {
			got, err := h2.Acquire(e.ctx)
			if err != nil || !committed.Load() {
				t.Errorf("h2's acquire gave %v before the run committed (%t)", err, committed.Load())
			}
			acquired <- got
		}()
		e.waitLockWaits(1)
		committed.Store(true)
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if got := <-acquired; got == nil || got.Token() != 5 {
			t.Fatalf("h2 holds %v, want token 5", got)
		}
		calls := 0
		err = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			_, err := updateOrder(ctx, h, e.orders, "o-1", "stale-run")
			return err
		}))
		if !errors.Is(err, lease.ErrFenced) || calls != 0 || e.count(`SELECT count(*) FROM orders WHERE status = 'stale-run'`) != 0 {
			t.Fatalf("the run with token 4 gave %v after %d calls", err, calls)
		}
	})

	t.Run("ST-SUP-RP10-LC22-T01 A tenant's run that would outlive the lease is rolled back before expiry and retried", func(t *testing.T) {
		const name = "stego.worker.long-run"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		client, err := lease.New(e.pool, name, "h1", lease.Settings{TTL: 15 * time.Second, Renew: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		held, err := client.Acquire(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		rt := e.runtime(nil)
		clock := newHandRunClock(time.Now())
		rt.runClock = clock
		var commitAttempted atomic.Bool
		rt.beforeCommit = func(context.Context, *sql.Tx) error { commitAttempted.Store(true); return nil }
		w := e.worker("long-run", "sync", nil, e.reservationE)
		// holdRun runs RunWorker with a fill that writes the reservation id and then holds until its context ends. It
		// gives the deadline of the run and the fill's context, or fails when the run ends first.
		holdRun := func(id string) (*handDeadline, context.Context, <-chan error) {
			t.Helper()
			wrote := make(chan context.Context, 1)
			result := make(chan error, 1)
			go func() {
				result <- RunWorker(e.ctx, rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
					if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: id, OrderID: "o-1", SKU: "s"}, nil); err != nil {
						return err
					}
					wrote <- ctx
					<-ctx.Done()
					return ctx.Err()
				}))
			}()
			var d *handDeadline
			select {
			case d = <-clock.made:
			case err := <-result:
				t.Fatalf("the run of %s ended with %v and asked for no deadline", id, err)
			}
			select {
			case ctx := <-wrote:
				return d, ctx, result
			case err := <-result:
				t.Fatalf("the run of %s ended with %v before its fill wrote", id, err)
			}
			return nil, nil, nil
		}
		// The deadline is the lease TTL less the renew interval after the transaction starts, 5 s, which is before
		// expires_at. The run does not end 1 ns before it and rolls back at it.
		start := clock.Now()
		d, fillCtx, result := holdRun("r-long")
		if want := start.Add(5 * time.Second); !d.at.Equal(want) || !d.at.Before(held.Expires()) {
			t.Fatalf("the run's deadline is %s after the start, want 5 s and before expires_at %s", d.at.Sub(start), held.Expires().Sub(start))
		}
		clock.advance(d.at.Add(-time.Nanosecond))
		if fillCtx.Err() != nil {
			t.Fatal("the run ended 1 ns before its deadline")
		}
		clock.advance(d.at)
		err = <-result
		if !errors.Is(err, ErrDeadline) || commitAttempted.Load() || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-long'`) != 0 {
			t.Fatalf("the long run gave %v at its deadline, commit attempted %t", err, commitAttempted.Load())
		}
		err = RunWorker(e.ctx, rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-long", OrderID: "o-1", SKU: "s"}, nil)
			return err
		}))
		if err != nil || !commitAttempted.Load() || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-long'`) != 1 {
			t.Fatalf("the next tick of t1 gave %v", err)
		}
		// A run that starts 1 s before expires_at has the deadline expires_at, not 5 s after its start.
		<-clock.made
		commitAttempted.Store(false)
		clock.advance(held.Expires().Add(-time.Second))
		d, _, result = holdRun("r-expiry")
		if !d.at.Equal(held.Expires()) {
			t.Fatalf("a run 1 s before expires_at has its deadline %s after expires_at", d.at.Sub(held.Expires()))
		}
		clock.advance(d.at)
		if err := <-result; !errors.Is(err, ErrDeadline) || commitAttempted.Load() || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-expiry'`) != 0 {
			t.Fatalf("the run at expires_at gave %v, commit attempted %t", err, commitAttempted.Load())
		}
	})

	t.Run("ST-SUP-RP10-LC26-T01 A worker conflict runs the tenant in the next tick, and a consumer conflict is dispatched again and is not an attempt", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		calls := map[string]int{}
		fill := func(conflictTenant string) TickFill[testProbe] {
			return tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				id := h.Tenant().ID()
				calls[id]++
				r := repo(t, h, e.orders)
				o, err := r.Get(ctx, "o-2")
				if err != nil {
					return err
				}
				if id == conflictTenant {
					e.exec(`UPDATE orders SET version = version + 1 WHERE tenant_id = '` + id + `' AND id = 'o-2'`)
				}
				v := o.Value()
				v.Status = "tick-" + id
				_, err = r.Update(ctx, o, v)
				return err
			})
		}
		results := map[string]error{}
		for _, id := range []string{"t1", "t2"} {
			results[id] = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, id), nil, fill("t1"))
		}
		if !errors.Is(results["t1"], ErrConflict) || results["t2"] != nil || calls["t1"] != 1 ||
			e.count(`SELECT count(*) FROM orders WHERE id = 'o-2' AND status LIKE 'tick-%'`) != 1 {
			t.Fatalf("the first tick gave %v after %d calls for t1", results, calls["t1"])
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), nil, fill("")); err != nil {
			t.Fatalf("the next tick of t1 gave %v", err)
		}
		if e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND id = 'o-2' AND status = 'tick-t1'`) != 1 {
			t.Fatal("the next tick did not commit t1")
		}
	})
}

// updateOrderFrom writes status over a loaded order.
func updateOrderFrom(ctx context.Context, r *Repository[order], o Loaded[order], status string) (Loaded[order], error) {
	v := o.Value()
	v.Status = status
	return r.Update(ctx, o, v)
}

// retiredTenants is a fake tenant list in which every tenant is active but the ones it holds as retired.
type retiredTenants struct{ retired sync.Map }

func (l *retiredTenants) Active(_ context.Context, t tenant.Tenant) (bool, error) {
	_, gone := l.retired.Load(t.ID())
	return !gone, nil
}

func TestConsumers(t *testing.T) {
	clock := &testClock{}
	e := newEnv(t, func(c *envConfig) { c.queueClock = clock })
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-1', 'new'), ('t1', 'o-2', 'new'),
 ('t1', 'o-3', 'new'), ('t2', 'o-1', 'new')`,
		`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 1, '0000')`)
	// cases holds the fill of a message whose payload names a case, for the subtests that need one of their own.
	var cases sync.Map
	var mode atomic.Value
	mode.Store("ok")
	var calls atomic.Int64
	// pendingOrders lists the orders of ST-SUP-RP10-LC17-T06, whose relations the relay writes late.
	pendingOrders, err := NewList(e.orders, ListSpec{Name: "pending-orders",
		Read: `SELECT id, status, version, authz_token FROM orders WHERE tenant_id = $1 AND id LIKE 'o-rp%' ORDER BY id LIMIT $2`})
	if err != nil {
		t.Fatal(err)
	}
	w := e.worker("apply-partner-replies", "apply", func(s *WorkerSpec[testProbe]) {
		s.Interval = 0
		s.Stagers = map[string]string{"reserve-and-charge": "reserve"}
		s.Lists = []AnyList{pendingOrders}
	}, e.orders, e.chargeE, e.reservationE)
	// The tenant list of the consumers: every tenant is active but the ones a subtest retires.
	retired := &retiredTenants{}
	e.rt.cfg.Tenants = retired
	// The log of the consumer attempts that do not commit (ST-SUP-RC35-OC06), set before the outbox worker starts.
	consumerLogs := &logRecorder{}
	e.rt.logger = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(consumerLogs))).Logger("test")
	e.consumer(w, func(c *outbox.WorkerConfig) { c.MaxAttempts = 3 }, func(ctx context.Context, m map[string]string, _ string, h *Handle[testProbe]) error {
		calls.Add(1)
		if fill, ok := cases.Load(m["case"]); ok {
			return fill.(func(context.Context, map[string]string, *Handle[testProbe]) error)(ctx, m, h)
		}
		r := repo(t, h, e.orders)
		o, err := r.Get(ctx, m["order"])
		if err != nil {
			return err
		}
		c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
		if err != nil {
			return err
		}
		if err := h.Effects().Stage(ctx, "billing", "charge-requested", c, []byte(`{}`)); err != nil {
			return err
		}
		switch mode.Load() {
		case "conflict-once":
			mode.Store("ok")
			e.exec(`UPDATE orders SET version = version + 1 WHERE id = '` + m["order"] + `'`)
		case "conflict-always":
			e.exec(`UPDATE orders SET version = version + 1 WHERE id = '` + m["order"] + `'`)
		}
		v := o.Value()
		v.Status = "applied-" + m["n"]
		_, err = r.Update(ctx, o, v)
		return err
	})
	chargeKeys := func() []string { return e.outgoingKey("t1", "billing", "charge-requested", "charge:c1") }
	derive := func(message uuid.UUID) string {
		return idempotency.Derive("t1", "service_account:apply-partner-replies", lp(message.String(), "c1"), "billing", "charge-requested")
	}

	t.Run("ST-SUP-RP10-LC19-T01 A message delivered twice runs the consumer fill once", func(t *testing.T) {
		calls.Store(0)
		m1 := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "1"})
		e.waitState(t, m1, "done")
		e.redeliver(m1)
		e.waitState(t, m1, "done")
		m2 := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "2"})
		e.waitState(t, m2, "done")
		if calls.Load() != 2 {
			t.Fatalf("the fill ran %d times, want for the first m-1 and for m-2 only", calls.Load())
		}
		if e.keyRows(m1.String()) != 1 || e.keyRows(m2.String()) != 1 {
			t.Fatal("each message does not have one key row")
		}
		keys := chargeKeys()
		if len(keys) != 2 || keys[0] != derive(m1) || keys[1] != derive(m2) {
			t.Fatalf("the effects carry %v, want the keys of m-1 and m-2", keys)
		}
	})

	t.Run("ST-SUP-RP10-LC08-T02 An effect that a consumer stages is keyed by the message identity and its entity", func(t *testing.T) {
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'billing'`)
		m1 := e.stage("t1", "o-2", map[string]string{"order": "o-2", "n": "3"})
		e.waitState(t, m1, "done")
		if keys := chargeKeys(); len(keys) != 1 || keys[0] != derive(m1) {
			t.Fatalf("the staged row carries %v, want %s", keys, derive(m1))
		}
		e.redeliver(m1)
		e.waitState(t, m1, "done")
		if keys := chargeKeys(); len(keys) != 1 {
			t.Fatalf("the second run staged %d rows, want none", len(keys)-1)
		}
	})

	t.Run("ST-SUP-RP10-LC26-T01 A worker conflict runs the tenant in the next tick, and a consumer conflict is dispatched again and is not an attempt", func(t *testing.T) {
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'billing'`)
		// A conflict makes the message wait 100 ms on the clock. On the system clock, a slow test sees the message
		// done, or a dispatch again in progress that counts an attempt until its conflict is recorded. The held clock
		// moves only when the test moves it, so each read below sees the message as it waits.
		defer clock.hold()()
		calls.Store(0)
		mode.Store("conflict-once")
		m := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "4"})
		deadline := time.Now().Add(5 * time.Second)
		for calls.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		var attempts, conflicts int
		var wait time.Duration
		e.waitState(t, m, "pending")
		var waitMS float64
		if err := e.admin.QueryRowContext(e.ctx, `SELECT attempts, conflicts,
 extract(epoch FROM available_at - dispatched_at) * 1000 FROM stego_outbox.messages WHERE id = $1`, m).
			Scan(&attempts, &conflicts, &waitMS); err != nil {
			t.Fatal(err)
		}
		wait = time.Duration(waitMS * float64(time.Millisecond))
		// The wait after the conflict starts at 100 ms on the injected clock, from the dispatch.
		if attempts != 0 || conflicts != 1 || wait < 100*time.Millisecond || e.keyRows(m.String()) != 0 ||
			len(chargeKeys()) != 0 {
			t.Fatalf("after the conflict: %d attempts, %d conflicts, due %s after the dispatch, %d key rows, %d effects",
				attempts, conflicts, wait, e.keyRows(m.String()), len(chargeKeys()))
		}
		clock.add(time.Second)
		e.waitState(t, m, "done")
		if keys := chargeKeys(); calls.Load() != 2 || len(keys) != 1 || keys[0] != derive(m) {
			t.Fatalf("the dispatch again: %d calls, keys %v", calls.Load(), keys)
		}
		calls.Store(0)
		// A message of a destination that must land is never parked at the attempts limit (ST-SUP-RP01-LC23): after
		// 20 conflicts that count no attempt, each conflict counts one, past the limit of 3, and it stays pending.
		mode.Store("conflict-always")
		pending := e.stage("t1", "o-2", map[string]string{"order": "o-2", "n": "5"})
		// The clock moves on only while the message waits in pending, to the time it is due, so no lease of a
		// dispatch expires on the clock.
		var state string
		var attempts64 int64
		for i := 0; i < 2000; i++ {
			var waitMS float64
			if err := e.admin.QueryRowContext(e.ctx, `SELECT state, attempts, extract(epoch FROM available_at) * 1000 FROM stego_outbox.messages WHERE id = $1`, pending).
				Scan(&state, &attempts64, &waitMS); err != nil {
				t.Fatal(err)
			}
			if state == "parked" || (calls.Load() >= 26 && state == "pending") {
				break
			}
			if state == "pending" {
				if due := time.UnixMilli(int64(waitMS)).Sub(clock.Now()); due > 0 {
					clock.add(due + time.Millisecond)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		mode.Store("ok")
		if state != "pending" || attempts64 < 4 {
			t.Fatalf("after %d calls the message is %s with %d attempts, want pending with its attempts past the limit of 3",
				calls.Load(), state, attempts64)
		}
		clock.add(time.Hour)
		e.waitState(t, pending, "done")

		// A delivery of the message that is in progress elsewhere holds the key row: the consumer's key insert waits
		// until its lock timeout, and the outbox dispatches the message again with a conflict, not an attempt.
		calls.Store(0)
		a := e.session()
		// The key row that the session holds blocks each delivery of the message, so it ends also when a check
		// fails, and the later subtests that stage for o-3 do not wait behind the message.
		defer func() { _ = a.tx.Rollback() }()
		var held uuid.UUID
		stager := e.runtime(nil)
		stager.beforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT id FROM stego_outbox.messages WHERE destination = 'partner-replies'
 ORDER BY sequence DESC LIMIT 1`).Scan(&held); err != nil {
				return err
			}
			a.exec(`INSERT INTO idempotency_keys (tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
 VALUES ('t1', 'service_account:apply-partner-replies', $1, 'f', 'in_progress', now(), now() + interval '1 hour')`, held.String())
			return nil
		}
		if _, err := e.runOn(stager, e.command("reserve-and-charge", nil, e.orders), e.request("t1", u7(t), "k-held", `{}`),
			fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				o, err := repo(t, h, e.orders).Get(ctx, "o-3")
				if err != nil {
					return err
				}
				return h.Effects().Stage(ctx, "partner-replies", "reply-received", o, []byte(`{"order":"o-3","n":"6"}`))
			})); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND conflicts > 0`, held) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if err := e.admin.QueryRowContext(e.ctx, `SELECT attempts, conflicts FROM stego_outbox.messages WHERE id = $1`, held).
			Scan(&attempts, &conflicts); err != nil {
			t.Fatal(err)
		}
		if attempts != 0 || conflicts < 1 || calls.Load() != 0 {
			t.Fatalf("a delivery in progress elsewhere gave %d attempts, %d conflicts and %d calls; want a conflict and no call",
				attempts, conflicts, calls.Load())
		}
		_ = a.tx.Rollback()
		clock.add(time.Hour)
		e.waitState(t, held, "done")
		if calls.Load() != 1 {
			t.Fatalf("the dispatch again called the fill %d times, want once", calls.Load())
		}
	})

	t.Run("ST-SUP-RP10-LC16-T01 The worker handle reports the service account and writes its audit rows as the worker", func(t *testing.T) {
		sync := e.worker("sync-partner-orders", "sync", nil, e.orders)
		var tickTenant, tickPrincipal string
		if err := RunWorker(e.ctx, e.rt, sync, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			tickTenant, tickPrincipal = h.Tenant().ID(), h.Principal().Subject().String()
			_, err := updateOrder(ctx, h, e.orders, "o-3", "synced")
			return err
		})); err != nil {
			t.Fatal(err)
		}
		if tickTenant != "t1" || tickPrincipal != "service_account:sync-partner-orders" ||
			e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't1' AND action = 'sync-partner-orders'
 AND actor_id = 'service_account:sync-partner-orders' AND operation = 'sync-partner-orders' AND on_behalf_of = ''`) != 1 {
			t.Fatalf("the worker handle reported %s and %s, or its audit row is not the worker's", tickTenant, tickPrincipal)
		}
		var seenTenant, seenPrincipal string
		cases.Store("lc16", func(ctx context.Context, m map[string]string, h *Handle[testProbe]) error {
			seenTenant, seenPrincipal = h.Tenant().ID(), h.Principal().Subject().String()
			_, err := updateOrder(ctx, h, e.orders, "o-1", "replied")
			return err
		})
		// The payload names another principal, which changes nothing: the staging principal comes from the row.
		m := e.stage("t2", "o-1", map[string]string{"case": "lc16", "principal": "user:u-9"})
		e.waitState(t, m, "done")
		var principal, origin string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT principal, origin FROM stego_outbox.messages WHERE id = $1`, m).
			Scan(&principal, &origin); err != nil {
			t.Fatal(err)
		}
		if seenTenant != "t2" || seenPrincipal != "service_account:apply-partner-replies" || principal != "user:u-7" ||
			origin != "reserve-and-charge" {
			t.Fatalf("the consumer saw %s and %s; the row stores %s and %s", seenTenant, seenPrincipal, principal, origin)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't2' AND action = 'apply-partner-replies'
 AND actor_id = 'service_account:apply-partner-replies' AND on_behalf_of = 'user:u-7' AND resource_id = 'o-1'`); n != 1 {
			t.Fatalf("%d audit rows of the consumer name the service account and, on whose behalf, user:u-7; want 1", n)
		}
	})

	t.Run("ST-SUP-RP10-LC17-T02 A user with only the command's permission cannot make the consuming worker touch a resource that the user cannot touch", func(t *testing.T) {
		var denyO2 atomic.Bool
		e.decision.setAllow(func(subject, action, resource string) bool {
			switch subject {
			case "service_account:apply-partner-replies":
				return action == "apply" && (resource == "order:o-1" || resource == "order:o-2")
			case "user:u-7":
				// The command's own read of o-1 is decided too; o-2 is allowed only until the fill holds its value.
				return (resource == "order:o-1" && (action == "view" || action == "reserve")) ||
					(resource == "order:o-2" && action == "reserve" && !denyO2.Load())
			}
			return false
		})
		defer e.decision.setAllow(nil)
		var after []evaluation
		var o2Update, o2Read error
		cases.Store("lc17", func(ctx context.Context, m map[string]string, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			o1, err := r.Get(ctx, "o-1")
			if err != nil {
				return err
			}
			o2, err := r.Get(ctx, "o-2")
			if err != nil {
				return err
			}
			denyO2.Store(true)
			e.decision.taken()
			if _, err := updateOrderFrom(ctx, r, o1, "applied-lc17"); err != nil {
				return err
			}
			_, o2Update = updateOrderFrom(ctx, r, o2, "applied-lc17")
			_, o2Read = r.Get(ctx, "o-2")
			after = e.decision.taken()
			return nil
		})
		m := e.stage("t1", "o-1", map[string]string{"case": "lc17", "orders": "o-1,o-2"})
		e.waitState(t, m, "done")
		if !errors.Is(o2Update, ErrRefused) || !errors.Is(o2Read, ErrRefused) {
			t.Fatalf("the update of o-2 gave %v and its read %v, want the refused error", o2Update, o2Read)
		}
		if e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND id = 'o-1' AND status = 'applied-lc17'`) != 1 ||
			e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND id = 'o-2' AND status = 'applied-lc17'`) != 0 {
			t.Fatal("the writes are not o-1 alone")
		}
		var calls []string
		for _, c := range after {
			calls = append(calls, c.Subject+" "+c.Action+" "+c.Resource)
			if c.Subject == "user:u-7" && c.Action == "apply" {
				t.Errorf("user:u-7 was decided on apply: %+v", c)
			}
		}
		want := "service_account:apply-partner-replies apply order:o-1,user:u-7 reserve order:o-1," +
			"service_account:apply-partner-replies apply order:o-2,user:u-7 reserve order:o-2," +
			"service_account:apply-partner-replies apply order:o-2,user:u-7 reserve order:o-2"
		if strings.Join(calls, ",") != want {
			t.Fatalf("the decisions are %v, want %s", calls, want)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't1' AND resource_id = 'o-1'
 AND action = 'apply-partner-replies' AND on_behalf_of = 'user:u-7' AND after::text LIKE '%applied-lc17%'`); n != 1 {
			t.Fatalf("%d audit rows of o-1 name user:u-7 on whose behalf the worker acted, want 1", n)
		}
	})

	t.Run("ST-SUP-RP10-LC25-T08 A consumer's unique violation is a failed attempt, not a conflict", func(t *testing.T) {
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r-dup', 'o-3', 's')`)
		var dup atomic.Bool
		var runs atomic.Int64
		dup.Store(true)
		cases.Store("unique", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			runs.Add(1)
			if !dup.Load() {
				return nil
			}
			// The reservation r-dup is there, so its create breaks the primary key of the tenant on each dispatch.
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-dup", OrderID: "o-3", SKU: "s"})
			return err
		})
		// The held clock gives the message one attempt until the test moves it: a unique violation in each attempt
		// would park it as poison after PoisonAttempts (ST-SUP-RC35-OC06).
		defer clock.hold()()
		m := e.stage("t1", "o-3", map[string]string{"case": "unique"})
		// The message ends done also when a check fails, so it does not run in the later subtests.
		defer func() {
			dup.Store(false)
			clock.add(time.Hour)
			e.waitState(t, m, "done")
		}()
		for deadline := time.Now().Add(5 * time.Second); runs.Load() < 1 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		e.waitState(t, m, "pending")
		var attempts, conflicts int
		if err := e.admin.QueryRowContext(e.ctx, `SELECT attempts, conflicts FROM stego_outbox.messages WHERE id = $1`, m).
			Scan(&attempts, &conflicts); err != nil {
			t.Fatal(err)
		}
		// Each dispatch that ran the fill counts an attempt, and none counts a conflict. A dispatch again may have
		// started, so the attempts are at least the runs.
		if n := runs.Load(); n < 1 || int64(attempts) < n || conflicts != 0 {
			t.Fatalf("after the unique violation: %d runs, %d attempts and %d conflicts, want an attempt for each run and no conflict", n, attempts, conflicts)
		}
	})

	// awaitRow waits until the row of the message id holds what done tells, with no deadline: only the timeout of go
	// test bounds it. It gives the row.
	type row struct {
		state, code         string
		attempts, conflicts int
	}
	awaitRow := func(t *testing.T, id uuid.UUID, done func(r row) bool) row {
		t.Helper()
		for {
			var r row
			if err := e.admin.QueryRowContext(e.ctx, `SELECT state, failure_code, attempts, conflicts FROM stego_outbox.messages
 WHERE id = $1`, id).Scan(&r.state, &r.code, &r.attempts, &r.conflicts); err != nil {
				t.Fatal(err)
			}
			if done(r) {
				return r
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	t.Run("ST-SUP-RC35-OC06-T05 A fill error that ends 5 attempts in a row parks the message as poison, and each attempt is reported and logged", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-p1', 'new')`)
		// The held clock gives the message one attempt each time the test moves it on.
		defer clock.hold()()
		// The fill reads a row that is not there, which is the same error in each attempt, but panics in the third
		// attempt, which starts the count again.
		var runs atomic.Int64
		cases.Store("poison", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			if runs.Add(1) == 3 {
				panic("the fill fails")
			}
			_, err := repo(t, h, e.orders).Get(ctx, "o-missing")
			return err
		})
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-p1", map[string]string{"case": "poison"})
		codes := []string{"fill.rolled_back.not_found.1", "fill.rolled_back.not_found.2", "fill.panicked.panic.1",
			"fill.rolled_back.not_found.1", "fill.rolled_back.not_found.2", "fill.rolled_back.not_found.3",
			"fill.rolled_back.not_found.4", PoisonCode}
		for i, code := range codes {
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i+1 && r.state != "dispatching" })
			want := "pending"
			if code == PoisonCode {
				want = "parked"
			}
			if r.state != want || r.code != code {
				t.Fatalf("after attempt %d the message is %s with %q, want %s with %q", i+1, r.state, r.code, want, code)
			}
			if i < len(codes)-1 {
				clock.add(time.Second)
			}
		}
		// The parked message stays in the outbox, so a person can send it again (ST-SUP-RP01-LC13).
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND parked_from = 'dispatching' AND must_land`, m); n != 1 ||
			runs.Load() != 8 || e.keyRows(m.String()) != 0 {
			t.Fatalf("the poison message: kept %d, %d runs, %d key rows", n, runs.Load(), e.keyRows(m.String()))
		}
		wantCauses := []string{"rolled_back/not_found", "rolled_back/not_found", "panicked/panic", "rolled_back/not_found",
			"rolled_back/not_found", "rolled_back/not_found", "rolled_back/not_found", "rolled_back/not_found"}
		if got := e.consumed.causeList(from); strings.Join(got, ",") != strings.Join(wantCauses, ",") {
			t.Fatalf("worker.run.failed sent %v, want %v", got, wantCauses)
		}
		var lines []string
		for _, record := range consumerLogs.all() {
			attributes := map[string]string{}
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				attributes[string(kv.Key)] = kv.Value.Emit()
				return true
			})
			if attributes["event_id"] != m.String() {
				continue
			}
			if record.EventName() != ConsumerFailedEvent || record.Severity() != otellog.SeverityWarn || len(attributes) != 6 ||
				attributes["worker"] != "apply-partner-replies" || attributes["destination"] != "partner-replies" ||
				record.Body().AsString() != "a consuming worker's run did not commit: apply-partner-replies "+attributes["outcome"]+" "+attributes["cause"] {
				t.Fatalf("the log line is %s %v %q with %v", record.EventName(), record.Severity(), record.Body().AsString(), attributes)
			}
			lines = append(lines, attributes["attempt"]+" "+attributes["outcome"]+"/"+attributes["cause"])
		}
		wantLines := []string{"1 rolled_back/not_found", "2 rolled_back/not_found", "3 panicked/panic", "4 rolled_back/not_found",
			"5 rolled_back/not_found", "6 rolled_back/not_found", "7 rolled_back/not_found", "8 rolled_back/not_found"}
		if strings.Join(lines, ",") != strings.Join(wantLines, ",") {
			t.Fatalf("the warn log holds %v, want %v", lines, wantLines)
		}
	})

	t.Run("ST-SUP-RC35-OC06-T07 A statement that the database fails never parks the message, and starts the count again", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-p4', 'new')`)
		defer clock.hold()()
		// Runs 1 and 2 read an order that is not there; runs 3 to 9 read the charge c1 while its column card_last4
		// is renamed, so the statement fails with SQLSTATE 42703 (undefined column); run 10 reads the missing order
		// again.
		var runs atomic.Int64
		cases.Store("statement", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			if n := runs.Add(1); n >= 3 && n <= 9 {
				_, err := repo(t, h, e.chargeE).Get(ctx, "c1")
				return err
			}
			_, err := repo(t, h, e.orders).Get(ctx, "o-missing")
			return err
		})
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-p4", map[string]string{"case": "statement"})
		defer func() {
			cases.Store("statement", func(context.Context, map[string]string, *Handle[testProbe]) error { return nil })
			clock.add(time.Hour)
			awaitRow(t, m, func(r row) bool { return r.state == "done" })
		}()
		for i := 1; i <= 10; i++ {
			switch i {
			case 3:
				e.exec(`ALTER TABLE charges RENAME COLUMN card_last4 TO card_last4_gone`)
			case 10:
				e.exec(`ALTER TABLE charges RENAME COLUMN card_last4_gone TO card_last4`)
			}
			want := "delivery-refused"
			switch i {
			case 1, 10:
				want = "fill.rolled_back.not_found.1"
			case 2:
				want = "fill.rolled_back.not_found.2"
			}
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if r.state != "pending" || r.code != want {
				t.Fatalf("after attempt %d the message is %s with %q, want pending with %q", i, r.state, r.code, want)
			}
			if i < 10 {
				clock.add(time.Second)
			}
		}
		got := e.consumed.causeList(from)
		if len(got) != 10 || got[0] != "rolled_back/not_found" || got[9] != "rolled_back/not_found" {
			t.Fatalf("worker.run.failed sent %v", got)
		}
		for _, c := range got[2:9] {
			if c != "rolled_back/statement" {
				t.Fatalf("a failed statement sent %s, want rolled_back/statement", c)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC18-T01 A consumer whose worker tuple is missing parks nothing and runs the message after the tuple is written", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-wt', 'new')`)
		defer clock.hold()()
		tuples := &tupleReader{}
		e.rt.cfg.WorkerTuples = tuples
		defer func() { e.rt.cfg.WorkerTuples = nil }()
		var runs atomic.Int64
		cases.Store("tuple", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			runs.Add(1)
			// As the fill of walkthrough 11: with the tuple missing, its list would be empty and it would fail.
			return errors.New("the tenant has no team")
		})
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-wt", map[string]string{"case": "tuple"})
		const tries = 2*PoisonAttempts + 1
		for i := 1; i <= tries; i++ {
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if r.state != "pending" || r.code != WorkerTupleMissingCode {
				t.Fatalf("after attempt %d the message is %s with %q, want pending with %q", i, r.state, r.code, WorkerTupleMissingCode)
			}
			clock.add(10 * time.Minute)
		}
		if runs.Load() != 0 {
			t.Fatalf("the fill ran %d times with the tuple missing, want 0", runs.Load())
		}
		if got := e.consumed.causeList(from); len(got) < tries || got[0] != "undecided/worker_tuple_missing" {
			t.Fatalf("worker.run.failed sent %v, want undecided/worker_tuple_missing for each attempt", got)
		}
		filter := tuples.last()
		if filter.ResourceType != "tenant" || filter.Relation != "worker_apply_partner_replies" ||
			filter.SubjectType != "service_account" || filter.SubjectID != "apply-partner-replies" {
			t.Fatalf("the read names %+v, want the tuple of the worker apply-partner-replies on its tenant", filter)
		}
		// The controller writes the tuple: the next attempt runs the fill once, and the message is done.
		tuples.held.Store(true)
		cases.Store("tuple", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			runs.Add(1)
			return nil
		})
		clock.add(10 * time.Minute)
		e.waitState(t, m, "done")
		if runs.Load() != 1 {
			t.Fatalf("the fill ran %d times after the tuple was written, want 1", runs.Load())
		}
	})

	t.Run("ST-SUP-RC35-OC18-T02 A failed read of the worker tuple is undecided, not missing, and a tuple missing for 15 minutes logs one warn line in each 15 minutes", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-wr', 'new')`)
		defer clock.hold()()
		tuples := &tupleReader{}
		tuples.fail.Store(true)
		// The tuple that the subtest before read as held is kept for HeldTupleAge: start with no kept tuple.
		e.rt.heldTuplesMu.Lock()
		e.rt.heldTuples = nil
		e.rt.heldTuplesMu.Unlock()
		e.rt.cfg.WorkerTuples = tuples
		defer func() { e.rt.cfg.WorkerTuples = nil }()
		var runs atomic.Int64
		cases.Store("read", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			runs.Add(1)
			return nil
		})
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-wr", map[string]string{"case": "read"})
		r := awaitRow(t, m, func(r row) bool { return r.attempts == 1 && r.state != "dispatching" })
		if r.state != "pending" || r.code == WorkerTupleMissingCode || runs.Load() != 0 {
			t.Fatalf("after a failed read the message is %s with %q and %d runs, want pending, not %s, and no run", r.state, r.code, runs.Load(), WorkerTupleMissingCode)
		}
		if got := e.consumed.causeList(from); len(got) == 0 || got[0] != "undecided/undecided" {
			t.Fatalf("worker.run.failed sent %v, want undecided/undecided", got)
		}
		tuples.fail.Store(false)
		tuples.held.Store(true)
		clock.add(10 * time.Minute)
		e.waitState(t, m, "done")

		// The warn line of a tuple that stays missing: none before 15 minutes, one at 15, none 5 minutes later, one
		// again 15 minutes after the first; none after the tuple was held.
		warns := func() int {
			n := 0
			for _, r := range consumerLogs.all() {
				if r.EventName() == WorkerTupleMissingEvent {
					n++
				}
			}
			return n
		}
		before := warns()
		at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		for _, c := range []struct {
			after time.Duration
			want  int
		}{{0, 0}, {14 * time.Minute, 0}, {15 * time.Minute, 1}, {20 * time.Minute, 1}, {30 * time.Minute, 2}} {
			e.rt.tupleMissing(e.ctx, "t9\x00w", "w", at.Add(c.after))
			if got := warns() - before; got != c.want {
				t.Fatalf("after %s missing, %d warn lines, want %d", c.after, got, c.want)
			}
		}
	})

	t.Run("ST-SUP-RC35-OC06-T09 A write that breaks a foreign key in 5 attempts parks the message as poison", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-p6', 'new')`)
		// The reservation of each run names an order that is not there, so its insert fails with SQLSTATE 23503 in
		// each attempt: the values of the row cause it, not the database.
		e.exec(`ALTER TABLE reservations ADD CONSTRAINT reservations_order_fk FOREIGN KEY (tenant_id, order_id)
 REFERENCES orders (tenant_id, id) NOT VALID`)
		defer e.exec(`ALTER TABLE reservations DROP CONSTRAINT reservations_order_fk`)
		defer clock.hold()()
		cases.Store("data", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-fk", OrderID: "o-none", SKU: "s"})
			return err
		})
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-p6", map[string]string{"case": "data"})
		for i := 1; i <= PoisonAttempts; i++ {
			want, state := fmt.Sprintf("fill.rolled_back.data.%d", i), "pending"
			if i == PoisonAttempts {
				want, state = PoisonCode, "parked"
			}
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if r.state != state || r.code != want {
				t.Fatalf("after attempt %d the message is %s with %q, want %s with %q", i, r.state, r.code, state, want)
			}
			if i < PoisonAttempts {
				clock.add(time.Second)
			}
		}
		if got := e.consumed.causeList(from); len(got) != PoisonAttempts || got[0] != "rolled_back/data" || got[PoisonAttempts-1] != "rolled_back/data" {
			t.Fatalf("worker.run.failed sent %v, want %d of rolled_back/data", got, PoisonAttempts)
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r-fk'`); n != 0 {
			t.Fatal("the refused insert left its row")
		}
	})

	t.Run("ST-SUP-RC35-OC06-T08 A conflict between two fill errors starts the count again", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-p5', 'new')`)
		defer clock.hold()()
		// Runs 1 and 2 read an order that is not there; run 3 meets a conflict, which counts no attempt; run 4 reads
		// the missing order, and its move sets the conflict count to zero; run 5 succeeds.
		var runs atomic.Int64
		cases.Store("restart", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			switch runs.Add(1) {
			case 3:
				r := repo(t, h, e.orders)
				o, err := r.Get(ctx, "o-p5")
				if err != nil {
					return err
				}
				e.exec(`UPDATE orders SET version = version + 1 WHERE tenant_id = 't1' AND id = 'o-p5'`)
				v := o.Value()
				v.Status = "conflicted"
				_, err = r.Update(ctx, o, v)
				return err
			case 5:
				return nil
			}
			_, err := repo(t, h, e.orders).Get(ctx, "o-missing")
			return err
		})
		m := e.stage("t1", "o-p5", map[string]string{"case": "restart"})
		steps := []struct {
			code                string
			attempts, conflicts int
		}{{"fill.rolled_back.not_found.1", 1, 0}, {"fill.rolled_back.not_found.2", 2, 0}, {"delivery-conflict", 2, 1},
			{"fill.rolled_back.not_found.1", 3, 0}}
		for i, step := range steps {
			r := awaitRow(t, m, func(r row) bool {
				return r.attempts == step.attempts && r.conflicts == step.conflicts && r.state != "dispatching"
			})
			if r.state != "pending" || r.code != step.code {
				t.Fatalf("after run %d the message is %s with %q, want pending with %q", i+1, r.state, r.code, step.code)
			}
			clock.add(time.Minute)
		}
		awaitRow(t, m, func(r row) bool { return r.state == "done" })
	})

	t.Run("ST-SUP-RC35-OC06-T06 A provider that cannot decide now and a conflict stay transient past 5 attempts, and each is reported", func(t *testing.T) {
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-p2', 'new'), ('t1', 'o-p3', 'new')`)
		defer clock.hold()()
		// The provider runs out of time on each decision of the worker's service account.
		e.decision.setFail(func(subject string) error {
			if subject == "service_account:apply-partner-replies" {
				return fmt.Errorf("the provider call: %w", context.DeadlineExceeded)
			}
			return nil
		})
		defer e.decision.setFail(nil)
		cases.Store("undecided", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.orders).Get(ctx, "o-p2")
			return err
		})
		from := len(e.consumed.causeList(0))
		undecided := e.stage("t1", "o-p2", map[string]string{"case": "undecided"})
		for i := 1; i <= PoisonAttempts+2; i++ {
			r := awaitRow(t, undecided, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if r.state != "pending" || r.code != "delivery-refused" {
				t.Fatalf("after attempt %d the undecided message is %s with %q, want pending with delivery-refused", i, r.state, r.code)
			}
			clock.add(time.Second)
		}
		e.decision.setFail(nil)
		awaitRow(t, undecided, func(r row) bool { return r.state == "done" })
		for _, got := range e.consumed.causeList(from) {
			if got != "undecided/undecided" {
				t.Fatalf("an undecided run sent %s", got)
			}
		}
		// A conflict in each attempt: the fill reads the order, another transaction moves its version, and the
		// update conflicts. The first 20 conflicts in a row count no attempt (ST-SUP-RP01-LC18).
		var conflict atomic.Bool
		conflict.Store(true)
		cases.Store("conflict", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			o, err := r.Get(ctx, "o-p3")
			if err != nil {
				return err
			}
			if conflict.Load() {
				e.exec(`UPDATE orders SET version = version + 1 WHERE tenant_id = 't1' AND id = 'o-p3'`)
			}
			v := o.Value()
			v.Status = "conflicted"
			_, err = r.Update(ctx, o, v)
			return err
		})
		from = len(e.consumed.causeList(0))
		conflicted := e.stage("t1", "o-p3", map[string]string{"case": "conflict"})
		for i := 1; i <= PoisonAttempts+2; i++ {
			r := awaitRow(t, conflicted, func(r row) bool { return r.conflicts == i && r.state != "dispatching" })
			if r.state != "pending" || r.code != "delivery-conflict" || r.attempts != 0 {
				t.Fatalf("after conflict %d the message is %s with %q after %d attempts", i, r.state, r.code, r.attempts)
			}
			clock.add(time.Minute)
		}
		conflict.Store(false)
		awaitRow(t, conflicted, func(r row) bool { return r.state == "done" })
		got := e.consumed.causeList(from)
		if len(got) < PoisonAttempts+2 {
			t.Fatalf("the conflicts sent %v", got)
		}
		for _, outcome := range got {
			if outcome != "conflict/conflict" {
				t.Fatalf("a conflict sent %s", outcome)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC12-T04 A consumer refuses the message of a retired tenant permanently and runs no fill", func(t *testing.T) {
		calls.Store(0)
		// The tenant list holds t2 as retired before the message is dispatched; a command of t2 still stages it, as
		// one that committed before the Retire did.
		retired.retired.Store("t2", true)
		defer retired.retired.Delete("t2")
		m := e.stage("t2", "o-1", map[string]string{"order": "o-1", "n": "retired"})
		e.waitState(t, m, "parked")
		if calls.Load() != 0 || e.keyRows(m.String()) != 0 {
			t.Fatalf("the message of the retired tenant ran the fill %d times or left a key row", calls.Load())
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND failure_code = 'delivery-refused-permanent'`, m); n != 1 {
			t.Fatal("the message of the retired tenant is not parked as a permanent refusal")
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't2' AND status = 'applied-retired'`); n != 0 {
			t.Fatal("the message of the retired tenant changed its order")
		}
	})

	t.Run("ST-SUP-RP10-LC26-T02 A consumer's deny of the staging principal and a declared refusal park the message", func(t *testing.T) {
		e.decision.setAllow(func(subject, action, resource string) bool {
			return !(subject == "user:u-7" && action == "reserve" && resource == "order:o-2")
		})
		defer e.decision.setAllow(nil)
		// The relay wrote the relations of o-2, so the deny is the decision of the row and not a lag
		// (ST-SUP-RP10-LC17).
		e.exec(`UPDATE orders SET authz_token = 'zt-o-2' WHERE tenant_id = 't1' AND id = 'o-2'`)
		cases.Store("denied", func(ctx context.Context, m map[string]string, h *Handle[testProbe]) error {
			_, err := updateOrder(ctx, h, e.orders, "o-2", "never")
			return err
		})
		cases.Store("refused", func(ctx context.Context, m map[string]string, h *Handle[testProbe]) error {
			if _, err := updateOrder(ctx, h, e.orders, "o-1", "never"); err != nil {
				return err
			}
			return Refused(errors.New("out of stock"))
		})
		// A parked message holds back the later messages of its key, so each case stages for an order of its own.
		for name, order := range map[string]string{"denied": "o-1", "refused": "o-3"} {
			calls.Store(0)
			m := e.stage("t1", order, map[string]string{"case": name})
			e.waitState(t, m, "parked")
			if calls.Load() != 1 || e.count(`SELECT count(*) FROM orders WHERE status = 'never'`) != 0 || e.keyRows(m.String()) != 0 {
				t.Fatalf("%s: parked after %d calls, with a write or a key row left", name, calls.Load())
			}
		}
	})

	t.Run("ST-SUP-RP10-LC17-T06 A consumer run that fails after a deny of a row with no relations yet is undecided, never parks, and passes once the relay wrote them", func(t *testing.T) {
		// o-rp1 and o-rp2 are rows that the run of an earlier message made: their authorization token is empty
		// until the relay writes their relations, and until then SpiceDB denies the worker on them.
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-rp1', 'new'), ('t1', 'o-rp2', 'new')`)
		var relayed sync.Map
		e.decision.setAllow(func(subject, action, resource string) bool {
			id, pending := strings.CutPrefix(resource, "order:o-rp")
			if !pending || subject != "service_account:apply-partner-replies" {
				return true
			}
			_, done := relayed.Load(id)
			return done
		})
		defer e.decision.setAllow(nil)
		relay := func(id string) {
			e.exec(`UPDATE orders SET authz_token = 'zt-o-rp` + id + `' WHERE tenant_id = 't1' AND id = 'o-rp` + id + `'`)
			relayed.Store(id, true)
		}
		// The fill of a list, as bill-bids of walkthrough 21 does: a change event of a row that the list does not
		// give is an error of the fill.
		cases.Store("relations-list", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			rows, err := r.List(ctx, pendingOrders, 10)
			if err != nil {
				return err
			}
			for _, o := range rows {
				if o.ID() == "o-rp1" {
					_, err := updateOrderFrom(ctx, r, o, "applied-rp")
					return err
				}
			}
			return errors.New("the order has no row yet")
		})
		// The fill of a read, which returns the refused error of the deny; a declared refusal of it would park the
		// message without the rule.
		cases.Store("relations-get", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			if _, err := updateOrder(ctx, h, e.orders, "o-rp2", "applied-rp"); err != nil {
				return Refused(err)
			}
			return nil
		})
		defer clock.hold()()
		for name, id := range map[string]string{"relations-list": "1", "relations-get": "2"} {
			from := len(e.consumed.causeList(0))
			// Each message has a key of its own, as a parked message holds back the later messages of its key.
			m := e.stage("t1", "o-rp"+id, map[string]string{"case": name})
			// More attempts than the poison bound: each one is undecided, so none is counted and none parks.
			for i := 1; i <= PoisonAttempts+1; i++ {
				r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
				if r.state != "pending" || !strings.HasPrefix(r.code, RelationsPendingCode+"."+strconv.Itoa(i)+".") {
					t.Fatalf("%s: after attempt %d the message is %s with %q, want pending with no poison count", name, i, r.state, r.code)
				}
				clock.add(time.Second)
			}
			got := e.consumed.causeList(from)
			if len(got) != PoisonAttempts+1 || slices.ContainsFunc(got, func(c string) bool { return c != "undecided/relations_pending" }) {
				t.Fatalf("%s: worker.run.failed sent %v, want undecided/relations_pending for each attempt", name, got)
			}
			// The relay writes the relations of the row: the next attempt decides the row with them and commits.
			relay(id)
			clock.add(time.Second)
			awaitRow(t, m, func(r row) bool { return r.state == "done" })
			if n := e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND id = 'o-rp` + id + `' AND status = 'applied-rp'`); n != 1 {
				t.Fatalf("%s: the run after the relay did not write the row", name)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC17-T07 A failure that a deny of a row with no relations did not cause counts for poison as before", func(t *testing.T) {
		// o-rp3 never gets its relations. The fill reads it, takes the deny, and then fails with an error of its own.
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-rp3', 'new')`)
		e.decision.setAllow(func(subject, action, resource string) bool {
			return !(subject == "service_account:apply-partner-replies" && resource == "order:o-rp3")
		})
		defer e.decision.setAllow(nil)
		cases.Store("relations-other", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.orders).Get(ctx, "o-rp3"); !errors.Is(err, ErrRefused) {
				t.Errorf("the read of o-rp3 gave %v, want the refused error", err)
			}
			return errors.New("the fill fails for a reason of its own")
		})
		defer clock.hold()()
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-rp3", map[string]string{"case": "relations-other"})
		for i := 1; i <= PoisonAttempts; i++ {
			want, state := "fill.rolled_back.fill_error."+strconv.Itoa(i), "pending"
			if i == PoisonAttempts {
				want, state = PoisonCode, "parked"
			}
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if r.state != state || r.code != want {
				t.Fatalf("after attempt %d the message is %s with %q, want %s with %q", i, r.state, r.code, state, want)
			}
			clock.add(time.Second)
		}
		if got := e.consumed.causeList(from); slices.Contains(got, "undecided/relations_pending") {
			t.Fatalf("worker.run.failed sent %v, with relations_pending for a failure that the deny did not cause", got)
		}
	})

	t.Run("ST-SUP-RP10-LC17-T08 A row whose relations never come parks its message after RelationsPendingAttempts attempts, with one warn line", func(t *testing.T) {
		// o-rp4 never gets its relations, as a row of an import or a tuple that the destination refuses.
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-rp4', 'new')`)
		e.decision.setAllow(func(subject, action, resource string) bool {
			return !(subject == "service_account:apply-partner-replies" && resource == "order:o-rp4")
		})
		defer e.decision.setAllow(nil)
		cases.Store("relations-never", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			if _, err := updateOrder(ctx, h, e.orders, "o-rp4", "applied-rp"); err != nil {
				return Refused(err)
			}
			return nil
		})
		defer clock.hold()()
		from := len(e.consumed.causeList(0))
		m := e.stage("t1", "o-rp4", map[string]string{"case": "relations-never"})
		for i := 1; i <= RelationsPendingAttempts+1; i++ {
			r := awaitRow(t, m, func(r row) bool { return r.attempts == i && r.state != "dispatching" })
			if i <= RelationsPendingAttempts {
				if r.state != "pending" || !strings.HasPrefix(r.code, RelationsPendingCode+"."+strconv.Itoa(i)+".") {
					t.Fatalf("after attempt %d the message is %s with %q, want pending and relations-pending", i, r.state, r.code)
				}
				clock.add(time.Second)
				continue
			}
			// The attempt past the bound is classed as with no such row: a declared refusal parks the message.
			if r.state != "parked" {
				t.Fatalf("after attempt %d the message is %s with %q, want parked", i, r.state, r.code)
			}
		}
		got := e.consumed.causeList(from)
		if len(got) != RelationsPendingAttempts+1 || got[len(got)-1] != "refused/declared_refusal" {
			t.Fatalf("worker.run.failed sent %v, want %d attempts of relations_pending and then the declared refusal", got, RelationsPendingAttempts)
		}
		var warned int
		for _, record := range consumerLogs.all() {
			if record.EventName() != RelationsPastBoundEvent {
				continue
			}
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				if string(kv.Key) == "event_id" && kv.Value.Emit() == m.String() {
					warned++
				}
				return true
			})
		}
		if warned != 1 {
			t.Fatalf("%d warn lines %s name the message, want 1", warned, RelationsPastBoundEvent)
		}
	})

	t.Run("ST-SUP-RC35-OC06-T02 A consumer run that the database refuses for a missing privilege parks the message and is reported", func(t *testing.T) {
		// The worker role of the test acts with the grants of the runtime role, which loses UPDATE on charges, so the
		// locked read and the update of the charge fail with SQLSTATE 42501 on every dispatch.
		e.exec("REVOKE UPDATE ON charges FROM " + e.roles[0])
		defer e.exec("GRANT UPDATE ON charges TO " + e.roles[0])
		cases.Store("not-granted", func(ctx context.Context, _ map[string]string, h *Handle[testProbe]) error {
			r := repo(t, h, e.chargeE)
			c, err := r.Get(ctx, "c1")
			if err != nil {
				return err
			}
			v := c.Value()
			v.Amount = 99
			_, err = r.Update(ctx, c, v)
			return err
		})
		calls.Store(0)
		before, _, _ := e.consumed.snapshot()
		m := e.stage("t1", "o-2", map[string]string{"case": "not-granted"})
		e.waitState(t, m, "parked")
		if calls.Load() != 1 || e.keyRows(m.String()) != 0 {
			t.Fatalf("parked after %d calls or with a key row left, want one call and no key row", calls.Load())
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND failure_code = 'delivery-refused-permanent'`, m); n != 1 {
			t.Fatal("the message is not parked as a permanent refusal")
		}
		if n := e.count(`SELECT count(*) FROM charges WHERE amount = 99`); n != 0 {
			t.Fatal("the refused run changed the charge")
		}
		after, _, _ := e.consumed.snapshot()
		if got := after[len(before):]; len(got) != 1 || got[0] != probe.WorkerNotGranted {
			t.Fatalf("the consumer sent the outcomes %v, want one not_granted", got)
		}
	})
}

// userV2 is a row of users in the release that adds the column seen_at.
type userV2 struct {
	ID, OrgID, Email string
	Admin            bool
	Rank             int64
	SeenAt           time.Time
}

// generatedUserList gives the list statements of users with the filter org_id = $3 AND rank >= $4 and the columns
// given, as the worker generator builds them from the entity of a release (ST-SUP-RC35-OC15): the column list of the
// entity's Scan, the tenant as $1, the limit or the key array as $2, the filter from $3, and ORDER BY the key.
func generatedUserList(columns, extra string) ListSpec {
	from := `SELECT ` + columns + ` FROM public."users" WHERE "tenant_id" = $1`
	filter := ` AND "org_id" = $3 AND "rank" >= $4` + extra
	return ListSpec{Name: "ranked",
		Read:   from + filter + ` ORDER BY "id" LIMIT $2`,
		Lock:   from + ` AND "id" = ANY($2::text[])` + filter + ` ORDER BY "id" FOR NO KEY UPDATE`,
		Values: DeclaredValues(ListValue{Constant: "org1"}, ListValue{Constant: int64(1)})}
}

func TestWorkerLists(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "org1", 3)
	e.seedUser("t1", "u1", "org1", true, 2, 1)
	e.seedUser("t1", "u2", "org1", false, 1, 1)
	e.seedUser("t1", "u3", "org1", false, 0, 1)
	v1, err := NewList(e.users, generatedUserList(`"id"::text, "org_id", "email", "admin", "rank", "version", "authz_token"`, ""))
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rows []Loaded[user]) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID())
		}
		return strings.Join(out, ",")
	}
	run := func(w *Worker[testProbe], fill func(ctx context.Context, h *Handle[testProbe]) error) error {
		return RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(fill))
	}
	withLists := func(lists ...AnyList) func(*WorkerSpec[testProbe]) {
		return func(s *WorkerSpec[testProbe]) { s.Lists = lists }
	}

	t.Run("ST-SUP-RP10-LC38-T01 A generated list reads and locks before and after an added column", func(t *testing.T) {
		w := e.worker("rank-scan", "sync", withLists(v1), e.users)
		read := func() (string, string) {
			var listed, locked string
			if err := run(w, func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, e.users)
				rows, err := r.List(ctx, v1, 10)
				if err != nil {
					return err
				}
				listed = ids(rows)
				rows, err = r.ListForUpdate(ctx, v1, nil)
				locked = ids(rows)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return listed, locked
		}
		if listed, locked := read(); listed != "u1,u2" || locked != "u1,u2" {
			t.Fatalf("before the change the list gave %q and locked %q, want u1,u2", listed, locked)
		}
		// The expand step of a schema change adds a column while the release before it still runs.
		e.exec(`ALTER TABLE users ADD COLUMN seen_at timestamptz NOT NULL DEFAULT '2026-01-01T00:00:00Z'`,
			`UPDATE users SET seen_at = '2030-01-01T00:00:00Z' WHERE id = 'u2'`)
		if listed, locked := read(); listed != "u1,u2" || locked != "u1,u2" {
			t.Fatalf("after the added column the list of the release before gave %q and locked %q, want u1,u2", listed, locked)
		}

		// The release that adds the field generates its Scan and its list from the same entity, with the new column
		// and a term on the time of the run.
		v2Spec := spec("User", "user", "users", []string{"org_id", "email", "admin", "rank", "seen_at"},
			func(s Scanner) (userV2, Meta, error) {
				var v userV2
				var m Meta
				err := s.Scan(&v.ID, &v.OrgID, &v.Email, &v.Admin, &v.Rank, &v.SeenAt, &m.Version, &m.Token)
				m.ID = v.ID
				return v, m, err
			},
			func(v userV2) []any { return []any{v.ID, v.OrgID, v.Email, v.Admin, v.Rank, v.SeenAt} },
			func(v userV2) map[string]any { return map[string]any{"id": v.ID} })
		v2Spec.LockAtMost = 3
		users2, err := NewEntity(v2Spec)
		if err != nil {
			t.Fatal(err)
		}
		v2Statements := generatedUserList(`"id"::text, "org_id", "email", "admin", "rank", "seen_at", "version", "authz_token"`, ` AND "seen_at" < $5`)
		v2Statements.Values = DeclaredValues(append(v2Statements.Values.all(), ListValue{RunTime: true})...)
		v2, err := NewList(users2, v2Statements)
		if err != nil {
			t.Fatal(err)
		}
		w2 := e.worker("rank-scan", "sync", withLists(v2), users2)
		var seen []string
		if err := run(w2, func(ctx context.Context, h *Handle[testProbe]) error {
			rows, err := repo(t, h, users2).ListForUpdate(ctx, v2, nil)
			for _, r := range rows {
				seen = append(seen, r.ID()+"@"+r.Value().SeenAt.UTC().Format("2006"))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The tick of the run is in 2026, so u2, seen in 2030, is not before the time of the run.
		if strings.Join(seen, ",") != "u1@2026" {
			t.Fatalf("the release with the new column listed %v, want u1 with its seen_at", seen)
		}
	})

	t.Run("ST-SUP-RP10-LC38-T02 A worker, consumer or command handle refuses a list that its declaration does not declare", func(t *testing.T) {
		own, err := NewList(e.users, ListSpec{Name: "own",
			Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users WHERE tenant_id = $1 ORDER BY id LIMIT $2`})
		if err != nil {
			t.Fatal(err)
		}
		w := e.worker("rank-scan", "sync", withLists(v1), e.users)
		var first, after error
		err = run(w, func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.users)
			_, first = r.List(ctx, own, 10)
			_, after = r.List(ctx, v1, 10)
			return first
		})
		if !errors.Is(first, ErrInternal) || !strings.Contains(first.Error(), "rank-scan reads the list own of User, which its declaration does not declare") ||
			!strings.Contains(first.Error(), "lists: of the worker") || !errors.Is(after, ErrInternal) || !errors.Is(err, ErrInternal) {
			t.Fatalf("the fill's own list gave %v, then %v, and the run %v; want the internal error that names the declared lists", first, after, err)
		}
		// A consumer's handle refuses it the same way.
		r := &Repository[user]{st: &state{mode: modeConsumer, action: "apply-replies"}, entity: e.users}
		if _, _, _, err := r.listArgs(e.ctx, own, nil); !errors.Is(err, ErrInternal) || !strings.Contains(err.Error(), "apply-replies reads the list own") {
			t.Fatalf("a consumer handle gave %v for an undeclared list", err)
		}
		// A command handle refuses it the same way, and tells the fill to declare it under lists: of the command
		// (walkthrough 7, B1). The log names the refusal, so the 500 internal of the call is diagnosable.
		var logs bytes.Buffer
		previous, held := slog.Default(), statementLog
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		statementLog = authz.NewCauseLog(func() time.Time { return tick })
		defer func() { slog.SetDefault(previous); statementLog = held }()
		cmd := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{v1} }, e.users)
		var read error
		_, err = e.run(cmd, e.request("t1", u7(t), "k-lc38-t02", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, read = repo(t, h, e.users).List(ctx, own, 10)
			return read
		}))
		if !errors.Is(read, ErrInternal) || !errors.Is(err, ErrInternal) ||
			!strings.Contains(read.Error(), "the command rank-users reads the list own of User, which its declaration does not declare") ||
			!strings.Contains(read.Error(), "lists: of the command") {
			t.Fatalf("a command handle gave %v and the run %v for an undeclared list", read, err)
		}
		if !strings.Contains(logs.String(), `msg="command: the call has an internal error" event.name=command.call.internal action=rank-users cause=internal reason="the command rank-users reads the list own of User, which its declaration does not declare`) {
			t.Fatalf("the log does not name the refused list:\n%s", logs.String())
		}
	})

	t.Run("ST-SUP-RP10-LC38-T03 A declared list takes no filter values from the fill, and the run binds the time of the run", func(t *testing.T) {
		w := e.worker("rank-scan", "sync", withLists(v1), e.users)
		var given error
		if err := run(w, func(ctx context.Context, h *Handle[testProbe]) error {
			_, given = repo(t, h, e.users).List(ctx, v1, 10, "org2", int64(0))
			return nil
		}); !errors.Is(err, ErrInternal) || !errors.Is(given, ErrInternal) || !strings.Contains(given.Error(), "declares its filter values") {
			t.Fatalf("filter values from the fill gave %v and the run %v, want an internal error", given, err)
		}
		// A declared list whose filter has no value, such as is_set terms only, takes no filter value either
		// (security review of PR #411, L3).
		isSet, err := NewList(e.users, ListSpec{Name: "set", Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users
 WHERE tenant_id = $1 AND org_id IS NOT NULL ORDER BY id LIMIT $2`})
		if err != nil {
			t.Fatal(err)
		}
		given = nil
		if err := run(e.worker("rank-scan", "sync", withLists(isSet), e.users), func(ctx context.Context, h *Handle[testProbe]) error {
			_, given = repo(t, h, e.users).List(ctx, isSet, 10, "org2")
			return nil
		}); !errors.Is(err, ErrInternal) || !errors.Is(given, ErrInternal) || !strings.Contains(given.Error(), "declares its filter values") {
			t.Fatalf("filter values from the fill for an is_set list gave %v and the run %v, want an internal error", given, err)
		}
		timed, err := NewList(e.users, ListSpec{Name: "timed", Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users
 WHERE tenant_id = $1 AND $3::timestamptz = $4::timestamptz ORDER BY id LIMIT $2`, Values: DeclaredValues(ListValue{RunTime: true}, ListValue{Constant: tick})})
		if err != nil {
			t.Fatal(err)
		}
		other := tick.Add(time.Minute)
		count := func(due time.Time) int {
			w := e.worker("rank-scan", "sync", withLists(timed), e.users)
			n := -1
			if err := RunWorker(e.ctx, e.rt, w, due, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				rows, err := repo(t, h, e.users).List(ctx, timed, 10)
				n = len(rows)
				return err
			})); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if at, next := count(tick), count(other); at != 3 || next != 0 {
			t.Fatalf("the run of the tick listed %d rows and the run of the next tick %d; want 3 and 0", at, next)
		}
		if _, err := NewList(e.users, ListSpec{Name: "both", Read: v1.spec.Read, Values: DeclaredValues(ListValue{Constant: 1, RunTime: true})}); err == nil {
			t.Fatal("NewList took a value that is a constant and the time of the run")
		}
		if _, err := NewWorker(WorkerSpec[testProbe]{Name: "rank-scan", Permission: "sync", Interval: time.Minute, Writes: []Declared{e.orders},
			Probe: newTestProbe, Lists: []AnyList{v1}}); err == nil || !strings.Contains(err.Error(), "does not write") {
			t.Fatalf("NewWorker took a list of an entity that the worker does not write: %v", err)
		}
	})
}

func TestCommandLists(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "org1", 3)
	e.seedUser("t1", "u1", "org1", true, 2, 1)
	e.seedUser("t1", "u2", "org1", true, 1, 1)
	e.seedOrg("t1", "org2", 3)
	e.seedUser("t1", "u3", "org2", true, 0, 1)
	cmd := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{e.admins} }, e.users)
	ids := func(rows []Loaded[user]) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID())
		}
		return strings.Join(out, ",")
	}
	// list runs the fill of one command with the list given and gives the rows, the error of List and of the run.
	n := 0
	list := func(c *Command[testProbe], l *List[user], args ...any) (string, error, error) {
		n++
		var rows []Loaded[user]
		var listed error
		_, err := e.run(c, e.request("t1", u7(t), fmt.Sprintf("k-lc38-t04-%d", n), `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			rows, listed = repo(t, h, e.users).List(ctx, l, 10, args...)
			return listed
		}))
		return ids(rows), listed, err
	}

	t.Run("ST-SUP-RP10-LC38-T04 A declared list binds its parameters as SQL parameters, and refuses a list that is not bound", func(t *testing.T) {
		if got, listed, err := list(cmd, e.admins.With("org1")); got != "u1,u2" || listed != nil || err != nil {
			t.Fatalf("the list bound to org1 gave %q, %v, %v; want u1,u2", got, listed, err)
		}
		if got, _, err := list(cmd, e.admins.With("org2")); got != "u3" || err != nil {
			t.Fatalf("the list bound to org2 gave %q, %v; want u3", got, err)
		}
		// A nil pointer binds NULL, which equals no row (security review of PR #416, L5).
		var none *string
		if got, listed, err := list(cmd, e.admins.With(none)); got != "" || listed != nil || err != nil {
			t.Fatalf("the list bound to a nil pointer gave %q, %v, %v; want no row", got, listed, err)
		}
		// A key parameter that the key check of the entity refuses gives an empty list and runs no statement, so the
		// cast of the statement never fails (security review of PR #416, L1).
		keyed, err := NewList(e.users, ListSpec{Name: "keyed", Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users
 WHERE tenant_id = $1 AND org_id = $3::uuid::text ORDER BY id LIMIT $2`, Values: DeclaredValues(ListValue{Param: "org_id", Key: true})})
		if err != nil {
			t.Fatal(err)
		}
		withKey := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{keyed} }, e.users)
		if got, listed, err := list(withKey, keyed.With("")); got != "" || listed != nil || err != nil {
			t.Fatalf("a key that the key check refuses gave %q, %v, %v; want an empty list", got, listed, err)
		}
		if _, listed, err := list(withKey, keyed.With("not-a-uuid")); !errors.Is(listed, ErrInternal) || !errors.Is(err, ErrInternal) {
			t.Fatalf("a key that the key check takes ran the statement and gave %v, %v; want the failed cast", listed, err)
		}
		// The key of a reference is checked with the key check of the referenced entity, not of the listed one
		// (security review of PR #416, sec-416b): a key that the listed entity takes and the referenced entity
		// refuses gives an empty list and runs no statement.
		referenced, err := NewList(e.users, ListSpec{Name: "referenced", Read: keyed.spec.Read,
			Values: DeclaredValues(ListValue{Param: "org_id", Key: true, KeyOf: KeyUUID})})
		if err != nil {
			t.Fatal(err)
		}
		withReferenced := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{referenced} }, e.users)
		if got, listed, err := list(withReferenced, referenced.With("not-a-uuid")); got != "" || listed != nil || err != nil {
			t.Fatalf("a key that the referenced entity refuses gave %q, %v, %v; want an empty list", got, listed, err)
		}
		// The contract package takes a copy of each declared list when it starts. A fill whose init then overwrites
		// the list through its pointer, before the spec of its command is built, gets a list that the handle refuses
		// (security review of PR #421, L1).
		owned, err := NewList(e.users, ListSpec{Name: "owned", Read: keyed.spec.Read, Values: DeclaredValues(ListValue{Param: "org_id", Key: true})})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot(owned)
		*owned = *Must(NewList(e.users, ListSpec{Name: "owned", Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users
 WHERE tenant_id = $1 AND $3::text IS NOT NULL ORDER BY id LIMIT $2`, Values: DeclaredValues(ListValue{Param: "org_id"})}))
		withOwned := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = snapshot }, e.users)
		if got, listed, err := list(withOwned, owned.With("org1")); got != "" || !errors.Is(listed, ErrInternal) || !errors.Is(err, ErrInternal) {
			t.Fatalf("a declared list that a fill overwrote gave %q, %v, %v; want the internal error of an undeclared list", got, listed, err)
		}
		kept, err := NewList(e.users, ListSpec{Name: "kept", Read: keyed.spec.Read, Values: DeclaredValues(ListValue{Param: "org_id", Key: true})})
		if err != nil {
			t.Fatal(err)
		}
		withKept := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = Snapshot(kept) }, e.users)
		if _, listed, _ := list(withKept, kept.With("6f1c7f4e-0000-4000-8000-000000000000")); listed != nil {
			t.Fatalf("a declared list that no fill changed gave %v", listed)
		}
		// A pointer is the Go value of an optional field: it binds its value.
		org := "org2"
		if got, _, err := list(cmd, e.admins.With(&org)); got != "u3" || err != nil {
			t.Fatalf("the list bound to a pointer to org2 gave %q, %v; want u3", got, err)
		}
		// A parameter is a SQL parameter, never text: a value with SQL in it matches no row.
		if got, _, err := list(cmd, e.admins.With("org1' OR 'x' = 'x")); got != "" || err != nil {
			t.Fatalf("a parameter with SQL text gave %q, %v; want no row", got, err)
		}
		for name, c := range map[string]struct {
			list *List[user]
			args []any
			want string
		}{
			"not bound":           {e.admins, nil, "takes 1 parameters"},
			"two values":          {e.admins.With("org1", "org2"), nil, "it takes 1 parameters and got 2"},
			"an int":              {e.admins.With(7), nil, "its parameter 1 is not a bool"},
			"a slice":             {e.admins.With([]string{"org1"}), nil, "its parameter 1 is not a bool"},
			"a pointer to an int": {e.admins.With(new(int)), nil, "its parameter 1 is not a bool"},
			"fill filter value":   {e.admins.With("org1"), []any{"org2"}, "declares its filter values, so the fill gives none"},
		} {
			got, listed, err := list(cmd, c.list, c.args...)
			if got != "" || !errors.Is(listed, ErrInternal) || !errors.Is(err, ErrInternal) || !strings.Contains(listed.Error(), c.want) {
				t.Fatalf("%s: the list gave %q, %v and the run %v; want the internal error %q", name, got, listed, err, c.want)
			}
		}
		// A command binds the time of the run as the start of its attempt.
		timed, err := NewList(e.users, ListSpec{Name: "timed", Read: `SELECT id, org_id, email, admin, rank, version, authz_token FROM users
 WHERE tenant_id = $1 AND org_id = $4 AND $3::timestamptz IS NOT NULL ORDER BY id LIMIT $2`,
			Values: DeclaredValues(ListValue{RunTime: true}, ListValue{Param: "org_id"})})
		if err != nil {
			t.Fatal(err)
		}
		withTime := e.command("rank-users", func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{timed} }, e.users)
		if got, listed, err := list(withTime, timed.With("org2")); got != "u3" || listed != nil || err != nil {
			t.Fatalf("a command list with the time of the run gave %q, %v, %v; want u3", got, listed, err)
		}
		for name, values := range map[string]*ListValues{
			"a constant and a parameter":          DeclaredValues(ListValue{Constant: "org1", Param: "org_id"}),
			"the time of the run and a parameter": DeclaredValues(ListValue{RunTime: true, Param: "seen"}),
			"a parameter twice":                   DeclaredValues(ListValue{Param: "org_id"}, ListValue{Param: "org_id"}),
		} {
			if _, err := NewList(e.users, ListSpec{Name: "bad", Read: e.admins.spec.Read, Values: values}); err == nil {
				t.Fatalf("NewList took %s", name)
			}
		}
		for name, lists := range map[string][]AnyList{
			"a bound list":                    {e.admins.With("org1")},
			"a nil list":                      {(*List[user])(nil)},
			"a list of an entity not written": {e.ofOrder},
		} {
			if _, err := NewCommand(CommandSpec[testProbe]{Name: "rank-users", Writes: []Declared{e.users}, Probe: newTestProbe, Lists: lists}); err == nil {
				t.Fatalf("NewCommand took %s", name)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC39-T01 A failed statement is logged with its cause class and SQLSTATE class, and no value", func(t *testing.T) {
		var logs bytes.Buffer
		previous, held := slog.Default(), statementLog
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		statementLog = authz.NewCauseLog(func() time.Time { return tick })
		t.Cleanup(func() { slog.SetDefault(previous); statementLog = held })
		const secret = "org-secret-7"
		all := `"id"::text, "org_id", "email", "admin", "rank", "version", "authz_token"`
		for _, c := range []struct {
			action, read, cause, class string
		}{
			// The list of the release before a rename names a column that the release after it dropped.
			{"read-gone", `SELECT ` + all + `, "gone" FROM users WHERE tenant_id = $1 AND org_id = $3 ORDER BY id LIMIT $2`, "schema", "42"},
			// The list of walkthrough 7, B1: the row has one column fewer than the generated Scan reads.
			{"read-short", `SELECT "id"::text, "org_id", "email", "admin", "rank", "version" FROM users WHERE tenant_id = $1 AND $3::text <> '' ORDER BY id LIMIT $2`, "scan", "none"},
			// A parameter that the column type cannot take fails on the value.
			{"read-cast", `SELECT ` + all + ` FROM users WHERE tenant_id = $1 AND rank = $3::int ORDER BY id LIMIT $2`, "data", "22"},
		} {
			l, err := NewList(e.users, ListSpec{Name: "of-org", Read: c.read, Values: DeclaredValues(ListValue{Param: "org_id"})})
			if err != nil {
				t.Fatal(err)
			}
			failing := e.command(c.action, func(s *CommandSpec[testProbe]) { s.Lists = []AnyList{l} }, e.users)
			for range 2 {
				if _, listed, err := list(failing, l.With(secret)); !errors.Is(listed, ErrInternal) || OutcomeOf(err).Status != 500 {
					t.Fatalf("%s gave %v and the run %v; want 500 internal", c.action, listed, err)
				}
			}
			want := fmt.Sprintf(`msg="command: a statement failed" event.name=command.statement.failed action=%s step="read the list of-org" cause=%s sqlstate_class=%s held_back=0`, c.action, c.cause, c.class)
			if got := strings.Count(logs.String(), want); got != 1 {
				t.Fatalf("the log holds %d lines %q, want 1 (the second failure is held back):\n%s", got, want, logs.String())
			}
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "SELECT") {
			t.Fatalf("the log holds a value or a statement:\n%s", logs.String())
		}
	})
}

func TestWorkerNoParent(t *testing.T) {
	t.Run("ST-SUP-RP10-LC17-T04 A worker's create of a row with a resource type and no parent is decided on the tenant resource, and a deny writes nothing", func(t *testing.T) {
		e := newEnv(t)
		w := e.worker("import-orders", "sync", nil, e.orders)
		create := func(id string) error {
			return RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.orders).Create(ctx, order{ID: id, Status: "new"})
				return err
			}))
		}
		// The worker's permission on the tenant resource of the run allows the create.
		e.decision.setAllow(func(subject, action, resource string) bool { return action == "sync" && resource == "tenant:t1" })
		e.decision.taken()
		if err := create("o-root"); err != nil {
			t.Fatalf("the root create allowed on the tenant resource gave %v", err)
		}
		var decided []string
		for _, d := range e.decision.taken() {
			decided = append(decided, d.Subject+" "+d.Action+" "+d.Resource)
		}
		if !slices.Contains(decided, "service_account:import-orders sync tenant:t1") {
			t.Fatalf("the root create was decided as %v, want the worker's permission sync on tenant:t1", decided)
		}
		if e.count(`SELECT count(*) FROM orders WHERE id = 'o-root'`) != 1 {
			t.Fatal("the allowed root create wrote no row")
		}
		// A deny on the tenant resource is the refused error, and writes nothing.
		e.decision.setAllow(func(string, string, string) bool { return false })
		err := create("o-denied")
		if !errors.Is(err, ErrRefused) || errors.Is(err, ErrNoParent) || causeOf(err) != "refused" {
			t.Fatalf("the denied root create gave %v (cause %s), want the refused error", err, causeOf(err))
		}
		if e.count(`SELECT count(*) FROM orders WHERE id = 'o-denied'`) != 0 {
			t.Fatal("the denied root create wrote its row")
		}
		// ErrNoParent keeps a fixed cause class of the run log.
		if causeOf(fmt.Errorf("fill: %w", &noParentError{entity: "Order"})) != "create_without_parent" ||
			!slices.Contains(probe.WorkerCauses, "create_without_parent") {
			t.Fatal("ErrNoParent has no fixed cause class")
		}
	})
	t.Run("ST-SUP-RP10-LC17-T05 A worker's create that sets a reference to an entity with a resource type and gives no parent is refused, with no decision on the tenant", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-2', 'new')`)
		w := e.worker("import-reservations", "sync", nil, e.typedRes)
		// The tenant allows everything, so a fallback to the tenant decision would write the row.
		e.decision.setAllow(func(string, string, string) bool { return true })
		e.decision.taken()
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.typedRes).Create(ctx, reservation{ID: "r-orphan", OrderID: "o-2", SKU: "s"})
			return err
		}))
		entity, ok := NoParentEntity(err)
		if !errors.Is(err, ErrNoParent) || !ok || entity != "TypedRes" || causeOf(err) != "create_without_parent" {
			t.Fatalf("the create under o-2 with no parent gave %v (entity %q), want ErrNoParent for TypedRes", err, entity)
		}
		for _, d := range e.decision.taken() {
			if strings.HasPrefix(d.Resource, "tenant:") {
				t.Fatalf("the refused create was decided on %s", d.Resource)
			}
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-orphan'`) != 0 {
			t.Fatal("the create with no parent wrote its row under o-2")
		}
	})
}

// tupleReader is a reader of SpiceDB whose worker tuple is held or not, and which keeps the last filter it read.
type tupleReader struct {
	held   atomic.Bool
	fail   atomic.Bool
	mu     sync.Mutex
	filter authz.Filter
}

func (r *tupleReader) RelationsOf(context.Context, string) ([]string, error) { return nil, nil }

func (r *tupleReader) Exists(_ context.Context, filter authz.Filter) (bool, error) {
	r.mu.Lock()
	r.filter = filter
	r.mu.Unlock()
	if r.fail.Load() {
		return false, errors.New("spicedb: connection refused")
	}
	return r.held.Load(), nil
}

func (r *tupleReader) last() authz.Filter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filter
}

// TestWorkerFillCauses proves the fixed cause classes of a fill error that the handle did not give (decision
// ST-SUP-Q203 (b)): the class comes from the type of an error in the chain, never from its text.
func TestWorkerFillCauses(t *testing.T) {
	t.Run("ST-SUP-RC35-OC06-T10 A fill error is classed fill_json, fill_parse or fill_canceled by the type of an error in its chain, and fill_error else, never by its text", func(t *testing.T) {
		var syntax *json.SyntaxError
		syntaxErr := json.Unmarshal([]byte("{"), &struct{}{})
		if !errors.As(syntaxErr, &syntax) {
			t.Fatalf("json.Unmarshal of { gave %T, want *json.SyntaxError", syntaxErr)
		}
		var typed struct{ N int }
		typeErr := json.Unmarshal([]byte(`{"N":"buyer@example.com"}`), &typed)
		// The error that json.Unmarshal gives for a target that is not a pointer.
		invalidErr := error(&json.InvalidUnmarshalError{Type: reflect.TypeOf(0)})
		_, numberErr := strconv.Atoi("buyer@example.com")
		_, parseErr := time.Parse(time.RFC3339, "buyer@example.com")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, c := range []struct {
			err  error
			want string
		}{
			{fmt.Errorf("no buyer %s: %w", "buyer@example.com", syntaxErr), "fill_json"},
			{fmt.Errorf("decode: %w", typeErr), "fill_json"},
			{fmt.Errorf("decode: %w", invalidErr), "fill_json"},
			{fmt.Errorf("qty: %w", numberErr), "fill_parse"},
			{fmt.Errorf("due: %w", parseErr), "fill_parse"},
			{fmt.Errorf("call: %w", ctx.Err()), "fill_canceled"},
			{errors.New("fill_json fill_parse fill_canceled"), "fill_error"},
			{errors.New("no buyer buyer@example.com"), "fill_error"},
			// An error of the handle keeps its class, also when its chain holds a json error.
			{fmt.Errorf("%w: %w", ErrInvalid, syntaxErr), "invalid"},
		} {
			outcome, cause := consumerOutcome(c.err)
			if outcome != probe.WorkerRolledBack || cause != c.want {
				t.Errorf("the fill error %T gave %s %s, want rolled_back %s", c.err, outcome, cause, c.want)
			}
			if !slices.Contains(probe.WorkerCauses, cause) {
				t.Errorf("the cause %s is not one of probe.WorkerCauses, so worker.run.failed would refuse it", cause)
			}
			if strings.Contains(cause, "@") {
				t.Errorf("the cause %q holds a value of the error text", cause)
			}
		}
		// The new classes count for the poison bound as fill_error does.
		for _, cause := range []string{"fill_error", "fill_json", "fill_parse", "fill_canceled"} {
			if !streakCauses[cause] {
				t.Errorf("the cause %s does not count for the poison bound", cause)
			}
		}
	})
}

// TestWorkerStopNotCounted proves that a stop of the worker never parks a valid message (security review of PR #478,
// L1).
func TestWorkerStopNotCounted(t *testing.T) {
	t.Run("ST-SUP-RC35-OC06-T11 A fill canceled while the run's context is done is not counted for the poison bound, and one with a live context is", func(t *testing.T) {
		live := context.Background()
		stopped, stop := context.WithCancel(context.Background())
		stop()
		canceled := fmt.Errorf("call the partner: %w", context.Canceled)
		// The fourth attempt in a row with the same class: a counted fifth attempt parks the message.
		d := outbox.Delivery{FailureCode: streakPrefix + string(probe.WorkerRolledBack) + ".fill_canceled.4"}
		if err := poisonStreak(live, d, canceled); !outbox.IsPermanent(err) {
			t.Fatalf("a fifth fill_canceled with a live context gave %v, want the permanent refusal poison", err)
		}
		if err := poisonStreak(stopped, d, canceled); outbox.IsPermanent(err) || outbox.IsConflict(err) {
			t.Fatalf("a fill canceled by a stop of the worker gave %v, want a transient refusal that is not counted", err)
		}
		// Another fill error of a stopped run is still counted.
		d.FailureCode = streakPrefix + string(probe.WorkerRolledBack) + ".fill_error.4"
		if err := poisonStreak(stopped, d, errors.New("no buyer")); !outbox.IsPermanent(err) {
			t.Fatalf("a fifth fill_error gave %v, want the permanent refusal poison", err)
		}
	})
}
