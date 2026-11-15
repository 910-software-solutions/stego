// Package applynotereplies is the fill of the worker apply-note-replies of the test service: it consumes the message
// that the command add-note stages, and adds 1000 to the qty of the order of the message when the worker may edit
// it, so the check sees the write of the consumer commit in the transaction of the worker role (ST-SUP-Q141).
package applynotereplies

import (
	"context"
	"encoding/json"

	workers "example.com/shop/out/workers"
)

// New gives the fill of the worker apply-note-replies.
func New() workers.ApplyNoteRepliesFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.ApplyNoteRepliesHandle) error {
		var note struct {
			Text  string `json:"text"`
			Order string `json:"order"`
		}
		if err := json.Unmarshal(message, &note); err != nil || note.Order == "" {
			return nil
		}
		orders, err := workers.OrderRepository(h)
		if err != nil {
			return err
		}
		// An order that the worker may not edit is not found: the message is done with no change. Only the order
		// that the check grants the worker on gets the write; its message, with the text consume, is tried again
		// until the decision sees the grant.
		order, err := orders.Get(ctx, note.Order)
		if err != nil {
			if note.Text == "consume" {
				return err
			}
			return nil
		}
		value := order.Value()
		value.Qty += 1000
		_, err = orders.Update(ctx, order, value)
		return err
	}
}
