package telemetry

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// capturingProcessor records the severities of every record it is forwarded, so a test
// can assert exactly which records the min-severity filter let through.
type capturingProcessor struct {
	emitted    []otellog.Severity
	flushed    bool
	shutdown   bool
}

func (c *capturingProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	c.emitted = append(c.emitted, r.Severity())
	return nil
}
func (c *capturingProcessor) Shutdown(context.Context) error   { c.shutdown = true; return nil }
func (c *capturingProcessor) ForceFlush(context.Context) error { c.flushed = true; return nil }

func record(sev otellog.Severity) *sdklog.Record {
	var r sdklog.Record
	r.SetSeverity(sev)
	return &r
}

func TestMinSeverityProcessorDropsBelowInfo(t *testing.T) {
	next := &capturingProcessor{}
	p := newMinSeverityProcessor(otellog.SeverityInfo, next)
	ctx := context.Background()

	// Below Info (Debug/Trace) is dropped; Info and above pass; undefined is kept
	// (indeterminate → default to forwarding rather than silently dropping).
	cases := []struct {
		sev  otellog.Severity
		pass bool
	}{
		{otellog.SeverityTrace1, false},
		{otellog.SeverityDebug, false},
		{otellog.SeverityInfo, true},
		{otellog.SeverityWarn, true},
		{otellog.SeverityError, true},
		{otellog.SeverityUndefined, true},
	}
	var want []otellog.Severity
	for _, c := range cases {
		if err := p.OnEmit(ctx, record(c.sev)); err != nil {
			t.Fatalf("OnEmit(%v): %v", c.sev, err)
		}
		if c.pass {
			want = append(want, c.sev)
		}
		// The Enabled hint must agree with OnEmit's drop decision so the SDK can skip
		// constructing a below-threshold record at the logger boundary.
		if got := p.Enabled(ctx, sdklog.EnabledParameters{Severity: c.sev}); got != c.pass {
			t.Errorf("Enabled(%v) = %v, want %v", c.sev, got, c.pass)
		}
	}
	if len(next.emitted) != len(want) {
		t.Fatalf("forwarded %v severities, want %v", next.emitted, want)
	}
	for i, sev := range want {
		if next.emitted[i] != sev {
			t.Errorf("forwarded[%d] = %v, want %v", i, next.emitted[i], sev)
		}
	}
}

func TestMinSeverityProcessorDelegatesLifecycle(t *testing.T) {
	next := &capturingProcessor{}
	p := newMinSeverityProcessor(otellog.SeverityInfo, next)
	if err := p.ForceFlush(context.Background()); err != nil || !next.flushed {
		t.Errorf("ForceFlush did not delegate (err=%v flushed=%v)", err, next.flushed)
	}
	if err := p.Shutdown(context.Background()); err != nil || !next.shutdown {
		t.Errorf("Shutdown did not delegate (err=%v shutdown=%v)", err, next.shutdown)
	}
}
