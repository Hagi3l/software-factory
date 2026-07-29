package orchestrator

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
	"github.com/Loxstomper/software-factory/internal/messaging"
)

// trustedDevConfig is kernelConfig under the trusted-dev profile, with the human-approved
// gate on integrate — the bootstrap's own shape, where every integrate is held for human
// approval (T2.10).
func trustedDevConfig() *config.Config {
	cfg := kernelConfig(2)
	cfg.Harness.Policy.Profile = config.ProfileTrustedDev
	st := cfg.Harness.DAG["integrate"]
	st.Postcondition = []string{core.PostconditionHumanApproved}
	cfg.Harness.DAG["integrate"] = st
	return cfg
}

// autonomousTCBConfig is kernelConfig under the autonomous profile with a TCB glob, so
// approval is required only for a TCB-touching diff. The integrate stage declares no
// human-approved gate — under autonomous the diff drives the decision, not the postcondition.
func autonomousTCBConfig() *config.Config {
	cfg := kernelConfig(2)
	cfg.Harness.Policy.Profile = config.ProfileAutonomous
	cfg.Harness.Policy.TCBPaths = []string{"internal/orchestrator/**"}
	return cfg
}

// doneResult is a passing implement candidate for iss-1 carrying a prompt sha, so a parked
// provenance has something to preserve.
func doneResult() core.Result {
	return core.Result{
		IssueID:  "iss-1",
		Status:   core.StatusDone,
		Branch:   core.Branch{Ref: core.CandidateBranch("iss-1")},
		Evidence: core.Evidence{PromptSHA: "sha256:abc"},
	}
}

// TestIntegrateUnderTrustedDevParks pins the core trusted-dev behavior: a verified candidate
// at integrate is PARKED awaiting human approval, not merged. It burns no retry (no route, no
// dead-letter), records the candidate ref + provenance on the issue, publishes an escalation,
// and does not close the issue — the parked issue is the human's action surface.
func TestIntegrateUnderTrustedDevParks(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{}
	g := &fakeGate{report: gate.Report{Passed: true, Checks: []gate.CheckResult{{Name: "tests-pass", Passed: true}}}}
	o, nc := newOrch(t, trustedDevConfig(), bd, g, m)

	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	transient, err := o.handleResult(context.Background(), doneResult())
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil) — parking is a clean outcome", transient, err)
	}

	if got := m.merged(); len(got) != 0 {
		t.Fatalf("merged = %v, want none — a trusted-dev candidate must not merge before approval", got)
	}
	if len(bd.parked) != 1 || bd.parked[0] != "iss-1" {
		t.Fatalf("parked = %v, want [iss-1]", bd.parked)
	}
	_, _, closed, _, applied := bd.snap()
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none (a parked issue stays open for the human)", closed)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none (parking burns no retry, spawns no fix)", applied)
	}
	// The parked issue durably carries the candidate ref and its provenance.
	parked, _ := bd.Get(context.Background(), "iss-1")
	if parked.CandidateRef != core.CandidateBranch("iss-1") {
		t.Errorf("parked candidate ref = %q, want %q", parked.CandidateRef, core.CandidateBranch("iss-1"))
	}
	var prov core.Provenance
	if err := json.Unmarshal([]byte(parked.ParkedProvenance), &prov); err != nil {
		t.Fatalf("parked provenance is not valid JSON: %v", err)
	}
	if prov.Issue != "iss-1" || prov.PromptSHA != "sha256:abc" || !reflect.DeepEqual(prov.Verified, []string{"tests-pass"}) {
		t.Errorf("parked provenance = %+v, want issue iss-1, prompt sha, verified [tests-pass]", prov)
	}
	if _, derr := sub.NextMsg(time.Second); derr != nil {
		t.Errorf("no escalation published for the parked candidate: %v", derr)
	}
}

// TestApproveResumesMergeAndCloses: a human approving a parked candidate merges it (replaying
// the preserved provenance) and closes the issue, recording who approved.
func TestApproveResumesMergeAndCloses(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{}
	g := &fakeGate{report: gate.Report{Passed: true, Checks: []gate.CheckResult{{Name: "tests-pass", Passed: true}}}}
	o, _ := newOrch(t, trustedDevConfig(), bd, g, m)

	if _, err := o.handleResult(context.Background(), doneResult()); err != nil {
		t.Fatalf("park: %v", err)
	}

	req := core.ApprovalRequest{IssueID: "iss-1", CandidateSHA: core.CandidateBranch("iss-1"), Approved: true, Approver: "alice"}
	transient, err := o.handleApproval(context.Background(), req)
	if err != nil || transient {
		t.Fatalf("handleApproval = (%v,%v), want (false,nil)", transient, err)
	}

	if got := m.merged(); len(got) != 1 || got[0] != core.CandidateBranch("iss-1") {
		t.Fatalf("merged = %v, want [candidate/iss-1] after approval", got)
	}
	if provs := m.provenance(); len(provs) != 1 || provs[0].Issue != "iss-1" || provs[0].PromptSHA != "sha256:abc" {
		t.Errorf("merge provenance = %+v, want the preserved parked provenance", m.provenance())
	}
	_, _, closed, _, _ := bd.snap()
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1] after a successful approved merge", closed)
	}
	if bd.approvedBy["iss-1"] != "alice" {
		t.Errorf("approver recorded = %q, want alice", bd.approvedBy["iss-1"])
	}
}

