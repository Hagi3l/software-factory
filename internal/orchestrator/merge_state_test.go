package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
	"github.com/Loxstomper/software-factory/internal/messaging"
	"github.com/nats-io/nats.go"
)

// drainMergeStates collects every merge-state event the orchestrator published, in order, until
// the subscription goes quiet — and proves each event's subject id matches its body id (the
// invariant the T4.25 pump relies on to attribute a step without decoding the body twice).
func drainMergeStates(t *testing.T, sub *nats.Subscription) []core.MergeStateEvent {
	t.Helper()
	var evs []core.MergeStateEvent
	for {
		msg, err := sub.NextMsg(300 * time.Millisecond)
		if err != nil {
			break // no more events within the quiet window
		}
		var ev core.MergeStateEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			t.Fatalf("decode merge-state event: %v", err)
		}
		if got := messaging.IssueIDFromMergeSubject(msg.Subject); got != ev.ID {
			t.Errorf("merge-state subject id %q != body id %q", got, ev.ID)
		}
		evs = append(evs, ev)
	}
	return evs
}

// mergeStateSeq is the ordered list of states from a drained event slice.
func mergeStateSeq(evs []core.MergeStateEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.State
	}
	return out
}

func subscribeMergeStates(t *testing.T, nc *nats.Conn) *nats.Subscription {
	t.Helper()
	sub, err := nc.SubscribeSync(messaging.MergeStateWildcard)
	if err != nil {
		t.Fatalf("subscribe merge-state: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return sub
}

// TestMergeStateAnnouncesQueuedThenLanded proves the happy fast-forward path announces exactly
// queued → landed (no rebase/re-gate steps, since main did not move), and that the landed event
// carries the new main commit + the issue's role/epic — the payload the merge-queue view renders.
func TestMergeStateAnnouncesQueuedThenLanded(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})
	sub := subscribeMergeStates(t, nc)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	evs := drainMergeStates(t, sub)
	if got := mergeStateSeq(evs); len(got) != 2 || got[0] != core.MergeStateQueued || got[1] != core.MergeStateLanded {
		t.Fatalf("merge states = %v, want [queued landed]", got)
	}
	landed := evs[1]
	if landed.ID != "iss-1" || landed.Role != "implement" || landed.Epic != "iss-1" {
		t.Errorf("landed event = %+v, want id=iss-1 role=implement epic=iss-1", landed)
	}
	if landed.Commit != "deadbeef" { // the fakeMerger's landed commit
		t.Errorf("landed Commit = %q, want deadbeef (the new main tip)", landed.Commit)
	}
	if landed.TS.IsZero() {
		t.Error("landed event TS is zero")
	}
	// Only the landed event carries a commit; queued (and any mid-merge step) must not.
	if evs[0].Commit != "" {
		t.Errorf("queued event carried a commit %q, want empty", evs[0].Commit)
	}
}

// TestMergeStateAnnouncesRebaseAndRegate proves the rebase path announces the full train —
// queued → rebasing → re-gating → landed — when main moved under the candidate and the rebased
// result re-gates clean. The fakeMerger stands in for the rebase (regateRef set), emitting the
// rebasing/re-gating steps through the orchestrator's progress closure exactly as gitMerger does.
func TestMergeStateAnnouncesRebaseAndRegate(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{reportFn: func(c gate.Candidate) (gate.Report, error) {
		return gate.Report{Passed: true}, nil // branch gate and re-gate both pass
	}}
	o, nc := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{regateRef: "integration/iss-1"})
	sub := subscribeMergeStates(t, nc)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	want := []string{core.MergeStateQueued, core.MergeStateRebasing, core.MergeStateReGating, core.MergeStateLanded}
	if got := mergeStateSeq(drainMergeStates(t, sub)); !equalStrings(got, want) {
		t.Fatalf("merge states = %v, want %v", got, want)
	}
}

// TestMergeStateAnnouncesConflicted proves a rebase conflict announces queued → conflicted — the
// terminal failure that correlates with the dead-letter/resolution the same transition routes.
func TestMergeStateAnnouncesConflicted(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{err: errRebaseConflict}
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, m)
	sub := subscribeMergeStates(t, nc)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	want := []string{core.MergeStateQueued, core.MergeStateConflicted}
	if got := mergeStateSeq(drainMergeStates(t, sub)); !equalStrings(got, want) {
		t.Fatalf("merge states = %v, want %v", got, want)
	}
}

// TestMergeStateAnnouncesRegateFailed proves a clean rebase whose rebased result fails the
// re-gate announces queued → rebasing → re-gating → regate-failed (no landed) — the
// two-green-branches case the merge-queue view surfaces, routed to a fix rather than landed.
func TestMergeStateAnnouncesRegateFailed(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{reportFn: func(c gate.Candidate) (gate.Report, error) {
		if c.Ref == "integration/iss-1" {
			return gate.Report{Passed: false}, nil // the rebased combination is broken
		}
		return gate.Report{Passed: true}, nil // the branch alone is green
	}}
	o, nc := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{regateRef: "integration/iss-1"})
	sub := subscribeMergeStates(t, nc)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	want := []string{core.MergeStateQueued, core.MergeStateRebasing, core.MergeStateReGating, core.MergeStateRegateFailed}
	if got := mergeStateSeq(drainMergeStates(t, sub)); !equalStrings(got, want) {
		t.Fatalf("merge states = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
