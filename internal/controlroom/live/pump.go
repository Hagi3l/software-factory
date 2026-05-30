package live

import (
	"encoding/json"

	"github.com/nats-io/nats.go"

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
// clients. This is the only piece of the substrate that touches NATS; it returns a stop
// func that unsubscribes (wire it into the run loop's teardown).
//
// Everything here is best-effort, matching the fire-and-forget agent events it tails: a
// payload that is not valid JSON is dropped (the broker only ever publishes JSON, so
// this is a guard, not a path), and a stalled browser is dropped by the hub. Losing a
// live event is harmless (specs/messaging.md); the durable record is the artifact-store
// transcript, not this stream.
func StartAgentEventPump(nc *nats.Conn, h *Hub) (func(), error) {
	sub, err := nc.Subscribe(messaging.AgentEventsWildcard, func(msg *nats.Msg) {
		env := AgentEvent{
			AgentID: messaging.AgentIDFromEventSubject(msg.Subject),
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
