package lease

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/910-software-solutions/stego/registry/runtime/internal/pgtest"
)

// The integration tests of this package run against the local PostgreSQL server of pgtest: the server that
// STEGO_TEST_POSTGRES_DSN names, else a container that the test binary starts and removes (ST-STD-LOCAL-R04).
// Each test makes its own database on that server and drops it at the end.

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04). When the stall
// variable names a database, the test binary is instead the holder of ST-SUP-RP09-LC04-T04, which stops inside a
// write under the lease.
func TestMain(m *testing.M) {
	if dsn := os.Getenv(stallVariable); dsn != "" {
		os.Exit(stallInWrite(dsn))
	}
	os.Exit(pgtest.Main(m))
}

// stallVariable names the database of the holder that stops inside a write under the lease.
const stallVariable = "STEGO_LEASE_TEST_STALL_DSN"

// stallTTL is the lease of the holder that stops inside a write under the lease.
const stallTTL = 2 * time.Second

// stallInWrite acquires the lease stego.controller as the holder "stalled", opens a write under the lease, writes
// "fenced" when the write holds the lease row, and then waits in the write, so the test can stop the process with
// the transaction open.
func stallInWrite(dsn string) int {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	db := stdlib.OpenDB(*config)
	defer func() { _ = db.Close() }()
	c, err := New(db, "stego.controller", "stalled", Settings{TTL: stallTTL, Renew: stallTTL / 2})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h, err := c.Acquire(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	err = h.WithToken(context.Background(), func(ctx context.Context, _ *sql.Tx, _ int64) error {
		fmt.Println("fenced")
		<-ctx.Done()
		return ctx.Err()
	})
	fmt.Fprintln(os.Stderr, err)
	return 1
}

// statement is one statement that the recorder read, with the connection that sent it.
type statement struct {
	conn *pgx.Conn
	sql  string
	args []any
}

// recorder reads every statement that a connection of the database sends.
type recorder struct {
	mu         sync.Mutex
	statements []statement
}

func (r *recorder) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	var args []any
	for _, arg := range data.Args {
		if _, ok := arg.(pgx.QueryResultFormatsByOID); !ok {
			args = append(args, arg)
		}
	}
	r.statements = append(r.statements, statement{conn: conn, sql: data.SQL, args: args})
	return ctx
}

func (r *recorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *recorder) all() []statement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]statement(nil), r.statements...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = nil
}

// database is one test database: db sends through the recorder, other is a second session with no recorder.
type database struct {
	db    *sql.DB
	other *sql.DB
	rec   *recorder
	dsn   string
}

func openDatabase(t *testing.T) *database {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.Database(t, "lease")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	plain := config.Copy()
	rec := &recorder{}
	config.Tracer = rec
	d := &database{db: stdlib.OpenDB(*config), other: stdlib.OpenDB(*plain), rec: rec, dsn: dsn}
	t.Cleanup(func() { _ = d.db.Close(); _ = d.other.Close() })
	for _, ddl := range []string{LeasesTable, "CREATE SCHEMA stego_schema", BackfillsTable, BackfillFunctions} {
		if _, err := d.other.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *database) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := d.other.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// leaseRow reads the lease row from the second session.
func (d *database) leaseRow(t *testing.T, name string) (holder string, token int64, expires time.Time) {
	t.Helper()
	err := d.other.QueryRowContext(context.Background(), "SELECT holder, token, CASE WHEN isfinite(expires_at) THEN expires_at ELSE 'epoch' END FROM leases WHERE name=$1", name).Scan(&holder, &token, &expires)
	if err != nil {
		t.Fatal(err)
	}
	return holder, token, expires
}

// expire moves expires_at of the lease into the past from the second session, as the database clock would.
func (d *database) expire(t *testing.T, name string) {
	t.Helper()
	d.exec(t, "UPDATE leases SET expires_at=now()-interval '1 second' WHERE name=$1", name)
}

// fakeClock is a process clock that moves only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Now()} }

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
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
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

// waitWaiters waits on the real clock until n timers wait on the fake clock.
func (c *fakeClock) waitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.waiters)
		c.mu.Unlock()
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %d timers wait on the fake clock", n)
}

func newClient(t *testing.T, db DB, name, holder string, settings Settings) *Client {
	t.Helper()
	c, err := New(db, name, holder, settings)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func acquire(t *testing.T, c *Client) *Held {
	t.Helper()
	h, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire as %s: %v", c.Holder(), err)
	}
	return h
}

// leaseStatements is the recorded statements that name the leases table.
func leaseStatements(statements []statement) []statement {
	var out []statement
	for _, s := range statements {
		if strings.Contains(s.sql, "leases") {
			out = append(out, s)
		}
	}
	return out
}

// noDB is a DB for the unit tests that need no database.
type noDB struct{}

func (noDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("no database")
}
func (noDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }
func (noDB) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	return nil, errors.New("no database")
}

