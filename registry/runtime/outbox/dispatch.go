package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/910-software-solutions/stego/registry/runtime/internal/seal"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// State is a dispatch state of a message (ST-SUP-RP01-LC08).
type State string

// The seven dispatch states.
const (
	Pending        State = "pending"
	Dispatching    State = "dispatching"
	OutcomeUnknown State = "outcome_unknown"
	Reconciling    State = "reconciling"
	Done           State = "done"
	Parked         State = "parked"
	Discarded      State = "discarded"
)

// States returns the seven dispatch states.
func States() []State {
	return []State{Pending, Dispatching, OutcomeUnknown, Reconciling, Done, Parked, Discarded}
}

// transitions are the 15 allowed transitions of ST-SUP-RP01-LC08. Parked to done, to pending and to discarded are
// taken only by a person, through Resolve.
var transitions = map[State][]State{
	Pending:        {Dispatching},
	Dispatching:    {Done, Pending, OutcomeUnknown, Parked},
	OutcomeUnknown: {Dispatching, Reconciling, Parked},
	Reconciling:    {Done, Pending, Parked, Reconciling},
	Parked:         {Done, Pending, Discarded},
}

// Transition returns nil when a message may move from the state from to the state to, and otherwise an error that
// names both states (ST-SUP-RP01-LC08).
func Transition(from, to State) error {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("outbox: a message in %s cannot move to %s", from, to)
}

// mark is how a handler marks an error.
type mark int

const (
	markNotSent mark = iota + 1
	markTransientRefusal
	markPermanentRefusal
	markConflict
	markNotUnderstood
)

type markedError struct {
	mark mark
	err  error
}

func (e *markedError) Error() string {
	if e.err == nil {
		return "outbox: marked error"
	}
	return e.err.Error()
}

func (e *markedError) Unwrap() error { return e.err }

func markOf(err error) mark {
	var marked *markedError
	if errors.As(err, &marked) {
		return marked.mark
	}
	return 0
}

// NotSent marks err as a failure before the request left the process, so the destination cannot have acted. The
// message goes back to pending (ST-SUP-RP01-LC09).
func NotSent(err error) error { return &markedError{mark: markNotSent, err: err} }

// TransientRefusal marks err as a refusal of the destination that may pass, such as a 503. The message goes back
// to pending (ST-SUP-RP01-LC09).
func TransientRefusal(err error) error { return &markedError{mark: markTransientRefusal, err: err} }

// PermanentRefusal marks err as a refusal of the destination that a retry does not change, such as a 400. The
// message is parked (ST-SUP-RP01-LC09, ST-SUP-RP01-LC12).
func PermanentRefusal(err error) error { return &markedError{mark: markPermanentRefusal, err: err} }

// NotUnderstood marks err as a message that the handler of this release cannot read, such as an authz tuple of a
// relation that a later release added to the schema: the handler did nothing, and a handler of a newer release, in
// another worker of the same queue, may read it. The message goes back to pending with no attempt counted and the
// wait of an unknown destination, for a worker that reads it, until it is older than the not-understood bound of its
// destination; then it is parked with the cause not_understood and an alert (ST-SUP-RP01-LC40). A handler MUST give
// it only before it acts, and never for a message that no release can read, which is a permanent refusal.
func NotUnderstood(err error) error { return &markedError{mark: markNotUnderstood, err: err} }

// IsNotUnderstood reports whether err is marked as a message that the handler cannot read.
func IsNotUnderstood(err error) bool { return markOf(err) == markNotUnderstood }

// Conflict marks err as a conflict of a consumer, such as the error that RunConsumer returns (ST-SUP-RP10-LC26). The
// message goes back to pending with a backoff, and the first 20 conflicts in a row count no attempt
// (ST-SUP-RP01-LC18).
func Conflict(err error) error { return &markedError{mark: markConflict, err: err} }

// WithFailureCode gives err the failure code code: when the attempt ends in pending or parked as a refusal or as not
// sent, the queue keeps code for the message in place of the code of its mark, such as poison in place of
// delivery-refused-permanent, and the next claim of the message's destination gives it back as
// Delivery.FailureCode (ST-SUP-RC35-OC06). A code that is not a short label of letters, digits, '.', '_' and '-' is
// not kept. The code must never hold an error text or a value of the message (ST-SUP-RP01-LC06).
func WithFailureCode(err error, code string) error { return &codedError{err: err, code: code} }

type codedError struct {
	err  error
	code string
}

func (e *codedError) Error() string {
	if e.err == nil {
		return "outbox: coded error"
	}
	return e.err.Error()
}

func (e *codedError) Unwrap() error { return e.err }

// codeOf gives the failure code that WithFailureCode gave err, when it is a label.
func codeOf(err error) (string, bool) {
	var coded *codedError
	if errors.As(err, &coded) && labelPattern.MatchString(coded.code) {
		return coded.code, true
	}
	return "", false
}

// IsConflict reports whether err is marked as a conflict.
func IsConflict(err error) bool { return markOf(err) == markConflict }

// IsPermanent reports whether err is marked as a permanent refusal, which parks the message.
func IsPermanent(err error) bool { return markOf(err) == markPermanentRefusal }

// The conflict backoff of ST-SUP-RP01-LC18.
const (
	ConflictWaitMin  = 100 * time.Millisecond
	ConflictWaitMax  = 30 * time.Second
	FreeConflicts    = 20
	DefaultLookups   = 10
	conflictJitterOf = 10 // the jitter is at most one tenth of the wait
)

