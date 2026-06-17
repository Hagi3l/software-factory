package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// testLease returns a far-future lease for seeding the in-flight projection directly in tests.
func testLease() time.Time { return time.Now().Add(time.Hour) }

// TestScheduleReadySkipsInflightCandidate proves the dispatch-storm fix (T3.12): bd.ready() is
// not read-your-writes consistent under load, so it can return an issue the orchestrator already
// claimed on a prior tick before the in_progress write propagates. The in-flight projection knows
// the claim happened, so scheduleReady must skip such a candidate rather than claim-and-dispatch
// it a second time. Without this skip the issue is re-dispatched every tick until the write becomes
// visible — the storm that multiplied agent spend and double-applied a decomposition in the demo run.
func TestScheduleReadySkipsInflightCandidate(t *testing.T) {
	bd := newFakeBeads()
	// A stale bd.ready() still lists iss-1, even though it was claimed on a prior tick.
	bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement"}}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	// Simulate the prior tick's claim: the issue is in the in-flight projection.
	o.inflight.add(core.Issue{ID: "iss-1", Role: "implement"}, testLease())

	o.scheduleReady(context.Background())

	if claimed, _, _, _, _ := bd.snap(); len(claimed) != 0 {
		t.Fatalf("claimed = %v, want none — an in-flight candidate must not be re-dispatched", claimed)
	}
}

// TestHandleResultAcceptsValidResultDespiteLaggingOpenStatus proves the result-gating half of
// T3.12: a valid Result must be processed when its issue is in the in-flight projection, even if
// the lagging beads read still shows the issue as `open` (the claim write not yet visible). The old
// code gated on `issue.Status != in_progress` and discarded such a result as "stale" — losing a
// real candidate. Here the issue reads `open` from beads but is in the projection, so the gate must
// run and the candidate must merge.
func TestHandleResultAcceptsValidResultDespiteLaggingOpenStatus(t *testing.T) {
	bd := newFakeBeads()
	// beads lags: the issue still reads `open` even though it was claimed and dispatched.
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: "open"})
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)
	// The single writer's own record: it claimed iss-1, so it is in flight regardless of the read.
	o.inflight.add(core.Issue{ID: "iss-1", Role: "implement"}, testLease())

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	if got := g.called(); len(got) != 1 {
		t.Fatalf("gate calls = %d, want 1 — a valid result was discarded despite a lagging status read", len(got))
	}
	if got := m.merged(); len(got) != 1 || got[0] != "candidate/iss-1" {
		t.Errorf("merged = %v, want [candidate/iss-1]", got)
	}
}

// TestHandleResultIgnoresResultNotInflight proves the duplicate/stale guard: a Result whose issue
// is NOT in the projection — a redelivery after the issue was already disposed, or a result the
// orchestrator never claimed — is ignored without acting on it. The terminal transition removes a
// settled issue from the projection, so a duplicate that arrives afterwards finds it absent.
func TestHandleResultIgnoresResultNotInflight(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0)) // in beads as in_progress...
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)
	// ...but the single writer no longer considers it in flight (already disposed / never claimed).
	o.inflight.remove("iss-1")

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	if len(g.called()) != 0 || len(m.merged()) != 0 {
		t.Error("a result for an issue not in flight was acted on")
	}
}

// TestHandleResultPlanDuplicateDoesNotDoubleChildren proves a redelivered plan Result does not
// apply the decomposition twice. The first Result fans the plan out and closes it (removing it from
// the projection); the redelivered Result then finds the plan absent from the projection and is
// ignored — so the children are created exactly once, not the doubled set the demo run produced.
func TestHandleResultPlanDuplicateDoesNotDoubleChildren(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0))
	o, _ := newOrch(t, planConfig(2), bd, &fakeGate{}, &fakeMerger{})

	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone,
		Proposes: []core.Proposal{
			{Issue: core.Issue{Title: "slice A", Role: "test-author"}, Key: "a"},
			{Issue: core.Issue{Title: "slice B", Role: "test-author"}, DependsOn: []string{"a"}},
		},
	}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("first handleResult: %v", err)
	}
	// Redelivery of the same plan Result (at-least-once): the plan is now closed and gone from the
	// projection, so this is ignored.
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("duplicate handleResult: %v", err)
	}

	_, _, closed, _, applied := bd.snap()
	if len(applied) != 2 {
		t.Fatalf("applied = %d proposals, want 2 (the decomposition must not be doubled)", len(applied))
	}
	if len(closed) != 1 || closed[0] != "plan-1" {
		t.Errorf("closed = %v, want [plan-1] exactly once", closed)
	}
}

