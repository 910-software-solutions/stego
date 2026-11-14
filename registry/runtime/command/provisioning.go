package command

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/txhold"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The commands of the tenant provisioning, which the audit rows name as their action (ST-SUP-RC27-OC10).
const (
	ProvisionCommand = "provision-tenant"
	RetireCommand    = "retire-tenant"
)

// TenantResourceType is the resource type of the audit rows of Provision and Retire, whose resource is the tenant.
const TenantResourceType = "tenant"

// TenantIDField is the member of the body of a Provision request that names the tenant, and TenantPathValue the
// path value of a Retire request that names it.
const (
	TenantIDField   = "tenant_id"
	TenantPathValue = "tenantId"
)

// The fixed codes of a Provision or Retire that the tenant list refuses: a Provision of a tenant that the list
// holds already, active or retired, so a retired tenant's ID never serves another customer and its rows stay apart;
// and a Retire of a tenant that the list does not hold as active.
const (
	ErrorCodeProvisioned    = "tenant.provisioned"
	ErrorCodeNotProvisioned = tenant.ErrNotProvisionedCode
)

// MaxTenantBody is the most bytes of the body of a Provision or Retire request.
const MaxTenantBody = 4096

// TenantSpec is what varies with the declared tenant provisioning of a service (ST-DECL-EN27), which the service
// runtime gives ServeTenant for each of the two commands (ST-SUP-RC27-OC09).
type TenantSpec struct {
	// Action is tenant.ProvisionAction or tenant.RetireAction.
	Action tenant.RequestAction
	// ResourceType is the type of the service-level resource, whose ID is the service name, and Permission the
	// declared permission of Action on it.
	ResourceType, Permission string
	// Schema is the schema of the tenant list stego_tenants.
	Schema string
	// SubjectType is the subject type of the schema that a verified identity maps to.
	SubjectType string
	// Retention is how long a completed idempotency key is kept; zero is the runtime's default.
	Retention time.Duration
	// Changed, when it is not nil, gets the tenant ID after a commit that changed the tenant list, such as the
	// Forget of the tenant gate.
	Changed func(id string)
	// Active, when it is not nil, tells whether the caller's own tenant is an active tenant of the tenant list, such
	// as the Active of the tenant gate. A denied or failed call of a caller whose tenant is not active, or whose
	// tenant cannot be read, writes no audit row: it writes a counted log line instead (ST-SUP-RC27-OC10), so the
	// claim of a verified token never starts the audit chain of a tenant that the list does not hold.
	Active func(ctx context.Context, t tenant.Tenant) (bool, error)
}

func (s TenantSpec) command() string {
	if s.Action == tenant.RetireAction {
		return RetireCommand
	}
	return ProvisionCommand
}

func (s TenantSpec) check(rt *Runtime) error {
	switch {
	case rt == nil || rt.cfg.DB == nil:
		return errors.New("command: the tenant provisioning needs the command runtime")
	case s.Action != tenant.ProvisionAction && s.Action != tenant.RetireAction:
		return errors.New("command: the tenant provisioning serves provision and retire only")
	case s.ResourceType == "" || s.Permission == "" || s.Schema == "" || s.SubjectType == "":
		return errors.New("command: the tenant provisioning needs the resource type, the permission, the schema of the tenant list and the subject type")
	}
	return nil
}

// errTenantState is the refusal of a change that the tenant list does not take: the code is ErrorCodeProvisioned or
// ErrorCodeNotProvisioned.
type errTenantState struct{ code string }

func (e *errTenantState) Error() string {
	return "command: the tenant list refuses the change: " + e.code
}

// errAnswered ends a provisioning hook whose idempotency wrapper answered without the handler: the change of the
// tenant list rolls back, and the request gets the wrapper's answer.
var errAnswered = errors.New("command: the idempotency key of the request answered it")

// tenantOutcome maps an error of a Provision or Retire to its answer: a deny is 404 denied, as every operation
// answers a deny (ST-SUP-RC29-OC02); a change that the tenant list refuses is 409 with its code; any other error is
// 500 internal. Each is audited as denied or failed (ST-SUP-RC27-OC10).
func tenantOutcome(err error) Outcome {
	var state *errTenantState
	switch {
	case errors.Is(err, ErrDenied):
		return Outcome{Status: http.StatusNotFound, Code: ErrorCodeDenied, Audit: audit.OutcomeDenied}
	case errors.As(err, &state):
		return Outcome{Status: http.StatusConflict, Code: state.code, Audit: audit.OutcomeFailed}
	}
	return Outcome{Status: http.StatusInternalServerError, Code: ErrorCodeInternal, Audit: audit.OutcomeFailed}
}

