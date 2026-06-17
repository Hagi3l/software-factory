package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/spec"
)

// orchWithPlanDAG builds an orchestrator over repo whose DAG has a single plan stage (the
// re-entry point the merged-delta sweep spawns into), so recompileMergedDelta has somewhere to
// re-derive to.
func orchWithPlanDAG(repo string, depth int) *Orchestrator {
	o := orchWithRepo(repo, depth)
	o.opts.Config.Harness.DAG = map[string]config.Stage{
		"plan": {Role: "planner", Kind: config.StageKindPlan, Produces: []string{"author-tests"}},
	}
	return o
}

// TestRecompileSpecDeltaReissuesDriftedWork proves the heart of "recompile the delta" (T3.7):
// an in-flight issue pinned to the spec version it was briefed against is left alone while the
// spec is unchanged, but is reissued once a human edits that spec so the re-resolved slice no
// longer matches the pin. This is the structural drift detector — the agent never sees the
// edit, so realignment must be the orchestrator re-dispatching the work (see
// specs/specs-process.md). The sweep uses the real spec resolver against an on-disk fixture so
// the hash comparison is genuinely exercised, not faked.
func TestRecompileSpecDeltaReissuesDriftedWork(t *testing.T) {
	repo := t.TempDir()
	specPath := filepath.Join(repo, "specs", "orders.md")
	mustWriteSpec(t, specPath, "# Orders\nreject negatives\n")

	// Pin the issue to the original slice version, as dispatch (scheduleReady) would have.
	orig, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve original slice: %v", err)
	}
	pinned := spec.Hash(orig)

	o := orchWithRepo(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	// Seed the in-flight projection as scheduleReady would have at dispatch (pinned to the
	// original slice version): the sweep now iterates the projection, not a beads read (T3.13).
	o.inflight.add(core.Issue{ID: "iss-1", Role: "implement", Status: statusInProgress, Spec: "specs/orders.md", SpecHash: pinned}, testLease())

	// No edit yet: the re-resolved hash matches the pin, so nothing is reissued.
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 0 {
		t.Fatalf("reissued %v with no spec change; want none", bd.reissued)
	}

	// A human refines the spec: the slice changes, so the pinned hash is now stale.
	mustWriteSpec(t, specPath, "# Orders\nreject negatives AND zero\n")
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 1 || bd.reissued[0] != "iss-1" {
		t.Fatalf("reissued = %v, want [iss-1] after the spec edit", bd.reissued)
	}
}

// TestScheduleReadyRecordsPinnedSpecHashForDriftSweep proves the wiring T3.13 depends on: the
// in-memory spec-drift sweep reads the pinned spec hash from the in-flight projection, and
// scheduleReady records that hash into the projection (updateIssue) AFTER it pins — because the
// claim transition recorded the issue before the pin existed. Without that record the sweep would
// diff against an empty hash and never fire. Drives the real path end to end: dispatch a ready
// issue against an on-disk spec, then edit the spec and assert the drift sweep reissues it.
func TestScheduleReadyRecordsPinnedSpecHashForDriftSweep(t *testing.T) {
	repo := t.TempDir()
	specPath := filepath.Join(repo, "specs", "orders.md")
	mustWriteSpec(t, specPath, "# Orders\nreject negatives\n")

	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement", Spec: "specs/orders.md"}}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	o.opts.Repo = repo
	o.opts.Config.Harness.SpecDepth = 1

	o.scheduleReady(context.Background())

	// The projection snapshot must carry the pinned (non-empty) hash, not the empty hash the
	// pre-pin claim transition recorded.
	inflight := o.inflight.issues()
	if len(inflight) != 1 || inflight[0].ID != "iss-1" {
		t.Fatalf("projection = %v, want [iss-1] in flight after dispatch", inflight)
	}
	if inflight[0].SpecHash == "" {
		t.Fatal("projection snapshot carries no pinned spec hash; the drift sweep would never fire")
	}

	// No edit: the sweep, reading the projection's pin, leaves the work alone.
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 0 {
		t.Fatalf("reissued %v with no spec change; want none", bd.reissued)
	}

	// A human edits the spec: the drift sweep reissues, proving the recorded pin is live.
	mustWriteSpec(t, specPath, "# Orders\nreject negatives AND zero\n")
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 1 || bd.reissued[0] != "iss-1" {
		t.Fatalf("reissued = %v, want [iss-1] after the spec edit", bd.reissued)
	}
}

