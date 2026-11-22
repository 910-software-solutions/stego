package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestServiceLogsLocalAndOTLP(t *testing.T) {
	for _, mode := range []string{"local", "otlp", "logs-none"} {
		t.Run(mode, func(t *testing.T) {
			var collector *traceCollector
			if mode == "local" {
				traceEnvironment(t)
			} else {
				collector = collectorFixture(t, false)
			}
			if mode == "logs-none" {
				t.Setenv("OTEL_LOGS_EXPORTER", "none")
			}
			t.Setenv("OTEL_SERVICE_NAME", "records-test")
			before := slog.Default()
			var output bytes.Buffer
			runtime, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if slog.Default() != before {
				t.Fatal("runtime changed the process logger")
			}
			traceID, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
			spanID, _ := trace.SpanIDFromHex("2222222222222222")
			ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
			// A context value cannot become a log field.
			type privateKey struct{}
			ctx = context.WithValue(ctx, privateKey{}, "private-credential")
			if !runtime.LogServiceEvent(ctx, ServiceReady) || !runtime.LogServiceEvent(ctx, ServiceFailed) {
				t.Fatal("declared event rejected")
			}
			if runtime.LogServiceEvent(ctx, ServiceEvent(255)) {
				t.Fatal("unknown event accepted")
			}
			runtime.Close()
			if runtime.LogServiceEvent(ctx, ServiceReady) {
				t.Fatal("closed runtime accepted an event")
			}
			decoder := json.NewDecoder(&output)
			for _, event := range []ServiceEvent{ServiceReady, ServiceFailed} {
				var record localRecord
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				name, message, severity, _ := event.description()
				if record.Service != "records-test" || record.Event != name || record.Message != message || record.Severity != severity || record.Time.IsZero() || record.TraceID != traceID.String() || record.SpanID != spanID.String() {
					t.Fatal("incorrect local event", record)
				}
			}
			var extra localRecord
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatal("unexpected local event", err)
			}
			if mode != "otlp" {
				if runtime.service.logger != nil {
					t.Fatal("disabled export created a service logger")
				}
				return
			}
			count := 0
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			for count < 2 {
				select {
				case batch := <-collector.logs:
					data, _ := proto.Marshal(batch)
					if strings.Contains(string(data), "private-credential") {
						t.Fatal("log exposed a context value")
					}
					for _, resource := range batch.ResourceLogs {
						if len(resource.Resource.Attributes) != 2 || value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != runtime.instance || value(resource.Resource.Attributes, "service.name").GetStringValue() != "records-test" {
							t.Fatal("incorrect service identity")
						}
						for _, scope := range resource.ScopeLogs {
							if scope.Scope.Name != "stego/service" {
								t.Fatal("incorrect service log scope")
							}
							for _, record := range scope.LogRecords {
								if count >= 2 {
									t.Fatal("unexpected exported event")
								}
								name, message, severity, _ := []ServiceEvent{ServiceReady, ServiceFailed}[count].description()
								if record.EventName != name || record.Body.GetStringValue() != message || record.SeverityText != severity || hex.EncodeToString(record.TraceId) != traceID.String() || hex.EncodeToString(record.SpanId) != spanID.String() || len(record.Attributes) != 0 {
									t.Fatal("incorrect exported event", record)
								}
								count++
							}
						}
					}
				case <-timer.C:
					t.Fatal("service logs were not exported")
				}
			}
		})
	}
}

type blockedLogWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestBlockedServiceLogOutputIsBounded(t *testing.T) {
	t.Run("ST-SUP-RC17-OC23-T01 A blocked local log output of the emitted runtime stays bounded and does not block its caller", func(t *testing.T) {
		collectorFixture(t, true)
		writer := &blockedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
		runtime, err := newRuntime(writer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			close(writer.release)
			runtime.Close()
			select {
			case <-runtime.service.local.done:
			case <-time.After(time.Second):
				t.Error("released log worker did not stop")
			}
		}()
		runtime.LogServiceEvent(context.Background(), ServiceReady)
		select {
		case <-writer.entered:
		case <-time.After(time.Second):
			t.Fatal("local writer did not start")
		}
		started := time.Now()
		for i := 0; i < QueueSize*3; i++ {
			runtime.LogServiceEvent(context.Background(), ServiceFailed)
		}
		if time.Since(started) > time.Second {
			t.Fatal("log queue blocked the caller")
		}
		if runtime.LocalLogDrops() != QueueSize*2 {
			t.Fatal("local queue is not bounded", runtime.LocalLogDrops())
		}
		runtime.service.lifecycle = true
		started = time.Now()
		runtime.Close()
		if elapsed := time.Since(started); elapsed < ShutdownTimeout || elapsed > ShutdownTimeout+time.Second {
			t.Fatal("shared shutdown budget not enforced", elapsed)
		}
		if runtime.LocalLogDrops() != QueueSize*2+2 {
			t.Fatal("final events bypassed the queue bound")
		}
		if runtime.LogServiceEvent(context.Background(), ServiceFailed) {
			t.Fatal("closed queue accepted an event")
		}
	})
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("private-output-error") }
func TestServiceLogWriteFailureAndConcurrentClose(t *testing.T) {
	traceEnvironment(t)
	runtime, err := newRuntime(failedLogWriter{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.LogServiceEvent(context.Background(), ServiceReady)
	var callers sync.WaitGroup
	for i := 0; i < 8; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for j := 0; j < 100; j++ {
				runtime.LogServiceEvent(context.Background(), ServiceFailed)
			}
		}()
	}
	callers.Add(1)
	go func() { defer callers.Done(); runtime.Close() }()
	callers.Wait()
	runtime.Close()
	if runtime.LocalLogFailures() == 0 {
		t.Fatal("output failure was not counted")
	}
	select {
	case <-runtime.service.local.done:
	default:
		t.Fatal("local writer did not stop")
	}
}

func TestServiceIdentityIsValidatedWithoutCollector(t *testing.T) {
	t.Run("ST-SUP-RC17-OC30-T01 The emitted runtime refuses an invalid service name and does not echo it", func(t *testing.T) {
		traceEnvironment(t)
		for _, service := range []string{"private\ncredential", strings.Repeat("x", 129), "name=value"} {
			t.Setenv("OTEL_SERVICE_NAME", service)
			if _, err := NewRuntime(); err == nil || strings.Contains(err.Error(), service) {
				t.Fatal("invalid identity accepted or exposed")
			}
		}
	})
}

func BenchmarkServiceLogs(b *testing.B) {
	traceEnvironment(b)
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		b.Fatal(err)
	}
	defer runtime.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.LogServiceEvent(context.Background(), ServiceReady)
	}
	b.StopTimer()
	b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "drops/op")
}

// localLines gives the local JSON lines of a closed runtime.
func localLines(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("a local line is not JSON: %v: %s", err, line)
		}
		lines = append(lines, record)
	}
	return lines
}

// linesOf gives the local lines of the event name.
func linesOf(lines []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		if line["event.name"] == event {
			out = append(out, line)
		}
	}
	return out
}

// noRequestValue fails when the local output holds a value of the requests of the tests.
func noRequestValue(t *testing.T, output []byte) {
	t.Helper()
	for _, value := range []string{"0198a1b2-7f3c", "q=secret", "tok-private", "pin-4711", "buyer@example.test", "Bearer"} {
		if bytes.Contains(output, []byte(value)) {
			t.Fatalf("the local output holds %q: %s", value, output)
		}
	}
}

