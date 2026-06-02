package live_test

import (
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/live"
)

// TestActiveAgents_CountsDistinctRecentAgents proves the status bar's "active agents" figure:
// distinct agent ids seen within the trailing window, counted once each regardless of how many
// events they emitted.
func TestActiveAgents_CountsDistinctRecentAgents(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", token("he"))
	a.Record("inv-1", token("llo")) // same agent, coalesced — still one agent
	a.Record("inv-2", token("hi"))

	if got := a.ActiveAgents(time.Minute); got != 2 {
		t.Fatalf("ActiveAgents = %d, want 2 (inv-1, inv-2)", got)
	}
}

// TestActiveAgents_ExcludesSystemRows confirms only sandboxed-agent rows count — the factory's own
// teed log rows (orchestrator/runner/gate) are not agents and must not inflate the figure.
func TestActiveAgents_ExcludesSystemRows(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", token("x"))
	a.RecordSystem("info", "orchestrator", "dispatching")
	a.RecordSystem("warn", "runner", "provisioning")

	if got := a.ActiveAgents(time.Minute); got != 1 {
		t.Fatalf("ActiveAgents = %d, want 1 (only inv-1; system rows excluded)", got)
	}
}

// TestActiveAgents_WindowExcludesStale confirms the trailing window filter: with the cutoff pushed
// into the future (a negative window), every recorded event is "stale" and nothing counts — the
// same path that drops an agent whose only events have aged past the window.
func TestActiveAgents_WindowExcludesStale(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", token("x"))
	a.Record("inv-2", token("y"))

	if got := a.ActiveAgents(-time.Hour); got != 0 {
		t.Fatalf("ActiveAgents(-1h) = %d, want 0 (all events older than the cutoff)", got)
	}
}

// TestActiveAgents_EmptyIsZero confirms an idle factory reports zero active agents.
func TestActiveAgents_EmptyIsZero(t *testing.T) {
	a := live.NewActivity(16)
	if got := a.ActiveAgents(time.Minute); got != 0 {
		t.Fatalf("ActiveAgents on empty buffer = %d, want 0", got)
	}
}
