package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// WorkerTuplesJob is the name of the periodic job of the controller mode that reconciles the worker tuples, and
// ReconcileCommand the action of its audit rows (ST-SUP-RC35-OC13).
const (
	WorkerTuplesJob  = "worker-tuples"
	ReconcileCommand = "reconcile-worker-tuples"
)

// WorkerTuplesPage is the most tenants that one page of the reconcile reads from the tenant list.
const WorkerTuplesPage = 100

// WorkerRelationPrefix starts the name of each relation of the type tenant that is reserved for the declared
// workers: worker_<name in snake case> (ST-SUP-RC35-OC13).
const WorkerRelationPrefix = "worker_"

// ReconcilePrincipal is the service account of the reconcile: the principal of its outbox rows and its revocation,
// and the actor of its audit rows, of the actor kind system.
const ReconcilePrincipal = "stego-controller"

// WorkerTupleReader reads what SpiceDB holds, at full consistency: the relations of a definition of the schema it
// serves, and whether a relationship that a filter of one resource and one relation matches exists. authz.SpiceDB
// is one.
type WorkerTupleReader interface {
	RelationsOf(ctx context.Context, definition string) ([]string, error)
	Exists(ctx context.Context, filter authz.Filter) (bool, error)
}

// The classes of a failure of the reconcile, which its log line and its error name, and never a tenant or a tuple.
const (
	ReconcileSchemaRead  = "schema_read"
	ReconcileTenantList  = "tenant_list"
	ReconcileTupleRead   = "tuple_read"
	ReconcileTransaction = "transaction"
	ReconcileStage       = "stage"
	ReconcileAudit       = "audit"
	ReconcileCommit      = "commit"
)

// reconcileError is a failure of one class of the reconcile.
type reconcileError struct {
	class string
	err   error
}

func (e *reconcileError) Error() string {
	return "command: the reconcile of the worker tuples failed at " + e.class + ": " + e.err.Error()
}

func (e *reconcileError) Unwrap() error { return e.err }

func failedAt(class string, err error) error { return &reconcileError{class: class, err: err} }

// classOf gives the class of a failure of the reconcile.
func classOf(err error) string {
	var r *reconcileError
	if errors.As(err, &r) {
		return r.class
	}
	return ReconcileTransaction
}

// WorkerReconcile reconciles the tuple of each declared worker on the tenant resource of each active tenant
// (ST-SUP-RC35-OC13).
type WorkerReconcile struct {
	rt        *Runtime
	schema    string
	workers   []string
	reader    WorkerTupleReader
	principal authz.Principal
	page      int
}

// ReconcileReport counts the tenants of one run of the reconcile: each one it visited, each one whose tuples it
// changed, and each one where it failed.
type ReconcileReport struct {
	Tenants, Changed, Failed int
	// FirstClass is the class of the first failure, or empty.
	FirstClass string
}

// NewWorkerReconcile gives the reconcile of the worker tuples on the command runtime rt, for the tenant list of
// schema, the declared workers and the reader of SpiceDB. It refuses no worker, a worker whose relation another
// worker gives, and a worker name that a tuple cannot hold, as Provision does.
func NewWorkerReconcile(rt *Runtime, schema string, workers []string, reader WorkerTupleReader) (*WorkerReconcile, error) {
	switch {
	case rt == nil || rt.cfg.DB == nil:
		return nil, errors.New("command: the reconcile of the worker tuples needs the command runtime")
	case schema == "":
		return nil, errors.New("command: the reconcile of the worker tuples needs the schema of the tenant list")
	case len(workers) == 0:
		return nil, errors.New("command: the reconcile of the worker tuples needs at least one declared worker")
	case reader == nil:
		return nil, errors.New("command: the reconcile of the worker tuples needs the reader of SpiceDB")
	}
	spec := TenantSpec{Action: tenant.ProvisionAction, ResourceType: authz.TenantType, Permission: "reconcile",
		Schema: schema, SubjectType: authz.ServiceAccountType, Workers: workers}
	if err := spec.check(rt); err != nil {
		return nil, err
	}
	principal, err := authz.System(ReconcilePrincipal)
	if err != nil {
		return nil, err
	}
	return &WorkerReconcile{rt: rt, schema: schema, workers: append([]string(nil), workers...), reader: reader,
		principal: principal, page: WorkerTuplesPage}, nil
}

