package otel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/jsell-rh/stego/registry/runtime/internal/spanevent"
)

// collector is a fake OTLP gRPC collector on loopback with TLS. With hold set, each export waits until release or
// until the caller gives up. A held trace export sends on arrived when it comes in, and on cancelled when the caller
// gives up. With large set, it answers a trace export with a response of about that many bytes.
type collector struct {
	tracecollector.UnimplementedTraceServiceServer
	traces    chan *tracecollector.ExportTraceServiceRequest
	logs      chan *logcollector.ExportLogsServiceRequest
	metrics   chan *metriccollector.ExportMetricsServiceRequest
	hold      chan struct{}
	holding   atomic.Bool
	large     atomic.Int64
	release   func()
	arrived   chan struct{}
	cancelled chan struct{}
	mu        sync.Mutex
	batches   []int
	// refuse makes the collector refuse each export at once with an error, so an export fails with no wait.
	refuse atomic.Bool
}

// refused is the answer of a collector that refuses the export.
var refused = status.Error(grpccodes.Unavailable, "the collector refuses the export")

// wait holds one export when the collector holds, until release or until the caller gives up.
func (c *collector) wait(ctx context.Context) error {
	if !c.holding.Load() {
		return nil
	}
	select {
	case <-c.hold:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// signal sends on a channel of the collector without blocking the export.
func signal(to chan struct{}) {
	select {
	case to <- struct{}{}:
	default:
	}
}

func (c *collector) Export(ctx context.Context, request *tracecollector.ExportTraceServiceRequest) (*tracecollector.ExportTraceServiceResponse, error) {
	if c.refuse.Load() {
		return nil, refused
	}
	if c.holding.Load() {
		signal(c.arrived)
	}
	if err := c.wait(ctx); err != nil {
		signal(c.cancelled)
		return nil, err
	}
	count := 0
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			count += len(scope.Spans)
		}
	}
	c.mu.Lock()
	c.batches = append(c.batches, count)
	c.mu.Unlock()
	select {
	case c.traces <- request:
	default:
	}
	if size := c.large.Load(); size > 0 {
		// An unknown field pads the response to the size, and a client that reads it ignores it.
		response := &tracecollector.ExportTraceServiceResponse{}
		padding := protowire.AppendTag(nil, 1000, protowire.BytesType)
		padding = protowire.AppendBytes(padding, make([]byte, int(size)-len(padding)-3))
		response.ProtoReflect().SetUnknown(padding)
		return response, nil
	}
	return &tracecollector.ExportTraceServiceResponse{}, nil
}

func (c *collector) exported() (total, largest int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, count := range c.batches {
		total += count
		largest = max(largest, count)
	}
	return total, largest
}

type logSink struct {
	logcollector.UnimplementedLogsServiceServer
	c *collector
}

func (s *logSink) Export(ctx context.Context, request *logcollector.ExportLogsServiceRequest) (*logcollector.ExportLogsServiceResponse, error) {
	if s.c.refuse.Load() {
		return nil, refused
	}
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	select {
	case s.c.logs <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &logcollector.ExportLogsServiceResponse{}, nil
}

type metricSink struct {
	metriccollector.UnimplementedMetricsServiceServer
	c *collector
}

func (s *metricSink) Export(ctx context.Context, request *metriccollector.ExportMetricsServiceRequest) (*metriccollector.ExportMetricsServiceResponse, error) {
	if s.c.refuse.Load() {
		return nil, refused
	}
	if err := s.c.wait(ctx); err != nil {
		return nil, err
	}
	select {
	case s.c.metrics <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &metriccollector.ExportMetricsServiceResponse{}, nil
}

// clearEnvironment removes every telemetry setting of the test process for the test.
func clearEnvironment(t testing.TB) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || name == tokenSetting {
			t.Setenv(name, "")
		}
	}
}

// trustFile writes the certificate of a new TLS test server to a PEM file, and returns the file and the server's
// certificate pair.
func trustFile(t testing.TB) (string, tls.Certificate) {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	server.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, server.TLS.Certificates[0]
}

// startCollector starts the fake collector and points the telemetry settings at it, with sampling ratio 1.
func startCollector(t testing.TB, hold bool) *collector {
	t.Helper()
	clearEnvironment(t)
	file, certificate := trustFile(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})))
	c := &collector{traces: make(chan *tracecollector.ExportTraceServiceRequest, 1024), logs: make(chan *logcollector.ExportLogsServiceRequest, 64), metrics: make(chan *metriccollector.ExportMetricsServiceRequest, 64), release: func() {}, arrived: make(chan struct{}, 1024), cancelled: make(chan struct{}, 1024)}
	if hold {
		c.hold = make(chan struct{})
		c.holding.Store(true)
		var once sync.Once
		c.release = func() { once.Do(func() { close(c.hold) }) }
	}
	tracecollector.RegisterTraceServiceServer(server, c)
	logcollector.RegisterLogsServiceServer(server, &logSink{c: c})
	metriccollector.RegisterMetricsServiceServer(server, &metricSink{c: c})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(c.release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	return c
}

// logRecorder is a log processor that keeps every record it is given.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (l *logRecorder) OnEmit(_ context.Context, record *sdklog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *logRecorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (l *logRecorder) Shutdown(context.Context) error                         { return nil }
func (l *logRecorder) ForceFlush(context.Context) error                       { return nil }
func (l *logRecorder) all() []sdklog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sdklog.Record(nil), l.records...)
}

// harness is a runtime with the runtime's own sampler, span limits, instruments and completion logs, whose spans,
// metrics and logs go to in-memory recorders instead of a collector.
type harness struct {
	runtime *Runtime
	spans   *tracetest.SpanRecorder
	reader  *sdkmetric.ManualReader
	logs    *logRecorder
}

