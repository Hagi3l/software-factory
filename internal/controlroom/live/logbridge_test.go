package live_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/controlroom/live"
)

// bridgeLogger wires a LogBridge over a buffer-backed base handler and returns the
// logger, the buffer (to assert the base handler still ran), and the activity feed.
func bridgeLogger(t *testing.T, act *live.Activity, hub *live.Hub) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(live.NewLogBridge(base, hub, act)), &buf
}

func TestLogBridge_TeesRecordAsSystemEvent(t *testing.T) {
	act := live.NewActivity(16)
	hub := live.NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()
	log, buf := bridgeLogger(t, act, hub)

	log.InfoContext(t.Context(), "orchestrator: dispatched", "issue", "factory-1", "role", "test-author")

	// The base handler still wrote the record — the bridge tees, it does not replace.
	if !strings.Contains(buf.String(), "dispatched") {
		t.Fatalf("base handler not invoked; buffer = %q", buf.String())
	}

	got := act.Recent()
	if len(got) != 1 {
		t.Fatalf("feed entries = %d, want 1", len(got))
	}
	e := got[0]
	if e.Source != live.SourceSystem {
		t.Errorf("source = %q, want %q", e.Source, live.SourceSystem)
	}
	if e.AgentID != "orchestrator" {
		t.Errorf("component = %q, want orchestrator", e.AgentID)
	}
	if e.Kind != "info" {
		t.Errorf("kind = %q, want info", e.Kind)
	}
	// The "component: " prefix is stripped and the attrs are appended to the detail.
	if !strings.Contains(e.Detail, "dispatched") || !strings.Contains(e.Detail, "issue=factory-1") || !strings.Contains(e.Detail, "role=test-author") {
		t.Errorf("detail = %q, want dispatched + issue + role", e.Detail)
	}

	// A nudge was broadcast so connected browsers refetch.
	select {
	case ev := <-ch:
		if ev.Name != "agent-event" {
			t.Errorf("nudge event name = %q, want agent-event", ev.Name)
		}
	default:
		t.Fatal("expected a hub broadcast nudge, got none")
	}
}

func TestLogBridge_LevelMapsToKind(t *testing.T) {
	cases := []struct {
		log  func(*slog.Logger)
		kind string
	}{
		{func(l *slog.Logger) { l.WarnContext(context.Background(), "runner: slow") }, "warn"},
		{func(l *slog.Logger) { l.ErrorContext(context.Background(), "gate: failed") }, "error"},
	}
	for _, c := range cases {
		act := live.NewActivity(4)
		log, _ := bridgeLogger(t, act, live.NewHub())
		c.log(log)
		got := act.Recent()
		if len(got) != 1 || got[0].Kind != c.kind {
			t.Fatalf("kind = %v, want %q", got, c.kind)
		}
	}
}

func TestLogBridge_NoComponentPrefixAttributedToHarness(t *testing.T) {
	act := live.NewActivity(4)
	log, _ := bridgeLogger(t, act, live.NewHub())

	log.InfoContext(t.Context(), "starting up")

	got := act.Recent()
	if len(got) != 1 || got[0].AgentID != "factory" || got[0].Detail != "starting up" {
		t.Fatalf("entry = %+v, want component factory, detail 'starting up'", got)
	}
}

func TestLogBridge_WithAttrsAreIncluded(t *testing.T) {
	act := live.NewActivity(4)
	log, _ := bridgeLogger(t, act, live.NewHub())

	log.With("run", "abc").InfoContext(t.Context(), "runner: provisioned", "id", "sb-1")

	got := act.Recent()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Detail, "run=abc") || !strings.Contains(got[0].Detail, "id=sb-1") {
		t.Errorf("detail = %q, want both the With attr and the call attr", got[0].Detail)
	}
}

func TestLogBridge_RespectsBaseLevel(t *testing.T) {
	act := live.NewActivity(4)
	// Base handler is Info-level (from bridgeLogger), so a Debug record is dropped by
	// Enabled and never reaches the feed.
	log, _ := bridgeLogger(t, act, live.NewHub())

	log.DebugContext(t.Context(), "orchestrator: noisy detail")

	if got := act.Recent(); len(got) != 0 {
		t.Fatalf("feed entries = %d, want 0 (debug below base level)", len(got))
	}
}
