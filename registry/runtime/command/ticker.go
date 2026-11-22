package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// RunBound is the longest time that one tenant's run of a tick, or one consumed message, may take
// (ST-SUP-RC35-OC07).
const RunBound = 30 * time.Second

// Clock is the clock of a ticker: the time now and a channel that gets a value after a duration. lease.Clock is one.
type Clock = lease.Clock

// systemTime is the system clock.
type systemTime struct{}

func (systemTime) Now() time.Time                         { return time.Now() }
func (systemTime) After(d time.Duration) <-chan time.Time { return time.After(d) }

// WorkerObserver gets the built-in worker observations of one worker (ST-SUP-RP12-LC21): probe.Worker is one.
type WorkerObserver interface {
	RunFailed(ctx context.Context, outcome probe.WorkerOutcome)
	TickFailed(ctx context.Context)
	TickSkipped()
	TenantUnreached()
}

// TickerSpec is what a ticker of one worker with an every trigger needs.
type TickerSpec[P any] struct {
	// Worker is the checked declaration of the worker; its interval sets the ticks.
	Worker *Worker[P]
	// Fill is the worker's one fill.
	Fill TickFill[P]
	// Tenants gives the tenants of a tick: the tenant list of the tenant runtime, which holds no retired tenant
	// (ST-SUP-RP04-LC07, ST-SUP-RC27-OC12).
	Tenants func(ctx context.Context) ([]tenant.Tenant, error)
	// Lease is the client of the lease stego.worker.<worker name> of a single worker, or nil for a worker that every
	// instance runs (ST-SUP-RC35-OC05).
	Lease *lease.Client
	// Observe gets the built-in worker observations.
	Observe WorkerObserver
	// Clock is the clock of the ticks and of the deadlines; nil is the system clock.
	Clock Clock
}

// Ticker runs the ticks of one worker with an every trigger (ST-SUP-RC35-OC04): a tick starts at each multiple of
// the interval since the Unix epoch, runs RunWorker once for each tenant of the tenant list in its own transaction,
// and never overlaps the previous tick; a tick that comes while one runs is skipped and counted as
// worker.tick.skipped. Each tenant's run has its own deadline (ST-SUP-RC35-OC07), a failed or panicked run ends only
// itself (ST-SUP-RC35-OC08), each run that does not commit is reported as worker.run.failed (ST-SUP-RC35-OC09), and a
// tick starts from the tenant after the last one that the previous tick reached and counts each tenant it does not
// reach as worker.tenant.unreached (ST-SUP-RC35-OC12). A single worker ticks only while it holds its lease
// (ST-SUP-RC35-OC05). Stop and Finish end it at shutdown (ST-SUP-RC35-OC10).
type Ticker struct {
	name     string
	interval time.Duration
	tenants  func(ctx context.Context) ([]tenant.Tenant, error)
	lease    *lease.Client
	observe  WorkerObserver
	clock    Clock
	run      func(ctx context.Context, due time.Time, t tenant.Tenant, held *lease.Held) error

	// abort ends every run that still runs when Finish gives up waiting.
	abort     context.Context
	abortRuns context.CancelFunc
	stopping  chan struct{}
	stopOnce  sync.Once

	mu      sync.Mutex
	busy    bool
	cursor  string // the ID of the tenant that the next tick starts with, or empty for the first tenant
	ticks   sync.WaitGroup
	running sync.WaitGroup
	started bool
	// step gets each step of the scheduler as it happens: a wait armed for a due time, a tick skipped, and the start
	// and the end of a tick. A test of the package sets it before Run, so it knows when the ticker is at rest with no
	// wall-clock wait; it is nil outside the tests.
	step func(s tickStep, due time.Time)
}

// tickStep is one step of the scheduler of a ticker.
type tickStep int

const (
	stepArmed tickStep = iota
	stepSkipped
	stepStarted
	stepEnded
)

// stepped gives the step to the hook of the tests, when there is one.
func (k *Ticker) stepped(s tickStep, due time.Time) {
	if k.step != nil {
		k.step(s, due)
	}
}

