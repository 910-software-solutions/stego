package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	logcollector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func browserTrace() *tracecollector.ExportTraceServiceRequest {
	now := uint64(time.Now().UnixNano())
	return &tracecollector.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "private.resource", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "private"}}}}}, ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "private.scope"}, Spans: []*tracepb.Span{{Name: "workflow.create", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), StartTimeUnixNano: now, EndTimeUnixNano: now, TraceState: "private=value"}}}}}}}
}
func TestBrowserRelaySignalIdentity(t *testing.T) {
	collector := collectorFixture(t, false)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := runtime.Context(context.Background())
	send := func(signal string, message proto.Message) {
		t.Helper()
		data, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := RelayBrowserTelemetry(ctx, signal, data, "example-browser"); err != nil {
			t.Fatal(signal, err)
		}
	}
	checkResource := func(resource *resourcepb.Resource, owner *Runtime) {
		t.Helper()
		if len(resource.GetAttributes()) != 3 || value(resource.Attributes, "service.name").GetStringValue() != "example-browser" || value(resource.Attributes, "stego.relay.service.name").GetStringValue() != owner.service.service || value(resource.Attributes, "stego.relay.instance.id").GetStringValue() != owner.instance {
			t.Fatal("browser resource or relay identity differs")
		}
	}
	spoofed := browserTrace()
	for _, key := range []string{"service.name", "stego.relay.service.name", "stego.relay.instance.id"} {
		spoofed.ResourceSpans[0].Resource.Attributes = append(spoofed.ResourceSpans[0].Resource.Attributes, &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "private-forged"}}})
	}
	send("traces", spoofed)
	select {
	case batch := <-collector.received:
		r := batch.ResourceSpans[0]
		checkResource(r.Resource, runtime)
		if r.ScopeSpans[0].Scope.Name != "stego.browser" || r.ScopeSpans[0].Spans[0].TraceState != "" {
			t.Fatal("browser resource or vendor state escaped")
		}
	case <-time.After(time.Second):
		t.Fatal("trace was not delivered")
	}
	now := uint64(time.Now().UnixNano())
	send("logs", &logcollector.ExportLogsServiceRequest{ResourceLogs: []*logpb.ResourceLogs{{ScopeLogs: []*logpb.ScopeLogs{{LogRecords: []*logpb.LogRecord{{TimeUnixNano: now, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "workflow.created"}}}}}}}}})
	select {
	case batch := <-collector.logs:
		checkResource(batch.ResourceLogs[0].Resource, runtime)
	case <-time.After(time.Second):
		t.Fatal("log was not delivered")
	}
	send("metrics", &metriccollector.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "workflow.created", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: now, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}}}}}}}}}}}})
	select {
	case batch := <-collector.metrics:
		checkResource(batch.ResourceMetrics[0].Resource, runtime)
	case <-time.After(time.Second):
		t.Fatal("metric was not delivered")
	}
	second, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.instance == runtime.instance {
		t.Fatal("relay instances share an identity")
	}
	ctx = second.Context(context.Background())
	send("traces", spoofed)
	select {
	case batch := <-collector.received:
		checkResource(batch.ResourceSpans[0].Resource, second)
	case <-time.After(time.Second):
		t.Fatal("second relay trace was not delivered")
	}
	ctx = runtime.Context(context.Background())
	for _, data := range [][]byte{nil, bytes.Repeat([]byte{1}, BrowserTelemetryMaxBytes+1), {255}, {0xF8, 0x07, 0x01}} {
		if err := RelayBrowserTelemetry(ctx, "traces", data, "example-browser"); !errors.Is(err, ErrBrowserTelemetryInput) {
			t.Fatal("invalid payload accepted", err)
		}
	}
	unknownKind := browserTrace()
	unknownKind.ResourceSpans[0].ScopeSpans[0].Spans[0].Kind = tracepb.Span_SpanKind(99)
	unknownData, _ := proto.Marshal(unknownKind)
	if !errors.Is(RelayBrowserTelemetry(ctx, "traces", unknownData, "example-browser"), ErrBrowserTelemetryInput) {
		t.Fatal("unknown span kind accepted")
	}
	invalid := browserTrace()
	invalid.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId = make([]byte, 16)
	data, _ := proto.Marshal(invalid)
	if !errors.Is(RelayBrowserTelemetry(ctx, "traces", data, "example-browser"), ErrBrowserTelemetryInput) {
		t.Fatal("zero trace ID accepted")
	}
}
func TestBrowserRelayCollectorDeadline(t *testing.T) {
	collectorFixture(t, true)
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	data, _ := proto.Marshal(browserTrace())
	start := time.Now()
	if !errors.Is(RelayBrowserTelemetry(runtime.Context(context.Background()), "traces", data, "example-browser"), ErrBrowserTelemetryUnavailable) || time.Since(start) > 3*time.Second {
		t.Fatal("collector deadline was not enforced")
	}
	if !errors.Is(RelayBrowserTelemetry(context.Background(), "traces", data, "example-browser"), ErrBrowserTelemetryUnavailable) {
		t.Fatal("relay ran without a runtime")
	}
}

func TestBrowserSettingsFollowRuntime(t *testing.T) {
	if settings := BrowserSettings(context.Background()); settings.Version != 1 || settings.Traces || settings.Logs || settings.Metrics || settings.SampleRatio != 0 {
		t.Fatal("missing runtime enabled browser exports")
	}
	collectorFixture(t, false)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := runtime.Context(context.Background())
	settings := BrowserSettings(ctx)
	if !settings.Traces || settings.Logs || settings.Metrics || settings.SampleRatio != 0.25 {
		t.Fatal("browser settings differ from runtime")
	}
	runtime.Close()
	if BrowserSettings(ctx).Traces {
		t.Fatal("closed runtime enabled browser export")
	}
}

