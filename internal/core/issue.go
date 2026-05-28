package core

// Issue is one work item in the beads dependency graph — the unit the
// orchestrator schedules and the seed of a Brief.
//
// Only the orchestrator writes issues; every other component treats them as
// read-only (see specs/components/orchestrator.md). The fields here are the issue
// facets an agent is handed. Beads owns lifecycle status (ready/in_progress/
// blocked/…) and the blocked-by edges; those are modelled with the beads
// integration (plan T1.3/T1.4) rather than guessed at here.
type Issue struct {
	ID    string // beads issue identifier; empty for a not-yet-created proposal
	Title string
	Body  string
	Role  string // the role/stage this issue is dispatched to
}