// NewTicker checks spec and gives the ticker of its worker on the command runtime rt.
func NewTicker[P any](rt *Runtime, spec TickerSpec[P]) (*Ticker, error) {
	switch {
	case rt == nil || spec.Worker == nil || spec.Fill == nil || spec.Tenants == nil || spec.Observe == nil:
		return nil, errors.New("command: a ticker needs the runtime, the worker, its fill, the tenant list and the observer")
	case spec.Worker.spec.Interval <= 0:
		return nil, fmt.Errorf("command: the worker %s has no interval, so it has no ticks", spec.Worker.spec.Name)
	}
	clock := spec.Clock
	if clock == nil {
		clock = systemTime{}
	}
	abort, abortRuns := context.WithCancel(context.Background())
	w, fill := spec.Worker, spec.Fill
	return &Ticker{
		name: w.spec.Name, interval: w.spec.Interval, tenants: spec.Tenants, lease: spec.Lease, observe: spec.Observe,
		clock: clock, abort: abort, abortRuns: abortRuns, stopping: make(chan struct{}),
		run: func(ctx context.Context, due time.Time, t tenant.Tenant, held *lease.Held) error {
			return RunWorker(ctx, rt, w, due, t, held, fill)
		},
	}, nil
}

// Name is the worker's name.
func (k *Ticker) Name() string { return k.name }

// Run schedules the ticks until ctx ends or Stop is called. A single worker first acquires its lease, retries while
// another holder has it, and starts a tick only while it holds it; when the lease is lost, the running tick ends at
// once and Run tries to acquire the lease again.
func (k *Ticker) Run(ctx context.Context) error {
	k.mu.Lock()
	if k.started {
		k.mu.Unlock()
		return fmt.Errorf("command: the ticker of %s runs already", k.name)
	}
	k.started = true
	k.mu.Unlock()
	if k.lease == nil {
		k.schedule(ctx, nil)
		return nil
	}
	// Stop does not end the hold: the running tick drains under the held lease, and the lease is released after it.
	for ctx.Err() == nil && !k.isStopping() {
		_ = k.lease.Hold(ctx, func(ctx context.Context, held *lease.Held) error {
			k.schedule(ctx, held)
			return nil
		})
		select {
		case <-ctx.Done():
		case <-k.stopping:
		case <-k.clock.After(k.lease.RetryEvery()):
		}
	}
	return nil
}

// schedule starts a tick at each due time while ctx runs, and waits for the running tick before it returns.
func (k *Ticker) schedule(ctx context.Context, held *lease.Held) {
	defer k.ticks.Wait()
	for {
		now := k.clock.Now()
		due := nextDue(now, k.interval)
		wait := k.clock.After(due.Sub(now))
		k.stepped(stepArmed, due)
		select {
		case <-ctx.Done():
			return
		case <-k.stopping:
			return
		case <-wait:
		}
		if ctx.Err() != nil || k.isStopping() {
			return
		}
		k.mu.Lock()
		if k.busy {
			k.mu.Unlock()
			// A tick that comes while one runs is skipped, never queued (ST-SUP-RC35-OC04).
			k.observe.TickSkipped()
			k.stepped(stepSkipped, due)
			continue
		}
		k.busy = true
		k.ticks.Add(1)
		k.running.Add(1)
		k.mu.Unlock()
		k.stepped(stepStarted, due)
		go func() {
			defer k.running.Done()
			defer k.ticks.Done()
			defer func() {
				k.mu.Lock()
				k.busy = false
				k.mu.Unlock()
				k.stepped(stepEnded, due)
			}()
			k.tick(ctx, due, held)
		}()
	}
}

// nextDue is the first multiple of interval since the Unix epoch after now.
func nextDue(now time.Time, interval time.Duration) time.Time {
	n := now.UnixNano()
	step := interval.Nanoseconds()
	return time.Unix(0, (n/step+1)*step)
}

// tick runs one tick due at due: each tenant of the list once, from the tenant that the cursor names, until the
// next tick is due, ctx ends (a lost lease, the shutdown) or Stop is called.
func (k *Ticker) tick(ctx context.Context, due time.Time, held *lease.Held) {
	next := due.Add(k.interval)
	tenants, err := k.tenants(ctx)
	if err != nil {
		// A tick that cannot read the tenant list runs no tenant: it is reported, with the cause named and no error
		// text, so a worker that stops is seen (security review of PR #337, L2).
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "command: the tick of the worker did not read the tenant list", "worker", k.name, "cause", "tenant_list_read")
			k.observe.TickFailed(context.WithoutCancel(ctx))
		}
		return
	}
	if len(tenants) == 0 {
		return
	}
	order := k.rotate(tenants)
	for i, t := range order {
		if k.ended(ctx, next) {
			// The tick ends: each tenant it did not reach is counted, and the next tick starts with the first of
			// them (ST-SUP-RC35-OC12).
			for range order[i:] {
				k.observe.TenantUnreached()
			}
			return
		}
		k.runOne(ctx, due, next, t, held)
		// The next tick starts with the tenant after the last one that this tick reached.
		k.setCursor(order[(i+1)%len(order)].ID())
	}
}

