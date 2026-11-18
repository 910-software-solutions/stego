// Package verifyorder is the fill of the command verify-order of the test service: it reads the readable secret of
// the order, which the command handle opens, and checks the compare-only secret, which it never reads.
package verifyorder

import (
	"context"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command verify-order.
func New() commands.VerifyOrderFill {
	return func(ctx context.Context, request *commands.VerifyOrderRequest, h *commands.VerifyOrderHandle) (*commands.VerifyOrderResponse, error) {
		orders, err := commands.OrderRepository(h)
		if err != nil {
			return nil, err
		}
		order, err := orders.Get(ctx, request.OrderId)
		if err != nil {
			return nil, err
		}
		v := order.Value()
		// Both checks take constant time for values of one length.
		return &commands.VerifyOrderResponse{TokenOk: v.ApiToken.Equal(request.Token), PinOk: v.Pin.Matches(request.Pin)}, nil
	}
}
