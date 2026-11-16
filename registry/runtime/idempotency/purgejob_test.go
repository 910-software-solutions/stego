package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// jobClock is a process clock that moves only when the test advances it. Each call of After sends its duration on
// asked, so the test knows when the job waits on the clock, and how long, with no wait on the real clock.
type jobClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []jobWaiter
	asked   chan time.Duration
}

type jobWaiter struct {
	at time.Time
	ch chan time.Time
}

func newJobClock() *jobClock {
	return &jobClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), asked: make(chan time.Duration, 64)}
}

func (c *jobClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *jobClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, jobWaiter{at: c.now.Add(d), ch: ch})
	c.asked <- d
	return ch
}

func (c *jobClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// count is the number of timers that wait now.
func (c *jobClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// waits receives the next wait of the job on the clock and fails the test when it is not of d. The job asks for the
// wait after its purge is counted, so the counts are final when it comes.
func (c *jobClock) waits(t *testing.T, d time.Duration) {
	t.Helper()
	if got := <-c.asked; got != d {
		t.Fatalf("the job waits %v on the clock, want %v", got, d)
	}
}

func TestPurgeJob(t *testing.T) {
	t.Run("ST-SUP-RP07-LC09-T01 The purge job runs on its interval and stops with its context", func(t *testing.T) {
		db, _ := testDB(t)
		clock := newJobClock()
		insertExpiring(t, db, "old", 7, clock.Now().Add(-time.Second))
		insertExpiring(t, db, "later", 3, clock.Now().Add(90*time.Second))
		insertExpiring(t, db, "live", 4, clock.Now().Add(time.Hour))
		rec := &countingExecer{db: db}
		j, err := NewPurgeJob(rec, PurgeJobConfig{Schema: testSchema(t, db), Interval: time.Minute, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- j.Run(ctx) }()
		clock.waits(t, time.Minute)
		if j.Runs() != 1 || countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'old%'`) != 0 ||
			countRows(t, db, `SELECT count(*) FROM idempotency_keys`) != 7 {
			t.Fatalf("at start: %d runs, deletes %v; want the 7 expired rows gone", j.Runs(), rec.batches())
		}
		// 59 s do not end the wait of the interval: the timer still waits, and the job purges no more.
		clock.Advance(59 * time.Second)
		if clock.count() != 1 || j.Runs() != 1 || len(clock.asked) != 0 {
			t.Fatal("the job purged before the interval passed")
		}
		clock.Advance(time.Second)
		clock.waits(t, time.Minute)
		if j.Runs() != 2 {
			t.Fatalf("after the first tick: %d runs, want 2", j.Runs())
		}
		clock.Advance(time.Minute)
		clock.waits(t, time.Minute)
		if j.Runs() != 3 || len(rec.batches()) != 3 || rec.batches()[2] != 3 {
			t.Fatalf("after the second tick: %d runs, deletes %v; want one purge per tick, the third of the 3 rows due by then", j.Runs(), rec.batches())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run gave %v after the cancel", err)
		}
		// Run returned: a tick after it purges nothing.
		clock.Advance(time.Minute)
		if j.Runs() != 3 || len(rec.batches()) != 3 || len(clock.asked) != 0 {
			t.Fatal("the job purged after its context ended")
		}

		// A purge whose pause waits on the job's clock ends with ctx, and is not counted as a run.
		pauses := newJobClock()
		insertExpiring(t, db, "many", 5, pauses.Now().Add(-time.Second))
		paused, err := NewPurgeJob(db, PurgeJobConfig{Schema: testSchema(t, db), Batch: 2, Pause: time.Hour, Clock: pauses})
		if err != nil {
			t.Fatal(err)
		}
		pctx, pcancel := context.WithCancel(context.Background())
		done = make(chan error, 1)
		go func() { done <- paused.Run(pctx) }()
		pauses.waits(t, time.Hour)
		pcancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run gave %v", err)
		}
		if paused.Runs() != 0 || paused.Failures() != 0 {
			t.Fatalf("the cancelled purge counts %d runs and %d failures, want none", paused.Runs(), paused.Failures())
		}

		// A failed purge is counted and reported, and the next tick runs again.
		fails := newJobClock()
		failing, err := NewPurgeJob(failingQuerier{db: closedDB(t)}, PurgeJobConfig{Clock: fails})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = failing.Run(ctx) }()
		fails.waits(t, DefaultPurgeInterval)
		if failing.Failures() != 1 || failing.Runs() != 1 {
			t.Fatalf("the failing job has %d runs and %d failures, want 1 and 1", failing.Runs(), failing.Failures())
		}
		fails.Advance(DefaultPurgeInterval)
		fails.waits(t, DefaultPurgeInterval)
		if failing.Failures() != 2 || failing.Runs() != 2 {
			t.Fatalf("the failing job has %d runs and %d failures, want 2 and 2", failing.Runs(), failing.Failures())
		}
		if ok, why := failing.Health(); ok || why == "" {
			t.Fatal("the failed purge is not reported")
		}

		// Two runs of one job never purge at once. The first purge holds its batch open until the second run is
		// blocked: in the lock of the job, or in a batch of its own, which is the fault.
		twice := newJobClock()
		gate := &gatedQuerier{db: db, entered: make(chan struct{}, 4), release: make(chan struct{})}
		both, err := NewPurgeJob(gate, PurgeJobConfig{Schema: testSchema(t, db), Clock: twice})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = both.Run(ctx) }()
		go func() { _ = both.Run(ctx) }()
		<-gate.entered
		waiting := secondRunBlocked(gate)
		close(gate.release)
		twice.waits(t, DefaultPurgeInterval)
		twice.waits(t, DefaultPurgeInterval)
		if !waiting || gate.most() != 1 || both.Runs() != 2 {
			t.Fatalf("the two runs made %d purges with at most %d at once, want 2 one after the other", both.Runs(), gate.most())
		}
	})
}

// secondRunBlocked waits until the second purge is blocked, and reports whether it waits on the lock of the job.
// It reads the stacks of the goroutines and yields between reads; it uses no clock. A second purge that begins a
// batch while the first holds one is seen in entered.
func secondRunBlocked(gate *gatedQuerier) bool {
	buf := make([]byte, 1<<20)
	for {
		if len(gate.entered) > 0 {
			return false
		}
		stacks := string(buf[:runtime.Stack(buf, true)])
		for _, g := range strings.Split(stacks, "\n\n") {
			if strings.Contains(g, "[sync.Mutex.Lock") && strings.Contains(g, "(*PurgeJob).purge") {
				return true
			}
		}
		runtime.Gosched()
	}
}

// failingQuerier is a database that is down: each statement fails.
type failingQuerier struct{ db *sql.DB }

func (f failingQuerier) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return f.db.BeginTx(ctx, opts)
}

// closedDB is a pool that is closed, so each statement on it fails at once.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	closed, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	return closed
}

// gatedQuerier signals each batch that begins on entered and holds it until release is closed. It records the most
// batches that began at once.
type gatedQuerier struct {
	db           *sql.DB
	entered      chan struct{}
	release      chan struct{}
	mu           sync.Mutex
	active, peak int
}

func (g *gatedQuerier) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	g.mu.Lock()
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
	return g.db.BeginTx(ctx, opts)
}

func (g *gatedQuerier) most() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}
