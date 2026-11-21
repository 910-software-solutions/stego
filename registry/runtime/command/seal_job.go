package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
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

// AfterSeal is an entity after the contract step of each of its open seals (ST-SUP-RP10-LC49, ST-SUP-RC01-OC49):
// the generated statements, scan and values with the sealed column alone of each readable secret whose seal step is
// open, which the runtime takes once the contract has dropped the clear columns, so a process of the release of the
// expand step goes on after the contract (walkthrough 14, W14-B6). Table is the table of the entity with its schema,
// each name quoted, such as public."patients", and Clear the clear columns that the contract steps drop. The
// statements take the parameters of EntitySpec's statements of the same names.
type AfterSeal[T any] struct {
	Table string
	Clear []string
	// Changes are the seal changes of the clear columns, one for each column of Clear in the same order: the runtime
	// takes the clear column again only while the checkpoint row of its change is there (recheckSeal).
	Changes []string

	Select, SelectForUpdate, SelectForDelete, Insert, Update string

	Scan   func(row Scanner) (T, Meta, error)
	Values func(value T) []any
}

// check refuses an AfterSeal with no table, no clear column, no scan or no values, a statement that does not take the
// tenant as $1, and a locked read without its lock clause. A nil AfterSeal passes.
func (a *AfterSeal[T]) check(entity string) error {
	if a == nil {
		return nil
	}
	if a.Table == "" || len(a.Clear) == 0 || a.Scan == nil || a.Values == nil || len(a.Changes) != len(a.Clear) {
		return fmt.Errorf("command: the AfterSeal of %s needs its table, its clear columns with the seal change of each, Scan and Values", entity)
	}
	for _, text := range []string{a.Select, a.SelectForUpdate, a.SelectForDelete, a.Insert, a.Update} {
		if !strings.Contains(text, "$1") {
			return fmt.Errorf("command: a statement of the AfterSeal of %s does not take the tenant as $1", entity)
		}
	}
	upper := strings.ToUpper(a.SelectForDelete)
	if !strings.Contains(strings.ToUpper(a.SelectForUpdate), "FOR NO KEY UPDATE") || !strings.Contains(upper, "FOR UPDATE") ||
		strings.Contains(upper, "NO KEY") {
		return fmt.Errorf("command: the locked reads of the AfterSeal of %s do not lock as SelectForUpdate and SelectForDelete do", entity)
	}
	return nil
}

// entityStatements are the statements, the scan and the values that an entity runs now.
type entityStatements[T any] struct {
	selectRow, forUpdate, forDelete, insert, update string
	scan                                            func(row Scanner) (T, Meta, error)
	values                                          func(value T) []any
}

// statements gives the statements of the entity: those of AfterSeal once the contract of its open seals dropped the
// clear columns, else those of the spec.
func (e *Entity[T]) statements() entityStatements[T] {
	if a := e.spec.AfterSeal; a != nil && e.contracted.Load() {
		return entityStatements[T]{a.Select, a.SelectForUpdate, a.SelectForDelete, a.Insert, a.Update, a.Scan, a.Values}
	}
	sp := &e.spec
	return entityStatements[T]{sp.Select, sp.SelectForUpdate, sp.SelectForDelete, sp.Insert, sp.Update, sp.Scan, sp.Values}
}

// readStatement and lockStatement give the Read and the Lock of the list that the entity runs now.
func (l *List[T]) readStatement() string {
	if l.spec.ReadAfterSeal != "" && l.entity.contracted.Load() {
		return l.spec.ReadAfterSeal
	}
	return l.spec.Read
}

func (l *List[T]) lockStatement() string {
	if l.spec.LockAfterSeal != "" && l.entity.contracted.Load() {
		return l.spec.LockAfterSeal
	}
	return l.spec.Lock
}

// undefinedColumn is the SQLSTATE undefined_column, which a statement of an open seal gives after the contract step
// dropped its clear column.
const undefinedColumn = "42703"

// sealSwitch is an entity whose statements can move to those after the contract step of its open seals.
type sealSwitch interface {
	switchAfterSeal(ctx context.Context, db *sql.DB, err error, began int64) bool
}

// sealSwitches counts the switches of every entity of the process to its statements after a seal contract, and each
// entity keeps the count at its own switch, so an attempt that began before it can tell its statements from those
// after the contract.
var sealSwitches atomic.Int64