// TestRecompileSpecDeltaSkipsWithoutDriftSignal proves the sweep's best-effort discipline: it
// touches in-flight work only on a clear drift signal. An issue with no spec reference, an
// issue dispatched but not yet pinned (degraded, not drifted), and an issue whose spec no
// longer resolves (mid-edit/deleted — an ambiguous signal) are all left in_progress rather
// than disrupted, mirroring buildBrief's degradation discipline.
func TestRecompileSpecDeltaSkipsWithoutDriftSignal(t *testing.T) {
	repo := t.TempDir()
	mustWriteSpec(t, filepath.Join(repo, "specs", "orders.md"), "# Orders\nreject negatives\n")

	o := orchWithRepo(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	// The sweep iterates the in-flight projection (T3.13), so seed it directly.
	o.inflight.add(core.Issue{ID: "no-spec", Role: "implement", Status: statusInProgress}, testLease())                                                  // no spec ref -> nothing to diff
	o.inflight.add(core.Issue{ID: "no-pin", Role: "implement", Status: statusInProgress, Spec: "specs/orders.md"}, testLease())                          // dispatched, not yet pinned -> skip
	o.inflight.add(core.Issue{ID: "gone", Role: "implement", Status: statusInProgress, Spec: "specs/gone.md", SpecHash: "sha256:deadbeef"}, testLease()) // unresolvable -> leave alone

	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 0 {
		t.Fatalf("reissued %v; none should be reissued (no spec / no pin / unresolvable)", bd.reissued)
	}
}

// TestRecompileMergedDeltaSpawnsRederivationPlan proves T3.7b: when a human edits a spec an
// epic's work has already merged against, the orchestrator spawns ONE fresh plan issue for that
// (epic, spec-path) — re-entry at planning so the planner decomposes only the delta against the
// merged code — and re-pins the closed issues so a subsequent sweep does not respawn. One edit is
// deduped across the epic's many closed issues that share the path (one plan, not one per issue).
func TestRecompileMergedDeltaSpawnsRederivationPlan(t *testing.T) {
	repo := t.TempDir()
	specPath := filepath.Join(repo, "specs", "orders.md")
	mustWriteSpec(t, specPath, "# Orders\nreject negatives\n")

	orig, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve original slice: %v", err)
	}
	pinned := spec.Hash(orig)

	o := orchWithPlanDAG(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	// A merged epic: its root plan issue plus two closed children, all sharing the epic id and
	// path, all pinned to the original slice version. (The root carries no EpicID — epicOf folds
	// it into the group by its own id.)
	bd.put(core.Issue{ID: "e1", Role: "planner", Status: statusClosed, Spec: "specs/orders.md", SpecHash: pinned, Tags: map[string]string{"lang": "go"}})
	bd.put(core.Issue{ID: "c1", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", SpecHash: pinned, EpicID: "e1", Tags: map[string]string{"lang": "go"}})
	bd.put(core.Issue{ID: "c2", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", SpecHash: pinned, EpicID: "e1", Tags: map[string]string{"lang": "go"}})

	// No edit yet: the slice still hashes to the pin, so nothing is spawned.
	o.recompileMergedDelta(context.Background())
	if _, _, _, _, applied := bd.snap(); len(applied) != 0 {
		t.Fatalf("spawned %d plan issues with no spec change; want none", len(applied))
	}

	// A human refines the spec: the merged work now trails the contract.
	mustWriteSpec(t, specPath, "# Orders\nreject negatives AND zero\n")
	edited, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve edited slice: %v", err)
	}
	current := spec.Hash(edited)

	o.recompileMergedDelta(context.Background())

	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("spawned %d plan issues; want exactly 1 (deduped across the epic)", len(applied))
	}
	p := applied[0].Issue
	if p.Role != "planner" {
		t.Errorf("re-derivation issue role = %q, want planner (re-entry at planning)", p.Role)
	}
	if p.EpicID != "e1" {
		t.Errorf("re-derivation issue EpicID = %q, want e1", p.EpicID)
	}
	if p.Spec != "specs/orders.md" {
		t.Errorf("re-derivation issue Spec = %q, want specs/orders.md", p.Spec)
	}
	if p.Base != "" {
		t.Errorf("re-derivation issue Base = %q, want empty (branch from the epic's merged tip = main)", p.Base)
	}
	if p.Tags["lang"] != "go" {
		t.Errorf("re-derivation issue Tags = %v, want lang=go threaded from the epic", p.Tags)
	}
	// The latch: every closed member re-pinned to the new slice.
	for _, id := range []string{"e1", "c1", "c2"} {
		if bd.pinned[id] != current {
			t.Errorf("issue %s re-pinned to %q, want current %q", id, bd.pinned[id], current)
		}
	}
}

// TestRecompileMergedDeltaSkipsWhenPlanAlreadyOpen proves the first idempotency mechanism: a
// drifted (epic, path) whose planning pass is already in flight (a prior re-derivation, or the
// epic's initial plan) does not get a second plan piled on, and the pins are left until that pass
// settles.
func TestRecompileMergedDeltaSkipsWhenPlanAlreadyOpen(t *testing.T) {
	repo := t.TempDir()
	specPath := filepath.Join(repo, "specs", "orders.md")
	mustWriteSpec(t, specPath, "# Orders\nreject negatives\n")
	orig, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve original slice: %v", err)
	}
	pinned := spec.Hash(orig)

	o := orchWithPlanDAG(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	bd.put(core.Issue{ID: "c1", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", SpecHash: pinned, EpicID: "e1"})
	// A re-derivation plan for this (epic, path) is already open.
	bd.put(core.Issue{ID: "p1", Role: "planner", Status: "open", Spec: "specs/orders.md", EpicID: "e1"})

	mustWriteSpec(t, specPath, "# Orders\nreject negatives AND zero\n")
	o.recompileMergedDelta(context.Background())

	if _, _, _, _, applied := bd.snap(); len(applied) != 0 {
		t.Fatalf("spawned %d plan issues while one is already open; want none", len(applied))
	}
	if _, ok := bd.pinned["c1"]; ok {
		t.Errorf("re-pinned c1 while a plan is already open; pins should wait until it settles")
	}
}

// TestRecompileMergedDeltaSkipsWithoutDriftSignal proves the merged sweep's best-effort
// discipline mirrors the in-flight one: no plan stage to re-derive into, a group with no pin, an
// unchanged slice, and an unresolvable slice all leave merged work untouched.
func TestRecompileMergedDeltaSkipsWithoutDriftSignal(t *testing.T) {
	repo := t.TempDir()
	mustWriteSpec(t, filepath.Join(repo, "specs", "orders.md"), "# Orders\nreject negatives\n")
	orig, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve slice: %v", err)
	}
	pinned := spec.Hash(orig)

	// No plan stage configured: nowhere to re-derive into, so the sweep no-ops even on drift.
	noPlan := orchWithRepo(repo, 1)
	npbd := newFakeBeads()
	noPlan.bd = npbd
	npbd.put(core.Issue{ID: "c1", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", SpecHash: "sha256:stale", EpicID: "e1"})
	noPlan.recompileMergedDelta(context.Background())
	if _, _, _, _, applied := npbd.snap(); len(applied) != 0 {
		t.Fatalf("spawned %d plans with no plan stage; want none", len(applied))
	}

	o := orchWithPlanDAG(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	bd.put(core.Issue{ID: "no-pin", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", EpicID: "e1"})    // closed but never pinned -> no version to diff
	bd.put(core.Issue{ID: "settled", Role: "implement", Status: statusClosed, Spec: "specs/orders.md", SpecHash: pinned, EpicID: "e2"}) // matches current -> no drift
	bd.put(core.Issue{ID: "gone", Role: "implement", Status: statusClosed, Spec: "specs/gone.md", SpecHash: "sha256:x", EpicID: "e3"})  // unresolvable -> leave alone

	o.recompileMergedDelta(context.Background())
	if _, _, _, _, applied := bd.snap(); len(applied) != 0 {
		t.Fatalf("spawned %d plans; none should be (no pin / no drift / unresolvable)", len(applied))
	}
}