func TestLeaseIsOneRow(t *testing.T) {
	t.Run("ST-SUP-RP09-LC02-T01 Acquire and Renew each run their own declared UPDATE", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		c := newClient(t, d.db, "stego.controller", "h1", Settings{})
		d.rec.reset()
		h := acquire(t, c)
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		wantAcquire := "UPDATE public.leases SET holder=$1, expires_at=pg_catalog.now() OPERATOR(pg_catalog.+) ttl, token=token OPERATOR(pg_catalog.+) 1 WHERE name OPERATOR(pg_catalog.=) $2 AND (holder OPERATOR(pg_catalog.=) $1 OR expires_at OPERATOR(pg_catalog.<) pg_catalog.now()) RETURNING token"
		wantRenew := "UPDATE public.leases SET expires_at=pg_catalog.now() OPERATOR(pg_catalog.+) ttl, token=token OPERATOR(pg_catalog.+) 1 WHERE name OPERATOR(pg_catalog.=) $2 AND holder OPERATOR(pg_catalog.=) $1 AND token OPERATOR(pg_catalog.=) $3 AND expires_at OPERATOR(pg_catalog.>=) pg_catalog.now() RETURNING token"
		sent := d.rec.all()
		if len(sent) != 2 {
			t.Fatalf("Acquire and Renew sent %d statements, want 2: %v", len(sent), sent)
		}
		if got := strings.Replace(sent[0].sql, "pg_catalog.make_interval(secs => $3)", "ttl", 1); got != wantAcquire {
			t.Errorf("Acquire sent %q, want %q with ttl as $3", sent[0].sql, wantAcquire)
		}
		if a := sent[0].args; len(a) != 3 || a[0] != "h1" || a[1] != "stego.controller" || a[2] != 15.0 {
			t.Errorf("Acquire binds %v, want h1, stego.controller and the lease of 15 s", a)
		}
		if got := strings.Replace(sent[1].sql, "pg_catalog.make_interval(secs => $4)", "ttl", 1); got != wantRenew {
			t.Errorf("Renew sent %q, want %q with ttl as $4", sent[1].sql, wantRenew)
		}
		if a := sent[1].args; len(a) != 4 || a[0] != "h1" || a[1] != "stego.controller" || a[2] != h.Token()-1 || a[3] != 15.0 {
			t.Errorf("Renew binds %v, want h1, stego.controller, the token it held and the lease of 15 s", a)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T08 Acquire and Renew run the functions and operators of pg_catalog on a search path that puts another schema before it", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		// A schema before pg_catalog on the path has its own now(), make_interval and + of bigint and integer, each
		// an exact match for the statements, which would set the lease far into the future and add 100 to the token.
		for _, statement := range []string{
			"CREATE SCHEMA hostile",
			"CREATE FUNCTION hostile.now() RETURNS timestamptz LANGUAGE sql AS $$SELECT pg_catalog.now() + interval '100 years'$$",
			"CREATE FUNCTION hostile.make_interval(secs double precision) RETURNS interval LANGUAGE sql AS $$SELECT interval '100 years'$$",
			"CREATE FUNCTION hostile.add(bigint, integer) RETURNS bigint LANGUAGE sql AS $$SELECT $1 OPERATOR(pg_catalog.+) 100$$",
			"CREATE OPERATOR hostile.+ (LEFTARG = bigint, RIGHTARG = integer, FUNCTION = hostile.add)",
			"CREATE FUNCTION hostile.lt(timestamptz, timestamptz) RETURNS boolean LANGUAGE sql AS $$SELECT true$$",
			"CREATE OPERATOR hostile.< (LEFTARG = timestamptz, RIGHTARG = timestamptz, FUNCTION = hostile.lt)",
		} {
			d.exec(t, statement)
		}
		conn, err := d.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "SET search_path = hostile, pg_catalog, public"); err != nil {
			t.Fatal(err)
		}
		// The schema is live for a plain statement of the session.
		var far bool
		if err := conn.QueryRowContext(ctx, "SELECT now() > pg_catalog.now() + interval '50 years'").Scan(&far); err != nil || !far {
			t.Fatalf("the hostile now() is not live on the path (error %v)", err)
		}
		_, before, _ := d.leaseRow(t, "stego.controller")
		h1 := acquire(t, newClient(t, conn, "stego.controller", "h1", Settings{TTL: 15 * time.Second}))
		check := func(what string, want int64) {
			t.Helper()
			holder, token, expires := d.leaseRow(t, "stego.controller")
			if holder != "h1" || token != want || h1.Token() != want || time.Until(expires) > time.Minute {
				t.Fatalf("after %s the row holds %s with the token %d (the holder has %d) until %v, want h1 with %d for at most 15 s", what, holder, token, h1.Token(), expires, want)
			}
		}
		check("the acquire", before+1)
		if err := h1.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		check("the renew", before+2)
		// Another holder cannot take the unexpired lease through the hostile < either.
		if _, err := newClient(t, conn, "stego.controller", "h2", Settings{}).Acquire(ctx); !errors.Is(err, ErrTaken) {
			t.Fatalf("h2 acquired the unexpired lease of h1 on the hostile path: %v", err)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T02 Each acquire and renew increments the token", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		_, n, _ := d.leaseRow(t, "stego.controller")
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{TTL: time.Second, Renew: 500 * time.Millisecond}))
		tokens := []int64{h1.Token()}
		for range 2 {
			if err := h1.Renew(ctx); err != nil {
				t.Fatal(err)
			}
			tokens = append(tokens, h1.Token())
		}
		d.expire(t, "stego.controller")
		h2 := acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		tokens = append(tokens, h2.Token())
		for i, token := range tokens {
			if token != n+int64(i)+1 {
				t.Fatalf("the tokens are %v, want %d, %d, %d and %d", tokens, n+1, n+2, n+3, n+4)
			}
		}
	})

	t.Run("ST-SUP-RP09-LC02-T03 Another holder cannot acquire an unexpired lease", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		h2, err := newClient(t, d.db, "stego.controller", "h2", Settings{}).Acquire(ctx)
		if !errors.Is(err, ErrTaken) || h2 != nil {
			t.Fatalf("h2 got %v, %v, want no token and ErrTaken", h2, err)
		}
		holder, token, _ := d.leaseRow(t, "stego.controller")
		if holder != "h1" || token != h1.Token() {
			t.Fatalf("the row holds %s with token %d, want h1 with %d", holder, token, h1.Token())
		}
	})

	t.Run("ST-SUP-RP09-LC02-T06 A renew after a takeover and a release takes nothing", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, newClient(t, d.db, "stego.controller", "a", Settings{}))
		d.expire(t, "stego.controller")
		b := acquire(t, newClient(t, d.db, "stego.controller", "b", Settings{}))
		if err := b.Release(ctx); err != nil {
			t.Fatal(err)
		}
		holder, token, expires := d.leaseRow(t, "stego.controller")
		if err := a.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew of A gave %v, want ErrLost and no token", err)
		}
		if h, k, e := d.leaseRow(t, "stego.controller"); h != holder || k != token || !e.Equal(expires) || e.After(time.Now()) {
			t.Fatalf("after the renew the row is %s, %d, %s, want B's release left as it is: %s, %d, %s", h, k, e, holder, token, expires)
		}
		if _, err := newClient(t, d.db, "stego.controller", "c", Settings{}).Acquire(ctx); err != nil {
			t.Fatalf("C could not acquire the lease at once: %v", err)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T07 A renew after the lease expired takes nothing", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, newClient(t, d.db, "stego.controller", "a", Settings{}))
		d.expire(t, "stego.controller")
		holder, token, expires := d.leaseRow(t, "stego.controller")
		if err := a.Renew(ctx); !errors.Is(err, ErrLost) {
			t.Fatalf("the renew after the expiry gave %v, want ErrLost and no token", err)
		}
		if h, k, e := d.leaseRow(t, "stego.controller"); h != holder || k != token || !e.Equal(expires) {
			t.Fatalf("after the renew the row is %s, %d, %s, want it unchanged: %s, %d, %s", h, k, e, holder, token, expires)
		}
	})

	t.Run("ST-SUP-RP09-LC02-T04 The lease defaults are 15 s, 10 s and 2 s", func(t *testing.T) {
		c := newClient(t, noDB{}, "stego.controller", "h1", Settings{})
		if c.TTL() != 15*time.Second || c.RenewEvery() != 10*time.Second || c.RetryEvery() != 2*time.Second {
			t.Fatalf("the defaults are %s, %s and %s, want 15s, 10s and 2s", c.TTL(), c.RenewEvery(), c.RetryEvery())
		}
	})

	t.Run("ST-SUP-RP09-LC02-T05 The lease uses no session-scoped advisory lock", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		// The locks of this test's database only: a test of another package on the same server may hold its own.
		var locks int
		if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())").Scan(&locks); err != nil {
			t.Fatal(err)
		}
		if locks != 0 {
			t.Fatalf("pg_locks holds %d advisory locks between two renews, want none", locks)
		}
		if err := h.Renew(ctx); err != nil {
			t.Fatal(err)
		}
		sources, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			if strings.HasSuffix(source, "_test.go") {
				continue
			}
			text, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			for _, function := range []string{"pg_advisory_lock", "pg_advisory_lock_shared", "pg_try_advisory_lock", "pg_try_advisory_lock_shared"} {
				if strings.Contains(string(text), function) {
					t.Errorf("%s calls %s", source, function)
				}
			}
		}
	})
}

