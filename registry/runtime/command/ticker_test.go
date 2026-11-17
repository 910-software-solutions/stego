package command

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
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
	fired   int64 // the waits that fired
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
		c.fired++
		return ch
	}
	c.waiters = append(c.waiters, fakeWait{at: c.now.Add(d), ch: ch})
	return ch
}

// fires gives the count of the waits that fired.
func (c *fakeClock) fires() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
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
			c.fired++
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
}

// clockView is a view of a fake clock that sends the duration of each wait that it gives on asks, after the wait is
// armed, so a test knows which part of which ticker waits now and for how long.
type clockView struct {
	*fakeClock
	asks chan time.Duration
}

func (c *fakeClock) view() clockView {
	return clockView{fakeClock: c, asks: make(chan time.Duration, 256)}
}

func (v clockView) After(d time.Duration) <-chan time.Time {
	ch := v.fakeClock.After(d)
	v.asks <- d
	return ch
}

// asked reads the next wait that the view gave and fails when it is not of want. The caller knows that the code
// under test asks for the wait: it is the next step of a lease or a schedule that the test released.
func (v clockView) asked(t *testing.T, what string, want time.Duration) {
	t.Helper()
	if d := <-v.asks; d != want {
		t.Fatalf("%s waits %s, want %s", what, d, want)
	}
}