// TestAcceptPlanIdempotentAcrossCloseFailure proves the belt-and-suspenders for the non-atomic
// Apply→Close window (T3.12): if Apply creates the children but the Close transition fails, the
// plan stays in_progress (and in the projection), so the redelivered Result re-enters acceptPlan.
// The parent-link children-existence check must then skip a second Apply and just finish closing —
// otherwise a transient Close failure would double the decomposition.
func TestAcceptPlanIdempotentAcrossCloseFailure(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0))
	o, _ := newOrch(t, planConfig(2), bd, &fakeGate{}, &fakeMerger{})

	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone,
		Proposes: []core.Proposal{
			{Issue: core.Issue{Title: "slice A", Role: "test-author"}, Key: "a"},
			{Issue: core.Issue{Title: "slice B", Role: "test-author"}, DependsOn: []string{"a"}},
		},
	}
	// First attempt: Apply succeeds (children created with the plan parent-link), Close fails.
	bd.closeErr = errors.New("beads close failed")
	transient, err := o.handleResult(context.Background(), res)
	if err == nil || !transient {
		t.Fatalf("handleResult = (%v,%v), want (true, err) on Close failure", transient, err)
	}
	if _, _, closed, _, applied := bd.snap(); len(applied) != 2 || len(closed) != 0 {
		t.Fatalf("after failed close: applied=%d closed=%v, want applied=2 closed=[]", len(applied), closed)
	}

	// Second attempt (redelivery): Close now succeeds. The children already exist (their DependsOn
	// carries plan-1), so acceptPlan must NOT re-apply — just close.
	bd.closeErr = nil
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("retry handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	_, _, closed, _, applied := bd.snap()
	if len(applied) != 2 {
		t.Fatalf("applied = %d, want 2 — the decomposition must not be re-applied", len(applied))
	}
	if len(closed) != 1 || closed[0] != "plan-1" {
		t.Errorf("closed = %v, want [plan-1] once the retry succeeds", closed)
	}
}

// TestRebuildInflightFromInProgressSet proves crash-safety: on restart the projection is rebuilt
// from beads' durable in_progress set before the first dispatch, so a restarted orchestrator
// resumes with an accurate live view (and does not re-dispatch genuinely in-flight work).
func TestRebuildInflightFromInProgressSet(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	bd.put(inProgress("iss-2", "implement", 0))
	bd.put(core.Issue{ID: "iss-3", Role: "implement", Status: "open"}) // not in flight
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	// newOrch already rebuilt; assert the in_progress set seeded the projection and `open` did not.
	if !o.inflight.has("iss-1") || !o.inflight.has("iss-2") {
		t.Error("rebuild did not seed the in-flight projection from the in_progress set")
	}
	if o.inflight.has("iss-3") {
		t.Error("rebuild seeded an open (not-in-flight) issue into the projection")
	}
	if got := o.inflight.size(); got != 2 {
		t.Errorf("projection size = %d, want 2", got)
	}

	// A restart re-derives the same set: rebuild again is idempotent (reset replaces contents).
	if err := o.rebuildInflight(context.Background()); err != nil {
		t.Fatalf("rebuildInflight: %v", err)
	}
	if o.inflight.size() != 2 {
		t.Errorf("projection size after re-rebuild = %d, want 2", o.inflight.size())
	}
}
