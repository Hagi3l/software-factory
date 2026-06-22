package config

import (
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// TestCommandCheckCommands proves the self-check command set resolves the gate's command
// checks across the DAG while excluding, by construction, the two classes a producer
// self-check must not run: the reserved proofs (resolved to the acceptance-test command
// instead) and the mutation metric (graded on a score, not an exit code) and other reserved
// postconditions (human-approved).
func TestCommandCheckCommands(t *testing.T) {
	h := &Harness{
		Checks: map[string]string{
			core.CheckAcceptanceTests: "make test-unit",
			"gosec":                   "make gosec",
			"govulncheck":             "make govulncheck",
			"mutation":                "make mutation",
		},
		DAG: map[string]Stage{
			"implement": {Postcondition: []string{core.PostconditionRedGreen}},
			"qa": {Postcondition: []string{
				core.CheckAcceptanceTests, "gosec", "govulncheck", "mutation>=0.8",
			}},
			"integrate": {Postcondition: []string{core.PostconditionHumanApproved}},
		},
	}
	got := h.CommandCheckCommands()

	want := map[string]string{
		core.CheckAcceptanceTests: "make test-unit", // from qa's tests-pass AND implement's proof
		"gosec":                   "make gosec",
		"govulncheck":             "make govulncheck",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d checks %v, want %d %v", len(got), got, len(want), want)
	}
	for name, cmd := range want {
		if got[name] != cmd {
			t.Errorf("check %q = %q, want %q", name, got[name], cmd)
		}
	}
	// The metric measurement command and the human-approved gate must NOT be self-checked.
	if _, ok := got["mutation"]; ok {
		t.Error("mutation metric command leaked into the self-check set")
	}
	if _, ok := got[core.PostconditionHumanApproved]; ok {
		t.Error("human-approved postcondition leaked into the self-check set")
	}
}
