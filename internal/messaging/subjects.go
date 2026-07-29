package messaging

import "strings"

// Subject taxonomy for all harness NATS traffic. Subjects are built through these
// helpers and constants so the taxonomy lives in exactly one place and is never
// hand-typed at call sites; see specs/messaging.md for the contract.
//
// Work, result, and DLQ subjects are JetStream-backed (durable, replayable). Agent
// events and control subjects are core NATS (fire-and-forget) and have no stream —
// losing a progress event is harmless.

const (
	// SubjectDLQ carries dead-lettered work for human triage. JetStream-backed and
	// retained until a human handles it (see specs/workflow.md).
	SubjectDLQ = "factory.dlq"

	// SubjectApprovals carries human approve/reject decisions published by `software-factory approve`
	// / `software-factory reject` for the single-writer orchestrator to consume and record. It is
	// JetStream-backed (durable, at-least-once) so a decision survives an orchestrator
	// restart and is redelivered until acked — the orchestrator's status-gated handling is
	// idempotent under that redelivery (see specs/configuration.md "human-approval", T2.10).
	SubjectApprovals = "factory.approvals"

	// WorkStreamSubjects matches every per-role work subject; it is the subject
	// filter for the work stream.
	WorkStreamSubjects = "factory.work.>"
	// ResultStreamSubjects matches every per-role result subject; the subject filter
	// for the result stream.
	ResultStreamSubjects = "factory.result.>"

	// ControlSubjects matches orchestrator control/health subjects (core NATS).
	ControlSubjects = "factory.control.*"
)

// WorkSubject is the JetStream work-queue subject for a role. Runners across hosts
// compete to pull from the role's consumer, which gives load balancing and
// horizontal scale by adding runners (see specs/messaging.md).
func WorkSubject(role string) string { return "factory.work." + role }

// ResultSubject is the subject an agent's Result envelope for a role flows back on,
// consumed and validated by the orchestrator.
func ResultSubject(role string) string { return "factory.result." + role }

// AgentEventsSubject is the core-NATS subject one invocation publishes progress and
// log events to; the control room tails it and pushes to browsers over SSE.
// Best-effort — losing one event is harmless.
func AgentEventsSubject(agentID string) string { return "factory.agent." + agentID + ".events" }

// AgentEventsWildcard matches the event subject of every invocation. The control
// room subscribes to it once to tail the live activity feed across all agents,
// rather than per-invocation subscriptions it would have to add and drop as work
// comes and goes (see specs/control-room.md "Activity feed", specs/messaging.md).
const AgentEventsWildcard = "factory.agent.*.events"

// AgentIDFromEventSubject recovers the invocation id from an agent-events subject
// (the inverse of AgentEventsSubject), returning "" if subj is not a well-formed
// agent-events subject. The control room uses it to label each tailed event with the
// agent that emitted it without the publisher having to repeat the id in the payload.
func AgentIDFromEventSubject(subj string) string {
	const prefix = "factory.agent."
	const suffix = ".events"
	if !strings.HasPrefix(subj, prefix) || !strings.HasSuffix(subj, suffix) {
		return ""
	}
	id := subj[len(prefix) : len(subj)-len(suffix)]
	// Reject an empty id, an embedded subject separator, or a NATS wildcard token: a
	// concrete delivered subject has exactly one non-wildcard token here.
	if id == "" || strings.ContainsAny(id, ".*>") {
		return ""
	}
	return id
}

// IssueStateSubject is the core-NATS subject the single-writer orchestrator publishes an
// issue's state transitions to (the inverse decode is IssueIDFromStateSubject). The control
// room tails it (T4.17 pump) to refresh the board/DAG/DLQ views crisply on the actual
// transition — a card moves the moment the orchestrator advances the issue — rather than
// polling around agent activity. Best-effort core NATS like agent events: losing one is
// harmless because the views keep a slow periodic backstop that reconverges them (see
// specs/messaging.md "Issue-state events").
func IssueStateSubject(issueID string) string { return "factory.issue." + issueID + ".state" }

// IssueStateWildcard matches the state subject of every issue. The control room subscribes
// to it once to tail all issue-state transitions, mirroring AgentEventsWildcard, rather than
// per-issue subscriptions it would have to add and drop as work comes and goes.
const IssueStateWildcard = "factory.issue.*.state"

// IssueIDFromStateSubject recovers the issue id from an issue-state subject (the inverse of
// IssueStateSubject), returning "" if subj is not a well-formed issue-state subject. The
// T4.17 pump uses it to label each tailed event with the issue it concerns. It mirrors
// AgentIDFromEventSubject exactly: a concrete delivered subject has exactly one non-wildcard
// token in the id position, so an empty id, an embedded separator, or a wildcard token is
// rejected.
func IssueIDFromStateSubject(subj string) string {
	const prefix = "factory.issue."
	const suffix = ".state"
	if !strings.HasPrefix(subj, prefix) || !strings.HasSuffix(subj, suffix) {
		return ""
	}
	id := subj[len(prefix) : len(subj)-len(suffix)]
	if id == "" || strings.ContainsAny(id, ".*>") {
		return ""
	}
	return id
}

// MergeStateSubject is the core-NATS subject the single-writer orchestrator publishes a
// candidate's merge-queue step transitions to (the inverse decode is IssueIDFromMergeSubject),
// keyed by the integrating issue's id. The control room tails it (T4.25 pump) to render the
// merge train in flight — queued → rebasing → re-gating → landed, or the terminal
// conflicted / regate-failed. It is a distinct subject tree from issue-state because the merge
// queue is a separate lifecycle layered over the integrate stage (specs/integration.md "The
// queue announces itself"). Best-effort core NATS like issue-state: losing one is harmless
// because the view keeps a periodic backstop that reconverges it.
func MergeStateSubject(issueID string) string { return "factory.merge." + issueID + ".state" }

// MergeStateWildcard matches the merge-state subject of every candidate. The control room
// subscribes to it once to tail all merge-queue transitions, mirroring IssueStateWildcard.
const MergeStateWildcard = "factory.merge.*.state"

// IssueIDFromMergeSubject recovers the issue id from a merge-state subject (the inverse of
// MergeStateSubject), returning "" if subj is not a well-formed merge-state subject. It mirrors
// IssueIDFromStateSubject exactly: a concrete delivered subject has one non-wildcard token in
// the id position, so an empty id, an embedded separator, or a wildcard token is rejected.
func IssueIDFromMergeSubject(subj string) string {
	const prefix = "factory.merge."
	const suffix = ".state"
	if !strings.HasPrefix(subj, prefix) || !strings.HasSuffix(subj, suffix) {
		return ""
	}
	id := subj[len(prefix) : len(subj)-len(suffix)]
	if id == "" || strings.ContainsAny(id, ".*>") {
		return ""
	}
	return id
}

// ControlSubject is a specific orchestrator control/health subject under
// factory.control.*.
func ControlSubject(name string) string { return "factory.control." + name }
