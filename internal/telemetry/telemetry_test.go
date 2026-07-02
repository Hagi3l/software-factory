package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newRecording builds a Provider backed by an in-memory span recorder and a manual
// metric reader, so a test can assert exactly which spans and metrics the schema's
// recording helpers emit — the deterministic, collector-free way to verify the contract
// (specs/observability.md). It returns the provider, the span recorder, and the reader.
func newRecording(t *testing.T) (*Provider, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := newInstruments(mp.Meter(ScopeName))
	if err != nil {
		t.Fatalf("newInstruments: %v", err)
	}
	return &Provider{tracer: tp.Tracer(ScopeName), inst: inst}, sr, reader
}

func TestNoopProviderRecordsNothingAndDoesNotPanic(t *testing.T) {
	p := Noop()
	if p.Tracer() == nil {
		t.Fatal("Noop provider has a nil tracer")
	}
	ctx := context.Background()
	// Every emit path must be safe on the off provider — that is the whole point of
	// instrumenting unconditionally.
	_, span := p.Tracer().Start(ctx, SpanInvocation)
	span.End()
	p.RecordInvocation(ctx, "implement", "done", time.Second)
	p.RecordLLMTurn(ctx, "m", 1, 2, 3, 4, time.Second)
	p.RecordCost(ctx, "m", 0.5)
	p.RecordGateRun(ctx, true, time.Second)
	p.RecordContextElision(ctx, "implement", 8, 40960)
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Noop Shutdown: %v", err)
	}
}

func TestSetupEmptyEndpointIsOff(t *testing.T) {
	p, err := Setup(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Setup off: %v", err)
	}
	if p.shutdown != nil {
		t.Error("an empty endpoint must produce an inert (Noop) provider with no exporters to shut down")
	}
}

