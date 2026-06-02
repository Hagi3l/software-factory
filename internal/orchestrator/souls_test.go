package orchestrator

import (
	"context"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// redGreenPlanConfig is planConfig with the implement stage carrying the red→green proof (the
// principled "this is the implement stage" signal stampProducingSoul keys off), so a test can
// drive both producing-soul stamps through the author-tests → implement → integrate spine.
func redGreenPlanConfig(maxRetries int) *config.Config {
	cfg := planConfig(maxRetries)
	cfg.Harness.DAG["implement"] = config.Stage{
		Role:          "implementor",
		Postcondition: []string{core.PostconditionRedGreen},
		OnFailure:     "implement",
		Produces:      []string{"integrate"},
	}
	return cfg
}

// TestAuthorTestsStampsTestsSoulAndThreads proves the orchestrator records the author-tests
// stage's producing soul onto the issue (keyed off its tests-red proof) and threads it forward
// onto the produced implement issue — the recording half of producer ≠ verifier (T4.22).
func TestAuthorTestsStampsTestsSoulAndThreads(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("at-1", "test-author", 0))
	g := &fakeGate{report: gate.Report{Passed: true}}
	o, _ := newOrch(t, redGreenPlanConfig(2), bd, g, &fakeMerger{})

	res := core.Result{IssueID: "at-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("at-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	// The author-tests issue itself carries the test author's soul.
	if got := bd.issues["at-1"].TestsSoul; got != "test-author-go" {
		t.Errorf("at-1 TestsSoul = %q, want test-author-go (stamped as the stage ran)", got)
	}
	// And the produced implement issue inherits it (threaded forward like TraceMap).
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "implementor" {
		t.Fatalf("applied = %+v, want one implementor child", applied)
	}
	if got := applied[0].Issue.TestsSoul; got != "test-author-go" {
		t.Errorf("produced implement TestsSoul = %q, want test-author-go (threaded forward)", got)
	}
	if applied[0].Issue.ImplementSoul != "" {
		t.Errorf("produced implement ImplementSoul = %q, want empty (implement has not run yet)", applied[0].Issue.ImplementSoul)
	}
}

// TestImplementStampsImplementSoulAndTrailer proves the implement stage's soul is stamped onto
// its issue (keyed off the red→green proof) and that at integrate the merge trailer cites both
// the implementor (Soul) and the threaded test author (Tests-Soul) — producer ≠ verifier made
// auditable from the commit (T4.22, specs/verification.md).
func TestImplementStampsImplementSoulAndTrailer(t *testing.T) {
	bd := newFakeBeads()
	// An implement issue that already carries the threaded author-tests soul (as advance would
	// have set it when author-tests produced this issue).
	impl := inProgress("impl-1", "implementor", 0)
	impl.TestsSoul = "test-author-go"
	bd.put(impl)
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, redGreenPlanConfig(2), bd, g, m)

	res := core.Result{IssueID: "impl-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("impl-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	if got := bd.issues["impl-1"].ImplementSoul; got != "implementor-go" {
		t.Errorf("impl-1 ImplementSoul = %q, want implementor-go (stamped as the stage ran)", got)
	}
	provs := m.provenance()
	if len(provs) != 1 {
		t.Fatalf("provenance recorded %d times, want 1", len(provs))
	}
	if provs[0].Soul != "implementor-go" {
		t.Errorf("trailer Soul = %q, want implementor-go", provs[0].Soul)
	}
	if provs[0].TestsSoul != "test-author-go" {
		t.Errorf("trailer Tests-Soul = %q, want test-author-go (the threaded test author)", provs[0].TestsSoul)
	}
}

// TestGateVerdictStampedOnAcceptedIssue proves the assembled gate-verdict hash the gate harvested
// is stamped onto the issue when the candidate is accepted — reachable for the verification view.
func TestGateVerdictStampedOnAcceptedIssue(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("impl-1", "implement", 0))
	g := &fakeGate{report: gate.Report{Passed: true, Verdict: core.ArtifactRef{Kind: core.ArtifactKindGateVerdict, Hash: "sha256:verdict"}}}
	o, _ := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{})

	res := core.Result{IssueID: "impl-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("impl-1")}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if got := bd.issues["impl-1"].GateVerdict; got != "sha256:verdict" {
		t.Errorf("GateVerdict = %q, want sha256:verdict (stamped from the gate report)", got)
	}
}

// TestGateVerdictStampedOnRejectedIssue proves a rejected candidate's verdict is stamped too —
// it is exactly what a human triages from the dead-letter queue (the verdict outlives the route).
func TestGateVerdictStampedOnRejectedIssue(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("impl-1", "implement", 0))
	g := &fakeGate{report: gate.Report{Passed: false, Verdict: core.ArtifactRef{Kind: core.ArtifactKindGateVerdict, Hash: "sha256:rejected"}}}
	o, _ := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{})

	res := core.Result{IssueID: "impl-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("impl-1")}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	// The original issue is closed and a fix spawned, but the verdict was stamped before routing.
	if got := bd.issues["impl-1"].GateVerdict; got != "sha256:rejected" {
		t.Errorf("GateVerdict = %q, want sha256:rejected (stamped despite rejection)", got)
	}
}
