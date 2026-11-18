package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The seal step of a field that the base stores in clear and the head declares secret (ST-SUP-RC01-OC49, decision
// ST-DECL-Q44). While the step is open, the field has two columns: the clear column, which a release before writes
// alone, and the sealed column <column>_sealed. A write of the handle sets both (ClearOf and SealAt), and a read takes
// the clear value when the two differ (SealingAt), so a release before and a roll back to it lose no write
// (ST-SUP-RP10-LC49). The seal job of the controller seals and checks each row in passes (ST-SUP-RP10-LC50), and the
// contract step drops the clear column only after a pass that no release before ran during (ST-SUP-RP09-LC15).

// ClearOf gives the statement value of the clear column of a readable secret whose seal step is open: its text, or
// NULL for a value that is not set (ST-SUP-RP10-LC49).
func ClearOf(value sealed.Secret) any {
	if !value.IsSet() {
		return nil
	}
	return value.Reveal()
}

// Sealing is the pair of scan targets of a readable secret whose seal step is open: Clear for its clear column and
// Sealed for its sealed column. The runtime gives the value after the scan of the row (ST-SUP-RP10-LC49).
type Sealing struct {
	table, column string
	row           *string
	into          *sealed.Secret
	clear         sql.NullString
	raw           []byte
}

// SealingAt gives the scan targets of a readable secret of the sealed column of the table whose seal step is open,
// which the runtime resolves into into after the scan of the row, at the row ID that row holds then.
func SealingAt(table, column string, row *string, into *sealed.Secret) *Sealing {
	return &Sealing{table: table, column: column, row: row, into: into}
}

// Clear gives the scan target of the clear column.
func (s *Sealing) Clear() any { return &s.clear }

// Sealed gives the scan target of the sealed column.
func (s *Sealing) Sealed() any { return sealingTarget{s} }

// sealingTarget keeps the bytes of the sealed column, or NULL.
type sealingTarget struct{ s *Sealing }

func (t sealingTarget) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		t.s.raw = nil
	case []byte:
		t.s.raw = append([]byte(nil), v...)
	default:
		return fmt.Errorf("command: the sealed column %s holds a %T, not bytes", t.s.column, src)
	}
	return nil
}

// resolve gives the value of the field: the clear value, which only a release before writes alone, when the sealed
// value is NULL, does not open or opens to another text; else the opened value, which is the same text. A clear
// column that is NULL is a value that is not set. A sealed value that does not open is logged with its column and
// row, never a value.
func (s *Sealing) resolve(ctx context.Context, ring *sealed.Ring, at sealed.Place) {
	if !s.clear.Valid {
		*s.into = sealed.Secret{}
		return
	}
	*s.into = sealed.SecretOf(s.clear.String)
	if s.raw == nil || ring == nil {
		return
	}
	plain, err := ring.Open(s.raw, at)
	if err != nil {
		slog.WarnContext(ctx, "command: a sealed value of an open seal step does not open; the clear value is read", "error", err.Error())
		return
	}
	clear(plain)
}

// SealColumn is one readable secret field whose seal step is open, which the seal job of the controller seals
// (ST-SUP-RP10-LC50): its change, its entity, its table, its clear and its sealed column, and the generated statements
// of the job, which take the tenant as $1:
//   - Select reads the ID text, the clear value as text and the sealed value of at most $3 rows after the ID $2, in
//     ID order, FOR UPDATE, so a write of a command waits for the batch or the batch for it;
//   - Update sets the sealed column of the row of ID $2 to $3.
type SealColumn struct {
	Change, Entity, Table, Clear, Sealed string
	Select, Update                       string
}

// SealBatch is the most rows that one batch of the seal job reads.
const SealBatch = 100

// SealPause is the wait of the seal job after each batch, which leaves room for the writes of the commands.
const SealPause = 50 * time.Millisecond

// The timeouts of a batch of the seal job, as of a backfill batch (ST-SUP-RP09-LC09).
const (
	sealLockTimeout      = "2s"
	sealStatementTimeout = "10s"
)

