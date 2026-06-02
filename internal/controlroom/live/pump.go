package live

import (
	"encoding/json"

	"github.com/nats-io/nats.go"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
)

// AgentEvent is the activity-feed datum a tailed agent event becomes: which invocation
// emitted it (recovered from the subject) and the raw best-effort payload the broker
// published on the agent's behalf — a token delta, or a progress/log message. The
// payload is left as opaque JSON; the feed view (T4.5) decides how to render it, the
// substrate only labels and delivers it.
type AgentEvent struct {
	AgentID string          `json:"agentId"`
	Payload json.RawMessage `json:"payload"`
}

// StartAgentEventPump bridges NATS to the browser: it subscribes once to every
// invocation's core-NATS event subject (the wildcard) and broadcasts each message into
// the hub as an "agent-event" SSE event, which the hub fans out to all connected
// clients. When act is non-nil it also records each event into the activity-feed buffer
// (the T4.5 view reads that buffer to render the feed; the hub broadcast is the live
// nudge that triggers the re-fetch). This is the only piece of the substrate that
// touches NATS; it returns a stop func that unsubscribes (wire it into the run loop's
// teardown).
//
// Everything here is best-effort, matching the fire-and-forget agent events it tails: a
// payload that is not valid JSON is dropped (the broker only ever publishes JSON, so
// this is a guard, not a path), and a stalled browser is dropped by the hub. Losing a
// live event is harmless (specs/messaging.md); the durable record is the artifact-store
// transcript, not this stream.
func StartAgentEventPump(nc *nats.Conn, h *Hub, act *Activity) (func(), error) {
	sub, err := nc.Subscribe(messaging.AgentEventsWildcard, func(msg *nats.Msg) {
		agentID := messaging.AgentIDFromEventSubject(msg.Subject)
		if act != nil && agentID != "" {
			act.Record(agentID, msg.Data)
		}
		env := AgentEvent{
			AgentID: agentID,
			Payload: json.RawMessage(msg.Data),
		}
		data, err := json.Marshal(env)
		if err != nil {
			return
		}
		h.Broadcast(Event{Name: "agent-event", Data: string(data)})
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// StartIssueStatePump bridges the single-writer orchestrator's issue-state transitions to
// the browser: it subscribes once to harness.issue.*.state (the wildcard) and broadcasts
// each transition into the hub as an "issue-state" SSE event, which the board/DAG/DLQ views
// consume as an hx-trigger nudge (server-render-a-fragment, not sse-swap) so a card moves the
// moment the orchestrator advances the issue — crisper than the agent-event firehose those
// views trigger off today (T4.18 swaps them over). It returns a stop func that unsubscribes
// (wire it into the run loop's teardown), the same shape as StartAgentEventPump.
//
// Unlike the agent-event pump there is no Activity buffer and no envelope: the payload is
// already a complete core.IssueStateEvent (the id is in the body, not only the subject), so the
// pump relays the original bytes after validating them. Everything is best-effort, matching the
// fire-and-forget core-NATS transitions it tails (specs/messaging.md "Issue-state events"): a
// message whose subject does not parse to an issue id, or whose body is not a well-formed event,
// is dropped (a guard, not a path — the orchestrator only ever publishes a marshaled event on a
// concrete subject), and a stalled browser is dropped by the hub. Losing a live transition is
// harmless: beads stays authoritative and the views keep a periodic backstop that reconverges them.
func StartIssueStatePump(nc *nats.Conn, h *Hub) (func(), error) {
	sub, err := nc.Subscribe(messaging.IssueStateWildcard, func(msg *nats.Msg) {
		if messaging.IssueIDFromStateSubject(msg.Subject) == "" {
			return
		}
		var ev core.IssueStateEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil || ev.ID == "" {
			return
		}
		h.Broadcast(Event{Name: "issue-state", Data: string(msg.Data)})
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
