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

// TestScheduleReadySkipsSettledCandidate proves the second half of the read-lag fix (T8.2):
// re-dispatching a just-*settled* candidate. After a terminal write, a lagging bd.ready() can still
// return an issue the orchestrator already closed or blocked (e.g. a plan closed at decomposition,
// or a dead-lettered issue) before that terminal write is visible. The old skip consulted only
// has() (in_progress), so a settled candidate slipped through and was dispatched a second time —
// a wasted invocation even when an idempotency guard later discards it. statusOf knows the issue is
// settled, so scheduleReady must skip it. An issue the projection reads back as `open` is the only
// genuinely dispatchable known candidate (re-derived ready, e.g. after a release).
func TestScheduleReadySkipsSettledCandidate(t *testing.T) {
	for _, st := range []string{statusClosed, statusBlocked} {
		t.Run(st, func(t *testing.T) {
			bd := newFakeBeads()
			// A stale bd.ready() still lists iss-1, even though it was settled on a prior tick.
			bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement"}}
			o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

			// The single writer's own record: it already settled iss-1 (closed/blocked).
			o.inflight.settle(core.Issue{ID: "iss-1", Role: "implement"}, st)

			o.scheduleReady(context.Background())

			if claimed, _, _, _, _ := bd.snap(); len(claimed) != 0 {
				t.Fatalf("claimed = %v, want none — a settled (%s) candidate must not be re-dispatched", claimed, st)
			}
		})
	}
}

