package command

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// fakeClock is the clock of the ticker tests: it stands still and moves only with advance, which fires each wait
// that is due.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWait
}

type fakeWait struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWait{at: c.now.Add(d), ch: ch})
	return ch
}

// advance moves the clock on by d and fires each wait that is due, the earliest first.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].at.Before(c.waiters[j].at) })
	var keep []fakeWait
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
}

// observed records the built-in worker observations of a ticker.
type observed struct {
	mu         sync.Mutex
	failed     []probe.WorkerOutcome
	skipped    int
	unreached  int
	tickFailed int
}

func (o *observed) TickFailed(context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tickFailed++
}

// tickFailures gives the count of worker.tick.failed.
func (o *observed) tickFailures() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tickFailed
}

func (o *observed) RunFailed(_ context.Context, outcome probe.WorkerOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed = append(o.failed, outcome)
}

func (o *observed) TickSkipped() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skipped++
}

func (o *observed) TenantUnreached() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unreached++
}

func (o *observed) snapshot() ([]probe.WorkerOutcome, int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]probe.WorkerOutcome(nil), o.failed...), o.skipped, o.unreached
}

// tickerRun is one call of a ticker test's fill.
type tickerRun struct {
	tenant string
	due    time.Time
	token  int64
}

// tickerFixture is one ticker of the worker sync-partner-orders with the interval given on the fake clock.
type tickerFixture struct {
	t       *testing.T
	e       *env
	clock   *fakeClock
	observe *observed
	ticker  *Ticker
	runs    chan tickerRun
	done    chan struct{}
	cancel  context.CancelFunc
}

// start starts a ticker whose fill sends each call on runs and then runs body, over the tenants given.
func startTicker(t *testing.T, e *env, clock *fakeClock, interval time.Duration, client *lease.Client, tenants []string,
	body func(ctx context.Context, tenant string, h *Handle[testProbe]) error) *tickerFixture {
	t.Helper()
	list := make([]tenant.Tenant, len(tenants))
	for i, id := range tenants {
		list[i] = mustTenant(t, id)
	}
	return startTickerOver(t, e, clock, interval, client, func(context.Context) ([]tenant.Tenant, error) { return list, nil }, body)
}

