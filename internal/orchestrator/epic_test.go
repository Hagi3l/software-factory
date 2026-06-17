package orchestrator

import (
	"context"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// epicCfg returns the kernel config switched into epic mode.
func epicCfg(maxRetries int) *config.Config {
	c := kernelConfig(maxRetries)
	c.Harness.Integration = &config.Integration{Mode: config.IntegrationEpic}
	return c
}

// TestIntegrationTargetRetargetsPerMode pins the core T7.3 retargeting rule: per-item lands on
// refs/heads/main, epic on the issue's epic branch. The epic id is core.EpicOf — a descendant's
// threaded EpicID, or a root seed's own id — so every child of one feature shares one branch and
// the root folds into it. The short form (for the brief/merge-resolver) and the fully-qualified
// form (for the merge queue's update-ref plumbing) are siblings.
func TestIntegrationTargetRetargetsPerMode(t *testing.T) {
	child := core.Issue{ID: "iss-7", EpicID: "feat-1"} // a descendant carries the root's id
	root := core.Issue{ID: "feat-1"}                   // the root seed IS its own epic

	perItem := &Orchestrator{opts: Options{Config: kernelConfig(2)}, base: "main"}
	if got := perItem.integrationTargetRef(child); got != "refs/heads/main" {
		t.Errorf("per-item target = %q, want refs/heads/main", got)
	}
	if got := perItem.integrationBranchName(child); got != "main" {
		t.Errorf("per-item branch name = %q, want main", got)
	}
	if perItem.epicMode() {
		t.Error("kernel config must not read as epic mode")
	}

	epic := &Orchestrator{opts: Options{Config: epicCfg(2)}, base: "main"}
	if !epic.epicMode() {
		t.Fatal("epic config must read as epic mode")
	}
	for _, tc := range []struct {
		name              string
		issue             core.Issue
		wantRef, wantName string
	}{
		{"descendant uses the root's epic branch", child, "refs/heads/epic/feat-1", "epic/feat-1"},
		{"root seed folds into its own epic branch", root, "refs/heads/epic/feat-1", "epic/feat-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := epic.integrationTargetRef(tc.issue); got != tc.wantRef {
				t.Errorf("target ref = %q, want %q", got, tc.wantRef)
			}
			if got := epic.integrationBranchName(tc.issue); got != tc.wantName {
				t.Errorf("branch name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestBuildBriefSurfacesIntegrationBase asserts the brief carries the integration branch so the
// merge-resolver soul rebases a conflicting candidate onto the epic branch (where its colliding
// sibling lives), not main — the agent-phase half of retargeting the resolve stage's rebase.
func TestBuildBriefSurfacesIntegrationBase(t *testing.T) {
	issue := core.Issue{ID: "iss-7", EpicID: "feat-1", Base: "candidate/iss-6"}
	stage := config.Stage{Role: "implement", Postcondition: []string{"tests-pass"}}
	soul := core.Soul{Name: "implementor-go", Role: "implement"}

	perItem := &Orchestrator{opts: Options{Config: kernelConfig(2), Repo: t.TempDir()}, base: "main"}
	if got := perItem.buildBrief(issue, stage, soul).IntegrationBase; got != "main" {
		t.Errorf("per-item IntegrationBase = %q, want main", got)
	}

	epic := &Orchestrator{opts: Options{Config: epicCfg(2), Repo: t.TempDir()}, base: "main"}
	if got := epic.buildBrief(issue, stage, soul).IntegrationBase; got != "epic/feat-1" {
		t.Errorf("epic IntegrationBase = %q, want epic/feat-1", got)
	}
}

// TestMergeCandidateUsesEpicTarget proves the retargeting threads end-to-end through the
// orchestrator's integrate path: a verified candidate whose issue carries an epic_id is handed
// to the merger with the epic branch as its integration target, not refs/heads/main (T7.3). It
// mirrors the per-item merge-flow tests but flips the config into epic mode.
func TestMergeCandidateUsesEpicTarget(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: "open", EpicID: "feat-1"})
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, epicCfg(2), bd, g, m)
	o.inflight.add(core.Issue{ID: "iss-1", Role: "implement", EpicID: "feat-1"}, testLease())

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	if got := m.merged(); len(got) != 1 || got[0] != "candidate/iss-1" {
		t.Fatalf("merged = %v, want [candidate/iss-1]", got)
	}
	if got := m.mergeTargets(); len(got) != 1 || got[0] != "refs/heads/epic/feat-1" {
		t.Errorf("merge target = %v, want [refs/heads/epic/feat-1] (epic-mode retargeting)", got)
	}
}
