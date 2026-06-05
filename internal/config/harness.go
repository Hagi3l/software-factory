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

	// RequirementsPlanner configures the trusted, non-sandboxed requirements planner —
	// the interactive LLM behind the control-room Create-Task wizard (T4.12,
	// specs/control-room.md, specs/workflow.md). Unlike a soul it fulfills no DAG role and
	// is never dispatched to a runner or sandbox: it runs no untrusted code (it converses
	// with a human and, later, drafts specs + seed issues), so it reuses the canonical
	// model layer directly rather than the runner/broker. It is optional — a config that
	// omits it builds a control room with the wizard disabled — and validated at startup
	// like a soul (its model must resolve in the infra registry; its persona file must
	// exist). See RequirementsPlanner and validateRequirementsPlanner.
	RequirementsPlanner *RequirementsPlanner `yaml:"requirements_planner,omitempty"`
}

// RequirementsPlanner is the requirements-stage planner config: the interactive,
// trusted, non-sandboxed LLM the control-room wizard drives a steered conversation with
// to converge on testable intent (specs/control-room.md). Its Model resolves through the
// infra model registry exactly like a soul's (the runner does not dispatch it, but the
// composition root resolves the same registry to an adapter); Persona is the markdown
// prompt that gives it its elicitation behavior, resolved against the config root like a
// soul persona. MaxTokens bounds one reply turn (0 = the adapter default).
//
// SandboxProfile and BaseRef are optional and enable read-only codebase exploration
// (T4.28): when SandboxProfile names a profile in the infra sandbox registry, the wizard
// provisions a read-only, zero-network sandbox over the integration repo (seeded at BaseRef,
// defaulting to the repo's current branch) and gives the planner the agent's read tools
// (read_file/list_dir/search + the LSP comprehension tools) so it can ground its specs and
// seed issues in the real code. Absent SandboxProfile, the planner has no tools and behaves
// exactly as before (pure conversation). The conversation itself stays trusted and host-side;
// only the model-chosen repo reads are sandbox-confined (see specs/control-room.md).
type RequirementsPlanner struct {
	Model          string `yaml:"model"`
	Persona        string `yaml:"persona"`
	MaxTokens      int    `yaml:"max_tokens,omitempty"`
	SandboxProfile string `yaml:"sandbox_profile,omitempty"`
	BaseRef        string `yaml:"base_ref,omitempty"`
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

// Autonomy profiles for Policy.Profile. They set when the human-approved postcondition
// on an integrate is enforced — the trusted-dev → autonomous progression of
// specs/bootstrap.md.
const (
	// ProfileTrustedDev requires human approval on EVERY integrate — the self-hosting
	// transition where the harness writes code and a human reviews every diff before it
	// lands. It is the bootstrap's own profile.
	ProfileTrustedDev = "trusted-dev"
	// ProfileAutonomous requires human approval only when a candidate's diff touches the
	// TCB (Policy.TCBPaths) — no-human-review is earned for non-TCB work, but the TCB stays
	// human-reviewed permanently. It is the default when Profile is unset.
	ProfileAutonomous = "autonomous"
)

// Policy is the termination guarantee: a retry cap and budgets bound the feedback
// loop, so a spec the factory cannot satisfy dead-letters instead of looping
// forever (see specs/workflow.md). It is not merely cost control. It also carries the
// autonomy profile — when a human-approval gate holds an integrate (see specs/bootstrap.md).
type Policy struct {
	MaxRetries int    `yaml:"max_retries"` // max on_failure cycles before dead-lettering
	Budget     Budget `yaml:"budget"`      // per-issue cumulative cap (tokens/USD/wall across the on_failure loop)
	EpicBudget Budget `yaml:"epic_budget"` // cumulative tokens/USD cap across a whole epic, enforced as an aggregate read over every issue sharing an epic_id (T3.8b; the wall dimension is per-issue only)
	DeadLetter string `yaml:"dead_letter"` // subject breached work is dead-lettered to, e.g. "harness.dlq"

	// Profile is the autonomy profile: "trusted-dev" (human approval on every integrate) or
	// "autonomous" (approval only for TCB-touching diffs). Empty defaults to "autonomous",
	// the permissive profile, so a config that names no profile keeps the pre-T2.10 behavior
	// (no approval unless TCBPaths forces it). See ProfileTrustedDev / ProfileAutonomous and
	// ApprovalRequired.
	Profile string `yaml:"profile,omitempty"`
	// TCBPaths are globs marking a diff TCB-touching — the orchestrator, runner/broker,
	// sandbox config, gate harness (e.g. "internal/orchestrator/**"). A candidate whose diff
	// hits any of them requires human approval regardless of profile, which is how
	// TCB-touching changes stay human-reviewed permanently. This list is also the
	// operational definition of the TCB boundary (see specs/bootstrap.md, specs/configuration.md).
	// `**` matches across path separators; `*`/`?` match within a single segment.
	TCBPaths []string `yaml:"tcb_paths,omitempty"`
}

// ApprovalRequired reports whether a candidate whose diff changed changedFiles needs a
// human-approved gate before it may integrate, under this policy. Trusted-dev requires it
// on every integrate; autonomous (the default for an unset profile) requires it only when
// a changed file matches a TCBPaths glob — and a TCB-touching diff forces approval under
// any profile, so the TCB stays human-reviewed permanently (see specs/bootstrap.md). The
// decision is the orchestrator's: this is the policy half, kept beside the profile/globs it
// reads so config is the single source of truth for what gates an integrate.
func (p Policy) ApprovalRequired(changedFiles []string) bool {
	if p.Profile == ProfileTrustedDev {
		return true
	}
	for _, f := range changedFiles {
		if p.tcbTouches(f) {
			return true
		}
	}
	return false
}

// tcbTouches reports whether a repo-relative path matches any TCBPaths glob.
func (p Policy) tcbTouches(path string) bool {
	for _, pat := range p.TCBPaths {
		if matchGlob(pat, path) {
			return true
		}
	}
	return false
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
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-supplied config location, not untrusted agent input.
	if err != nil {
		return nil, fmt.Errorf("config: read harness file %s: %w", path, err)
	}
	var h Harness
	if err := unmarshalStrict(data, &h); err != nil {
		return nil, fmt.Errorf("config: parse harness file %s: %w", path, err)
	}
	return &h, nil
}
