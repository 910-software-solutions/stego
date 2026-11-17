// Package placeorder is the fill of the command place-order of the test service: it creates one Order with no
// parent. The command is decided on the service-level resource of the tenant (ST-SUP-RC32-OC15), so its handle
// offers only the create and the read.
package placeorder

import (
	"context"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command place-order.
func New() commands.PlaceOrderFill {
	return func(ctx context.Context, request *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		order, err := h.CreateOrder(ctx, commands.Order{Sku: request.Sku, Qty: request.Qty, Email: request.Email})
		if err != nil {
			return nil, err
		}
		return &commands.PlaceOrderResponse{OrderId: order.ID()}, nil
	}
}