// TestRejectRoutesFix: a human rejecting a parked candidate routes a fix attempt through the
// normal on_failure machinery (a new implement issue), closing the rejected one — "reject → fix".
func TestRejectRoutesFix(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{}
	g := &fakeGate{report: gate.Report{Passed: true}}
	o, _ := newOrch(t, trustedDevConfig(), bd, g, m)

	if _, err := o.handleResult(context.Background(), doneResult()); err != nil {
		t.Fatalf("park: %v", err)
	}

	req := core.ApprovalRequest{IssueID: "iss-1", CandidateSHA: core.CandidateBranch("iss-1"), Approved: false, Approver: "alice"}
	transient, err := o.handleApproval(context.Background(), req)
	if err != nil || transient {
		t.Fatalf("handleApproval = (%v,%v), want (false,nil)", transient, err)
	}

	if got := m.merged(); len(got) != 0 {
		t.Fatalf("merged = %v, want none — a rejected candidate must not land", got)
	}
	_, _, closed, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "implement" || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want one implement fix at attempt 1", applied)
	}
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1] (the rejected issue is closed, its fix carries on)", closed)
	}
}

// TestApprovalStaleCandidateIgnored: an approval naming a candidate that no longer matches the
// parked one is invalidated (the candidate changed), so nothing merges or closes.
func TestApprovalStaleCandidateIgnored(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{}
	o, _ := newOrch(t, trustedDevConfig(), bd, &fakeGate{report: gate.Report{Passed: true}}, m)
	if _, err := o.handleResult(context.Background(), doneResult()); err != nil {
		t.Fatalf("park: %v", err)
	}

	req := core.ApprovalRequest{IssueID: "iss-1", CandidateSHA: "candidate/stale", Approved: true, Approver: "alice"}
	if _, err := o.handleApproval(context.Background(), req); err != nil {
		t.Fatalf("handleApproval: %v", err)
	}
	if got := m.merged(); len(got) != 0 {
		t.Errorf("merged = %v, want none — a stale approval must be ignored", got)
	}
	_, _, closed, _, _ := bd.snap()
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none", closed)
	}
}

// TestApprovalForNonParkedIgnored: an approval for an issue that is not parked (no candidate
// ref) is a no-op — idempotency against a duplicate/stale decision.
func TestApprovalForNonParkedIgnored(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: statusInProgress}) // never parked
	m := &fakeMerger{}
	o, _ := newOrch(t, trustedDevConfig(), bd, &fakeGate{}, m)

	req := core.ApprovalRequest{IssueID: "iss-1", Approved: true, Approver: "alice"}
	if _, err := o.handleApproval(context.Background(), req); err != nil {
		t.Fatalf("handleApproval: %v", err)
	}
	if got := m.merged(); len(got) != 0 {
		t.Errorf("merged = %v, want none — only a parked issue can be approved", got)
	}
}

// TestAutonomousTCBDiffParks: under autonomous a TCB-touching diff requires approval (parks),
// while a non-TCB diff integrates without one.
func TestAutonomousTCBDiffParks(t *testing.T) {
	t.Run("tcb diff parks", func(t *testing.T) {
		bd := newFakeBeads()
		bd.put(inProgress("iss-1", "implement", 0))
		m := &fakeMerger{}
		o, _ := newOrch(t, autonomousTCBConfig(), bd, &fakeGate{report: gate.Report{Passed: true}}, m)
		o.diffFiles = func(context.Context, string, string, string) ([]string, error) {
			return []string{"internal/orchestrator/results.go"}, nil
		}
		if _, err := o.handleResult(context.Background(), doneResult()); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		if got := m.merged(); len(got) != 0 {
			t.Errorf("merged = %v, want none — a TCB diff requires approval", got)
		}
		if len(bd.parked) != 1 {
			t.Errorf("parked = %v, want [iss-1]", bd.parked)
		}
	})

	t.Run("non-tcb diff merges", func(t *testing.T) {
		bd := newFakeBeads()
		bd.put(inProgress("iss-1", "implement", 0))
		m := &fakeMerger{}
		o, _ := newOrch(t, autonomousTCBConfig(), bd, &fakeGate{report: gate.Report{Passed: true}}, m)
		o.diffFiles = func(context.Context, string, string, string) ([]string, error) {
			return []string{"docs/readme.md"}, nil
		}
		if _, err := o.handleResult(context.Background(), doneResult()); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		if got := m.merged(); len(got) != 1 {
			t.Errorf("merged = %v, want [candidate/iss-1] — a non-TCB diff needs no approval", got)
		}
		if len(bd.parked) != 0 {
			t.Errorf("parked = %v, want none", bd.parked)
		}
	})
}
