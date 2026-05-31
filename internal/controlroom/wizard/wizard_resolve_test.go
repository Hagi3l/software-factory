package wizard_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/wizard"
)

// TestNewResolveGroundsAndOpens proves Resolve mode (T4.15): NewResolve records the dead-lettered
// issue it is unsticking, folds the escalation + spec slice into the planner's system prompt (so
// the planner reasons with that context, kept out of the visible transcript), and auto-opens the
// conversation with one turn — the "pre-loaded conversation" the spec promises. It drives the
// blocking fakeAdapter so the in-flight opening turn is observed deterministically.
func TestNewResolveGroundsAndOpens(t *testing.T) {
	fa := &fakeAdapter{release: make(chan struct{}), reply: "I see the ambiguity; here is a minimal refinement."}
	p := wizard.NewPlanner(fa, "BASE PERSONA", wizard.WithTurnTimeout(5*time.Second))

	sess := p.NewResolve(wizard.ResolveSeed{
		IssueID:   "h-7",
		Title:     "stuck CSV importer",
		Role:      "implement",
		Spec:      "specs/csv.md",
		Reason:    "agent escalated: needs-spec-clarification",
		SpecSlice: "# CSV\nthe acceptance criteria are ambiguous\n",
	})

	if sess.ResolveIssue() != "h-7" {
		t.Fatalf("ResolveIssue = %q, want h-7 (the issue the consent gate commits against)", sess.ResolveIssue())
	}

	// The opening turn is auto-run and currently blocked in the adapter; subscribe, then release
	// it and wait for the terminal `turn` nudge.
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()
	close(fa.release)

	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-sub:
			if ev.Name == "turn" {
				done = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the opening turn to complete")
		}
	}

	// The grounding rides in the system prompt: the base persona plus the escalation + spec slice.
	if !strings.Contains(fa.gotSystem, "BASE PERSONA") {
		t.Errorf("system prompt dropped the base persona: %q", fa.gotSystem)
	}
	for _, want := range []string{"RESOLVE MODE", "h-7", "specs/csv.md", "the acceptance criteria are ambiguous", "needs-spec-clarification"} {
		if !strings.Contains(fa.gotSystem, want) {
			t.Errorf("system prompt missing grounding %q in:\n%s", want, fa.gotSystem)
		}
	}

	// The transcript is the human↔planner conversation only (the grounding is NOT a visible
	// message): the opening user turn plus the planner's reply.
	msgs := sess.Messages()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("transcript = %+v, want [user opener, assistant reply]", msgs)
	}
	if !strings.Contains(msgs[1].Text, "minimal refinement") {
		t.Errorf("assistant reply = %q, want the planner's opening analysis", msgs[1].Text)
	}
	// A blank Create session is unaffected: it carries no issue id.
	if p.New().ResolveIssue() != "" {
		t.Error("a Create session must carry no resolve issue id")
	}
}