func TestWriteCarriesToken(t *testing.T) {
	writes := "CREATE TABLE writes (v text NOT NULL, token bigint NOT NULL)"
	insert := func(v string) func(context.Context, *sql.Tx, int64) error {
		return func(ctx context.Context, tx *sql.Tx, token int64) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO writes (v, token) VALUES ($1, $2)", v, token)
			return err
		}
	}
	count := func(t *testing.T, d *database) int {
		t.Helper()
		var n int
		if err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM writes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("ST-SUP-RP09-LC03-T01 A write under the lease carries the holder's token", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		d.rec.reset()
		if err := h.WithToken(ctx, insert("one")); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, s := range d.rec.all() {
			if strings.HasPrefix(s.sql, "INSERT INTO writes") {
				found = len(s.args) == 2 && s.args[1] == h.Token()
			}
		}
		if !found {
			t.Fatalf("no write bound the holder's token %d: %v", h.Token(), d.rec.all())
		}
		var token int64
		if err := d.other.QueryRowContext(ctx, "SELECT token FROM writes WHERE v='one'").Scan(&token); err != nil || token != h.Token() {
			t.Fatalf("the write holds token %d (%v), want %d", token, err, h.Token())
		}
	})

	t.Run("ST-SUP-RP09-LC03-T02 A write fails with the fenced error when the lease row holds a higher token", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h1 := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		d.expire(t, "stego.controller")
		acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		err := h1.WithToken(ctx, insert("stale"))
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("the write of h1 gave %v, want ErrFenced", err)
		}
		if n := count(t, d); n != 0 {
			t.Fatalf("%d writes applied, want none", n)
		}
	})

	t.Run("ST-SUP-RP09-LC03-T05 A write under the lease reads the holder's state while a renew waits for the row", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		h := acquire(t, newClient(t, d.db, "stego.controller", "h1", Settings{}))
		before := h.Token()
		inWrite := make(chan struct{})
		renewWaits := make(chan struct{})
		renewed := make(chan error, 1)
		var reads []time.Duration
		var nested int64
		var nestedErr error
		go func() {
			<-inWrite
			go func() { renewed <- h.Renew(ctx) }()
			// The renew's UPDATE waits for the lease row that the write holds FOR SHARE.
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var waiting int
				if err := d.other.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'").Scan(&waiting); err != nil || waiting > 0 {
					break
				}
			}
			close(renewWaits)
		}()
		err := h.WithToken(ctx, func(ctx context.Context, tx *sql.Tx, token int64) error {
			close(inWrite)
			<-renewWaits
			start := time.Now()
			_ = h.Token()
			reads = append(reads, time.Since(start))
			start = time.Now()
			_ = h.Expires()
			reads = append(reads, time.Since(start))
			start = time.Now()
			nested, nestedErr = h.Fence(ctx, tx)
			reads = append(reads, time.Since(start))
			return insert("during the renew")(ctx, tx, token)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := within(t, renewed, "the renew"); err != nil {
			t.Fatalf("the renew gave %v", err)
		}
		for i, took := range reads {
			if took > time.Second {
				t.Fatalf("read %d in the write waited %s for the renew, want no wait", i, took)
			}
		}
		if nestedErr != nil || nested != before {
			t.Fatalf("the nested fence gave %d, %v, want the token %d", nested, nestedErr, before)
		}
		if h.Token() != before+1 {
			t.Fatalf("after the renew the token is %d, want %d", h.Token(), before+1)
		}
	})

	t.Run("ST-SUP-RP09-LC03-T03 The holder's loop stops on the fenced error", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, writes)
		ctx := context.Background()
		if err := Declare(ctx, d.db, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		clock := newFakeClock()
		c := newClient(t, d.db, "stego.controller", "h1", Settings{Clock: clock})
		first := make(chan struct{})
		takenOver := make(chan struct{})
		var after []error
		result := make(chan error, 1)
		go func() {
			result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
				if err := h.WithToken(ctx, insert("first")); err != nil {
					return err
				}
				close(first)
				<-takenOver
				for range 3 {
					after = append(after, h.WithToken(ctx, insert("after")))
				}
				return nil
			})
		}()
		within(t, first, "the first write")
		d.expire(t, "stego.controller")
		acquire(t, newClient(t, d.db, "stego.controller", "h2", Settings{}))
		close(takenOver)
		select {
		case err := <-result:
			if !errors.Is(err, ErrFenced) {
				t.Fatalf("the loop returned %v, want ErrFenced", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the loop did not stop")
		}
		if !errors.Is(after[0], ErrFenced) || !errors.Is(after[1], ErrStopped) || !errors.Is(after[2], ErrStopped) {
			t.Fatalf("the writes after the takeover gave %v, want ErrFenced and then ErrStopped", after)
		}
		if n := count(t, d); n != 1 {
			t.Fatalf("%d writes applied, want only the first", n)
		}
	})
}

