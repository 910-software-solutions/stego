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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authn"
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
	// Workers are the names of the declared workers, which the generated code gives (Parts.WorkerNames). A Provision
	// stages, in its transaction and inside the idempotency wrapper of the call, the tuple
	// tenant:<tenant resource ID>#worker_<name in snake case>@service_account:<name> of each, and a Retire stages the
	// delete of each, so no worker holds another's relation and none acts in a retired tenant (ST-SUP-RC35-OC13).
	Workers []string
	// Reader, when it is not nil, reads the relations of the type tenant that SpiceDB serves, and Release is the
	// release of the binary. A Retire then also stages the delete of every tuple of each relation worker_<name> that
	// no declared worker gives, on the tenant resource of the retired tenant, but only when the release identity row
	// is the identity of the release and the workers; the delete of the declared workers' tuples has no fence
	// (ST-SUP-RC35-OC13).
	Reader  WorkerTupleReader
	Release string
	// AdminRelation is the declared relation of ResourceType that a Provision gives each first administrator of the
	// new tenant that its body names, on the tenant-qualified service-level resource of that tenant only, or "" when
	// the provisioning declares none (ST-SUP-RC27-OC13, ST-DECL-EN27).
	AdminRelation string
	// TrustedIssuers are the issuers whose tokens the verifier of the service accepts. A Provision refuses an admin
	// whose issuer is not one of them, so no grant waits for a person whom no trusted issuer can name
	// (ST-SUP-RC27-OC13, security review of PR #414, L1). With an admin relation, it holds at least one issuer.
	TrustedIssuers []string
}

// The members of the body of a Provision that name the first administrators, and their bounds (ST-SUP-RC27-OC13).
const (
	AdminsField     = "admins"
	MaxAdmins       = 10
	maxAdminPartLen = 1024
)

// Admin is one first administrator of a new tenant that the body of a Provision names: the subject (sub) claim of
// the person, and the issuer of the person's tokens, which defaults to the issuer of the verified caller.
type Admin struct {
	Subject string `json:"subject"`
	Issuer  string `json:"issuer,omitempty"`
}

// errAdmins is the refusal of a body whose admins the provisioning does not take: 400 with the field admins.
var errAdmins = errors.New("command: the admins of the provision request are not valid")

// adminSubjects checks the admins of a Provision body against the spec and gives the subject of each, in order: a
// spec with an admin relation needs 1 to MaxAdmins persons, each once, with a subject and an issuer (the caller's
// issuer when it is left out) that are not empty, are at most 1024 bytes and hold no control character; a spec with
// no admin relation, and a Retire, take no admins (ST-SUP-RC27-OC13).
func (s TenantSpec) adminSubjects(admins []Admin, given bool, callerIssuer string) ([]authz.Subject, error) {
	if s.Action != tenant.ProvisionAction || s.AdminRelation == "" {
		if given {
			return nil, errAdmins
		}
		return nil, nil
	}
	if len(admins) == 0 || len(admins) > MaxAdmins {
		return nil, errAdmins
	}
	subjects := make([]authz.Subject, 0, len(admins))
	seen := map[authz.Subject]bool{}
	for _, admin := range admins {
		issuer := admin.Issuer
		if issuer == "" {
			issuer = callerIssuer
		}
		if !adminPart(admin.Subject) || !adminPart(issuer) || !slices.Contains(s.TrustedIssuers, issuer) {
			return nil, errAdmins
		}
		subject, err := authz.PersonSubject(s.SubjectType, issuer, admin.Subject)
		if err != nil || seen[subject] {
			return nil, errAdmins
		}
		seen[subject] = true
		subjects = append(subjects, subject)
	}
	return subjects, nil
}