func newHarness(t testing.TB, ratio float64) *harness {
	t.Helper()
	h := &harness{spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader(), logs: &logRecorder{}}
	h.runtime = &Runtime{service: "records", instance: "00000000-0000-4000-8000-000000000000"}
	h.runtime.resource = serviceResource(h.runtime.service, h.runtime.instance)
	h.runtime.startTraces(ratio, h.spans)
	if err := h.runtime.startSignals(h.reader, h.logs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.runtime.Close)
	return h
}

func (h *harness) collect(t testing.TB) metricdata.ResourceMetrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func (h *harness) lastSpan(t testing.TB) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := h.spans.Ended()
	if len(ended) == 0 {
		t.Fatal("no span ended")
	}
	return ended[len(ended)-1]
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.Emit(), true
		}
	}
	return "", false
}

type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func TestWithoutCollector(t *testing.T) {
	t.Run("ST-SUP-RP03-LC01-T01 The runtime starts no provider without a collector", func(t *testing.T) {
		clearEnvironment(t)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []string{"open", "closed"} {
			// The logger provider has only the console, which opens no connection (ST-SUP-RP03-LC16).
			if r.provider != nil || r.connection != nil || r.meter != nil || r.spans != nil || r.console == nil {
				t.Fatalf("%s: a provider or a connection exists without a collector", phase)
			}
			if r.TracerProvider() != nil || r.MeterProvider() != nil || r.CollectorSet() || r.Service() != "records" || r.Instance() == "" {
				t.Fatalf("%s: the accessors do not tell that telemetry is absent", phase)
			}
			next := &countingHandler{}
			if r.Handler(next) != http.Handler(next) || r.Route(next) != http.Handler(next) {
				t.Fatalf("%s: the handler is not served directly", phase)
			}
			ctx := context.Background()
			if got, finish := r.TraceRPC(ctx, "/sample.v1.Records/Get"); got != ctx {
				t.Fatalf("%s: the RPC context changed", phase)
			} else {
				finish(nil)
			}
			r.Close()
		}
	})
}

// silentCollector accepts connections and never answers.
func silentCollector(t testing.TB) <-chan struct{} {
	t.Helper()
	clearEnvironment(t)
	file, _ := trustFile(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			signal(accepted)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", file)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	return accepted
}

// created is the result of New.
type created struct {
	r   *Runtime
	err error
}

func TestExportConnection(t *testing.T) {
	t.Run("ST-SUP-RP03-LC02-T01 The runtime starts while the collector never answers", func(t *testing.T) {
		accepted := silentCollector(t)
		global := otel.GetTracerProvider()
		// New must return before it opens a connection to the collector. A collector that never answers holds each
		// connection, so a startup that waits for it opens one and then waits: the test sees the connection and
		// fails, and does not wait for New on the wall clock.
		result := make(chan created, 1)
		go func() {
			r, err := New("records")
			result <- created{r, err}
		}()
		var got created
		select {
		case got = <-result:
		case <-accepted:
			select {
			case got = <-result:
			default:
				t.Fatal("startup connected to the collector and waited for it")
			}
		}
		r, err := got.r, got.err
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
		if r.ExportFailures() != 0 || r.MetricExportFailures() != 0 || r.LogExportFailures() != 0 {
			t.Fatal("an export completed before creation returned")
		}
		if otel.GetTracerProvider() != global || r.provider == nil || r.connection == nil {
			t.Fatal("the runtime set a global provider or has no private one")
		}
	})
	t.Run("ST-SUP-RP03-LC02-T02 A disabled runtime returns the caller context unchanged", func(t *testing.T) {
		startCollector(t, false)
		closed, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		closed.Close()
		for name, r := range map[string]*Runtime{"closed": closed, "no provider": {}} {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-11111111111111111111111111111111-2222222222222222-01"))
			got, finish := r.TraceRPC(ctx, "/sample.v1.Records/Echo")
			finish(nil)
			if got != ctx {
				t.Fatalf("%s: the runtime changed the caller context", name)
			}
		}
	})
}

// holdSpans and holdLogs are exporters that never answer: each export waits until its context ends.
type holdSpans struct{}

func (holdSpans) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	<-ctx.Done()
	return ctx.Err()
}
func (holdSpans) Shutdown(context.Context) error { return nil }

type holdLogs struct{}

func (holdLogs) Export(ctx context.Context, _ []sdklog.Record) error {
	<-ctx.Done()
	return ctx.Err()
}
func (holdLogs) Shutdown(context.Context) error   { return nil }
func (holdLogs) ForceFlush(context.Context) error { return nil }

// blockedRuntime is a runtime with the runtime's own trace queue and log queue (ST-SUP-RP03-LC03), whose exporters
// never answer. A test calls it in a synctest bubble, where the export bound runs on the clock of the bubble.
func blockedRuntime(t testing.TB) *Runtime {
	t.Helper()
	r := &Runtime{service: "records", instance: "00000000-0000-4000-8000-000000000000", deadline: context.WithTimeout}
	r.resource = serviceResource(r.service, r.instance)
	r.spans = newSpanQueue(&safeSpanExporter{SpanExporter: holdSpans{}, failures: &r.traceFailures}, context.WithTimeout)
	r.startTraces(1, r.spans)
	if err := r.startSignals(nil, logQueue(holdLogs{}, &r.logFailures)); err != nil {
		t.Fatal(err)
	}
	return r
}

// unblocked runs calls on its own goroutine in a synctest bubble and fails the test when the calls still wait once
// every goroutine of the bubble waits: the exports never answer, so nothing can free a place in a queue. The caller
// closes the runtime only after unblocked returns: a call that waits can hold the lock of the queue that the close
// takes, so the bubble ends with its goroutines blocked and the test fails, and does not hang.
func unblocked(t *testing.T, calls func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		calls()
	}()
	synctest.Wait()
	select {
	case <-done:
	default:
		t.Fatal("a call waited for an export that does not answer")
	}
}