// holdUntilCancel holds the lease with a fake clock and a work function that waits for its context. It returns the
// fake clock, the held lease, the channel of the clock time at the cancel and the result of Hold.
func holdUntilCancel(t *testing.T, d *database, onCancel func(h *Held)) (*fakeClock, *Held, chan time.Time, chan error) {
	t.Helper()
	ctx := context.Background()
	if err := Declare(ctx, d.db, "stego.controller"); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	c := newClient(t, d.db, "stego.controller", "h1", Settings{Clock: clock})
	held := make(chan *Held, 1)
	cancelled := make(chan time.Time, 1)
	result := make(chan error, 1)
	go func() {
		result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
			held <- h
			<-ctx.Done()
			cancelled <- clock.Now()
			if onCancel != nil {
				onCancel(h)
			}
			return ctx.Err()
		})
	}()
	h := within(t, held, "the held lease")
	clock.waitWaiters(t, 1)
	return clock, h, cancelled, result
}

func TestHolderStops(t *testing.T) {
	t.Run("ST-SUP-RP09-LC04-T01 A holder whose renew fails stops within one renew interval and before the lease expires", func(t *testing.T) {
		d := openDatabase(t)
		clock, h, cancelled, result := holdUntilCancel(t, d, nil)
		_, _, expires := d.leaseRow(t, "stego.controller")
		d.exec(t, "ALTER TABLE leases RENAME TO leases_refused")
		failure := clock.Now().Add(h.RenewEvery())
		clock.Advance(h.RenewEvery())
		select {
		case at := <-cancelled:
			if at.Sub(failure) > h.RenewEvery() {
				t.Fatalf("the work was cancelled %s after the failed renew, more than one renew interval", at.Sub(failure))
			}
			if !at.Before(expires) {
				t.Fatalf("the work was cancelled at %s, not before expires_at %s", at, expires)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the work was not cancelled")
		}
		if err := within(t, result, "the end of Hold"); !errors.Is(err, ErrLost) {
			t.Fatalf("Hold returned %v, want ErrLost", err)
		}
	})

	t.Run("ST-SUP-RP09-LC04-T02 A holder whose renew returns another token stops", func(t *testing.T) {
		d := openDatabase(t)
		clock, h, cancelled, result := holdUntilCancel(t, d, nil)
		_, _, expires := d.leaseRow(t, "stego.controller")
		d.exec(t, "UPDATE leases SET token=token+5 WHERE name='stego.controller'")
		renewAt := clock.Now().Add(h.RenewEvery())
		clock.Advance(h.RenewEvery())
		select {
		case at := <-cancelled:
			if at.Sub(renewAt) > h.RenewEvery() || !at.Before(expires) {
				t.Fatalf("the work was cancelled at %s, want within one renew interval of %s and before %s", at, renewAt, expires)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the work was not cancelled")
		}
		if err := within(t, result, "the end of Hold"); !errors.Is(err, ErrLost) {
			t.Fatalf("Hold returned %v, want ErrLost", err)
		}
	})

	t.Run("ST-SUP-RP09-LC04-T03 A holder does not act on the lease after it stopped", func(t *testing.T) {
		d := openDatabase(t)
		d.exec(t, "CREATE TABLE writes (v text NOT NULL, token bigint NOT NULL)")
		late := make(chan error, 1)
		clock, h, _, result := holdUntilCancel(t, d, func(h *Held) {
			late <- h.WithToken(context.Background(), func(ctx context.Context, tx *sql.Tx, token int64) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO writes (v, token) VALUES ('late', $1)", token)
				return err
			})
		})
		d.exec(t, "UPDATE leases SET token=token+5 WHERE name='stego.controller'")
		clock.Advance(h.RenewEvery())
		if err := within(t, late, "the write after the stop"); !errors.Is(err, ErrStopped) {
			t.Fatalf("the write after the stop gave %v, want ErrStopped", err)
		}
		within(t, result, "the end of Hold")
		var n int
		if err := d.other.QueryRowContext(context.Background(), "SELECT count(*) FROM writes").Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d writes applied (%v), want none", n, err)
		}
	})
}

