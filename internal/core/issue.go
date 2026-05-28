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
