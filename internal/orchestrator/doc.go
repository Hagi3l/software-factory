// Package orchestrator is the scheduler, gatekeeper, and sole beads writer. Its
// reconciliation loop computes ready work, dispatches it to roles, gates the
// returned Results, advances the workflow graph, routes failures, and dead-letters
// budget breaches.
//
// It executes nothing itself: concentrating all scheduling and the only beads-write
// path here is what makes the work graph consistent (one writer, total order) and
// tamper-resistant against confused or hostile agents.
package orchestrator
