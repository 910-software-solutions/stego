package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

// maxAttempts is the highest limit of a counter (ST-DECL-INV104).
const maxAttempts = 100

// chargeBound bounds the short transaction of one charge.
const chargeBound = 5 * time.Second

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

// rowCounter is the counter of one row as one read of the handle bound it.
type rowCounter struct {
	s   *state
	key attemptKey
}

// Charge takes one check of the row in a short transaction of its own, which commits before the check runs: false in
// a worker, after the call ended, at the limit, and when the charge cannot commit, which also makes the call answer
// 503 (ST-SUP-RP10-LC54).
func (k *rowCounter) Charge() bool {
	s := k.s
	s.mu.Lock()
	open := !s.closed && s.mode == modeCommand
	s.mu.Unlock()
	if !open {
		return false
	}
	charged, err := s.rt.charge(s.tenant, k.key, s.deadline)
	if err != nil {
		s.mu.Lock()
		s.chargeFailed = true
		s.mu.Unlock()
		s.rt.logChargeFailure(s.action, k.key.counter.spec.Entity, err)
		return false
	}
	if s.rt.charged != nil {
		s.rt.charged(charged)
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
	tx, err := tenant.Begin(ctx, rt.cfg.DB, t, nil)
	if err != nil {
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
	switch {
	case tenant.Unavailable(err):
		return "unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "internal"
}

// logChargeFailure logs the charge of a counted check that cannot commit, so the call answers 503: one error line
// through the log provider of the runtime, in the format of each other log line of the runtime, with the action, the
// entity and the cause class, and no value and no error text (ST-SUP-RP10-LC54; walkthrough 20, P-C). With no log
// provider, it logs the same fields with log/slog.
func (rt *Runtime) logChargeFailure(action, entity string, err error) {
	cause := chargeCause(err)
	if rt.logger == nil {
		slog.Error(AttemptsFailedEvent, "action", action, "entity", entity, "cause", cause)
		return
	}
	var record otellog.Record
	record.SetEventName(AttemptsFailedEvent)
	record.SetSeverity(otellog.SeverityError)
	record.SetSeverityText("ERROR")
	record.SetBody(attribute.StringValue("the charge of a counted check did not commit, so the check fails and the call answers 503"))
	record.AddAttributes(attribute.String("stego.action", action), attribute.String("stego.entity", entity), attribute.String("stego.cause", cause))
	rt.logger.Emit(context.Background(), record)
}

// bindCounted binds the value of a counted target to the counter of its row after the scan of the row.
func (s *state) bindCounted(t *countedTarget) {
	if t.row == nil {
		return
	}
	k := &rowCounter{s: s, key: attemptKey{counter: t.counter, row: strings.ToLower(*t.row)}}
	*t.into = sealed.CountedBy(*t.into, k)
}

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

// chargeUnavailable gives the error of a call whose charge of a check did not commit: the check did not run, and the
// call answers 503 (ST-SUP-RP10-LC54).
func chargeUnavailable() error {
	return unavailablef("the charge of a check of a counted secret did not commit")
}