// conflictWait is the wait after the conflict number n in a row (n from 1): 100 ms that doubles at each conflict up
// to 30 s, with a random jitter of at most a tenth of the wait that never takes it past 30 s.
func conflictWait(n int64) time.Duration {
	wait := ConflictWaitMin
	for i := int64(1); i < n && wait < ConflictWaitMax; i++ {
		wait *= 2
	}
	if wait >= ConflictWaitMax {
		return ConflictWaitMax - time.Duration(rand.Int64N(int64(ConflictWaitMax/conflictJitterOf)+1))
	}
	return wait + time.Duration(rand.Int64N(int64(wait/conflictJitterOf)+1))
}

// outcome is the end of one attempt.
type outcome struct {
	to          State
	failureCode string
	conflict    bool
	notSent     bool // no request reached the destination: the attempt is not counted
	// notUnderstood is true when the handler cannot read the message, which a handler of a newer release may read
	// (ST-SUP-RP01-LC40).
	notUnderstood bool
}

// classify gives the state that an attempt ends in (ST-SUP-RP01-LC09). A conflict ends in pending, also when the
// deadline has passed. Otherwise a passed deadline or an error with no mark ends in outcome unknown, because the
// request may have reached the destination; only a success ends in done.
func classify(err error, expired bool) outcome {
	switch m := markOf(err); {
	case errors.Is(err, errHandlerPanic):
		return outcome{to: OutcomeUnknown, failureCode: "handler-panic"}
	case err != nil && m == markConflict:
		return outcome{to: Pending, failureCode: "delivery-conflict", conflict: true}
	case expired:
		return outcome{to: OutcomeUnknown, failureCode: "delivery-timeout"}
	case err == nil:
		return outcome{to: Done}
	case m == markNotSent:
		return outcome{to: Pending, failureCode: coded(err, "delivery-not-sent")}
	case m == markTransientRefusal:
		return outcome{to: Pending, failureCode: coded(err, "delivery-refused")}
	case m == markPermanentRefusal:
		return outcome{to: Parked, failureCode: coded(err, "delivery-refused-permanent")}
	case m == markNotUnderstood:
		return outcome{to: Pending, failureCode: coded(err, NotUnderstoodCode), notUnderstood: true}
	default:
		return outcome{to: OutcomeUnknown, failureCode: "delivery-failed"}
	}
}

// coded gives the failure code that the handler gave err with WithFailureCode, or fallback.
func coded(err error, fallback string) string {
	if code, ok := codeOf(err); ok {
		return code
	}
	return fallback
}

// errHandlerPanic stands for a handler or lookup that panicked. It keeps no part of the panic value, which can hold
// the payload or a credential.
var errHandlerPanic = errors.New("outbox: the handler panicked")

// callHandler calls handler in the message's tenant transaction tx and turns a panic into errHandlerPanic, so the
// request may have reached the destination, the message ends in outcome unknown, and the worker keeps running
// (ST-SUP-RP01-LC27).
func callHandler(ctx context.Context, handler Handler, tx *sql.Tx, d Delivery) (err error) {
	defer func() {
		if recover() != nil {
			err = errHandlerPanic
		}
	}()
	return handler(ctx, tx, d)
}

// callLookup calls lookup and turns a panic into an error, which keeps the message in reconciling.
func callLookup(ctx context.Context, lookup Lookup, request LookupRequest) (status LookupStatus, err error) {
	defer func() {
		if recover() != nil {
			status, err = StatusUnknown, errHandlerPanic
		}
	}()
	return lookup(ctx, request)
}

// LookupStatus is the answer of a status lookup.
type LookupStatus int

// The three answers of a status lookup (ST-SUP-RP01-LC11).
const (
	StatusUnknown LookupStatus = iota
	StatusActed
	StatusNotActed
)

// LookupRequest is what a status lookup gets: the message identity and its outgoing key.
type LookupRequest struct {
	ID          uuid.UUID
	OutgoingKey string
	Destination string
	Tenant      tenant.Tenant
}

// Lookup asks a destination whether it acted on a message (ST-SUP-RP01-LC11).
type Lookup func(ctx context.Context, request LookupRequest) (LookupStatus, error)

// StateChange is one change of a message's state that a destination's state hook gets (ST-SUP-RP01-LC25).
type StateChange struct {
	ID          uuid.UUID
	Group       string
	Destination string
	Tenant      tenant.Tenant
	State       State
}

// StateHook follows the state of a destination's messages. The worker calls it in the transaction of each change of
// a message to done, parked, pending from parked, or discarded, in the message's tenant; an error rolls the change
// back (ST-SUP-RP01-LC25). It must not commit.
type StateHook func(ctx context.Context, tx *sql.Tx, change StateChange) error

// hooked reports whether a change from the state from to the state to calls the state hook.
func hooked(from, to State) bool {
	switch to {
	case Done, Parked, Discarded:
		return true
	case Pending:
		return from == Parked
	}
	return false
}

// step takes the next step for one claimed message.
func (w *Worker) step(ctx context.Context, d Delivery) {
	reg, ok := w.destinations[d.Destination]
	switch d.Receipt.State {
	case Dispatching:
		if !ok {
			// No request is sent: the attempt is taken back (ST-SUP-RP01-LC22).
			w.unknownDestinations.Add(1)
			w.finish(ctx, d, reg, Pending, change{attempts: -1, delay: w.unknownDelay(), failureCode: "destination-unknown"})
			return
		}
		w.dispatch(ctx, d, reg)
	case OutcomeUnknown:
		if !ok {
			w.unknownDestinations.Add(1)
			w.releaseLease(ctx, d, w.unknownDelay(), "destination-unknown")
			return
		}
		w.afterUnknown(ctx, d, reg)
	case Reconciling:
		if !ok {
			w.unknownDestinations.Add(1)
			w.releaseLease(ctx, d, w.unknownDelay(), "destination-unknown")
			return
		}
		w.reconcile(ctx, d, reg)
	}
}

