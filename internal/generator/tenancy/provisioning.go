package tenancy

import (
	"fmt"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// The paths of Provision and Retire after the base path of the service (ST-SUP-RC27-OC09). The path value tenantId
// is the one that the command runtime of the runtime library reads.
const (
	ProvisionPath = "/stego/tenants"
	RetirePath    = ProvisionPath + "/{tenantId}/retire"
)

// The operations of Provision and Retire in the operation-to-permission table, which the audit rows of the runtime
// library name as their action (ST-SUP-RC27-OC10, ST-SUP-RC29-OC15).
const (
	ProvisionOperation = "provision-tenant"
	RetireOperation    = "retire-tenant"
)

// ownPrefix is the path under the base path that the generated service keeps for its own routes.
const ownPrefix = "/stego"

// ProvisioningRoutes gives the routes of Provision and Retire of a service whose base path is basePath: POST <base
// path>/stego/tenants and POST <base path>/stego/tenants/{tenantId}/retire (ST-SUP-RC27-OC09). The emitted router
// registers them from their entries of the operation-to-permission table only.
func ProvisioningRoutes(basePath string) (provision, retire string) {
	base := strings.TrimSuffix(basePath, "/")
	return "POST " + base + ProvisionPath, "POST " + base + RetirePath
}

// CheckOwnPattern refuses a route pattern of a component, METHOD /path or /path with the base path, such as the
// route of a collection that the REST API registers, whose path is under <base path>/stego/, which the generated
// service keeps for its own routes: a route there would match a request of Provision or Retire, or one near them,
// and answer it with a handler that the tenant check does not expect (ST-SUP-RC27-OC09, ST-SUP-RC27-OC11).
func CheckOwnPattern(basePath, pattern string) error {
	target := pattern
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		target = rest
	}
	own := strings.TrimSuffix(basePath, "/") + ownPrefix
	if target == own || strings.HasPrefix(target, own+"/") {
		return fmt.Errorf("the route %q is under %s/, which the generated service keeps for its own routes, such as the tenant provisioning; give it another path", pattern, own)
	}
	return nil
}

// CheckOwnRoute refuses a declared route, METHOD /path after the base path, that is under /stego/, which the
// generated service keeps for its own routes, such as Provision and Retire: a declared route there would take a
// path of the service or answer it with a handler the tenant check does not expect (ST-SUP-RC27-OC09,
// ST-SUP-RC27-OC11).
func CheckOwnRoute(route string) error {
	_, target, ok := strings.Cut(route, " ")
	if !ok {
		return fmt.Errorf("the route %q has no method and path", route)
	}
	if target == ownPrefix || strings.HasPrefix(target, ownPrefix+"/") {
		return fmt.Errorf("the route %q is under %s/, which the generated service keeps for its own routes, such as the tenant provisioning; give it another path", route, ownPrefix)
	}
	return nil
}

// AdminRelation gives the relation that the generated Provision gives each first administrator of a new tenant: the
// declared admin relation of the provisioning p, or "" when p is nil or declares none, so that Provision takes no
// administrators (ST-SUP-RC27-OC13). Checking holds the relation to the schema first (ST-DECL-INV95).
func AdminRelation(p *types.TenantProvisioning) string {
	if p == nil {
		return ""
	}
	return p.Admin
}