// ended tells whether the tick must start no further tenant run.
func (k *Ticker) ended(ctx context.Context, next time.Time) bool {
	return k.isStopping() || ctx.Err() != nil || !k.clock.Now().Before(next)
}

// isStopping tells whether Stop was called.
func (k *Ticker) isStopping() bool {
	select {
	case <-k.stopping:
		return true
	default:
		return false
	}
}

// rotate gives the tenants in the order of the list, from the first one whose ID is the cursor or after it.
func (k *Ticker) rotate(tenants []tenant.Tenant) []tenant.Tenant {
	k.mu.Lock()
	cursor := k.cursor
	k.mu.Unlock()
	start := 0
	if cursor != "" {
		for i, t := range tenants {
			if t.ID() >= cursor {
				start = i
				break
			}
		}
	}
	return append(append([]tenant.Tenant{}, tenants[start:]...), tenants[:start]...)
}

func (k *Ticker) setCursor(id string) {
	k.mu.Lock()
	k.cursor = id
	k.mu.Unlock()
}

// bounded gives a context that ends at the earliest of RunBound after now, the time limit, the end of ctx and the
// abort of Finish, on the ticker's clock.
func (k *Ticker) bounded(ctx context.Context, limit time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	now := k.clock.Now()
	wait := RunBound
	if until := limit.Sub(now); until < wait {
		wait = until
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-k.abort.Done():
		case <-k.clock.After(wait):
		}
		cancel()
	}()
	return ctx, cancel
}

// runOne runs the tick of one tenant under its own deadline (ST-SUP-RC35-OC07): the earliest of RunBound, the next
// due time and the abort of Finish; RunWorker gives the deadline of a held lease. A lost lease ends the run at once.
// A run that does not commit is reported with its fixed outcome, and a panic ends only this run
// (ST-SUP-RC35-OC08, ST-SUP-RC35-OC09).
func (k *Ticker) runOne(ctx context.Context, due, next time.Time, t tenant.Tenant, held *lease.Held) {
	runCtx, cancel := k.bounded(ctx, next)
	defer cancel()
	if held != nil {
		// The context of the hold ends when the lease is lost; the run ends with it.
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
	}
	err := func() (err error) {
		defer func() {
			if recover() != nil {
				err = errPanic
			}
		}()
		return k.run(runCtx, due, t, held)
	}()
	// A tenant that the gate holds as not active, such as one retired after the tick read the list, is skipped and
	// is no failed run (security review of PR #337, L1).
	if err == nil {
		// A run that committed is sent as worker.run.completed (decision on ST-SUP-Q210).
		runCompleted(context.WithoutCancel(ctx), k.observe)
		return
	}
	if errors.Is(err, errTenantNotActive) {
		return
	}
	// The event carries the fixed cause class of the run's error, such as internal for a list that fails
	// (ST-SUP-RC35-OC15).
	runFailed(context.WithoutCancel(ctx), k.observe, workerOutcome(err), causeOf(err))
}

// Stop makes the ticker start no new tick and no new tenant run (ST-SUP-RC35-OC10). A later call does nothing.
func (k *Ticker) Stop() { k.stopOnce.Do(func() { close(k.stopping) }) }

// Finish waits for the running tick until ctx ends, which the worker mode bounds with its drain bound
// (ST-SUP-RP09-LC06); it then ends each run that still runs, which rolls back, and waits for it (ST-SUP-RC35-OC10).
// It returns the error of ctx when it had to end a run.
func (k *Ticker) Finish(ctx context.Context) error {
	k.Stop()
	done := make(chan struct{})
	go func() {
		k.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		k.abortRuns()
		<-done
		return ctx.Err()
	}
}
