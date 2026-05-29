package core

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
}