// browserTraceWith gives a browser trace batch with one span for each trace ID.
func browserTraceWith(t *testing.T, ids ...[]byte) []byte {
	t.Helper()
	request := browserTrace()
	scope := request.ResourceSpans[0].ScopeSpans[0]
	first := scope.Spans[0]
	scope.Spans = nil
	for i, id := range ids {
		span := proto.Clone(first).(*tracepb.Span)
		span.TraceId, span.SpanId = id, bytes.Repeat([]byte{byte(i + 2)}, 8)
		scope.Spans = append(scope.Spans, span)
	}
	data, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// traceID gives a trace ID whose four 32-bit words XOR to word, as the browser TraceIdRatio rule reads it.
func traceID(word uint32) []byte {
	id := bytes.Repeat([]byte{0x5a}, 16)
	id[12], id[13], id[14], id[15] = id[12]^byte(word>>24), id[13]^byte(word>>16), id[14]^byte(word>>8), id[15]^byte(word)
	return id
}

func TestBrowserRelayDeploymentRatio(t *testing.T) {
	t.Run("ST-SUP-RC17-OC15-T02 The browser relay refuses traces at a zero ratio and drops a span that the deployment ratio does not sample", func(t *testing.T) {
		collector := collectorFixture(t, false)
		pending := func() int {
			count := 0
			for {
				select {
				case batch := <-collector.received:
					count += len(batch.ResourceSpans[0].ScopeSpans[0].Spans)
				default:
					return count
				}
			}
		}
		// At the ratio 0.5 the bound is floor(0.5 * 0xffffffff) = 0x7fffffff: the word 0x10 is below it, 0x7fffffff is not.
		sampled, dropped := traceID(0x10), traceID(0x7fffffff)
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
		zero, err := NewRuntime()
		if err != nil {
			t.Fatal(err)
		}
		defer zero.Close()
		if err := RelayBrowserTelemetry(zero.Context(context.Background()), "traces", browserTraceWith(t, sampled), "example-browser"); !errors.Is(err, ErrBrowserTelemetryUnavailable) {
			t.Fatal("the relay forwarded browser traces at the ratio 0:", err)
		}
		if count := pending(); count != 0 {
			t.Fatal("the relay exported spans at the ratio 0:", count)
		}
		zero.Close()
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.5")
		half, err := NewRuntime()
		if err != nil {
			t.Fatal(err)
		}
		defer half.Close()
		ctx := half.Context(context.Background())
		if err := RelayBrowserTelemetry(ctx, "traces", browserTraceWith(t, sampled, dropped), "example-browser"); err != nil {
			t.Fatal(err)
		}
		batch := <-collector.received
		if spans := batch.ResourceSpans[0].ScopeSpans[0].Spans; len(spans) != 1 || !bytes.Equal(spans[0].TraceId, sampled) {
			t.Fatalf("the relay did not keep only the sampled span: %d spans", len(spans))
		}
		if err := RelayBrowserTelemetry(ctx, "traces", browserTraceWith(t, dropped), "example-browser"); err != nil {
			t.Fatal(err)
		}
		if count := pending(); count != 0 {
			t.Fatal("the relay exported a span that the ratio does not sample:", count)
		}
		if !browserSampled(traceID(0), 1) || !browserSampled(traceID(0xffffffff), 1) || browserSampled(traceID(0), 0) {
			t.Fatal("the ratio rule differs from the browser TraceIdRatio rule at its ends")
		}
	})
}

// samplerVectors are the shared vectors of the TraceIdRatio rule: trace IDs and ratios with the decision of the
// TraceIdRatioBasedSampler of @opentelemetry/sdk-trace-base at the version that the browser runtime pins. The Node
// test of the browser runtime checks the same file against that sampler (ST-SUP-RC17-OC15-T04).
type samplerVectors struct {
	Source string `json:"source"`
	Cases  []struct {
		TraceID string  `json:"trace_id"`
		Ratio   float64 `json:"ratio"`
		Sampled bool    `json:"sampled"`
	} `json:"cases"`
}

func TestBrowserRelaySamplerVectors(t *testing.T) {
	t.Run("ST-SUP-RC17-OC15-T03 The relay rule gives the decision of the browser sampler for each shared vector", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("testdata", "browser_sampler_vectors.json"))
		if err != nil {
			t.Fatal(err)
		}
		var vectors samplerVectors
		if err := json.Unmarshal(data, &vectors); err != nil {
			t.Fatal(err)
		}
		if vectors.Source != "@opentelemetry/sdk-trace-base 2.10.0 TraceIdRatioBasedSampler" || len(vectors.Cases) < 100 {
			t.Fatalf("the vectors are not those of the pinned browser sampler: %q, %d cases", vectors.Source, len(vectors.Cases))
		}
		sampled := 0
		for _, c := range vectors.Cases {
			id, err := hex.DecodeString(c.TraceID)
			if err != nil || len(id) != 16 {
				t.Fatalf("the vector trace ID %q is not 16 bytes of hex", c.TraceID)
			}
			if got := browserSampled(id, c.Ratio); got != c.Sampled {
				t.Errorf("trace ID %s at the ratio %v: the relay rule gives %v, the browser sampler %v", c.TraceID, c.Ratio, got, c.Sampled)
			}
			if c.Sampled {
				sampled++
			}
		}
		if sampled == 0 || sampled == len(vectors.Cases) {
			t.Fatalf("the vectors hold %d sampled cases of %d, so they test one decision only", sampled, len(vectors.Cases))
		}
	})
}
