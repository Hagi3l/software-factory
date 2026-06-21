package telemetry

import (
	"context"
	"log/slog"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// capturingHandler is a minimal slog.Handler that records the messages it is handed and
// the attrs bound via WithAttrs, so a test can assert the fan-out delivered a record to it.
type capturingHandler struct {
	level    slog.Level
	messages *[]string
	attrs    []slog.Attr
}

func (h *capturingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.messages = append(*h.messages, r.Message)
	return nil
}
func (h *capturingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &capturingHandler{level: h.level, messages: h.messages, attrs: append(append([]slog.Attr(nil), h.attrs...), as...)}
}
func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

func TestMultiHandlerFansOutToEverySink(t *testing.T) {
	var aMsgs, bMsgs []string
	a := &capturingHandler{messages: &aMsgs}
	b := &capturingHandler{messages: &bMsgs}
	m := newMultiHandler(a, b)
	ctx := context.Background()

	if err := m.Handle(ctx, slog.Record{Message: "hello"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(aMsgs) != 1 || len(bMsgs) != 1 || aMsgs[0] != "hello" || bMsgs[0] != "hello" {
		t.Fatalf("record not fanned to both sinks: a=%v b=%v", aMsgs, bMsgs)
	}

	// Enabled is OR across sinks: a record one sink would take is processed even if another
	// sink drops it.
	hi := &capturingHandler{level: slog.LevelError, messages: &aMsgs}
	lo := &capturingHandler{level: slog.LevelDebug, messages: &bMsgs}
	if !newMultiHandler(hi, lo).Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled should be true when any sink accepts the level")
	}
	if newMultiHandler(hi).Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled should be false when the only sink rejects the level")
	}

	// WithAttrs propagates to every sink (so the per-invocation join columns reach both the
	// console/feed and the OTLP backend).
	wa := m.WithAttrs([]slog.Attr{slog.String("k", "v")})
	if _, ok := wa.(*multiHandler); !ok {
		t.Fatal("WithAttrs must return a multiHandler")
	}
}

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

func TestWrapLogHandlerOffIsPassthrough(t *testing.T) {
	// An off (Noop) provider has no real log pipeline, so wrapping must return the base
	// handler unchanged — no otelslog sink, no allocation. This is what lets the run wiring
	// call WrapLogHandler unconditionally.
	base := &capturingHandler{messages: &[]string{}}
	if got := Noop().WrapLogHandler(base, "harness"); got != slog.Handler(base) {
		t.Errorf("off provider must return base unchanged, got %T", got)
	}
	var nilProvider *Provider
	if got := nilProvider.WrapLogHandler(base, "harness"); got != slog.Handler(base) {
		t.Errorf("nil provider must return base unchanged, got %T", got)
	}
}

func TestWrapLogHandlerOnFansOutKeepingBase(t *testing.T) {
	// A real (exporting) provider must fan out to BOTH the base sink and the OTel bridge:
	// the returned handler differs from base, and a record logged through it still reaches
	// base (so console/feed never lose a record when OTLP export is on).
	p, err := Setup(context.Background(), Config{Endpoint: EndpointStdout})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	var msgs []string
	base := &capturingHandler{messages: &msgs}
	wrapped := p.WrapLogHandler(base, "harness")
	if wrapped == slog.Handler(base) {
		t.Fatal("an exporting provider must wrap base, not return it unchanged")
	}
	slog.New(wrapped).InfoContext(context.Background(), "orchestrator: dispatched")
	if len(msgs) != 1 || msgs[0] != "orchestrator: dispatched" {
		t.Fatalf("base sink lost the record when OTLP export is on: %v", msgs)
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
