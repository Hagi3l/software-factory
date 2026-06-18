package wizard

import (
	"strings"
	"testing"
)

// TestEpicModeFoldsOneRootRuleIntoPrompt proves WithEpicMode (T8.7) folds the one-root contract
// into a Create session's system prompt, so the planner honors it when drafting rather than having
// the consent gate refuse a two-root draft after the fact. The directive rides the system channel
// (the planner's persona), never the human↔planner transcript — so it shapes reasoning without
// appearing as a chat message. nil adapter is fine: New() only registers the session, it never
// calls the model.
func TestEpicModeFoldsOneRootRuleIntoPrompt(t *testing.T) {
	p := NewPlanner(nil, "base persona", WithEpicMode())
	sess := p.New()

	if !strings.HasPrefix(sess.persona, "base persona") {
		t.Fatalf("epic grounding should append to the persona, not replace it: %q", sess.persona)
	}
	if !strings.Contains(sess.persona, "exactly one") {
		t.Errorf("epic-mode persona missing the one-root rule: %q", sess.persona)
	}
	if !strings.Contains(sess.persona, "epic") {
		t.Errorf("epic-mode persona should name the integration mode: %q", sess.persona)
	}
	// The directive is background context only — it must not leak into the transcript the human reads.
	if len(sess.Messages()) != 0 {
		t.Errorf("epic grounding must not seed a transcript message, got %+v", sess.Messages())
	}
}

// TestPerItemModeLeavesPromptUnchanged proves the grounding is opt-in: without WithEpicMode the
// session's system prompt is byte-for-byte the bare persona (the default per-item mode is
// unchanged), so existing deployments see no behavior drift.
func TestPerItemModeLeavesPromptUnchanged(t *testing.T) {
	p := NewPlanner(nil, "base persona")
	if got := p.New().persona; got != "base persona" {
		t.Errorf("per-item session persona = %q, want the bare persona unchanged", got)
	}
}
