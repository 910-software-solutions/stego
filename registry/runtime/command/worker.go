package command

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// ErrDeadline matches the error of a worker run that reached its deadline under a lease and rolled back before the
// lease can expire (ST-SUP-RP10-LC22); it is also a conflict: the tenant runs again in the next tick.
var ErrDeadline = errDeadline

// WorkerSpec is what the generated code declares of one worker.
type WorkerSpec[P any] struct {
	// Name is the worker's name: its service account is service_account:<Name> (ST-SUP-RP06-LC07), and it is the
	// action of its audit rows and the operation of its probe.
	Name string
	// Permission is the worker's declared permission, which its handle decides on each resource it touches
	// (ST-SUP-RP10-LC17).
	Permission string
	// Interval is the interval of a worker with an every trigger; its ticks are due at its multiples since the Unix
	// epoch (ST-SUP-RP10-LC21). It is zero for a worker with a consumes trigger.
	Interval time.Duration
	// Writes are the entities that the worker declares it writes.
	Writes []Declared
	// Reads are the read-only declarations of the entities that the worker declares it reads (EntitySpec ReadOnly),
	// which its fill reaches only through ReadRepo (ST-SUP-RP10-LC53, decision ST-SUP-Q186 (a)).
	Reads []Declared
	// Destinations are the outbox destinations of its effects.
	Destinations []Destination
	// Relations are the relations whose tuples its fill may write.
	Relations []authz.Relation
	// Stagers maps the name of each command or worker that stages a message for a consumed worker to its own
	// declared permission, which the consumer decides as the staging principal (ST-SUP-RP10-LC17).
	Stagers map[string]string
	// Probe wraps the probe of one call in the worker's typed probe.
	Probe func(*probe.Probe) P
	// ChangeEvents are the names of the change events of each entity that the worker writes, which the generator
	// gives when the service has the kafka-producer component, or none: each create, update and delete of the
	// handle in RunWorker and RunConsumer then stages its change event (ST-SUP-RC35-OC14, ST-SUP-RP10-LC36).
	ChangeEvents []ChangeEvents
	// Lists are the lists of the worker's declaration, which the generator emits in the worker contract from the
	// entity of the release (ST-SUP-RC35-OC15). The worker's handle takes only these lists, so its fill gives no SQL
	// text (ST-SUP-RP10-LC38). Each is a list of an entity in Writes.
	Lists []AnyList
}

// Worker is one checked worker declaration.
type Worker[P any] struct {
	spec         WorkerSpec[P]
	principal    authz.Principal
	writes       map[Declared]bool
	reads        map[Declared]bool
	destinations map[string]Destination
	bound        []authz.Relation
	own          map[authz.Relation]bool
	changeEvents map[string]ChangeEvents
	lists        map[AnyList]listCopy
}

// NewWorker checks the declaration of one worker.
func NewWorker[P any](spec WorkerSpec[P]) (*Worker[P], error) {
	if spec.Name == "" || spec.Permission == "" || spec.Probe == nil {
		return nil, errors.New("command: a worker needs a name, its permission and its probe")
	}
	if spec.Interval < 0 || spec.Interval%time.Second != 0 {
		return nil, fmt.Errorf("command: the interval of %s is not a whole number of seconds", spec.Name)
	}
	principal, err := authz.System(spec.Name)
	if err != nil {
		return nil, err
	}
	writes, err := checkWrites(spec.Name, spec.Writes)
	if err != nil {
		return nil, err
	}
	reads, err := checkReads(spec.Name, spec.Writes, spec.Reads)
	if err != nil {
		return nil, err
	}
	destinations, err := checkDestinations(spec.Name, spec.Destinations)
	if err != nil {
		return nil, err
	}
	changeEvents, err := checkChangeEvents(spec.Name, spec.Writes, destinations, spec.ChangeEvents)
	if err != nil {
		return nil, err
	}
	lists, err := checkLists(spec.Name, spec.Lists, writes, reads)
	if err != nil {
		return nil, err
	}
	bound, own := boundRelations(spec.Relations, spec.Writes)
	return &Worker[P]{spec: spec, principal: principal, writes: writes, reads: reads, destinations: destinations, bound: bound, own: own,
		changeEvents: changeEvents, lists: lists}, nil
}

// checkReads gives the read-only entities that the worker owner reads, and refuses a nil entity, an entity of reads
// that is not read-only, a read-only entity in writes, and an entity name in both (ST-SUP-RP10-LC53).
func checkReads(owner string, writes, declared []Declared) (map[Declared]bool, error) {
	written := map[string]bool{}
	for _, e := range writes {
		if e != nil && e.readOnly() {
			return nil, fmt.Errorf("command: %s writes the read-only entity %s; name it in Reads", owner, e.entityName())
		}
		if e != nil {
			written[e.entityName()] = true
		}
	}
	reads := make(map[Declared]bool, len(declared))
	for _, e := range declared {
		switch {
		case e == nil:
			return nil, fmt.Errorf("command: %s declares a nil entity in Reads", owner)
		case !e.readOnly():
			return nil, fmt.Errorf("command: %s reads %s, which is not a read-only entity", owner, e.entityName())
		case written[e.entityName()]:
			return nil, fmt.Errorf("command: %s both writes and reads %s", owner, e.entityName())
		}
		reads[e] = true
	}
	return reads, nil
}

