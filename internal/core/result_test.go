package core

import "testing"

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
