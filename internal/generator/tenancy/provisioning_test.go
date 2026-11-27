package tenancy

import (
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestProvisioningRoutes(t *testing.T) {
	t.Run("ST-SUP-RC27-OC09-T02 The provisioning routes are under the base path, and a declared route under /stego/ is refused", func(t *testing.T) {
		for _, base := range []string{"/api/shop/v1", "/api/shop/v1/"} {
			provision, retire := ProvisioningRoutes(base)
			if provision != "POST /api/shop/v1/stego/tenants" || retire != "POST /api/shop/v1/stego/tenants/{tenantId}/retire" {
				t.Errorf("the base path %q gives the routes %q and %q", base, provision, retire)
			}
		}
		for _, route := range []string{"POST /stego/tenants", "GET /stego", "POST /stego/tenants/{tenantId}/retire", "GET /stego/other"} {
			if err := CheckOwnRoute(route); err == nil {
				t.Errorf("the declared route %q under /stego/ is accepted", route)
			}
		}
		for _, route := range []string{"POST /orders", "GET /stegosaurus", "GET /orders/stego/tenants"} {
			if err := CheckOwnRoute(route); err != nil {
				t.Errorf("the declared route %q is refused: %v", route, err)
			}
		}
		if err := CheckOwnRoute("/stego"); err == nil {
			t.Error("a route with no method is accepted")
		}
		// A route of a component holds the base path, such as the route of a collection with a path_prefix.
		for _, pattern := range []string{"POST /api/shop/v1/stego/tenants/x/retire", "GET /api/shop/v1/stego", "/api/shop/v1/stego/other"} {
			if err := CheckOwnPattern("/api/shop/v1/", pattern); err == nil {
				t.Errorf("the component route %q under /stego/ is accepted", pattern)
			}
		}
		for _, pattern := range []string{"POST /api/shop/v1/orders", "GET /api/shop/v1/stegosaurus", "GET /stego/tenants", "GET /livez"} {
			if err := CheckOwnPattern("/api/shop/v1", pattern); err != nil {
				t.Errorf("the component route %q is refused: %v", pattern, err)
			}
		}
	})
}

func TestAdminRelation(t *testing.T) {
	t.Run("ST-SUP-RC27-OC13-T03 The generated Provision takes the declared admin relation, and none with no provisioning or no admin", func(t *testing.T) {
		for _, c := range []struct {
			p    *types.TenantProvisioning
			want string
		}{
			{nil, ""},
			{&types.TenantProvisioning{ResourceType: "service", Provision: "provision", Retire: "retire"}, ""},
			{&types.TenantProvisioning{ResourceType: "service", Provision: "provision", Retire: "retire", Admin: "admin"}, "admin"},
		} {
			if got := AdminRelation(c.p); got != c.want {
				t.Errorf("AdminRelation(%+v) = %q, want %q", c.p, got, c.want)
			}
		}
	})
}
