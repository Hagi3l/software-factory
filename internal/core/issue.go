package core

import "time"

// Issue is one work item in the beads dependency graph — the unit the
// orchestrator schedules and the seed of a Brief.
//
// Only the orchestrator writes issues; every other component treats them as
// read-only (see specs/components/orchestrator.md). The fields here are the issue
// facets an agent is handed. Beads owns lifecycle status (ready/in_progress/
// blocked/…) and the blocked-by edges; those are modeled with the beads
// integration (plan T1.3/T1.4) rather than guessed at here.
type Issue struct {
	ID    string // beads issue identifier; empty for a not-yet-created proposal
	Title string
	Body  string
	Role  string // the role/stage this issue is dispatched to

	// Base is the git ref a produced issue's candidate must branch from. It is empty for
	// a freshly seeded issue — the orchestrator then seeds the worktree from the pipeline
	// base (main) — and set when one agent stage produces the next: the produced issue
	// inherits the predecessor's verified candidate branch so the downstream stage builds
	// on the work already done. This is what carries the failing acceptance tests from
	// the author-tests candidate into the implementor's worktree, rather than branching
	// implement from a main that has neither tests nor implementation (see
	// specs/workflow.md, specs/verification.md). Like Attempt and Role it rides in beads
	// metadata, and is preserved across on_failure retries so a fix attempt builds on the
	// same base its predecessor did.
	Base string

	// Status is the beads lifecycle status (open/in_progress/blocked/closed),
	// populated when an issue is read back. It is read-only to everything but the
	// orchestrator's single-writer transitions. The orchestrator reads it to stay
	// idempotent: a Result is acted on only while its issue is in_progress, so a
	// duplicate or stale redelivery for an already-processed issue is ignored (see
	// specs/components/orchestrator.md).
	Status string

	// Attempt is the on_failure retry generation: 0 for a freshly seeded or produced
	// issue, incremented each time the orchestrator routes a failure into a new fix
	// issue. It is the persistent counter the retry cap is enforced against — half of
	// the termination guarantee — and rides in beads metadata (see specs/workflow.md).
	Attempt int

	// TraceMap is the artifact-store hash of the test↔spec traceability map produced by
	// the author-tests stage (see core.Result.Trace, ArtifactKindTraceabilityMap). It is
	// empty for a freshly seeded issue and stamped when an agent stage produces the next:
	// the author-tests candidate's map is threaded forward onto the implement issue, just
	// as Base threads the candidate branch, so it survives to the integrate stage where
	// the orchestrator cites it in the merge's provenance trailer (`Traceability: <hash>`).
	// Like Base it rides in beads metadata and is preserved across on_failure retries, so a
	// re-implemented candidate still traces back to the same author's interpretation (see
	// specs/verification.md, specs/security.md).
	TraceMap string

	// Spec is the repository-relative path of the spec file this issue is governed by
	// (e.g. "specs/orders.md"). It is the issue's structured reference into the spec graph
	// — a first-class field, not prose buried in Body — so the orchestrator can resolve the
	// bounded spec slice for the Brief (the referenced file + its linked neighbors to a
	// configured depth; see internal/spec, specs/specs-process.md) without parsing untrusted
	// agent-authored text. It is set at seed time and by the decomposition planner per child
	// (request_subtask's `spec`), rides in beads metadata like Base/TraceMap, and is threaded
	// forward across an epic's stages so author-tests, implement, and qa all resolve the same
	// contract. Empty when the issue names no spec (the slice is then omitted and the agent
	// falls back to the specs/ tree in its worktree). T3.6 will additionally pin the slice's
	// content hash on the issue for spec-version drift detection.
	Spec string

	// SpecHash is the content address of the spec slice this issue was last briefed against
	// (see internal/spec.Hash). Unlike Spec (the path, set at creation and threaded forward),
	// the orchestrator pins this at *dispatch* — when it materializes the slice for the Brief —
	// because it records the spec *version* the agent actually worked against, not a property
	// inherited from the parent. A later edit to the governing spec changes the re-resolved
	// hash, which is how T3.7 detects which in-flight issues are stale and must be re-derived
	// ("recompile the delta"). Empty until first dispatched, or when the issue names no spec.
	SpecHash string

	// SpentTokens and SpentUSD are the spend already consumed by EARLIER attempts in this
	// issue's on_failure retry chain — the cumulative tokens and dollars burned across the
	// feedback loop so far, not counting the just-finished invocation. They are 0 for a
	// freshly seeded issue and for one produced by advancing to the NEXT stage (each stage
	// begins with a fresh per-issue budget; the cross-stage total is the epic budget, a
	// separate cap — see the plan's T3.8b). When the orchestrator routes a failure it adds
	// the finished invocation's spend (from Result.Usage, priced by the per-model cost
	// table) to these and threads the new total onto the fix issue, exactly as Attempt is
	// threaded; it enforces config Policy.Budget against the running sum and dead-letters on
	// a breach. This is the cumulative half of the termination guarantee that the retry cap
	// alone does not cover — a spec the factory cannot satisfy could otherwise burn unbounded
	// tokens within the retry cap. Like Base/Attempt they ride in beads metadata and are
	// preserved across on_failure retries (see specs/workflow.md).
	SpentTokens int
	SpentUSD    float64

	// SpentWall is the cumulative wall-clock already consumed by EARLIER attempts in this
	// issue's on_failure retry chain — the sum of each prior invocation's elapsed time
	// (Result.Elapsed), not counting the just-finished one. It is the wall-clock analog of
	// SpentTokens: threaded forward on each routed fix issue exactly like the token/dollar
	// spend, so the orchestrator can enforce a *cumulative* wall cap (config Policy.Budget.Wall)
	// across the feedback loop — the third dimension of the budget half of the termination
	// guarantee. It is distinct from the per-invocation wall ceiling the sandbox enforces (that
	// bounds one attempt; this bounds the whole chain). 0 for a freshly seeded or next-stage
	// issue (each stage begins fresh); like SpentTokens it is stamped from the trusted side
	// (the runner's Result.Elapsed, never the agent's self-report) and rides in beads metadata
	// (see specs/workflow.md).
	SpentWall time.Duration

	// ClosingTokens and ClosingUSD are this issue's OWN invocation spend — the marginal tokens
	// and dollars the single invocation answering this issue consumed (not the threaded chain
	// total in SpentTokens/SpentUSD, which is its predecessors' spend). The orchestrator stamps
	// them when it processes the issue's Result (whatever the disposition — accepted, routed,
	// or dead-lettered), so an epic's total spend can be read as an *aggregate*: the sum of
	// ClosingUSD/ClosingTokens over every issue sharing an EpicID. An epic is a fan-out DAG, not
	// a line, so its total cannot be a counter threaded down each branch (that would double-count
	// the shared prefix at the join) — it must be summed across the marginals, which is what these
	// per-issue fields make possible (see EpicID, specs/workflow.md "epic_budget"). They are only
	// stamped when an epic budget is configured (otherwise the extra write is skipped); 0/absent
	// on an issue whose invocation has not yet been processed. Stamped post-hoc by
	// StampClosingSpend, never at creation, so they ride in beads metadata but are not threaded.
	ClosingTokens int
	ClosingUSD    float64

	// EpicID is the id of the root seed issue of this issue's epic — the work item a human
	// seeded, from which the whole fan-out of plan/author-tests/implement/qa issues descends.
	// It is threaded forward onto every produced child and every on_failure fix exactly like
	// Base (the candidate ref): a root seed carries none (it IS its own epic, so its effective
	// epic id is its own ID — see the orchestrator's epicOf), and each descendant carries the
	// root's id so all issues of one epic share it. It is what makes the cross-issue epic budget
	// enforceable as an aggregate read (sum the per-issue ClosingUSD over all issues with this
	// EpicID) rather than a threaded counter, which a fan-out would double-count. Like Base it
	// rides in beads metadata; empty on a root seed (the epicOf fallback supplies its own id).
	// (See specs/workflow.md "epic_budget"; future spec-re-derivation work keys on it too.)
	EpicID string

	// Tags are the issue's selector input: the orchestrator picks which soul fulfills the
	// issue's Role by matching these against each candidate soul's Selector (a role may map
	// to a set of souls — see core.Soul.Matches, specs/configuration.md). They are a
	// distinct binding from Role, stored separately: Role lives in beads metadata and
	// drives dispatch to work.<role>; Tags ride in beads *labels* (one `key=value` label
	// per entry) so a soul selector like {lang: go} never collides with the role binding.
	// Set by the decomposition planner at issue-creation and threaded forward across the
	// stages of an epic (like Base/TraceMap), so a `lang=go` epic routes every stage to
	// the matching soul. Empty when no soul disambiguation is needed (the trivial 1:1
	// single-soul-per-role case ignores them).
	Tags map[string]string

	// CandidateRef is the candidate branch/sha an issue is parked on while awaiting human
	// approval (the human-approved postcondition; see core.PostconditionHumanApproved). It
	// is empty for every issue that is not parked, and set by the orchestrator when an
	// integrate is held for approval: it is the exact candidate a `harness approve` must
	// name, and the binding the orchestrator re-checks so a stale approval (the candidate
	// changed) is invalidated. Like Base it rides in beads metadata; unlike Base it is
	// written by a status transition (AwaitApproval), not at issue creation, so a fresh or
	// produced issue never carries it (see specs/configuration.md, specs/bootstrap.md).
	CandidateRef string

	// ParkedProvenance is the JSON-encoded core.Provenance the orchestrator captured when it
	// parked the issue awaiting approval. The candidate was already gate-verified before
	// parking, so its provenance — soul/model, prompt sha, the checks it passed, the
	// traceability map — is preserved here and replayed onto the merge commit once a human
	// approves, rather than re-graded. It rides in beads metadata (one line, JSON, so a
	// multi-line trailer never has to survive metadata storage), written by AwaitApproval
	// alongside CandidateRef and decoded back into a core.Provenance on resume. Empty for an
	// issue that is not parked.
	ParkedProvenance string

	// Transcript is the artifact-store hash of the broker-captured conversation from this
	// issue's most recent invocation (core.ArtifactKindTranscript). Unlike the merge trailer's
	// transcript — retained only for *merged* work — the orchestrator stamps this onto the
	// issue itself when it processes the issue's Result, whatever the disposition (accepted,
	// routed, or dead-lettered), via StampTranscript. That makes the decision trail reachable
	// for in-flight and *dead-lettered* work too, not just merged commits — which is what lets
	// the control room's Resolve wizard (T4.15) pre-load "the agent transcript that raised the
	// escalation" and what unblocks replaying non-merged invocations (the T4.11 follow-up). It
	// rides in beads metadata, is not threaded forward (each issue records its own latest run),
	// and is empty until the issue's first invocation is processed (see specs/observability.md).
	Transcript string

	// TestsSoul and ImplementSoul record the producing souls of the two stages whose
	// independence is the keystone of the design: the soul that authored the acceptance
	// tests and the soul that implemented against them. Soul selection is otherwise
	// transient — the orchestrator picks a soul at dispatch and the choice is gone once the
	// Brief is sent — so "author ≠ implementor" is only *demonstrable* after the fact if both
	// identities survive. The orchestrator stamps each onto the issue as its stage advances
	// (TestsSoul on the author-tests issue, ImplementSoul on the implement issue, keyed off
	// the stage's reserved proof) and threads them forward onto every produced child and
	// on_failure fix exactly like TraceMap, so a re-implemented candidate still names the same
	// test author. At integrate the threaded TestsSoul rides into the merge trailer
	// (`Tests-Soul:` alongside `Soul:`), and for in-flight or dead-lettered work the stamps
	// make the producer≠verifier split renderable in the verification view without a merge
	// commit (see specs/verification.md "The separation is recorded", the plan's T4.22). Like
	// TraceMap they ride in beads metadata; empty until the relevant stage has run in the
	// issue's lineage.
	TestsSoul     string
	ImplementSoul string

	// GateVerdict is the artifact-store hash of the assembled per-check record of the gate
	// run that graded this issue's candidate (core.GateVerdict, ArtifactKindGateVerdict).
	// Unlike the per-check evidence cited on the merge trailer, this is the *index* over one
	// gate run — pass/fail, red→green, mutation score, scanner exits — and the orchestrator
	// stamps it onto the issue for **every** disposition (accepted, routed, dead-lettered),
	// like Transcript, so the verification view can render a *rejected* candidate's trust
	// argument forensically, not only a merged one. It rides in beads metadata, is NOT
	// threaded forward (each issue records the verdict of its own gate run), and is empty
	// until the issue's candidate has been gated (see specs/verification.md, the plan's T4.22).
	GateVerdict string

	// DeadLetterReason is the orchestrator's one-line classification of *why* an issue
	// dead-lettered — the same reason published on the DLQ alert (an escalation it cannot
	// resolve, an exhausted retry cap, a budget breach). It is empty for every issue that is
	// not blocked, and stamped by the orchestrator in the same write that blocks the issue
	// (Block), so the dead-letter queue and the Resolve wizard can show the human *why* the
	// work is stuck without re-deriving it from the transcript. The agent's detailed reasoning
	// still lives in the Transcript (the orchestrator's reason is the classification, the
	// transcript is the evidence). It rides in beads metadata (see specs/workflow.md,
	// specs/control-room.md).
	DeadLetterReason string

	// StateEnteredAt is when the issue last entered its current beads status — the single
	// writer stamps it (state_entered_at metadata) in the same transition that changes the
	// status, so it is the durable anchor the control-room board ticks its "time in current
	// state" counter from (client-side; the orchestrator never re-renders to tick). Like
	// Status it is populated when an issue is read back, and it is set (not incremented) on
	// each transition, so a redelivered result that lands on an already-settled issue — which
	// the orchestrator ignores without re-writing status — neither moves nor resets it. Zero
	// for an issue that has not transitioned since this field was introduced (the view then
	// falls back to the issue's creation time). It rides in beads metadata (see
	// specs/components/orchestrator.md §9, specs/control-room.md "The board, in motion").
	StateEnteredAt time.Time

	// Lease is the expiry of the claim that put this issue in_progress (UTC), populated when
	// an in_progress issue is read back (it rides in beads metadata, stamped by Claim). It is
	// the durable record of a claim's deadline: a runner that dies mid-task leaves its issue
	// in_progress, and once the lease passes the orchestrator's sweep releases it back to ready.
	// The orchestrator caches it in its in-flight projection at claim time and, crucially, seeds
	// the projection from this durable value on restart so the in-memory lease sweep recovers
	// pre-restart stranded work on the original deadline rather than a fresh one (T3.13). Zero
	// for an issue that is not in_progress (Release clears the lease) — treated as immediately
	// strandable by the sweep, mirroring the old beads stranded query. See
	// specs/components/orchestrator.md ("Live state vs. durable state").
	Lease time.Time

	// CreatedAt is when beads first created the issue — the anchor the control-room board's
	// per-card "total time" timer ticks from (client-side, like StateEnteredAt). Unlike the
	// other facets it is not harness-written metadata: it is beads' own top-level `created_at`
	// timestamp, decoded straight off the read (every issue bd returns carries one), so it is
	// always populated for a real issue and zero only for a not-yet-created proposal. It is the
	// fallback the board uses for "time in current state" when StateEnteredAt is unstamped (an
	// issue that has not transitioned since that field landed). See specs/control-room.md
	// ("The board, in motion").
	CreatedAt time.Time

	// DependsOn carries the blocked-by edge targets beads emits inline on a read: the ids
	// of the issues this one is blocked by (its blockers). Like Status it is populated when
	// an issue is read back — beads owns these edges (the orchestrator writes them via
	// `bd dep`), and the read path decodes the `dependencies` array bd already returns on
	// `bd list --json` rather than issuing a separate dependency query. It backs the control
	// room's DAG view (T4.6): an edge runs blocker→dependent, i.e. for each id in DependsOn
	// there is an edge from that id to this issue. This is the *read-side* dependency facet
	// and is deliberately distinct from the write-side core.Proposal.DependsOn, which is an
	// agent's *proposed* new edges flowing through the single-writer path. Empty for an issue
	// with no blockers (see specs/control-room.md, specs/components/orchestrator.md).
	DependsOn []string
}

// EpicOf returns the id of the epic an issue belongs to: its EpicID when set, else its own
// ID. A root seed carries no EpicID (it IS its own epic), so it folds into its own epic via
// the fallback, with no extra write to stamp its id onto itself; every descendant carries
// the root's id, so all issues of one epic share this value. It is the single source of
// truth for epic grouping — the orchestrator enforces the aggregate epic budget over it
// (summing each member's ClosingUSD/ClosingTokens, which a threaded counter would
// double-count across a fan-out), and the control room's budget view groups by it the same
// way (see specs/workflow.md "epic_budget", specs/control-room.md).
func EpicOf(i Issue) string {
	if i.EpicID != "" {
		return i.EpicID
	}
	return i.ID
}