// TestStoppedHolder runs the holder in a process of its own and stops that process, as a frozen container, a
// paused virtual machine or a long stall of the process does: no timer of the process runs, so only the database
// can end the transaction that the process left open.
func TestStoppedHolder(t *testing.T) {
	t.Run("ST-SUP-RP09-LC04-T04 A holder that stops inside a write under the lease keeps the lease row no longer than its lease", func(t *testing.T) {
		d := openDatabase(t)
		ctx := context.Background()
		// The bound is the time the lease has left, never longer and never 0, which turns the timeout off.
		if err := Declare(ctx, d.other, "stego.bound"); err != nil {
			t.Fatal(err)
		}
		clock := newFakeClock()
		conn, err := d.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		bounded := acquire(t, newClient(t, conn, "stego.bound", "bounded", Settings{TTL: stallTTL, Renew: stallTTL / 2, Clock: clock}))
		clock.Advance(500*time.Millisecond + 300*time.Microsecond)
		var timeout string
		if err := bounded.WithToken(ctx, func(ctx context.Context, tx *sql.Tx, _ int64) error {
			return tx.QueryRowContext(ctx, "SELECT current_setting('idle_in_transaction_session_timeout')").Scan(&timeout)
		}); err != nil {
			t.Fatal(err)
		}
		if want := "1499ms"; timeout != want {
			t.Fatalf("the idle-in-transaction timeout of a write with 1499.7 ms of lease left is %s, want %s", timeout, want)
		}
		if err := conn.QueryRowContext(ctx, "SELECT current_setting('idle_in_transaction_session_timeout')").Scan(&timeout); err != nil || timeout != "0" {
			t.Fatalf("after the write the session keeps the idle-in-transaction timeout %s (%v), want 0: the timeout is the transaction's only", timeout, err)
		}
		clock.Advance(stallTTL - 500*time.Millisecond - 300*time.Microsecond - 500*time.Microsecond)
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := bounded.Fence(ctx, tx); err == nil || !strings.Contains(err.Error(), "no time left") {
			t.Fatalf("Fence with 0.5 ms of lease left gave %v, want the error of no time left", err)
		}
		var locks int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pg_catalog.pg_locks WHERE pid = pg_catalog.pg_backend_pid() AND locktype = 'transactionid'").Scan(&locks); err != nil || locks != 0 {
			t.Fatalf("Fence with no time left took %d row locks (%v), want none", locks, err)
		}
		_ = tx.Rollback()
		if err := Declare(ctx, d.other, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		holder := exec.Command(os.Args[0], "-test.run=^$")
		holder.Env = append(os.Environ(), stallVariable+"="+d.dsn)
		holder.Stderr = os.Stderr
		out, err := holder.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = holder.Process.Signal(syscall.SIGCONT)
			_ = holder.Process.Kill()
			_ = holder.Wait()
		})
		fenced := make(chan bool, 1)
		go func() {
			line, _ := bufio.NewReader(out).ReadString('\n')
			fenced <- line == "fenced\n"
		}()
		if !within(t, fenced, "the write of the holder under the lease") {
			t.Fatal("the holder ended before its write held the lease row")
		}
		// The process stops with its transaction open and the lease row locked FOR SHARE.
		if err := holder.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		holderName, _, expires := d.leaseRow(t, "stego.controller")
		if holderName != "stalled" {
			t.Fatalf("the lease row holds %q, want the stopped holder", holderName)
		}
		// The other holder takes the lease once it expires, and not one lease later.
		bound := expires.Add(stallTTL)
		other := newClient(t, d.other, "stego.controller", "other", Settings{TTL: stallTTL, Renew: stallTTL / 2})
		acquireCtx, cancel := context.WithDeadline(ctx, bound)
		defer cancel()
		for {
			_, err := other.Acquire(acquireCtx)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrTaken) || acquireCtx.Err() != nil {
				t.Fatalf("the other holder did not take the lease within one lease after it expired at %s, while the stopped holder's transaction stays open: %v", expires.Format(time.RFC3339Nano), err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if h, _, _ := d.leaseRow(t, "stego.controller"); h != "other" {
			t.Fatalf("the lease row holds %q after the takeover, want other", h)
		}
	})
	t.Run("ST-SUP-RP09-LC04-T05 A holder that runs a statement under the lease and then stops keeps the lease row no longer than its lease", func(t *testing.T) {
		// Review of PR #309, L1: the idle timeout counts only idle time, so a holder that worked 1.5 s in the
		// transaction and then stopped kept the row 1.5 s past the expiry. transaction_timeout counts the statement too.
		var version int
		if err := openDatabase(t).other.QueryRow("SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version < 170000 {
			t.Skipf("PostgreSQL %d has no transaction_timeout", version)
		}
		// The bound is the transaction's only: a pooled session keeps no transaction_timeout after the write.
		{
			d := openDatabase(t)
			ctx := context.Background()
			if err := Declare(ctx, d.other, "stego.local"); err != nil {
				t.Fatal(err)
			}
			conn, err := d.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			local := acquire(t, newClient(t, conn, "stego.local", "local", Settings{TTL: 2 * time.Second, Renew: time.Second}))
			var inside, after string
			if err := local.WithToken(ctx, func(ctx context.Context, tx *sql.Tx, _ int64) error {
				return tx.QueryRowContext(ctx, "SELECT current_setting('transaction_timeout')").Scan(&inside)
			}); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRowContext(ctx, "SELECT current_setting('transaction_timeout')").Scan(&after); err != nil || inside == "0" || after != "0" {
				t.Fatalf("the transaction timeout is %s in the write and %s after it (%v), want a bound in the write and 0 after it", inside, after, err)
			}
		}
		// The bound of the transaction is the time left on the process clock, and the server ends the transaction at
		// that bound, whether it runs a statement or waits. The process clock is a hand clock, so the bound is exact.
		for _, work := range []string{"0", "1.5", "4"} {
			t.Run("work "+work+" s then stop", func(t *testing.T) {
				d := openDatabase(t)
				ctx := context.Background()
				if err := Declare(ctx, d.other, "stego.stop"); err != nil {
					t.Fatal(err)
				}
				clock := newFakeClock()
				a := acquire(t, newClient(t, d.db, "stego.stop", "a", Settings{TTL: 2 * time.Second, Renew: time.Second, Clock: clock}))
				// 300 ms pass on the process clock: 1700 ms of the lease are left.
				clock.Advance(300 * time.Millisecond)
				tx, err := d.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := a.Fence(ctx, tx); err != nil {
					t.Fatal(err)
				}
				var statement, idle bool
				if err := tx.QueryRowContext(ctx, "SELECT current_setting('transaction_timeout')::interval = interval '1700 milliseconds', current_setting('idle_in_transaction_session_timeout')::interval = interval '1700 milliseconds'").Scan(&statement, &idle); err != nil || !statement || !idle {
					t.Fatalf("the transaction timeout is the time left %v and the idle timeout %v (%v), want both 1700 ms", statement, idle, err)
				}
				// The work: a statement of 4 s runs past the bound, and the server ends it.
				_, _ = tx.ExecContext(ctx, "SELECT pg_catalog.pg_sleep("+work+")")
				// The process "stops": no statement and no timer of the holder. The row is expired as the database
				// clock would make it; the update waits on the row lock of Fence until the server ends the transaction.
				d.expire(t, "stego.stop")
				b := newClient(t, d.other, "stego.stop", "b", Settings{TTL: 2 * time.Second, Renew: time.Second})
				if _, err := b.Acquire(ctx); err != nil {
					t.Fatalf("the other holder did not take the expired lease: %v", err)
				}
				if _, err := tx.ExecContext(ctx, "SELECT 1"); err == nil {
					t.Fatal("the transaction of the stopped holder still runs after the other holder took the lease")
				}
			})
		}
	})
}