// ServeTenant gives the HTTP handler of Provision or Retire, POST <base path>/stego/tenants and POST <base
// path>/stego/tenants/{tenantId}/retire (ST-SUP-RC27-OC09). It binds the verified caller before any transaction
// (401), requires an Idempotency-Key (400), takes the tenant ID from the member tenant_id of the body of a Provision
// or the path value tenantId of a Retire and checks it with tenant.ValidID before any decision (400 invalid with the
// field tenant_id). It decides the declared permission on the service-level resource <resource type>:<service
// name>, whose ID is not tenant-qualified (ST-SUP-RP06-LC22), through the authorization port at the floor
// (ST-SUP-RP06-LC17), and only on an allow
// builds the tenant from the decision with tenant.Requested; then tenant.Provision or tenant.Retire changes the
// tenant list with a hook that runs the idempotency wrapper and writes the one audit row of the change in the same
// transaction, with the command as the action, the caller as the actor and the tenant as the resource
// (ST-SUP-RC27-OC10). A deny is 404 and changes nothing. A denied or failed call writes its outcome row in its own
// short transaction in the caller's tenant, so a caller without the permission writes no row into the tenant it
// names; when the caller's own tenant is not an active tenant of the list (Active), it writes no row at all, only a
// counted log line (UnlistedOutcomes). A Provision of a tenant that the list holds, active or retired, and a Retire of a tenant that the list does
// not hold as active are 409, unless the key of the request holds the answer of an earlier call, which is replayed.
// Retire deletes no row (ST-SUP-RC27-OC12).
func ServeTenant(rt *Runtime, spec TenantSpec) (http.Handler, error) {
	if err := spec.check(rt); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeProblem(w, http.StatusMethodNotAllowed, "", "")
			return
		}
		b, err := BindIdentity(r.Context(), spec.SubjectType)
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, "", "")
			return
		}
		key, err := idempotency.KeyFrom(r.Header, true)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, idempotency.ProblemMissing, "")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxTenantBody))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		id, err := requestedTenant(spec.Action, r, body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		if err := tenant.ValidID(id); err != nil {
			writeFieldProblem(w, http.StatusBadRequest, "", ErrorCodeInvalid, TenantIDField)
			return
		}
		fingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		req := Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: id, RequestID: NewID(), ActorIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(),
			Retention: spec.Retention}
		result, err := runTenant(r.Context(), rt, spec, req)
		if err != nil {
			o := tenantOutcome(err)
			if spec.callerListed(r.Context(), b.Tenant) {
				_ = rt.RecordFailure(r.Context(), req, spec.command(), TenantResourceType, o)
			} else {
				rt.unlistedOutcome(r.Context(), spec.command(), o)
			}
			writeProblem(w, o.Status, "", o.AnswerCode())
			return
		}
		if result.Problem != "" {
			writeProblem(w, result.Response.Status, result.Problem, "")
			return
		}
		for name, values := range result.Response.Header {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(result.Response.Body)))
		w.WriteHeader(result.Response.Status)
		_, _ = w.Write(result.Response.Body)
	}), nil
}

// callerListed tells whether the outcome row of a denied or failed call goes into the chain of the caller's tenant
// t: always when the spec has no Active, else only when Active holds t as an active tenant of the list. A failed
// read of the list is not active, so no row is written.
func (s TenantSpec) callerListed(ctx context.Context, t tenant.Tenant) bool {
	if s.Active == nil {
		return true
	}
	active, err := s.Active(ctx, t)
	return err == nil && active
}

// unlistedOutcome counts and logs a denied or failed Provision or Retire of a caller whose tenant is not an active
// tenant of the list, which writes no audit row (ST-SUP-RC27-OC10). The log line holds the command, the outcome and
// the fixed code only, and no tenant ID, principal or tenant that the request names.
func (rt *Runtime) unlistedOutcome(ctx context.Context, command string, o Outcome) {
	otel.SetErrorCode(ctx, o.AnswerCode())
	rt.unlistedOutcomes.Add(1)
	if rt.logger == nil {
		return
	}
	var record otellog.Record
	record.SetSeverity(otellog.SeverityWarn)
	record.SetEventName("command.tenant_outcome.unlisted")
	record.SetBody(attribute.StringValue("a provisioning call of a caller whose tenant is not active wrote no audit row"))
	record.AddAttributes(
		attribute.String("stego.action", command),
		attribute.String("stego.outcome", string(o.Audit)),
		attribute.String("stego.error_code", o.Code),
	)
	rt.logger.Emit(ctx, record)
}

