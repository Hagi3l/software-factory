package core

// ApprovalRequest is the message a `software-factory approve` / `software-factory reject` invocation
// publishes over NATS for the single-writer orchestrator to consume and record. It
// lives in core because both ends must agree on one schema: the CLI (the write side,
// cmd/software-factory) marshals it and the orchestrator (the read side) unmarshals it, the same
// single-source discipline core.Provenance uses for the merge trailer.
//
// A human never writes beads directly during a run — only the orchestrator does (the
// single-writer invariant). So an approval is a *proposal* the orchestrator validates and
// applies, exactly like an agent's Result: the CLI publishes the human's decision, and the
// orchestrator binds it to the issue's current candidate before acting on it.
//
// The decision is bound to CandidateSHA — the exact candidate the human reviewed (read off
// the parked issue by the CLI). The orchestrator re-checks it against the issue's recorded
// candidate ref and ignores a mismatch, so an approval for a candidate that has since
// changed is invalidated rather than silently applied to different code (see
// specs/configuration.md "human-approval", specs/bootstrap.md).
type ApprovalRequest struct {
	IssueID string `json:"issue_id"` // the parked issue being approved/rejected
	// CandidateSHA is the candidate ref/sha the decision is bound to — what the human
	// reviewed. The orchestrator applies the decision only if it still matches the issue's
	// parked candidate, so a stale approval (the candidate changed) is invalidated.
	CandidateSHA string `json:"candidate_sha"`
	// Approved is true for an approve, false for a reject. An approve lets the parked
	// candidate merge; a reject routes a fix attempt (or, with no route/budget left,
	// dead-letters for spec refinement).
	Approved bool `json:"approved"`
	// Approver identifies the human who decided, recorded on the issue for audit. It is
	// not the trust boundary (the NATS endpoint is); it is an accountability record.
	Approver string `json:"approver"`
}
