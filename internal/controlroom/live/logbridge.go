package live

import (
	"context"
	"log/slog"
	"strings"
)

// NewLogBridge wraps a base slog.Handler so every factory log record is teed into the
// activity feed as a "system" event, in addition to being written by the base handler.
// It is the system half of the feed: the trusted side (orchestrator, runner, gate) already
// narrates what it is doing through structured logs, so bridging that log *is* the
// "what is the factory doing" stream — no second instrumentation pass, the same logic as
// "the broker is already the collector" (specs/observability.md) applied to the host side.
//
// It only makes sense in the co-located run (where the control room shares the process
// that emits these logs); wire it in `software-factory run --serve-addr ...` and leave the base
// handler bare otherwise. Teeing is best-effort and non-blocking (the hub drops slow
// browsers; Activity is bounded), so it can never stall or fail a log call.
func NewLogBridge(inner slog.Handler, hub *Hub, act *Activity) slog.Handler {
	return &logBridge{inner: inner, hub: hub, act: act}
}

type logBridge struct {
	inner slog.Handler
	hub   *Hub
	act   *Activity
	attrs []slog.Attr // attrs bound by WithAttrs, replayed into each record's feed line
}

// Enabled defers to the base handler, so the feed honors the same level threshold as the
// log (the bridge never surfaces a record the base handler would itself drop).
func (b *logBridge) Enabled(ctx context.Context, level slog.Level) bool {
	return b.inner.Enabled(ctx, level)
}

// Handle tees the record into the feed, then hands it to the base handler. The base
// handler's result is what's returned — the tee must not turn a successful log into a
// failed one — and the tee runs first so a (best-effort) feed row is recorded even if the
// base write later errors.
func (b *logBridge) Handle(ctx context.Context, r slog.Record) error {
	b.tee(r)
	return b.inner.Handle(ctx, r)
}

func (b *logBridge) WithAttrs(as []slog.Attr) slog.Handler {
	return &logBridge{
		inner: b.inner.WithAttrs(as),
		hub:   b.hub,
		act:   b.act,
		attrs: append(append([]slog.Attr(nil), b.attrs...), as...),
	}
}

func (b *logBridge) WithGroup(name string) slog.Handler {
	// Groups namespace attrs in the base handler's output; the feed line is a flat
	// human summary, so the bridge keeps its own attr list flat and just propagates the
	// group to the base handler.
	return &logBridge{inner: b.inner.WithGroup(name), hub: b.hub, act: b.act, attrs: b.attrs}
}

// tee renders one record as a system feed row and nudges connected browsers to refetch.
// The factory logs messages as "component: what happened" (e.g. "orchestrator: dispatched"),
// so the prefix becomes the row's component label and the rest, with the record's attrs
// appended, becomes the detail.
func (b *logBridge) tee(r slog.Record) {
	component, detail := splitComponent(r.Message)

	var sb strings.Builder
	sb.WriteString(detail)
	appendAttr := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		sb.WriteByte(' ')
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		sb.WriteString(a.Value.String())
	}
	for _, a := range b.attrs {
		appendAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(a)
		return true
	})

	b.act.RecordSystem(levelString(r.Level), component, sb.String())
	// A content-less nudge: the activity view refetches the rendered list on any
	// agent-event, so reusing that event name fires the same refresh for system rows.
	b.hub.Broadcast(Event{Name: "agent-event"})
}

// splitComponent peels the "component: " prefix the factory uses off a log message,
// returning the component and the remaining detail. A message with no such prefix is
// attributed to the factory as a whole.
func splitComponent(msg string) (component, detail string) {
	if c, d, ok := strings.Cut(msg, ": "); ok && c != "" && !strings.ContainsAny(c, " ") {
		return c, d
	}
	return "factory", msg
}

// levelString maps a slog level to the lowercase kind the feed badges on.
func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
