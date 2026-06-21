package telemetry

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// WrapLogHandler fans base out to a second slog.Handler that bridges every record to the
// OTel logs pipeline (otelslog → the LoggerProvider Setup built), so one trusted-side
// slog source feeds both the console/feed sinks (base) and the OTLP logs backend without a
// second instrumentation pass — the "the broker is already the collector" principle
// applied to host-side logs (specs/observability.md "Logs are trusted-side only"). It is
// a no-op passthrough when export is off (a Noop/test provider has no real log pipeline),
// so the run wiring can call it unconditionally. name is the instrumentation scope the
// records are attributed to (the service name).
func (p *Provider) WrapLogHandler(base slog.Handler, name string) slog.Handler {
	// shutdown is non-nil only for a Setup-built provider with a live exporter pipeline;
	// Noop and the test NewWith provider both leave it nil and export nothing, so adding
	// an otelslog sink over their inert LoggerProvider would only burn allocations.
	if p == nil || p.shutdown == nil {
		return base
	}
	otelHandler := otelslog.NewHandler(name, otelslog.WithLoggerProvider(p.logs))
	return newMultiHandler(base, otelHandler)
}

// multiHandler fans one slog.Record out to several handlers — the console/feed handler and
// the OTel bridge — so a single logger writes to every sink. It is the host-side analog
// of the control-room LogBridge's tee, generalized to N terminal handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(hs ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: hs}
}

// Enabled reports true if any sink would accept the level, so a record is dropped only
// when every sink would drop it (e.g. the OTel min-severity filter and the console level
// both reject it).
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every sink, joining their errors so one failing sink
// neither drops the record from the others nor masks its own error.
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

// minSeverityProcessor drops log records below a severity threshold before they reach the
// wrapped processor (the batch exporter). It is how "batch logs at Info+" is realized:
// debug/trace records — chatty by nature on a busy run — never enter the batch queue, so
// the export wire carries only Info and above. Records with an unset (undefined) severity
// are kept, matching the FilterProcessor "default to true when indeterminate" contract.
//
// It implements both sdklog.Processor (OnEmit/Shutdown/ForceFlush) and the
// log.FilterProcessor Enabled hint, so the SDK can also skip *constructing* a dropped
// record at the logger boundary, not just discard it here.
type minSeverityProcessor struct {
	min  otellog.Severity
	next sdklog.Processor
}

// newMinSeverityProcessor wraps next so only records at or above min are forwarded.
func newMinSeverityProcessor(min otellog.Severity, next sdklog.Processor) *minSeverityProcessor {
	return &minSeverityProcessor{min: min, next: next}
}

// passes reports whether a record at sev should be forwarded: at/above the threshold, or
// of undefined severity (indeterminate — kept rather than silently dropped).
func (p *minSeverityProcessor) passes(sev otellog.Severity) bool {
	return sev == otellog.SeverityUndefined || sev >= p.min
}

func (p *minSeverityProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	if !p.passes(record.Severity()) {
		return nil
	}
	return p.next.OnEmit(ctx, record)
}

// Enabled is the FilterProcessor hint the SDK consults before building a record, so a
// below-threshold log call is cheap (no record allocation). The param carries only
// partial information (often just the severity); we answer from it.
func (p *minSeverityProcessor) Enabled(_ context.Context, param sdklog.EnabledParameters) bool {
	return p.passes(param.Severity)
}

func (p *minSeverityProcessor) Shutdown(ctx context.Context) error   { return p.next.Shutdown(ctx) }
func (p *minSeverityProcessor) ForceFlush(ctx context.Context) error { return p.next.ForceFlush(ctx) }
