package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/910-software-solutions/stego/registry/runtime/internal/logline"
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
	r.console = newConsole(out, "records", "instance-1", clock.Now, nil, consoleSkip(false))
	if err := r.startSignals(nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

// dropped is the count stego.dropped of a count line, and nil for another line.
func dropped(line map[string]any) any {
	attrs, _ := line["attributes"].(map[string]any)
	return attrs["stego.dropped"]
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
		logger := r.LoggerProvider().Logger("github.com/910-software-solutions/stego/registry/runtime/command")
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
			attrs["stego.worker"] != "ship-order" || attrs["stego.attempt"] != float64(3) || attrs["stego.cause"] != "fill_error" || len(attrs) != len(consumer) ||
			first["service.name"] != "records" || first["service.instance.id"] != "instance-1" {
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
		if last["event.name"] != "kafka.delivery_failed" || dropped(last) != float64(burst-ConsoleLimit) || len(lines) != 1+ConsoleLimit+1 {
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
				if !slices.Contains([]string{"timestamp", "severity", "service.name", "service.instance.id", "event.name", "message", "attributes"}, key) {
					t.Fatalf("a console line holds the key %q", key)
				}
			}
		}
	})
	t.Run("ST-SUP-RP03-LC16-T01 The ERROR lines of an event name have a budget of their own, so a WARN burst keeps none off", func(t *testing.T) {
		out := &lockedBuffer{}
		clock := &consoleClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		r := consoleRuntime(t, out, clock)
		logger := r.LoggerProvider().Logger("github.com/910-software-solutions/stego/registry/runtime/service")
		// A burst of 4xx completions at WARN over the limit, then 5xx completions of the same event name at ERROR and
		// FATAL (security review of PR #421, I1).
		const warnings = ConsoleLimit + 5
		for range warnings {
			emitRecord(logger, otellog.SeverityWarn, "http.server.request", "a request completed", attribute.Int64("status", 404))
		}
		emitRecord(logger, otellog.SeverityError, "http.server.request", "a request failed", attribute.Int64("status", 500))
		emitRecord(logger, otellog.SeverityFatal, "http.server.request", "a request failed", attribute.Int64("status", 503))
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		severities := map[string]int{}
		for _, line := range out.lines(t) {
			severities[line["severity"].(string)]++
		}
		if severities["WARN"] != ConsoleLimit || severities["ERROR"] != 1 || severities["FATAL"] != 1 {
			t.Fatalf("the console wrote %v, want %d WARN lines and the ERROR and FATAL lines after the WARN burst", severities, ConsoleLimit)
		}
		// The ERROR budget has its own limit, and its own count line, of the severity ERROR.
		for range ConsoleLimit {
			emitRecord(logger, otellog.SeverityError, "http.server.request", "a request failed", attribute.Int64("status", 500))
		}
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.move(ConsoleWindow)
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		var counts []map[string]any
		errors := 0
		for _, line := range out.lines(t) {
			if dropped(line) != nil {
				counts = append(counts, line)
			} else if line["severity"] != "WARN" {
				errors++
			}
		}
		if errors != ConsoleLimit || len(counts) != 2 || counts[0]["severity"] != "WARN" || dropped(counts[0]) != float64(warnings-ConsoleLimit) ||
			counts[1]["severity"] != "ERROR" || dropped(counts[1]) != float64(2) {
			t.Fatalf("the console wrote %d ERROR and FATAL lines and the count lines %v, want %d and a WARN count of %d and an ERROR count of 2",
				errors, counts, ConsoleLimit, warnings-ConsoleLimit)
		}
	})
	t.Run("ST-SUP-RP03-LC16-T01 A full console queue drops the line, counts it, and never blocks the record", func(t *testing.T) {
		out := &blockedWriter{release: make(chan struct{})}
		clock := &consoleClock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
		c := newConsole(out, "records", "instance-1", clock.Now, nil, consoleSkip(false))
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
		if line["event.name"] == event && dropped(line) == nil {
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

// text gives the raw lines written so far, each with its line feed.
func (b *lockedBuffer) text() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.SplitAfter(b.buf.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestLineForm(t *testing.T) {
	t.Run("ST-SUP-RP03-LC16-T08 Each line of the console and of log/slog has the one form of the lines of the runtime", func(t *testing.T) {
		out := &lockedBuffer{}
		clock := &consoleClock{now: time.Date(2026, 11, 22, 7, 29, 43, 822602285, time.FixedZone("EST", -5*3600))}
		r := consoleRuntime(t, out, clock)
		logger := r.LoggerProvider().Logger("github.com/910-software-solutions/stego/registry/runtime/command")
		// A record with attributes of the runtime and of the semantic conventions, one with no attribute, and a burst
		// over the limit, which gives the count line at the end of the window.
		emitRecord(logger, otellog.SeverityWarn, "command.consumer.run_failed", "a consuming worker's run did not commit",
			attribute.String("worker", "ship-order"), attribute.Int64("attempt", 3), attribute.Bool("retry", true),
			attribute.Float64("duration_seconds", 0.25), attribute.String("http.route", "/orders/{id}"), attribute.String("error.type", "500"))
		emitRecord(logger, otellog.SeverityError, "kafka.delivery_failed", "a Kafka delivery failed")
		for range ConsoleLimit + 2 {
			emitRecord(logger, otellog.SeverityFatal, "command.fill.failed", "a fill failed")
		}
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.move(ConsoleWindow)
		if err := r.console.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		// The lines of log/slog: of the runtime with an event name, with a message that is an event name, of a group,
		// with a span, and with no identity and with one.
		var slogOut lockedBuffer
		h := NewSlogHandlerAt(&slogOut, slog.LevelDebug)
		logger2 := slog.New(h)
		logger2.Info("the migration job started", SlogEventKey, "migrate.job.started")
		h.SetIdentity("records", "instance-1")
		logger2.Debug("command.fill.failed", "action", "ship", "held_back", 2)
		logger2.Warn("command: a sealed value does not open", SlogEventKey, "command.seal.unopened", slog.Group("seal", "entity", "order"))
		logger2.With("mode", "api").Error("plain text of a fill")
		sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5}, TraceFlags: trace.FlagsSampled})
		logger2.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "the server listens", SlogEventKey, "service.listening", "address", "[::]:8080", "at", clock.Now())

		lines := append(out.text(), slogOut.text()...)
		if len(lines) != 1+1+ConsoleLimit+1+5 {
			t.Fatalf("the console and log/slog wrote %d lines, want %d: %q", len(lines), 1+1+ConsoleLimit+1+5, lines)
		}
		events := map[string]bool{}
		for _, line := range lines {
			if err := logline.Check([]byte(line)); err != nil {
				t.Fatalf("the line %q: %v", line, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatal(err)
			}
			events[decoded["event.name"].(string)] = true
			if !strings.HasSuffix(decoded["timestamp"].(string), "Z") {
				t.Fatalf("the line %q has no UTC timestamp", line)
			}
		}
		for _, want := range []string{"command.consumer.run_failed", "kafka.delivery_failed", "command.fill.failed", "migrate.job.started", "command.seal.unopened", "stego.log", "service.listening"} {
			if !events[want] {
				t.Fatalf("no line has the event %s: %v", want, events)
			}
		}
		// The form refuses the shapes that walkthrough 25 found: fields at the top level, fields under fields, a
		// local offset, a key with no prefix, and no service name.
		for _, bad := range []string{
			`{"http.request.method":"GET","timestamp":"2026-11-22T02:54:43.322614534-05:00","severity":"WARN","service.name":"companies","event.name":"http.server.request.completed","message":"HTTP request completed"}` + "\n",
			`{"timestamp":"2026-11-22T07:29:43.822602285Z","severity":"ERROR","service.name":"procurement","event.name":"worker.run.failed","message":"m","fields":{"worker":"record-jobs"}}` + "\n",
			`{"timestamp":"2026-11-22T07:29:43.822602285-05:00","severity":"INFO","service.name":"s","event.name":"stego.log","message":"m"}` + "\n",
			`{"timestamp":"2026-11-22T07:29:43.822602285Z","severity":"INFO","service.name":"s","event.name":"a.b","message":"m","attributes":{"worker":"w"}}` + "\n",
			`{"timestamp":"2026-11-22T07:27:13.205904421Z","severity":"INFO","event.name":"stego.log","message":"starting server on [::]:30830"}` + "\n",
		} {
			if logline.Check([]byte(bad)) == nil {
				t.Fatalf("the form takes the line %q", bad)
			}
		}
	})
}
