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

// Merge-queue lifecycle states a candidate passes through in the serialized merge train
// (specs/integration.md). They are single-source here so the orchestrator (write side) and the
// merge-queue view (read side, T4.25) agree on spelling, the same discipline the gate-check
// kinds use. The happy path is queued → (rebasing → re-gating →)? landed — the rebasing/re-gating
// pair only fires when main moved under the candidate (a fast-forward lands the exact gated tree,
// so it skips straight to landed). The two terminal failures correlate with the dead-letter / fix
// issue the same transition already routes: conflicted (the rebase collides, step 2) and
// regate-failed (the rebased combination breaks the gate, step 3).
const (
	MergeStateQueued       = "queued"
	MergeStateRebasing     = "rebasing"
	MergeStateReGating     = "re-gating"
	MergeStateLanded       = "landed"
	MergeStateConflicted   = "conflicted"
	MergeStateRegateFailed = "regate-failed"
)

// MergeStateEvent is the single-writer orchestrator's typed announcement of a merge-queue
// *step transition* — published on messaging.MergeStateSubject(id) at each position a candidate
// passes through in the serialized merge train (specs/integration.md "The queue announces
// itself"). It makes the integration pipeline observable *in flight* (the merge-queue view,
// T4.25) — the rebase-and-re-gate interval where independently-green branches actually break —
// rather than only after a commit lands on main.
//
// It lives in core for the same single-source reason IssueStateEvent does: the orchestrator
// marshals it and the control-room pump (T4.25) unmarshals it. It is an *additive observability
// emit* (publish-only, fire-and-forget core NATS), exactly like issue-state: the authoritative
// queue state stays the git refs + beads and is never reconstructed from these events, so a
// dropped one is harmless (the view's periodic backstop reconverges it).
type MergeStateEvent struct {
	// ID is the issue whose candidate is being integrated (the candidate ref is
	// CandidateBranch(ID)). It keys the subject so the view can correlate a merge step with the
	// issue, its dead-letter/fix routing, and its provenance.
	ID string `json:"id"`
	// State is the queue step entered — one of the MergeState* constants above.
	State string `json:"state"`
	// Role is the issue's role/stage, so a consumer can attribute the step without a beads read.
	Role string `json:"role"`
	// Epic is the issue's epic (EpicOf), so the view can group merge activity by epic.
	Epic string `json:"epic"`
	// Commit is the new main commit the integration landed, set only on MergeStateLanded so the
	// view can link a landed row onward to provenance; empty for every other state.
	Commit string `json:"commit,omitempty"`
	// TS is when the orchestrator announced the step (UTC).
	TS time.Time `json:"ts"`
}
