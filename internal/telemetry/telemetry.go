package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	mnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tnoop "go.opentelemetry.io/otel/trace/noop"
)

// EndpointStdout selects the stdout exporter: traces and metrics print to the process's
// own stdout with no collector required — the offline, self-contained dev default. An
// empty endpoint disables export entirely (the production-safe default, and what an
// unset config resolves to); any other value is an OTLP/gRPC collector address
// (host:port). Standing up an external backend (Tempo/Jaeger) at such an address is a
// Phase 5 deployment step (T5.8); the exporter wiring is complete here so the knob is
// real, not stubbed.
const EndpointStdout = "stdout"

// Config selects how telemetry is exported. It is derived from config.Infra.OTel — the
// single source of truth for the endpoint lives in the infra overlay.
type Config struct {
	// Endpoint is "" (off), "stdout" (stdout exporter), or an OTLP/gRPC collector
	// host:port. config.Validate enforces this set before Setup runs.
	Endpoint string
	// ServiceName is the resource service.name reported to the backend. Defaults to
	// "harness" when empty.
	ServiceName string
	// Headers are sent with every OTLP export — the auth + routing metadata an
	// authenticated backend (e.g. OpenObserve, Grafana Cloud, Honeycomb) requires. Values
	// are already resolved: the caller (config.OTelConfig.ResolveHeaders) expands any
	// ${ENV} reference from the environment, so a credential never lives literal in
	// config. Ignored by the stdout exporter (nothing to authenticate to).
	Headers map[string]string
	// TLS selects transport security for the OTLP/gRPC dial. False (the default) dials
	// insecurely — the local-collector posture the dev endpoint localhost:4317 expects.
	// True dials with the host's root CAs, the posture an authenticated public backend
	// reached over the internet requires. Ignored by the stdout exporter.
	TLS bool
}

// Provider is the harness's live telemetry: a tracer for spans and a set of metric
// instruments, plus a shutdown that flushes pending exports. Every instrumented call
// site holds one and emits unconditionally; whether anything is actually exported is the
// Provider's concern, not the call site's — an off Provider (Noop) makes every emit a
// no-op. This mirrors the nil-logger→discard pattern: components default a nil Provider
// to Noop so telemetry is never a required collaborator.
type Provider struct {
	tracer   trace.Tracer
	logs     otellog.LoggerProvider
	inst     *Instruments
	shutdown func(context.Context) error
}

// Tracer returns the span tracer. It is never nil (Noop supplies an inert one).
func (p *Provider) Tracer() trace.Tracer { return p.tracer }

// LoggerProvider returns the OTel logs provider the trusted-side slog→OTel bridge
// (T5.13) builds its handler over. It is never nil (Noop supplies an inert one), so the
// bridge wiring is unconditional — whether a record is actually exported is the
// Provider's concern, mirroring Tracer()/the metric instruments. The harness only ever
// feeds it from trusted host-side code; untrusted model text and sandbox output are span
// attributes / artifact-store evidence, never log records (specs/observability.md "Logs
// are trusted-side only").
func (p *Provider) LoggerProvider() otellog.LoggerProvider { return p.logs }

// Shutdown flushes and stops the exporters, draining any batched spans/metrics. It is a
// no-op for a Noop provider. The composition root defers it so a clean process exit
// delivers the final batch.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Noop returns a Provider that exports nothing: a real-but-inert tracer and metric
// instruments backed by the OTel no-op providers. It is what an unset endpoint resolves
// to and what a nil Provider defaults to at every call site, so instrumentation runs
// unconditionally with zero overhead when telemetry is off.
func Noop() *Provider {
	inst, _ := newInstruments(mnoop.NewMeterProvider().Meter(ScopeName))
	return &Provider{
		tracer: tnoop.NewTracerProvider().Tracer(ScopeName),
		logs:   lognoop.NewLoggerProvider(),
		inst:   inst,
	}
}

// NewWith builds a Provider over caller-supplied OTel providers instead of the
// endpoint-driven exporter pipeline Setup wires. It exists so a caller that already owns a
// TracerProvider/MeterProvider — chiefly the cross-package tests that assert the harness's
// call sites emit the right spans and metrics against in-memory recorders — can obtain a
// real, recording Provider without Setup's exporter machinery. Production uses Setup; this
// has no Shutdown (the caller owns the pipelines' lifecycle). It returns an error only if
// the instruments fail to build (a schema typo), matching Setup.
func NewWith(tp trace.TracerProvider, mp metric.MeterProvider) (*Provider, error) {
	inst, err := newInstruments(mp.Meter(ScopeName))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build instruments: %w", err)
	}
	return &Provider{tracer: tp.Tracer(ScopeName), logs: lognoop.NewLoggerProvider(), inst: inst}, nil
}