// observed records the built-in worker observations of a ticker.
type observed struct {
	mu     sync.Mutex
	failed []probe.WorkerOutcome
	// causes holds the outcome and the cause class of each worker.run.failed, as "outcome/cause".
	causes     []string
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

func (o *observed) RunFailedCause(_ context.Context, outcome probe.WorkerOutcome, cause string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed = append(o.failed, outcome)
	o.causes = append(o.causes, string(outcome)+"/"+cause)
}

// causeList gives the outcome and the cause class of each worker.run.failed, from the index from.
func (o *observed) causeList(from int) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if from > len(o.causes) {
		return nil
	}
	return append([]string(nil), o.causes[from:]...)
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

// tickerFixture is one ticker of the worker sync-partner-orders with the interval given on the fake clock. The
// steps of its scheduler and the calls of its fill come to the fixture as they happen, so a helper waits for a step
// with no wall-clock limit, and fails, not hangs, once the ticker is at rest without it.
type tickerFixture struct {
	t       *testing.T
	e       *env
	clock   *fakeClock
	observe *observed
	ticker  *Ticker
	done    chan struct{}
	cancel  context.CancelFunc

	mu       sync.Mutex
	changed  *sync.Cond
	runs     []tickerRun // the calls of the fill that no helper has read yet
	arms     int64       // the waits of the scheduler for a due time
	armedDue time.Time   // the due time of the last of them
	skips    int64
	starts   int64
	ends     int64
	parked   int   // the calls of the fill that wait for the end of their context
	parkedAt int64 // the waits of the clock that had fired when the last of them started to wait
	ran      bool  // Run returned
}

// park is the body of a fill that waits for the end of its context. While it waits, the tick moves only with the
// clock, so the fixture counts the ticker at rest while no wait of the clock fired after the fill started to wait.
// A wait that fired then can still end the context, so the fixture waits for it.
func (f *tickerFixture) park(ctx context.Context) error {
	f.mu.Lock()
	f.parked++
	f.parkedAt = f.clock.fires()
	f.changed.Broadcast()
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.parked--
	f.changed.Broadcast()
	f.mu.Unlock()
	return ctx.Err()
}

// stepped takes a step of the scheduler, on the goroutine of the ticker.
func (f *tickerFixture) stepped(s tickStep, due time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch s {
	case stepArmed:
		f.arms++
		f.armedDue = due
	case stepSkipped:
		f.skips++
	case stepStarted:
		f.starts++
	case stepEnded:
		f.ends++
	}
	f.changed.Broadcast()
}

// atRest tells, with f.mu held, whether nothing more comes from the ticker until the test moves the clock: no tick
// runs and the scheduler waits for a due time after the time now, or Run returned.
func (f *tickerFixture) atRest() bool {
	return f.ran || (f.starts == f.ends && f.armedDue.After(f.clock.Now())) || (f.parked > 0 && f.clock.fires() == f.parkedAt)
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
	return startTickerOn(t, e, clock, clock, interval, client, list, body)
}

// startTickerOn is startTickerOver with the ticker on tickClock, a view of clock.
func startTickerOn(t *testing.T, e *env, clock *fakeClock, tickClock Clock, interval time.Duration, client *lease.Client,
	list func(context.Context) ([]tenant.Tenant, error), body func(ctx context.Context, tenant string, h *Handle[testProbe]) error) *tickerFixture {
	t.Helper()
	w := e.worker("sync-partner-orders", "sync", func(s *WorkerSpec[testProbe]) { s.Interval = interval }, e.orders)
	f := &tickerFixture{t: t, e: e, clock: clock, observe: &observed{}, done: make(chan struct{})}
	f.changed = sync.NewCond(&f.mu)
	ticker, err := NewTicker(e.rt, TickerSpec[testProbe]{Worker: w, Lease: client, Observe: f.observe, Clock: tickClock,
		Tenants: list,
		Fill: func(ctx context.Context, due time.Time, h *Handle[testProbe]) error {
			run := tickerRun{tenant: h.Tenant().ID(), due: due}
			f.mu.Lock()
			f.runs = append(f.runs, run)
			f.changed.Broadcast()
			f.mu.Unlock()
			if body == nil {
				return nil
			}
			return body(ctx, run.tenant, h)
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.ticker = ticker
	ticker.step = f.stepped
	ctx, cancel := context.WithCancel(e.ctx)
	f.cancel = cancel
	go func() {
		defer close(f.done)
		_ = ticker.Run(ctx)
		f.mu.Lock()
		f.ran = true
		f.changed.Broadcast()
		f.mu.Unlock()
	}()
	t.Cleanup(func() {
		cancel()
		// After a failure a run can still wait that nothing ends: Finish then gets an ended drain bound, so it
		// ends each run at once.
		bound := newDrainBound()
		if t.Failed() {
			bound.end()
		}
		_ = ticker.Finish(bound)
		<-f.done
	})
	return f
}

// waitArms waits until the scheduler armed its wait for a due time n times in all.
func (f *tickerFixture) waitArms(n int64) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.arms < n {
		if f.atRest() {
			f.t.Fatalf("the scheduler is at rest after %d waits, want %d", f.arms, n)
		}
		f.changed.Wait()
	}
}

// waitTickDone waits until no tick that started runs.
func (f *tickerFixture) waitTickDone() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.starts > f.ends {
		f.changed.Wait()
	}
}

// next gives the next call of the fill, and fails when the ticker is at rest with no call.
func (f *tickerFixture) next() tickerRun {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.runs) == 0 {
		if f.atRest() {
			f.t.Fatal("the ticker is at rest and the fill was not called")
		}
		f.changed.Wait()
	}
	r := f.runs[0]
	f.runs = f.runs[1:]
	return r
}

// none checks that the fill has no call that no helper read. The caller first waits for the step after which a
// call would have come, such as the end of the tick.
func (f *tickerFixture) none() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runs) > 0 {
		f.t.Fatalf("the fill was called for %s at %v", f.runs[0].tenant, f.runs[0].due)
	}
}

// stopped moves the clock on by d after the ticker stopped, and checks that its scheduler starts no tick and that
// Run returns.
func (f *tickerFixture) stopped(d time.Duration) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	starts := f.starts
	f.clock.advance(d)
	for !f.ran {
		if f.starts > starts || f.arms > 0 && f.atRest() {
			f.t.Fatalf("the stopped ticker started %d ticks and Run returned %v", f.starts-starts, f.ran)
		}
		f.changed.Wait()
	}
	if f.starts > starts || len(f.runs) > 0 {
		f.t.Fatalf("the stopped ticker started %d ticks and called the fill %d times", f.starts-starts, len(f.runs))
	}
}

// failedRuns gives the failed runs that the observer holds, and fails when they are fewer than n. The caller first
// waits for the end of the tick, which reports each failed run before it ends.
func (f *tickerFixture) failedRuns(n int) []probe.WorkerOutcome {
	f.t.Helper()
	failed, _, _ := f.observe.snapshot()
	if len(failed) < n {
		f.t.Fatalf("%d failed runs, want %d", len(failed), n)
	}
	return failed
}