func TestQueuesDoNotBlock(t *testing.T) {
	t.Run("ST-SUP-RP03-LC03-T01 A full queue does not block the request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := blockedRuntime(t)
			handler := r.Handler(&countingHandler{})
			failed := false
			unblocked(t, func() {
				for i := 0; i < QueueSize*3; i++ {
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, httptest.NewRequest("GET", "/records", nil))
					failed = failed || w.Code != http.StatusNoContent
				}
			})
			r.Close()
			if failed {
				t.Fatal("the collector failure reached the request")
			}
			if r.DroppedSpans() == 0 {
				t.Fatal("the full trace queue dropped no span")
			}
		})
	})
}

func TestHTTPSpan(t *testing.T) {
	t.Run("ST-SUP-RP03-LC04-T01 The span carries the status the client received", func(t *testing.T) {
		h := newHarness(t, 1)
		for _, tc := range []struct {
			name    string
			want    int
			handler http.HandlerFunc
		}{
			{"upgrade", 101, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(101) }},
			{"hints then upgrade", 101, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(101) }},
			{"readfrom then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("data"))
				w.WriteHeader(101)
			}},
			{"empty readfrom then created", 201, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader(""))
				w.WriteHeader(201)
			}},
			{"hints then created", 201, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(103); w.WriteHeader(201) }},
			{"final then upgrade", 204, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204); w.WriteHeader(101) }},
			{"implicit then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("data")); w.WriteHeader(101) }},
			{"string then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "data"); w.WriteHeader(101) }},
			{"flush then upgrade", 200, func(w http.ResponseWriter, _ *http.Request) {
				_ = http.NewResponseController(w).Flush()
				w.WriteHeader(101)
			}},
			{"empty", 200, func(http.ResponseWriter, *http.Request) {}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				done := make(chan struct{})
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.want == 101 {
						w.Header().Set("Upgrade", "websocket")
						w.Header().Set("Connection", "Upgrade")
					}
					h.runtime.Handler(tc.handler).ServeHTTP(w, r)
					close(done)
				}))
				server.Config.ErrorLog = log.New(io.Discard, "", 0)
				server.Start()
				defer server.Close()
				client := &http.Client{}
				defer client.CloseIdleConnections()
				response, err := client.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != tc.want {
					t.Fatalf("wire status: got %d, want %d", response.StatusCode, tc.want)
				}
				<-done
				if got, _ := spanAttribute(h.lastSpan(t), "http.response.status_code"); got != strconv.Itoa(tc.want) {
					t.Fatalf("span status: got %s, want %d", got, tc.want)
				}
			})
		}
	})
	t.Run("ST-SUP-RP03-LC04-T02 The span names the registered route and not the path", func(t *testing.T) {
		c := startCollector(t, false)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /records/{id}", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("first"))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error("the wrapper lost flush support", err)
			}
			w.Write([]byte("second"))
		})
		result := httptest.NewRecorder()
		r.Handler(mux).ServeHTTP(result, httptest.NewRequest("GET", "/records/private-record-id", nil))
		if !result.Flushed || result.Body.String() != "firstsecond" {
			t.Fatal("the wrapper changed the streaming response")
		}
		r.Close()
		select {
		case request := <-c.traces:
			span := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
			if span.Name != "GET /records/{id}" || len(span.Attributes) != 3 {
				t.Fatal("the registered route was not kept", span)
			}
			raw, _ := proto.Marshal(request)
			if strings.Contains(string(raw), "private-record-id") {
				t.Fatal("the span used the request path instead of the route pattern")
			}
		default:
			// Close returns after the trace queue sent what it held, and the collector keeps each request before it
			// answers, so the span is there with no wait.
			t.Fatal("the span was not exported")
		}
	})
}

const parent = "00-11111111111111111111111111111111-2222222222222222-01"

func TestRPCSpan(t *testing.T) {
	failing := map[grpccodes.Code]bool{grpccodes.Unknown: true, grpccodes.DeadlineExceeded: true, grpccodes.Unimplemented: true, grpccodes.Internal: true, grpccodes.Unavailable: true, grpccodes.DataLoss: true}
	private := func() context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", parent, "authorization", "Bearer private-token", "baggage", "user=private-user", "tracestate", "vendor=private-state"))
	}
	t.Run("ST-SUP-RP03-LC04-T03 The RPC span lasts until the final status", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			ctx, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			if !trace.SpanContextFromContext(ctx).IsValid() || len(h.spans.Ended()) != int(code) {
				t.Fatal("the span is missing or ended before the call finished")
			}
			finish(status.Error(code, "private-error"))
			ended := h.spans.Ended()
			if len(ended) != int(code)+1 || ended[len(ended)-1].SpanKind() != trace.SpanKindServer {
				t.Fatal("no server span ended after finish")
			}
		}
	})
	t.Run("ST-SUP-RP03-LC05-T01 The RPC span carries three fields and a fixed failure class", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			_, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			finish(status.Error(code, "private-error"))
			span := h.lastSpan(t)
			want := 3
			if failing[code] {
				want = 4
				if kind, _ := spanAttribute(span, "error.type"); kind != statusNames[code] {
					t.Fatal("the failure class is not the status name", code)
				}
			}
			if len(span.Attributes()) != want || (span.Status().Code == otelcodes.Error) != failing[code] || span.Status().Description != "" {
				t.Fatal("incorrect RPC failure status", code)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC05-T02 The RPC span holds no private call data", func(t *testing.T) {
		h := newHarness(t, 1)
		for code := grpccodes.OK; code <= grpccodes.Unauthenticated; code++ {
			_, finish := h.runtime.TraceRPC(private(), "/sample.v1.Records/Watch")
			finish(status.Error(code, "private-error"))
			span := h.lastSpan(t)
			if span.SpanContext().TraceState().Len() != 0 || len(span.Events()) != 0 || len(span.Links()) != 0 {
				t.Fatal("the span kept a trace state, an event or a link")
			}
			for _, attr := range span.Attributes() {
				if strings.Contains(attr.Value.Emit(), "private") {
					t.Fatal("private data in the RPC span")
				}
			}
		}
	})
}

