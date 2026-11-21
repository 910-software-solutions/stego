package command

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The limits of wrong attempts (ST-SUP-RP10-LC54, decision ST-SUP-Q185 (a), security review of PR #451, H1): a
// compare-only field with a limit (ST-DECL-INV104) is read into CountedAt, which binds each stored value that the
// handle reads to the counter of its row. Matches then charges one check before it verifies the value: the charge is
// an update of the row of the field and the row ID in the table attempt_counts, in a short transaction of its own that
// commits before the verify. The table is not the table of the entity, so the charge never waits for the lock that the
// command holds on the entity row, and parallel checks of one row each see the charges before them. At the limit no
// row comes back and no check runs. A charge that cannot commit fails the check and makes the command answer 503. A
// check that matches, and a write of a new value of the field, reset the count in the transaction of the command, so
// the reset commits only with the command.
//
// The charge runs on the pool of the charges (Config.ChargeDB), apart from the main pool, so a full main pool never
// makes a check wait for a second main connection (security review of PR #451, L2). A rerun of the call, after a
// conflict, a stale plan or a seal switch, checks the same request again: it charges a row only for the checks
// beyond those that an earlier attempt of the call charged on that row (security review of PR #451, second review,
// I1), for the same stored value only: a new value that a reset wrote between two attempts gets its own charges
// (security review of PR #479, L2). A charge waits at most chargeLockTimeout for the row of its count (security
// review of PR #479, L3). The charges of one tenant hold at most half of the connections of that pool at one time, so
// the checks of one tenant cannot fill the pool that all tenants share (security review of PR #481, L1). A stored
// counted value gives its hash only to this runtime, through storeKey, so a fill cannot check it against the hash with
// no charge (security review of PR #451, L1), and the runtime writes it again only into the field and the row of its
// count, in the attempt that read it, so a fill cannot copy it to a field with no limit or to a row with a new count
// (security review of PR #479, L1).

// maxAttempts is the highest limit of a counter (ST-DECL-INV104).
const maxAttempts = 100

// chargeBound bounds the short transaction of one charge.
const chargeBound = 5 * time.Second

// storeKey is the store key of the command runtime: the counted values that the handle reads give their stored hash
// only to the statements of this package (sealed.StoredBy).
var storeKey = sealed.NewStoreKey()

// chargeLockTimeout is the lock timeout of the transaction of one charge: a charge that waits longer for the row of
// its count, such as behind the reset of a command that has not committed, fails, and the call answers 503. Thus a
// flood of checks on one row cannot hold the small pool of the charges, which all tenants share (security review of
// PR #479, L3).
const chargeLockTimeout = "1s"

// setChargeLockTimeout sets the lock timeout of the transaction of a charge.
const setChargeLockTimeout = `SELECT pg_catalog.set_config('lock_timeout', $1, true)`

// saturationLogEvery is the shortest time between two logs that the pool of the charges is full.
const saturationLogEvery = 10 * time.Second

// chargeKey names the checks of one stored value of one row: the row of the counter and a digest of the stored hash
// that the read gave. A check that an earlier attempt of the call paid is free only for the same stored value, so a
// new PIN that a reset wrote between two attempts gets its own charges (security review of PR #479, L2).
type chargeKey struct {
	attemptKey
	stored [sha256.Size]byte
}

// charges are the checks that the attempts of one call charged, by stored value: an attempt charges a value only for
// its checks beyond the most that an earlier attempt of the call charged on that value. Run makes one for each call.
type charges struct {
	mu   sync.Mutex
	paid map[chargeKey]int
}

// take tells whether the n-th check of key in an attempt is paid by an earlier attempt of the call.
func (c *charges) take(key chargeKey, n int) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return n <= c.paid[key]
}

// pay records that an attempt of the call charged n checks of key.
func (c *charges) pay(key chargeKey, n int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paid == nil {
		c.paid = map[chargeKey]int{}
	}
	c.paid[key] = max(c.paid[key], n)
}

// The statements of the counts in the table idempotency.AttemptsTable, which the migration job makes beside the key
// table. Each takes the tenant as $1, the counter as $2 and the row ID as $3; the charge takes the limit as $4 and
// gives no row at the limit.
const (
	chargeStatement = `INSERT INTO public.attempt_counts AS a (tenant_id, counter, row_id, checks) VALUES ($1, $2, $3, 1)
ON CONFLICT (tenant_id, counter, row_id) DO UPDATE SET checks = a.checks + 1 WHERE a.checks < $4 RETURNING a.checks`
	resetStatement = `DELETE FROM public.attempt_counts WHERE tenant_id = $1 AND counter = $2 AND row_id = $3`
)

// CounterSpec is the generated declaration of the limit of wrong attempts of one compare-only field
// (ST-SUP-RC32-OC24): the entity, the field and the limit.
type CounterSpec struct {
	Entity, Field string
	Limit         int
}