// SealResult is what one run of the seal job did for one change: the rows of its pass that it sealed and checked,
// whether the pass completed, and whether it waits for a release before the step to stop.
type SealResult struct {
	Change           string
	Sealed, Checked  int64
	Completed, Waits bool
}

// SealJob is the seal job of the open seal steps of a service (ST-SUP-RP10-LC50).
type SealJob struct {
	db       *sql.DB
	schema   string
	ring     *sealed.Ring
	columns  []SealColumn
	holder   string
	settings lease.Settings
	tenants  func(ctx context.Context) ([]tenant.Tenant, error)
	pause    time.Duration
	// batched, when a test sets it, runs after each batch that commits.
	batched func()
}

// NewSealJob makes the seal job of the columns over the tenants of the tenant list of schema, on db, a pool of the
// runtime role that tenant.Open returned, with the key ring, under the lease of each change for the holder.
func NewSealJob(db *sql.DB, schema string, ring *sealed.Ring, columns []SealColumn, holder string, settings lease.Settings) (*SealJob, error) {
	switch {
	case db == nil || ring == nil:
		return nil, errors.New("command: the seal job needs a pool and the key ring")
	case schema == "" || holder == "":
		return nil, errors.New("command: the seal job needs the schema of the tenant list and a holder")
	}
	for _, c := range columns {
		if c.Change == "" || c.Entity == "" || c.Table == "" || c.Clear == "" || c.Sealed == "" || c.Select == "" || c.Update == "" {
			return nil, fmt.Errorf("command: the seal column %s.%s lacks a name or a statement", c.Entity, c.Clear)
		}
	}
	j := &SealJob{db: db, schema: schema, ring: ring, columns: columns, holder: holder, settings: settings, pause: SealPause}
	j.tenants = func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, db, schema) }
	return j, nil
}

// SealLease gives the lease of the seal job of a change: the backfill lease stego.backfill.<change ID>.
func SealLease(change string) string { return "stego.backfill." + change }

// Run runs the pass of each change under its lease until the pass ends, the lease ends or ctx ends, and logs, for
// each change, the rows that it sealed and checked and whether the pass completed, and no value. A change whose pass
// completed and is not stale has no work.
func (j *SealJob) Run(ctx context.Context) ([]SealResult, error) {
	var results []SealResult
	var failed []error
	for _, c := range j.columns {
		result := SealResult{Change: c.Change}
		client, err := lease.New(j.db, SealLease(c.Change), j.holder, j.settings)
		if err != nil {
			return results, err
		}
		err = client.Hold(ctx, func(ctx context.Context, held *lease.Held) error {
			var err error
			result, err = j.pass(ctx, held, c)
			return err
		})
		results = append(results, result)
		if err != nil {
			failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: %w", c.Clear, c.Entity, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		slog.InfoContext(ctx, "command: seal of a secret column", "change", c.Change, "entity", c.Entity, "column", c.Clear,
			"sealed", result.Sealed, "checked", result.Checked, "completed", result.Completed, "waits", result.Waits)
	}
	return results, errors.Join(failed...)
}

