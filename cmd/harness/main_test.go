package main

import (
	"reflect"
	"testing"
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
