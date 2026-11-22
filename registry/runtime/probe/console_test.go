package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/internal/logline"
	"github.com/jsell-rh/stego/registry/runtime/otel"
)

// childSetting runs a test again as a child process, which writes to its real standard output.
const childSetting = "STEGO_PROBE_TEST_CHILD"

// runChild runs the subtest pattern of the test binary again as a child process with the settings, and returns its
// standard output and standard error.
func runChild(t *testing.T, pattern string, settings ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+pattern, "-test.count=1")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "STEGO_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, settings...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the child process failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// socketCount counts the open sockets of this process.
func socketCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("the process has no /proc/self/fd to count sockets")
	}
	count := 0
	for _, entry := range entries {
		if target, err := os.Readlink("/proc/self/fd/" + entry.Name()); err == nil && strings.HasPrefix(target, "socket:") {
			count++
		}
	}
	return count
}

func TestProbeConsole(t *testing.T) {
	t.Run("ST-SUP-RP12-LC07-T01 The console sink writes one JSON or logfmt line per emission to standard output", func(t *testing.T) {
		if os.Getenv(childSetting) == "console" {
			telemetry, err := otel.New("orders", SpanNames()...)
			if err != nil {
				os.Exit(2)
			}
			r, err := Start(telemetry, nil)
			if err != nil {
				os.Exit(3)
			}
			call := r.Bind(unsampled(), "t1", "user:u-7", command)
			call.Probe().Emit(reservationRefused(t, CountsAttempt), Text("A-1"), Enum("short"))
			call.Probe().Emit(reservationsRefused(t, CountsAttempt), Enum("expired"))
			call.Probe().Emit(histogram(t, "reservation_wait", Milliseconds), Number(12.5))
			call.Returned()
			call.Commit()
			r.Close(context.Background())
			os.Exit(0)
		}
		sc := unsampledSpanContext()
		common := []string{"timestamp", "severity", "service.name", "service.instance.id", "event.name", "message", "stego.outcome", "trace_id", "span_id", "stego.tenant", "stego.principal", "stego.operation"}
		for _, setting := range []string{"json", "logfmt"} {
			stdout, stderr := runChild(t, "^TestProbeConsole$/^ST-SUP-RP12-LC07-T01_", childSetting+"=console", consoleSetting+"="+setting)
			if stderr != "" {
				t.Fatalf("%s: the child wrote %q to standard error", setting, stderr)
			}
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			if len(lines) != 4 || !strings.Contains(lines[3], `"event.name":"telemetry.runtime.stopped"`) {
				t.Fatalf("%s: the child wrote %d lines, want 3 and the line of telemetry.runtime.stopped: %q", setting, len(lines), stdout)
			}
			lines = lines[:3]
			for i, line := range lines {
				values := map[string]string{}
				if setting == "json" {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(line), &decoded); err != nil {
						t.Fatalf("the line %q is not one JSON object: %v", line, err)
					}
					for k, v := range decoded {
						if attrs, ok := v.(map[string]any); ok {
							for name, value := range attrs {
								values[name] = strings.Trim(strings.TrimSpace(jsonText(value)), `"`)
							}
							continue
						}
						values[k] = strings.Trim(jsonText(v), `"`)
					}
				} else {
					for _, pair := range strings.Fields(line) {
						k, v, ok := strings.Cut(pair, "=")
						if !ok {
							t.Fatalf("the logfmt line %q has the pair %q", line, pair)
						}
						values[k] = strings.Trim(v, `"`)
					}
				}
				for _, key := range common {
					if values[key] == "" {
						t.Fatalf("%s: the line %q has no %s", setting, line, key)
					}
				}
				if values["service.name"] != "orders" || values["stego.tenant"] != "t1" || values["stego.principal"] != "user:u-7" || values["stego.operation"] != command || values["trace_id"] != sc.TraceID().String() || values["span_id"] != sc.SpanID().String() {
					t.Fatalf("%s: the line %q has the wrong correlation", setting, line)
				}
				switch i {
				case 0:
					if values["event.name"] != "domain.reservation_refused" || values["severity"] != "WARN" || values["stego.field.sku"] != "A-1" || values["stego.field.reason"] != "short" {
						t.Fatalf("%s: the event line is %q", setting, line)
					}
				case 1:
					if values["event.name"] != "domain.reservations_refused" || values["stego.field.reason"] != "expired" {
						t.Fatalf("%s: the counter line is %q", setting, line)
					}
				case 2:
					if values["event.name"] != "domain.reservation_wait" || values["stego.value"] != "12.5" {
						t.Fatalf("%s: the histogram line is %q", setting, line)
					}
				}
			}
		}
	})

	t.Run("ST-SUP-RP12-LC18-T01 The console sink starts no provider and opens no connection", func(t *testing.T) {
		clearEnvironment(t)
		before := socketCount(t)
		telemetry := newTelemetry(t)
		stdout := &lines{}
		r, err := start(telemetry, nil, "json", stdout)
		if err != nil {
			t.Fatal(err)
		}
		emitCalls(r, reservationRefused(t, CountsAttempt), 10)
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(stdout.all()) != 10 {
			t.Fatalf("the console holds %d lines, want 10", len(stdout.all()))
		}
		// The logger provider of the telemetry runtime has only its console (ST-SUP-RP03-LC16), which opens no
		// connection; the probe starts no provider of its own.
		if telemetry.TracerProvider() != nil || telemetry.MeterProvider() != nil || telemetry.CollectorSet() || r.otlp != nil {
			t.Fatal("a provider or the OTLP sink was started with no collector")
		}
		if after := socketCount(t); after != before {
			t.Fatalf("the process has %d sockets, %d before the probe", after, before)
		}
	})
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func unsampledSpanContext() trace.SpanContext { return trace.SpanContextFromContext(unsampled()) }