// TickFill is the fill of a worker with an every trigger: it gets the scheduled tick time and the handle.
type TickFill[P any] func(ctx context.Context, tick time.Time, h *Handle[P]) error

// RunWorker runs one tick of the worker w for the tenant t (ST-SUP-RP10-LC18): it calls fill at most once, in one
// transaction that it opens with tenant.Begin, with no idempotency wrapper, and commits with tenant.Commit only when
// the fill returns no error and met no conflict; it rolls back every write, audit row and staged effect otherwise.
// The handle is bound to the worker's service account (ST-SUP-RP10-LC16), and each effect takes the key of the tick
// due, a multiple of the worker's interval since the Unix epoch (ST-SUP-RP10-LC21).
//
// When held is not nil, RunWorker locks the lease row with FOR SHARE right after the tenant setting and the lock
// timeout and before the fill, and keeps it until the transaction ends; a row with another token fences the run,
// which returns lease.ErrFenced without calling the fill (ST-SUP-RP10-LC20). The transaction then has a deadline no
// later than the lease TTL less the renew interval after it starts, and no later than the lease's expiry as last
// renewed; a run that reaches it rolls back and returns ErrDeadline, and never commits after it (ST-SUP-RP10-LC22).
// A conflict rolls back and returns ErrConflict: the fill does not run again in this tick (ST-SUP-RP10-LC26).
func RunWorker[P any](ctx context.Context, rt *Runtime, w *Worker[P], due time.Time, t tenant.Tenant, held *lease.Held,
	fill TickFill[P]) (err error) {
	if rt == nil || w == nil || fill == nil || t.IsZero() {
		return errors.New("command: RunWorker needs the runtime, the worker, the fill and the tenant")
	}
	interval := w.spec.Interval
	if interval <= 0 || due.UnixNano()%interval.Nanoseconds() != 0 {
		return fmt.Errorf("command: the tick of %s is not due at a multiple of its interval", w.spec.Name)
	}
	ctx, finish := rt.workerSpan(ctx, WorkerSpanName, w.spec.Name)
	defer func() { finish(spanOutcome(err), false, err != nil) }()

	var notAfter time.Time
	if held != nil {
		if held.Err() != nil {
			return fmt.Errorf("command: %s: %w", w.spec.Name, lease.ErrStopped)
		}
		clock := rt.clock()
		notAfter = clock.Now().Add(held.TTL() - held.RenewEvery())
		if expires := held.Expires(); expires.Before(notAfter) {
			notAfter = expires
		}
		var cancel context.CancelFunc
		// The transaction's context ends at the deadline, and database/sql then rolls the transaction back.
		ctx, cancel = clock.WithDeadline(ctx, notAfter)
		defer cancel()
	}
	// The tenant gate before the transaction: a tenant that was retired after the tick read the tenant list gets no
	// run (security review of PR #337, L1).
	if rt.cfg.Tenants != nil {
		active, err := rt.cfg.Tenants.Active(ctx, t)
		switch {
		case err != nil:
			return internalf("the tenant list was not read")
		case !active:
			return errTenantNotActive
		}
	}
	recheckSeals(ctx, rt, w.writes)
	tx, err := tenant.Begin(ctx, rt.cfg.DB, t, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return workerBegin(ctx, rt, notAfter, err)
	}
	if err := rt.markStep(ctx, tx); err != nil {
		_ = tx.Rollback()
		return workerBegin(ctx, rt, notAfter, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	s := newState(ctx, rt, tx, t, w.principal, modeWorker, w.spec.Name, w.writes, w.destinations)
	s.reads = w.reads
	s.changeEvents = w.changeEvents
	// A list value RunTime takes the due tick time (ST-SUP-RC35-OC15).
	s.lists, s.runTime = w.lists, due
	s.request = Request{RequestID: uuid.NewString()}
	s.permission = w.spec.Permission
	s.lockTimeout = DefaultLockTimeout
	tick := strconv.FormatInt(due.Unix(), 10)
	s.inbound = func(id string) string { return lengthPrefixed(w.spec.Name, tick, id) }
	if err := s.setLockTimeout(ctx); err != nil {
		return s.failRun(ctx, notAfter, "lock timeout", err)
	}
	if err := s.checkIsolation(ctx); err != nil {
		return deadlineOr(ctx, rt, notAfter, err)
	}
	if held != nil {
		if _, err := held.Fence(ctx, tx); err != nil {
			if errors.Is(err, lease.ErrFenced) {
				return fmt.Errorf("command: %s: %w", w.spec.Name, lease.ErrFenced)
			}
			return s.failRun(ctx, notAfter, "lease fence", err)
		}
	}
	s.ownRelations = w.own
	if s.az, err = rt.cfg.Authz.Bind(tx, t, w.principal, w.bound); err != nil {
		return internalf("the authorization handle was not bound")
	}
	call := rt.cfg.Probes.Bind(ctx, t.ID(), w.principal.Subject().String(), w.spec.Name)
	defer call.Rollback()
	h := &Handle[P]{st: s, probe: w.spec.Probe(call.Probe())}
	if _, err := callFill(ctx, s, call, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fill(ctx, due, h)
	}); err != nil {
		var failure *fillFailure
		if errors.As(err, &failure) {
			err = failure.err
		}
		return deadlineOr(ctx, rt, notAfter, err)
	}
	// The transaction's context ends at the deadline, so database/sql refuses a COMMIT after it
	// (ST-SUP-RP10-LC22). An outcome of COMMIT that is not known stays one: it is never a deadline to run again.
	if err := commit(ctx, s); err != nil {
		if errors.Is(err, ErrCommitUnknown) {
			return err
		}
		return deadlineOr(ctx, rt, notAfter, err)
	}
	committed = true
	call.Commit()
	return nil
}

// workerOutcome is the fixed outcome of a worker run that did not commit (ST-SUP-RC35-OC09): fenced for a run that
// its lease fenced, panicked for a fill that panicked, and rolled_back for any other error.
func workerOutcome(err error) probe.WorkerOutcome {
	switch {
	case errors.Is(err, lease.ErrFenced):
		return probe.WorkerFenced
	case errors.Is(err, errPanic):
		return probe.WorkerPanicked
	}
	return probe.WorkerRolledBack
}

// spanOutcome is the outcome of the span of a worker run: committed for a run that returned nil, and else the fixed
// outcome of workerOutcome (ST-SUP-RC35-OC09).
func spanOutcome(err error) otel.Outcome {
	if err == nil {
		return otel.OutcomeCommitted
	}
	return otel.Outcome(workerOutcome(err))
}

// workerBegin maps an error of tenant.Begin in a worker run.
func workerBegin(ctx context.Context, rt *Runtime, notAfter time.Time, err error) error {
	if isConflict(ctx, err) {
		return deadlineOr(ctx, rt, notAfter, &conflictError{text: "the transaction did not begin before the deadline"})
	}
	return internalf("the transaction did not begin")
}

// failRun maps a statement error of a worker run.
func (s *state) failRun(ctx context.Context, notAfter time.Time, step string, err error) error {
	return deadlineOr(ctx, s.rt, notAfter, s.fail(ctx, step, err))
}

// deadlineOr gives ErrDeadline for an error of a run under a lease that reached its deadline, and err otherwise.
func deadlineOr(ctx context.Context, rt *Runtime, notAfter time.Time, err error) error {
	if !notAfter.IsZero() && errors.Is(ctx.Err(), context.DeadlineExceeded) && !rt.clock().Now().Before(notAfter) {
		return errDeadline
	}
	return err
}

// errTenantNotActive is the refusal of a message of a tenant that the tenant list does not hold as active, such as a
// retired tenant (ST-SUP-RC27-OC12).
var errTenantNotActive = errors.New("command: the tenant of the message is not an active tenant of the tenant list")

// activeTenant checks the tenant of a claimed message against the tenant list of Config.Tenants, before any fill: a
// tenant that the list does not hold as active, such as a retired tenant, is a permanent refusal, which parks the
// message with an alert and runs no fill (ST-SUP-RC27-OC12, ST-SUP-RP01-LC12); a failed read of the list is a
// transient refusal, so the message is tried again and no fill runs. With no Tenants it checks nothing.
func (rt *Runtime) activeTenant(ctx context.Context, t tenant.Tenant) error {
	if rt.cfg.Tenants == nil {
		return nil
	}
	active, err := rt.cfg.Tenants.Active(ctx, t)
	switch {
	case err != nil:
		return outbox.TransientRefusal(errTenantList)
	case !active:
		return outbox.PermanentRefusal(errTenantNotActive)
	}
	return nil
}

// WorkerTupleMissingCode is the failure code of a consumed message whose worker's own tuple SpiceDB does not hold yet,
// such as for a subscription that a new release adds, before its controller writes the tuple (ST-SUP-RC35-OC18).
const WorkerTupleMissingCode = "worker-tuple-missing"

// HeldTupleAge is the most time that RunConsumer keeps that SpiceDB holds the tuple of a worker in a tenant.
const HeldTupleAge = time.Minute

// errWorkerTupleMissing matches the transient refusal of a consumed message whose worker's tuple is not in SpiceDB.
var errWorkerTupleMissing = errors.New("command: the tuple of the consuming worker on the tenant of the message is not in SpiceDB")

// workerTupleHeld checks, before any fill, that SpiceDB holds the tuple tenant:<t>#worker_<name>@service_account:
// <name> of the consuming worker, at full consistency (ST-SUP-RC35-OC18). A tuple that is missing is a transient
// refusal with the failure code worker-tuple-missing, which the poison count does not count: the outbox tries the
// message again with its back-off, and the fill never runs with lists that the missing tuple empties. A read that
// fails is a transient refusal of an undecided run, with its own cause, so an outage of SpiceDB never reads as a
// missing tuple (security review of PR #437, L1). When the tuple of a worker stays missing for WorkerTupleAlertAfter,
// it logs one warn line, at most once in each WorkerTupleAlertAfter. It keeps that the tuple is held for at most
// HeldTupleAge, and never that it is missing. With no Config.WorkerTuples it checks nothing.
func (rt *Runtime) workerTupleHeld(ctx context.Context, t tenant.Tenant, worker string) error {
	reader := rt.cfg.WorkerTuples
	if reader == nil {
		return nil
	}
	cacheKey := t.ID() + "\x00" + worker
	now := rt.clock().Now()
	rt.heldTuplesMu.Lock()
	at, ok := rt.heldTuples[cacheKey]
	rt.heldTuplesMu.Unlock()
	if ok && now.Sub(at) < HeldTupleAge && !now.Before(at) {
		return nil
	}
	undecided := outbox.TransientRefusal(fmt.Errorf("%w: the tuple of the consuming worker was not read", ErrUndecided))
	resource, err := authz.TenantResource(t)
	if err != nil {
		return undecided
	}
	subject, err := authz.ServiceAccount(worker)
	if err != nil {
		return undecided
	}
	held, err := reader.Exists(ctx, authz.FilterOf(authz.Tuple{Resource: resource, Relation: WorkerTenantRelation(worker), Subject: subject}))
	if err != nil {
		return undecided
	}
	if !held {
		rt.tupleMissing(ctx, cacheKey, worker, now)
		return outbox.WithFailureCode(outbox.TransientRefusal(errWorkerTupleMissing), WorkerTupleMissingCode)
	}
	rt.heldTuplesMu.Lock()
	delete(rt.missingSince, cacheKey)
	if rt.heldTuples == nil {
		rt.heldTuples = map[string]time.Time{}
	}
	if len(rt.heldTuples) >= maxHeldTuples {
		clear(rt.heldTuples)
	}
	rt.heldTuples[cacheKey] = now
	rt.heldTuplesMu.Unlock()
	return nil
}

// WorkerTupleAlertAfter is how long the tuple of a worker in a tenant stays missing before RunConsumer logs the warn
// line command.consumer.worker_tuple_missing, and the least time between two such lines of one worker and tenant.
const WorkerTupleAlertAfter = 15 * time.Minute

// WorkerTupleMissingEvent is the event name of the warn line of a worker tuple that stays missing.
const WorkerTupleMissingEvent = "command.consumer.worker_tuple_missing"

// tupleMissing records that the tuple of key is missing at now, and logs one warn line when it has been missing for
// WorkerTupleAlertAfter and no line of it came in the last WorkerTupleAlertAfter: the worker and the minutes, and no
// tenant and no message.
func (rt *Runtime) tupleMissing(ctx context.Context, key, worker string, now time.Time) {
	rt.heldTuplesMu.Lock()
	if rt.missingSince == nil {
		rt.missingSince, rt.missingWarned = map[string]time.Time{}, map[string]time.Time{}
	}
	if len(rt.missingSince) >= maxHeldTuples {
		clear(rt.missingSince)
		clear(rt.missingWarned)
	}
	since, ok := rt.missingSince[key]
	if !ok || now.Before(since) {
		rt.missingSince[key], since = now, now
	}
	warned, was := rt.missingWarned[key]
	due := now.Sub(since) >= WorkerTupleAlertAfter && (!was || now.Sub(warned) >= WorkerTupleAlertAfter)
	if due {
		rt.missingWarned[key] = now
	}
	rt.heldTuplesMu.Unlock()
	if !due || rt.logger == nil {
		return
	}
	var record otellog.Record
	record.SetEventName(WorkerTupleMissingEvent)
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("WARN")
	record.SetBody(attribute.StringValue("the tuple of a consuming worker is still missing in SpiceDB, so its messages wait; the controller writes it"))
	record.AddAttributes(attribute.String("worker", worker), attribute.Int64("minutes", int64(now.Sub(since)/time.Minute)))
	rt.logger.Emit(context.WithoutCancel(ctx), record)
}

// maxHeldTuples bounds the memory of the held tuples: when it is full, it starts again empty.
const maxHeldTuples = 10000

// errTenantList is the transient refusal of a consumed message whose tenant list was not read.
var errTenantList = internalf("the tenant list was not read")

// errMessage matches the permanent refusal of a consumed message that the runtime cannot take: a row with no staging
// principal or with a stager that the worker does not consume, a payload that is not one JSON value, or a key of the
// message that is used for another payload.
var errMessage = errors.New("command: the consumed message cannot be taken")

// MessageFill is the fill of a worker with a consumes trigger: it gets the decoded message, its effect name and the
// handle.
type MessageFill[M, P any] func(ctx context.Context, message M, effect string, h *Handle[P]) error

// RunConsumer calls the fill of the consuming worker w once for the claimed outbox message d, in tx, the outbox
// worker's transaction of the message's tenant, which the outbox opened with tenant.Begin and commits with the
// message's state (ST-SUP-RP01-LC19). It is the body of the destination handler of the consumed destination.
//
// The handle is bound to tx, to the tenant of the claimed row and to the worker's service account, and decides each
// resource as the service account with the worker's permission and as the staging principal, which it builds from
// the row's principal column through the sealed constructor, with the permission of the row's stager
// (ST-SUP-RP10-LC16, ST-SUP-RP10-LC17); neither comes from the fill or the payload. Before the fill it inserts the
// message identity as an idempotency key of the tenant and the service account, and returns success without calling
// the fill when that key is completed (ST-SUP-RP10-LC19); each staged effect's key derives from that identity
// (ST-SUP-RP10-LC08).
//
// With Config.Tenants, a message of a tenant that the tenant list does not hold as active, such as a retired tenant,
// is refused with outbox.PermanentRefusal before the fill, so the outbox parks it (ST-SUP-RC27-OC12).
//
// It returns nil when the outbox may commit tx with the message done; an error marked with outbox.Conflict for a
// conflict, which the outbox dispatches again and does not count as an attempt (ST-SUP-RP10-LC26), except a unique
// violation, which a dispatch again gives again and so is an attempt; an error marked
// with outbox.PermanentRefusal for a row whose stager or principal it cannot read; and an error marked with
// outbox.TransientRefusal for a fill that fails or panics, whose writes and effects roll back with tx
// (ST-SUP-RC35-OC06).
func RunConsumer[M, P any](ctx context.Context, rt *Runtime, w *Worker[P], tx *sql.Tx, d outbox.Delivery,
	decode func(payload []byte) (M, error), fill MessageFill[M, P]) (err error) {
	if rt == nil || w == nil || tx == nil || decode == nil || fill == nil || d.Tenant.IsZero() {
		return errors.New("command: RunConsumer needs the runtime, the worker, the transaction, the decoder, the fill and a claimed message")
	}
	ctx, finish := rt.workerSpan(ctx, ConsumerSpanName, w.spec.Name)
	outcome := otel.OutcomeRolledBack
	defer func() {
		if outcome == otel.OutcomeRolledBack {
			outcome = otel.Outcome(workerOutcome(err))
		}
		finish(outcome, false, err != nil && !outbox.IsConflict(err))
	}()
	// Each attempt that does not commit is logged and carries its outcome and cause class to ObserveConsumer
	// (ST-SUP-RC35-OC06).
	defer func() {
		if err != nil {
			err = rt.consumerFailed(ctx, w.spec.Name, d, err)
		}
	}()
	if err := rt.activeTenant(ctx, d.Tenant); err != nil {
		return err
	}
	if err := rt.workerTupleHeld(ctx, d.Tenant, w.spec.Name); err != nil {
		return err
	}
	if err := rt.markStep(ctx, tx); err != nil {
		return outbox.Conflict(fmt.Errorf("command: %s: set the writer marker: %w", w.spec.Name, err))
	}

	stagingPrincipal, err := authz.StagingPrincipal(staging.NewStaging(), d.Principal)
	if err != nil {
		return outbox.PermanentRefusal(fmt.Errorf("%w: %s: the message has no staging principal", errMessage, w.spec.Name))
	}
	stagerPermission, ok := w.spec.Stagers[d.Origin]
	if !ok || stagerPermission == "" {
		return outbox.PermanentRefusal(fmt.Errorf("%w: %s consumes no message that %q stages", errMessage, w.spec.Name, d.Origin))
	}
	// The outbox began the transaction of a consumer, so its check runs beside it.
	recheckSeals(ctx, rt, w.writes)
	s := newState(ctx, rt, tx, d.Tenant, w.principal, modeConsumer, w.spec.Name, w.writes, w.destinations)
	s.reads = w.reads
	s.changeEvents = w.changeEvents
	// A list value RunTime takes the time at the start of the run on the runtime's clock (ST-SUP-RC35-OC15).
	s.lists, s.runTime = w.lists, rt.clock().Now()
	s.request = Request{RequestID: uuid.NewString(), Key: d.ID.String()}
	s.permission = w.spec.Permission
	s.stagingPermission = stagerPermission
	s.onBehalfOf = stagingPrincipal.Subject().String()
	s.lockTimeout = DefaultLockTimeout
	message := d.ID.String()
	s.inbound = func(id string) string { return lengthPrefixed(message, id) }
	if err := s.setLockTimeout(ctx); err != nil {
		return consumerError(s.fail(ctx, "lock timeout", err))
	}
	if err := s.checkIsolation(ctx); err != nil {
		return consumerError(err)
	}
	s.ownRelations = w.own
	if s.az, err = rt.cfg.Authz.Bind(tx, d.Tenant, w.principal, w.bound); err != nil {
		return outbox.TransientRefusal(internalf("the authorization handle was not bound"))
	}
	if s.staging, err = rt.cfg.Authz.Bind(tx, d.Tenant, stagingPrincipal, nil); err != nil {
		return outbox.TransientRefusal(internalf("the authorization handle of the staging principal was not bound"))
	}
	fingerprint, err := idempotency.Fingerprint("CONSUME", d.Destination+"/"+d.Kind, d.Payload)
	if err != nil {
		return outbox.PermanentRefusal(fmt.Errorf("%w: %s: the payload is not one JSON value", errMessage, w.spec.Name))
	}
	call := rt.cfg.Probes.Bind(ctx, d.Tenant.ID(), w.principal.Subject().String(), w.spec.Name)
	defer call.Rollback()
	h := &Handle[P]{st: s, probe: w.spec.Probe(call.Probe())}
	wrapped, err := idempotency.Wrap(ctx, wrapTx{s: s}, rt.keyed(idempotency.Request{Tenant: d.Tenant.ID(),
		Principal: w.principal.Subject().String(), Key: message, Fingerprint: fingerprint}),
		func(ctx context.Context) (idempotency.Response, error) {
			decoded, err := decode(bytes.Clone(d.Payload))
			if err != nil {
				s.close()
				call.Returned()
				return idempotency.Response{}, &fillFailure{err: err}
			}
			_, err = callFill(ctx, s, call, func(ctx context.Context) (struct{}, error) {
				return struct{}{}, fill(ctx, decoded, d.Kind, h)
			})
			if err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{Status: statusOK}, nil
		})
	var failure *fillFailure
	switch {
	case s.notGranted || sqlState(err) == notGrantedState:
		// The worker role lacks a privilege that the fill needs: a dispatch again fails the same way, so the message
		// is parked and does not hold back its key without end (decision on ST-SUP-Q141, orchestrator under the
		// owner's delegation, 2026-11-15).
		return outbox.PermanentRefusal(&notGrantedError{worker: w.spec.Name})
	case errors.As(err, &failure):
		if failure.conflict && !uniqueConflict(failure.err) {
			return outbox.Conflict(failure.err)
		}
		// A deny of the staging principal and a declared refusal give the same answer on every dispatch: the
		// message is parked, so it does not hold back its key without end (ST-SUP-RP10-LC26, ST-SUP-RC35-OC06).
		var denied *deniedError
		if (errors.As(failure.err, &denied) && denied.staging) || errors.Is(failure.err, errDeclaredRefusal) {
			return outbox.PermanentRefusal(failure.err)
		}
		// A fill error or a panic that ends PoisonAttempts attempts of the message in a row parks it as poison; a
		// provider that cannot decide now stays transient (ST-SUP-RC35-OC06).
		return poisonStreak(d, failure.err)
	case err != nil:
		return outbox.TransientRefusal(internalf("the idempotency wrapper failed"))
	case wrapped.Replayed:
		// The message was consumed already: the outbox completes it, and nothing changes a second time.
		outcome = otel.OutcomeSuccess
		return nil
	case wrapped.Problem == idempotency.ProblemInProgress:
		return outbox.Conflict(&conflictError{text: "another delivery of the message is in progress"})
	case wrapped.Problem != "" || !wrapped.Commit:
		return outbox.PermanentRefusal(fmt.Errorf("%w: %s: the key of the message is used for another payload", errMessage, w.spec.Name))
	}
	if rt.beforeCommit != nil {
		if err := rt.beforeCommit(ctx, tx); err != nil {
			return outbox.TransientRefusal(internalf("the step before COMMIT failed"))
		}
	}
	if err := s.setLockTimeout(ctx); err != nil {
		return consumerError(s.fail(ctx, "lock timeout", err))
	}
	outcome = otel.OutcomeCommitted
	call.Commit()
	return nil
}

// PoisonAttempts is the number of attempts in a row of one consumed message that end with the same fill error or
// panic, of the same outcome and cause class, after which the message is parked as poison (ST-SUP-RC35-OC06): a fill
// has no network, so such an error almost always comes again, and each attempt holds the locks of its run.
const PoisonAttempts = 5

// PoisonCode is the failure code of a message that is parked as poison. The message stays in the outbox and can be
// sent again with Resolve, so a message that must land is never dropped.
const PoisonCode = "poison"

// ErrPoison matches the error of a consumer run that parks its message as poison.
var ErrPoison = errors.New("command: the same fill error ended the attempts of the message in a row")

// poisonError is the refusal of a run that parks its message as poison: it matches ErrPoison and unwraps to the
// error of the run.
type poisonError struct{ err error }

func (e *poisonError) Error() string        { return ErrPoison.Error() + ": " + e.err.Error() }
func (e *poisonError) Unwrap() error        { return e.err }
func (e *poisonError) Is(target error) bool { return target == ErrPoison }

// streakPrefix is the start of the failure code of a run that ended with a fill error or a panic.
const streakPrefix = "fill."

// poisonStreak gives the refusal of a run whose fill failed with err. A fill error or a panic is counted in the
// failure code of the message, fill.<outcome>.<cause>.<n>, which the claim of the message's destination gives back:
// the attempt that makes n reach PoisonAttempts with the same outcome and cause class is a permanent refusal with the
// failure code poison, so the message is parked with the alert of ST-SUP-RP01-LC12; any other attempt between them,
// such as a conflict, starts the count again. Any other error, such as a provider that cannot decide now, is a
// transient refusal that is not counted.
func poisonStreak(d outbox.Delivery, err error) error {
	outcome, cause := consumerOutcome(err)
	if (outcome != probe.WorkerRolledBack && outcome != probe.WorkerPanicked) || !streakCauses[cause] {
		// The failure code delivery-refused of this attempt starts the count again.
		return outbox.TransientRefusal(err)
	}
	prefix := streakPrefix + string(outcome) + "." + cause + "."
	n := 1
	if rest, ok := strings.CutPrefix(d.FailureCode, prefix); ok {
		if before, err := strconv.Atoi(rest); err == nil && before >= 1 && before < PoisonAttempts {
			n = before + 1
		}
	}
	if n >= PoisonAttempts {
		return outbox.WithFailureCode(outbox.PermanentRefusal(&poisonError{err: err}), PoisonCode)
	}
	return outbox.WithFailureCode(outbox.TransientRefusal(err), prefix+strconv.Itoa(n))
}

// streakCauses are the cause classes that the fill causes, which count for the poison bound: an error of the fill,
// a read of a row that is not there, an invalid value, a deny of the worker's permission, a unique violation, which
// a consumer counts as an attempt, a statement that fails on the values of the row (data: SQLSTATE classes 22 and
// 23, decision on the re-review of PR #411), and a panic. A statement that the database failed, a timeout, a lost connection
// and any other internal error stay transient, are not counted and start the count again (decision of the
// orchestrator on the security review of PR #411, M1).
var streakCauses = map[string]bool{"fill_error": true, "not_found": true, "invalid": true, "refused": true, "conflict": true,
	"panic": true, "data": true, "create_without_parent": true}

// consumerOutcome gives the fixed outcome and cause class of the error of a consumer run (ST-SUP-RC35-OC06).
func consumerOutcome(err error) (probe.WorkerOutcome, string) {
	// The attempt that parks its message as poison has the outcome and the cause class of its fill error.
	var poison *poisonError
	if errors.As(err, &poison) {
		return consumerOutcome(poison.err)
	}
	var denied *deniedError
	switch {
	case errors.Is(err, ErrNotGranted):
		return probe.WorkerNotGranted, "not_granted"
	case outbox.IsConflict(err):
		return probe.WorkerConflict, causeOf(err)
	case errors.Is(err, errPanic):
		return probe.WorkerPanicked, "panic"
	case errors.Is(err, errDeclaredRefusal):
		return probe.WorkerRefused, "declared_refusal"
	case errors.As(err, &denied) && denied.staging:
		return probe.WorkerDenied, "staging_denied"
	case errors.Is(err, errWorkerTupleMissing):
		// Not counted for poison: a later attempt runs when the controller wrote the tuple (ST-SUP-RC35-OC18).
		return probe.WorkerUndecided, "worker_tuple_missing"
	case errors.Is(err, errTenantNotActive) || errors.Is(err, errTenantList):
		if outbox.IsPermanent(err) {
			return probe.WorkerRefused, "tenant"
		}
		return probe.WorkerRolledBack, "tenant"
	case errors.Is(err, ErrUndecided) || authz.Transient(err):
		return probe.WorkerUndecided, "undecided"
	case errors.Is(err, errMessage):
		return probe.WorkerRefused, "message"
	case outbox.IsPermanent(err):
		return probe.WorkerRefused, causeOf(err)
	}
	return probe.WorkerRolledBack, causeOf(err)
}

// causeOf gives the fixed cause class of the error of a run, one of probe.WorkerCauses, and never its text.
func causeOf(err error) string {
	var statement *internalError
	switch {
	case errors.Is(err, errPanic):
		return "panic"
	case errors.Is(err, errDeadline) || errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, lease.ErrFenced):
		return "fenced"
	case errors.Is(err, ErrNoParent):
		// A worker's create with no parent to decide on (ST-SUP-RP10-LC17; walkthrough 10, B2).
		return "create_without_parent"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, ErrRefused):
		return "refused"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.As(err, &statement) && statement.statement:
		return "statement"
	case errors.As(err, &statement) && statement.data:
		return "data"
	case errors.Is(err, ErrInternal):
		return "internal"
	}
	return "fill_error"
}