func TestParentAndSampling(t *testing.T) {
	t.Run("ST-SUP-RP03-LC08-T01 A single valid traceparent continues its trace and parent", func(t *testing.T) {
		h := newHarness(t, 1)
		request := httptest.NewRequest("GET", "/records", nil)
		request.Header.Set("traceparent", parent)
		h.runtime.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), request)
		span := h.lastSpan(t)
		if span.SpanContext().TraceID().String() != "11111111111111111111111111111111" || span.Parent().SpanID().String() != "2222222222222222" || !span.Parent().IsRemote() {
			t.Fatal("the span did not continue the remote trace")
		}
		for _, values := range [][]string{nil, {"bad"}, {"00-00000000000000000000000000000000-2222222222222222-01"}, {parent, parent}} {
			request := httptest.NewRequest("GET", "/records", nil)
			request.Header["Traceparent"] = values
			h.runtime.Handler(&countingHandler{}).ServeHTTP(httptest.NewRecorder(), request)
			if h.lastSpan(t).Parent().IsValid() {
				t.Fatal("an invalid set of parents was continued", values)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC08-T02 The remote sampled flag does not change the local ratio decision", func(t *testing.T) {
		traceIDs := []string{"0123456789abcdef0000000000000001", "0123456789abcdefffffffffffffffff", "0123456789abcdef4000000000000000"}
		for _, ratio := range []float64{0, 0.5, 1} {
			h := newHarness(t, ratio)
			for _, id := range traceIDs {
				for _, flag := range []string{"00", "01"} {
					var sampled bool
					handler := h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						sampled = trace.SpanContextFromContext(r.Context()).IsSampled()
					}))
					request := httptest.NewRequest("GET", "/records", nil)
					request.Header.Set("traceparent", "00-"+id+"-2222222222222222-"+flag)
					handler.ServeHTTP(httptest.NewRecorder(), request)
					if want := ratioDecision(t, ratio, id); sampled != want {
						t.Fatalf("ratio %v, trace %s, flag %s: sampled %v, the ratio decides %v", ratio, id, flag, sampled, want)
					}
				}
			}
		}
	})
}

// ratioDecision is the trace ID ratio rule of OpenTelemetry, worked out here and not by the SDK: a trace is sampled
// when its last 63 bits are below the ratio of 2^63.
func ratioDecision(t testing.TB, ratio float64, id string) bool {
	t.Helper()
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		t.Fatal("bad trace ID", id)
	}
	if ratio >= 1 {
		return true
	}
	bound := uint64(ratio * math.Exp2(63))
	return binary.BigEndian.Uint64(raw[8:16])>>1 < bound
}

