package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
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
	t.Run("ST-SUP-RP03-LC16-T06 STEGO_LOG_LEVEL sets the lowest level of the log/slog lines, and a value that is not a level is refused", func(t *testing.T) {
		for _, c := range []struct {
			value string
			want  []string
		}{
			{"", []string{"INFO", "WARN", "ERROR"}},
			{"info", []string{"INFO", "WARN", "ERROR"}},
			{"debug", []string{"DEBUG", "INFO", "WARN", "ERROR"}},
			{"warn", []string{"WARN", "ERROR"}},
			{"error", []string{"ERROR"}},
		} {
			level, err := ParseLogLevel(c.value)
			if err != nil {
				t.Fatalf("the level %q gave %v", c.value, err)
			}
			var out bytes.Buffer
			logger := slog.New(NewSlogHandlerAt(&out, level)).With("worker", "sync")
			logger.Debug("a debug line")
			logger.Info("an info line")
			logger.Warn("a warn line")
			logger.Error("an error line")
			var got []string
			for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
				if line == "" {
					continue
				}
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				got = append(got, record["severity"].(string))
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Fatalf("STEGO_LOG_LEVEL=%q wrote %v, want %v", c.value, got, c.want)
			}
		}
		if _, err := ParseLogLevel("verbose-private"); err == nil || strings.Contains(err.Error(), "verbose-private") {
			t.Fatalf("a value that is not a level gave %v, want a refusal that does not hold the value", err)
		}
	})
	t.Run("ST-SUP-RP03-LC16-T07 A line of the security class is written at each level, and the runtime marks each line of the class", func(t *testing.T) {
		var out bytes.Buffer
		logger := slog.New(NewSlogHandlerAt(&out, slog.LevelError))
		ctx := context.Background()
		logger.WarnContext(ctx, "a warn line")
		logger.WarnContext(Security(ctx), "a security warn line")
		logger.InfoContext(Security(ctx), "a security info line")
		logger.DebugContext(Security(ctx), "a security debug line")
		got := out.String()
		if strings.Contains(got, "a warn line") || !strings.Contains(got, "a security warn line") || !strings.Contains(got, "a security info line") || strings.Contains(got, "debug") {
			t.Fatalf("STEGO_LOG_LEVEL=error wrote %s, want the security lines of INFO and WARN only", got)
		}
		// Each line of the class in the runtime logs with a context that Security marks.
		for file, line := range map[string]string{
			"../audit/auditsink/table.go": `slog.WarnContext(otel.Security(ctx), "auditsink: the default sink holds another entry`,
			"../idempotency/wrap.go":      `slog.WarnContext(otel.Security(ctx), "idempotency: a stored answer does not open`,
			"../command/authz.go":         `slog.WarnContext(otel.Security(ctx), "command: the authorization provider could not decide"`,
			"../query/authz.go":           `slog.WarnContext(otel.Security(ctx), "query: the authorization provider could not decide the read"`,
			"../command/seal_job.go":      `slog.InfoContext(otel.Security(ctx), "command: seal of a secret column"`,
			"../command/sealed.go":        `slog.InfoContext(otel.Security(ctx), "command: re-seal of a secret column"`,
		} {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), line) {
				t.Errorf("%s does not write its line of the security class with otel.Security: %s", file, line)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC16-T04 A log/slog line holds at most 12 attributes of at most 256 characters each", func(t *testing.T) {
		var out bytes.Buffer
		logger := slog.New(NewSlogHandler(&out))
		args := []any{"long", strings.Repeat("é", 300)}
		for i := range 14 {
			args = append(args, fmt.Sprintf("a%02d", i), i)
		}
		logger.Warn("command: a line with many attributes", args...)
		var got struct {
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("the line is not JSON: %v\n%s", err, out.String())
		}
		if len(got.Attributes) != SlogAttributeLimit {
			t.Errorf("the line has %d attributes, want %d: %v", len(got.Attributes), SlogAttributeLimit, got.Attributes)
		}
		if long, _ := got.Attributes["stego.long"].(string); long != strings.Repeat("é", SlogValueLimit) {
			t.Errorf("the long value has %d characters, want %d", len([]rune(long)), SlogValueLimit)
		}
		if _, ok := got.Attributes["stego.a11"]; ok {
			t.Error("the line holds the 13th attribute")
		}
	})
	t.Run("ST-SUP-RP03-LC16-T05 A log/slog line of code outside the runtime does not name its event", func(t *testing.T) {
		var out bytes.Buffer
		h := NewSlogHandler(&out)
		// A record with no program counter, as of code outside the packages of the runtime, such as a fill.
		for _, r := range []slog.Record{slog.NewRecord(time.Now(), slog.LevelError, "command.attempts.failed", 0),
			slog.NewRecord(time.Now(), slog.LevelError, "a fill line", 0)} {
			r.AddAttrs(slog.String(SlogEventKey, "command.attempts.failed"))
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if !strings.Contains(line, `"event.name":"stego.log"`) || !strings.Contains(line, `"stego.event.name":"command.attempts.failed"`) {
				t.Errorf("a line of code outside the runtime names an event of the runtime: %s", line)
			}
		}
	})
}
