package core

import "testing"

// IssueIDFromCandidateBranch is the inverse of CandidateBranch; the trusted side uses it to
// recover an issue id from a candidate ref alone (e.g. the gate's verification span, which
// runs in a separate trace from the producer). A round-trip must hold, and a ref that does
// not follow the convention must be rejected rather than yield a bogus id.
func TestIssueIDFromCandidateBranch(t *testing.T) {
	for _, id := range []string{"issue-1", "factory-42", "a/b-c"} {
		got, ok := IssueIDFromCandidateBranch(CandidateBranch(id))
		if !ok || got != id {
			t.Errorf("round-trip %q = (%q, %v), want (%q, true)", id, got, ok, id)
		}
	}
	for _, ref := range []string{"", "candidate/", "main", "refs/heads/candidate/x", "candidatex"} {
		if got, ok := IssueIDFromCandidateBranch(ref); ok {
			t.Errorf("IssueIDFromCandidateBranch(%q) = (%q, true), want (_, false)", ref, got)
		}
	}
}

// The ResultStatus wire values are a contract, not cosmetic: they are what an
// agent writes into its Result envelope and what the orchestrator routes on
// (done → gate, failed → on_failure, needs-spec-clarification → human re-entry).
// Renaming a constant's underlying value would silently break that routing, so we
// pin the exact strings asserted by specs/components/agent.md.
func TestResultStatusWireValues(t *testing.T) {
	want := map[ResultStatus]string{
		StatusDone:                   "done",
		StatusFailed:                 "failed",
		StatusNeedsSpecClarification: "needs-spec-clarification",
	}
	for status, wire := range want {
		if string(status) != wire {
			t.Errorf("ResultStatus wire value = %q, want %q", string(status), wire)
		}
	}
}
