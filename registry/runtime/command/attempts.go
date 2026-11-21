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
	"go.opentelemetry.io/otel/metric"

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
// review of PR #479, L3). The charges of one tenant hold at most half of the connections of that pool at one time,
// and the charges of all the tenants hold at most all but one, which a tenant with no charge in flight can take, so
// the checks of one or two tenants cannot fill the pool that all tenants share (security review of PR #481, L1, and
// of PR #482, L1). A stored
// counted value gives its hash only to this runtime, through storeKey, so a fill cannot check it against the hash with
// no charge (security review of PR #451, L1), and the runtime writes it again only into the field and the row of its
// count, in the attempt that read it, so a fill cannot copy it to a field with no limit or to a row with a new count
// (security review of PR #479, L1).
//
// A count decays with time (decision ST-SUP-Q205 (c)): a charge whose count was last charged UnlockAfter or longer
// before, on the clock of the database, starts the count again at 1, so a row at its limit opens again after the
// declared window, which a client cannot make shorter: the window is the declaration's, the time is the database's,
// and a check at the limit charges nothing, so it moves the time of the last charge neither back nor forward. A
// command that declares the reset of the field (CommandSpec.Resets) sets the count to 0 with ResetAttempts, decided
// with its own permission on the row, and gives the fill no value, no hash and no count.

// maxAttempts is the highest limit of a counter (ST-DECL-INV104).
const maxAttempts = 100

// The window of the decay of a count (ST-DECL-INV104, decision ST-SUP-Q205 (c)): DefaultUnlockAfter for a counter
// that gives none, from MinUnlockAfter to MaxUnlockAfter, in whole seconds.
const (
	DefaultUnlockAfter = 15 * time.Minute
	MinUnlockAfter     = time.Minute
	MaxUnlockAfter     = 24 * time.Hour
)

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
// table. Each takes the tenant as $1, the counter as $2 and the row ID as $3; the charge takes the limit as $4 and the
// window of the decay in seconds as $5, and gives no row at the limit. The charge counts again from 1 when the last
// charge of the row is the window or longer before the start of its statement on the clock of the database, or has no
// time, as a charge of a release before the decay leaves (decision ST-SUP-Q205 (c)). At the limit it changes nothing,
// so checks at the limit never move the end of the window.
const (
	chargeStatement = `INSERT INTO public.attempt_counts AS a (tenant_id, counter, row_id, checks, charged_at)
VALUES ($1, $2, $3, 1, pg_catalog.statement_timestamp())
ON CONFLICT (tenant_id, counter, row_id) DO UPDATE SET
checks = CASE WHEN a.charged_at IS NULL OR a.charged_at <= pg_catalog.statement_timestamp() - pg_catalog.make_interval(secs => $5::double precision)
THEN 1 ELSE a.checks + 1 END,
charged_at = pg_catalog.statement_timestamp()
WHERE a.checks < $4 OR a.charged_at IS NULL OR a.charged_at <= pg_catalog.statement_timestamp() - pg_catalog.make_interval(secs => $5::double precision)
RETURNING a.checks`
	resetStatement = `DELETE FROM public.attempt_counts WHERE tenant_id = $1 AND counter = $2 AND row_id = $3`
)

// CounterSpec is the generated declaration of the limit of wrong attempts of one compare-only field
// (ST-SUP-RC32-OC24): the entity, the field, the limit and the window of the decay of a count, UnlockAfter, which is
// DefaultUnlockAfter when it is zero, as in the code of a generator before the decay (decision ST-SUP-Q205 (c)).
type CounterSpec struct {
	Entity, Field string
	Limit         int
	UnlockAfter   time.Duration
}

// Counter is one checked counter declaration.
type Counter struct{ spec CounterSpec }

// NewCounter checks the declaration of a counter: its names, a limit from 1 to 100, and a window of the decay from
// MinUnlockAfter to MaxUnlockAfter in whole seconds, or zero for DefaultUnlockAfter.
func NewCounter(spec CounterSpec) (*Counter, error) {
	if spec.Entity == "" || spec.Field == "" || strings.Contains(spec.Entity+spec.Field, ".") {
		return nil, errors.New("command: a counter needs its entity and its field, with no period in them")
	}
	if spec.Limit < 1 || spec.Limit > maxAttempts {
		return nil, fmt.Errorf("command: the counter of %s.%s has the limit %d; give 1 to %d", spec.Entity, spec.Field, spec.Limit, maxAttempts)
	}
	if spec.UnlockAfter == 0 {
		spec.UnlockAfter = DefaultUnlockAfter
	}
	if u := spec.UnlockAfter; u < MinUnlockAfter || u > MaxUnlockAfter || u%time.Second != 0 {
		return nil, fmt.Errorf("command: the counter of %s.%s unlocks after %s; give whole seconds from %s to %s", spec.Entity, spec.Field, u, MinUnlockAfter, MaxUnlockAfter)
	}
	return &Counter{spec: spec}, nil
}