func TestSetupStdoutBuildsAndShutsDown(t *testing.T) {
	p, err := Setup(context.Background(), Config{Endpoint: EndpointStdout, ServiceName: "harness-test"})
	if err != nil {
		t.Fatalf("Setup stdout: %v", err)
	}
	if p.shutdown == nil {
		t.Fatal("stdout endpoint must build real exporters with a shutdown")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSetupOTLPEndpointBuildsLazily(t *testing.T) {
	// A host:port endpoint must build without dialing — Setup is network-free, the
	// exporter connects on first export. Nothing is listening at this address, so a
	// non-lazy (blocking) exporter would fail at Setup; that it builds is the property
	// under test. Shutdown then tries a final flush to the dead collector and errors —
	// expected and logged in production, so it is drained (with a bounded ctx) but not
	// asserted here.
	p, err := Setup(context.Background(), Config{Endpoint: "127.0.0.1:65534"})
	if err != nil {
		t.Fatalf("Setup otlp: %v", err)
	}
	if p.shutdown == nil {
		t.Fatal("otlp endpoint must build real exporters with a shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Shutdown(ctx) // best-effort drain; a flush to the unreachable collector errors
}

func TestSetupOTLPWithHeadersAndTLSBuildsLazily(t *testing.T) {
	// Auth headers + TLS must thread into all three exporters without dialing — the same
	// lazy-build property as the plain OTLP case, now with the authenticated-backend
	// posture (resolved headers, transport security). Nothing listens here, so a non-lazy
	// build would fail; that it builds is the property under test.
	p, err := Setup(context.Background(), Config{
		Endpoint: "127.0.0.1:65534",
		Headers:  map[string]string{"organization": "default", "authorization": "Basic deadbeef"},
		TLS:      true,
	})
	if err != nil {
		t.Fatalf("Setup otlp+headers+tls: %v", err)
	}
	if p.shutdown == nil {
		t.Fatal("otlp endpoint must build real exporters with a shutdown")
	}
	if p.LoggerProvider() == nil {
		t.Error("a real provider must expose a non-nil LoggerProvider for the slog bridge")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Shutdown(ctx) // best-effort drain
}

// TestLoggerProviderNeverNil pins the invariant the T5.13 slog bridge relies on: every
// construction path yields a usable LoggerProvider, so bridge wiring is unconditional.
func TestLoggerProviderNeverNil(t *testing.T) {
	if Noop().LoggerProvider() == nil {
		t.Error("Noop LoggerProvider is nil")
	}
	stdout, err := Setup(context.Background(), Config{Endpoint: EndpointStdout})
	if err != nil {
		t.Fatalf("Setup stdout: %v", err)
	}
	if stdout.LoggerProvider() == nil {
		t.Error("stdout LoggerProvider is nil")
	}
	_ = stdout.Shutdown(context.Background())
}

func TestTracerStartsNamedSpanWithAttributes(t *testing.T) {
	p, sr, _ := newRecording(t)
	_, span := p.Tracer().Start(context.Background(), SpanLLMTurn)
	span.End()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name() != SpanLLMTurn {
		t.Errorf("span name = %q, want %q", spans[0].Name(), SpanLLMTurn)
	}
}

func TestRecordLLMTurnEmitsLatencyThroughputAndTokens(t *testing.T) {
	p, _, reader := newRecording(t)
	p.RecordLLMTurn(context.Background(), "claude-opus-4-8", 100, 50, 10, 5, 250*time.Millisecond)

	rm := collect(t, reader)
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = true
		}
	}
	for _, want := range []string{MetricLLMTurns, MetricLLMTurnDuration, MetricTokens} {
		if !got[want] {
			t.Errorf("missing metric %q (have %v)", want, got)
		}
	}

	// The token counter splits by kind: input/output/cache_read/cache_write all set, so
	// four data points, never collapsed into one untyped sum.
	tokens := findSum(t, rm, MetricTokens)
	if len(tokens.DataPoints) != 4 {
		t.Fatalf("token data points = %d, want 4 (one per kind)", len(tokens.DataPoints))
	}
	kinds := map[string]int64{}
	for _, dp := range tokens.DataPoints {
		v, ok := dp.Attributes.Value(attribute.Key(AttrTokenKind))
		if !ok {
			t.Fatalf("token data point missing %q attribute", AttrTokenKind)
		}
		kinds[v.AsString()] = dp.Value
	}
	if kinds[TokenKindInput] != 100 || kinds[TokenKindOutput] != 50 ||
		kinds[TokenKindCacheRead] != 10 || kinds[TokenKindCacheWrite] != 5 {
		t.Errorf("token kinds = %v, want input100/output50/cache_read10/cache_write5", kinds)
	}
}

func TestRecordTokensSkipsZeroKinds(t *testing.T) {
	p, _, reader := newRecording(t)
	// Only input nonzero — a zero kind must record no data point rather than a noisy zero.
	p.RecordLLMTurn(context.Background(), "m", 7, 0, 0, 0, time.Millisecond)
	rm := collect(t, reader)
	tokens := findSum(t, rm, MetricTokens)
	if len(tokens.DataPoints) != 1 {
		t.Fatalf("token data points = %d, want 1 (zeros skipped)", len(tokens.DataPoints))
	}
}

func TestRecordCostSkipsZero(t *testing.T) {
	p, _, reader := newRecording(t)
	p.RecordCost(context.Background(), "m", 0) // unpriced model contributes nothing
	rm := collect(t, reader)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == MetricCostUSD {
				t.Fatalf("zero cost recorded a %q data point", MetricCostUSD)
			}
		}
	}
}

func TestRecordGateRunCarriesPassedAttribute(t *testing.T) {
	p, _, reader := newRecording(t)
	p.RecordGateRun(context.Background(), false, 3*time.Second)
	rm := collect(t, reader)
	runs := findSumInt(t, rm, MetricGateRuns)
	if len(runs.DataPoints) != 1 {
		t.Fatalf("gate-run data points = %d, want 1", len(runs.DataPoints))
	}
	v, ok := runs.DataPoints[0].Attributes.Value(attribute.Key(AttrGatePassed))
	if !ok || v.AsBool() != false {
		t.Errorf("gate-run passed attribute = %v (ok=%v), want false", v.AsBool(), ok)
	}
}

func TestRecordContextElisionByRoleSkipsZero(t *testing.T) {
	p, _, reader := newRecording(t)
	// A zero delta (no boundary advance this turn) must record nothing.
	p.RecordContextElision(context.Background(), "implement", 0, 0)
	p.RecordContextElision(context.Background(), "implement", 8, 40960)
	rm := collect(t, reader)
	results := findSumInt(t, rm, MetricContextElidedResults)
	if len(results.DataPoints) != 1 || results.DataPoints[0].Value != 8 {
		t.Fatalf("elided-results data points = %+v, want one point of 8 (zero delta skipped)", results.DataPoints)
	}
	v, ok := results.DataPoints[0].Attributes.Value(attribute.Key(AttrIssueRole))
	if !ok || v.AsString() != "implement" {
		t.Errorf("role attribute = %v (ok=%v), want implement", v, ok)
	}
	bytes := findSumInt(t, rm, MetricContextElidedBytes)
	if len(bytes.DataPoints) != 1 || bytes.DataPoints[0].Value != 40960 {
		t.Fatalf("elided-bytes data points = %+v, want one point of 40960", bytes.DataPoints)
	}
}

// --- metric collection helpers ----------------------------------------------

func collect(t *testing.T, r *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

func findSum(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				s, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("metric %q is %T, want Sum[int64]", name, m.Data)
				}
				return s
			}
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Sum[int64]{}
}

func findSumInt(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	return findSum(t, rm, name)
}