func TestSettings(t *testing.T) {
	endpoint := "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4317"
	t.Run("ST-SUP-RP03-LC09-T01 With a collector endpoint and no exporter setting all three signals are enabled", func(t *testing.T) {
		values, err := readSettings("records", []string{endpoint})
		if err != nil || !values.metrics || !values.logs || values.endpoint == "" {
			t.Fatal("a signal is not enabled", err)
		}
		startCollector(t, false)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.provider == nil || r.meter == nil || r.logs == nil || r.signals.httpDuration == nil || r.signals.logger == nil {
			t.Fatal("traces, metrics or request logs are not enabled")
		}
		if r.TracerProvider() == nil || r.MeterProvider() == nil || r.LoggerProvider() == nil || !r.CollectorSet() || r.Service() != "records" || len(r.Instance()) != 36 {
			t.Fatal("the accessors do not give the private providers and the identity")
		}
	})
	t.Run("ST-SUP-RP03-LC09-T02 The settings defaults are ratio 0.1, a ten-second interval and the declared service name", func(t *testing.T) {
		values, err := readSettings("records", []string{endpoint})
		if err != nil || values.ratio != 0.1 || values.interval != 10*time.Second || values.service != "records" {
			t.Fatal("unexpected defaults", values, err)
		}
	})
	t.Run("ST-SUP-RP03-LC09-T03 A protocol setting other than grpc is refused", func(t *testing.T) {
		for protocol, accepted := range map[string]bool{"": true, "grpc": true, "http/protobuf": false, "http/json": false} {
			_, err := readSettings("records", []string{endpoint, "OTEL_EXPORTER_OTLP_PROTOCOL=" + protocol})
			if (err == nil) != accepted {
				t.Fatalf("protocol %q: accepted %v", protocol, err == nil)
			}
			if err != nil && !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL") {
				t.Fatal("the error does not name the setting", err)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC09-T03 An endpoint with no port is the HTTPS port 443", func(t *testing.T) {
		for endpoint, host := range map[string]string{"https://collector.example": "collector.example:443", "https://collector.example:4317": "collector.example:4317", "https://[2001:db8::1]": "[2001:db8::1]:443"} {
			values, err := readSettings("records", []string{"OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint})
			if err != nil || values.origin.Host != host {
				t.Fatalf("%s: got %v, %v, want %s", endpoint, values.origin, err, host)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC09-T04 The certificate setting is read only as a bounded regular PEM file", func(t *testing.T) {
		valid, certificate := trustFile(t)
		dir := t.TempDir()
		device := filepath.Join(dir, "device.pem")
		if err := os.Symlink("/dev/zero", device); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(valid)
		large := filepath.Join(dir, "large.pem")
		if err := os.WriteFile(large, append(append([]byte{}, data...), make([]byte, certificateLimit)...), 0o600); err != nil {
			t.Fatal(err)
		}
		text := filepath.Join(dir, "text.pem")
		if err := os.WriteFile(text, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		roots, err := trustRoots(valid)
		parsed, _ := x509.ParseCertificate(certificate.Certificate[0])
		want := x509.NewCertPool()
		want.AddCert(parsed)
		if err != nil || !roots.Equal(want) {
			t.Fatal("the PEM file was not used", err)
		}
		for name, file := range map[string]string{"directory": dir, "device link": device, "larger than the bound": large, "not PEM": text} {
			if _, err := trustRoots(file); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
		system, err := x509.SystemCertPool()
		if err != nil {
			t.Skip("no system roots on this host")
		}
		if roots, err := trustRoots(""); err != nil || !roots.Equal(system) {
			t.Fatal("the system roots are not used when the setting is absent")
		}
	})
}

func TestExportLimits(t *testing.T) {
	t.Run("ST-SUP-RP03-LC10-T01 The trace queue holds at most 256 spans in batches of at most 32", func(t *testing.T) {
		c := startCollector(t, true)
		r, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		// The collector holds the first batch, so at most the queue and that batch are outside the drop count,
		// whatever the speed of the export goroutine.
		for range 300 {
			_, span := r.testSpan("span")
			span.End()
		}
		c.release()
		r.Close()
		exported, largest := c.exported()
		dropped := int(r.DroppedSpans())
		if exported+dropped != 300 || exported > QueueSize+BatchSize || dropped < 300-QueueSize-BatchSize || largest > BatchSize {
			t.Fatalf("exported %d, dropped %d, largest batch %d", exported, dropped, largest)
		}
	})
	t.Run("ST-SUP-RP03-LC10-T02 Each export ends within two seconds with a 64 KiB response limit", func(t *testing.T) {
		clearEnvironment(t)
		if r, err := New("records"); err != nil || reflect.ValueOf(r.deadline).Pointer() != reflect.ValueOf(context.WithTimeout).Pointer() {
			t.Fatal("New does not bound the exports with the real clock", err)
		}

		// The queue gives the exporter a context that ends at two seconds on the clock, and not before.
		clock := newHandClock(t)
		exporter := &heldExporter{calls: make(chan context.Context, 1)}
		q := newSpanQueue(exporter, clock.deadline)
		sampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled})
		q.OnEnd(tracetest.SpanStub{SpanContext: sampled}.Snapshot())
		export := <-exporter.calls
		bound := clock.next(t)
		if bound.d != ExportTimeout {
			t.Fatalf("the export is bounded at %v, not at %v", bound.d, ExportTimeout)
		}
		clock.advance(ExportTimeout - time.Nanosecond)
		if export.Err() != nil {
			t.Fatal("the export was cancelled before its bound")
		}
		clock.advance(time.Nanosecond)
		if export.Err() == nil || !errors.Is(context.Cause(export), context.DeadlineExceeded) {
			t.Fatal("the held export was not cancelled at two seconds", export.Err())
		}
		<-clock.ended
		if err := q.Shutdown(context.Background()); err != nil || q.dropped.Load() != 1 {
			t.Fatal("the cancelled export was not counted dropped", err, q.dropped.Load())
		}

		// Through the gRPC exporter, the collector sees the held export cancelled at the bound, and the export
		// fails.
		clock = newHandClock(t)
		c := startCollector(t, true)
		r, err := newRuntime("records", clock.deadline)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, span := r.testSpan("held")
		span.End()
		<-c.arrived
		bound = clock.next(t)
		if bound.d != ExportTimeout {
			t.Fatalf("the export is bounded at %v, not at %v", bound.d, ExportTimeout)
		}
		clock.advance(ExportTimeout)
		<-c.cancelled
		<-clock.ended
		if r.ExportFailures() != 1 {
			t.Fatal("the cancelled export was not counted as a failure", r.ExportFailures())
		}

		c.holding.Store(false)
		for _, size := range []int64{ResponseLimit - 1024, 65 << 10} {
			c.large.Store(size)
			before := r.ExportFailures()
			_, span := r.testSpan("large")
			span.End()
			// The queue calls the cancel function of the export's context when the export returned.
			<-clock.ended
			refused := r.ExportFailures() > before
			if refused != (size > ResponseLimit) {
				t.Fatalf("a response of %d bytes: refused %v", size, refused)
			}
		}
	})
	t.Run("ST-SUP-RP03-LC10-T03 Shutdown gives the provider at most three seconds", func(t *testing.T) {
		clock := newHandClock(t)
		c := startCollector(t, true)
		r, err := newRuntime("records", clock.deadline)
		if err != nil {
			t.Fatal(err)
		}
		for range QueueSize {
			_, span := r.testSpan("span")
			span.End()
		}
		<-c.arrived
		returned := make(chan struct{})
		go func() {
			r.Close()
			close(returned)
		}()
		// Shutdown asks for its bound before it shuts the providers down. The provider does not shut the queue down
		// when its context has already ended, so the clock moves only after the queue's shutdown started.
		<-r.spans.stop
		clock.find(t, ShutdownTimeout)
		clock.advance(ShutdownTimeout - time.Nanosecond)
		select {
		case <-returned:
			t.Fatal("shutdown ended before its bound while the collector held an export")
		default:
		}
		clock.advance(time.Nanosecond)
		<-returned
		exported, _ := c.exported()
		if dropped := int(r.DroppedSpans()); exported != 0 || dropped != QueueSize {
			t.Fatalf("exported %d, dropped %d: each span must be exported or counted", exported, dropped)
		}
		if r.ShutdownFailures() == 0 {
			t.Fatal("an incomplete shutdown was not counted")
		}
	})
	t.Run("ST-SUP-RP03-LC10-T03 Shutdown ends by the earlier of the caller's deadline and three seconds", func(t *testing.T) {
		clock := newHandClock(t)
		c := startCollector(t, true)
		r, err := newRuntime("records", clock.deadline)
		if err != nil {
			t.Fatal(err)
		}
		_, span := r.testSpan("span")
		span.End()
		<-c.arrived
		caller, end := context.WithCancel(context.Background())
		defer end()
		result := make(chan error, 1)
		go func() { result <- r.Shutdown(caller) }()
		<-r.spans.stop
		if bound := clock.find(t, ShutdownTimeout); bound.parent != caller {
			t.Fatal("the three-second bound does not start from the caller's context")
		}
		select {
		case <-result:
			t.Fatal("shutdown ended before a bound while the collector held an export")
		default:
		}
		// The caller's deadline ends first: its context ends while the clock is before three seconds.
		end()
		if err := <-result; err == nil {
			t.Fatal("a blocked shutdown reported success")
		}
		if r.Shutdown(context.Background()) == nil || r.DroppedSpans() != 1 {
			t.Fatal("a second shutdown did not return the first result, or the span was not counted")
		}
	})
}

// handClock is a clock that the test moves itself, for the bounds of ST-SUP-RP03-LC10. Each context it gives ends
// when the test moves the clock to its deadline, or when its parent ends. Its cancel function does not end it: it
// sends the bound on ended, so the test knows that the bounded work returned, and reads when the context ended.
type handClock struct {
	mu     sync.Mutex
	now    time.Duration
	bounds []*handBound
	made   chan *handBound
	ended  chan *handBound
}

// handBound is one context that the clock gave.
type handBound struct {
	parent context.Context
	d, at  time.Duration
	end    context.CancelCauseFunc
}

func newHandClock(t testing.TB) *handClock {
	c := &handClock{made: make(chan *handBound, 1024), ended: make(chan *handBound, 1024)}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, bound := range c.bounds {
			bound.end(context.Canceled)
		}
	})
	return c
}

func (c *handClock) deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, end := context.WithCancelCause(parent)
	c.mu.Lock()
	bound := &handBound{parent: parent, d: d, at: c.now + d, end: end}
	c.bounds = append(c.bounds, bound)
	c.mu.Unlock()
	c.made <- bound
	return ctx, func() {
		select {
		case c.ended <- bound:
		default:
		}
	}
}

// advance moves the clock and ends each context whose deadline it reached.
func (c *handClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	for _, bound := range c.bounds {
		if bound.at <= c.now {
			bound.end(context.DeadlineExceeded)
		}
	}
}

// next returns the bound that the clock gave first and that the test did not read yet. The code under test asks for
// the bound before it starts the work, so a missing bound fails the test and does not wait.
func (c *handClock) next(t testing.TB) *handBound {
	t.Helper()
	select {
	case bound := <-c.made:
		return bound
	default:
		t.Fatal("the work has no bound on the clock")
		return nil
	}
}

// find reads the bounds that the clock gave until one of d. It passes over export bounds. Any other bound, or no
// bound of d, fails the test: the caller knows that the code under test asked for the bound already.
func (c *handClock) find(t testing.TB, d time.Duration) *handBound {
	t.Helper()
	for {
		bound := c.next(t)
		switch bound.d {
		case d:
			return bound
		case ExportTimeout:
		default:
			t.Fatalf("a bound of %v, not of %v", bound.d, d)
		}
	}
}

// heldExporter sends each export's context on calls and holds the export until that context ends.
type heldExporter struct{ calls chan context.Context }

func (e *heldExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.calls <- ctx
	<-ctx.Done()
	return ctx.Err()
}
func (e *heldExporter) Shutdown(context.Context) error { return nil }

// countingExporter counts the spans it exports.
type countingExporter struct{ spans atomic.Int64 }

func (e *countingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.spans.Add(int64(len(spans)))
	return nil
}
func (e *countingExporter) Shutdown(context.Context) error { return nil }

func TestSpanQueueShutdownRace(t *testing.T) {
	t.Run("ST-SUP-RP03-LC10-T01 Spans that end during shutdown are exported or counted dropped", func(t *testing.T) {
		sampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled})
		span := tracetest.SpanStub{SpanContext: sampled}.Snapshot()
		for range 20 {
			exporter := &countingExporter{}
			// A nil bound is the real clock's bound: the queue must not export with no bound.
			q := newSpanQueue(exporter, nil)
			var ended atomic.Int64
			var group sync.WaitGroup
			for range 4 {
				group.Add(1)
				go func() {
					defer group.Done()
					for range 200 {
						q.OnEnd(span)
						ended.Add(1)
					}
				}()
			}
			time.Sleep(time.Millisecond)
			if err := q.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			group.Wait()
			if got := exporter.spans.Load() + int64(q.dropped.Load()); got != ended.Load() {
				t.Fatalf("ended %d, exported and dropped %d", ended.Load(), got)
			}
		}
	})
}

