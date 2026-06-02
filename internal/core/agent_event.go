package core

import "encoding/json"

// AgentEventEnvelope is the best-effort live-feed datum the runner's broker publishes on
// messaging.AgentEventsSubject(id) for one invocation (specs/messaging.md "Agent events"). It
// wraps the opaque inner event — a token/reasoning/tool delta or an agent progress/log message —
// with the originating issue id and role so a consumer can scope a live feed to a *single live
// invocation* (the control room's invocation view, plan T4.21) WITHOUT a second beads read. The
// runner holds this binding in the Brief the orchestrator dispatched, so it stamps it at publish
// time rather than making every viewer reconstruct it.
//
// It lives in core because both ends must agree on one schema: the runner (the write side)
// marshals it and the control-room pump (the read side) unmarshals it — the same single-source
// discipline core.IssueStateEvent / core.DLQAlert use. It is an *additive observability emit*
// (publish-only, fire-and-forget core NATS); losing one is harmless, the durable record is the
// artifact-store transcript.
//
// The invocation (agent) id is deliberately NOT carried here — it is the final token of the
// subject (messaging.AgentEventsSubject), recovered by the consumer via AgentIDFromEventSubject,
// so the wire payload need only add what the subject does not already say. Payload stays raw JSON:
// the runner controls the inner shape (runner.tokenEvent or broker.PublishRequest) and the feed
// decides how to render it.
type AgentEventEnvelope struct {
	// IssueID is the issue this invocation is working (empty if the runner had no binding).
	IssueID string `json:"issueId,omitempty"`
	// Role is the issue's role/stage, so a consumer can attribute the event without a beads read.
	Role string `json:"role,omitempty"`
	// Payload is the opaque inner event the broker published on the agent's behalf.
	Payload json.RawMessage `json:"payload"`
}
