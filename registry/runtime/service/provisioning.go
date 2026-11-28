package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/command"
	"github.com/910-software-solutions/stego/registry/runtime/otel"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// Provisioning is the declared tenant provisioning of a pooled service (ST-DECL-EN27), from its two entries of the
// operation-to-permission table (ST-SUP-RC27-OC09, ST-SUP-RC29-OC15): the resource type of the service-level
// resource, the permission of Provision and of Retire on it, and the route of each.
type Provisioning struct {
	ResourceType      string
	Provision, Retire string
	// ProvisionRoute is POST <base path>/stego/tenants, and RetireRoute POST <base path>/stego/tenants/{tenantId}/retire.
	ProvisionRoute, RetireRoute string
	// AdminRelation is the declared relation of ResourceType that Provision gives each first administrator of a new
	// tenant, or "" when the provisioning declares none (ST-SUP-RC27-OC13, ST-DECL-EN27).
	AdminRelation string
	// List is the permission list_tenants of ResourceType when the schema declares it, and ListRoute GET <base
	// path>/stego/tenants, the tenant list of the operator; with no List, the service serves no tenant list
	// (ST-SUP-RC27-OC16).
	List, ListRoute string
}

// The path of the provisioning routes after the base path.
const (
	TenantsPath     = "/stego/tenants"
	RetireTailPath  = "/{" + command.TenantPathValue + "}/retire"
	tenantRoutesTag = "POST "
)

// check refuses a provisioning that lacks a part, or whose routes are not POST <base path>/stego/tenants and POST
// <base path>/stego/tenants/{tenantId}/retire of one base path.
func (p *Provisioning) check() error {
	if p.ResourceType == "" || p.Provision == "" || p.Retire == "" {
		return errors.New("service: the tenant provisioning needs the resource type and the Provision and Retire permissions")
	}
	base, ok := strings.CutPrefix(p.ProvisionRoute, tenantRoutesTag)
	if !ok || !strings.HasPrefix(base, "/") || !strings.HasSuffix(base, TenantsPath) ||
		p.RetireRoute != p.ProvisionRoute+RetireTailPath || strings.ContainsAny(base, " {}") {
		return fmt.Errorf("service: the provisioning routes must be POST <base path>%s and POST <base path>%s%s", TenantsPath, TenantsPath, RetireTailPath)
	}
	if p.ListRoute != "" && p.ListRoute != "GET "+base {
		return fmt.Errorf("service: the route of the tenant list must be GET <base path>%s", TenantsPath)
	}
	if p.List != "" && p.ListRoute == "" {
		return errors.New("service: the tenant list needs its route")
	}
	return nil
}

// tenantProvisioning is what New makes of the declared provisioning: the gate of the tenant list, the routes that
// the gate lets through, and the handlers of Provision and Retire.
type tenantProvisioning struct {
	gate      *tenant.Gate
	exempt    *http.ServeMux
	marker    *exemptRoute
	patterns  map[string]bool
	provision http.Handler
	retire    http.Handler
	// list is the tenant list of the operator, or nil when the schema declares no list_tenants (ST-SUP-RC27-OC16).
	list        http.Handler
	problemType string
	// rt is the command runtime of Provision and Retire, on which the controller mode reconciles the worker tuples
	// (ST-SUP-RC35-OC13).
	rt *command.Runtime
	// service is the resource type of the provisioning when the schema declares the relation service of the type
	// tenant, whose tuple the reconcile touches on each active tenant, and else empty (decision ST-SUP-Q220 (a)).
	service string
}

