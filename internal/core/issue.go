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
}