// Run reconciles once (ST-SUP-RC35-OC13). It reads the relations of the type tenant that SpiceDB serves, and visits
// the active tenants of the list in pages of WorkerTuplesPage, in tenant ID order after the last tenant of the page
// before. For each tenant it reads whether SpiceDB holds the tuple of each declared worker, and whether it holds a
// tuple of each relation worker_<name> that no declared worker gives; and only when something differs, it stages in
// one transaction of that tenant, through the authz destination, the touch of each missing tuple and the delete of
// every tuple of each such relation, with one audit row. A tenant whose tuples match writes nothing. A failure in
// one tenant does not stop the next tenant: the run ends with an error that counts the failed tenants and names the
// class of the first failure, which its log line also names, with no tenant and no tuple.
func (r *WorkerReconcile) Run(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport
	relations, err := r.reader.RelationsOf(ctx, authz.TenantType)
	if err != nil {
		return report, r.fail(ctx, &report, failedAt(ReconcileSchemaRead, err), 0)
	}
	declared := map[string]bool{}
	for _, name := range r.workers {
		declared[WorkerTenantRelation(name)] = true
	}
	var undeclared []string
	for _, relation := range relations {
		if strings.HasPrefix(relation, WorkerRelationPrefix) && !declared[relation] {
			undeclared = append(undeclared, relation)
		}
	}
	after := ""
	for {
		page, err := tenant.ListAfter(ctx, r.rt.cfg.DB, r.schema, after, r.page)
		if err != nil {
			return report, r.fail(ctx, &report, failedAt(ReconcileTenantList, err), 0)
		}
		for _, t := range page {
			if ctx.Err() != nil {
				return report, r.fail(ctx, &report, failedAt(ReconcileTransaction, ctx.Err()), 0)
			}
			report.Tenants++
			changed, err := r.tenant(ctx, t, undeclared)
			switch {
			case err != nil:
				report.Failed++
				if report.FirstClass == "" {
					report.FirstClass = classOf(err)
				}
			case changed:
				report.Changed++
			}
		}
		if len(page) < r.page {
			break
		}
		after = page[len(page)-1].ID()
	}
	if report.Failed > 0 {
		return report, r.fail(ctx, &report, nil, report.Failed)
	}
	return report, nil
}

// fail logs the failed run and gives its error: the count of failed tenants, the count of tenants visited and the
// class of the first failure, and for a failure of the whole run its cause, which names no tenant.
func (r *WorkerReconcile) fail(ctx context.Context, report *ReconcileReport, whole error, failed int) error {
	if whole != nil {
		report.FirstClass = classOf(whole)
	}
	r.logFailure(ctx, *report)
	if whole != nil {
		return fmt.Errorf("command: the job %s stopped after %d tenants, %d of them failed; the first error class is %s: %w",
			WorkerTuplesJob, report.Tenants, report.Failed, report.FirstClass, whole)
	}
	return fmt.Errorf("command: the job %s failed in %d of %d tenants; the first error class is %s, and the next run tries them again",
		WorkerTuplesJob, failed, report.Tenants, report.FirstClass)
}

// logFailure emits the one log line of a failed run: the job, the counts and the class of the first failure only.
func (r *WorkerReconcile) logFailure(ctx context.Context, report ReconcileReport) {
	if r.rt.logger == nil {
		return
	}
	var record otellog.Record
	record.SetSeverity(otellog.SeverityError)
	record.SetEventName("command.worker_tuples.failed")
	record.SetBody(attribute.StringValue("the reconcile of the worker tuples failed; the next run of the job tries the failed tenants again"))
	record.AddAttributes(
		attribute.String("stego.job", WorkerTuplesJob),
		attribute.Int("stego.tenants", report.Tenants),
		attribute.Int("stego.failed_tenants", report.Failed),
		attribute.String("stego.error_class", report.FirstClass),
	)
	r.rt.logger.Emit(ctx, record)
}