// switchAfterSeal takes the statements of AfterSeal from now on, and reports true, when err of a statement of the
// entity is SQLSTATE 42703 and the catalog shows that each clear column of AfterSeal is gone from its table, which is
// there (ST-SUP-RP10-LC49). It reports false, and changes nothing, for any other error, an entity with no open seal or
// that already took them, and a catalog that still shows a clear column or that it cannot read: an undefined column
// of another cause keeps the statements, so no write of the seal goes to the sealed column alone while a release
// before reads the clear column. The first switch of an entity logs it once, with no value.
func (e *Entity[T]) switchAfterSeal(ctx context.Context, db *sql.DB, err error, began int64) bool {
	a := e.spec.AfterSeal
	if a == nil || db == nil || sqlState(err) != undefinedColumn {
		return false
	}
	// A call that began before another call switched the entity ran a statement before the contract: it is the
	// same conflict, which Run runs again once with the statements after the contract (security review of PR #452,
	// L2). A call that began after the switch ran the statements after the contract, and its error is its own.
	if e.contracted.Load() {
		// Zero is a switch that has not stored its count yet.
		at := e.switchedAt.Load()
		return at == 0 || at > began
	}
	// The transaction of the statement failed, so the check reads the catalog on the pool, in a short time of its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogWait)
	defer cancel()
	for _, column := range a.Clear {
		var gone bool
		if err := db.QueryRowContext(ctx, clearGoneQuery, a.Table, column).Scan(&gone); err != nil || !gone {
			return false
		}
	}
	if e.contracted.CompareAndSwap(false, true) {
		e.switchedAt.Store(sealSwitches.Add(1))
		// The first attempt after the switch checks the catalog again (recheckSeal).
		e.recheckAt.Store(0)
		slog.InfoContext(ctx, "command: the contract step of a seal dropped the clear columns; the entity reads and writes its sealed columns alone",
			"entity", e.spec.Name, "table", a.Table)
	}
	return true
}

// catalogWait is the longest time that the catalog check of switchAfterSeal waits.
const catalogWait = 2 * time.Second

// sealRecheck is the shortest time between two catalog checks of recheckSeal of one entity.
const sealRecheck = time.Second

// recheckSeal takes the statements of the spec again, with the clear columns, when the entity took its statements
// after the contract and the catalog shows a clear column of AfterSeal in its table again together with the seal
// checkpoint row of its change, as after a restore of a backup from before the contract step (security review of PR
// #452, L1; ST-SUP-RP10-LC49). Else a process of the release of the expand step would write the sealed column alone
// while the clear column, which the other processes and the seal job read first, keeps an older value, and lose the
// update. A clear column with no checkpoint row, such as one that a person added by hand, changes nothing and is logged
// at the level error once, so no secret goes to it in clear (security review of PR #459, L1). Each attempt of a command
// or a worker on the entity calls it before its transaction begins; it reads the catalog on the pool at most once each
// sealRecheck, and a catalog that it cannot read changes nothing. The reset logs once, with no value.
func (e *Entity[T]) recheckSeal(ctx context.Context, db *sql.DB) {
	a := e.spec.AfterSeal
	if a == nil || db == nil || !e.contracted.Load() {
		return
	}
	now, next := time.Now().UnixNano(), e.recheckAt.Load()
	if now < next || !e.recheckAt.CompareAndSwap(next, now+int64(sealRecheck)) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogWait)
	defer cancel()
	for i, column := range a.Clear {
		var there, checkpoint bool
		if err := db.QueryRowContext(ctx, clearThereQuery, a.Table, column).Scan(&there); err != nil {
			return
		}
		if !there {
			continue
		}
		if err := db.QueryRowContext(ctx, checkpointThereQuery, a.Changes[i]).Scan(&checkpoint); err != nil || !checkpoint {
			if e.strayLogged.CompareAndSwap(false, true) {
				slog.ErrorContext(ctx, "command: a clear column of a contracted seal is in its table with no seal checkpoint; the entity keeps writing the sealed column alone, so no secret goes to the clear column; drop the column, or restore the backup with its checkpoint",
					"entity", e.spec.Name, "table", a.Table, "column", column, "change", a.Changes[i])
			}
			return
		}
		if e.contracted.CompareAndSwap(true, false) {
			e.strayLogged.Store(false)
			slog.WarnContext(ctx, "command: a clear column of an open seal is in its table again, as after a restore from before the contract step; the entity writes and reads the clear and the sealed columns again",
				"entity", e.spec.Name, "table", a.Table, "column", column, "change", a.Changes[i])
		}
		return
	}
}

// checkpointThereQuery tells whether the seal checkpoint row of the change $1 is there. A database with no seal
// checkpoint function gives an error, which recheckSeal takes as no row.
const checkpointThereQuery = `SELECT EXISTS (SELECT 1 FROM stego_schema.seal_read($1))`

