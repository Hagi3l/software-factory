package core

// DLQAlert is the escalation the single-writer orchestrator publishes on
// messaging.SubjectDLQ when it dead-letters an issue — a budget breach, an exhausted retry
// cap, or a needs-spec-clarification escalation it could not resolve. It carries enough for
// a human to find the issue and understand why it terminated; the full transcript/evidence
// lives in the artifact store keyed by the issue (see specs/observability.md).
//
// It lives in core because both ends must agree on one schema: the orchestrator (the write
// side) marshals it onto the durable harness.dlq subject, and the control-room DLQ pump (the
// read side, T4.19) unmarshals it to fire a browser escalation alert — the same single-source
// discipline core.IssueStateEvent and core.ApprovalRequest use. The durable JetStream queue
// stays the source of truth; the SSE tail is only the nudge to come look (specs/messaging.md).
type DLQAlert struct {
	IssueID string `json:"issue_id"`
	Role    string `json:"role"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
}
