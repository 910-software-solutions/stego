package otel

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSlogHandler(t *testing.T) {
	t.Run("ST-SUP-RP03-LC16-T02 Each log/slog line of the runtime is one JSON line with its event name and stego attributes", func(t *testing.T) {
		var out bytes.Buffer
		logger := slog.New(NewSlogHandler(&out))
		logger.Info("command: seal of a secret column", SlogEventKey, "command.seal.pass", "change", "customer-iban-seal",
			"sealed", 12, "completed", true)
		logger.Error("migrate: the step is refused", "step", "ledger_check", "refusal", "the ledger row of the migration 001_initial holds\nanother digest")
		logger.Warn("command.fill.failed", "stego.action", "verify-pin")
		logger.With("worker", "sync").WithGroup("pool").Info("lease: the release report works again", "in_use", 4)
		logger.Debug("a debug line is not written")
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("got %d lines, want 4:\n%s", len(lines), out.String())
		}
		type line struct {
			Timestamp  string         `json:"timestamp"`
			Severity   string         `json:"severity"`
			Event      string         `json:"event.name"`
			Message    string         `json:"message"`
			Attributes map[string]any `json:"attributes"`
		}
		want := []line{
			{Severity: "INFO", Event: "command.seal.pass", Message: "command: seal of a secret column",
				Attributes: map[string]any{"stego.change": "customer-iban-seal", "stego.sealed": float64(12), "stego.completed": true}},
			{Severity: "ERROR", Event: SlogEvent, Message: "migrate: the step is refused",
				Attributes: map[string]any{"stego.step": "ledger_check", "stego.refusal": "the ledger row of the migration 001_initial holds\nanother digest"}},
			{Severity: "WARN", Event: "command.fill.failed", Message: "command.fill.failed", Attributes: map[string]any{"stego.action": "verify-pin"}},
			{Severity: "INFO", Event: SlogEvent, Message: "lease: the release report works again",
				Attributes: map[string]any{"stego.worker": "sync", "stego.pool.in_use": float64(4)}},
		}
		for i, text := range lines {
			var got line
			if err := json.Unmarshal([]byte(text), &got); err != nil {
				t.Fatalf("line %d is not JSON: %v\n%s", i, err, text)
			}
			if got.Timestamp == "" || got.Severity != want[i].Severity || got.Event != want[i].Event || got.Message != want[i].Message {
				t.Errorf("line %d is %s, want the severity %s, the event %s and the message %q", i, text, want[i].Severity, want[i].Event, want[i].Message)
			}
			if len(got.Attributes) != len(want[i].Attributes) {
				t.Errorf("line %d has the attributes %v, want %v", i, got.Attributes, want[i].Attributes)
			}
			for k, v := range want[i].Attributes {
				if got.Attributes[k] != v {
					t.Errorf("line %d has %s = %v, want %v", i, k, got.Attributes[k], v)
				}
			}
		}
		// A value of the event attribute that is not an event name stays an attribute, and the line keeps the default
		// event name.
		out.Reset()
		logger.Info("a line", SlogEventKey, "Not An Event", "cause", errors.New("internal"))
		if text := out.String(); !strings.Contains(text, `"event.name":"stego.log"`) || !strings.Contains(text, `"stego.event.name":"Not An Event"`) ||
			!strings.Contains(text, `"stego.cause":"internal"`) {
			t.Errorf("the line of a bad event name is %s", text)
		}
	})
}
