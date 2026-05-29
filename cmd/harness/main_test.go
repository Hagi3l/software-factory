package main

import (
	"reflect"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
)

// testConfigDir is the shipped bootstrap config, relative to this package dir.
const testConfigDir = "../../config"

// TestDispatchExitCodes pins the CLI's exit-code contract: 0 for success/help/
// version, 2 for a usage error (missing/unknown command), 1 for a command error
// (a bad config). The exit code is the only thing a shell or CI step sees, so it is
// part of the interface, not an incidental detail.
func TestDispatchExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown command", []string{"frobnicate"}, 2},
		{"help", []string{"help"}, 0},
		{"version", []string{"version"}, 0},
		{"validate ok", []string{"validate", "--config", testConfigDir}, 0},
		{"validate missing config", []string{"validate", "--config", "/does/not/exist"}, 1},
		{"validate unknown env", []string{"validate", "--config", testConfigDir, "--env", "nope"}, 1},
		{"seed without title", []string{"seed", "--config", testConfigDir}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatch(tc.args); got != tc.want {
				t.Fatalf("dispatch(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestValidateShippedConfig is the regression guard on the bootstrap config itself:
// the config the kernel ships with must pass the full startup gate. If a future edit
// breaks role↔soul resolution, a produces target, the model registry, or a persona
// path, this fails loudly here rather than at `harness run`.
func TestValidateShippedConfig(t *testing.T) {
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load shipped config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shipped config failed validation: %v", err)
	}
}

// TestShippedQAStageWired pins T2.9: the shipped DAG runs the spec-independent qa stage
// between implement and integrate, fulfilled by the `security` soul, with the mutation
// metric and the three scanners as its postconditions. It is the contract guard on the
// live wiring — a refactor that drops a check, re-points implement straight at integrate,
// or renames the qa role would silently weaken the gate, so catch it here rather than at
// `harness run`. The check *commands* (their tools) live in the role image (T5.3/T5.6);
// this test asserts only the routing and the registry, which are config.
func TestShippedQAStageWired(t *testing.T) {
	cfg := loadTestConfig(t)

	impl, ok := cfg.Harness.DAG["implement"]
	if !ok || !reflect.DeepEqual(impl.Produces, []string{"qa"}) {
		t.Fatalf("implement.produces = %v, want [qa] (implement must feed the qa stage, not integrate directly)", impl.Produces)
	}

	qa, ok := cfg.Harness.DAG["qa"]
	if !ok {
		t.Fatal("shipped DAG has no qa stage")
	}
	if qa.Role != "security" {
		t.Errorf("qa.role = %q, want %q", qa.Role, "security")
	}
	if qa.OnFailure != "implement" {
		t.Errorf("qa.on_failure = %q, want %q (a finding is fixed by a fresh implement attempt)", qa.OnFailure, "implement")
	}
	if !reflect.DeepEqual(qa.Produces, []string{"integrate"}) {
		t.Errorf("qa.produces = %v, want [integrate]", qa.Produces)
	}
	wantPost := []string{"tests-pass", "mutation>=0.8", "gosec", "govulncheck", "license-scan"}
	if !reflect.DeepEqual(qa.Postcondition, wantPost) {
		t.Errorf("qa.postcondition = %v, want %v", qa.Postcondition, wantPost)
	}

	// Every qa postcondition must resolve: the metric and the scanners need a registered
	// command (the reserved proofs reuse tests-pass). Validation enforces this, but pin
	// the four T2.9 commands explicitly so a dropped registry entry is named here.
	for _, name := range []string{"gosec", "govulncheck", "license-scan", "mutation"} {
		if cfg.Harness.Checks[name] == "" {
			t.Errorf("checks[%q] is empty; the qa gate cannot resolve it", name)
		}
	}
}

// TestShippedResolveStageWired pins T3.11: the shipped DAG carries a merge-conflict
// resolution stage (kind: resolve, role merge-resolver) that the orchestrator spawns on a
// rebase conflict. It must be gated (a postcondition re-verifying the resolved tree),
// produce integrate (loop back into the merge queue), and route on_failure for a bounded
// retry. A refactor that drops it would silently revert conflict handling to a dead-letter,
// so catch it here. The check *commands* live in the role image (T5.3/T5.6); this asserts
// only the routing and that the role resolves to a soul, which are config.
func TestShippedResolveStageWired(t *testing.T) {
	cfg := loadTestConfig(t)

	resolve, ok := cfg.Harness.DAG["resolve"]
	if !ok {
		t.Fatal("shipped DAG has no resolve stage")
	}
	if resolve.Kind != config.StageKindResolve {
		t.Errorf("resolve.kind = %q, want %q", resolve.Kind, config.StageKindResolve)
	}
	if resolve.Role != "merge-resolver" {
		t.Errorf("resolve.role = %q, want %q", resolve.Role, "merge-resolver")
	}
	if !reflect.DeepEqual(resolve.Produces, []string{"integrate"}) {
		t.Errorf("resolve.produces = %v, want [integrate] (a resolved candidate re-enters the merge queue)", resolve.Produces)
	}
	if resolve.OnFailure != "resolve" {
		t.Errorf("resolve.on_failure = %q, want %q (a failed resolution retries, bounded by the cap)", resolve.OnFailure, "resolve")
	}
	if len(resolve.Postcondition) == 0 {
		t.Error("resolve.postcondition is empty; the resolved tree must be re-verified (producer != verifier)")
	}
	// No stage may produce resolve — it is spawned by the orchestrator on a conflict, not
	// reached through a produces edge.
	for name, st := range cfg.Harness.DAG {
		for _, p := range st.Produces {
			if p == "resolve" {
				t.Errorf("stage %q produces resolve; the resolve stage is orchestrator-spawned, not a produces target", name)
			}
		}
	}

	// A merge-resolver soul must fulfill the role, or the resolve stage cannot be dispatched.
	found := false
	for _, s := range cfg.Souls {
		if s.Role == "merge-resolver" {
			found = true
			break
		}
	}
	if !found {
		t.Error("no soul fulfills the merge-resolver role")
	}
}
