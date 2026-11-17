// Package placeorder is the fill of the command place-order of the test service: it creates one Order with no
// parent. The command is decided on the service-level resource of the tenant (ST-SUP-RC32-OC15), so its handle
// offers only the create and the read.
package placeorder

import (
	"context"
	"errors"

	"github.com/jsell-rh/stego/registry/runtime/command"

	commands "example.com/shop/out/commands"
)

// New gives the fill of the command place-order.
func New() commands.PlaceOrderFill {
	return func(ctx context.Context, request *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		order, err := h.CreateOrder(ctx, commands.Order{Sku: request.Sku, Qty: request.Qty, Email: request.Email})
		if err != nil {
			return nil, err
		}
		// A note on another order reads that order, which its read permission decides, and the create of the note
		// decides the permission of its reference order_id on it (ST-SUP-RP10-LC05). Each deny is not found, which
		// the fill answers with its declared refusal.
		if request.NoteOn != "" {
			on, err := h.GetOrder(ctx, request.NoteOn)
			if errors.Is(err, command.ErrNotFound) {
				return nil, commands.PlaceOrderNoOrder
			}
			if err != nil {
				return nil, err
			}
			_, err = h.CreateNote(ctx, commands.Note{OrderID: request.NoteOn, Text: request.Note}, on)
			if errors.Is(err, command.ErrNotFound) {
				return nil, commands.PlaceOrderNoOrder
			}
			if err != nil {
				return nil, err
			}
		}
		return &commands.PlaceOrderResponse{OrderId: order.ID()}, nil
	}
}
