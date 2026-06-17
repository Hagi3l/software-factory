package orchestrator

import (
	"context"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// closedEpicIssue is a closed issue of feat-1 (the root folds in via EpicID == "" → its own id).
func closedEpicIssue(id, epicID string) core.Issue {
	return core.Issue{ID: id, Title: "Feature", Role: "implement", Status: statusClosed, EpicID: epicID}
}

// TestSweepEpicCompletionLandsDrainedEpic is the core T7.4 assertion: under epic mode, an epic
// whose subtree has fully drained (root + every child closed, nothing in flight) is landed on main
// by a single terminal merge of its epic branch, carrying whole-feature provenance (the epic id as
// the durable reference, the root's title as the subject).
func TestSweepEpicCompletionLandsDrainedEpic(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "feat-1", Title: "Add sharing", Role: "plan", Status: statusClosed}) // root (its own epic)
	bd.put(closedEpicIssue("iss-2", "feat-1"))
	bd.put(closedEpicIssue("iss-3", "feat-1"))
	m := &fakeMerger{}
	o, _ := newOrch(t, epicCfg(2), bd, &fakeGate{}, m)

	o.sweepEpicCompletion(context.Background())

	calls := m.epicMerges()
	if len(calls) != 1 {
		t.Fatalf("MergeEpic called %d times, want 1 (one drained epic)", len(calls))
	}
	got := calls[0]
	if got.epicRef != "refs/heads/epic/feat-1" {
		t.Errorf("terminal merge epic ref = %q, want refs/heads/epic/feat-1", got.epicRef)
	}
	if got.target != "refs/heads/main" {
		t.Errorf("terminal merge target = %q, want refs/heads/main (main advances once)", got.target)
	}
	if got.prov.Issue != "feat-1" {
		t.Errorf("whole-feature provenance Issue = %q, want feat-1 (the epic id)", got.prov.Issue)
	}
	if got.prov.Subject != "Add sharing" {
		t.Errorf("terminal merge subject = %q, want the epic root's title", got.prov.Subject)
	}
}

// TestSweepEpicCompletionNoopInPerItemMode proves the sweep is epic-mode-only: per-item runs never
// hold an epic branch back, so there is nothing to terminally merge and the sweep must not fire.
func TestSweepEpicCompletionNoopInPerItemMode(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "feat-1", Title: "Add sharing", Role: "plan", Status: statusClosed})
	bd.put(closedEpicIssue("iss-2", "feat-1"))
	m := &fakeMerger{}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, m) // per-item config

	o.sweepEpicCompletion(context.Background())

	if calls := m.epicMerges(); len(calls) != 0 {
		t.Fatalf("MergeEpic called %d times in per-item mode, want 0", len(calls))
	}
}

// TestSweepEpicCompletionAllOrNothing proves the all-or-nothing rule: a single dead-lettered
// (blocked) or still-open child means the subtree never drains clean, so the terminal merge never
// fires and main stays untouched (the epic branch is abandoned).
func TestSweepEpicCompletionAllOrNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"a blocked child abandons the epic", "blocked"},
		{"a still-open child holds the epic", "open"},
		{"an in-progress child holds the epic", statusInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := newFakeBeads()
			bd.put(core.Issue{ID: "feat-1", Title: "Add sharing", Role: "plan", Status: statusClosed})
			bd.put(closedEpicIssue("iss-2", "feat-1"))
			incomplete := closedEpicIssue("iss-3", "feat-1")
			incomplete.Status = tc.status
			bd.put(incomplete)
			m := &fakeMerger{}
			o, _ := newOrch(t, epicCfg(2), bd, &fakeGate{}, m)

			o.sweepEpicCompletion(context.Background())

			if calls := m.epicMerges(); len(calls) != 0 {
				t.Fatalf("MergeEpic called %d times with a non-closed child, want 0 (all-or-nothing)", len(calls))
			}
		})
	}
}

// TestSweepEpicCompletionWaitsForInflight proves the in-flight clause: even when every issue
// ListAll returns is closed, an epic with a member still in the in-flight projection is not
// declared drained — that running invocation may still request a subtask, growing the subtree. The
// projection is the read-your-writes-consistent "nothing in flight" signal that closes the window
// where a just-spawned child is not yet visible in ListAll.
func TestSweepEpicCompletionWaitsForInflight(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "feat-1", Title: "Add sharing", Role: "plan", Status: statusClosed})
	bd.put(closedEpicIssue("iss-2", "feat-1"))
	m := &fakeMerger{}
	o, _ := newOrch(t, epicCfg(2), bd, &fakeGate{}, m)
	// A member is still in flight (not yet reflected as closed in any snapshot).
	o.inflight.add(core.Issue{ID: "iss-3", Role: "implement", EpicID: "feat-1"}, testLease())

	o.sweepEpicCompletion(context.Background())

	if calls := m.epicMerges(); len(calls) != 0 {
		t.Fatalf("MergeEpic called %d times with an in-flight member, want 0", len(calls))
	}
}

// TestSweepEpicCompletionWaitsForRoot proves the sweep waits when the epic root is not yet visible
// in the ListAll snapshot (eventual consistency): descendants are closed but the root — which
// supplies the merge subject and confirms the whole subtree is present — is absent, so no merge
// fires until a later sweep sees it.
func TestSweepEpicCompletionWaitsForRoot(t *testing.T) {
	bd := newFakeBeads()
	// Only descendants present; the root issue feat-1 is missing from this snapshot.
	bd.put(closedEpicIssue("iss-2", "feat-1"))
	bd.put(closedEpicIssue("iss-3", "feat-1"))
	m := &fakeMerger{}
	o, _ := newOrch(t, epicCfg(2), bd, &fakeGate{}, m)

	o.sweepEpicCompletion(context.Background())

	if calls := m.epicMerges(); len(calls) != 0 {
		t.Fatalf("MergeEpic called %d times with no visible root, want 0", len(calls))
	}
}

// TestSweepEpicCompletionIdempotentNoop proves the steady state of a landed epic: the root stays
// closed, so every later sweep re-detects drain and calls MergeEpic — which reports merged=false
// (its prior merge commit is already on main). The sweep must absorb that quietly without error;
// here the fake stands in for the already-landed case and the sweep completes cleanly.
func TestSweepEpicCompletionIdempotentNoop(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "feat-1", Title: "Add sharing", Role: "plan", Status: statusClosed})
	bd.put(closedEpicIssue("iss-2", "feat-1"))
	m := &fakeMerger{epicAlready: true}
	o, _ := newOrch(t, epicCfg(2), bd, &fakeGate{}, m)

	o.sweepEpicCompletion(context.Background()) // must not panic or error on a merged=false return

	if calls := m.epicMerges(); len(calls) != 1 {
		t.Fatalf("MergeEpic called %d times, want 1 (drain re-detected, no-op landing)", len(calls))
	}
}
