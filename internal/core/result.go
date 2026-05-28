package core

// ResultStatus is an agent's self-reported outcome for one invocation. It is a
// proposal, never an acceptance: even StatusDone means only "candidate ready", and
// acceptance is decided independently by the orchestrator's gate (producer ≠
// verifier — see specs/verification.md).
type ResultStatus string

const (
	// StatusDone means a candidate branch is ready for gating — NOT that it is
	// accepted. An agent never self-certifies its own work.
	StatusDone ResultStatus = "done"
	// StatusFailed means the agent could not produce a usable candidate (e.g. it
	// could not make the tests pass). The orchestrator routes it via on_failure.
	StatusFailed ResultStatus = "failed"
	// StatusNeedsSpecClarification means the agent detected spec ambiguity or
	// contradiction and is escalating rather than inventing intent; it routes to
	// the human re-entry loop (see specs/specs-process.md).
	StatusNeedsSpecClarification ResultStatus = "needs-spec-clarification"
)

// Result is what an agent returns out of the sandbox. Everything in it is a
// proposal; the orchestrator validates and applies it (see
// specs/components/agent.md, specs/components/orchestrator.md).
type Result struct {
	// IssueID is the beads issue this Result answers. It is stamped by the runner — the
	// trusted component that received the dispatched Brief — NOT self-reported by the
	// agent, so the orchestrator can correlate a Result to an issue without trusting
	// sandboxed code to address its own work (see specs/components/runner.md). It is the
	// only reliable correlator for a failed/escalated Result, which carries no candidate
	// branch to derive the issue from.
	IssueID  string
	Status   ResultStatus // the self-reported outcome
	Branch   Branch       // the candidate branch the agent produced
	Evidence Evidence     // proof for the gate and the provenance trail
	Proposes []Proposal   // proposed child issues (emergent breadth)
}

// Branch identifies the candidate an agent produced. Agents never merge; they
// produce a candidate branch and the trusted merge queue decides on it (see
// specs/integration.md).
type Branch struct {
	Ref     string   // candidate branch ref
	Commits []string // commit SHAs the agent added on the branch
}

// CandidateBranch is the one branch name an invocation for the given issue may push.
// It is the single source of truth for the task-branch convention, shared by both ends
// of the broker: the agent's submit tool commits and pushes onto this ref, and the
// runner's broker relay refuses any other branch (enforcing "push only the task branch",
// in particular refusing main). It lives in core so neither side owns it — see
// specs/components/runner.md, specs/security.md.
func CandidateBranch(issueID string) string { return "candidate/" + issueID }

// Evidence is the proof attached to a Result. Large items (transcripts, gate
// output, diffs) are not inlined — they are written to the content-addressed
// artifact store and referenced by hash, so the proof survives sandbox teardown
// and the provenance trail stays intact (see specs/components/artifact-store.md).
type Evidence struct {
	PromptSHA string        // SHA of the exact prompt the invocation ran with
	Artifacts []ArtifactRef // references into the artifact store
}

// ArtifactRef points at one item in the content-addressed artifact store.
type ArtifactRef struct {
	Kind string // what the artifact is, e.g. "transcript", "gate-output", "log", "diff"
	Hash string // content address into the artifact store
}

// Proposal is a child issue an agent proposes (emergent breadth). The
// orchestrator validates DAG-legality — valid role, edges keep the graph acyclic,
// within budget — before writing it; an illegal proposal is simply rejected (see
// specs/workflow.md).
type Proposal struct {
	Issue     Issue    // the proposed child issue; ID is assigned by the orchestrator on write
	DependsOn []string // blocked-by edges: IDs of issues this child depends on
}