func TestLocalAccessLines(t *testing.T) {
	for _, mode := range []string{"local", "otlp"} {
		t.Run("ST-SUP-RC17-OC20-T02 Each request and each RPC call writes one local access line with the route template and no value ("+mode+")", func(t *testing.T) {
			if mode == "local" {
				traceEnvironment(t)
			} else {
				collectorFixture(t, false)
			}
			t.Setenv("STEGO_LOG_LEVEL", "")
			var output bytes.Buffer
			runtime, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
				switch r.PathValue("id") {
				case "0198a1b2-7f3c-missing":
					w.WriteHeader(http.StatusNotFound)
				case "0198a1b2-7f3c-broken":
					w.WriteHeader(http.StatusInternalServerError)
				default:
					_, _ = w.Write([]byte("pin-4711"))
				}
			})
			handler := runtime.Handler(runtime.Route(mux))
			for _, id := range []string{"0198a1b2-7f3c-found", "0198a1b2-7f3c-missing", "0198a1b2-7f3c-broken"} {
				request := httptest.NewRequest("GET", "/records/"+id+"?q=secret", strings.NewReader("buyer@example.test"))
				request.Header.Set("Authorization", "Bearer tok-private")
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}
			_, finish := runtime.TraceRPC(context.Background(), "/sample.Records/Get")
			finish(nil)
			_, finish = runtime.TraceRPC(context.Background(), "/sample.Records/Get")
			finish(status.Error(grpccodes.NotFound, "0198a1b2-7f3c buyer@example.test"))
			_, finish = runtime.TraceRPC(context.Background(), "/sample.Records/Get")
			finish(status.Error(grpccodes.Internal, "tok-private"))
			runtime.Close()
			lines := localLines(t, &output)
			access := linesOf(lines, "http.server.request.completed")
			if len(access) != 3 {
				t.Fatalf("%d access lines, want 3: %s", len(access), output.Bytes())
			}
			for i, want := range []struct {
				status   float64
				severity string
				errType  any
			}{{200, "INFO", nil}, {404, "WARN", nil}, {500, "ERROR", "500"}} {
				line := access[i]
				if line["http.request.method"] != "GET" || line["http.route"] != "/records/{id}" || line["http.response.status_code"] != want.status ||
					line["severity"] != want.severity || line["error.type"] != want.errType || line["duration_seconds"] == nil || line["service.instance.id"] != runtime.instance {
					t.Fatalf("the access line %d is %v, want GET /records/{id} %v %s", i, line, want.status, want.severity)
				}
			}
			calls := linesOf(lines, "rpc.server.call.completed")
			if len(calls) != 3 || calls[0]["severity"] != "INFO" || calls[1]["severity"] != "WARN" || calls[2]["severity"] != "ERROR" ||
				calls[1]["rpc.response.status_code"] != "NOT_FOUND" || calls[2]["error.type"] != "INTERNAL" || calls[0]["rpc.method"] != "sample.Records/Get" {
				t.Fatalf("the RPC lines are %v", calls)
			}
			noRequestValue(t, output.Bytes())
		})
	}
}

func TestLocalLogLevel(t *testing.T) {
	t.Run("ST-SUP-RC17-OC23-T02 STEGO_LOG_LEVEL sets the lowest local line, a database call that does not fail is DEBUG, and a bad value is refused", func(t *testing.T) {
		traceEnvironment(t)
		run := func(level string) []map[string]any {
			t.Helper()
			t.Setenv("STEGO_LOG_LEVEL", level)
			var output bytes.Buffer
			runtime, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			runtime.LogServiceEvent(context.Background(), ServiceReady)
			ok := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			missing := runtime.Handler(http.NotFoundHandler())
			ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
			missing.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
			ctx := runtime.Context(context.Background())
			_, done := TraceDatabase(ctx, "query")
			done("success")
			_, done = TraceDatabase(ctx, "query")
			done("failure")
			runtime.Close()
			noRequestValue(t, output.Bytes())
			return localLines(t, &output)
		}
		count := func(lines []map[string]any, event string, severity string) int {
			n := 0
			for _, line := range linesOf(lines, event) {
				if line["severity"] == severity {
					n++
				}
			}
			return n
		}
		for _, c := range []struct {
			level                           string
			ready, ok, missing, dbOK, dbBad int
		}{
			{"", 1, 1, 1, 0, 1},
			{"info", 1, 1, 1, 0, 1},
			{"debug", 1, 1, 1, 1, 1},
			{"warn", 1, 0, 1, 0, 1},
			{"error", 1, 0, 0, 0, 1},
		} {
			lines := run(c.level)
			got := [5]int{count(lines, "service.ready", "INFO"), count(lines, "http.server.request.completed", "INFO"),
				count(lines, "http.server.request.completed", "WARN"), count(lines, "db.client.operation.completed", "DEBUG"),
				count(lines, "db.client.operation.completed", "ERROR")}
			if got != [5]int{c.ready, c.ok, c.missing, c.dbOK, c.dbBad} {
				t.Fatalf("STEGO_LOG_LEVEL=%q gave the lines %v, want %v", c.level, got, [5]int{c.ready, c.ok, c.missing, c.dbOK, c.dbBad})
			}
		}
		t.Setenv("STEGO_LOG_LEVEL", "verbose-private")
		if _, err := newRuntime(io.Discard); err == nil || strings.Contains(err.Error(), "verbose-private") {
			t.Fatalf("a bad STEGO_LOG_LEVEL gave %v, want a refusal that does not echo the value", err)
		}
	})
}
