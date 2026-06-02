package core

import "time"

// IssueStateEvent is the single-writer orchestrator's typed announcement of an issue *state
// transition* — published on messaging.IssueStateSubject(id) whenever the orchestrator changes
// an issue's beads status (and stamps the matching state_entered_at). It is the live nudge the
// control room's board / DAG / dead-letter views refresh off so a card moves columns the moment
// the orchestrator advances it, rather than polling around agent activity (see
// specs/messaging.md "Issue-state events", specs/components/orchestrator.md §9).
//
// It lives in core because both ends must agree on one schema: the orchestrator (the write
// side) marshals it and the control-room pump (the read side, T4.17) unmarshals it — the same
// single-source discipline core.ApprovalRequest and core.Provenance use. It is an *additive
// observability emit* (publish-only, fire-and-forget core NATS): beads stays the authoritative
// state and is never reconstructed from these events, so a dropped one is harmless (the views'
// periodic backstop reconverges them).
type IssueStateEvent struct {
	// ID is the issue whose state changed.
	ID string `json:"id"`
	// Status is the beads status the issue just entered (in_progress/closed/blocked/open).
	Status string `json:"status"`
	// Role is the issue's role/stage, so a consumer can attribute the transition without a
	// beads read; empty when the orchestrator only had the id (a stranded-lease release whose
	// issue could not be re-read).
	Role string `json:"role"`
	// Epic is the issue's epic (EpicOf — its EpicID, else its own ID), so a consumer can group
	// transitions by epic the same way the budget view does.
	Epic string `json:"epic"`
	// TS is when the orchestrator announced the transition (UTC). It is the announce time, a
	// close analog of the durable state_entered_at the same write stamped on the issue.
	TS time.Time `json:"ts"`
}