// tenant reconciles the tuples of one tenant, and tells whether it staged a change.
func (r *WorkerReconcile) tenant(ctx context.Context, t tenant.Tenant, undeclared []string) (bool, error) {
	resource, err := authz.TenantResource(t)
	if err != nil {
		return false, failedAt(ReconcileTupleRead, err)
	}
	var touches []authz.Tuple
	for _, name := range r.workers {
		subject, err := authz.ServiceAccount(name)
		if err != nil {
			return false, failedAt(ReconcileTupleRead, err)
		}
		tuple := authz.Tuple{Resource: resource, Relation: WorkerTenantRelation(name), Subject: subject}
		held, err := r.reader.Exists(ctx, authz.FilterOf(tuple))
		if err != nil {
			return false, failedAt(ReconcileTupleRead, err)
		}
		if !held {
			touches = append(touches, tuple)
		}
	}
	var drops []string
	for _, relation := range undeclared {
		held, err := r.reader.Exists(ctx, authz.Filter{ResourceType: resource.Type, ResourceID: resource.ID, Relation: relation})
		if err != nil {
			return false, failedAt(ReconcileTupleRead, err)
		}
		if held {
			drops = append(drops, relation)
		}
	}
	if len(touches)+len(drops) == 0 {
		return false, nil
	}
	return r.stage(ctx, t, resource, touches, drops)
}

// workerTuplesLock is the transaction lock of the worker tuples of one tenant, which the reconcile and the
// provisioning hook of Provision and Retire take before they stage a tuple of it, so the reconcile reads whether the
// tenant is active after a Retire of it committed, and never stages a touch for a retired tenant after its delete.
const workerTuplesLock = `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('stego.worker-tuples:' || $1, 0))`

// stage writes the change of one tenant in one transaction of it: the lock of its worker tuples, the check that it is
// still active, the touches and the deletes through the authz destination, and one audit row.
func (r *WorkerReconcile) stage(ctx context.Context, t tenant.Tenant, resource authz.Resource, touches []authz.Tuple, drops []string) (bool, error) {
	tx, err := tenant.Begin(ctx, r.rt.cfg.DB, t, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, failedAt(ReconcileTransaction, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, setLockTimeout, fmt.Sprintf("%dms", DefaultLockTimeout.Milliseconds())); err != nil {
		return false, failedAt(ReconcileTransaction, err)
	}
	if _, err := tx.ExecContext(ctx, workerTuplesLock, t.ID()); err != nil {
		return false, failedAt(ReconcileTransaction, err)
	}
	active, err := tenant.ActiveIn(ctx, tx, r.schema, t)
	if err != nil {
		return false, failedAt(ReconcileTransaction, err)
	}
	if !active {
		// Retired since the page was read: Retire deleted its tuples, and the reconcile writes none.
		return false, nil
	}
	allowed := make([]authz.Relation, 0, len(touches)+len(drops))
	for _, tuple := range touches {
		allowed = append(allowed, authz.Relation{ResourceType: resource.Type, Name: tuple.Relation})
	}
	for _, relation := range drops {
		allowed = append(allowed, authz.Relation{ResourceType: resource.Type, Name: relation})
	}
	handle, err := r.rt.cfg.Authz.Bind(tx, t, r.principal, allowed)
	if err != nil {
		return false, failedAt(ReconcileStage, err)
	}
	for _, tuple := range touches {
		if _, err := handle.Relate(ctx, tuple); err != nil {
			return false, failedAt(ReconcileStage, fmt.Errorf("the touch of %s was not staged: %w", tuple.Relation, err))
		}
	}
	for _, relation := range drops {
		if _, err := handle.UnrelateAll(ctx, resource, relation); err != nil {
			return false, failedAt(ReconcileStage, fmt.Errorf("the delete of the relation %s was not staged: %w", relation, err))
		}
	}
	if _, err := r.rt.cfg.Audit.Record(ctx, tx, t, []audit.Event{r.event(ctx, t)}); err != nil {
		return false, failedAt(ReconcileAudit, err)
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return false, failedAt(ReconcileCommit, err)
	}
	return true, nil
}

// event is the audit row of a reconcile of one tenant: the action reconcile-worker-tuples, the reconcile's service
// account as the actor of the kind system, and the tenant as the resource, in the tenant's own chain
// (ST-SUP-RC27-OC10, ST-SUP-RC35-OC13).
func (r *WorkerReconcile) event(ctx context.Context, t tenant.Tenant) audit.Event {
	cfg := r.rt.cfg
	e := audit.Event{
		TenantID: t.ID(), ActorID: r.principal.Subject().String(), ActorKind: audit.ActorSystem,
		Action: ReconcileCommand, Operation: ReconcileCommand, ResourceType: TenantResourceType, ResourceID: t.ID(),
		Outcome: audit.OutcomeOK,
		Service: cfg.Service, ServiceVersion: cfg.ServiceVersion, DeclarationVersion: cfg.DeclarationVersion,
	}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		e.TraceID, e.SpanID = span.TraceID().String(), span.SpanID().String()
	}
	return e
}