// exhausted reports whether the attempts of d reached the limit for a destination that may park on it: a
// destination registered as must land is never parked for its attempt count (ST-SUP-RP01-LC12, ST-SUP-RP01-LC23).
func (w *Worker) exhausted(d Delivery, reg Destination) bool {
	return !reg.MustLand && d.Attempts >= w.config.MaxAttempts
}

// dispatch sends a dispatching message to its handler in a transaction of the message's tenant, and moves it to the
// state the attempt ends in. A success moves it to done in the handler's transaction and commits both together; any
// other end rolls the handler's transaction back and moves the message in a new one (ST-SUP-RP01-LC19).
func (w *Worker) dispatch(ctx context.Context, d Delivery, reg Destination) {
	if ctx.Err() != nil {
		// The worker stops before it sends: the attempt is taken back (ST-SUP-RP01-LC22).
		w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "worker-stopping"})
		return
	}
	// The handler runs only while the lease, measured on the local monotonic clock from the claim, holds the attempt
	// and the two statements after it; else the message goes back to pending unsent (ST-SUP-RP01-LC02).
	left := d.leaseEnd.Sub(w.now())
	if left < w.config.AttemptTimeout+2*operationTimeout {
		w.lostLeases.Add(1)
		w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "lease-short"})
		return
	}
	// A destination that serves only active tenants gets no message of a tenant that the list does not hold as
	// active, such as a retired tenant: it is parked before the handler runs (ST-SUP-RC27-OC12).
	if reg.ActiveTenants && w.config.Tenants != nil {
		checkCtx, checkCancel := context.WithTimeout(ctx, operationTimeout)
		active, err := w.config.Tenants.Active(checkCtx, d.Tenant)
		checkCancel()
		switch {
		case err != nil:
			w.databaseFailures.Add(1)
			w.finish(ctx, d, reg, Pending, change{attempts: -1, delay: w.retryDelay(d.Attempts), failureCode: "tenant-list-failed"})
			return
		case !active:
			w.park(ctx, d, reg, change{attempts: -1, failureCode: "tenant-not-active"})
			return
		}
	}
	// The transaction, its begin included, ends with the lease, so nothing of the attempt commits after the lease
	// is gone.
	txCtx, txCancel := context.WithTimeout(context.WithoutCancel(ctx), left)
	defer txCancel()
	tx, err := tenant.Begin(txCtx, w.queue.db, d.Tenant, nil)
	if err == nil {
		if left = d.leaseEnd.Sub(w.now()); left < w.config.AttemptTimeout+2*operationTimeout {
			_ = tx.Rollback()
			w.lostLeases.Add(1)
			w.finish(ctx, d, reg, Pending, change{attempts: -1, failureCode: "lease-short"})
			return
		}
	}
	if err != nil {
		// No request is sent, but a failed begin is not one of the takebacks of ST-SUP-RP01-LC22.
		w.databaseFailures.Add(1)
		w.finish(ctx, d, reg, Pending, change{delay: w.retryDelay(d.Attempts), failureCode: "database-failed"})
		return
	}
	deadline := min(w.config.AttemptTimeout, left-2*operationTimeout)
	attempt, cancel := w.deadline(ctx, deadline)
	err = callHandler(attempt, reg.Handler, tx, d)
	expired := errors.Is(attempt.Err(), context.DeadlineExceeded)
	cancel()
	result := classify(err, expired)
	if result.to == Done {
		w.apply(ctx, tx, d, reg, Done, change{})
		return
	}
	_ = tx.Rollback()
	if result.notUnderstood {
		w.leave(ctx, d, reg, result.failureCode)
		return
	}
	switch result.to {
	case Pending:
		if result.conflict {
			w.conflicts.Add(1)
			n := d.Conflicts + 1
			c := change{conflict: true, delay: conflictWait(n), failureCode: result.failureCode}
			if n <= FreeConflicts {
				c.attempts = -1
			} else if w.exhausted(d, reg) {
				w.park(ctx, d, reg, change{conflict: true, failureCode: "attempts-exhausted"})
				return
			}
			w.finish(ctx, d, reg, Pending, c)
			return
		}
		w.deliveryFailures.Add(1)
		if w.exhausted(d, reg) {
			w.park(ctx, d, reg, change{failureCode: "attempts-exhausted"})
			return
		}
		w.finish(ctx, d, reg, Pending, change{delay: w.retryDelay(d.Attempts), failureCode: result.failureCode})
	case OutcomeUnknown:
		w.deliveryFailures.Add(1)
		w.unknownOutcomes.Add(1)
		w.finish(ctx, d, reg, OutcomeUnknown, change{failureCode: result.failureCode})
	case Parked:
		w.deliveryFailures.Add(1)
		w.park(ctx, d, reg, change{failureCode: result.failureCode})
	}
}