// armClock is a process clock that stands still and gives the test each timer that the client arms, so the test
// fires a timer by name and knows when the client waits on it.
type armClock struct {
	now   time.Time
	armed chan chan time.Time
}

func newArmClock() *armClock { return &armClock{now: time.Now(), armed: make(chan chan time.Time, 8)} }

func (c *armClock) Now() time.Time { return c.now }

func (c *armClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.armed <- ch
	return ch
}

// renewGate is a DB that stops each Renew statement before it goes to the database: it tells the test on sent,
// waits until the test closes proceed, sends the statement with the context of the renew, and tells the test on
// returned when the database answered.
type renewGate struct {
	DB
	sent     chan struct{}
	proceed  chan struct{}
	returned chan struct{}
	// detach sends the statement with a context that the end of the context of the renew does not end, so the
	// database gives its own answer.
	detach bool
}

func (g *renewGate) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query != renewStatement {
		return g.DB.QueryRowContext(ctx, query, args...)
	}
	g.sent <- struct{}{}
	<-g.proceed
	if g.detach {
		ctx = context.WithoutCancel(ctx)
	}
	row := g.DB.QueryRowContext(ctx, query, args...)
	close(g.returned)
	return row
}

// TestHoldReleaseAfterRenew is the race of the review of PR #387, L2: the renew timer of Hold wins the select against
// the end of ctx, or ctx ends while a renew runs. The renew then fails because ctx ended, and Hold must release the
// lease as it does when ctx ends first, so the next holder does not wait for the lease to expire.
func TestHoldReleaseAfterRenew(t *testing.T) {
	t.Run("ST-SUP-RP09-LC06-T05 A holder whose context ends before or while a renew runs releases the lease", func(t *testing.T) {
		for _, during := range []bool{false, true} {
			name := "ctx ends after the renew timer fired"
			if during {
				name = "ctx ends while the renew waits on the lease row"
			}
			t.Run(name, func(t *testing.T) { holdReleaseAfterRenew(t, during) })
		}
	})
}

