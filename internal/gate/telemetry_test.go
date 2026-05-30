package gate

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/telemetry"
)

// A reached verdict must emit exactly one gate-run span (carrying the pass/fail outcome,
// the checks-run count, and the issue id recovered from the candidate ref) and one gate-run
// throughput data point. This locks the observability contract T4.10/T4.11 read: the gate
// runs in a trace distinct from the producer (producer ≠ verifier), so id correlation comes
// from the ref, not span parentage.
func TestRunEmitsGateRunSpanAndMetric(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel, err := telemetry.NewWith(tp, mp)
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	// One failing check, so the verdict is a real (false) outcome — the more interesting
	// case to record than an all-green pass.
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0, Stdout: []byte("built")},
		"make test-unit": {ExitCode: 1, Stderr: []byte("boom")},
	}}
	g := New(&fakeBackend{sb: sb}, testRegistry(), testStore(t), t.TempDir(), nil, tel)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (a check failed)")
	}

	// --- span ---
	spans := sr.Ended()
	var gr sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == telemetry.SpanGateRun {
			gr = s
		}
	}
	if gr == nil {
		t.Fatalf("no %q span emitted (got %d spans)", telemetry.SpanGateRun, len(spans))
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range gr.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if v := attrs[attribute.Key(telemetry.AttrGatePassed)]; v.AsBool() != false {
		t.Errorf("span %s = %v, want false", telemetry.AttrGatePassed, v.AsBool())
	}
	if v := attrs[attribute.Key(telemetry.AttrGateChecksRun)]; v.AsInt64() != int64(len(report.Checks)) {
		t.Errorf("span %s = %d, want %d", telemetry.AttrGateChecksRun, v.AsInt64(), len(report.Checks))
	}
	// The issue id is recovered from the candidate ref, not inherited from a parent span.
	wantID, _ := core.IssueIDFromCandidateBranch(testCandidate().Ref)
	if v := attrs[attribute.Key(telemetry.AttrIssueID)]; v.AsString() != wantID {
		t.Errorf("span %s = %q, want %q", telemetry.AttrIssueID, v.AsString(), wantID)
	}

	// --- metric ---
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == telemetry.MetricGateRuns {
				found = true
				s, ok := m.Data.(metricdata.Sum[int64])
				if !ok || len(s.DataPoints) != 1 {
					t.Fatalf("%s data = %T with %d points, want one Sum[int64] point", m.Name, m.Data, len(s.DataPoints))
				}
				v, ok := s.DataPoints[0].Attributes.Value(attribute.Key(telemetry.AttrGatePassed))
				if !ok || v.AsBool() != false {
					t.Errorf("%s point passed attr = %v (ok=%v), want false", m.Name, v.AsBool(), ok)
				}
			}
		}
	}
	if !found {
		t.Errorf("no %q metric emitted", telemetry.MetricGateRuns)
	}
}

// An infra error (provisioning failed — no verdict reached) must NOT record a gate-run
// metric point: the throughput counter and pass/fail split count only real verdicts, never
// a sandbox that died mid-run.
func TestRunInfraErrorRecordsNoGateRunMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tp := sdktrace.NewTracerProvider()
	tel, err := telemetry.NewWith(tp, mp)
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}

	g := New(&fakeBackend{provErr: errors.New("no host capacity")}, testRegistry(), testStore(t), t.TempDir(), nil, tel)
	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run returned nil error, want a provisioning (infra) error")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == telemetry.MetricGateRuns {
				t.Fatalf("an infra error recorded a %q point, want none", m.Name)
			}
		}
	}
}