// UnlockAfter is the window of the decay of the counter: a row at its limit opens again this long after its last
// charge.
func (c *Counter) UnlockAfter() time.Duration { return c.spec.UnlockAfter }

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

// ResetAttempts sets the count of the wrong attempts of the counter c on the row of on to 0 when the call commits: the
// step-up reset of a command that declares the reset of the field in CommandSpec.Resets (ST-DECL-INV104, decision
// ST-SUP-Q205 (c)). on is a loaded value of the entity of c that this handle read. The runtime first decides the
// command's own permission on that row, as for a locked read, so a caller who has the permission on another row
// cannot reset this one; a deny is ErrNotFound. The reset gives the fill only an error: no value, no hash and no
// count, and nil both for a row at its limit and for a row with no count, so a reset tells nothing of the secret.
func ResetAttempts[P any](ctx context.Context, h *Handle[P], c *Counter, on Ref) error {
	if h == nil || h.st == nil || c == nil {
		return errors.New("command: a reset of the attempts needs a handle and a counter")
	}
	s := h.st
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	if s.mode != modeCommand || !s.resets[c.name()] || s.commandPermission == "" {
		return fmt.Errorf("command: %s does not declare the reset of the attempts of %s", s.action, c.name())
	}
	if on == nil {
		return ErrNoResource
	}
	r := refOf(on)
	if r.st != s || r.entity != c.spec.Entity || r.resourceType == "" {
		return ErrNoResource
	}
	if err := s.decideCommandOn(ctx, r.resourceType, r.entity, r.id, r.token); err != nil {
		return err
	}
	s.resetAttemptsHeld(attemptKey{counter: c, row: strings.ToLower(r.id)})
	return nil
}