// UnlistedOutcomes returns how many denied or failed provisioning calls of a caller whose tenant is not active wrote
// no audit row (ST-SUP-RC27-OC10).
func (rt *Runtime) UnlistedOutcomes() int64 { return rt.unlistedOutcomes.Load() }

// tenantBody is the body of a Provision request; a Retire takes no body, or one whose tenant_id is its path value.
type tenantBody struct {
	TenantID string `json:"tenant_id"`
}

// requestedTenant gives the tenant ID that a request names: the member tenant_id of the body of a Provision, which
// must be one JSON object with no other member, or the path value tenantId of a Retire.
func requestedTenant(action tenant.RequestAction, r *http.Request, body []byte) (string, error) {
	var named tenantBody
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&named); err != nil {
			return "", ErrBadRequest
		}
		if decoder.More() {
			return "", ErrBadRequest
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return "", ErrBadRequest
		}
	}
	if action == tenant.ProvisionAction {
		return named.TenantID, nil
	}
	id := r.PathValue(TenantPathValue)
	if named.TenantID != "" && named.TenantID != id {
		return "", ErrBadRequest
	}
	return id, nil
}

// runTenant decides the request and changes the tenant list. It gives the answer to send, or the error whose
// outcome the handler answers and audits.
func runTenant(ctx context.Context, rt *Runtime, spec TenantSpec, req Request) (Result, error) {
	decision, err := decideTenant(ctx, rt, spec, req)
	if err != nil {
		return Result{}, err
	}
	target, err := tenant.New(tenant.Requested(spec.Action, req.Resource, decision))
	if err != nil {
		return Result{}, internalf("the decided tenant was not built")
	}
	keyRequest := idempotency.Request{Tenant: target.ID(), Principal: req.Principal.Subject().String(), Key: req.Key,
		Fingerprint: req.Fingerprint, Retention: req.Retention}
	var result Result
	hook := func(ctx context.Context, q tenant.Querier, t tenant.Tenant) error {
		// The audit writer stages the row for the audit sink in the outbox of this transaction, which needs the
		// transaction itself; the hook still neither commits nor rolls back.
		tx, ok := txhold.Of(q)
		if !ok {
			return errors.New("command: the provisioning hook has no transaction of the tenant runtime")
		}
		if _, err := q.ExecContext(ctx, setLockTimeout, fmt.Sprintf("%dms", DefaultLockTimeout.Milliseconds())); err != nil {
			return err
		}
		wrapped, err := idempotency.Wrap(ctx, hookTx{q: q}, keyRequest, func(ctx context.Context) (idempotency.Response, error) {
			if _, err := rt.cfg.Audit.Record(ctx, tx, t, []audit.Event{tenantEvent(ctx, rt, spec, req, t)}); err != nil {
				return idempotency.Response{}, err
			}
			return tenantResponse(spec.Action, t.ID())
		})
		if err != nil {
			return err
		}
		result = Result{Response: wrapped.Response, Problem: wrapped.Problem, Replayed: wrapped.Replayed}
		if !wrapped.Commit {
			return errAnswered
		}
		return nil
	}
	change := tenant.Provision
	unchanged, code := tenant.ErrProvisioned, ErrorCodeProvisioned
	if spec.Action == tenant.RetireAction {
		change, unchanged, code = tenant.Retire, tenant.ErrNotProvisioned, ErrorCodeNotProvisioned
	}
	err = change(ctx, rt.cfg.DB, spec.Schema, target, []tenant.Hook{hook})
	switch {
	case err == nil:
		if spec.Changed != nil {
			spec.Changed(target.ID())
		}
		return result, nil
	case errors.Is(err, errAnswered):
		return result, nil
	case errors.Is(err, unchanged):
		// The list holds the change already: a request that is sent again gets the stored answer of its first call.
		replayed, found, err := replayTenant(ctx, rt, target, keyRequest)
		if err != nil {
			return Result{}, internalf("the idempotency key of the request was not read")
		}
		if found {
			return replayed, nil
		}
		return Result{}, &errTenantState{code: code}
	}
	return Result{}, internalf("the tenant list was not changed")
}