// startTickerOver is startTicker over the tenant list that list reads.
func startTickerOver(t *testing.T, e *env, clock *fakeClock, interval time.Duration, client *lease.Client,
	list func(context.Context) ([]tenant.Tenant, error), body func(ctx context.Context, tenant string, h *Handle[testProbe]) error) *tickerFixture {
	t.Helper()
	w := e.worker("sync-partner-orders", "sync", func(s *WorkerSpec[testProbe]) { s.Interval = interval }, e.orders)
	f := &tickerFixture{t: t, e: e, clock: clock, observe: &observed{}, runs: make(chan tickerRun, 64), done: make(chan struct{})}
	ticker, err := NewTicker(e.rt, TickerSpec[testProbe]{Worker: w, Lease: client, Observe: f.observe, Clock: clock,
		Tenants: list,
		Fill: func(ctx context.Context, due time.Time, h *Handle[testProbe]) error {
			run := tickerRun{tenant: h.Tenant().ID(), due: due}
			f.runs <- run
			if body == nil {
				return nil
			}
			return body(ctx, run.tenant, h)
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.ticker = ticker
	ctx, cancel := context.WithCancel(e.ctx)
	f.cancel = cancel
	go func() {
		defer close(f.done)
		_ = ticker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ticker.Finish(context.Background())
		<-f.done
	})
	return f
}

// waitArms waits until the scheduler armed its wait for a due time n times in all.
func (f *tickerFixture) waitArms(n int64) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.ticker.armed.Load() < n {
		if time.Now().After(deadline) {
			f.t.Fatalf("the scheduler armed %d waits, want %d", f.ticker.armed.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitTickDone waits until no tick runs.
func (f *tickerFixture) waitTickDone() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.ticker.mu.Lock()
		busy := f.ticker.busy
		f.ticker.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the tick does not end")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// next gives the next call of the fill.
func (f *tickerFixture) next() tickerRun {
	f.t.Helper()
	select {
	case r := <-f.runs:
		return r
	case <-time.After(10 * time.Second):
		f.t.Fatal("the fill was not called")
		return tickerRun{}
	}
}

// none checks that the fill is not called for a moment.
func (f *tickerFixture) none() {
	f.t.Helper()
	select {
	case r := <-f.runs:
		f.t.Fatalf("the fill was called for %s at %v", r.tenant, r.due)
	case <-time.After(300 * time.Millisecond):
	}
}

// waitFailed waits until the observer holds n failed runs.
func (f *tickerFixture) waitFailed(n int) []probe.WorkerOutcome {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		failed, _, _ := f.observe.snapshot()
		if len(failed) >= n {
			return failed
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%d failed runs, want %d", len(failed), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// aligned is a time just after a multiple of interval since the Unix epoch.
func aligned(interval time.Duration) time.Time {
	return time.Unix(0, (tick.UnixNano()/interval.Nanoseconds())*interval.Nanoseconds()).Add(time.Second)
}

// status reads the status of the order o-1 of the tenant.
func (e *env) status(tenantID string) string {
	var status string
	if err := e.admin.QueryRowContext(e.ctx, `SELECT status FROM orders WHERE tenant_id = $1 AND id = 'o-1'`, tenantID).Scan(&status); err != nil {
		e.t.Fatal(err)
	}
	return status
}

func TestTicker(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-1', 'new'), ('t2', 'o-1', 'new'), ('t3', 'o-1', 'new')`)
	const interval = 10 * time.Second
	write := func(ctx context.Context, h *Handle[testProbe], status string) error {
		_, err := updateOrder(ctx, h, e.orders, "o-1", status)
		return err
	}

	t.Run("ST-SUP-RC35-OC04-T02 A ticker runs each tenant once a tick in its own transaction and skips an overlapping tick", func(t *testing.T) {
		clock := newFakeClock(aligned(interval))
		release := make(chan struct{})
		var blocking atomic.Bool
		f := startTicker(t, e, clock, interval, nil, []string{"t1", "t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if blocking.Load() && tenantID == "t1" {
				<-release
			}
			return write(ctx, h, "tick-"+tenantID)
		})
		f.waitArms(1)
		clock.advance(interval)
		first, second := f.next(), f.next()
		if first.tenant != "t1" || second.tenant != "t2" || first.due.UnixNano()%interval.Nanoseconds() != 0 {
			t.Fatalf("the first tick ran %+v and %+v", first, second)
		}
		f.waitTickDone()
		if e.status("t1") != "tick-t1" || e.status("t2") != "tick-t2" {
			t.Fatalf("the runs did not commit: %s, %s", e.status("t1"), e.status("t2"))
		}
		blocking.Store(true)
		f.waitArms(2)
		clock.advance(interval)
		if r := f.next(); r.tenant != "t1" {
			t.Fatalf("the second tick started with %+v, want t1, the tenant after t2", r)
		}
		// The run of t1 blocks: the next due time comes while the tick runs, and is skipped.
		f.waitArms(3)
		clock.advance(interval)
		f.waitArms(4)
		f.none()
		if _, skipped, _ := f.observe.snapshot(); skipped != 1 {
			t.Fatalf("worker.tick.skipped counted %d, want 1", skipped)
		}
		close(release)
		f.waitTickDone()
	})

	t.Run("ST-SUP-RC35-OC07-T02 A slow tenant ends at its own deadline and the next tenant gets its own", func(t *testing.T) {
		e.exec(`UPDATE orders SET status = 'new'`)
		clock := newFakeClock(aligned(time.Minute))
		f := startTicker(t, e, clock, time.Minute, nil, []string{"t1", "t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if err := write(ctx, h, "slow-"+tenantID); err != nil {
				return err
			}
			if tenantID == "t1" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
		f.waitArms(1)
		clock.advance(time.Minute)
		if r := f.next(); r.tenant != "t1" {
			t.Fatalf("the tick started with %s", r.tenant)
		}
		f.waitArms(2)
		clock.advance(31 * time.Second)
		if second := f.next(); second.tenant != "t2" {
			t.Fatalf("the second run was %s", second.tenant)
		}
		f.waitTickDone()
		if failed := f.waitFailed(1); len(failed) != 1 || failed[0] != probe.WorkerRolledBack {
			t.Fatalf("the failed runs are %v", failed)
		}
		if e.status("t1") == "slow-t1" || e.status("t2") != "slow-t2" {
			t.Fatalf("t1 has %s and t2 %s", e.status("t1"), e.status("t2"))
		}
	})

	t.Run("ST-SUP-RC35-OC08-T02 A panic in one tenant's run ends only that run", func(t *testing.T) {
		e.exec(`UPDATE orders SET status = 'new'`)
		clock := newFakeClock(aligned(interval))
		var panicking atomic.Bool
		panicking.Store(true)
		f := startTicker(t, e, clock, interval, nil, []string{"t1", "t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if err := write(ctx, h, "panic-"+tenantID); err != nil {
				return err
			}
			if panicking.Load() && tenantID == "t1" {
				panic("secret-7")
			}
			return nil
		})
		f.waitArms(1)
		clock.advance(interval)
		f.next()
		f.next()
		f.waitTickDone()
		if failed := f.waitFailed(1); failed[0] != probe.WorkerPanicked {
			t.Fatalf("the failed runs are %v", failed)
		}
		if e.status("t1") == "panic-t1" || e.status("t2") != "panic-t2" {
			t.Fatalf("t1 has %s and t2 %s", e.status("t1"), e.status("t2"))
		}
		panicking.Store(false)
		e.exec(`UPDATE orders SET status = 'new'`)
		f.waitArms(2)
		clock.advance(interval)
		f.next()
		f.next()
		f.waitTickDone()
		if e.status("t1") != "panic-t1" || e.status("t2") != "panic-t2" {
			t.Fatalf("the next tick left t1 with %s and t2 with %s", e.status("t1"), e.status("t2"))
		}
	})

	t.Run("ST-SUP-RC35-OC09-T02 Each run that does not commit is one worker.run.failed with its fixed outcome only", func(t *testing.T) {
		clock := newFakeClock(aligned(interval))
		var end atomic.Value
		end.Store("nil")
		f := startTicker(t, e, clock, interval, nil, []string{"t1"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			switch end.Load() {
			case "error":
				return errors.New("secret-7")
			case "panic":
				panic("secret-7")
			}
			return nil
		})
		for i, next := range []string{"nil", "error", "panic"} {
			end.Store(next)
			f.waitArms(int64(i + 1))
			clock.advance(interval)
			f.next()
			f.waitTickDone()
		}
		failed := f.waitFailed(2)
		if len(failed) != 2 || failed[0] != probe.WorkerRolledBack || failed[1] != probe.WorkerPanicked {
			t.Fatalf("the failed runs are %v, want rolled_back and panicked", failed)
		}
	})

	t.Run("ST-SUP-RC35-OC09-T03 A tick that cannot read the tenant list sends worker.tick.failed and runs no tenant", func(t *testing.T) {
		clock := newFakeClock(aligned(interval))
		var fail atomic.Bool
		fail.Store(true)
		list := func(context.Context) ([]tenant.Tenant, error) {
			if fail.Load() {
				return nil, errors.New("secret-7")
			}
			return []tenant.Tenant{mustTenant(t, "t1")}, nil
		}
		f := startTickerOver(t, e, clock, interval, nil, list, nil)
		f.waitArms(1)
		clock.advance(interval)
		f.waitArms(2)
		f.waitTickDone()
		f.none()
		if n := f.observe.tickFailures(); n != 1 {
			t.Fatalf("%d worker.tick.failed after a tick whose list read failed, want 1", n)
		}
		if failed, _, _ := f.observe.snapshot(); len(failed) != 0 {
			t.Fatalf("the failed list read gave the failed runs %v, want none", failed)
		}
		// The next tick reads the list and runs the tenant, and sends no worker.tick.failed.
		fail.Store(false)
		clock.advance(interval)
		if r := f.next(); r.tenant != "t1" {
			t.Fatalf("the next tick ran %s, want t1", r.tenant)
		}
		f.waitTickDone()
		if n := f.observe.tickFailures(); n != 1 {
			t.Fatalf("%d worker.tick.failed after a tick that read the list, want 1", n)
		}
	})

	t.Run("ST-SUP-RC27-OC12-T06 A tenant that the gate holds as retired after the tick read the list gets no run", func(t *testing.T) {
		clock := newFakeClock(aligned(interval))
		retired := &retiredTenants{}
		retired.retired.Store("t2", true)
		e.rt.cfg.Tenants = retired
		// The cleanup runs after the cleanup of the ticker, which waits for its runs.
		t.Cleanup(func() { e.rt.cfg.Tenants = nil })
		// The list still holds t2, as a list read before the retire does.
		f := startTicker(t, e, clock, interval, nil, []string{"t1", "t2", "t3"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			return write(ctx, h, "gated-"+tenantID)
		})
		f.waitArms(1)
		clock.advance(interval)
		var ran []string
		for range 2 {
			ran = append(ran, f.next().tenant)
		}
		f.waitTickDone()
		f.none()
		if !slices.Equal(ran, []string{"t1", "t3"}) {
			t.Fatalf("the tick ran %v, want t1 and t3 only", ran)
		}
		if got := e.status("t2"); got == "gated-t2" {
			t.Fatal("the retired tenant t2 got the write of the tick")
		}
		if failed, _, _ := f.observe.snapshot(); len(failed) != 0 {
			t.Fatalf("the skipped tenant gave the failed runs %v, want none", failed)
		}
	})

	t.Run("ST-SUP-RC35-OC10-T02 Finish starts no run, waits for the running one and rolls back what outlives the bound", func(t *testing.T) {
		e.exec(`UPDATE orders SET status = 'new'`)
		clock := newFakeClock(aligned(interval))
		release := make(chan struct{})
		f := startTicker(t, e, clock, interval, nil, []string{"t1", "t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if err := write(ctx, h, "drain-"+tenantID); err != nil {
				return err
			}
			<-release
			return nil
		})
		f.waitArms(1)
		clock.advance(interval)
		f.next()
		finished := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished <- f.ticker.Finish(ctx)
		}()
		for !f.ticker.isStopping() {
			time.Sleep(2 * time.Millisecond)
		}
		close(release)
		// t1 commits; the tick then starts no run for t2, since the ticker stops.
		if err := <-finished; err != nil {
			t.Fatalf("Finish gave %v", err)
		}
		f.none()
		if e.status("t1") != "drain-t1" || e.status("t2") == "drain-t2" {
			t.Fatalf("t1 has %s and t2 %s after the drain", e.status("t1"), e.status("t2"))
		}
		if _, _, unreached := f.observe.snapshot(); unreached != 1 {
			t.Fatalf("worker.tenant.unreached counted %d, want 1", unreached)
		}
		clock.advance(interval)
		f.none()

		blocked := startTicker(t, e, newFakeClock(aligned(interval)), interval, nil, []string{"t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if err := write(ctx, h, "blocked-"+tenantID); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
		blocked.waitArms(1)
		blocked.clock.advance(interval)
		blocked.next()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := blocked.ticker.Finish(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Finish gave %v, want the end of its bound", err)
		}
		if e.status("t2") == "blocked-t2" {
			t.Fatal("the run that outlived the drain bound committed")
		}
		blocked.clock.advance(interval)
		blocked.none()
	})

	t.Run("ST-SUP-RC35-OC12-T02 A tick that runs out of time counts the unreached tenants, and the next starts from them", func(t *testing.T) {
		clock := newFakeClock(aligned(time.Minute))
		var slow atomic.Bool
		slow.Store(true)
		f := startTicker(t, e, clock, time.Minute, nil, []string{"t1", "t2", "t3"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if slow.Load() {
				switch tenantID {
				case "t1":
					clock.advance(40 * time.Second)
				case "t2":
					clock.advance(25 * time.Second)
				}
			}
			return nil
		})
		f.waitArms(1)
		clock.advance(time.Minute)
		first := []string{f.next().tenant, f.next().tenant}
		f.waitTickDone()
		f.none()
		if _, _, unreached := f.observe.snapshot(); unreached != 1 || first[0] != "t1" || first[1] != "t2" {
			t.Fatalf("the first tick ran %v and counted %d unreached", first, unreached)
		}
		slow.Store(false)
		// The clock moved past the second due time in the first tick, which skipped it; the third is next.
		f.waitArms(3)
		clock.advance(time.Minute)
		second := []string{f.next().tenant, f.next().tenant, f.next().tenant}
		f.waitTickDone()
		if second[0] != "t3" {
			t.Fatalf("the second tick ran %v, want t3 first", second)
		}
		f.waitArms(4)
		clock.advance(time.Minute)
		if third := f.next().tenant; third != "t3" {
			t.Fatalf("the third tick started with %s, want t3, the tenant after t2, the last that the second tick reached", third)
		}
		f.waitTickDone()
	})

	t.Run("ST-SUP-RC35-OC05-T02 Of two tickers of a single worker only the lease holder runs, and a lost lease moves the ticks", func(t *testing.T) {
		const name = "stego.worker.sync-partner-orders"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		// RunWorker bounds a run under a lease by the lease's expiry on the lease clock and by the system clock, so
		// this fake clock starts at the system time.
		now := time.Now()
		clock := newFakeClock(time.Unix(0, (now.UnixNano()/interval.Nanoseconds())*interval.Nanoseconds()).Add(time.Second))
		settings := lease.Settings{TTL: 15 * time.Second, Renew: 10 * time.Second, Clock: clock}
		c1, err := lease.New(e.pool, name, "h1", settings)
		if err != nil {
			t.Fatal(err)
		}
		c2, err := lease.New(e.pool, name, "h2", settings)
		if err != nil {
			t.Fatal(err)
		}
		f1 := startTicker(t, e, clock, interval, c1, []string{"t1"}, nil)
		f1.waitArms(1)
		f2 := startTicker(t, e, clock, interval, c2, []string{"t1"}, nil)
		time.Sleep(200 * time.Millisecond)
		clock.advance(interval - time.Second)
		f1.next()
		f2.none()
		f1.waitTickDone()
		// h2 takes the row while it is not expired: the renew of h1 fails, so h1 stops, and h2 acquires the lease
		// that it holds now at its next retry.
		e.exec(`UPDATE leases SET holder = 'h2', token = token + 1, expires_at = now() + interval '1 minute' WHERE name = 'stego.worker.sync-partner-orders'`)
		for i := 0; i < 8; i++ {
			clock.advance(2 * time.Second)
			time.Sleep(100 * time.Millisecond)
		}
		for len(f1.runs) > 0 {
			<-f1.runs
		}
		f2.waitArms(1)
		clock.advance(interval)
		f2.next()
		f1.none()
	})
}
