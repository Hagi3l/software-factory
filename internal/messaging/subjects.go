package messaging

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

// ControlSubject is a specific orchestrator control/health subject under
// harness.control.*.
func ControlSubject(name string) string { return "harness.control." + name }