// Setup builds a Provider for the configured endpoint. An empty endpoint returns Noop
// (export off). It is network-free: the OTLP exporter dials lazily on first export, so a
// missing collector degrades to dropped exports and logged errors rather than a boot
// failure, and Setup can run in the otherwise network-free composition root.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" {
		return Noop(), nil
	}
	name := cfg.ServiceName
	if name == "" {
		name = "harness"
	}
	res := resource.NewSchemaless(attribute.String("service.name", name))

	spanExp, metExp, logExp, err := exporters(ctx, cfg)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExp),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metExp)),
		sdkmetric.WithResource(res),
	)
	// Logs ride a batch processor (one export RPC per batch, not per record, so a busy
	// run doesn't firehose the wire) wrapped in a min-severity filter that drops anything
	// below Info — the third signal, off the same endpoint as traces/metrics.
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(newMinSeverityProcessor(otellog.SeverityInfo, sdklog.NewBatchProcessor(logExp))),
	)
	inst, err := newInstruments(mp.Meter(ScopeName))
	if err != nil {
		// Tear down the providers we just built so a failed Setup leaks no exporter.
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		_ = lp.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: build instruments: %w", err)
	}

	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return &Provider{tracer: tp.Tracer(ScopeName), logs: lp, inst: inst, shutdown: shutdown}, nil
}

// exporters builds the trace + metric + log exporters for the configured endpoint, all
// three off the one endpoint so a single backend ingests the whole record. "stdout"
// prints to the process stdout (offline, no auth). Any other value is an OTLP/gRPC
// collector address, dialed lazily with the configured auth/routing Headers and either
// insecurely (cfg.TLS=false, the local-collector default) or over TLS with the host's
// root CAs (cfg.TLS=true, an authenticated public backend).
func exporters(ctx context.Context, cfg Config) (sdktrace.SpanExporter, sdkmetric.Exporter, sdklog.Exporter, error) {
	if cfg.Endpoint == EndpointStdout {
		spanExp, err := stdouttrace.New()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telemetry: stdout trace exporter: %w", err)
		}
		metExp, err := stdoutmetric.New()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telemetry: stdout metric exporter: %w", err)
		}
		logExp, err := stdoutlog.New()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telemetry: stdout log exporter: %w", err)
		}
		return spanExp, metExp, logExp, nil
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
	if !cfg.TLS {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		traceOpts = append(traceOpts, otlptracegrpc.WithHeaders(cfg.Headers))
		metricOpts = append(metricOpts, otlpmetricgrpc.WithHeaders(cfg.Headers))
		logOpts = append(logOpts, otlploggrpc.WithHeaders(cfg.Headers))
	}

	spanExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("telemetry: otlp trace exporter: %w", err)
	}
	metExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("telemetry: otlp metric exporter: %w", err)
	}
	logExp, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("telemetry: otlp log exporter: %w", err)
	}
	return spanExp, metExp, logExp, nil
}

// Instruments are the metric instruments for the schema's three families. They are
// exposed so a test can drive recording, but production code records through the typed
// Record* helpers below so metric attributes stay defined in one place.
type Instruments struct {
	Invocations          metric.Int64Counter
	InvocationDuration   metric.Float64Histogram
	LLMTurns             metric.Int64Counter
	LLMTurnDuration      metric.Float64Histogram
	Tokens               metric.Int64Counter
	CostUSD              metric.Float64Counter
	GateRuns             metric.Int64Counter
	GateDuration         metric.Float64Histogram
	ContextElidedResults metric.Int64Counter
	ContextElidedBytes   metric.Int64Counter
}

