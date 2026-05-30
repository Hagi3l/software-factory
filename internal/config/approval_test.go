package config

import (
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"internal/orchestrator/**", "internal/orchestrator/results.go", true},
		{"internal/orchestrator/**", "internal/orchestrator/sub/deep.go", true}, // ** crosses separators
		{"internal/orchestrator/**", "internal/runner/run.go", false},
		{"internal/*/run.go", "internal/runner/run.go", true},
		{"internal/*/run.go", "internal/a/b/run.go", false}, // * stays within one segment
		{"config/**", "config/harness.yaml", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false}, // * does not cross the separator
		{"go.mod", "go.mod", true},
		{"go.mod", "go.sum", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestPolicyApprovalRequired(t *testing.T) {
	tcb := []string{"internal/orchestrator/**", "config/**"}
	cases := []struct {
		name    string
		policy  Policy
		changed []string
		want    bool
	}{
		{"trusted-dev always", Policy{Profile: ProfileTrustedDev}, []string{"docs/x.md"}, true},
		{"trusted-dev always, even empty diff", Policy{Profile: ProfileTrustedDev}, nil, true},
		{"autonomous non-tcb", Policy{Profile: ProfileAutonomous, TCBPaths: tcb}, []string{"docs/x.md", "internal/agent/a.go"}, false},
		{"autonomous tcb diff", Policy{Profile: ProfileAutonomous, TCBPaths: tcb}, []string{"docs/x.md", "internal/orchestrator/r.go"}, true},
		{"autonomous config tcb", Policy{Profile: ProfileAutonomous, TCBPaths: tcb}, []string{"config/harness.yaml"}, true},
		{"empty profile defaults autonomous, no tcb globs", Policy{}, []string{"internal/orchestrator/r.go"}, false},
		{"empty profile, tcb diff", Policy{TCBPaths: tcb}, []string{"internal/orchestrator/r.go"}, true},
	}
	for _, tc := range cases {
		if got := tc.policy.ApprovalRequired(tc.changed); got != tc.want {
			t.Errorf("%s: ApprovalRequired(%v) = %v, want %v", tc.name, tc.changed, got, tc.want)
		}
	}
}

// trustedDev mutates validConfig into a sound trusted-dev configuration: the human-approved
// gate on integrate plus the profile and TCB globs.
func trustedDev(c *Config) {
	c.Harness.Policy.Profile = ProfileTrustedDev
	c.Harness.Policy.TCBPaths = []string{"internal/orchestrator/**"}
	st := c.Harness.DAG["integrate"]
	st.Postcondition = []string{core.PostconditionHumanApproved}
	c.Harness.DAG["integrate"] = st
}

func TestValidateTrustedDevHappyPath(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	trustedDev(c)
	if err := c.Validate(); err != nil {
		t.Fatalf("a sound trusted-dev config must validate: %v", err)
	}
}

func TestValidateTrustedDevRequiresHumanApprovedOnIntegrate(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Policy.Profile = ProfileTrustedDev // but integrate carries no human-approved gate
	mustContain(t, problems(t, c), "trusted-dev requires human approval on every integrate")
}

func TestValidateHumanApprovedOnlyOnTrustedMerge(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	// Put the human-approved gate on an agent stage, where the orchestrator never evaluates it.
	st := c.Harness.DAG["qa"]
	st.Postcondition = append(st.Postcondition, core.PostconditionHumanApproved)
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), "human approval gates integrate only")
}

func TestValidateTrustedMergeRejectsCommandCheck(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	// A command check on the inline merge has no gate to run it.
	st := c.Harness.DAG["integrate"]
	st.Postcondition = []string{"tests-pass"}
	c.Harness.DAG["integrate"] = st
	mustContain(t, problems(t, c), "a trusted-merge stage runs no gate")
}

func TestValidateUnknownProfile(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Policy.Profile = "yolo"
	mustContain(t, problems(t, c), "policy.profile \"yolo\" is unknown")
}

func TestValidateEmptyTCBGlob(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Policy.TCBPaths = []string{""}
	mustContain(t, problems(t, c), "empty glob")
}