// probeKey is the span event capability of the probe runtime, as the probe makes it.
var probeKey = spanevent.New()

// testSpan starts a span of the runtime with its record, as the runtime's own wrappers do.
func (r *Runtime) testSpan(name string) (context.Context, trace.Span) {
	ctx, span, _ := r.startSpan(context.Background(), r.tracer, name, trace.SpanKindInternal, time.Now())
	return ctx, span
}

func TestSpanOwnData(t *testing.T) {
	t.Run("ST-SUP-RP03-LC05-T03 Code that holds the server span adds no event, attribute, status message, name or span", func(t *testing.T) {
		const secret = "password=hunter2"
		forge := func(ctx context.Context) {
			span := trace.SpanFromContext(ctx)
			span.RecordError(errors.New("exception.message=" + secret))
			span.AddEvent("domain.forged", trace.WithAttributes(attribute.String("password", secret)))
			span.AddEvent("free " + secret)
			span.SetAttributes(attribute.String("error.type", secret), attribute.String("http.route", secret), attribute.String("password", secret))
			span.SetStatus(otelcodes.Error, secret)
			span.SetName(secret)
			_, child := span.TracerProvider().Tracer("fill").Start(ctx, secret, trace.WithAttributes(attribute.String("password", secret)))
			child.End()
		}
		h := newHarness(t, 1)
		handler := h.runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			forge(request.Context())
			// The probe runtime's own event goes through SpanEvent and is kept.
			if h.runtime.SpanEvent(probeKey, trace.SpanContextFromContext(request.Context()), "domain.reservation_refused", time.Now(), attribute.String("reason", "short")) {
				t.Error("SpanEvent dropped the probe event")
			}
			w.WriteHeader(http.StatusOK)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/items", nil))
		ctx, finish := h.runtime.TraceRPC(context.Background(), "/sample.v1.Records/Get")
		forge(ctx)
		finish(nil)
		spans := h.spans.Ended()
		if len(spans) != 2 {
			t.Fatalf("%d spans ended, want the HTTP and the RPC span only", len(spans))
		}
		for _, span := range spans {
			text := span.Name() + " " + span.Status().Description
			var keys []string
			for _, attr := range span.Attributes() {
				keys = append(keys, string(attr.Key))
				text += " " + attr.Value.Emit()
			}
			for _, event := range span.Events() {
				text += " " + event.Name
				for _, attr := range event.Attributes {
					text += " " + string(attr.Key) + " " + attr.Value.Emit()
				}
			}
			if strings.Contains(text, "hunter2") || strings.Contains(text, "password") || strings.Contains(text, "exception") || strings.Contains(text, "forged") {
				t.Fatalf("the span %q holds data that code in its context set: %s", span.Name(), text)
			}
			if span.Status().Code != otelcodes.Unset || len(span.Links()) != 0 || slices.Contains(keys, "error.type") {
				t.Fatalf("the span %q has the status %v and the attributes %v", span.Name(), span.Status(), keys)
			}
		}
		if events := spans[0].Events(); len(events) != 1 || events[0].Name != "domain.reservation_refused" {
			t.Fatalf("the HTTP span has the events %v, want the probe event only", events)
		}
		if got := spans[1].Name(); got != "sample.v1.Records/Get" {
			t.Fatalf("the RPC span is named %q", got)
		}
	})

	t.Run("ST-SUP-RP03-LC05-T03 A span keeps at most SpanEventLimit probe events and reports the rest", func(t *testing.T) {
		h := newHarness(t, 1)
		ctx, span := h.runtime.testSpan("call")
		sc := trace.SpanContextFromContext(ctx)
		dropped := 0
		for range SpanEventLimit + 3 {
			if h.runtime.SpanEvent(probeKey, sc, "domain.x", time.Now()) {
				dropped++
			}
		}
		span.End()
		if h.runtime.SpanEvent(probeKey, sc, "domain.x", time.Now()) {
			t.Fatal("an event after the end of the span was reported for a span that is no longer the runtime's")
		}
		if dropped != 3 || len(h.lastSpan(t).Events()) != SpanEventLimit || h.lastSpan(t).DroppedEvents() != 3 {
			t.Fatalf("SpanEvent dropped %d, the span holds %d events", dropped, len(h.lastSpan(t).Events()))
		}
	})
}