// runFailure is the error of a consumer run that did not commit, with its fixed outcome and cause class, which
// ObserveConsumer sends as worker.run.failed. It unwraps to the error of the run, so the outbox reads its mark.
type runFailure struct {
	outcome probe.WorkerOutcome
	cause   string
	err     error
}

func (f *runFailure) Error() string { return f.err.Error() }
func (f *runFailure) Unwrap() error { return f.err }

// consumerFailed writes the one warn log line of a consumer attempt that did not commit, with the worker, the
// consumed destination, the event ID of the message, the attempt number, the outcome and the cause class, and never
// the error text or a value of the message, as the Kafka destination logs kafka.delivery_failed (ST-SUP-RC35-OC06);
// it gives err with its outcome and cause class for ObserveConsumer.
func (rt *Runtime) consumerFailed(ctx context.Context, worker string, d outbox.Delivery, err error) error {
	outcome, cause := consumerOutcome(err)
	if rt.logger != nil {
		var record otellog.Record
		record.SetEventName(ConsumerFailedEvent)
		record.SetSeverity(otellog.SeverityWarn)
		record.SetSeverityText("WARN")
		record.SetBody(attribute.StringValue("a consuming worker's run did not commit: " + worker + " " + string(outcome) + " " + cause))
		record.AddAttributes(attribute.String("worker", worker), attribute.String("destination", d.Destination),
			attribute.String("event_id", d.ID.String()), attribute.Int64("attempt", d.Attempts),
			attribute.String("outcome", string(outcome)), attribute.String("cause", cause))
		if entity, ok := NoParentEntity(err); ok {
			// The declared name of the entity whose create had no parent, never a value (walkthrough 10, B2).
			record.AddAttributes(attribute.String("entity", entity))
		}
		rt.logger.Emit(context.WithoutCancel(ctx), record)
	}
	return &runFailure{outcome: outcome, cause: cause, err: err}
}