func holdReleaseAfterRenew(t *testing.T, during bool) {
	d := openDatabase(t)
	if err := Declare(context.Background(), d.other, "stego.controller"); err != nil {
		t.Fatal(err)
	}
	gate := &renewGate{DB: d.db, sent: make(chan struct{}), proceed: make(chan struct{}), returned: make(chan struct{})}
	clock := newArmClock()
	c := newClient(t, gate, "stego.controller", "h1", Settings{Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan *Held, 1)
	result := make(chan error, 1)
	go func() {
		result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
			held <- h
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	h := <-held
	token := h.Token()
	// The renew timer fires while ctx has not ended, so Hold takes the renew branch of its select.
	(<-clock.armed) <- clock.Now()
	<-gate.sent
	var lock *sql.Tx
	if during {
		// A second session locks the lease row, so the renew waits on it in the database until ctx ends.
		var err error
		if lock, err = d.other.BeginTx(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback() }()
		if _, err := lock.Exec("SELECT 1 FROM leases WHERE name='stego.controller' FOR UPDATE"); err != nil {
			t.Fatal(err)
		}
		close(gate.proceed)
		// ctx ends only when the renew statement waits on the lock in the database. If ctx ends first, the renew
		// does not go to the database, and the test is the case of a renew after the end of ctx.
		waitLocked(t, d, renewStatement)
		cancel()
		// The renew answers with the end of ctx; only then does the lock go. The client does not cancel the
		// statement on the server, so the UPDATE of the renew can commit when the lock goes and give the row the
		// next token, which the holder does not know (ST-SUP-RP09-LC06-T06). The release must also make that
		// lease expire.
		<-gate.returned
		if err := lock.Rollback(); err != nil {
			t.Fatal(err)
		}
	} else {
		// The end of ctx comes before the renew statement goes to the database.
		cancel()
		close(gate.proceed)
	}
	err := <-result
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrLost) {
		t.Fatalf("Hold returned %v, want the end of ctx and not a lost lease, as when ctx ends first", err)
	}
	if h.Token() != token {
		t.Fatalf("the holder has token %d, want %d: the renew gave no answer", h.Token(), token)
	}
	// leaseRow gives the epoch for -infinity. In the case during, the renew can have committed on the server after
	// its client stopped, so the row can hold the next token; the lease must be released with either token.
	holder, after, expires := d.leaseRow(t, "stego.controller")
	if holder != "h1" || (after != token && !(during && after == token+1)) || !expires.Equal(time.Unix(0, 0)) {
		t.Fatalf("after Hold the row has holder %s, token %d, expires_at %v; want h1, %d (or %d in the case during) and released (-infinity)", holder, after, expires, token, token+1)
	}
	if _, err := newClient(t, d.other, "stego.controller", "h2", Settings{}).Acquire(context.Background()); err != nil {
		t.Fatalf("the next holder cannot take the lease at once: %v", err)
	}
}

// waitLocked reads pg_stat_activity again and again until a session of the test database runs the statement query
// and waits on a lock. It does not sleep and has no deadline: each read is one round trip to the database.
func waitLocked(t *testing.T, d *database, query string) {
	t.Helper()
	for {
		var waiting bool
		err := d.other.QueryRowContext(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query=$1)", query).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
	}
}

// committedRenewGate is a DB whose Renew statement commits on the server and whose answer the end of the context
// of the renew cuts off: it sends the statement with a context that does not end, tells the test on committed,
// waits for the end of the context of the renew, and then gives the renew the error of that context, as a client
// whose reply did not come before its context ended.
type committedRenewGate struct {
	DB
	committed chan struct{}
}

func (g *committedRenewGate) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query != renewStatement {
		return g.DB.QueryRowContext(ctx, query, args...)
	}
	if _, err := g.DB.ExecContext(context.WithoutCancel(ctx), query, args...); err != nil {
		panic(err)
	}
	close(g.committed)
	<-ctx.Done()
	return g.DB.QueryRowContext(ctx, query, args...)
}

