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

	// Trace is the test↔spec traceability map an author-tests agent emits: one entry per
	// acceptance test naming the spec heading and sentence it claims to encode. It does
	// not prove faithfulness — the gate's red→green/mutation checks do that — but it makes
	// the test author's interpretation of pure-prose specs auditable, the only window a
	// human has into how the model read the prose (see specs/verification.md). Like other
	// large evidence it is not carried to main on the envelope: the runner harvests it to
	// the artifact store and the structured form is cleared, leaving only the ArtifactRef
	// on Evidence; the provenance trailer cites it by hash. Empty for non-author roles.
	Trace []TraceEntry
}

// TraceEntry is one row of the test↔spec traceability map: the acceptance test, and the
// spec heading + sentence the author claims it encodes. The spec is pure prose, so this
// is the author's own account of its interpretation — auditable after the fact, not a
// proof (see specs/verification.md, specs/specs-process.md).
type TraceEntry struct {
	Test     string // the test this entry traces, e.g. "TestRejectsNegativeQuantity"
	Spec     string // the spec file the heading lives in, e.g. "verification.md"
	Heading  string // the spec heading the test claims to encode, e.g. "Red→green proof"
	Sentence string // the spec sentence the test claims to encode
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

// ArtifactKindTraceabilityMap is the artifact Kind under which the runner stores a
// harvested test↔spec traceability map (see Result.Trace). It lives in core so the
// writer (the runner's harvest) and the reader (the orchestrator threading the map's
// hash into the provenance trailer) agree on the spelling — the same single-source
// pattern the postcondition/metric identifiers use.
const ArtifactKindTraceabilityMap = "traceability-map"

// Proposal is a child issue an agent proposes (emergent breadth). The
// orchestrator validates DAG-legality — valid role, edges keep the graph acyclic,
// within budget — before writing it; an illegal proposal is simply rejected (see
// specs/workflow.md).
type Proposal struct {
	Issue Issue // the proposed child issue; ID is assigned by the orchestrator on write

	// Key is an optional batch-local label so a sibling proposed in the SAME Apply
	// batch can be named in another proposal's DependsOn before any real ID exists. A
	// decomposition planner emits an ordered set of children at once (the seed has no
	// pre-existing siblings to depend on), so inter-sibling edges can only be expressed
	// symbolically; Apply resolves a DependsOn entry that matches a sibling's Key to that
	// sibling's assigned ID (see Apply). Empty for a child with no siblings to reference.
	Key string

	// DependsOn are blocked-by edges. Each entry is either an existing issue ID or the
	// Key of a sibling proposed in the same batch (resolved by Apply). bd rejects any
	// edge that would close a cycle, the acyclicity guarantee for the issue DAG.
	DependsOn []string
}
