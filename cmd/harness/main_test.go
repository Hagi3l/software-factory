package main

import "testing"

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