// holdOnce starts Hold on c with a work that waits for its context, and gives the held lease and the result of Hold.
func holdOnce(ctx context.Context, c *Client) (*Held, chan error) {
	held := make(chan *Held, 1)
	result := make(chan error, 1)
	go func() {
		result <- c.Hold(ctx, func(ctx context.Context, h *Held) error {
			held <- h
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	return <-held, result
}

// TestHoldReleaseAfterCommittedRenew is the case of the review of PR #389, L1: the UPDATE of a renew commits on the
// server, so the row holds the next token, but the end of ctx cuts off the answer, so the holder keeps the token
// before it. The release must also try the next token, or it matches no row and the next holder waits for the lease
// to expire.
func TestHoldReleaseAfterCommittedRenew(t *testing.T) {
	t.Run("ST-SUP-RP09-LC06-T06 A holder whose renew commits on the server but whose context ends before the answer releases the lease", func(t *testing.T) {
		d := openDatabase(t)
		if err := Declare(context.Background(), d.other, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		gate := &committedRenewGate{DB: d.db, committed: make(chan struct{})}
		clock := newArmClock()
		c := newClient(t, gate, "stego.controller", "h1", Settings{Clock: clock})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h, result := holdOnce(ctx, c)
		token := h.Token()
		(<-clock.armed) <- clock.Now()
		<-gate.committed
		if holder, now, _ := d.leaseRow(t, "stego.controller"); holder != "h1" || now != token+1 {
			t.Fatalf("after the renew committed the row has holder %s and token %d; want h1 and %d", holder, now, token+1)
		}
		cancel()
		err := <-result
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrLost) {
			t.Fatalf("Hold returned %v, want the end of ctx and not a lost lease", err)
		}
		if h.Token() != token {
			t.Fatalf("the holder has token %d, want %d: the renew gave no answer", h.Token(), token)
		}
		holder, after, expires := d.leaseRow(t, "stego.controller")
		if holder != "h1" || after != token+1 || !expires.Equal(time.Unix(0, 0)) {
			t.Fatalf("after Hold the row has holder %s, token %d, expires_at %v; want h1, %d and released (-infinity)", holder, after, expires, token+1)
		}
		if _, err := newClient(t, d.other, "stego.controller", "h2", Settings{}).Acquire(context.Background()); err != nil {
			t.Fatalf("the next holder cannot take the lease at once: %v", err)
		}
	})
}

// TestHoldLostWithContextEnd is the case of the review of PR #389, L2: a renew finds the lease taken while ctx ends.
// Hold must return ErrLost with the end of ctx, so a caller that classifies by ErrLost sees the lost lease.
func TestHoldLostWithContextEnd(t *testing.T) {
	t.Run("ST-SUP-RP09-LC04-T06 A holder whose renew finds the lease lost while its context ends returns the lost lease with the end of the context", func(t *testing.T) {
		d := openDatabase(t)
		if err := Declare(context.Background(), d.other, "stego.controller"); err != nil {
			t.Fatal(err)
		}
		gate := &renewGate{DB: d.db, sent: make(chan struct{}), proceed: make(chan struct{}), returned: make(chan struct{}), detach: true}
		clock := newArmClock()
		c := newClient(t, gate, "stego.controller", "h1", Settings{Clock: clock})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, result := holdOnce(ctx, c)
		(<-clock.armed) <- clock.Now()
		<-gate.sent
		// Another holder takes the lease before the renew goes to the database, and ctx ends at the same time.
		d.exec(t, "UPDATE leases SET holder='h2', token=token+1, expires_at=now()+interval '1 hour' WHERE name='stego.controller'")
		_, taken, _ := d.leaseRow(t, "stego.controller")
		cancel()
		close(gate.proceed)
		err := <-result
		if !errors.Is(err, ErrLost) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Hold returned %v, want the lost lease and the end of ctx", err)
		}
		holder, after, expires := d.leaseRow(t, "stego.controller")
		if holder != "h2" || after != taken || !expires.After(time.Now()) {
			t.Fatalf("after Hold the row has holder %s, token %d, expires_at %v; want the lease of h2 with token %d as it was", holder, after, expires, taken)
		}
	})
}

// within receives from ch, and fails the test when nothing comes within 10 seconds.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not come within 10 s", what)
		panic("unreachable")
	}
}
