package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The counters of wrong attempts (ST-SUP-RP10-LC54, decision ST-SUP-Q185 (a)): a compare-only field with a counter
// (ST-DECL-INV104) is read into CountedAt, which binds each stored value that the handle reads to the counter of its
// row. Matches then asks the counter before it checks, and records each result. Run adds the wrong attempts of a call
// to the count of each row: in the transaction of a command that commits, and in a short transaction of its own after
// a call that rolls back, so a declared refusal does not roll the count back.

// maxAttempts is the highest limit of a counter (ST-DECL-INV104).
const maxAttempts = 100

// attemptsBound bounds the short transaction that commits the count after a rollback.
const attemptsBound = 10 * time.Second

// CounterSpec is the generated declaration of the counter of one compare-only field (ST-SUP-RC32-OC24): the entity,
// the counted field, the counter column and the limit, and its statements, which take the tenant as $1 and the row ID
// as $2:
//   - Count adds $3 to the count of the row, at most to the limit, raises the version of the row by one and sets its
//     update time;
//   - Reset sets the count of the row to 0.
type CounterSpec struct {
	Entity, Field, Column string
	Limit                 int
	Count, Reset          string
}

// Counter is one checked counter declaration.
type Counter struct{ spec CounterSpec }

// NewCounter checks the declaration of a counter: its names, a limit from 1 to 100, and statements that take their
// parameters.
func NewCounter(spec CounterSpec) (*Counter, error) {
	if spec.Entity == "" || spec.Field == "" || spec.Column == "" {
		return nil, errors.New("command: a counter needs its entity, its field and its column")
	}
	if spec.Limit < 1 || spec.Limit > maxAttempts {
		return nil, fmt.Errorf("command: the counter of %s.%s has the limit %d; give 1 to %d", spec.Entity, spec.Field, spec.Limit, maxAttempts)
	}
	for _, p := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(spec.Count, p) {
			return nil, fmt.Errorf("command: the Count statement of the counter of %s.%s does not take %s", spec.Entity, spec.Field, p)
		}
	}
	if !strings.Contains(spec.Reset, "$1") || !strings.Contains(spec.Reset, "$2") {
		return nil, fmt.Errorf("command: the Reset statement of the counter of %s.%s does not take the tenant and the row", spec.Entity, spec.Field)
	}
	return &Counter{spec: spec}, nil
}

// countedTarget is the scan target of a counted compare-only field: its stored hash, which the handle binds to the
// counter of its row after the scan.
type countedTarget struct {
	counter *Counter
	row     *string
	into    *sealed.Compared
	count   *int32
}

// Scan keeps the stored hash, as the target of a compare-only field does.
func (t *countedTarget) Scan(src any) error { return comparedTarget{into: t.into}.Scan(src) }

// CountedAt gives the scan target of a compare-only field with the counter c: the row ID that row holds and the count
// that count holds after the scan of the row bind the value to its counter (ST-SUP-RP10-LC54).
func CountedAt(c *Counter, row *string, into *sealed.Compared, count *int32) any {
	return &countedTarget{counter: c, row: row, into: into, count: count}
}

// attemptKey names the count of one row of one counter.
type attemptKey struct {
	counter *Counter
	row     string
}

// attemptRow is what the checks of one call did to the count of one row: the count that the first bound read gave,
// the wrong attempts and whether a check matched.
type attemptRow struct {
	loaded  int32
	wrong   int
	matched bool
}

// rowCounter is the counter of one row as one read of the handle bound it.
type rowCounter struct {
	s      *state
	key    attemptKey
	count  int32
	locked bool
}

// Allow gives true only in a command, for a row that the handle locked, while the call is open and the count with the
// wrong attempts of the call is below the limit.
func (k *rowCounter) Allow() bool {
	s := k.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.mode != modeCommand || !k.locked {
		return false
	}
	row := s.attemptRow(k.key, k.count)
	return int(row.loaded)+row.wrong < k.key.counter.spec.Limit
}

