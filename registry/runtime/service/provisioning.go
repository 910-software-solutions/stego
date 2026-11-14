package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Provisioning is the declared tenant provisioning of a pooled service (ST-DECL-EN27), from its two entries of the
// operation-to-permission table (ST-SUP-RC27-OC09, ST-SUP-RC29-OC15): the resource type of the service-level
// resource, the permission of Provision and of Retire on it, and the route of each.
type Provisioning struct {
	ResourceType      string
	Provision, Retire string
	// ProvisionRoute is POST <base path>/stego/tenants, and RetireRoute POST <base path>/stego/tenants/{tenantId}/retire.
	ProvisionRoute, RetireRoute string
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
	return nil
}

// tenantProvisioning is what New makes of the declared provisioning: the gate of the tenant list, the routes that
// the gate lets through, and the handlers of Provision and Retire.
type tenantProvisioning struct {
	gate        *tenant.Gate
	exempt      *http.ServeMux
	marker      *exemptRoute
	provision   http.Handler
	retire      http.Handler
	problemType string
}

// newProvisioning makes the gate and the handlers of the declared provisioning p on the command runtime rt, whose
// pool reads the tenant list in schema.
func newProvisioning(svc *Service, rt *command.Runtime, p *Provisioning, schema string) (*tenantProvisioning, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	gate, err := tenant.NewGate(svc.pool, schema, tenant.MaxGateAge)
	if err != nil {
		return nil, err
	}
	tp := &tenantProvisioning{gate: gate, exempt: http.NewServeMux(), problemType: svc.parts.ProblemType}
	tp.marker = &exemptRoute{}
	tp.exempt.Handle(p.ProvisionRoute, tp.marker)
	tp.exempt.Handle(p.RetireRoute, tp.marker)
	spec := command.TenantSpec{ResourceType: p.ResourceType, Schema: schema, SubjectType: svc.parts.SubjectType,
		Changed: gate.Forget}
	spec.Action, spec.Permission = tenant.ProvisionAction, p.Provision
	if tp.provision, err = command.ServeTenant(rt, spec); err != nil {
		return nil, err
	}
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
// verified identity or no tenant claim, a tenant that is absent or retired, and a failed read of the list, so the
// check fails closed.
func (tp *tenantProvisioning) admit(ctx context.Context) error {
	identity, ok := authn.FromContext(ctx)
	if !ok {
		return errors.New("service: the request has no verified identity")
	}
	t, err := tenant.New(tenant.Claim(identity))
	if err != nil {
		return err
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

// exempted tells whether r is a request of one of the two provisioning routes, as a ServeMux with only those two
// routes matches it. A path that the ServeMux would clean or redirect first is not exempt.
func (tp *tenantProvisioning) exempted(r *http.Request) bool {
	h, pattern := tp.exempt.Handler(r)
	route, ok := h.(*exemptRoute)
	return pattern != "" && ok && route == tp.marker
}

// gated serves next only for a request whose verified tenant is an active tenant of the tenant list, after the
// verifier and before any handler, decision or fill; the two provisioning routes are exempt, because they are
// decided on the service-level resource and not on a tenant (ST-SUP-RC27-OC11). It answers any other request with
// 403 and the code tenant.not_provisioned, or 500 when the list cannot be read, with Cache-Control no-store.
func (tp *tenantProvisioning) gated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp.exempted(r) {
			next.ServeHTTP(w, r)
			return
		}
		switch err := tp.admit(r.Context()); {
		case err == nil:
			next.ServeHTTP(w, r)
		case errors.Is(err, errNotProvisioned):
			writeTenantProblem(w, http.StatusForbidden, tp.problemType, tenant.ErrNotProvisionedCode)
		default:
			writeTenantProblem(w, http.StatusInternalServerError, tp.problemType, command.ErrorCodeInternal)
		}
	})
}

// writeTenantProblem writes the problem response (RFC 9457) of a refused tenant, with the fixed code only.
func writeTenantProblem(w http.ResponseWriter, httpStatus int, problemType, code string) {
	if problemType == "" {
		problemType = "about:blank"
	}
	body := "{\"type\":" + strconv.Quote(problemType) + ",\"title\":" + strconv.Quote(http.StatusText(httpStatus)) +
		",\"status\":" + strconv.Itoa(httpStatus) + ",\"code\":" + strconv.Quote(code) + "}"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(httpStatus)
	_, _ = io.WriteString(w, body)
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
	default:
		return nil, status.Error(codes.Internal, command.ErrorCodeInternal)
	}
}