// adminPart tells whether a subject or an issuer of an admin is not empty, is at most 1024 bytes, is valid UTF-8 and
// holds no control character.
func adminPart(value string) bool {
	if value == "" || len(value) > maxAdminPartLen || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// adminTuples gives the tuple of each first administrator on the tenant-qualified service-level resource of t:
// <resource type>:<base64url tenant>/<base64url service>#<admin relation>@<subject>, never on the unqualified ID of
// the whole service, so the grant allows no Provision or Retire (ST-SUP-RC27-OC13, ST-SUP-Q125).
func (s TenantSpec) adminTuples(t tenant.Tenant, service string, admins []authz.Subject) ([]authz.Tuple, error) {
	if len(admins) == 0 {
		return nil, nil
	}
	resource, err := authz.NewResource(t, s.ResourceType, service)
	if err != nil {
		return nil, err
	}
	tuples := make([]authz.Tuple, len(admins))
	for i, subject := range admins {
		tuples[i] = authz.Tuple{Resource: resource, Relation: s.AdminRelation, Subject: subject}
	}
	return tuples, nil
}

// relateAdmins stages, through an authorization handle bound to the caller in the transaction tx of the Provision of
// t, a touch of the tuple of each first administrator, joined to the revocation of the transaction
// (ST-SUP-RC27-OC13).
func relateAdmins(ctx context.Context, rt *Runtime, spec TenantSpec, tx *sql.Tx, req Request, t tenant.Tenant, admins []authz.Subject) error {
	tuples, err := spec.adminTuples(t, rt.cfg.Service, admins)
	if err != nil || len(tuples) == 0 {
		return err
	}
	handle, err := rt.cfg.Authz.Bind(tx, t, req.Principal, []authz.Relation{{ResourceType: spec.ResourceType, Name: spec.AdminRelation}})
	if err != nil {
		return fmt.Errorf("command: the authorization handle of the admin tuples was not bound: %w", err)
	}
	for _, tuple := range tuples {
		if _, err := handle.Relate(ctx, tuple); err != nil {
			return fmt.Errorf("command: the tuple %s#%s of a first administrator was not staged: %w", tuple.Resource.Type, tuple.Relation, err)
		}
	}
	return nil
}

// WorkerTenantRelation is the relation of the type tenant that holds the tuple of the worker name in each tenant:
// worker_<name in snake case> (ST-SUP-RC35-OC13).
func WorkerTenantRelation(name string) string { return "worker_" + strings.ReplaceAll(name, "-", "_") }

// workerTuples gives the tuple of each worker's relation on the tenant resource of t (ST-SUP-RC35-OC13): the subject
// is the worker's own service account, never a wildcard, and the resource is of t only.
func (s TenantSpec) workerTuples(t tenant.Tenant) ([]authz.Tuple, error) {
	if len(s.Workers) == 0 {
		return nil, nil
	}
	resource, err := authz.TenantResource(t)
	if err != nil {
		return nil, err
	}
	tuples := make([]authz.Tuple, 0, len(s.Workers))
	for _, name := range s.Workers {
		subject, err := authz.ServiceAccount(name)
		if err != nil {
			return nil, fmt.Errorf("command: the worker %q is no service account of a tuple: %w", name, err)
		}
		tuples = append(tuples, authz.Tuple{Resource: resource, Relation: WorkerTenantRelation(name), Subject: subject})
	}
	return tuples, nil
}

// relateWorkers stages, through an authorization handle bound to the caller in the transaction tx of the change of
// t, a touch of each worker's tuple for a Provision and a delete of each for a Retire, joined to the revocation of the
// transaction (ST-SUP-RC35-OC13).
func relateWorkers(ctx context.Context, rt *Runtime, spec TenantSpec, tx *sql.Tx, req Request, t tenant.Tenant) error {
	tuples, err := spec.workerTuples(t)
	if err != nil || len(tuples) == 0 {
		return err
	}
	allowed := make([]authz.Relation, len(tuples))
	for i, tuple := range tuples {
		allowed[i] = authz.Relation{ResourceType: tuple.Resource.Type, Name: tuple.Relation}
	}
	handle, err := rt.cfg.Authz.Bind(tx, t, req.Principal, allowed)
	if err != nil {
		return fmt.Errorf("command: the authorization handle of the worker tuples was not bound: %w", err)
	}
	write := handle.Relate
	if spec.Action == tenant.RetireAction {
		write = handle.Unrelate
	}
	for _, tuple := range tuples {
		if _, err := write(ctx, tuple); err != nil {
			return fmt.Errorf("command: the tuple %s#%s of the worker %s was not staged: %w", tuple.Resource.Type, tuple.Relation, tuple.Subject.ID, err)
		}
	}
	return nil
}

// unrelateUndeclared stages, for a Retire, the delete of every tuple of each relation of undeclared on the tenant
// resource of t, in the transaction tx of the change, only when the release identity row is the identity of the
// binary (ST-SUP-RC35-OC13).
func unrelateUndeclared(ctx context.Context, rt *Runtime, spec TenantSpec, tx *sql.Tx, req Request, t tenant.Tenant, undeclared []string) error {
	if len(undeclared) == 0 {
		return nil
	}
	own := tenant.ReleaseIdentity{Release: spec.Release, Workers: tenant.WorkersDigest(spec.Workers)}
	ok, _, err := fenceIdentity(ctx, rt, tx, spec.Schema, own, RetireCommand)
	if err != nil || !ok {
		return err
	}
	resource, err := authz.TenantResource(t)
	if err != nil {
		return err
	}
	allowed := make([]authz.Relation, len(undeclared))
	for i, relation := range undeclared {
		allowed[i] = authz.Relation{ResourceType: resource.Type, Name: relation}
	}
	handle, err := rt.cfg.Authz.Bind(tx, t, req.Principal, allowed)
	if err != nil {
		return fmt.Errorf("command: the authorization handle of the undeclared worker tuples was not bound: %w", err)
	}
	for _, relation := range undeclared {
		if _, err := handle.UnrelateAll(ctx, resource, relation); err != nil {
			return fmt.Errorf("command: the delete of the relation %s of the retired tenant was not staged: %w", relation, err)
		}
	}
	return nil
}

// undeclaredRelations reads, for a Retire with a reader, the relations worker_<name> of the type tenant that no
// declared worker gives.
func (s TenantSpec) undeclaredRelations(ctx context.Context) ([]string, error) {
	if s.Action != tenant.RetireAction || s.Reader == nil {
		return nil, nil
	}
	relations, err := s.Reader.RelationsOf(ctx, authz.TenantType)
	if err != nil {
		return nil, err
	}

	declared := map[string]bool{}
	for _, name := range s.Workers {
		declared[WorkerTenantRelation(name)] = true
	}
	var undeclared []string
	for _, relation := range relations {
		if strings.HasPrefix(relation, WorkerRelationPrefix) && !declared[relation] {
			undeclared = append(undeclared, relation)
		}
	}
	return undeclared, nil
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
	if s.AdminRelation != "" && len(s.TrustedIssuers) == 0 {
		return errors.New("command: the admin relation of the tenant provisioning needs the trusted issuers of the verifier")
	}
	if s.AdminRelation != "" {
		// The relation tenant and each worker relation are the runtime's own (ST-DECL-INV95).
		if s.AdminRelation == authz.TenantRelation || strings.HasPrefix(s.AdminRelation, WorkerRelationPrefix) {
			return fmt.Errorf("command: the admin relation %q of the tenant provisioning is a relation of the runtime", s.AdminRelation)
		}
		if _, err := authz.ParseRelation(s.ResourceType + "#" + s.AdminRelation); err != nil {
			return fmt.Errorf("command: the admin relation %q of the tenant provisioning is no relation of %s: %w", s.AdminRelation, s.ResourceType, err)
		}
	}
	relations := map[string]string{}
	for _, name := range s.Workers {
		// Two workers with one relation would hold each other's grant (ST-SUP-RC35-OC13).
		if other, taken := relations[WorkerTenantRelation(name)]; taken {
			return fmt.Errorf("command: the workers %q and %q of the tenant provisioning both give the relation tenant#%s, so each would hold the other's grant", other, name, WorkerTenantRelation(name))
		}
		relations[WorkerTenantRelation(name)] = name
		if _, err := authz.ServiceAccount(name); err != nil {
			return fmt.Errorf("command: the worker %q of the tenant provisioning is no service account that a tuple can hold: %w", name, err)
		}
		if _, err := authz.ParseRelation(authz.TenantType + "#" + WorkerTenantRelation(name)); err != nil {
			return fmt.Errorf("command: the worker %q of the tenant provisioning gives no relation of the type tenant: %w", name, err)
		}
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
		id, named, err := requestedTenant(spec.Action, r, body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		if err := tenant.ValidID(id); err != nil {
			writeFieldProblem(w, http.StatusBadRequest, "", ErrorCodeInvalid, TenantIDField)
			return
		}
		var callerIssuer string
		if identity, ok := authn.FromContext(r.Context()); ok {
			callerIssuer = identity.Issuer()
		}
		// The admins are checked after the decision (runTenant), so a caller who may not provision learns nothing
		// of them, such as whether the provisioning declares an admin relation (security review of PR #414, L3).
		admins := func() ([]authz.Subject, error) {
			return spec.adminSubjects(named.Admins, named.Admins != nil, callerIssuer)
		}
		fingerprint, err := idempotency.Fingerprint(r.Method, r.URL.Path, body)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "", "")
			return
		}
		req := Request{Tenant: b.Tenant, Principal: b.Principal, Key: key, Fingerprint: fingerprint, Body: body,
			Resource: id, RequestID: NewID(), ActorIP: remoteIP(r.RemoteAddr), UserAgent: r.UserAgent(),
			Retention: spec.Retention}
		result, err := runTenant(r.Context(), rt, spec, req, admins)
		if errors.Is(err, errAdmins) {
			writeFieldProblem(w, http.StatusBadRequest, "", ErrorCodeInvalid, AdminsField)
			return
		}
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

// undeclaredReadFailed logs the one warn line of a Retire whose read of the undeclared worker relations failed: the
// command and the class of the failure only, and no tenant.
func (rt *Runtime) undeclaredReadFailed(ctx context.Context) {
	if rt.logger == nil {
		return
	}
	var record otellog.Record
	record.SetSeverity(otellog.SeverityWarn)
	record.SetEventName("command.worker_tuples.read_failed")
	record.SetBody(attribute.StringValue("the retire did not read the undeclared worker relations; it deletes the tuples of its declared workers, and the reconcile of the worker tuples deletes the others later"))
	record.AddAttributes(
		attribute.String("stego.job", RetireCommand),
		attribute.String("stego.error_class", ReconcileSchemaRead),
	)
	rt.logger.Emit(ctx, record)
}

// UnlistedOutcomes returns how many denied or failed provisioning calls of a caller whose tenant is not active wrote
// no audit row (ST-SUP-RC27-OC10).
func (rt *Runtime) UnlistedOutcomes() int64 { return rt.unlistedOutcomes.Load() }

// tenantBody is the body of a Provision request; a Retire takes no body, or one whose tenant_id is its path value.
type tenantBody struct {
	TenantID string  `json:"tenant_id"`
	Admins   []Admin `json:"admins"`
}

// requestedTenant gives the tenant ID that a request names: the member tenant_id of the body of a Provision, which
// must be one JSON object with no other member, or the path value tenantId of a Retire.
func requestedTenant(action tenant.RequestAction, r *http.Request, body []byte) (string, tenantBody, error) {
	var named tenantBody
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&named); err != nil {
			return "", tenantBody{}, ErrBadRequest
		}
		if decoder.More() {
			return "", tenantBody{}, ErrBadRequest
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return "", tenantBody{}, ErrBadRequest
		}
	}
	if action == tenant.ProvisionAction {
		return named.TenantID, named, nil
	}
	id := r.PathValue(TenantPathValue)
	if named.TenantID != "" && named.TenantID != id {
		return "", tenantBody{}, ErrBadRequest
	}
	return id, named, nil
}