// ConsumerFailedEvent is the event name of the warn log line of a consumer attempt that did not commit
// (ST-SUP-RC35-OC06).
const ConsumerFailedEvent = "command.consumer.run_failed"

// ErrNotGranted matches the error of a consumer run whose statement failed with SQLSTATE 42501, as the worker role
// lacks a privilege that the fill needs: RunConsumer parks the message (decision on ST-SUP-Q141).
var ErrNotGranted = errors.New("command: the worker role lacks a privilege that the fill needs")

// notGrantedError is the refusal of a consumer run that the database refused for a missing privilege. It names the
// worker and no database text.
type notGrantedError struct{ worker string }

func (e *notGrantedError) Error() string {
	return "command: " + e.worker + ": the worker role lacks a privilege that the fill needs (SQLSTATE 42501)"
}
func (e *notGrantedError) Is(target error) bool { return target == ErrNotGranted }

// ObserveConsumer reports the error err of one RunConsumer of a consuming worker to its built-in observations and
// returns it: each attempt that does not commit is sent as worker.run.failed with its outcome, such as not_granted
// for a run that the database refused for a missing privilege (decision on ST-SUP-Q141), and its fixed cause class
// (ST-SUP-RC35-OC06). The generated destination handler of a consuming worker calls it.
func ObserveConsumer(ctx context.Context, observe WorkerObserver, err error) error {
	if observe == nil || err == nil {
		return err
	}
	var failure *runFailure
	if !errors.As(err, &failure) {
		outcome, cause := consumerOutcome(err)
		failure = &runFailure{outcome: outcome, cause: cause}
	}
	runFailed(context.WithoutCancel(ctx), observe, failure.outcome, failure.cause)
	return err
}

