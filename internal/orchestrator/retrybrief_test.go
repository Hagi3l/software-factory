package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

func TestFailingFindings(t *testing.T) {
	report := gate.Report{Checks: []gate.CheckResult{
		{Name: "tests-pass", Passed: false, Findings: core.Findings{{Rule: "TestA", Message: "boom"}}},
		{Name: "gosec", Passed: true, Findings: core.Findings{{Rule: "G999", Message: "should be ignored"}}}, // passed → excluded
		{Name: "govulncheck", Passed: false, Findings: core.Findings{{Rule: "GO-1", Message: "vuln"}}},
		{Name: "mutation", Passed: false}, // failed but no findings (a metric) → contributes nothing
	}}
	got := failingFindings(report)
	if len(got) != 2 {
		t.Fatalf("failingFindings = %+v, want 2 (only failed checks with findings)", got)
	}
	if got[0].Rule != "TestA" || got[1].Rule != "GO-1" {
		t.Errorf("findings = %+v, want [TestA, GO-1] in gate order", got)
	}
}

func TestBodyWithGateFeedback(t *testing.T) {
	findings := core.Findings{{File: "calc.go", Line: 12, Rule: "TestAdd", Message: "want 5, got 4"}}

	t.Run("appends a delimited section", func(t *testing.T) {
		got := bodyWithGateFeedback("the original task", findings)
		if !strings.HasPrefix(got, "the original task") {
			t.Errorf("original body not preserved: %q", got)
		}
		for _, want := range []string{"A previous attempt failed the gate", "calc.go:12", "TestAdd", "want 5, got 4", gateFeedbackMarker} {
			if !strings.Contains(got, want) {
				t.Errorf("body missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("no findings leaves the body unchanged", func(t *testing.T) {
		if got := bodyWithGateFeedback("just the task", nil); got != "just the task" {
			t.Errorf("body = %q, want it unchanged with no findings", got)
		}
	})

	t.Run("idempotent across retries — only the latest findings survive", func(t *testing.T) {
		// Attempt 2's body already carries attempt-1 findings; building attempt 3 must strip the
		// stale section and carry only attempt-2's, never a growing pile.
		stale := core.Findings{{File: "old.go", Line: 1, Rule: "OldTest", Message: "old failure"}}
		attempt2Body := bodyWithGateFeedback("the original task", stale)
		attempt3Body := bodyWithGateFeedback(attempt2Body, findings)

		if strings.Contains(attempt3Body, "OldTest") || strings.Contains(attempt3Body, "old.go") {
			t.Errorf("stale findings leaked across retries:\n%s", attempt3Body)
		}
		if !strings.Contains(attempt3Body, "TestAdd") {
			t.Errorf("latest findings missing:\n%s", attempt3Body)
		}
		if !strings.HasPrefix(attempt3Body, "the original task") {
			t.Errorf("original body not preserved:\n%s", attempt3Body)
		}
		// Exactly one feedback section (not a stacked pile).
		if n := strings.Count(attempt3Body, gateFeedbackMarker); n != 1 {
			t.Errorf("found %d feedback sections, want exactly 1:\n%s", n, attempt3Body)
		}
	})
}

// TestHandleResultGateFailThreadsFindingsIntoFixBody is the end-to-end wire-up: a gate
// rejection routes a fix issue whose body carries the failed checks' findings, so the retry
// attempt sees exactly what to fix instead of re-deriving blind (T9.8). Mirrors
// TestHandleResultGateFailRetries, asserting the body payload.
func TestHandleResultGateFailThreadsFindingsIntoFixBody(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	report := gate.Report{Passed: false, Checks: []gate.CheckResult{
		{Name: "tests-pass", Passed: false, Findings: core.Findings{
			{File: "calc.go", Line: 12, Rule: "TestAdd", Message: "want 5, got 4"},
		}},
	}}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{report: report}, &fakeMerger{})

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	body := applied[0].Issue.Body
	for _, want := range []string{"calc.go:12", "TestAdd", "want 5, got 4", "A previous attempt failed the gate"} {
		if !strings.Contains(body, want) {
			t.Errorf("fix issue body missing %q:\n%s", want, body)
		}
	}
}
