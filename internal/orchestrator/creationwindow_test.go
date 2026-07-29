package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
)

// windowBeads models bd.Apply's NON-ATOMIC two-phase write so a test can drive the creation window
// the real bd.Apply opens (T10.2, specs/components/orchestrator.md "The creation window"). Phase 1
// creates the children visible AND dispatchable — open, with no blocking edges yet — then Apply
// blocks on a barrier; Phase 2 (after the test closes `release`) adds the blocked-by edges. Ready
// computes readiness from the graph like bd.ready() (an issue is ready iff it is open and every
// dependency target is closed), so during the window the edge-less children read as ready, and once
// the edges exist a child blocked-by its still-open plan (or an unsatisfied sibling) is correctly
// excluded. Every other Beads method is inherited from the embedded *fakeBeads.
//
// This timing case is exactly what the fast in-memory fakeBeads.Apply never exercised — it created
// children and edges in one locked step, so the half-built window never existed in tests. Its
// absence is why the 2026-06-23 stall shipped; this fake reintroduces the window so the fix is
// guarded (T10.3).
type windowBeads struct {
	*fakeBeads
	entered chan struct{} // closed once Apply has created the edge-less children (window open)
	release chan struct{} // test closes this to let Apply add the edges (window closes)
}

func (w *windowBeads) Apply(_ context.Context, proposals []core.Proposal) ([]core.Issue, error) {
	w.mu.Lock()
	keyToID := map[string]string{}
	created := make([]core.Issue, 0, len(proposals))
	for _, p := range proposals {
		w.seq++
		is := p.Issue
		is.ID = fmt.Sprintf("new-%d", w.seq)
		is.Status = "open"
		// Phase 1: NO edges — the child exists open and blocker-free, so Ready returns it as
		// dispatchable. This is the half-built state the real bd.Apply briefly exposes.
		w.issues[is.ID] = is
		created = append(created, is)
		w.applied = append(w.applied, p)
		if p.Key != "" {
			keyToID[p.Key] = is.ID
		}
	}
	w.mu.Unlock()

	close(w.entered) // the window is open: edge-less children are now visible to Ready
	<-w.release      // hold the window until the test has driven a concurrent dispatch

	// Phase 2: add the blocked-by edges, resolving sibling keys to assigned IDs like the real Apply.
	w.mu.Lock()
	for i, p := range proposals {
		deps := make([]string, 0, len(p.DependsOn))
		for _, dep := range p.DependsOn {
			if id, ok := keyToID[dep]; ok {
				dep = id
			}
			deps = append(deps, dep)
		}
		is := w.issues[created[i].ID]
		is.DependsOn = deps
		w.issues[created[i].ID] = is
		created[i].DependsOn = deps
	}
	w.mu.Unlock()
	return created, nil
}

func (w *windowBeads) Ready(_ context.Context) ([]core.Issue, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []core.Issue
	for _, is := range w.issues {
		if is.Status != "open" {
			continue
		}
		blocked := false
		for _, dep := range is.DependsOn {
			if d, ok := w.issues[dep]; !ok || d.Status != "closed" {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, is)
		}
	}
	return out, nil
}

// TestCreationWindowConcurrentDecompositionRespectsOrdering is the T10.3 regression test for the
// creation-window race: it runs the real dispatch path (scheduleReady) concurrently with
// handleResult processing a planner decomposition, against a Beads fake whose Apply is two-phase and
// slow (children visible-and-ready after Phase 1, before Phase 2 adds the edges) — mirroring the
// real bd.Apply that wedged the 2026-06-23 vault-demo run. It asserts the two invariants of "The
// creation window": (1) the dispatch oracle never claims a half-built child (T10.2 — createMu
// serializes bd.Apply+track against bd.Ready), and the children that do dispatch go in dependency
// order (no all-at-once burst); (2) a claimed child stays live in the projection, so its harvested
// Result is not dropped as "not in flight" (T10.1 — track does not downgrade the live claim, the
// gate handleResult uses at results.go).
func TestCreationWindowConcurrentDecompositionRespectsOrdering(t *testing.T) {
	wb := &windowBeads{
		fakeBeads: newFakeBeads(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	wb.put(inProgress("plan-1", "planner", 0))
	o, _ := newOrch(t, planConfig(2), wb, &fakeGate{}, &fakeMerger{})

	// A two-child decomposition with an inter-sibling edge: B depends on A. Each child is also
	// blocked-by the plan (acceptPlan adds the parent link), so neither is ready until the plan closes.
	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone,
		Proposes: []core.Proposal{
			{Issue: core.Issue{Title: "slice A", Role: "test-author"}, Key: "a"},
			{Issue: core.Issue{Title: "slice B", Role: "test-author"}, DependsOn: []string{"a"}},
		},
	}

	// G1: process the decomposition. applyTracked holds createMu across bd.Apply (which blocks in its
	// window) and the projection track, so the dispatch oracle is locked out for the whole creation.
	done := make(chan error, 1)
	go func() { _, err := o.handleResult(context.Background(), res); done <- err }()

	<-wb.entered // Apply has created the edge-less children; the window is open and createMu is held.

	// G2: a real dispatch pass racing the open window. With the fix it parks on createMu until the
	// creation completes; WITHOUT it, bd.ready() returns the edge-less children and claims them both.
	dispatched := make(chan struct{})
	go func() { o.scheduleReady(context.Background()); close(dispatched) }()

	// Give G2 ample time to reach — and, on the buggy path, get past — the oracle read. With the fix
	// it stays parked however long we wait (so the passing run is never flaky); the wait only gives a
	// regression its chance to misbehave.
	select {
	case <-dispatched:
		t.Fatal("dispatch completed during the creation window — the oracle was not serialized against bd.Apply")
	case <-time.After(200 * time.Millisecond):
	}
	if claimed, _, _, _, _ := wb.snap(); len(claimed) != 0 {
		t.Fatalf("claimed %v during the creation window — a half-built child was dispatched", claimed)
	}

	close(wb.release) // close the window: Apply adds the edges, applyTracked tracks + releases createMu.
	if err := <-done; err != nil {
		t.Fatalf("handleResult(decomposition): %v", err)
	}
	<-dispatched // the parked dispatch now runs (post-creation); the children are blocked-by the plan.

	// One more pass now the plan is closed: child A's only blocker (plan-1) is gone, so it dispatches;
	// child B still waits on its open sibling A — dependency order honored, no all-at-once burst.
	o.scheduleReady(context.Background())

	claimed, _, closed, _, applied := wb.snap()
	if len(applied) != 2 {
		t.Fatalf("applied = %d proposals, want the 2-child decomposition exactly once", len(applied))
	}
	if !containsStr(closed, "plan-1") {
		t.Fatalf("plan-1 not closed; closed=%v", closed)
	}
	// new-1 is child A (created first); new-2 is child B (depends on A).
	if !containsStr(claimed, "new-1") {
		t.Errorf("child A (new-1) should dispatch once the plan closed; claimed=%v", claimed)
	}
	if containsStr(claimed, "new-2") {
		t.Errorf("child B (new-2) dispatched before its sibling A closed — dependency order violated; claimed=%v", claimed)
	}
	// (2)/T10.1: the claimed child is in_progress in the projection, so a Result for it would NOT be
	// dropped as "not in flight" (handleResult gates on inflight.has) — the stall does not recur.
	if !o.inflight.has("new-1") {
		t.Errorf("claimed child new-1 is not in_progress in the projection — its result would be dropped as stale")
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