func TestProbeLineForm(t *testing.T) {
	t.Run("ST-SUP-RP12-LC22-T02 Each JSON line of the probe console has the one form of the lines of the runtime", func(t *testing.T) {
		clearEnvironment(t)
		builtins, err := NewBuiltins([]string{"ship-orders"}, []SchemaChange{{ID: "split-name", Open: true}})
		if err != nil {
			t.Fatal(err)
		}
		stdout := &lines{}
		r, err := start(newTelemetry(t), builtins, "json", stdout)
		if err != nil {
			t.Fatal(err)
		}
		// The built-in service, worker and backfill observations, a burst over the rate limit, whose count line the
		// stop writes, and the declared event, counter and histogram of a call with its tenant and its trace.
		r.Service(context.Background(), ServiceReady)
		r.SchemaMismatch(context.Background(), "grant")
		worker := r.Worker("ship-orders")
		worker.RunCompleted(context.Background())
		worker.TickSkipped()
		worker.TenantUnreached()
		for range otel.ConsoleLimit + 2 {
			worker.RunFailedCause(context.Background(), WorkerUndecided, RelationsPendingCause)
		}
		r.Backfill().BackfillRows("split-name", 40)
		r.Backfill().BackfillThrottled("split-name", "replica_lag")
		call := r.Bind(trace.ContextWithSpanContext(context.Background(), unsampledSpanContext()), "t1", "user:u-7", command)
		call.Probe().Emit(reservationRefused(t, CountsAttempt), Text("A-1"), Enum("short"))
		call.Probe().Emit(reservationsRefused(t, CountsCommitted), Enum("expired"))
		call.Probe().Emit(histogram(t, "reservation_wait", Milliseconds), Number(12.5))
		call.Returned()
		call.Commit()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		events := map[string]bool{}
		for _, line := range stdout.everything() {
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			if err := logline.Check([]byte(line)); err != nil {
				t.Fatalf("the line %q: %v", line, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatal(err)
			}
			events[decoded["event.name"].(string)] = true
		}
		for _, want := range []string{"service.ready", "schema.manifest.mismatch", "worker.run.completed", "worker.tick.skipped", "worker.tenant.unreached",
			"worker.run.failed", "schema.backfill.rows", "schema.backfill.throttled", "domain.reservation_refused", "domain.reservations_refused",
			"domain.reservation_wait", "telemetry.runtime.stopped"} {
			if !events[want] {
				t.Fatalf("the console has no line of %s: %v", want, events)
			}
		}
	})
}

// blockedOut is a standard output that blocks each write until release closes, and tells when a write entered.
type blockedOut struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockedOut) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

// endsAtThirdWait is a context whose Done channel closes at its third call: the drains of the two queues of the
// console see it open, and the wait for the watch sees it closed, so the test needs no clock.
type endsAtThirdWait struct {
	context.Context
	calls atomic.Int32
	once  sync.Once
	done  chan struct{}
}

func (c *endsAtThirdWait) Done() <-chan struct{} {
	if c.calls.Add(1) >= 3 {
		c.once.Do(func() { close(c.done) })
	}
	return c.done
}

func (c *endsAtThirdWait) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestProbeDrainBound(t *testing.T) {
	t.Run("ST-SUP-RP12-LC12-T02 The stop does not wait past its limit for a watch that a blocked standard output holds", func(t *testing.T) {
		out := &blockedOut{entered: make(chan struct{}, 1), release: make(chan struct{})}
		defer close(out.release)
		s := newConsoleSink(out, formatJSON, "orders", "instance-1")
		clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		s.mu.Lock()
		s.now = func() time.Time { return clock }
		s.windows[budget{event: "worker.run.failed", errors: true}] = &window{start: clock, dropped: 3}
		s.mu.Unlock()
		ticks := make(chan time.Time)
		s.watch(ticks, nil)
		s.mu.Lock()
		clock = clock.Add(otel.ConsoleWindow)
		s.mu.Unlock()
		// The tick closes the window, and the watch blocks in the write of its count line, holding the lock.
		ticks <- clock
		<-out.entered
		ctx := &endsAtThirdWait{Context: context.Background(), done: make(chan struct{})}
		if s.drain(ctx, &record{obs: telemetryStopped, count: 1, time: clock}) {
			t.Fatal("the drain wrote each line although standard output blocks")
		}
		if ctx.calls.Load() < 3 {
			t.Fatalf("the drain ended after %d waits, before the wait for the watch", ctx.calls.Load())
		}
	})
}
