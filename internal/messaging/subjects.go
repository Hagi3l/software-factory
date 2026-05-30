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
	SubjectDLQ = "harness.dlq"

	// WorkStreamSubjects matches every per-role work subject; it is the subject
	// filter for the work stream.
	WorkStreamSubjects = "harness.work.>"
	// ResultStreamSubjects matches every per-role result subject; the subject filter
	// for the result stream.
	ResultStreamSubjects = "harness.result.>"

	// ControlSubjects matches orchestrator control/health subjects (core NATS).
	ControlSubjects = "harness.control.*"
)

// WorkSubject is the JetStream work-queue subject for a role. Runners across hosts
// compete to pull from the role's consumer, which gives load balancing and
// horizontal scale by adding runners (see specs/messaging.md).
func WorkSubject(role string) string { return "harness.work." + role }

// ResultSubject is the subject an agent's Result envelope for a role flows back on,
// consumed and validated by the orchestrator.
func ResultSubject(role string) string { return "harness.result." + role }

// AgentEventsSubject is the core-NATS subject one invocation publishes progress and
// log events to; the control room tails it and pushes to browsers over SSE.
// Best-effort — losing one event is harmless.
func AgentEventsSubject(agentID string) string { return "harness.agent." + agentID + ".events" }

// AgentEventsWildcard matches the event subject of every invocation. The control
// room subscribes to it once to tail the live activity feed across all agents,
// rather than per-invocation subscriptions it would have to add and drop as work
// comes and goes (see specs/control-room.md "Activity feed", specs/messaging.md).
const AgentEventsWildcard = "harness.agent.*.events"

// AgentIDFromEventSubject recovers the invocation id from an agent-events subject
// (the inverse of AgentEventsSubject), returning "" if subj is not a well-formed
// agent-events subject. The control room uses it to label each tailed event with the
// agent that emitted it without the publisher having to repeat the id in the payload.
func AgentIDFromEventSubject(subj string) string {
	const prefix = "harness.agent."
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

// ControlSubject is a specific orchestrator control/health subject under
// harness.control.*.
func ControlSubject(name string) string { return "harness.control." + name }
