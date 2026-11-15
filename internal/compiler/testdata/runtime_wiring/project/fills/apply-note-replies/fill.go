// Package applynotereplies is the fill of the worker apply-note-replies of the test service: it consumes the message
// that the command add-note stages and changes nothing, so the check sees the message done.
package applynotereplies

import (
	"context"
	"encoding/json"

	workers "example.com/shop/out/workers"
)

// New gives the fill of the worker apply-note-replies.
func New() workers.ApplyNoteRepliesFill {
	return func(ctx context.Context, message json.RawMessage, effect string, h *workers.ApplyNoteRepliesHandle) error {
		return nil
	}
}