// Record records one check of the row.
func (k *rowCounter) Record(matched bool) {
	s := k.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	row := s.attemptRow(k.key, k.count)
	if matched {
		row.matched = true
	} else {
		row.wrong++
	}
}

// attemptRow gives the record of the row of key in the call, with the loaded count of its first bound read. The
// caller holds the lock.
func (s *state) attemptRow(key attemptKey, count int32) *attemptRow {
	if s.attempts == nil {
		s.attempts = map[attemptKey]*attemptRow{}
	}
	row, ok := s.attempts[key]
	if !ok {
		row = &attemptRow{loaded: count}
		s.attempts[key] = row
	}
	return row
}

// bindCounted binds the value of a counted target to the counter of its row after the scan of the row.
func (s *state) bindCounted(t *countedTarget, locked bool) {
	if t.row == nil || t.count == nil {
		return
	}
	k := &rowCounter{s: s, key: attemptKey{counter: t.counter, row: strings.ToLower(*t.row)}, count: *t.count, locked: locked}
	*t.into = sealed.CountedBy(*t.into, k)
}

// sortedAttempts gives the rows of the call in a fixed order, so two calls lock their rows in the same order.
func (s *state) sortedAttempts() []attemptKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]attemptKey, 0, len(s.attempts))
	for key := range s.attempts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.counter.spec.Entity != b.counter.spec.Entity {
			return a.counter.spec.Entity < b.counter.spec.Entity
		}
		if a.counter.spec.Field != b.counter.spec.Field {
			return a.counter.spec.Field < b.counter.spec.Field
		}
		return a.row < b.row
	})
	return keys
}

// countAttempts runs, in the transaction of a call that commits, the count of each row: it adds the wrong attempts,
// or sets the count of a row with a match and no wrong attempt to 0 (ST-SUP-RP10-LC54).
func (s *state) countAttempts(ctx context.Context) error {
	for _, key := range s.sortedAttempts() {
		row := s.attempts[key]
		spec := key.counter.spec
		var err error
		switch {
		case row.wrong > 0:
			_, err = s.exec(ctx, spec.Count, s.tenant.ID(), key.row, row.wrong)
		case row.matched:
			_, err = s.exec(ctx, spec.Reset, s.tenant.ID(), key.row)
		default:
			continue
		}
		if err != nil {
			return s.fail(ctx, "count the attempts of "+spec.Entity, err)
		}
	}
	return nil
}

// commitAttempts adds the wrong attempts of a call that rolled back to the count of each row, in a short transaction
// of its own with the tenant of the call, which writes no audit row and stages no change event. It logs a failure as
// command.attempts.failed with the action and the entity, and no value (ST-SUP-RP10-LC54).
func (rt *Runtime) commitAttempts(ctx context.Context, s *state) {
	if s == nil || len(s.attempts) == 0 {
		return
	}
	var keys []attemptKey
	for _, key := range s.sortedAttempts() {
		if s.attempts[key].wrong > 0 {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptsBound)
	defer cancel()
	failed := func(entity string) {
		slog.ErrorContext(ctx, "command.attempts.failed", "action", s.action, "entity", entity)
	}
	tx, err := tenant.Begin(ctx, rt.cfg.DB, s.tenant, nil)
	if err != nil {
		failed(keys[0].counter.spec.Entity)
		return
	}
	for _, key := range keys {
		spec := key.counter.spec
		rt.record(spec.Count, nil)
		if _, err := tx.ExecContext(ctx, spec.Count, s.tenant.ID(), key.row, s.attempts[key].wrong); err != nil {
			_ = tx.Rollback()
			failed(spec.Entity)
			return
		}
	}
	if err := tenant.Commit(ctx, tx, s.tenant); err != nil {
		failed(keys[0].counter.spec.Entity)
	}
}
