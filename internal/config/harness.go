package config

import (
	"fmt"
	"os"
)

// Harness is harness.yaml: the workflow DAG plus the termination policy. It changes
// rarely and is environment-independent — the per-environment knobs live in the
// infra overlay (see Infra). See specs/configuration.md and specs/workflow.md.
type Harness struct {
	DAG    map[string]Stage `yaml:"dag"`
	Policy Policy           `yaml:"policy"`
}

// Stage kinds for non-agent stages. An agent stage names a Role instead; a stage is
// exactly one or the other (see Stage and validateDAG).
const (
	// StageKindHuman is the trusted, non-sandboxed requirements stage: a human authors
	// specs and seed issues. The orchestrator never dispatches it to a runner.
	StageKindHuman = "human"
	// StageKindTrustedMerge is the integrate stage: the orchestrator itself merges a
	// verified candidate to main (the trusted merge queue), never an agent.
	StageKindTrustedMerge = "trusted-merge"
)

// Stage is one node in the workflow DAG, keyed by stage name in the DAG map. A
// stage is either an agent stage — it names a Role that souls fulfill and may carry
// guards — or a non-agent stage with Kind set ("human" for requirements,
// "trusted-merge" for integrate). Depth between stages is declarative via Produces;
// breadth within a stage is emergent (see specs/workflow.md).
type Stage struct {
	Kind          string   `yaml:"kind,omitempty"`          // non-agent stage: "human" | "trusted-merge"
	Role          string   `yaml:"role,omitempty"`          // role souls fulfill for an agent stage
	Precondition  string   `yaml:"precondition,omitempty"`  // guard that must hold before entry, e.g. "blockers-closed"
	Postcondition []string `yaml:"postcondition,omitempty"` // guards evaluated in a clean verification sandbox before acceptance
	OnFailure     string   `yaml:"on_failure,omitempty"`    // mandatory route when a postcondition fails (a stage name)
	Produces      []string `yaml:"produces,omitempty"`      // declarative depth: stages the orchestrator creates on success
}

// Policy is the termination guarantee: a retry cap and budgets bound the feedback
// loop, so a spec the factory cannot satisfy dead-letters instead of looping
// forever (see specs/workflow.md). It is not merely cost control.
type Policy struct {
	MaxRetries int    `yaml:"max_retries"` // max on_failure cycles before dead-lettering
	Budget     Budget `yaml:"budget"`      // per-issue cap
	EpicBudget Budget `yaml:"epic_budget"` // cumulative cap across an epic
	DeadLetter string `yaml:"dead_letter"` // subject breached work is dead-lettered to, e.g. "harness.dlq"
}

// Budget caps spend along one or more dimensions. A zero field means that dimension
// is uncapped. Tokens and USD bound model spend; Wall bounds elapsed wall-clock.
type Budget struct {
	Tokens int      `yaml:"tokens,omitempty"`
	USD    float64  `yaml:"usd,omitempty"`
	Wall   Duration `yaml:"wall,omitempty"`
}

// LoadHarness reads and unmarshals harness.yaml. It parses strictly (unknown keys
// are errors) but does not check DAG legality — that is harness validate's job (see
// specs/configuration.md). A missing file, malformed YAML, or unknown key fails here.
func LoadHarness(path string) (*Harness, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read harness file %s: %w", path, err)
	}
	var h Harness
	if err := unmarshalStrict(data, &h); err != nil {
		return nil, fmt.Errorf("config: parse harness file %s: %w", path, err)
	}
	return &h, nil
}
