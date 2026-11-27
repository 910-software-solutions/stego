package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/910-software-solutions/stego/registry/runtime/authn"
	"github.com/910-software-solutions/stego/registry/runtime/authn/authntest"
	rtotel "github.com/910-software-solutions/stego/registry/runtime/otel"
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
			runtime, err := newRuntime(formOf(&output))
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
			runtime, err := newRuntime(formOf(&output))
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

func TestLocalCommandRoute(t *testing.T) {
	t.Run("ST-SUP-RC17-OC20-T05 The access line of a declared command route, a custom method with a colon, carries its template", func(t *testing.T) {
		traceEnvironment(t)
		t.Setenv("STEGO_LOG_LEVEL", "")
		var output bytes.Buffer
		runtime, err := newRuntime(formOf(&output))
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /jobs/{job_id}/status:convert", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("POST /proposals/{id}/status:accept", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })
		handler := runtime.Handler(runtime.Route(mux))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/jobs/0198a1b2-7f3c-job/status:convert", nil))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/proposals/0198a1b2-7f3c-p/status:accept", nil))
		runtime.Close()
		access := linesOf(localLines(t, &output), "http.server.request.completed")
		if len(access) != 2 || access[0]["http.route"] != "/jobs/{job_id}/status:convert" || access[1]["http.route"] != "/proposals/{id}/status:accept" {
			t.Fatalf("the access lines of the command routes are %v, want the templates /jobs/{job_id}/status:convert and /proposals/{id}/status:accept", access)
		}
		noRequestValue(t, output.Bytes())
	})
}