// afterUnknown takes the next step of a message in outcome unknown (ST-SUP-RP01-LC10, ST-SUP-RP01-LC11,
// ST-SUP-RP01-LC12): it sends the message again with the same outgoing key only to an idempotent destination, moves
// it to reconciling for a destination with a status lookup, due after the attempt deadline plus the settle time,
// and parks it otherwise.
func (w *Worker) afterUnknown(ctx context.Context, d Delivery, reg Destination) {
	switch {
	case reg.Idempotent:
		if ctx.Err() != nil {
			// The worker stops: the message keeps its outcome unknown for the next claim.
			w.releaseLease(ctx, d, 0, "worker-stopping")
			return
		}
		if w.exhausted(d, reg) {
			w.park(ctx, d, reg, change{failureCode: "attempts-exhausted"})
			return
		}
		sent := w.now()
		ok, m := w.resend(ctx, &d)
		if !ok {
			return
		}
		d.leaseEnd = sent.Add(w.config.LeaseDuration)
		d.Attempts = m.attempts
		d.Conflicts = m.conflicts
		w.dispatch(ctx, d, reg)
	case reg.Lookup != nil:
		due := d.DispatchedAt.Add(w.config.AttemptTimeout + reg.SettleTime)
		w.finish(ctx, d, reg, Reconciling, change{dueAt: &due, failureCode: "outcome-unknown"})
	default:
		w.park(ctx, d, reg, change{failureCode: "outcome-unknown"})
	}
}

// resend moves a message in outcome unknown back to dispatching with a new lease and one more attempt, in a
// transaction of its tenant.
func (w *Worker) resend(ctx context.Context, d *Delivery) (bool, moved) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	tx, err := tenant.Begin(finish, w.queue.db, d.Tenant, nil)
	if err != nil {
		w.databaseFailures.Add(1)
		return false, moved{}
	}
	defer func() { _ = tx.Rollback() }()
	ok, m, err := w.queue.move(finish, tx, &d.Receipt, Dispatching, change{attempts: 1, failureCode: "resend",
		lease: w.config.LeaseDuration})
	if !w.counted(ok, err) {
		return false, moved{}
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
		return false, moved{}
	}
	return true, m
}

// reconcile asks the status lookup of a reconciling message and moves it as the answer says (ST-SUP-RP01-LC11).
func (w *Worker) reconcile(ctx context.Context, d Delivery, reg Destination) {
	if reg.Lookup == nil {
		w.park(ctx, d, reg, change{failureCode: "outcome-unknown"})
		return
	}
	if ctx.Err() != nil {
		w.releaseLease(ctx, d, 0, "worker-stopping")
		return
	}
	attempt, cancel := w.deadline(ctx, w.config.AttemptTimeout)
	status, err := callLookup(attempt, reg.Lookup, LookupRequest{ID: d.ID, OutgoingKey: d.OutgoingKey,
		Destination: d.Destination, Tenant: d.Tenant})
	cancel()
	w.lookups.Add(1)
	switch {
	case err == nil && status == StatusActed:
		w.finish(ctx, d, reg, Done, change{lookup: true})
	case err == nil && status == StatusNotActed:
		w.finish(ctx, d, reg, Pending, change{lookup: true, failureCode: "lookup-not-acted"})
	default:
		if d.Lookups+1 >= int64(w.config.LookupBound) {
			w.park(ctx, d, reg, change{lookup: true, failureCode: "lookups-exhausted"})
			return
		}
		code := "lookup-unknown"
		if err != nil {
			code = "lookup-failed"
		}
		w.finish(ctx, d, reg, Reconciling, change{lookup: true, delay: w.retryDelay(d.Lookups + 1), failureCode: code})
	}
}

// finish moves a claimed message under its lease in a new transaction of its tenant.
func (w *Worker) finish(ctx context.Context, d Delivery, reg Destination, to State, c change) {
	w.apply(ctx, nil, d, reg, to, c)
}

// apply moves a claimed message under its lease in tx, or in a new transaction of the message's tenant when tx is
// nil, calls the destination's state hook in the same transaction, and commits with tenant.Commit. A failed move,
// hook or commit rolls everything back and changes nothing. On done it records the delivery delay
// (ST-SUP-RP01-LC07); on parked it raises the alert (ST-SUP-RP01-LC12). It reports whether the move committed.
func (w *Worker) apply(ctx context.Context, tx *sql.Tx, d Delivery, reg Destination, to State, c change) bool {
	// A completed delivery is still recorded during shutdown; a database result that is not known keeps the lease.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	from := d.Receipt.State
	if tx == nil {
		var err error
		if tx, err = tenant.Begin(finish, w.queue.db, d.Tenant, nil); err != nil {
			w.databaseFailures.Add(1)
			return false
		}
	}
	defer func() { _ = tx.Rollback() }()
	ok, m, err := w.queue.move(finish, tx, &d.Receipt, to, c)
	if !w.counted(ok, err) {
		return false
	}
	if reg.StateHook != nil && hooked(from, to) {
		if err := callHook(finish, reg.StateHook, tx, StateChange{ID: d.ID, Group: d.Group, Destination: d.Destination,
			Tenant: d.Tenant, State: to}); err != nil {
			_ = tx.Rollback()
			// The rollback undid the move: the message is in its old state under the same lease.
			d.Receipt.State = from
			w.hookFailed(ctx, d, reg, from)
			return false
		}
	}
	var freed []freedMessage
	if to == Done {
		var err error
		if freed, err = w.freeSuccessors(finish, tx, d, reg); err != nil {
			_ = tx.Rollback()
			d.Receipt.State = from
			w.databaseFailures.Add(1)
			// The done of d rolls back with it, so a failed audit write holds the key: it is counted and alerted
			// (ST-SUP-RP01-LC39; security review of PR #457, I1).
			w.metrics.freeFailed(finish, d.Destination)
			return false
		}
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
		return false
	}
	for _, f := range freed {
		w.freed.Add(1)
		w.metrics.freedSuccessor(finish, d.Destination, f.id)
	}
	switch to {
	case Done:
		w.delivered.Add(1)
		w.metrics.delivered(finish, d.Destination, m.delay)
	case Pending:
		w.retried.Add(1)
	case Parked:
		w.parked.Add(1)
		w.metrics.parked(finish, d.Destination, from, parkCause(c), c.failureCode)
	}
	return true
}