// inTx runs fn in one transaction of the pool that is not of a tenant: the checkpoint functions need none.
func (j *SealJob) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// pass goes on with the pass of the change c from its checkpoint under held, and starts a new pass when there is
// none or a release before the step ran during it.
func (j *SealJob) pass(ctx context.Context, held *lease.Held, c SealColumn) (SealResult, error) {
	result := SealResult{Change: c.Change}
	var state lease.SealState
	if err := j.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		state, err = lease.ReadSeal(ctx, tx, c.Change)
		return err
	}); err != nil {
		return result, err
	}
	if state.Completed && !state.Stale {
		result.Sealed, result.Checked, result.Completed = state.Sealed, state.Checked, true
		return result, nil
	}
	if !state.Started || state.Stale {
		if state.Waiting {
			result.Waits = true
			return result, nil
		}
		err := j.inTx(ctx, func(tx *sql.Tx) error {
			return lease.SealCheckpoint(ctx, tx, held, c.Change, "", "", 0, 0, true)
		})
		if errors.Is(err, lease.ErrSealWaiting) {
			result.Waits = true
			return result, nil
		}
		if err != nil {
			return result, err
		}
		state = lease.SealState{Started: true}
	}
	tenants, err := j.tenants(ctx)
	if err != nil {
		return result, err
	}
	slices.SortFunc(tenants, func(a, b tenant.Tenant) int { return strings.Compare(a.ID(), b.ID()) })
	result.Sealed, result.Checked = state.Sealed, state.Checked
	for _, t := range tenants {
		if state.LastTenant != "" && t.ID() < state.LastTenant {
			continue
		}
		after := firstID
		if t.ID() == state.LastTenant && state.LastID != "" {
			after = state.LastID
		}
		for {
			var read int
			err := tenant.Each(ctx, j.db, []tenant.Tenant{t}, func(ctx context.Context, tx tenant.Querier, t tenant.Tenant) error {
				var err error
				read, after, err = j.batch(ctx, tx, held, t, c, after, &result)
				return err
			})
			if err != nil {
				return result, unwrapEach(err)
			}
			if j.batched != nil {
				j.batched()
			}
			if read < SealBatch {
				break
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(j.pause):
			}
		}
	}
	var stale error
	err = j.inTx(ctx, func(tx *sql.Tx) error {
		// The start of the pass again after ErrSealStale is committed.
		if stale = lease.SealComplete(ctx, tx, held, c.Change); errors.Is(stale, lease.ErrSealStale) {
			return nil
		}
		return stale
	})
	if err == nil && stale != nil {
		err = stale
	}
	if errors.Is(err, lease.ErrSealStale) {
		// A release before the step ran during the pass and can have written the clear column alone: the next run
		// starts the pass again.
		slog.WarnContext(ctx, "command: a release before the seal step ran during the pass; the pass starts again", "change", c.Change)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Completed = true
	return result, nil
}

// batch seals and checks at most SealBatch rows of the column c of the tenant t after the ID after, in the
// transaction of the tenant, and moves the checkpoint in the same transaction. It gives the rows that it read and the
// last ID.
func (j *SealJob) batch(ctx context.Context, tx tenant.Querier, held *lease.Held, t tenant.Tenant, c SealColumn, after string, result *SealResult) (int, string, error) {
	if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true), pg_catalog.set_config('statement_timeout', $2, true)", sealLockTimeout, sealStatementTimeout); err != nil {
		return 0, after, err
	}
	rows, err := tx.QueryContext(ctx, c.Select, t.ID(), after, SealBatch)
	if err != nil {
		return 0, after, err
	}
	type found struct {
		id    string
		clear sql.NullString
		raw   []byte
	}
	var all []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.id, &f.clear, &f.raw); err != nil {
			rows.Close()
			return 0, after, err
		}
		all = append(all, f)
	}
	if err := rows.Close(); err != nil {
		return 0, after, err
	}
	if err := rows.Err(); err != nil {
		return 0, after, err
	}
	sealedRows, checked := result.Sealed, result.Checked
	last := after
	for _, f := range all {
		last = f.id
		checked++
		at := sealed.Place{Tenant: t.ID(), Table: c.Table, Column: c.Sealed, Row: f.id}
		var value any
		switch {
		case !f.clear.Valid && f.raw == nil:
			continue
		case !f.clear.Valid:
			// A release before set the field to no value after a seal.
			value = nil
		case f.raw != nil:
			plain, err := j.ring.Open(f.raw, at)
			same := err == nil && string(plain) == f.clear.String
			clear(plain)
			if same {
				continue
			}
			fallthrough
		default:
			b, err := j.ring.Seal([]byte(f.clear.String), at)
			if err != nil {
				return 0, after, sealedf("command: the column %s of the row %s of %s does not seal", c.Sealed, f.id, c.Table)
			}
			value = b
		}
		if _, err := tx.ExecContext(ctx, c.Update, t.ID(), f.id, value); err != nil {
			return 0, after, err
		}
		sealedRows++
	}
	if len(all) > 0 {
		if err := lease.SealCheckpoint(ctx, tx, held, c.Change, t.ID(), last, sealedRows, checked, false); err != nil {
			return 0, after, err
		}
	}
	result.Sealed, result.Checked = sealedRows, checked
	return len(all), last, nil
}