// runTenant decides the request and changes the tenant list. It gives the answer to send, or the error whose
// outcome the handler answers and audits.
func runTenant(ctx context.Context, rt *Runtime, spec TenantSpec, req Request, adminsOf func() ([]authz.Subject, error)) (Result, error) {
	decision, err := decideTenant(ctx, rt, spec, req)
	if err != nil {
		return Result{}, err
	}
	admins, err := adminsOf()
	if err != nil {
		return Result{}, err
	}
	target, err := tenant.New(tenant.Requested(spec.Action, req.Resource, decision))
	if err != nil {
		return Result{}, internalf("the decided tenant was not built")
	}
	keyRequest := idempotency.Request{Tenant: target.ID(), Principal: req.Principal.Subject().String(), Key: req.Key,
		Fingerprint: req.Fingerprint, Retention: req.Retention}
	// The undeclared worker relations are read before the transaction, so no call to SpiceDB holds it open. A
	// schema with no definition tenant has none. A failed read never fails the Retire: it still deletes the tuples of
	// its declared workers and retires the tenant, and only the extra deletes are left to the reconcile, with one
	// warn line (security re-review of PR #379, H1).
	undeclared, err := spec.undeclaredRelations(ctx)
	switch {
	case errors.Is(err, authz.ErrNoDefinition):
		undeclared = nil
	case err != nil:
		undeclared = nil
		rt.undeclaredReadFailed(ctx)
	}
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
		// The lock of the worker tuples of t, which the reconcile takes too, comes first: before the key of the
		// request and the chain lock of the audit row, which is the last lock of the transaction (ST-SUP-RP10-LC06),
		// in the order of the reconcile, so the two never wait on each other (security review of PR #379, M1).
		if len(spec.Workers) > 0 || len(undeclared) > 0 {
			if _, err := q.ExecContext(ctx, workerTuplesLock, t.ID()); err != nil {
				return fmt.Errorf("command: the lock of the worker tuples of the tenant was not taken: %w", err)
			}
		}
		wrapped, err := idempotency.Wrap(ctx, hookTx{q: q}, keyRequest, func(ctx context.Context) (idempotency.Response, error) {
			event, err := tenantEvent(ctx, rt, spec, req, t, len(admins))
			if err != nil {
				return idempotency.Response{}, err
			}
			if _, err := rt.cfg.Audit.Record(ctx, tx, t, []audit.Event{event}); err != nil {
				return idempotency.Response{}, err
			}
			// The grants of the first administrators commit with the new tenant, or not at all; a replay of the key
			// stages none again (ST-SUP-RC27-OC13).
			if err := relateAdmins(ctx, rt, spec, tx, req, t, admins); err != nil {
				return idempotency.Response{}, err
			}
			// The tuples of the workers commit with the change of the tenant list, or not at all; a replay of the
			// key stages none again (ST-SUP-RC35-OC13).
			if err := relateWorkers(ctx, rt, spec, tx, req, t); err != nil {
				return idempotency.Response{}, err
			}
			if err := unrelateUndeclared(ctx, rt, spec, tx, req, t, undeclared); err != nil {
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
// and the tenant as the resource, in the tenant's own chain (ST-SUP-RC27-OC10). A Provision that grants first
// administrators records the admin relation and their count as its after state, and no subject or issuer
// (ST-SUP-RC27-OC13).
func tenantEvent(ctx context.Context, rt *Runtime, spec TenantSpec, req Request, t tenant.Tenant, admins int) (audit.Event, error) {
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
	if admins > 0 {
		after := map[string]any{"admin_relation": spec.AdminRelation, AdminsField: admins}
		fields := audit.Fields{"admin_relation": audit.Plain, AdminsField: audit.Plain}
		if err := e.SetStates(cfg.AuditSettings, fields, nil, after); err != nil {
			return audit.Event{}, err
		}
	}
	return e, nil
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