// freedMessage is one message that freeSuccessors moved back to pending: its ID and its group.
type freedMessage struct {
	id    uuid.UUID
	group string
}

// freeSuccessors moves, in tx, each later message of the key of d that is parked as poison back to pending, through
// the relay function free_successors, when d is a message of the Kafka inbox that is now done (ST-SUP-RP01-LC39). It
// calls the state hook of each and writes the audit row of each resolution not_acted in tx, so all commit with the
// move of d or not at all. The subject of each resolution is the resolver that the destination is registered with
// (SetResolver), never a column of the row (security review of PR #457, L2). A worker with no auditor, or a destination
// with no resolver, frees nothing, because it cannot write the audit row.
func (w *Worker) freeSuccessors(ctx context.Context, tx *sql.Tx, d Delivery, reg Destination) ([]freedMessage, error) {
	w.mu.Lock()
	by := w.resolvers[d.Destination]
	w.mu.Unlock()
	if w.auditor == nil || by == "" {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT r_id, r_group FROM stego_outbox.free_successors($1::timestamptz, $2::uuid, $3::text)`,
		w.queue.now(), d.ID, by)
	if err != nil {
		return nil, canceled(ctx, err)
	}
	var freed []freedMessage
	for rows.Next() {
		var f freedMessage
		if err := rows.Scan(&f.id, &f.group); err != nil {
			_ = rows.Close()
			return nil, err
		}
		freed = append(freed, f)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, canceled(ctx, err)
	}
	var at time.Time
	if len(freed) > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE($1::timestamptz, pg_catalog.clock_timestamp())`, w.queue.now()).Scan(&at); err != nil {
			return nil, canceled(ctx, err)
		}
	}
	for _, f := range freed {
		if reg.StateHook != nil {
			if err := callHook(ctx, reg.StateHook, tx, StateChange{ID: f.id, Group: f.group, Destination: d.Destination,
				Tenant: d.Tenant, State: Pending}); err != nil {
				return nil, err
			}
		}
		if err := w.auditor.Record(ctx, tx, AuditEntry{MessageID: f.id, Tenant: d.Tenant, Destination: d.Destination,
			Subject: by, Resolution: NotActed, At: at}); err != nil {
			return nil, err
		}
	}
	return freed, nil
}

// SetResolver registers subject as the resolver of the parked messages of destination that a done message of the
// Kafka inbox frees (ST-SUP-RP01-LC39): the system principal of the consuming worker of the subscription, which the
// service gives from the inbox, so the audit row of each resolution names the worker's own identity (security review
// of PR #457, L2). It refuses a destination that is not registered and a subject that is not a text of 1 to 255 bytes.
func (w *Worker) SetResolver(destination, subject string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.destinations[destination]; !ok {
		return fmt.Errorf("outbox: the destination %s is not registered on this worker", destination)
	}
	if subject == "" || !validText(subject, MaxKeyBytes) {
		return errors.New("outbox: a resolver needs a subject")
	}
	if w.resolvers == nil {
		w.resolvers = map[string]string{}
	}
	w.resolvers[destination] = subject
	return nil
}

// park moves a claimed message to parked, records the state it left, and raises one alert event and metric that
// name the destination and that state (ST-SUP-RP01-LC12).
func (w *Worker) park(ctx context.Context, d Delivery, reg Destination, c change) {
	w.apply(ctx, nil, d, reg, Parked, c)
}

// NotUnderstoodCode is the failure code of a message that a handler cannot read, when the handler gives none
// (ST-SUP-RP01-LC40).
const NotUnderstoodCode = "not-understood"

// DefaultNotUnderstoodBound is the not-understood bound of a destination that gives none: a message that no handler
// reads within one hour of its enqueue is parked (ST-SUP-RP01-LC40). A rollout of a release takes minutes; a release
// that stays paused for longer needs a destination with a longer bound.
const DefaultNotUnderstoodBound = time.Hour

// leave takes the next step of a message that the handler cannot read (ST-SUP-RP01-LC40): it is not an attempt, so
// the count of the claim is taken back. A message younger than the not-understood bound of its destination goes back
// to pending with the wait of an unknown destination, so a worker of a newer release, which reads it, can claim it,
// and the later messages of its key keep their place behind it; an older one is parked with the cause
// not_understood and the alert of a park. A failed read of the age counts as younger: the message is never parked
// because of a read that failed.
func (w *Worker) leave(ctx context.Context, d Delivery, reg Destination, code string) {
	w.notUnderstood.Add(1)
	w.metrics.left(ctx, d.Destination)
	// The first hold of a message, whose last attempt did not end so, writes one WARN line, at most one a minute for
	// each destination (security review of PR #490, L2).
	if d.FailureCode != code {
		w.metrics.heldFirst(ctx, d.Destination, code, w.now())
	}
	bound := reg.NotUnderstoodBound
	if bound == 0 {
		bound = DefaultNotUnderstoodBound
	}
	age, err := w.queue.age(ctx, d)
	if err != nil {
		w.databaseFailures.Add(1)
	} else if age >= bound {
		w.park(ctx, d, reg, change{attempts: -1, failureCode: code, cause: CauseNotUnderstood})
		return
	}
	w.finish(ctx, d, reg, Pending, change{attempts: -1, delay: w.unknownDelay(), failureCode: code})
}