// Counter is one checked counter declaration.
type Counter struct{ spec CounterSpec }

// NewCounter checks the declaration of a counter: its names and a limit from 1 to 100.
func NewCounter(spec CounterSpec) (*Counter, error) {
	if spec.Entity == "" || spec.Field == "" || strings.Contains(spec.Entity+spec.Field, ".") {
		return nil, errors.New("command: a counter needs its entity and its field, with no period in them")
	}
	if spec.Limit < 1 || spec.Limit > maxAttempts {
		return nil, fmt.Errorf("command: the counter of %s.%s has the limit %d; give 1 to %d", spec.Entity, spec.Field, spec.Limit, maxAttempts)
	}
	return &Counter{spec: spec}, nil
}

// name is the counter as the table of the counts holds it.
func (c *Counter) name() string { return c.spec.Entity + "." + c.spec.Field }

// countedTarget is the scan target of a counted compare-only field: its stored hash, which the handle binds to the
// counter of its row after the scan.
type countedTarget struct {
	counter *Counter
	row     *string
	into    *sealed.Compared
}

// Scan keeps the stored hash, as the target of a compare-only field does.
func (t *countedTarget) Scan(src any) error { return comparedTarget{into: t.into}.Scan(src) }

// CountedAt gives the scan target of a compare-only field with the counter c: the row ID that row holds after the
// scan of the row binds the value to its counter (ST-SUP-RP10-LC54).
func CountedAt(c *Counter, row *string, into *sealed.Compared) any {
	return &countedTarget{counter: c, row: row, into: into}
}

// countedHash is the statement value of a counted compare-only field: its hash, and a reset of the count of its row
// when the value is a new text (security review of PR #451, L2).
type countedHash struct {
	hashArg
	counter *Counter
	row     string
}

// HashCountedOf gives the statement value of a compare-only field with the counter c in the row of ID row: the
// runtime stores its hash as HashOf does, and resets the count of the row when the value is a new text.
func HashCountedOf(c *Counter, row string, value sealed.Compared) any {
	return countedHash{hashArg: hashArg{value: value}, counter: c, row: strings.ToLower(row)}
}

// attemptKey names the count of one row of one counter.
type attemptKey struct {
	counter *Counter
	row     string
}

// attemptRow is what the checks and the writes of one call did to the count of one row: whether a check matched, or
// a write stored a new value, which resets the count when the call commits.
type attemptRow struct{ reset bool }

// rowCounter is the counter of one row as one read of the handle bound it, with the digest of the stored hash that
// the read gave.
type rowCounter struct {
	s      *state
	key    attemptKey
	stored [sha256.Size]byte
}

// Charge takes one check of the row in a short transaction of its own, which commits before the check runs: false in
// a worker, after the call ended, at the limit, and when the charge cannot commit, which also makes the call answer
// 503 (ST-SUP-RP10-LC54). A check that an earlier attempt of the call charged runs with no new charge.
func (k *rowCounter) Charge() bool {
	s := k.s
	s.mu.Lock()
	open := !s.closed && s.mode == modeCommand
	paid := chargeKey{attemptKey: k.key, stored: k.stored}
	var n int
	if open {
		if s.checks == nil {
			s.checks = map[chargeKey]int{}
		}
		s.checks[paid]++
		n = s.checks[paid]
	}
	s.mu.Unlock()
	if !open {
		return false
	}
	if s.charges.take(paid, n) {
		return true
	}
	charged, err := s.rt.charge(s.tenant, k.key, s.deadline)
	if err != nil {
		s.mu.Lock()
		s.chargeFailed = true
		s.mu.Unlock()
		if s.rt.chargeErr != nil {
			s.rt.chargeErr(err)
		}
		s.rt.logChargeFailure(s.action, k.key.counter.spec.Entity, err)
		return false
	}
	if s.rt.charged != nil {
		s.rt.charged(charged)
	}
	if charged {
		s.charges.pay(paid, n)
	}
	return charged
}

// Record records one check of the row: a match resets the count when the call commits.
func (k *rowCounter) Record(matched bool) {
	if !matched {
		return
	}
	k.s.resetAttempts(k.key)
}

// resetAttempts records that the call resets the count of the row of key when it commits.
func (s *state) resetAttempts(key attemptKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetAttemptsHeld(key)
}

// resetAttemptsHeld is resetAttempts for a caller that holds the lock of the state, such as a method of the handle.
func (s *state) resetAttemptsHeld(key attemptKey) {
	if s.attempts == nil {
		s.attempts = map[attemptKey]*attemptRow{}
	}
	s.attempts[key] = &attemptRow{reset: true}
}