// recheckSeals calls recheckSeal for each entity that an attempt writes, before the attempt begins its transaction,
// so the catalog check holds no second connection of the pool beside it (security review of PR #459, I1).
func recheckSeals(ctx context.Context, rt *Runtime, writes map[Declared]bool) {
	for d := range writes {
		d.recheckSeal(ctx, rt.cfg.DB)
	}
}

// clearThereQuery tells whether the table $1 is there and has the column $2.
const clearThereQuery = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)
 AND a.attname OPERATOR(pg_catalog.=) $2 AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped)`

// sealTable gives the table of a seal column with its schema, each name quoted, such as public."patients": the
// tables of the entities are in the schema public.
func sealTable(table string) string {
	return `public."` + strings.ReplaceAll(table, `"`, `""`) + `"`
}

// clearGoneQuery tells whether the table $1 is there and has no column $2.
const clearGoneQuery = `SELECT pg_catalog.to_regclass($1) IS NOT NULL AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
 WHERE a.attrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1) AND a.attname OPERATOR(pg_catalog.=) $2 AND a.attnum OPERATOR(pg_catalog.>) 0 AND NOT a.attisdropped)`

// sealSwitched is the conflict of a call whose statement failed because the contract step of a seal dropped a clear
// column: the entity took its statements after the contract, and Run runs the call again once, also when the command
// declares no Retry on conflict (ST-SUP-RP10-LC49).
func sealSwitched(err error) bool {
	var c *conflictError
	return errors.As(err, &c) && c.sealSwitch
}

// rerunsOnce tells whether Run runs the call again once with no wait, also when the command declares no Retry on
// conflict: after a seal switch (sealSwitched), and after a stale prepared statement, which the pools dropped, so the
// next attempt prepares the statement again (ST-SUP-RP04-LC11).
func rerunsOnce(err error) bool {
	var c *conflictError
	return sealSwitched(err) || (errors.As(err, &c) && c.stalePlan)
}

// failOf is fail for a statement of the entity e: when the contract step of an open seal of e dropped its clear
// columns (switchAfterSeal), the call is a conflict that Run runs again with the statements after the contract.
func (s *state) failOf(ctx context.Context, e sealSwitch, step string, err error) error {
	if e != nil && e.switchAfterSeal(ctx, s.rt.cfg.DB, err, s.sealSwitches) {
		if s.conflict == nil {
			s.conflict = &conflictError{text: step + " ran before the entity took its statements after the seal contract", sealSwitch: true}
		}
		return s.conflict
	}
	return s.fail(ctx, step, err)
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
	// contracted holds the changes whose checkpoint row and clear column are gone, as after the contract step, which
	// the job logged.
	contracted map[string]bool
	// stray holds the changes with no checkpoint row whose clear column is there, which the job logged once (security
	// review of PR #459, I4).
	stray map[string]bool
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
	j := &SealJob{db: db, schema: schema, ring: ring, columns: columns, holder: holder, settings: settings, pause: SealPause, contracted: map[string]bool{}, stray: map[string]bool{}}
	j.tenants = func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, db, schema) }
	return j, nil
}

// SealLease gives the lease of the seal job of a change: the backfill lease stego.backfill.<change ID>.
func SealLease(change string) string { return "stego.backfill." + change }

// ErrSealLease is the failure of a run of the seal job for a change whose lease row is not in the lease table: the
// migration job of the release makes it (ST-SUP-RP10-LC50, ST-SUP-RC01-OC49).
var ErrSealLease = errors.New("the lease of the seal job is not in the lease table; run the migration job of this release")

// ErrSealCheckpoint is the failure of a run of the seal job for a change with no checkpoint row while the clear
// column is still in its table: the migration job of the release makes the row, and the contract step deletes it only
// with the clear column (security review of PR #452, I2).
var ErrSealCheckpoint = errors.New("the seal has no checkpoint and its clear column is there; run the migration job of this release")

// leaseDeclared tells whether the lease table holds the row of the lease $1.
const leaseDeclared = `SELECT EXISTS (SELECT 1 FROM public.leases l WHERE l.name OPERATOR(pg_catalog.=) $1)`

