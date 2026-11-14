package tenancy

import "testing"

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
	})
}