func TestLocalLogLevel(t *testing.T) {
	t.Run("ST-SUP-RC17-OC23-T02 STEGO_LOG_LEVEL sets the lowest local line, a database call that does not fail is DEBUG, and a bad value is refused", func(t *testing.T) {
		traceEnvironment(t)
		run := func(level string) []map[string]any {
			t.Helper()
			t.Setenv("STEGO_LOG_LEVEL", level)
			var output bytes.Buffer
			runtime, err := newRuntime(formOf(&output))
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

// heldWriter holds its first write until release, and keeps each line.
type heldWriter struct {
	entered, release chan struct{}
	once             sync.Once
	mu               sync.Mutex
	out              bytes.Buffer
}

func (w *heldWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

func TestLocalLogReserveAndBudget(t *testing.T) {
	t.Run("ST-SUP-RC17-OC23-T03 A flood of access lines leaves the places of the service events and the ERROR lines, and a WARN access line has a budget for each route and status", func(t *testing.T) {
		traceEnvironment(t)
		t.Setenv("STEGO_LOG_LEVEL", "")
		writer := &heldWriter{entered: make(chan struct{}), release: make(chan struct{})}
		runtime, err := newRuntime(writer)
		if err != nil {
			t.Fatal(err)
		}
		runtime.LogServiceEvent(context.Background(), ServiceReady)
		<-writer.entered
		ok := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		for range 3 * QueueSize {
			ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		}
		runtime.LogServiceEvent(context.Background(), ServiceFailed)
		_, done := TraceDatabase(runtime.Context(context.Background()), "query")
		done("failure")
		close(writer.release)
		runtime.Close()
		lines := localLines(t, &writer.out)
		if n := len(linesOf(lines, "service.failed")); n != 1 {
			t.Fatalf("%d service.failed lines after a flood of access lines, want 1", n)
		}
		if n := len(linesOf(lines, "db.client.operation.completed")); n != 1 {
			t.Fatalf("%d failed database lines after a flood of access lines, want 1", n)
		}
		if n := len(linesOf(lines, "http.server.request.completed")); n != QueueSize-LocalReserve {
			t.Fatalf("%d access lines of the flood, want %d", n, QueueSize-LocalReserve)
		}

		// The budget: 25 answers 401 of one route and 25 answers 404 of another give AccessLimit lines each, and one
		// line with the count of each at the end of the window.
		var output bytes.Buffer
		runtime, err = newRuntime(formOf(&output))
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		runtime.service.now = func() time.Time { return clock }
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		mux.HandleFunc("GET /orders", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		handler := runtime.Handler(runtime.Route(mux))
		for range 25 {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/orders", nil))
		}
		// The window ends: the next line of the route writes the count first, and starts a new window.
		clock = clock.Add(AccessWindow)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		runtime.Close()
		got := map[string]int{}
		dropped := map[string]float64{}
		for _, line := range linesOf(localLines(t, &output), "http.server.request.completed") {
			key := fmt.Sprintf("%v %v", line["http.route"], line["http.response.status_code"])
			if n, ok := line["dropped"].(float64); ok {
				dropped[key] += n
				continue
			}
			got[key]++
		}
		if got["/records 401"] != AccessLimit+1 || got["/orders 404"] != AccessLimit || dropped["/records 401"] != 25-AccessLimit || dropped["/orders 404"] != 25-AccessLimit {
			t.Fatalf("the budget gave the lines %v and the counts %v, want %d and %d lines and %d dropped of each", got, dropped, AccessLimit+1, AccessLimit, 25-AccessLimit)
		}
	})
}

func TestLocalAccessLineEdges(t *testing.T) {
	t.Run("ST-SUP-RC17-OC20-T03 An access line of a route that no pattern matches has no route, and a method out of the fixed set is _OTHER", func(t *testing.T) {
		traceEnvironment(t)
		var output bytes.Buffer
		runtime, err := newRuntime(formOf(&output))
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, _ *http.Request) {})
		handler := runtime.Handler(runtime.Route(mux))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/0198a1b2-7f3c/unknown?q=secret", nil))
		odd := httptest.NewRequest("GET", "/records/0198a1b2-7f3c", nil)
		odd.Method = "G\x01T\nbuyer@example.test"
		handler.ServeHTTP(httptest.NewRecorder(), odd)
		runtime.Close()
		access := linesOf(localLines(t, &output), "http.server.request.completed")
		if len(access) != 2 {
			t.Fatalf("%d access lines, want 2", len(access))
		}
		if _, ok := access[0]["http.route"]; ok || access[0]["http.response.status_code"] != float64(404) || access[0]["http.request.method"] != "GET" {
			t.Fatalf("the line of an unmatched route is %v, want 404 and no route", access[0])
		}
		if access[1]["http.request.method"] != "_OTHER" {
			t.Fatalf("the line of a method out of the set is %v, want _OTHER", access[1])
		}
		noRequestValue(t, output.Bytes())
		if bytes.Contains(output.Bytes(), []byte("G\\u0001T")) {
			t.Fatalf("the local output holds the method: %s", output.Bytes())
		}
	})
	t.Run("ST-SUP-RC17-OC20-T06 The access line of a refusal before the router holds its fixed problem code and no value", func(t *testing.T) {
		// erp-demo production run (2026-11-27): a 401 of the authentication and a 403 tenant.not_provisioned of the
		// tenant check came before the router, so their access lines had only the status.
		traceEnvironment(t)
		var output bytes.Buffer
		runtime, err := newRuntime(formOf(&output))
		if err != nil {
			t.Fatal(err)
		}
		const token, tenantID = "eyJhbGciOiJFZERTQSJ9.secret-token-value.sig", "acme-tenant-4711"
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
			// A code that a handler gives for a request that then passes does not reach its line.
			rtotel.SetErrorCode(r.Context(), "unused")
		})
		// The real authentication of the runtime library: a Verifier with no keys answers each request 401.
		var verifier *authn.Verifier
		authenticated := runtime.Handler(verifier.Middleware(runtime.Route(mux)))
		request := httptest.NewRequest("GET", "/records/"+tenantID+"?tenant="+tenantID, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		answer := httptest.NewRecorder()
		authenticated.ServeHTTP(answer, request)
		if answer.Code != http.StatusUnauthorized {
			t.Fatalf("the authentication gave %d, want 401", answer.Code)
		}
		// The tenant check of the runtime library gives the code as service.refuseTenant does, before the router; a
		// code with text of a value is not a code and is ignored.
		gated := runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rtotel.SetErrorCode(r.Context(), "tenant "+tenantID)
			rtotel.SetErrorCode(r.Context(), "tenant.not_provisioned")
			w.WriteHeader(http.StatusForbidden)
		}))
		gated.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records/"+tenantID, nil))
		runtime.Handler(runtime.Route(mux)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records/"+tenantID, nil))
		runtime.Close()
		access := linesOf(localLines(t, &output), "http.server.request.completed")
		if len(access) != 3 {
			t.Fatalf("%d access lines, want 3", len(access))
		}
		for i, want := range []struct {
			status float64
			code   string
		}{{401, "AUT-001"}, {403, "tenant.not_provisioned"}, {200, ""}} {
			if _, routed := access[i]["http.route"]; access[i]["http.response.status_code"] != want.status || i < 2 && routed {
				t.Fatalf("access line %d is %v, want the status %v and no route", i, access[i], want.status)
			}
			code, _ := access[i]["stego.error_code"].(string)
			if code != want.code {
				t.Errorf("access line %d holds the error code %q, want %q: %v", i, code, want.code, access[i])
			}
		}
		for _, value := range []string{token, "secret-token-value", tenantID, "Bearer"} {
			if bytes.Contains(output.Bytes(), []byte(value)) {
				t.Errorf("the local output holds the value %q: %s", value, output.Bytes())
			}
		}
	})
}

// formWriter takes the local lines of a runtime: it checks each line against the one form of the lines of the runtime
// (CheckLine, the copy of registry/runtime/internal/logline; walkthrough 25, L-1), and a line of another form stops
// the test binary. It writes each line to out with its attributes at the top level under the keys of localRecord,
// so a check reads a field by its name.
type formWriter struct{ out io.Writer }

// formOf gives the formWriter of out.
func formOf(out io.Writer) io.Writer { return formWriter{out: out} }

// legacyKeys are the keys of localRecord of the attributes whose key on the line has the prefix stego.
var legacyKeys = map[string]string{"stego.startup.stage": "startup.stage", "stego.operation": "operation", "stego.outcome": "outcome",
	"stego.retry": "retry", "stego.dropped": "dropped", "stego.duration_seconds": "duration_seconds"}

func (w formWriter) Write(line []byte) (int, error) {
	if err := CheckLine(line); err != nil {
		panic(fmt.Sprintf("the local line %q is not of the form of the lines of the runtime: %v", line, err))
	}
	flat := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &flat); err != nil {
		panic(err)
	}
	if raw, ok := flat["attributes"]; ok {
		delete(flat, "attributes")
		attrs := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &attrs); err != nil {
			panic(err)
		}
		for key, value := range attrs {
			if legacy, ok := legacyKeys[key]; ok {
				key = legacy
			}
			flat[key] = value
		}
	}
	data, err := json.Marshal(flat)
	if err != nil {
		panic(err)
	}
	if _, err := w.out.Write(append(data, '\n')); err != nil {
		return 0, err
	}
	return len(line), nil
}

