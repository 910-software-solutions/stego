// Package syncpartnerorders is the fill of the worker sync-partner-orders of the test service: at each tick it adds
// one to the qty of the order SyncedOrder in each tenant whose ID starts with org-w, so the check sees how many ticks
// ran in each tenant.
package syncpartnerorders

import (
	"context"
	"strings"
	"time"

	workers "example.com/shop/out/workers"
)

// SyncedOrder is the ID of the order that the check seeds in each tenant of the worker and grants the worker on.
const SyncedOrder = "5b7c2f0e-3a1d-4c8e-9f60-1d2e3f4a5b6c"

// New gives the fill of the worker sync-partner-orders.
func New() workers.SyncPartnerOrdersFill {
	return func(ctx context.Context, tick time.Time, h *workers.SyncPartnerOrdersHandle) error {
		if !strings.HasPrefix(h.Tenant().ID(), "org-w") {
			return nil
		}
		orders, err := workers.OrderRepository(h)
		if err != nil {
			return err
		}
		// A tenant without the seeded order, or without the grant of the worker on it, has nothing to sync: the read
		// is not found. The slot contract names no error kind, so the fill ends each failed read with no change.
		order, err := orders.Get(ctx, SyncedOrder)
		if err != nil {
			return nil
		}
		value := order.Value()
		value.Qty++
		_, err = orders.Update(ctx, order, value)
		return err
	}
}