// newProvisioning makes the handlers of the declared provisioning p on the command runtime rt, whose tenant list in
// schema gate reads.
func newProvisioning(svc *Service, rt *command.Runtime, p *Provisioning, schema string, gate *tenant.Gate) (*tenantProvisioning, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, errors.New("service: the tenant provisioning needs the gate of the tenant list")
	}
	tp := &tenantProvisioning{gate: gate, exempt: http.NewServeMux(), problemType: svc.parts.ProblemType, rt: rt,
		patterns: map[string]bool{p.ProvisionRoute: true, p.RetireRoute: true}}
	tp.marker = &exemptRoute{}
	tp.exempt.Handle(p.ProvisionRoute, tp.marker)
	tp.exempt.Handle(p.RetireRoute, tp.marker)
	if p.ListRoute != "" {
		// The tenant list is decided on the service-level resource as Provision and Retire are, so the tenant check
		// exempts it too (ST-SUP-RC27-OC16).
		tp.patterns[p.ListRoute] = true
		tp.exempt.Handle(p.ListRoute, tp.marker)
	}
	// Provision writes the tuple of each declared worker's relation on the new tenant, and Retire deletes it
	// (ST-SUP-RC35-OC13).
	spec := command.TenantSpec{ResourceType: p.ResourceType, Schema: schema, SubjectType: svc.parts.SubjectType,
		Changed: gate.Forget, Active: gate.Active, Workers: svc.parts.WorkerNames, Release: svc.parts.Release}
	if svc.provider != nil && svc.parts.declaresTenant() {
		// A Retire also deletes the tuples of undeclared workers, under the fence of the release identity row.
		spec.Reader = svc.provider
	}
	// A Retire deletes the tuples of each tenant-qualified type of the schema (ST-SUP-RC27-OC15).
	for _, definition := range svc.parts.Definitions {
		if definition != svc.parts.SubjectType && definition != authz.ServiceAccountType && definition != p.ResourceType &&
			definition != authz.DefaultMarker.Resource.Type {
			spec.TenantTypes = append(spec.TenantTypes, definition)
		}
	}
	// A schema that declares the relation service of the type tenant gets the tuple that relates the tenant resource
	// to the service-level resource of the tenant (ST-SUP-RC27-OC18, ST-SUP-RP10-LC52).
	spec.ServiceRelation = slices.Contains(svc.parts.Relations, authz.Relation{ResourceType: authz.TenantType, Name: command.TenantServiceRelation})
	if spec.ServiceRelation {
		tp.service = p.ResourceType
	}
	var err error
	spec.Action, spec.Permission, spec.AdminRelation = tenant.ProvisionAction, p.Provision, p.AdminRelation
	if p.AdminRelation != "" {
		// An admin of another issuer than the one the verifier trusts could never call (security review of PR #414, L1).
		spec.TrustedIssuers = []string{svc.issuer}
	}
	if tp.provision, err = command.ServeTenant(rt, spec); err != nil {
		return nil, err
	}
	// A Retire takes no admins (ST-SUP-RC27-OC13), and deletes the tuples of the admin relation of the tenant
	// (security review of PR #414, L4).
	spec.Action, spec.Permission = tenant.RetireAction, p.Retire
	if tp.retire, err = command.ServeTenant(rt, spec); err != nil {
		return nil, err
	}
	return tp, nil
}

// errNotProvisioned is the refusal of a request whose verified claim names a tenant that is not an active tenant of
// the tenant list (ST-SUP-RC27-OC11).
var errNotProvisioned = errors.New("service: the tenant of the request is not provisioned")

// admit checks the tenant of the verified identity of ctx against the tenant list: it refuses a context with no
// verified identity, a tenant that is absent or retired, and a failed read of the list, so the check fails closed. A
// verified identity whose tenant claim is missing or is not a valid tenant ID names no tenant of the list, so it is
// refused as not provisioned, as an absent tenant is, and not as an internal error.
func (tp *tenantProvisioning) admit(ctx context.Context) error {
	identity, ok := authn.FromContext(ctx)
	if !ok {
		return errors.New("service: the request has no verified identity")
	}
	t, err := tenant.New(tenant.Claim(identity))
	if err != nil {
		return errNotProvisioned
	}
	active, err := tp.gate.Active(ctx, t)
	if err != nil {
		return err
	}
	if !active {
		return errNotProvisioned
	}
	return nil
}

// exemptRoute marks the two provisioning routes in the ServeMux of exempted. It is a pointer, so the handler that
// the ServeMux gives compares equal to it only for those routes.
type exemptRoute struct{}