// TestScheduleReadyDispatchesKnownOpenCandidate guards the inverse of the settled-skip: a candidate
// the projection knows about but reads back as `open` (re-derived ready, e.g. released after a failed
// publish, or hydrated open at cold start) is still genuinely dispatchable. The statusOf skip must
// fire only for in_progress/settled, never for open — otherwise a legitimately-ready known issue
// would wedge forever, never re-dispatched.
func TestScheduleReadyDispatchesKnownOpenCandidate(t *testing.T) {
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement"}}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	// Known to the projection but open (not in flight, not settled).
	o.inflight.settle(core.Issue{ID: "iss-1", Role: "implement"}, statusOpen)

	o.scheduleReady(context.Background())

	if claimed, _, _, _, _ := bd.snap(); len(claimed) != 1 || claimed[0] != "iss-1" {
		t.Fatalf("claimed = %v, want [iss-1] — a known-but-open candidate must still be dispatched", claimed)
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
	// ...but the single writer no longer considers it in flight: a terminal transition settled it
	// (retained in the projection under a closed status, not in_progress), so has() reports false.
	o.inflight.settle(core.Issue{ID: "iss-1", Role: "implement"}, statusClosed)

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

// TestInflightExpiredAndLeaseSeeding proves the in-memory lease sweep's source (T3.13). expired()
// returns entries whose lease has elapsed (and an anomalous zero-lease entry) while excluding a
// live lease — the in-memory replacement for the old beads stranded query. reset() seeds each
// entry from the issue's OWN durable lease (decoded from beads' lease_until), so a restart recovers
// pre-restart stranded work on its original deadline rather than resetting the clock to now+ttl.
func TestInflightExpiredAndLeaseSeeding(t *testing.T) {
	p := newInflightProjection()
	now := time.Now().UTC()
	p.add(core.Issue{ID: "live"}, now.Add(time.Hour))
	p.add(core.Issue{ID: "expired"}, now.Add(-time.Minute))
	p.add(core.Issue{ID: "nolease"}, time.Time{})

	got := map[string]bool{}
	for _, is := range p.expired(now) {
		got[is.ID] = true
	}
	if got["live"] {
		t.Error("a live lease was reported expired")
	}
	if !got["expired"] || !got["nolease"] {
		t.Errorf("expired set = %v, want expired and nolease (zero lease is strandable)", got)
	}

	// reset seeds each entry from issue.Lease (the durable deadline), NOT now+ttl: an issue whose
	// durable lease already passed is immediately strandable after a rebuild. Only in_progress
	// entries are sweepable, so the recovered work carries the in_progress status it had in beads.
	p.reset([]core.Issue{
		{ID: "stale", Status: statusInProgress, Lease: now.Add(-time.Hour)},
		{ID: "fresh", Status: statusInProgress, Lease: now.Add(time.Hour)},
	})
	exp := map[string]bool{}
	for _, is := range p.expired(now) {
		exp[is.ID] = true
	}
	if !exp["stale"] {
		t.Error("reset ignored the durable (already-expired) lease; pre-restart stranded work would wait a fresh leaseTTL")
	}
	if exp["fresh"] {
		t.Error("a future durable lease was reported expired after reset")
	}
}

// TestRebuildHydratesFullWorkGraph proves the T8.1 cold-start contract: on restart the work-graph
// projection is rebuilt from beads' FULL graph (every status, not just in_progress) before the first
// dispatch, so a restarted orchestrator resumes with an accurate live view of every issue it knows.
// In-flight work seeds has()/size() (crash-safe resume, no re-dispatch of genuinely in-flight work);
// settled work is RETAINED too (statusOf knows it) so the scheduler will not re-dispatch a
// just-closed issue a lagging bd.ready() still lists and the control room reads a consistent surface.
func TestRebuildHydratesFullWorkGraph(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	bd.put(inProgress("iss-2", "implement", 0))
	bd.put(core.Issue{ID: "iss-3", Role: "implement", Status: "open"})           // ready, not in flight
	bd.put(core.Issue{ID: "iss-4", Role: "implement", Status: statusClosed})     // settled (integrated/closed)
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	// newOrch already rebuilt; the in_progress set seeds the in-flight accessors, `open`/`closed` do not.
	if !o.inflight.has("iss-1") || !o.inflight.has("iss-2") {
		t.Error("rebuild did not seed the in-flight set from the in_progress issues")
	}
	if o.inflight.has("iss-3") || o.inflight.has("iss-4") {
		t.Error("has() reported a settled (open/closed) issue as in flight")
	}
	if got := o.inflight.size(); got != 2 {
		t.Errorf("in-flight size = %d, want 2", got)
	}

	// The generalization: settled issues are KNOWN to the projection with their real status, so the
	// whole graph (4 issues) is hydrated, not only the in_progress set.
	for id, want := range map[string]string{"iss-1": statusInProgress, "iss-2": statusInProgress, "iss-3": "open", "iss-4": statusClosed} {
		if got, ok := o.inflight.statusOf(id); !ok || got != want {
			t.Errorf("statusOf(%s) = (%q,%v), want (%q,true)", id, got, ok, want)
		}
	}
	if got := len(o.inflight.snapshot()); got != 4 {
		t.Errorf("snapshot size = %d, want 4 (the whole graph)", got)
	}

	// A restart re-derives the same set: rebuild again is idempotent (reset replaces contents).
	if err := o.rebuildInflight(context.Background()); err != nil {
		t.Fatalf("rebuildInflight: %v", err)
	}
	if o.inflight.size() != 2 || len(o.inflight.snapshot()) != 4 {
		t.Errorf("after re-rebuild: in-flight=%d snapshot=%d, want 2 and 4", o.inflight.size(), len(o.inflight.snapshot()))
	}
}

// TestProjectionRetainsSettledIssue proves the choke-point generalization: a transition AWAY from
// in_progress settles the issue in the projection (retained under its new status) rather than
// deleting it. has() then reports false (no longer in flight) but statusOf still knows it, and the
// snapshot carries the settled status. This is what closes the just-settled re-dispatch window (the
// scheduler can see a candidate is already closed) and lets the control room read settled state.
func TestProjectionRetainsSettledIssue(t *testing.T) {
	p := newInflightProjection()
	p.add(core.Issue{ID: "iss-1", Role: "implement"}, testLease())
	if !p.has("iss-1") || p.size() != 1 {
		t.Fatalf("after add: has=%v size=%d, want true and 1", p.has("iss-1"), p.size())
	}

	p.settle(core.Issue{ID: "iss-1", Role: "implement"}, statusClosed)
	if p.has("iss-1") {
		t.Error("a settled issue still reads as in flight via has()")
	}
	if p.size() != 0 {
		t.Errorf("in-flight size after settle = %d, want 0", p.size())
	}
	if got, ok := p.statusOf("iss-1"); !ok || got != statusClosed {
		t.Errorf("statusOf after settle = (%q,%v), want (closed,true) — settled issues are retained", got, ok)
	}
	snap := p.snapshot()
	if len(snap) != 1 || snap[0].ID != "iss-1" || snap[0].Status != statusClosed {
		t.Errorf("snapshot = %+v, want one iss-1 with status closed", snap)
	}

	// An unknown id is unknown — statusOf distinguishes "settled" from "never seen".
	if _, ok := p.statusOf("nope"); ok {
		t.Error("statusOf reported an unknown id as known")
	}
}

// TestProjectionTrackRecordsCreatedOpenIssue proves track() (T8.4) records a CREATED issue — one
// the orchestrator never transitioned — into the projection as open, with the board's time anchors
// stamped when beads supplied none (a bd.Apply-created issue carries no created_at/state_entered_at).
// Without this a freshly created child/seed is absent from the projection until first claimed, so
// the projection-backed board would not show it.
func TestProjectionTrackRecordsCreatedOpenIssue(t *testing.T) {
	p := newInflightProjection()
	now := time.Now().UTC()
	p.track(core.Issue{ID: "child-1", Role: "implement"}, now) // no Status, no timestamps

	if got, ok := p.statusOf("child-1"); !ok || got != statusOpen {
		t.Fatalf("statusOf = (%q,%v), want (open,true) — a tracked created issue is open", got, ok)
	}
	if p.has("child-1") || p.size() != 0 {
		t.Errorf("a tracked open issue must not count as in flight (has=%v size=%d)", p.has("child-1"), p.size())
	}
	snap := p.snapshot()
	if len(snap) != 1 || snap[0].ID != "child-1" || snap[0].Status != statusOpen {
		t.Fatalf("snapshot = %+v, want one open child-1", snap)
	}
	if snap[0].StateEnteredAt.IsZero() || snap[0].CreatedAt.IsZero() {
		t.Errorf("track must stamp the board's time anchors when beads supplied none (state=%v created=%v)",
			snap[0].StateEnteredAt, snap[0].CreatedAt)
	}
}

// TestProjectionTrackDoesNotDowngradeLiveClaim proves T10.1: a creation/reopen track() that races
// a dispatch claim must NOT clobber the live in_progress status back to open. bd.Apply is non-atomic,
// so a freshly created child can be claimed (add → in_progress) inside the creation window, before
// the creating loop's track() runs with the bd.Apply-fresh issue. If track() downgraded that claim,
// has() would read false forever, the returning Result would be dropped as stale, and bd.ready()
// (seeing in_progress) would never re-surface it — the permanent stall of the 2026-06-23 vault run.
func TestProjectionTrackDoesNotDowngradeLiveClaim(t *testing.T) {
	p := newInflightProjection()
	now := time.Now().UTC()
	lease := now.Add(time.Hour)

	// The dispatch loop claims the child first (creation window), then the creating loop tracks it.
	p.add(core.Issue{ID: "child-1", Role: "implement"}, lease)
	p.track(core.Issue{ID: "child-1", Role: "implement"}, now) // bd.Apply-fresh: no Status

	if got, ok := p.statusOf("child-1"); !ok || got != statusInProgress {
		t.Fatalf("statusOf = (%q,%v), want (in_progress,true) — track must not downgrade a live claim", got, ok)
	}
	if !p.has("child-1") || p.size() != 1 {
		t.Errorf("the live claim must survive track (has=%v size=%d)", p.has("child-1"), p.size())
	}
	// The claim's lease must be preserved too, or the lease sweep would mis-time the in-flight work.
	exp := p.expired(now)
	if len(exp) != 0 {
		t.Errorf("preserved claim's lease was lost: %d entries reported expired at claim time", len(exp))
	}
}

// TestProjectionTrackReopensBlockedIssue proves the guard added in T10.1 does NOT block the
// legitimate Resolve-wizard reopen: a blocked entry (a dead-letter) is reopened by track() to open,
// because the no-downgrade guard fires only on an in_progress entry, never a settled one.
func TestProjectionTrackReopensBlockedIssue(t *testing.T) {
	p := newInflightProjection()
	now := time.Now().UTC()
	p.settle(core.Issue{ID: "dl-1", Role: "implement"}, statusBlocked)
	p.track(core.Issue{ID: "dl-1", Role: "implement"}, now) // wizard reopen, blocked→open

	if got, ok := p.statusOf("dl-1"); !ok || got != statusOpen {
		t.Fatalf("statusOf = (%q,%v), want (open,true) — a blocked dead-letter must reopen", got, ok)
	}
}

// TestProjectionMarkIntegratedSurvivesSettle proves the durable integration marker (T8.4) is
// mirrored into the projection in the merge path (markIntegrated, while the bead is still
// in_progress) and CARRIED FORWARD by the close transition that settles it — so the board hero's
// projection-backed roll-up counts the integration. The issue passed to settle carries
// Integrated=false (accept never learned it landed), so without the monotonic preserve the marker
// would be lost.
func TestProjectionMarkIntegratedSurvivesSettle(t *testing.T) {
	p := newInflightProjection()
	p.add(core.Issue{ID: "iss-1", Role: "qa"}, testLease())
	p.markIntegrated("iss-1")
	p.settle(core.Issue{ID: "iss-1", Role: "qa"}, statusClosed) // Integrated unset on the incoming value

	snap := p.snapshot()
	if len(snap) != 1 || !snap[0].Integrated || snap[0].Status != statusClosed {
		t.Fatalf("snapshot = %+v, want one closed iss-1 with Integrated=true", snap)
	}
}

// TestApplyTrackedRecordsCreatedIssues proves the orchestrator's applyTracked (T8.4) records every
// issue it creates through bd.Apply into the work-graph projection, so the control room's
// projection-backed board sees a freshly created child the instant it exists.
func TestApplyTrackedRecordsCreatedIssues(t *testing.T) {
	bd := newFakeBeads()
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	created, err := o.applyTracked(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "child", Role: "implement"}},
	})
	if err != nil || len(created) != 1 {
		t.Fatalf("applyTracked = (%v, %v), want one created issue and no error", created, err)
	}
	if got, ok := o.inflight.statusOf(created[0].ID); !ok || got != statusOpen {
		t.Fatalf("created issue not tracked open in the projection: (%q,%v)", got, ok)
	}
}