// newInstruments creates every instrument from a meter, joining any creation errors so a
// schema typo surfaces once at startup rather than as silent dropped metrics.
func newInstruments(m metric.Meter) (*Instruments, error) {
	var errs []error
	counter := func(name, desc string) metric.Int64Counter {
		c, err := m.Int64Counter(name, metric.WithDescription(desc))
		errs = append(errs, err)
		return c
	}
	fcounter := func(name, desc string) metric.Float64Counter {
		c, err := m.Float64Counter(name, metric.WithDescription(desc))
		errs = append(errs, err)
		return c
	}
	hist := func(name, desc string) metric.Float64Histogram {
		h, err := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"))
		errs = append(errs, err)
		return h
	}
	in := &Instruments{
		Invocations:          counter(MetricInvocations, "completed agent invocations, by role and result status"),
		InvocationDuration:   hist(MetricInvocationDuration, "wall-clock duration of an agent invocation"),
		LLMTurns:             counter(MetricLLMTurns, "model turns relayed through the broker, by model"),
		LLMTurnDuration:      hist(MetricLLMTurnDuration, "latency of one brokered model turn"),
		Tokens:               counter(MetricTokens, "tokens consumed, by model and token kind"),
		CostUSD:              fcounter(MetricCostUSD, "model spend in USD, by model"),
		GateRuns:             counter(MetricGateRuns, "gate verdicts reached, by pass/fail"),
		GateDuration:         hist(MetricGateDuration, "wall-clock duration of a gate run"),
		ContextElidedResults: counter(MetricContextElidedResults, "tool results aged out of the model's view, by role"),
		ContextElidedBytes:   counter(MetricContextElidedBytes, "tool-result content bytes elided from the model's view, by role"),
	}
	return in, errors.Join(errs...)
}

// RecordInvocation records one completed invocation's throughput and duration. The
// runner calls it whatever the disposition, so the counter measures real agent
// throughput (including failed/retried attempts), keyed by role and result status.
func (p *Provider) RecordInvocation(ctx context.Context, role, status string, d time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String(AttrIssueRole, role),
		attribute.String(AttrResultStatus, status),
	)
	p.inst.Invocations.Add(ctx, 1, attrs)
	p.inst.InvocationDuration.Record(ctx, d.Seconds(), attrs)
}

// RecordLLMTurn records one brokered model turn's latency, throughput, and per-kind
// token consumption. Cost in USD is recorded separately by the orchestrator, the only
// component that holds the per-model price table — the broker sees tokens, not dollars.
func (p *Provider) RecordLLMTurn(ctx context.Context, model string, in, out, cacheRead, cacheWrite int, d time.Duration) {
	a := metric.WithAttributes(attribute.String(AttrModel, model))
	p.inst.LLMTurns.Add(ctx, 1, a)
	p.inst.LLMTurnDuration.Record(ctx, d.Seconds(), a)
	p.recordTokens(ctx, model, TokenKindInput, in)
	p.recordTokens(ctx, model, TokenKindOutput, out)
	p.recordTokens(ctx, model, TokenKindCacheRead, cacheRead)
	p.recordTokens(ctx, model, TokenKindCacheWrite, cacheWrite)
}

func (p *Provider) recordTokens(ctx context.Context, model, kind string, n int) {
	if n == 0 {
		return
	}
	p.inst.Tokens.Add(ctx, int64(n), metric.WithAttributes(
		attribute.String(AttrModel, model),
		attribute.String(AttrTokenKind, kind),
	))
}

// RecordCost records an invocation's priced spend in USD. The orchestrator calls it
// after pricing a Result's token usage against the issue's model rate; a zero cost (an
// unpriced model) records nothing, so the cost view shows only real spend.
func (p *Provider) RecordCost(ctx context.Context, model string, usd float64) {
	if usd == 0 {
		return
	}
	p.inst.CostUSD.Add(ctx, usd, metric.WithAttributes(attribute.String(AttrModel, model)))
}

// RecordContextElision records tool results aged out of the model's view by the agent
// loop (specs/components/agent.md "Tool-result aging") — the counter pair that makes the
// aging's effect measurable rather than assumed. The loop calls it with the DELTA each
// time the elision boundary advances (the aged view is recomputed per request, so
// recording totals would double-count). Role is the only dimension (bounded, per the
// cardinality rule); a zero delta records nothing.
func (p *Provider) RecordContextElision(ctx context.Context, role string, results, bytes int64) {
	if results == 0 {
		return
	}
	a := metric.WithAttributes(attribute.String(AttrIssueRole, role))
	p.inst.ContextElidedResults.Add(ctx, results, a)
	p.inst.ContextElidedBytes.Add(ctx, bytes, a)
}

// RecordGateRun records one gate verdict's throughput (by pass/fail) and duration. A
// gate that could not reach a verdict (an infra error) is not a verdict and is not
// recorded here.
func (p *Provider) RecordGateRun(ctx context.Context, passed bool, d time.Duration) {
	a := metric.WithAttributes(attribute.Bool(AttrGatePassed, passed))
	p.inst.GateRuns.Add(ctx, 1, a)
	p.inst.GateDuration.Record(ctx, d.Seconds(), a)
}