func (*exemptRoute) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

// exempted tells whether r is a request of one of the two provisioning routes: a ServeMux with only those two
// routes matches it, so a path that a ServeMux would clean or redirect first is not exempt, and the main router of
// the service, router, matches it to the pattern of Provision or of Retire, the pattern that r.Pattern holds when the
// router serves it. So a route of another handler that the router holds under <base path>/stego/, which would match
// first, is not exempt. With no router, no request is exempt.
func (tp *tenantProvisioning) exempted(router *http.ServeMux, r *http.Request) bool {
	if router == nil {
		return false
	}
	h, pattern := tp.exempt.Handler(r)
	route, ok := h.(*exemptRoute)
	if pattern == "" || !ok || route != tp.marker {
		return false
	}
	_, matched := router.Handler(r)
	return tp.patterns[matched]
}

// unrouted tells whether the main router, router, matches r to no route: the router then answers it itself, 404 for
// a path that no route holds or 405 for a path whose routes take another method, and the tenant check has nothing to
// decide, so it does not run (ST-SUP-RC27-OC14). With no router, every request is routed.
func unrouted(router *http.ServeMux, r *http.Request) bool {
	if router == nil {
		return false
	}
	_, pattern := router.Handler(r)
	return pattern == ""
}

// notRouted serves a request that the main router matches to no route: the answer of the router, 404 or 405 with its
// Allow header, in the one problem form of the service (ST-SUP-RC27-OC14, ST-SUP-RC27-OC17); any other answer of
// the router, such as the redirect of a path that it cleans, goes out as the router gave it.
func (tp *tenantProvisioning) notRouted(router *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	h, _ := router.Handler(r)
	answer := &routerAnswer{header: http.Header{}}
	h.ServeHTTP(answer, r)
	if answer.status == http.StatusNotFound || answer.status == http.StatusMethodNotAllowed {
		if allow := answer.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeTenantProblem(w, answer.status, tp.problemType, "")
		return
	}
	for name, values := range answer.header {
		w.Header()[name] = values
	}
	if answer.status == 0 {
		answer.status = http.StatusOK
	}
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body.Bytes())
}

// routerAnswer keeps the answer of the main router to a request of no route, which is small: a status, its headers
// and a short body.
type routerAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *routerAnswer) Header() http.Header { return a.header }

func (a *routerAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

func (a *routerAnswer) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if a.body.Len()+len(b) > maxRouterAnswer {
		return len(b), nil
	}
	return a.body.Write(b)
}

// maxRouterAnswer bounds the body of the router's own answer that notRouted keeps.
const maxRouterAnswer = 4096

// TenantGateAction is the action of the warn line command.database.unavailable of the tenant gate: the step is http
// or grpc, the transport of the call.
const TenantGateAction = "tenant-gate"

// gateUnavailableLog holds back the warn lines of the tenant gate to one for each transport in each authz.LogEvery.
var gateUnavailableLog = authz.NewCauseLog(nil)

// gated serves next only for a request whose verified tenant is an active tenant of the tenant list, after the
// verifier and before any handler, decision or fill; the two provisioning routes of the main router are exempt,
// because they are decided on the service-level resource and not on a tenant (ST-SUP-RC27-OC11), and a request that
// the main router matches to no route gets the answer of the router (ST-SUP-RC27-OC14). It answers any other request with 403 and the code tenant.not_provisioned, also a verified token with no valid tenant claim, 503
// with Retry-After and the code unavailable when the database cannot serve the list now, or 500 when the list cannot
// be read for another cause, with Cache-Control no-store.
func (tp *tenantProvisioning) gated(router *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp.exempted(router, r) {
			next.ServeHTTP(w, r)
			return
		}
		if unrouted(router, r) {
			tp.notRouted(router, w, r)
			return
		}
		switch err := tp.admit(r.Context()); {
		case err == nil:
			next.ServeHTTP(w, r)
		case errors.Is(err, errNotProvisioned):
			refuseTenant(w, r, http.StatusForbidden, tp.problemType, tenant.ErrNotProvisionedCode)
		case tenant.Unavailable(err):
			// A tenant list that the database cannot serve now is 503 with Retry-After, never a bare 500
			// (walkthrough 10, B7; ST-SUP-RP10-LC43), with the warn line of its SQLSTATE class (walkthrough 29,
			// SQL53-LOG).
			command.LogUnavailable(r.Context(), gateUnavailableLog, TenantGateAction, "http", err)
			w.Header().Set("Retry-After", authz.RetryAfter())
			refuseTenant(w, r, http.StatusServiceUnavailable, tp.problemType, command.ErrorCodeUnavailable)
		default:
			refuseTenant(w, r, http.StatusInternalServerError, tp.problemType, command.ErrorCodeInternal)
		}
	})
}

