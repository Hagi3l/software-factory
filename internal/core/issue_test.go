package core

import "testing"

// EpicOf is the single source of truth for epic grouping, shared by the orchestrator's
// aggregate epic budget and the control room's budget view. A root seed (no EpicID) must fold
// into its own epic via the ID fallback — so the aggregate naturally includes the root with no
// extra write — while a descendant reports its threaded EpicID.
func TestEpicOf(t *testing.T) {
	if got := EpicOf(Issue{ID: "root-1"}); got != "root-1" {
		t.Errorf("root seed EpicOf = %q, want its own id %q", got, "root-1")
	}
	if got := EpicOf(Issue{ID: "child-9", EpicID: "root-1"}); got != "root-1" {
		t.Errorf("descendant EpicOf = %q, want the threaded epic id %q", got, "root-1")
	}
}