// The fixed cause classes of a parked message, which the alert of each park names (ST-SUP-RP01-LC12): the handler
// refused the message for good, no handler read it within the not-understood bound, its attempts or its status
// lookups are exhausted, its outcome is not known and its destination cannot learn it, or its tenant is not active.
const (
	CauseRefused           = "refused"
	CauseNotUnderstood     = "not_understood"
	CauseAttemptsExhausted = "attempts_exhausted"
	CauseLookupsExhausted  = "lookups_exhausted"
	CauseOutcomeUnknown    = "outcome_unknown"
	CauseTenantNotActive   = "tenant_not_active"
)

// ParkCauses returns the fixed cause classes of a parked message.
func ParkCauses() []string {
	return []string{CauseRefused, CauseNotUnderstood, CauseAttemptsExhausted, CauseLookupsExhausted, CauseOutcomeUnknown,
		CauseTenantNotActive}
}

// parkCause gives the fixed cause class of the park c: the class that the step set, else the class of the failure
// code of the worker, else refused, for a permanent refusal of the handler with a code of its own, such as poison.
func parkCause(c change) string {
	if c.cause != "" {
		return c.cause
	}
	switch c.failureCode {
	case "attempts-exhausted":
		return CauseAttemptsExhausted
	case "lookups-exhausted":
		return CauseLookupsExhausted
	case "outcome-unknown":
		return CauseOutcomeUnknown
	case "tenant-not-active":
		return CauseTenantNotActive
	}
	return CauseRefused
}

// releaseLease ends the lease of a claimed message in outcome unknown or reconciling and keeps its state, in a
// transaction of the message's tenant.
func (w *Worker) releaseLease(ctx context.Context, d Delivery, delay time.Duration, code string) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	defer cancel()
	tx, err := tenant.Begin(finish, w.queue.db, d.Tenant, nil)
	if err != nil {
		w.databaseFailures.Add(1)
		return
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := w.queue.release(finish, tx, d.Receipt, delay, code)
	if !w.counted(ok, err) {
		return
	}
	if err := tenant.Commit(finish, tx, d.Tenant); err != nil {
		w.databaseFailures.Add(1)
	}
}

// callHook calls a state hook and turns a panic into an error, so a hook never stops the worker.
func callHook(ctx context.Context, hook StateHook, tx *sql.Tx, change StateChange) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("outbox: the state hook panicked")
		}
	}()
	return hook(ctx, tx, change)
}

// hookFailed counts and alerts on a state hook that refused a change, which is rolled back (ST-SUP-RP01-LC25), and
// lets the message wait out a backoff instead of being claimed again at once. A message that was dispatching
// goes to outcome unknown, because its handler may have acted; one in outcome unknown or reconciling keeps its
// state.
func (w *Worker) hookFailed(ctx context.Context, d Delivery, reg Destination, from State) {
	w.hookFailures.Add(1)
	w.metrics.hookFailed(ctx, d.Destination)
	delay := w.retryDelay(max(d.Attempts, 1))
	switch from {
	case Dispatching:
		w.apply(ctx, nil, d, reg, OutcomeUnknown, change{delay: delay, failureCode: "hook-failed"})
	case OutcomeUnknown, Reconciling:
		w.releaseLease(ctx, d, delay, "hook-failed")
	}
}

// counted counts a database failure or a lost lease, and reports whether the move took place. A message that moved
// on from the receipt's state, such as one another holder finished, is a lost lease, not a database failure.
func (w *Worker) counted(ok bool, err error) bool {
	var movedOn *movedOnError
	if errors.As(err, &movedOn) {
		w.lostLeases.Add(1)
		return false
	}
	if err != nil {
		w.databaseFailures.Add(1)
		return false
	}
	if !ok {
		w.lostLeases.Add(1)
		return false
	}
	return true
}

// Resolution is the choice of a person for one parked message (ST-SUP-RP01-LC13).
type Resolution string

// The three resolutions.
const (
	Acted    Resolution = "acted"
	NotActed Resolution = "not_acted"
	Discard  Resolution = "discarded"
)

// resource is the prefix of the resource that Resolve asks the decision port about (ST-SUP-RP01-LC17).
const resource = "outbox_destination:"

// DecisionRequest is one question to the decision port: may Subject do Permission on Resource. The port decides at
// the consistency floor of ST-SUP-RP06-LC09: an outbox message carries no row token, so the floor is the service's
// revocation floor (ST-SUP-RP06-LC13), which the port holds.
type DecisionRequest struct {
	Subject    string
	Permission string
	Resource   string
	Tenant     tenant.Tenant
}

// Decider is the decision port that the worker is built with (ST-SUP-RP01-LC17). A deny is allowed false with no
// error; an error is not a decision.
type Decider interface {
	Decide(ctx context.Context, request DecisionRequest) (allowed bool, err error)
}

// AuditEntry is the audit row of one resolution.
type AuditEntry struct {
	MessageID   uuid.UUID
	Tenant      tenant.Tenant
	Destination string
	Subject     string
	Resolution  Resolution
	At          time.Time
}

// Auditor writes the audit row of a resolution through the caller's transaction tx, so it commits with the state
// change or not at all (ST-SUP-RP01-LC13). It must not commit.
type Auditor interface {
	Record(ctx context.Context, tx *sql.Tx, entry AuditEntry) error
}