// lockedBuffer is the output of a runtime whose watch writes from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLocalLineForm(t *testing.T) {
	t.Run("ST-SUP-RC17-OC20-T04 Each local line has the one form of the lines of the runtime, with its fields under attributes", func(t *testing.T) {
		traceEnvironment(t)
		t.Setenv("STEGO_LOG_LEVEL", "debug")
		var output lockedBuffer
		runtime, err := newRuntime(&output)
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Date(2026, 11, 22, 2, 54, 43, 322614534, time.FixedZone("EST", -5*3600))
		runtime.service.mu.Lock()
		runtime.service.now = func() time.Time { return clock }
		runtime.service.mu.Unlock()
		runtime.LogServiceEvent(context.Background(), ServiceReady)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, _ *http.Request) {})
		mux.HandleFunc("GET /fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
		handler := runtime.Handler(runtime.Route(mux))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records/r1", nil))
		for range AccessLimit + 2 {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/fail", nil))
		}
		_, finish := runtime.TraceRPC(context.Background(), "/sample.Records/Get")
		finish(status.Error(grpccodes.NotFound, "private"))
		_, done := TraceDatabase(runtime.Context(context.Background()), "query")
		done("failure")
		runtime.Close()
		events := map[string]bool{}
		for _, line := range strings.SplitAfter(output.String(), "\n") {
			if line == "" {
				continue
			}
			if err := CheckLine([]byte(line)); err != nil {
				t.Fatalf("the line %q: %v", line, err)
			}
			var decoded struct {
				Event      string         `json:"event.name"`
				Timestamp  string         `json:"timestamp"`
				Service    string         `json:"service.name"`
				Attributes map[string]any `json:"attributes"`
			}
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatal(err)
			}
			events[decoded.Event] = true
			if decoded.Service != "records" || !strings.HasSuffix(decoded.Timestamp, "Z") {
				t.Fatalf("the line %q has no service name or no UTC timestamp", line)
			}
			if decoded.Event == "http.server.request.completed" && decoded.Attributes["stego.dropped"] == nil &&
				(decoded.Attributes["http.request.method"] != "GET" || decoded.Attributes["http.response.status_code"] == nil || decoded.Attributes["stego.duration_seconds"] == nil) {
				t.Fatalf("the access line %q does not hold its fields under attributes", line)
			}
		}
		for _, want := range []string{"service.ready", "http.server.request.completed", "rpc.server.call.completed", "db.client.operation.completed"} {
			if !events[want] {
				t.Fatalf("no line of %s: %v", want, events)
			}
		}
	})
	t.Run("ST-SUP-RC17-OC23-T04 The count line of an access window comes at the end of the window with no later request of its route and status", func(t *testing.T) {
		traceEnvironment(t)
		t.Setenv("STEGO_LOG_LEVEL", "")
		var output lockedBuffer
		runtime, err := newRuntime(&output)
		if err != nil {
			t.Fatal(err)
		}
		// The test gives the ticks of the watch itself and moves its own clock, so no wall clock bounds it.
		runtime.unwatchAccess()
		var mu sync.Mutex
		clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		runtime.service.mu.Lock()
		runtime.service.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
		runtime.service.mu.Unlock()
		ticks := make(chan time.Time)
		runtime.watchAccess(ticks, nil)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		handler := runtime.Handler(runtime.Route(mux))
		for range AccessLimit + 7 {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		}
		count := func() (float64, bool) {
			for _, line := range strings.Split(output.String(), "\n") {
				var decoded struct {
					Attributes map[string]any `json:"attributes"`
				}
				if json.Unmarshal([]byte(line), &decoded) == nil {
					if n, ok := decoded.Attributes["stego.dropped"].(float64); ok {
						return n, true
					}
				}
			}
			return 0, false
		}
		ticks <- clock
		ticks <- clock
		if _, ok := count(); ok {
			t.Fatal("a tick in the window wrote the count line")
		}
		mu.Lock()
		clock = clock.Add(AccessWindow)
		mu.Unlock()
		ticks <- clock
		ticks <- clock
		// The second tick comes when the watch handled the first: the tick closed the window and queued its count
		// line, so no window is left for the close to end.
		runtime.service.mu.Lock()
		left := len(runtime.service.windows)
		runtime.service.mu.Unlock()
		if left != 0 {
			t.Fatalf("after a tick at the end of the window %d windows are open, want none: the count line waits for a later request", left)
		}
		// The local writer writes the queued count line before the close ends.
		runtime.unwatchAccess()
		runtime.Close()
		if n, ok := count(); !ok || n != 7 {
			t.Fatalf("after the window and a tick the output holds the count %v (%v), want 7 with no later request:\n%s", n, ok, output.String())
		}
		if strings.Count(output.String(), `"http.route":"/records"`) != AccessLimit+1 {
			t.Fatalf("the output holds %d lines of the route, want %d and the count line", strings.Count(output.String(), `"http.route":"/records"`), AccessLimit)
		}
	})
}

