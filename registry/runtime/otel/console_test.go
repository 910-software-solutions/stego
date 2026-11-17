package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// consoleClock is a clock that only the test moves.
type consoleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *consoleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *consoleClock) move(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lockedBuffer is standard output for a test: the writer goroutine writes it and the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines gives each line written so far, decoded as JSON.
func (b *lockedBuffer) lines(t testing.TB) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, text := range strings.Split(strings.TrimSuffix(b.buf.String(), "\n"), "\n") {
		if text == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("a console line is not one JSON object: %q", text)
		}
		out = append(out, line)
	}
	return out
}

// consoleRuntime is a runtime with no collector whose console writes to out with the hand clock clock, as New
// starts it with no OTLP endpoint.
func consoleRuntime(t *testing.T, out *lockedBuffer, clock *consoleClock) *Runtime {
	t.Helper()
	r := &Runtime{service: "records", instance: "00000000-0000-4000-8000-000000000000"}
	r.resource = serviceResource(r.service, r.instance)
	r.console = newConsole(out, clock.Now, nil, consoleSkip(false))
	if err := r.startSignals(nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func emitRecord(logger otellog.Logger, severity otellog.Severity, event, body string, attrs ...attribute.KeyValue) {
	var record otellog.Record
	record.SetEventName(event)
	record.SetSeverity(severity)
	record.SetBody(attribute.StringValue(body))
	record.AddAttributes(attrs...)
	logger.Emit(context.Background(), record)
}

func TestConsole(t *testing.T) {
	t.Run("ST-SUP-RP03-LC16-T01 A warning record reaches the console with no OTLP endpoint, at most at the rate limit", func(t *testing.T) {
		clearEnvironment(t)
		started, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		if started.CollectorSet() || started.LoggerProvider() == nil || started.console == nil || started.TracerProvider() != nil {
			t.Fatal("a runtime with no OTLP endpoint has no console logger provider, or has a collector")
		}
		started.Close()

		out := &lockedBuffer{}
		clock := &consoleClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		r := consoleRuntime(t, out, clock)
		logger := r.LoggerProvider().Logger("github.com/jsell-rh/stego/registry/runtime/command")
		consumer := []attribute.KeyValue{attribute.String("worker", "ship-order"), attribute.String("destination", "orders"),
			attribute.String("event_id", "0190a1b2-0000-7000-8000-000000000001"), attribute.Int64("attempt", 3),
			attribute.String("outcome", "failed"), attribute.String("cause", "fill_error")}
		emitRecord(logger, otellog.SeverityWarn, "command.consumer.run_failed", "a consuming worker's run did not commit", consumer...)
		emitRecord(logger, otellog.SeverityInfo, "command.consumer.run_ok", "a run committed")
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines := out.lines(t)
		if len(lines) != 1 {
			t.Fatalf("the console holds %d lines, want 1 for the WARN record and none for the INFO record", len(lines))
		}
		first := lines[0]
		attrs, _ := first["attributes"].(map[string]any)
		if first["event.name"] != "command.consumer.run_failed" || first["severity"] != "WARN" ||
			first["message"] != "a consuming worker's run did not commit" || first["timestamp"] == "" ||
			attrs["worker"] != "ship-order" || attrs["attempt"] != float64(3) || attrs["cause"] != "fill_error" || len(attrs) != len(consumer) {
			t.Fatalf("the line of the WARN record is %v", first)
		}

		// A burst of one event name over the limit, in one window.
		const burst = ConsoleLimit + 7
		for range burst {
			emitRecord(logger, otellog.SeverityError, "kafka.delivery_failed", "a Kafka delivery failed", attribute.String("destination", "orders"))
		}
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := countEvent(out.lines(t), "kafka.delivery_failed"); got != ConsoleLimit {
			t.Fatalf("the console wrote %d lines of the burst, want the limit %d", got, ConsoleLimit)
		}
		clock.move(ConsoleWindow)
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines = out.lines(t)
		last := lines[len(lines)-1]
		if last["event.name"] != "kafka.delivery_failed" || last["dropped"] != float64(burst-ConsoleLimit) || len(lines) != 1+ConsoleLimit+1 {
			t.Fatalf("after the window the last line is %v of %d lines, want the count %d of the dropped lines", last, len(lines), burst-ConsoleLimit)
		}
		// The window of the consumer event dropped nothing, so it has no count line; a new window writes again.
		emitRecord(logger, otellog.SeverityError, "kafka.delivery_failed", "a Kafka delivery failed", attribute.String("destination", "orders"))
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines = out.lines(t)
		if got := countEvent(lines, "kafka.delivery_failed"); got != ConsoleLimit+1 {
			t.Fatalf("a new window wrote %d lines of the event in all, want %d", got, ConsoleLimit+1)
		}
		// No line holds a key other than the fixed keys and the attributes of its record.
		for _, line := range lines {
			for key := range line {
				if !slices.Contains([]string{"timestamp", "severity", "event.name", "message", "attributes", "dropped"}, key) {
					t.Fatalf("a console line holds the key %q", key)
				}
			}
		}
	})
	t.Run("ST-SUP-RP03-LC16-T01 A full console queue drops the line, counts it, and never blocks the record", func(t *testing.T) {
		out := &blockedWriter{release: make(chan struct{})}
		clock := &consoleClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		c := newConsole(out, clock.Now, nil, consoleSkip(false))
		r := &Runtime{service: "records", instance: "00000000-0000-4000-8000-000000000000"}
		r.resource = serviceResource(r.service, r.instance)
		r.console = c
		if err := r.startSignals(nil, nil); err != nil {
			t.Fatal(err)
		}
		logger := r.LoggerProvider().Logger("stego/outbox")
		// The writer blocks on the first line, so the queue fills: each emit returns, and the lines over the queue
		// are counted.
		const sent = ConsoleQueue + 50
		for i := range sent {
			emitRecord(logger, otellog.SeverityWarn, "outbox.parked."+string(rune('a'+i%20)), "an outbox message is parked")
		}
		if r.ConsoleDropped() == 0 {
			t.Fatal("a full console queue dropped no line")
		}
		close(out.release)
		r.Close()
	})
	t.Run("ST-SUP-RP03-LC16-T01 The probe scope always, and the controller scope with no collector, stay off the console", func(t *testing.T) {
		out := &lockedBuffer{}
		clock := &consoleClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		r := consoleRuntime(t, out, clock)
		for _, scope := range []string{probeScope, controllerScope} {
			emitRecord(r.LoggerProvider().Logger(scope), otellog.SeverityWarn, "worker.run.failed", "a run failed")
		}
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if lines := out.lines(t); len(lines) != 0 {
			t.Fatalf("the console wrote %d lines of a skipped scope", len(lines))
		}
		if skip := consoleSkip(true); skip(controllerScope) || !skip(probeScope) {
			t.Fatal("with a collector the controller scope is not on the console, or the probe scope is")
		}
	})
}

func countEvent(lines []map[string]any, event string) int {
	n := 0
	for _, line := range lines {
		if line["event.name"] == event && line["dropped"] == nil {
			n++
		}
	}
	return n
}

// blockedWriter blocks each write until release is closed.
type blockedWriter struct{ release chan struct{} }

func (w *blockedWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}