// charge runs the charge of one check of key in the tenant t, in a transaction of its own that it commits, and tells
// whether the count was below the limit.
func (rt *Runtime) charge(t tenant.Tenant, key attemptKey, deadline time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), chargeBound)
	defer cancel()
	if !deadline.IsZero() {
		var cancelDeadline context.CancelFunc
		ctx, cancelDeadline = context.WithDeadline(ctx, deadline)
		defer cancelDeadline()
	}
	db := rt.chargeDB()
	release, err := rt.chargeSlots.take(ctx, t.ID(), perTenantCharges(db))
	if err != nil {
		return false, err
	}
	defer release()
	rt.saturated(db, key)
	tx, err := tenant.Begin(ctx, db, t, nil)
	if err != nil {
		return false, err
	}
	if rt.chargeBegun != nil {
		rt.chargeBegun(t.ID())
	}
	rt.record(setChargeLockTimeout, nil)
	if _, err := tx.ExecContext(ctx, setChargeLockTimeout, chargeLockTimeout); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	rt.record(chargeStatement, nil)
	var checks int
	err = tx.QueryRowContext(ctx, chargeStatement, t.ID(), key.counter.name(), key.row, key.counter.spec.Limit).Scan(&checks)
	charged := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return false, err
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return false, err
	}
	return charged, nil
}

// AttemptsFailedEvent is the event name of the error log line of a charge of a counted check that cannot commit.
const AttemptsFailedEvent = "command.attempts.failed"