func TestAccessLineHostileInput(t *testing.T) {
	t.Run("ST-SUP-RC17-OC20-T07 The access line holds only a fixed code of the charset for hostile Authorization values, a forged or shadowed place, long codes and a panic, in the local and the OTLP modes", func(t *testing.T) {
		// Security review of PR #14, I1: the red-team test of the reviewer. The generated errorCodeOf is the only
		// defence when a package stores into the shared place directly, and no other test proved it.
		for _, mode := range []string{"local", "otlp"} {
			t.Run(mode, func(t *testing.T) {
				if mode == "local" {
					traceEnvironment(t)
				} else {
					collectorFixture(t, false)
				}
				var output bytes.Buffer
				runtime, err := newRuntime(formOf(&output))
				if err != nil {
					t.Fatal(err)
				}
				signing := authntest.NewKey(t, "k1")
				keys, err := authn.ParseKeys(authntest.Document(signing.JWK()))
				if err != nil {
					t.Fatal(err)
				}
				v, err := authn.New(authn.Settings{Issuer: "https://issuer.example", Audience: "orders", ProblemCode: "ORDERS", TenantClaim: "tenant", Keys: keys})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(v.Stop)
				mux := http.NewServeMux()
				mux.HandleFunc("GET /r/{id}", func(w http.ResponseWriter, r *http.Request) {
					switch r.PathValue("id") {
					case "forge":
						place := r.Context().Value(struct{ StegoErrorCode struct{} }{}).(*atomic.Pointer[string])
						bad := "EVILCODE\n\"injected\":\"x"
						place.Store(&bad)
						w.WriteHeader(403)
					case "long65":
						rtotel.SetErrorCode(r.Context(), strings.Repeat("L", 65))
						w.WriteHeader(400)
					case "long64":
						rtotel.SetErrorCode(r.Context(), strings.Repeat("M", 64))
						w.WriteHeader(400)
					case "panic":
						rtotel.SetErrorCode(r.Context(), "internal")
						panic("PANICVALUE")
					case "shadow":
						ctx := context.WithValue(r.Context(), struct{ StegoErrorCode struct{} }{}, new(atomic.Pointer[string]))
						rtotel.SetErrorCode(ctx, "shadowed")
						w.WriteHeader(409)
					case "ok":
						rtotel.SetErrorCode(r.Context(), "denied")
					}
				})
				mk := func(id string) *http.Request { return httptest.NewRequest("GET", "/r/"+id, nil) }
				claims := authntest.Claims("https://issuer.example", "orders", "u-1", time.Now())
				claims["tenant"] = "acmetenant"
				token := authntest.Mint(t, authntest.Header("k1"), claims, signing)
				claims["tenant"] = "TEN\nANT\"x"
				badTenant := authntest.Mint(t, authntest.Header("k1"), claims, signing)
				authed := runtime.Handler(v.Middleware(runtime.Route(mux)))
				type c struct {
					hdr  []string
					id   string
					code string
					st   float64
				}
				huge := strings.Repeat("HUGETOKEN", 2000)
				cases := []c{
					{nil, "x", "ORDERS-AUT-001", 401},
					{[]string{"Bearer " + huge}, "x", "ORDERS-AUT-001", 401},
					{[]string{"Basic SECRETBASIC"}, "x", "ORDERS-AUT-001", 401},
					{[]string{"Bearer AAA", "Bearer BBB"}, "x", "ORDERS-AUT-001", 401},
					{[]string{"Bearer bad\"tok\nen"}, "x", "ORDERS-AUT-001", 401},
					{[]string{"Bearer " + badTenant}, "x", "ORDERS-AUT-001", 401},
					{[]string{"Bearer " + token}, "forge", "", 403},
					{[]string{"Bearer " + token}, "long65", "", 400},
					{[]string{"Bearer " + token}, "long64", strings.Repeat("M", 64), 400},
					{[]string{"Bearer " + token}, "shadow", "", 409},
					{[]string{"Bearer " + token}, "ok", "", 200},
					{[]string{"Bearer " + token}, "panic", "internal", 0},
				}
				for _, cs := range cases {
					r := mk(cs.id)
					for _, h := range cs.hdr {
						r.Header.Add("Authorization", h)
					}
					func() {
						defer func() { _ = recover() }()
						authed.ServeHTTP(httptest.NewRecorder(), r)
					}()
				}
				runtime.Close()
				access := linesOf(localLines(t, &output), "http.server.request.completed")
				if len(access) != len(cases) {
					t.Fatalf("%d lines want %d: %s", len(access), len(cases), output.Bytes())
				}
				for i, cs := range cases {
					code, _ := access[i]["stego.error_code"].(string)
					st, _ := access[i]["http.response.status_code"].(float64)
					if code != cs.code || st != cs.st {
						t.Errorf("case %d (%s): code %q status %v, want %q %v: %v", i, cs.id, code, st, cs.code, cs.st, access[i])
					}
				}
				for _, bad := range []string{"HUGETOKEN", "SECRETBASIC", "EVILCODE", "injected", "TEN", "PANICVALUE", "shadowed", token[:20], "AAA", "BBB", "denied", "acmetenant", badTenant[:20]} {
					if bytes.Contains(output.Bytes(), []byte(bad)) {
						t.Errorf("output holds %q", bad)
					}
				}
				t.Logf("lines: %s", output.Bytes())
			})
		}
	})
}
