package config

import (
	"fmt"
	"os"
)

// Harness is harness.yaml: the workflow DAG plus the termination policy. It changes
// rarely and is environment-independent — the per-environment knobs live in the
// infra overlay (see Infra). See specs/configuration.md and specs/workflow.md.
type Harness struct {
	DAG map[string]Stage `yaml:"dag"`
	// Checks is the check registry: it maps a command-check postcondition identifier
	// (e.g. "tests-pass") to the shell command that realizes it in the clean
	// verification sandbox (exit 0 = pass). It is the bridge from a declared
	// postcondition name to a runnable gate check, so the command a check runs is
	// config rather than code — the single source of truth the gate resolves against
	// (see specs/configuration.md, specs/verification.md). Postconditions backed by a
	// built-in check kind (metric comparisons like "mutation>=0.8", reserved proofs
	// like "tests-red-then-green") are not command checks and need no entry here.
	Checks map[string]string `yaml:"checks,omitempty"`
	Policy Policy            `yaml:"policy"`

	// SpecDepth bounds the spec slice handed to an agent: the issue's referenced spec file
	// plus its cross-linked neighbors reachable within this many link hops (0 = just the
	// referenced file; 1 = it plus its direct neighbors). It is the configured depth of
	// the spec context horizon — large enough to carry the contract and the terms it leans
	// on, small enough not to slurp the whole tree (see internal/spec, specs/specs-process.md).
	// Unset (0) is a safe minimal slice; the bootstrap sets 1.
	SpecDepth int `yaml:"spec_depth,omitempty"`
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
	// StageKindPlan is the decomposition stage: an agent stage (it dispatches to a
	// planner soul and so also names a Role) that is NOT sandbox-gated. The planner
	// writes no candidate; its output is the child issues it proposes (emergent
	// breadth), which the orchestrator validates structurally and writes. A plan stage
	// therefore declares a role but no postcondition (see specs/workflow.md). It is the
	// one kind that coexists with a role.
	StageKindPlan = "plan"
	// StageKindResolve is the merge-conflict-resolution stage: like plan it is an agent
	// stage that names a Role (a merge-resolver soul), but UNLIKE plan it IS sandbox-gated.
	// The orchestrator spawns a resolve issue when a verified candidate cannot be cleanly
	// rebased onto the current main (specs/integration.md step 2): the agent rebases the
	// conflicting candidate onto main and resolves the conflicts, producing a new candidate
	// that the orchestrator re-verifies in a clean sandbox (producer != verifier) before it
	// can land. It is entered only on a conflict — never reached through a produces edge —
	// so it is excluded from the pipeline-entry computation (see cmd/harness entryRole). It
	// must declare a postcondition (the suite re-verifying the resolved tree) and produce
	// the integrate stage to loop back into the merge queue.
	StageKindResolve = "resolve"
)

// Stage is one node in the workflow DAG, keyed by stage name in the DAG map. A
// stage is either an agent stage — it names a Role that souls fulfill and may carry
// guards — or a non-agent stage with Kind set ("human" for requirements,
// "trusted-merge" for integrate). The one hybrid is "plan": an agent stage (it names a
// Role) that is not sandbox-gated, so it carries Kind="plan" alongside its Role and no
// postcondition. Depth between stages is declarative via Produces; breadth within a
// stage is emergent (see specs/workflow.md).
type Stage struct {
	Kind          string   `yaml:"kind,omitempty"`          // non-agent stage: "human" | "trusted-merge"; or agent: "plan" (ungated) | "resolve" (gated, conflict-spawned)
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
