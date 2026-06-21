package telemetry

import (
	"context"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

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