// TestOrchestratorTrackAndSnapshot proves the public Track/Snapshot surface (T8.4): Track records an
// externally-written issue (the wizard's seed/reopen path) into the projection, and Snapshot exposes
// it as the control room's live read model.
func TestOrchestratorTrackAndSnapshot(t *testing.T) {
	bd := newFakeBeads()
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	o.Track(core.Issue{ID: "seed-1", Role: "plan", Status: statusOpen})
	snap, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var found bool
	for _, is := range snap {
		if is.ID == "seed-1" && is.Status == statusOpen {
			found = true
		}
	}
	if !found {
		t.Fatalf("Snapshot = %+v, want it to include the tracked open seed-1", snap)
	}
}

// TestTransitionStampsStateEnteredIntoProjection proves transition() mirrors state_entered_at onto
// the projection's cached snapshot (T8.4), so the projection-backed board's per-card timer anchors
// on the right instant rather than the zero time.
func TestTransitionStampsStateEnteredIntoProjection(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: "open"})
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	err := o.transition(context.Background(), core.Issue{ID: "iss-1", Role: "implement"}, statusClosed,
		func(ctx context.Context) error { return bd.Close(ctx, "iss-1") })
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	snap := o.inflight.snapshot()
	if len(snap) != 1 || snap[0].ID != "iss-1" || snap[0].Status != statusClosed {
		t.Fatalf("snapshot = %+v, want one closed iss-1", snap)
	}
	if snap[0].StateEnteredAt.IsZero() {
		t.Error("transition did not stamp state_entered_at into the projection snapshot")
	}
}