// checkResets checks the declared resets of the attempts of a command: each names <entity>.<field> of an entity that
// the command writes, and the command decides a permission of its own on a row, not on the service-level resource
// (ST-DECL-INV104).
func checkResets(name, permission string, resource *Resource, resets []string, writes []Declared) (map[string]bool, error) {
	if len(resets) == 0 {
		return nil, nil
	}
	if permission == "" || resource == nil || resource.Service != "" {
		return nil, fmt.Errorf("command: %s declares a reset of attempts and no permission on a row", name)
	}
	written := map[string]bool{}
	for _, w := range writes {
		written[w.entityName()] = true
	}
	out := make(map[string]bool, len(resets))
	for _, r := range resets {
		entity, field, ok := strings.Cut(r, ".")
		if !ok || field == "" || strings.Contains(field, ".") || !written[entity] {
			return nil, fmt.Errorf("command: %s declares the reset of the attempts of %q, which is not <entity>.<field> of an entity that it writes", name, r)
		}
		out[r] = true
	}
	return out, nil
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
	rt.warnSmallChargePool()
	perTenant, shared := chargeLimits(db)
	release, err := rt.chargeSlots.take(ctx, t.ID(), perTenant, shared, func(inUse, waiting int) {
		rt.saturated(db, key, inUse, waiting)
	})
	if err != nil {
		return false, err
	}
	defer release()
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
	err = tx.QueryRowContext(ctx, chargeStatement, t.ID(), key.counter.name(), key.row, key.counter.spec.Limit,
		int64(key.counter.spec.UnlockAfter/time.Second)).Scan(&checks)
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

// AttemptsFailedEvent is the event name of the error log line of a charge of a counted check that cannot commit, and
// the name of its counter.
const AttemptsFailedEvent = "command.attempts.failed"

// attemptsFailedDescription is the description of the counter AttemptsFailedEvent.
const attemptsFailedDescription = "The charges of counted checks that did not commit, so the call answered 503, by action, entity and cause class."

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

// logChargeFailure counts and logs the charge of a counted check that cannot commit, so the call answers 503: it adds
// one to the counter AttemptsFailedEvent of the Meter of the runtime by action, entity and cause class; and logs one
// error line through the log provider of the runtime, in the format of each other log line of the runtime, with the action, the
// entity, the cause class and the count of the lines held back, and no value and no error text (ST-SUP-RP10-LC54;
// walkthrough 20, P-C). It logs one line for each action, entity and cause class in each authz.LogEvery. With no log
// provider, it logs the same fields with log/slog.
func (rt *Runtime) logChargeFailure(action, entity string, err error) {
	cause := chargeCause(err)
	// The counter counts each failed charge, also those whose line the rate limit holds back, with no tenant and no
	// value (security review of PR #482, I4).
	if rt.attemptsFailed != nil {
		rt.attemptsFailed.Add(context.Background(), 1, metric.WithAttributes(attribute.String("action", action),
			attribute.String("entity", entity), attribute.String("cause", cause)))
	}
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

// saturated logs, at most once in saturationLogEvery, that the charge of key waits for a slot while the charges hold
// each shared slot of the pool of the charges, which is all but one of its connections (walkthrough 22, P1-PS): the
// slots never let the charges hold more connections than the pool has, so a full pool shows at the wait for a slot,
// not at the wait for a connection. It logs through the log provider of the runtime, or with log/slog when it has
// none. inUse is the count of the slots in use and waiting the count of the charges that wait for a slot, with this
// one. The log names the entity of the counter and the counts of the pool, never a value or a tenant.
func (rt *Runtime) saturated(db *sql.DB, key attemptKey, inUse, waiting int) {
	maxOpen := db.Stats().MaxOpenConnections
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
		slog.Warn(PoolSaturatedEvent, "entity", entity, "in_use", inUse, "max_open", maxOpen, "waiting", waiting)
		return
	}
	var record otellog.Record
	record.SetEventName(PoolSaturatedEvent)
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("WARN")
	record.SetBody(attribute.StringValue("the charges of counted checks hold each shared connection of their pool, so a charge waits for a slot"))
	record.AddAttributes(attribute.String("stego.entity", entity), attribute.Int("stego.pool.in_use", inUse),
		attribute.Int("stego.pool.max_open", maxOpen), attribute.Int("stego.pool.waiting", waiting))
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

// chargeLimits gives the limits of the slots of the charges on the pool db of n connections: each tenant holds at
// most perTenant of the n-1 shared slots, and only a tenant that holds no slot can take the one reserved slot. Thus
// the charges of one or two tenants never hold the whole pool, and a tenant with no charge in flight can take the
// last connection (security review of PR #481, L1, and of PR #482, L1). perTenant is half of n, and at least one and
// at most n-1 when n is 2 or more. A pool with no bound has no limit of all the slots and no reserved slot (shared is
// -1), and takes defaultTenantCharges for each tenant; a pool of 1 has no shared slot, so its charges run one at a
// time. New warns of both.
func chargeLimits(db *sql.DB) (perTenant, shared int) {
	n := db.Stats().MaxOpenConnections
	if n <= 0 {
		return defaultTenantCharges, -1
	}
	return max(1, min(n/2, n-1)), n - 1
}

// defaultTenantCharges is the most charges of one tenant at one time on a pool of the charges with no bound.
const defaultTenantCharges = 2

// minChargeConnections is the smallest pool of the charges with which the slots keep the charges of two tenants from
// holding the whole pool: New warns of a smaller pool, and of a pool with no bound.
const minChargeConnections = 3

// tenantSlots limits the charges that run at one time, before a charge takes a connection of the pool of the charges
// (security review of PR #481, L1, and of PR #482, L1): a charge takes one of the shared slots while its tenant holds
// fewer than perTenant slots and fewer than shared slots are in use, and else the one reserved slot while its tenant
// holds no slot and the reserved slot is free. Else it waits for a slot, never for a connection that another tenant
// needs. It keeps an entry only for a tenant with a charge that runs or waits, so many tenants cannot grow it. Its
// zero value is ready.
type tenantSlots struct {
	mu       sync.Mutex
	tenants  map[string]*tenantSlot
	shared   int
	reserved bool
	// changed is closed and made new when a slot is given back, so each waiting charge looks again.
	changed chan struct{}
	// scans counts the reads of blockedAll, which only the report of a full pool needs (security review of PR #488,
	// L2).
	scans int
}

// tenantSlot is the slots that one tenant holds, the count of its charges that run or wait, and the count of those
// that wait.
type tenantSlot struct {
	held, users, blocked int
}

// take takes a slot for a charge of the tenant id under the limits perTenant and shared (no limit of all the slots,
// and no reserved slot, when shared is negative, as for a pool with no bound), and gives the function that
// gives it back. It waits at most until ctx ends, and then gives the error of ctx, so the check fails and the call
// answers 503. When the charge must wait while each shared slot is in use, it calls full once, outside the lock,
// with the count of the slots in use and the count of the charges that wait (walkthrough 22, P1-PS); a charge that
// waits only for the limit of its own tenant, while a shared slot is free, does not call it.
func (s *tenantSlots) take(ctx context.Context, id string, perTenant, shared int, full func(inUse, waiting int)) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenants == nil {
		s.tenants = map[string]*tenantSlot{}
	}
	slot := s.tenants[id]
	if slot == nil {
		slot = &tenantSlot{}
		s.tenants[id] = slot
	}
	slot.users++
	bounded := shared >= 0
	reported := false
	for {
		switch {
		case slot.held < perTenant && (!bounded || s.shared < shared):
			slot.held++
			s.shared++
			return func() { s.give(id, slot, false) }, nil
		case bounded && slot.held == 0 && !s.reserved:
			slot.held++
			s.reserved = true
			return func() { s.give(id, slot, true) }, nil
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		changed := s.changed
		slot.blocked++
		report := !reported && full != nil && bounded && s.shared >= shared
		// The counts of the report are read only for a report: blockedAll reads each tenant under s.mu, so each wake
		// with no report holds the lock only for the checks above (security review of PR #488, L2).
		var inUse, waiting int
		if report {
			inUse, waiting = s.shared, s.blockedAll()
			if s.reserved {
				inUse++
			}
		}
		s.mu.Unlock()
		if report {
			reported = true
			full(inUse, waiting)
		}
		select {
		case <-changed:
			s.mu.Lock()
			slot.blocked--
		case <-ctx.Done():
			s.mu.Lock()
			slot.blocked--
			s.leave(id, slot)
			return nil, ctx.Err()
		}
	}
}

// give gives back a slot of the tenant id, the reserved slot when reserved, and wakes each waiting charge.
func (s *tenantSlots) give(id string, slot *tenantSlot, reserved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot.held--
	if reserved {
		s.reserved = false
	} else {
		s.shared--
	}
	s.leave(id, slot)
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// leave counts out one charge of the tenant id, and removes the entry of a tenant with no charge that runs or waits.
// The caller holds s.mu.
func (s *tenantSlots) leave(id string, slot *tenantSlot) {
	if slot.users--; slot.users == 0 {
		delete(s.tenants, id)
	}
}

// blockedAll gives the count of the charges of all the tenants that wait for a slot. The caller holds s.mu.
func (s *tenantSlots) blockedAll() int {
	s.scans++
	n := 0
	for _, slot := range s.tenants {
		n += slot.blocked
	}
	return n
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

// blocked gives the count of the charges of the tenant id that wait for a slot.
func (s *tenantSlots) blocked(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.tenants[id]; slot != nil {
		return slot.blocked
	}
	return 0
}

// warnSmallChargePool logs, once for the runtime, that the pool of the charges has fewer than minChargeConnections
// connections or no bound, so the slots cannot keep the charges of two tenants from holding the whole pool (security
// review of PR #482, L1): through the log provider of the runtime, or with log/slog when it has none. New calls it
// for a configuration with Config.ChargeDB, and the first charge calls it, so a runtime whose charges use the audit
// pool warns only when it charges.
func (rt *Runtime) warnSmallChargePool() {
	n := rt.chargeDB().Stats().MaxOpenConnections
	if n >= minChargeConnections || !rt.chargePoolWarned.CompareAndSwap(false, true) {
		return
	}
	const body = "the pool of the charges of counted checks has fewer than 3 connections or no bound, so the charges of a few tenants can hold the whole pool"
	if rt.logger == nil {
		slog.Warn(PoolSmallEvent, "max_open", n, "min_open", minChargeConnections)
		return
	}
	var record otellog.Record
	record.SetEventName(PoolSmallEvent)
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("WARN")
	record.SetBody(attribute.StringValue(body))
	record.AddAttributes(attribute.Int("stego.pool.max_open", n), attribute.Int("stego.pool.min_open", minChargeConnections))
	rt.logger.Emit(context.Background(), record)
}

// PoolSmallEvent is the event name of the warn log line of a pool of the charges that is too small or has no bound.
const PoolSmallEvent = "command.attempts.pool_small"

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