// decideTenant decides the declared permission of the request on the service-level resource <resource type>:<service
// name>, which is of the whole service and not of a tenant, so its ID is not tenant-qualified (authz.ServiceResource,
// ST-SUP-RP06-LC22). It reads the floor in a read-only transaction of the caller's tenant, through a handle bound with
// no relation, and gives the decision that tenant.Requested takes, or ErrDenied (ST-SUP-RC27-OC09,
// ST-SUP-RP06-LC17). A grant on a resource of one tenant, such as service:<tenant>/<service name>, decides nothing.
func decideTenant(ctx context.Context, rt *Runtime, spec TenantSpec, req Request) (tenant.Decision, error) {
	tx, err := tenant.Begin(ctx, rt.cfg.DB, req.Tenant, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		return nil, internalf("the transaction of the decision did not begin")
	}
	defer func() { _ = tx.Rollback() }()
	handle, err := rt.cfg.Authz.Bind(tx, req.Tenant, req.Principal, nil)
	if err != nil {
		return nil, internalf("the authorization handle was not bound")
	}
	resource, err := authz.ServiceResource(spec.ResourceType, rt.cfg.Service)
	if err != nil {
		return nil, internalf("the service-level resource was not built")
	}
	decision, allowed, err := handle.DecideTenantPermission(ctx, spec.Action, spec.Permission, req.Resource, resource, nil)
	switch {
	case err != nil:
		return nil, internalf("the permission of the tenant provisioning was not decided")
	case !allowed:
		return nil, ErrDenied
	}
	return decision, nil
}

// replayTenant reads the key of a request whose change the tenant list holds already, in a read-only transaction of
// the tenant.
func replayTenant(ctx context.Context, rt *Runtime, t tenant.Tenant, req idempotency.Request) (Result, bool, error) {
	tx, err := tenant.Begin(ctx, rt.cfg.DB, t, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Result{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	replayed, found, err := idempotency.Replay(ctx, idempotency.SQLTx(tx), req)
	if err != nil || !found {
		return Result{}, false, err
	}
	return Result{Response: replayed.Response, Problem: replayed.Problem, Replayed: replayed.Replayed}, true, nil
}

// tenantEvent is the audit row of a change of the tenant list: the command as the action, the caller as the actor
// and the tenant as the resource, in the tenant's own chain (ST-SUP-RC27-OC10).
func tenantEvent(ctx context.Context, rt *Runtime, spec TenantSpec, req Request, t tenant.Tenant) audit.Event {
	cfg := rt.cfg
	e := audit.Event{
		TenantID: t.ID(), ActorID: req.Principal.Subject().String(), ActorKind: actorKind(req.Principal),
		ActorIP: audit.Untrusted(req.ActorIP), UserAgent: audit.Untrusted(req.UserAgent),
		Action: spec.command(), Operation: spec.command(), ResourceType: TenantResourceType, ResourceID: t.ID(),
		Outcome: audit.OutcomeOK, RequestID: req.RequestID, IdempotencyKey: req.Key,
		Service: cfg.Service, ServiceVersion: cfg.ServiceVersion, DeclarationVersion: cfg.DeclarationVersion,
	}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		e.TraceID, e.SpanID = span.TraceID().String(), span.SpanID().String()
	}
	return e
}

// tenantResponse is the answer of a committed change: 201 with the tenant ID for a Provision, and 200 with the
// tenant ID and retired true for a Retire.
func tenantResponse(action tenant.RequestAction, id string) (idempotency.Response, error) {
	status, document := http.StatusCreated, map[string]any{TenantIDField: id}
	if action == tenant.RetireAction {
		status, document["retired"] = http.StatusOK, true
	}
	body, err := json.Marshal(document)
	if err != nil {
		return idempotency.Response{}, err
	}
	return idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
}

// hookTx is the transaction of a provisioning hook as the idempotency wrapper takes it. The tenant runtime commits
// and rolls back; the hook cannot.
type hookTx struct{ q tenant.Querier }

func (h hookTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return h.q.ExecContext(ctx, query, args...)
}

func (h hookTx) QueryRowContext(ctx context.Context, query string, args ...any) idempotency.Row {
	return h.q.QueryRowContext(ctx, query, args...)
}

func (hookTx) Rollback() error { return errors.New("command: a provisioning hook does not roll back") }