// Run runs the pass of each change under its lease until the pass ends, the lease ends or ctx ends, and logs, for
// each change, the rows that it sealed and checked and whether the pass completed, and no value. A change whose pass
// completed and is not stale has no work.
func (j *SealJob) Run(ctx context.Context) ([]SealResult, error) {
	var results []SealResult
	var failed []error
	for _, c := range j.columns {
		result := SealResult{Change: c.Change}
		// The contract step of the change deletes its checkpoint row with the clear column: the release of the
		// expand step then has no work for it, and runs no statement on its columns (ST-SUP-RP10-LC50).
		if _, err := lease.ReadSeal(ctx, j.db, c.Change); errors.Is(err, lease.ErrNoSeal) {
			// The job takes the change as contracted only when the catalog shows the clear column gone: with the
			// column there, the pass is not complete, and the run names the remedy (security review of PR #452, I2).
			var there bool
			if err := j.db.QueryRowContext(ctx, clearThereQuery, sealTable(c.Table), c.Clear).Scan(&there); err != nil {
				failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: read the catalog: %w", c.Clear, c.Entity, err))
				results = append(results, result)
				continue
			}
			if there {
				if !j.stray[c.Change] {
					j.stray[c.Change] = true
					slog.ErrorContext(ctx, "command: the seal of a secret column has no checkpoint while its clear column is there, so the seal job cannot run; run the migration job of this release, which makes the checkpoint",
						"change", c.Change, "entity", c.Entity, "column", c.Clear)
				}
				failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: %w", c.Clear, c.Entity, ErrSealCheckpoint))
				results = append(results, result)
				continue
			}
			if !j.contracted[c.Change] {
				j.contracted[c.Change] = true
				slog.InfoContext(ctx, "command: the seal of a secret column has no checkpoint, as after its contract step; the seal job has no work for it",
					"change", c.Change, "entity", c.Entity)
			}
			results = append(results, SealResult{Change: c.Change, Completed: true})
			continue
		} else if err != nil {
			failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: read the checkpoint: %w", c.Clear, c.Entity, err))
			continue
		}
		// The checkpoint row is there, also again after a restore of a backup from before the contract step: the
		// next run with no checkpoint logs it again (security review of PR #452, L1).
		delete(j.contracted, c.Change)
		delete(j.stray, c.Change)
		// The migration job makes the lease row (ST-SUP-RC01-OC49). With no row, a hold would wait for ever, so the
		// run names the lease and the remedy, and fails (walkthrough 14, W14-B7).
		var declared bool
		if err := j.db.QueryRowContext(ctx, leaseDeclared, SealLease(c.Change)).Scan(&declared); err != nil {
			failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: read the lease %s: %w", c.Clear, c.Entity, SealLease(c.Change), err))
			continue
		}
		if !declared {
			slog.ErrorContext(ctx, "command: the lease of the seal job is not in the lease table, so the seal job cannot run; run the migration job of this release, which makes it, and the job runs at its next tick",
				"change", c.Change, "entity", c.Entity, "lease", SealLease(c.Change))
			failed = append(failed, fmt.Errorf("command: the seal of the column %s of %s: %w: %s", c.Clear, c.Entity, ErrSealLease, SealLease(c.Change)))
			continue
		}
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

// inTx runs fn in one transaction that holds the lease row of held (lease.Held.WithToken), which commits it: the
// checkpoint functions need no tenant, and the command runtime commits a transaction of a tenant only through
// tenant.Commit (ST-SUP-RP10-LC30).
func inTx(ctx context.Context, held *lease.Held, fn func(tx *sql.Tx) error) error {
	return held.WithToken(ctx, func(ctx context.Context, tx *sql.Tx, _ int64) error { return fn(tx) })
}

// pass goes on with the pass of the change c from its checkpoint under held, and starts a new pass when there is
// none, a release before the step ran during it, or it started in another database instance, as before a restore or
// a restart of the database server: such a pass, also a completed one, reads as stale.
func (j *SealJob) pass(ctx context.Context, held *lease.Held, c SealColumn) (SealResult, error) {
	result := SealResult{Change: c.Change}
	var state lease.SealState
	if err := inTx(ctx, held, func(tx *sql.Tx) error {
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
		// A start that waits commits too: its first start in this database sets the guard time of the row, which a
		// dump does not carry, and makes a pass of another instance, also a completed one, not started (security
		// review of PR #470, sec-470, and of PR #471, L3).
		err := inTx(ctx, held, func(tx *sql.Tx) error {
			err := lease.SealCheckpoint(ctx, tx, held, c.Change, "", "", 0, 0, true)
			if errors.Is(err, lease.ErrSealWaiting) {
				result.Waits = true
				return nil
			}
			return err
		})
		if err != nil {
			return result, err
		}
		if result.Waits {
			return result, nil
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
	err = inTx(ctx, held, func(tx *sql.Tx) error {
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
