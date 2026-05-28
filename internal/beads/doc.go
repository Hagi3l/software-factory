// Package beads integrates with the beads (bd) work-item store: querying ready
// work and reading issue fields into Briefs, and applying the orchestrator's
// single-writer status transitions and validated issue-graph mutations.
//
// Only the orchestrator writes beads; agents merely propose changes via a Result.
// Funnelling all access through this one package is what keeps the single-writer
// invariant — and thus the consistency of the work graph — enforceable.
package beads