func TestOperationSpan(t *testing.T) {
	t.Run("ST-SUP-RP03-LC15-T01 An operation span of a registered name is exported with its own record", func(t *testing.T) {
		h := newHarness(t, 1)
		if err := h.runtime.register([]string{"reconcile", "scan"}); err != nil {
			t.Fatal(err)
		}
		parent, _ := h.runtime.testSpan("outer")
		rootCtx, finishRoot, err := h.runtime.StartOperation(parent, Root, "reconcile")
		if err != nil {
			t.Fatal(err)
		}
		childCtx, finishChild, err := h.runtime.StartOperation(rootCtx, Child, "scan")
		if err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{rootCtx, childCtx} {
			span := trace.SpanFromContext(ctx)
			span.SetAttributes(attribute.String("fill", "PRIVATE"))
			span.SetStatus(otelcodes.Error, "PRIVATE message")
			span.RecordError(errors.New("PRIVATE"))
		}
		if h.runtime.SpanEvent(probeKey, trace.SpanContextFromContext(childCtx), "domain.reservation_refused", time.Now()) {
			t.Fatal("SpanEvent did not accept the operation span")
		}
		finishChild(OutcomeFailure, false, true)
		finishRoot(OutcomeFailure, true, true)
		var root, child sdktrace.ReadOnlySpan
		for _, span := range h.spans.Ended() {
			switch span.Name() {
			case "reconcile":
				root = span
			case "scan":
				child = span
			}
		}
		if root == nil || child == nil {
			t.Fatal("the operation spans were not exported")
		}
		if root.Parent().IsValid() || root.SpanContext().TraceID() == trace.SpanContextFromContext(parent).TraceID() {
			t.Fatal("the root span continued the outer span")
		}
		if child.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatal("the child span is not under the root")
		}
		for _, span := range []sdktrace.ReadOnlySpan{root, child} {
			var keys []string
			for _, kv := range span.Attributes() {
				keys = append(keys, string(kv.Key))
				if strings.Contains(kv.Value.Emit(), "PRIVATE") {
					t.Fatal("a fill value reached the span")
				}
			}
			sort.Strings(keys)
			if !slices.Equal(keys, []string{"stego.operation", "stego.outcome", "stego.retry"}) {
				t.Fatalf("the span %s has the attributes %v", span.Name(), keys)
			}
			if span.Status().Code != otelcodes.Error || span.Status().Description != "" {
				t.Fatalf("the span %s has the status %v", span.Name(), span.Status())
			}
			if outcome, _ := spanAttribute(span, "stego.outcome"); outcome != "failure" {
				t.Fatalf("the span %s has the outcome %q", span.Name(), outcome)
			}
		}
		if events := child.Events(); len(events) != 1 || events[0].Name != "domain.reservation_refused" {
			t.Fatal("the probe event is not on the operation span", events)
		}
	})

	t.Run("ST-SUP-RP03-LC15-T01 The caller holds a span that does nothing", func(t *testing.T) {
		h := newHarness(t, 1)
		if err := h.runtime.register([]string{"reconcile"}); err != nil {
			t.Fatal(err)
		}
		ctx, finish, err := h.runtime.StartOperation(context.Background(), Root, "reconcile")
		if err != nil {
			t.Fatal(err)
		}
		early := time.Now().Add(-time.Hour)
		held := trace.SpanFromContext(ctx)
		held.SetAttributes(attribute.String("fill", "PRIVATE"))
		held.End(trace.WithTimestamp(early))
		if len(h.spans.Ended()) != 0 {
			t.Fatal("the caller ended the operation span")
		}
		time.Sleep(time.Millisecond)
		finish(OutcomeFailure, false, true)
		span := h.lastSpan(t)
		if outcome, _ := spanAttribute(span, "stego.outcome"); outcome != "failure" || span.Status().Code != otelcodes.Error || !span.EndTime().After(span.StartTime()) {
			t.Fatal("the outcome, the status or the end time was lost", span.Status(), span.EndTime())
		}
		if _, ok := spanAttribute(span, "fill"); ok {
			t.Fatal("the caller added an attribute")
		}
	})

	t.Run("ST-SUP-RP03-LC15-T01 SpanEvent takes the probe key and plain names only", func(t *testing.T) {
		h := newHarness(t, 1)
		if err := h.runtime.register([]string{"reconcile"}); err != nil {
			t.Fatal(err)
		}
		ctx, finish, err := h.runtime.StartOperation(context.Background(), Root, "reconcile")
		if err != nil {
			t.Fatal(err)
		}
		sc := trace.SpanContextFromContext(ctx)
		if !h.runtime.SpanEvent(spanevent.Key{}, sc, "domain.forged", time.Now()) {
			t.Fatal("a zero key added an event")
		}
		if !h.runtime.SpanEvent(probeKey, sc, "domain.free text", time.Now()) || !h.runtime.SpanEvent(probeKey, sc, "domain.x", time.Now(), attribute.String("Free Key", "v")) {
			t.Fatal("a free-text name or key was accepted")
		}
		finish(OutcomeSuccess, false, false)
		if events := h.lastSpan(t).Events(); len(events) != 0 {
			t.Fatal("the span holds forged events", events)
		}
	})

	t.Run("ST-SUP-RP03-LC15-T01 Spans that never end are bounded and counted", func(t *testing.T) {
		h := newHarness(t, 1)
		if err := h.runtime.register([]string{"reconcile"}); err != nil {
			t.Fatal(err)
		}
		for range SpanRecordLimit + 5 {
			if _, _, err := h.runtime.StartOperation(context.Background(), Root, "reconcile"); err != nil {
				t.Fatal(err)
			}
		}
		if live := h.runtime.live.Load(); live != SpanRecordLimit || h.runtime.DroppedSpans() != 5 {
			t.Fatal("the records are not bounded", live, h.runtime.DroppedSpans())
		}
	})

	t.Run("ST-SUP-RP03-LC15-T01 Without a provider an operation still has IDs", func(t *testing.T) {
		clearEnvironment(t)
		r, err := New("records", "reconcile", "database.query")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if !r.Registered("reconcile") || r.Registered("scan") {
			t.Fatal("Registered does not follow the names given to New")
		}
		root, _, err := r.StartOperation(context.Background(), Root, "reconcile")
		if err != nil {
			t.Fatal(err)
		}
		child, _, err := r.StartOperation(root, Child, "database.query")
		if err != nil {
			t.Fatal(err)
		}
		rc, cc := trace.SpanContextFromContext(root), trace.SpanContextFromContext(child)
		if !rc.IsValid() || !cc.IsValid() || rc.IsSampled() || cc.TraceID() != rc.TraceID() || cc.SpanID() == rc.SpanID() {
			t.Fatal("the operation IDs", rc, cc)
		}
	})

	t.Run("ST-SUP-RP03-LC15-T02 An unregistered span name is refused", func(t *testing.T) {
		h := newHarness(t, 1)
		if err := h.runtime.register([]string{"reconcile"}); err != nil {
			t.Fatal(err)
		}
		ctx, finish, err := h.runtime.StartOperation(context.Background(), Root, "unregistered")
		if err == nil || finish != nil || trace.SpanFromContext(ctx).SpanContext().IsValid() {
			t.Fatal("an unregistered name started a span", err)
		}
		if _, _, err := h.runtime.StartOperation(context.Background(), 0, "reconcile"); err == nil {
			t.Fatal("a span with no parent choice was started")
		}
		if len(h.spans.Ended()) != 0 || len(h.spans.Started()) != 0 {
			t.Fatal("a span was started or exported")
		}
		if _, err := New("records", "Bad Name"); err == nil {
			t.Fatal("an invalid name was registered")
		}
	})
}