// refuseTenant writes the problem response of a refused tenant, as writeTenantProblem does, and gives its fixed code
// to the access line of the request (otel.SetErrorCode): the tenant check answers before the router, so the line has
// no route, and the code tells an operator why (erp-demo production run, ST-SUP-Q210).
func refuseTenant(w http.ResponseWriter, r *http.Request, httpStatus int, base, code string) {
	otel.SetErrorCode(r.Context(), code)
	writeTenantProblem(w, httpStatus, base, code)
}

// writeTenantProblem writes the problem response (RFC 9457) of a refused tenant, with the fixed code only, for the
// declared problem base.
func writeTenantProblem(w http.ResponseWriter, httpStatus int, base, code string) {
	// The type is the type of the code under the declared problem base, never the bare base (ST-SUP-RC27-OC17).
	body := "{\"type\":" + strconv.Quote(authn.ProblemTypeOf(base, httpStatus, code)) + ",\"title\":" + strconv.Quote(http.StatusText(httpStatus)) +
		",\"status\":" + strconv.Itoa(httpStatus)
	if code != "" {
		body += ",\"code\":" + strconv.Quote(code)
	}
	body += "}"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(httpStatus)
	_, _ = io.WriteString(w, body)
}

// TenantList gives the handler of the tenant list of the operator, GET <base path>/stego/tenants, which the
// generated code registers at the route of its entry of the operation-to-permission table, or nil for a service whose
// schema declares no list_tenants (ST-SUP-RC27-OC16).
func (svc *Service) TenantList() http.Handler {
	if svc.tenants == nil || svc.tenants.list == nil {
		return nil
	}
	return svc.tenants.list
}

// TenantRoutes gives the handlers of Provision and Retire, which the generated code registers at the routes of their
// entries of the operation-to-permission table (ST-SUP-RC27-OC09), or nil for a service that declares no
// provisioning.
func (svc *Service) TenantRoutes() (provision, retire http.Handler) {
	if svc.tenants == nil {
		return nil, nil
	}
	return svc.tenants.provision, svc.tenants.retire
}

// Authenticate verifies the credential of a gRPC call and, for a service that declares its provisioning, refuses a
// call whose verified tenant is not an active tenant of the tenant list with PERMISSION_DENIED and the code
// tenant.not_provisioned before any handler (ST-SUP-RC27-OC11). The transport answers any other error as
// UNAUTHENTICATED.
func (svc *Service) Authenticate(ctx context.Context, credential string) (context.Context, error) {
	verified, err := svc.verifier.Authenticate(ctx, credential)
	if err != nil {
		return nil, err
	}
	if svc.tenants == nil {
		return verified, nil
	}
	switch err := svc.tenants.admit(verified); {
	case err == nil:
		return verified, nil
	case errors.Is(err, errNotProvisioned):
		return nil, status.Error(codes.PermissionDenied, tenant.ErrNotProvisionedCode)
	case tenant.Unavailable(err):
		command.LogUnavailable(ctx, gateUnavailableLog, TenantGateAction, "grpc", err)
		return nil, status.Error(codes.Unavailable, command.ErrorCodeUnavailable)
	default:
		return nil, status.Error(codes.Internal, command.ErrorCodeInternal)
	}
}