// chargeCause gives the fixed cause class of a charge that cannot commit, and never the error text: unavailable for a
// database that cannot serve now (tenant.Unavailable), deadline and canceled for the end of its context, and internal
// for any other error.
func chargeCause(err error) string {
	var coded interface{ SQLState() string }
	switch {
	case tenant.Unavailable(err):
		return "unavailable"
	case errors.As(err, &coded) && coded.SQLState() == "55P03":
		// The charge waited longer than chargeLockTimeout for the row of its count (security review of PR #479, L3).
		return "lock_timeout"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "internal"
}

// logChargeFailure logs the charge of a counted check that cannot commit, so the call answers 503: one error line
// through the log provider of the runtime, in the format of each other log line of the runtime, with the action, the
// entity, the cause class and the count of the lines held back, and no value and no error text (ST-SUP-RP10-LC54;
// walkthrough 20, P-C). It logs one line for each action, entity and cause class in each authz.LogEvery. With no log
// provider, it logs the same fields with log/slog.
func (rt *Runtime) logChargeFailure(action, entity string, err error) {
	cause := chargeCause(err)
	// One line for each action, entity and cause class in each authz.LogEvery, with the count of the lines held back,
	// so a flood of failed charges cannot flood the log (security review of PR #481, I1).
	allowed, held := statementLog.Allow(action + "/attempts_failed/" + entity + "/" + cause)
	if !allowed {
		return
	}
	if rt.logger == nil {
		slog.Error(AttemptsFailedEvent, "action", action, "entity", entity, "cause", cause, "held_back", held)
		return
	}
	var record otellog.Record
	record.SetEventName(AttemptsFailedEvent)
	record.SetSeverity(otellog.SeverityError)
	record.SetSeverityText("ERROR")
	record.SetBody(attribute.StringValue("the charge of a counted check did not commit, so the check fails and the call answers 503"))
	record.AddAttributes(attribute.String("stego.action", action), attribute.String("stego.entity", entity), attribute.String("stego.cause", cause),
		attribute.Int("stego.held_back", held))
	rt.logger.Emit(context.Background(), record)
}

// bindCounted binds the value of a counted target to the counter of its row after the scan of the row.
func (s *state) bindCounted(t *countedTarget) {
	if t.row == nil {
		return
	}
	k := &rowCounter{s: s, key: attemptKey{counter: t.counter, row: strings.ToLower(*t.row)}}
	// The value is not bound yet, so Stored gives its hash.
	if stored, err := t.into.Stored(); err == nil {
		k.stored = sha256.Sum256([]byte(stored))
	}
	*t.into = sealed.CountedByKey(*t.into, k, storeKey)
}

// boundTo tells whether the stored counted value c was bound, by a read of this handle, to the count of key.
func (s *state) boundTo(c sealed.Compared, key attemptKey) bool {
	k, ok := c.CounterBy(storeKey).(*rowCounter)
	return ok && k.s == s && k.key == key
}

// saturated logs, at most once in saturationLogEvery, that each connection of the pool of the charges is in use, so
// the charge of key waits for one: through the log provider of the runtime, or with log/slog when it has none. The
// log names the entity of the counter and the counts of the pool, never a value.
func (rt *Runtime) saturated(db *sql.DB, key attemptKey) {
	st := db.Stats()
	if st.MaxOpenConnections <= 0 || st.InUse < st.MaxOpenConnections {
		return
	}
	now := time.Now().UnixNano()
	last := rt.saturationLogged.Load()
	if last != 0 && now-last < int64(saturationLogEvery) {
		return
	}
	if !rt.saturationLogged.CompareAndSwap(last, now) {
		return
	}
	entity := key.counter.spec.Entity
	if rt.logger == nil {
		slog.Warn(PoolSaturatedEvent, "entity", entity, "in_use", st.InUse, "max_open", st.MaxOpenConnections,
			"wait_count", st.WaitCount)
		return
	}
	var record otellog.Record
	record.SetEventName(PoolSaturatedEvent)
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("WARN")
	record.SetBody(attribute.StringValue("each connection of the pool of the charges is in use, so a charge of a counted check waits"))
	record.AddAttributes(attribute.String("stego.entity", entity), attribute.Int("stego.pool.in_use", st.InUse),
		attribute.Int("stego.pool.max_open", st.MaxOpenConnections), attribute.Int64("stego.pool.wait_count", st.WaitCount))
	rt.logger.Emit(context.Background(), record)
}

// PoolSaturatedEvent is the event name of the warn log line of a full pool of the charges.
const PoolSaturatedEvent = "command.attempts.pool_saturated"

// countAttempts resets, in the transaction of a call that commits, the count of each row whose check matched or whose
// field got a new value (ST-SUP-RP10-LC54).
func (s *state) countAttempts(ctx context.Context) error {
	s.mu.Lock()
	keys := make([]attemptKey, 0, len(s.attempts))
	for key, row := range s.attempts {
		if row.reset {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if a, b := keys[i].counter.name(), keys[j].counter.name(); a != b {
			return a < b
		}
		return keys[i].row < keys[j].row
	})
	for _, key := range keys {
		if _, err := s.exec(ctx, resetStatement, s.tenant.ID(), key.counter.name(), key.row); err != nil {
			return s.fail(ctx, "reset the attempts of "+key.counter.spec.Entity, err)
		}
	}
	return nil
}

// perTenantCharges gives the most charges of one tenant that run at one time on the pool db: half of its
// connections, and at least one, so the charges of one tenant never hold the whole pool, which all tenants share
// (security review of PR #481, L1). A pool with no bound takes defaultTenantCharges.
func perTenantCharges(db *sql.DB) int {
	n := db.Stats().MaxOpenConnections
	if n <= 0 {
		return defaultTenantCharges
	}
	return max(1, n/2)
}

// defaultTenantCharges is the most charges of one tenant at one time on a pool of the charges with no bound.
const defaultTenantCharges = 2

// tenantSlots limits the charges of each tenant that run at one time (security review of PR #481, L1): a charge takes
// a slot of its tenant before it takes a connection of the pool of the charges, and a charge whose tenant has each
// slot in use waits for one of them, never for a connection that another tenant needs. It keeps an entry only for a
// tenant with a charge that runs or waits, so many tenants cannot grow it. Its zero value is ready.
type tenantSlots struct {
	mu      sync.Mutex
	tenants map[string]*tenantSlot
}

// tenantSlot is the slots of one tenant and the count of its charges that run or wait.
type tenantSlot struct {
	sem   chan struct{}
	users int
}

// take takes a slot of the tenant id, of n slots, and gives the function that gives it back. It waits at most until
// ctx ends, and then gives the error of ctx, so the check fails and the call answers 503.
func (s *tenantSlots) take(ctx context.Context, id string, n int) (func(), error) {
	s.mu.Lock()
	if s.tenants == nil {
		s.tenants = map[string]*tenantSlot{}
	}
	slot := s.tenants[id]
	if slot == nil {
		slot = &tenantSlot{sem: make(chan struct{}, n)}
		s.tenants[id] = slot
	}
	slot.users++
	s.mu.Unlock()
	leave := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if slot.users--; slot.users == 0 {
			delete(s.tenants, id)
		}
	}
	select {
	case slot.sem <- struct{}{}:
		return func() { <-slot.sem; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}

// waiting gives the count of the charges of the tenant id that run or wait for a slot.
func (s *tenantSlots) waiting(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.tenants[id]; slot != nil {
		return slot.users
	}
	return 0
}

// chargeDB is the pool of the charges: Config.ChargeDB, or the audit pool when the configuration has none. Both are
// apart from the main pool.
func (rt *Runtime) chargeDB() *sql.DB {
	if rt.cfg.ChargeDB != nil {
		return rt.cfg.ChargeDB
	}
	return rt.cfg.AuditDB
}

// chargeUnavailable gives the error of a call whose charge of a check did not commit: the check did not run, and the
// call answers 503 (ST-SUP-RP10-LC54).
func chargeUnavailable() error {
	return unavailablef("the charge of a check of a counted secret did not commit")
}