func TestOneInstancePerProcess(t *testing.T) {
	t.Run("ST-SUP-RP03-LC16-T10 A runtime started with the instance identity of the process names that identity on each signal and line", func(t *testing.T) {
		clearEnvironment(t)
		const instance = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"
		r, err := NewWithInstance("records", instance)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.Instance() != instance || r.console == nil || r.console.instance != instance {
			t.Fatalf("the runtime names the instance %q and its console %q, want %q", r.Instance(), r.console.instance, instance)
		}
		if value, ok := r.resource.Set().Value("service.instance.id"); !ok || value.AsString() != instance {
			t.Fatalf("the resource of the signals names the instance %v, want %q", value, instance)
		}
		// New still gives each runtime an identity of its own (ST-SUP-RC17-OC19).
		other, err := New("records")
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if other.Instance() == instance || !instancePattern.MatchString(other.Instance()) {
			t.Fatalf("New gave the instance %q", other.Instance())
		}
		// A value that is not a UUID v4 in lower case is refused, with text that does not hold it.
		for _, bad := range []string{"", "host-01", strings.ToUpper(instance), "3f2a1b4c-5d6e-1f70-8a9b-0c1d2e3f4a5b", instance + "\n"} {
			if refused, err := NewWithInstance("records", bad); err == nil || refused != nil || (bad != "" && strings.Contains(err.Error(), bad)) {
				t.Fatalf("NewWithInstance(%q) gave %v, %v; want a refusal that does not hold the value", bad, refused, err)
			}
		}
	})
}