// Principal is who resolves a message: a principal of the authorization runtime, such as authz.Person makes. Only
// the authorization runtime makes one with a subject (ST-SUP-RP01-LC17), so a caller cannot name a subject of its
// own choice.
type Principal = seal.Principal

// ResolveRequest is one resolution of a parked message by a person: the message, the destination that the caller
// names for it, such as the list of dead letters gives, the principal of the person and the chosen resolution.
type ResolveRequest struct {
	ID          uuid.UUID
	Destination string
	Principal   Principal
	Resolution  Resolution
}

// ErrNotAllowed is the error of Resolve when the decision port does not allow the principal to resolve messages of
// the named destination. Resolve decides before it reads the message, so the answer is the same for every message:
// one that is not there, one of another tenant or destination, and one in any state.
var ErrNotAllowed = errors.New("outbox: the subject may not resolve messages of this destination")

// ErrNotFound is the error of Resolve, after an allow, for a message that the transaction's tenant does not hold
// under the named destination.
var ErrNotFound = errors.New("outbox: the tenant of the transaction holds no message with this ID and destination")

// ErrNotParked is the error of Resolve, after an allow, for a message that is not parked.
var ErrNotParked = errors.New("outbox: only a parked message can be resolved")

// Resolve applies a person's resolution of one parked message in the caller's transaction tx (ST-SUP-RP01-LC13).
// It takes the subject only from a principal of the authorization runtime. Before it reads the message, it asks the
// decision port itself whether the subject has the permission resolve on outbox_destination:<destination>, for the
// destination that the request names and the tenant of tx, and refuses a deny, a port error or a worker with no port
// (ST-SUP-RP01-LC17); it takes no decision from its caller. So a refusal of the permission gives one answer for every
// message, and only an allowed caller learns whether the message is there and parked. acted moves the message to done, not acted to pending with the same
// identity and outgoing key, and discarded to discarded, which releases its key. For an idempotent destination it
// accepts only not acted, or discarded when the destination must land; discarded is refused for a destination that
// need not land. The audit row and the destination's state hook are written in tx with the state change. A refused
// resolution changes nothing. tx is a transaction of one tenant, which tenant.Begin opened; Resolve runs under the
// tenant policy, so it answers ErrNotFound for a message of another tenant (ST-SUP-RP01-LC19). The caller commits
// tx with tenant.Commit.
//
// Resolve returns a Resolved. The caller calls its Committed after its own commit of tx succeeds, and only then, so
// a message resolved as acted records its delivery delay only once it is done (ST-SUP-RP01-LC07).
func (w *Worker) Resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest) (Resolved, error) {
	var resolved Resolved
	err := w.resolve(ctx, tx, request, &resolved)
	if err != nil {
		return Resolved{}, err
	}
	return resolved, nil
}

// Resolved is a resolution that Resolve applied in the caller's transaction.
type Resolved struct {
	worker      *Worker
	destination string
	delay       time.Duration
	done        bool
}

// Committed records what the resolution changed once the caller's transaction committed: for a message resolved
// as acted, its delivery delay under its destination. A caller that rolls back does not call it. A second call
// records nothing.
func (r *Resolved) Committed(ctx context.Context) {
	if r == nil || r.worker == nil || !r.done {
		return
	}
	r.done = false
	r.worker.metrics.delivered(ctx, r.destination, r.delay)
}

func (w *Worker) resolve(ctx context.Context, tx *sql.Tx, request ResolveRequest, out *Resolved) error {
	if w.decider == nil {
		return errors.New("outbox: the worker has no decision port, so it refuses every resolution")
	}
	if w.auditor == nil {
		return errors.New("outbox: the worker has no auditor, so it refuses every resolution")
	}
	// A destination that is not registered on this worker is refused before the message is read.
	admit := func(destination string) error {
		w.mu.Lock()
		_, ok := w.destinations[destination]
		w.mu.Unlock()
		if !ok {
			return fmt.Errorf("outbox: the destination %s is not registered on this worker", destination)
		}
		return nil
	}
	// The worker resolves by the stricter of the row and the registration of the destination on it, as the
	// resolver does (security review of PR #421, L3).
	rules := func(destination string, row resolveRow) (resolveRules, error) {
		reg, ok := w.destinations[destination]
		if !ok {
			return resolveRules{}, fmt.Errorf("outbox: the destination %s is not registered on this worker", destination)
		}
		rules, err := stricter(destination, row, Rules{Name: reg.Name, MustLand: reg.MustLand, Idempotent: reg.Idempotent}, true)
		rules.hook = reg.StateHook
		return rules, err
	}
	hookFailed := func(ctx context.Context, destination string) {
		w.hookFailures.Add(1)
		w.metrics.hookFailed(ctx, destination)
	}
	seconds, to, err := resolveCore(ctx, tx, request, w.decider, w.auditor, w.queue.now(), admit, rules, hookFailed)
	if err != nil {
		return err
	}
	*out = Resolved{worker: w, destination: request.Destination, delay: time.Duration(seconds * float64(time.Second)),
		done: to == Done}
	return nil
}

// ErrRefusedOutcome is the error of a resolution, after an allow, whose outcome the destination of the message
// refuses: acted for an idempotent destination, and discarded for a destination that need not land
// (ST-SUP-RP01-LC13).
var ErrRefusedOutcome = errors.New("outbox: the destination of the message refuses this resolution")

// resolveRow is what the row of a parked message recorded at enqueue.
type resolveRow struct {
	mustLand   bool
	idempotent bool
}

