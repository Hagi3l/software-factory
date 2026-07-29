package live

import (
	"encoding/json"

	"github.com/nats-io/nats.go"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/messaging"
)

// AgentEvent is the activity-feed datum a tailed agent event becomes: which invocation
// emitted it (recovered from the subject), the issue id + role it is working (stamped on
// the wire envelope by the runner, so a consumer can scope a feed to one live invocation
// without a beads read — plan T4.20), and the raw best-effort payload the broker published
// on the agent's behalf — a token delta, or a progress/log message. The payload is left as
// opaque JSON; the feed view (T4.5) decides how to render it, the substrate only labels and
// delivers it.
type AgentEvent struct {
	AgentID string          `json:"agentId"`
	IssueID string          `json:"issueId,omitempty"`
	Role    string          `json:"role,omitempty"`
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
// subject that does not parse to an invocation id, or a body that is not a well-formed
// core.AgentEventEnvelope, is dropped (the runner only ever publishes a marshaled envelope on
// a concrete subject, so these are guards, not paths), and a stalled browser is dropped by the
// hub. Losing a live event is harmless (specs/messaging.md); the durable record is the
// artifact-store transcript, not this stream.
//
// The wire payload is the runner's issue/role-stamped envelope (core.AgentEventEnvelope); the
// pump unwraps it, labels the broadcast + buffer entry with the agent id recovered from the
// subject (the one field the envelope leaves to the subject), and carries the issue id + role
// through so a downstream view can scope a feed to one invocation (plan T4.20/T4.21).
func StartAgentEventPump(nc *nats.Conn, h *Hub, act *Activity) (func(), error) {
	sub, err := nc.Subscribe(messaging.AgentEventsWildcard, func(msg *nats.Msg) {
		agentID := messaging.AgentIDFromEventSubject(msg.Subject)
		if agentID == "" {
			return
		}
		var env core.AgentEventEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			return
		}
		if act != nil {
			act.Record(agentID, env.IssueID, env.Role, env.Payload)
		}
		data, err := json.Marshal(AgentEvent{
			AgentID: agentID,
			IssueID: env.IssueID,
			Role:    env.Role,
			Payload: env.Payload,
		})
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
// the browser: it subscribes once to factory.issue.*.state (the wildcard) and broadcasts
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

// StartDLQPump bridges the durable dead-letter queue to the browser: it subscribes once to the
// fixed factory.dlq subject (messaging.SubjectDLQ) and broadcasts each escalation into the hub as
// a "dlq-arrival" SSE event. The dead-letter queue is the human's only action surface, so an
// arrival is the one factory event worth a *push* — the layout's alerts.js fires a browser
// Notification on it, and the status bar bumps its escalation count — while everything else stays
// pull (specs/control-room.md, specs/messaging.md). It returns a stop func that unsubscribes
// (wire it into the run loop's teardown), the same shape as the other pumps.
//
// Two differences from StartIssueStatePump, both deliberate: (1) the subject is a *fixed* subject,
// not a wildcard, so there is no id-from-subject guard — the id rides in the body; (2) it tails a
// JetStream-backed subject with a plain core subscription, which is sound because a JetStream
// publish is an ordinary publish on the subject that core subscribers also receive — the durable
// stream remains the source of truth and this tail is only the live nudge, so a dropped message is
// harmless (matching the best-effort posture of the other pumps). A body that is not a well-formed
// core.DLQAlert (or is missing its issue id) is dropped — a guard, not a path, since the
// orchestrator only ever publishes a marshaled alert; a stalled browser is dropped by the hub.
func StartDLQPump(nc *nats.Conn, h *Hub) (func(), error) {
	sub, err := nc.Subscribe(messaging.SubjectDLQ, func(msg *nats.Msg) {
		var alert core.DLQAlert
		if err := json.Unmarshal(msg.Data, &alert); err != nil || alert.IssueID == "" {
			return
		}
		h.Broadcast(Event{Name: "dlq-arrival", Data: string(msg.Data)})
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// StartMergeStatePump bridges the single-writer orchestrator's merge-queue step transitions to
// the browser: it subscribes once to factory.merge.*.state (the wildcard) and broadcasts each
// step into the hub as a "merge-state" SSE event, which the merge-queue view (plan T4.25)
// consumes as an hx-trigger nudge to refetch the train. When mq is non-nil it also records the
// step into the merge-queue buffer (the view reads that buffer to render the rows; the hub
// broadcast is the live nudge that triggers the re-fetch) — the same buffer+broadcast shape the
// agent-event pump uses for the activity feed. It returns a stop func that unsubscribes (wire it
// into the run loop's teardown).
//
// It mirrors StartIssueStatePump's guards: the payload is already a complete
// core.MergeStateEvent (the id is in the body, not only the subject), so the pump relays the
// original bytes after validating them. Everything is best-effort, matching the fire-and-forget
// core-NATS merge-state transitions it tails (specs/messaging.md, specs/integration.md "The
// queue announces itself"): a subject that does not parse to an issue id, or a body that is not
// a well-formed event, is dropped (a guard, not a path — the orchestrator only publishes a
// marshaled event on a concrete subject), and a stalled browser is dropped by the hub. Losing a
// step is harmless: the git refs + beads stay authoritative and the view keeps a periodic
// backstop that re-reads the buffer.
func StartMergeStatePump(nc *nats.Conn, h *Hub, mq *MergeQueue) (func(), error) {
	sub, err := nc.Subscribe(messaging.MergeStateWildcard, func(msg *nats.Msg) {
		if messaging.IssueIDFromMergeSubject(msg.Subject) == "" {
			return
		}
		var ev core.MergeStateEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil || ev.ID == "" {
			return
		}
		if mq != nil {
			mq.Record(ev)
		}
		h.Broadcast(Event{Name: "merge-state", Data: string(msg.Data)})
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