// waitSkipped waits until the scheduler skipped n ticks.
func (f *tickerFixture) waitSkipped(n int64) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.skips < n {
		if f.atRest() {
			f.t.Errorf("the scheduler is at rest after %d skipped ticks, want %d", f.skips, n)
			return
		}
		f.changed.Wait()
	}
}

// drainBound is the drain bound of Finish in a test: a context that ends as at its deadline when the test ends it.
type drainBound struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newDrainBound() *drainBound {
	return &drainBound{Context: context.Background(), done: make(chan struct{})}
}

func (b *drainBound) Done() <-chan struct{} { return b.done }

func (b *drainBound) Err() error {
	select {
	case <-b.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (b *drainBound) end() { b.once.Do(func() { close(b.done) }) }

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
		// A failed check ends the subtest with the run of t1 blocked; the run is let go before the cleanup of the
		// ticker waits for it.
		letGo := sync.OnceFunc(func() { close(release) })
		t.Cleanup(letGo)
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
		letGo()
		f.waitTickDone()
	})

	t.Run("ST-SUP-RC35-OC07-T02 A slow tenant ends at its own deadline and the next tenant gets its own", func(t *testing.T) {
		e.exec(`UPDATE orders SET status = 'new'`)
		clock := newFakeClock(aligned(time.Minute))
		var f *tickerFixture
		f = startTicker(t, e, clock, time.Minute, nil, []string{"t1", "t2"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if err := write(ctx, h, "slow-"+tenantID); err != nil {
				return err
			}
			if tenantID == "t1" {
				return f.park(ctx)
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
		if failed := f.failedRuns(1); len(failed) != 1 || failed[0] != probe.WorkerRolledBack {
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
		if failed := f.failedRuns(1); failed[0] != probe.WorkerPanicked {
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

	t.Run("ST-SUP-RC35-OC09-T02 Each run that does not commit is one worker.run.failed with its fixed outcome and cause class only", func(t *testing.T) {
		clock := newFakeClock(aligned(interval))
		var end atomic.Value
		end.Store("nil")
		f := startTicker(t, e, clock, interval, nil, []string{"t1"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			switch end.Load() {
			case "error":
				return errors.New("secret-7")
			case "panic":
				panic("secret-7")
			case "missing":
				_, err := repo(t, h, e.orders).Get(ctx, "secret-7")
				return err
			}
			return nil
		})
		for i, next := range []string{"nil", "error", "panic", "missing"} {
			end.Store(next)
			f.waitArms(int64(i + 1))
			clock.advance(interval)
			f.next()
			f.waitTickDone()
		}
		failed := f.failedRuns(3)
		if len(failed) != 3 || failed[0] != probe.WorkerRolledBack || failed[1] != probe.WorkerPanicked {
			t.Fatalf("the failed runs are %v, want rolled_back, panicked and rolled_back", failed)
		}
		// The cause class is fixed and holds no text of the error (ST-SUP-RC35-OC06, ST-SUP-RC35-OC15).
		if got := f.observe.causeList(0); strings.Join(got, ",") != "rolled_back/fill_error,panicked/panic,rolled_back/not_found" {
			t.Fatalf("the failed runs sent %v", got)
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
		letGo := sync.OnceFunc(func() { close(release) })
		t.Cleanup(letGo)
		f.waitArms(1)
		clock.advance(interval)
		f.next()
		finished := make(chan error, 1)
		go func() { finished <- f.ticker.Finish(context.Background()) }()
		// Finish stops the ticker first, and then waits for the run.
		<-f.ticker.stopping
		letGo()
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
		f.stopped(interval)

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
		// The drain bound ends while Finish waits for the run, which waits for its context.
		bound := newDrainBound()
		go func() { finished <- blocked.ticker.Finish(bound) }()
		<-blocked.ticker.stopping
		bound.end()
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Finish gave %v, want the end of its bound", err)
		}
		if e.status("t2") == "blocked-t2" {
			t.Fatal("the run that outlived the drain bound committed")
		}
		blocked.stopped(interval)
	})

	t.Run("ST-SUP-RC35-OC12-T02 A tick that runs out of time counts the unreached tenants, and the next starts from them", func(t *testing.T) {
		clock := newFakeClock(aligned(time.Minute))
		var slow atomic.Bool
		slow.Store(true)
		var f *tickerFixture
		f = startTicker(t, e, clock, time.Minute, nil, []string{"t1", "t2", "t3"}, func(ctx context.Context, tenantID string, h *Handle[testProbe]) error {
			if slow.Load() {
				switch tenantID {
				case "t1":
					// The scheduler waits for the second due time before the clock passes it.
					f.waitArms(2)
					clock.advance(40 * time.Second)
				case "t2":
					clock.advance(25 * time.Second)
					// The second due time comes while this tick runs: the scheduler skips it before the tick ends.
					f.waitSkipped(1)
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
		// this fake clock starts at the system time. A is the due time before it: the clock starts at A + 1 s.
		now := time.Now()
		a := time.Unix(0, (now.UnixNano()/interval.Nanoseconds())*interval.Nanoseconds())
		clock := newFakeClock(a.Add(time.Second))
		// Each lease client and each ticker waits on its own view of the clock, so the test reads which one waits
		// and for how long, and moves the clock only when each is in its wait.
		lease1, lease2, tick1, tick2 := clock.view(), clock.view(), clock.view(), clock.view()
		const ttl, renew, retry = 15 * time.Second, 10 * time.Second, lease.DefaultRetry
		c1, err := lease.New(e.pool, name, "h1", lease.Settings{TTL: ttl, Renew: renew, Clock: lease1})
		if err != nil {
			t.Fatal(err)
		}
		c2, err := lease.New(e.pool, name, "h2", lease.Settings{TTL: ttl, Renew: renew, Clock: lease2})
		if err != nil {
			t.Fatal(err)
		}
		tenants := func(context.Context) ([]tenant.Tenant, error) { return []tenant.Tenant{mustTenant(t, "t1")}, nil }
		// h1 acquires the lease: it waits to renew it, and its ticker waits for A + 10 s.
		f1 := startTickerOn(t, e, clock, tick1, interval, c1, tenants, nil)
		tick1.asked(t, "the ticker of h1", interval-time.Second)
		// h2 finds the lease taken: its first wait is the retry of its lease, not a due time of its ticker.
		f2 := startTickerOn(t, e, clock, tick2, interval, c2, tenants, nil)
		select {
		case d := <-lease2.asks:
			if d != retry {
				t.Fatalf("the lease of h2 waits %s, want the retry %s", d, retry)
			}
		case d := <-tick2.asks:
			t.Fatalf("the ticker of h2 waits %s for a due time with no lease", d)
		}
		// h1 waits to renew its lease; it is read only now, because a ticker with no lease has no renew to wait for.
		lease1.asked(t, "the lease of h1", renew)
		// A + 10 s: the tick runs on h1 only; h2 tries again, finds the lease taken, and waits to retry.
		clock.advance(interval - time.Second)
		f1.next()
		lease2.asked(t, "the lease of h2", retry)
		f1.waitTickDone()
		// The ticker of h1 waits for A + 20 s, and the run of t1 had its bound at A + 20 s too.
		tick1.asked(t, "the ticker of h1", interval)
		tick1.asked(t, "the ticker of h1", interval)
		f2.none()
		// h2 takes the row while it is not expired: the renew of h1 at A + 11 s fails, so h1 stops its ticks and
		// waits to retry. At A + 12 s h2 acquires the lease that it holds now, and its ticker waits for A + 20 s.
		e.exec(`UPDATE leases SET holder = 'h2', token = token + 1, expires_at = now() + interval '1 minute' WHERE name = 'stego.worker.sync-partner-orders'`)
		clock.advance(time.Second)
		tick1.asked(t, "the ticker of h1 after the renew of its lease failed", retry)
		clock.advance(time.Second)
		lease2.asked(t, "the lease of h2 after it acquired", renew)
		tick2.asked(t, "the ticker of h2", interval-2*time.Second)
		// A + 20 s: the tick runs on h2 only; h1 tries again at A + 13 s and finds the lease taken.
		clock.advance(interval - 2*time.Second)
		f2.next()
		lease1.asked(t, "the lease of h1", retry)
		f1.none()
	})
}