// CauseObserver is a WorkerObserver that also takes the fixed cause class of a failed run: probe.Worker is one. An
// observer that is not one gets RunFailed with the outcome only.
type CauseObserver interface {
	RunFailedCause(ctx context.Context, outcome probe.WorkerOutcome, cause string)
}

// runFailed sends worker.run.failed with the outcome and, when observe takes it, the cause class.
func runFailed(ctx context.Context, observe WorkerObserver, outcome probe.WorkerOutcome, cause string) {
	if with, ok := observe.(CauseObserver); ok {
		with.RunFailedCause(ctx, outcome, cause)
		return
	}
	observe.RunFailed(ctx, outcome)
}

// errDeclaredRefusal matches an error that Refusal marks.
var errDeclaredRefusal = errors.New("command: a declared refusal")

// declaredRefusal is a declared refusal of a fill: it matches errDeclaredRefusal and unwraps to the refusal.
type declaredRefusal struct{ err error }

func (r *declaredRefusal) Error() string        { return r.err.Error() }
func (r *declaredRefusal) Unwrap() error        { return r.err }
func (r *declaredRefusal) Is(target error) bool { return target == errDeclaredRefusal }

// Refused marks err as a declared refusal of a fill (ST-DECL-EN18, ST-SUP-RC32-OC09); a *Refusal is one without
// it. RunConsumer parks a message whose fill returns one, because a dispatch again gives the same refusal
// (ST-SUP-RP10-LC26).
func Refused(err error) error {
	if err == nil {
		return nil
	}
	return &declaredRefusal{err: err}
}

// consumerError marks the error of a consumer's statement for the outbox.
func consumerError(err error) error {
	if errors.Is(err, ErrConflict) && !uniqueConflict(err) {
		return outbox.Conflict(err)
	}
	return outbox.TransientRefusal(err)
}