// resolveRules are the rules of one destination that a resolution follows: whether it must land, whether it is
// idempotent, and its state hook.
type resolveRules struct {
	mustLand   bool
	idempotent bool
	hook       StateHook
}

// resolveCore applies one resolution in tx (ST-SUP-RP01-LC13, ST-SUP-RP01-LC17): it checks the request, asks the
// decision port before it reads the message, reads the message under the tenant policy, applies the rules that
// rules gives for the destination, changes the state through the relay function resolve, runs the state hook and
// writes the audit row last. It gives the age of the message in seconds and its new state.
func resolveCore(ctx context.Context, tx *sql.Tx, request ResolveRequest, decider Decider, auditor Auditor, now any,
	admit func(destination string) error, rules func(destination string, row resolveRow) (resolveRules, error), hookFailed func(ctx context.Context, destination string)) (float64, State, error) {
	subject, ok := seal.SubjectOf(request.Principal)
	if !ok || !validText(subject, MaxKeyBytes) {
		return 0, "", errors.New("outbox: a resolution needs a principal of the authorization runtime")
	}
	if tx == nil || request.ID == uuid.Nil || !labelPattern.MatchString(request.Destination) {
		return 0, "", errors.New("outbox: a resolution needs a transaction, a message ID and a destination name")
	}
	if request.Resolution != Acted && request.Resolution != NotActed && request.Resolution != Discard {
		return 0, "", fmt.Errorf("outbox: %q is not a resolution", request.Resolution)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	destination := request.Destination
	// The tenant of the decision is the tenant of tx, which tenant.Begin set; the message is read only after the
	// decision, under the tenant policy, so a message of another tenant is not found (ST-SUP-RP01-LC19).
	owner, err := scanTenant(ctx, tx, `SELECT pg_catalog.current_setting('app.tenant', true)`)
	if err != nil {
		return 0, "", err
	}
	allowed, err := decider.Decide(ctx, DecisionRequest{Subject: subject, Permission: "resolve",
		Resource: resource + destination, Tenant: owner})
	if err != nil {
		return 0, "", fmt.Errorf("outbox: the decision port failed, so the resolution is refused: %w", err)
	}
	if !allowed {
		return 0, "", ErrNotAllowed
	}
	// After the allow and before the read: a destination that admit refuses gets one answer for every ID.
	if err := admit(destination); err != nil {
		return 0, "", err
	}
	var state, group string
	var row resolveRow
	err = tx.QueryRowContext(ctx, `SELECT state, group_id, must_land, idempotent FROM stego_outbox.messages WHERE id = $1 AND destination = $2`,
		request.ID, destination).Scan(&state, &group, &row.mustLand, &row.idempotent)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", canceled(ctx, err)
	}
	if State(state) != Parked {
		return 0, "", ErrNotParked
	}
	reg, err := rules(destination, row)
	if err != nil {
		return 0, "", err
	}
	switch {
	case request.Resolution == Discard && !reg.mustLand:
		return 0, "", fmt.Errorf("%w: a message of %s, which need not land, cannot be discarded", ErrRefusedOutcome, destination)
	case request.Resolution == Acted && reg.idempotent:
		return 0, "", fmt.Errorf("%w: a message of the idempotent destination %s is resolved as not acted or discarded", ErrRefusedOutcome, destination)
	}
	to := map[Resolution]State{Acted: Done, NotActed: Pending, Discard: Discarded}[request.Resolution]
	if err := Transition(Parked, to); err != nil {
		return 0, "", err
	}
	var at time.Time
	var seconds float64
	// The state changes only through the relay function resolve, which requires the transaction's tenant
	// (ST-SUP-RP01-LC19).
	err = tx.QueryRowContext(ctx, `SELECT o_resolved_at, o_age_seconds FROM stego_outbox.resolve($1::timestamptz, $2::uuid, $3::text, $4::text, $5::text)`,
		now, request.ID, string(to), string(request.Resolution), subject).Scan(&at, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotParked
	}
	if err != nil {
		return 0, "", canceled(ctx, err)
	}
	if reg.hook != nil {
		if err := callHook(ctx, reg.hook, tx, StateChange{ID: request.ID, Group: group, Destination: destination,
			Tenant: owner, State: to}); err != nil {
			if hookFailed != nil {
				hookFailed(ctx, destination)
			}
			return 0, "", fmt.Errorf("outbox: the state hook of %s refused the resolution: %w", destination, err)
		}
	}
	// The audit row is the last write before the caller's commit, as the audit runtime asks (ST-SUP-RP05-LC12).
	if err := auditor.Record(ctx, tx, AuditEntry{MessageID: request.ID, Tenant: owner, Destination: destination,
		Subject: subject, Resolution: request.Resolution, At: at}); err != nil {
		return 0, "", fmt.Errorf("outbox: write the audit row of the resolution: %w", err)
	}
	return seconds, to, nil
}

// scanTenant reads the tenant of one row of one column that query gives in tx, through tenant.ScanRows, and refuses
// no row or an empty tenant.
func scanTenant(ctx context.Context, tx *sql.Tx, query string, args ...any) (tenant.Tenant, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return tenant.Tenant{}, canceled(ctx, err)
	}
	var t tenant.Tenant
	if rows.Next() {
		t, err = tenant.ScanRows(rows)
	} else {
		err = errors.New("outbox: the transaction has no tenant")
	}
	closeErr := rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return tenant.Tenant{}, canceled(ctx, err)
	}
	return t, nil
}
