package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/messaging"
)

// TestAgentEventPump proves the NATS->hub bridge end to end over a real in-process
// server: an event published on one invocation's subject arrives at a hub subscriber,
// labeled with the agent id recovered from the subject and carrying the original
// payload intact. This is the substrate's only NATS-touching path, so it is exercised
// against the actual transport rather than a fake.
func TestAgentEventPump(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	act := NewActivity(8)
	stop, err := StartAgentEventPump(nc, hub, act)
	if err != nil {
		t.Fatalf("StartAgentEventPump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	// The runner publishes the issue/role-stamped envelope (core.AgentEventEnvelope), the
	// inner event being the opaque token payload.
	inner := `{"type":"token","delta":"hi"}`
	env, err := json.Marshal(core.AgentEventEnvelope{
		IssueID: "factory-7",
		Role:    "implementor",
		Payload: json.RawMessage(inner),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := nc.Publish(messaging.AgentEventsSubject("inv-1"), env); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	if ev.Name != "agent-event" {
		t.Fatalf("event name = %q, want agent-event", ev.Name)
	}
	var got AgentEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal AgentEvent: %v (data=%q)", err, ev.Data)
	}
	// AgentID is recovered from the subject; IssueID + Role are carried through from the
	// envelope so a view can scope a feed to one live invocation (plan T4.20). The broadcast
	// payload is the unwrapped inner event, not the envelope.
	if got.AgentID != "inv-1" || got.IssueID != "factory-7" || got.Role != "implementor" {
		t.Fatalf("AgentEvent = %+v, want inv-1 / factory-7 / implementor", got)
	}
	if string(got.Payload) != inner {
		t.Fatalf("Payload = %q, want %q", string(got.Payload), inner)
	}

	// The same event is recorded into the activity buffer, labeled with the agent id
	// recovered from the subject and the issue id + role from the envelope — so the pump feeds
	// both the live nudge (hub) and the rendered feed (buffer) from one subscription.
	rec := act.Recent()
	if len(rec) != 1 {
		t.Fatalf("activity entries = %d, want 1", len(rec))
	}
	if rec[0].AgentID != "inv-1" || rec[0].IssueID != "factory-7" || rec[0].Role != "implementor" ||
		rec[0].Kind != "token" || rec[0].Detail != "hi" {
		t.Fatalf("activity entry = %+v, want inv-1 / factory-7 / implementor token 'hi'", rec[0])
	}
}

// TestAgentEventPumpStopUnsubscribes confirms the stop func detaches the subscription:
// after stop, a published event no longer reaches the hub.
func TestAgentEventPumpStopUnsubscribes(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartAgentEventPump(nc, hub, nil)
	if err != nil {
		t.Fatalf("StartAgentEventPump: %v", err)
	}
	stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	if err := nc.Publish(messaging.AgentEventsSubject("inv-2"), []byte(`{}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	select {
	case ev := <-ch:
		t.Fatalf("received event after stop: %+v", ev)
	default:
	}
}

// TestIssueStatePump proves the orchestrator->hub bridge end to end over a real in-process
// server: a marshaled core.IssueStateEvent published on an issue's state subject arrives at a
// hub subscriber as an "issue-state" SSE event carrying the original payload intact. Like the
// agent-event pump this is exercised against the actual transport, not a fake.
func TestIssueStatePump(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartIssueStatePump(nc, hub)
	if err != nil {
		t.Fatalf("StartIssueStatePump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	want := core.IssueStateEvent{
		ID:     "factory-7",
		Status: "in_progress",
		Role:   "implementor",
		Epic:   "factory-1",
		TS:     time.Unix(1700000000, 0).UTC(),
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal IssueStateEvent: %v", err)
	}
	if err := nc.Publish(messaging.IssueStateSubject(want.ID), payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	if ev.Name != "issue-state" {
		t.Fatalf("event name = %q, want issue-state", ev.Name)
	}
	var got core.IssueStateEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal IssueStateEvent: %v (data=%q)", err, ev.Data)
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

// TestIssueStatePumpDropsMalformed confirms the pump's best-effort guards: a body that is not a
// well-formed event (and an event missing its id) is dropped rather than broadcast. The proof is
// ordering — NATS delivers a single subscription's messages in publish order, so if a dropped
// message were instead broadcast it would arrive before the trailing valid one. Receiving the
// valid event first therefore proves the bad ones never reached the hub.
func TestIssueStatePumpDropsMalformed(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartIssueStatePump(nc, hub)
	if err != nil {
		t.Fatalf("StartIssueStatePump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	// Not JSON at all — dropped.
	if err := nc.Publish(messaging.IssueStateSubject("factory-1"), []byte("not json")); err != nil {
		t.Fatalf("Publish malformed: %v", err)
	}
	// Well-formed JSON but no id — dropped (the id is the one field a consumer cannot act without).
	if err := nc.Publish(messaging.IssueStateSubject("factory-2"), []byte(`{"status":"open"}`)); err != nil {
		t.Fatalf("Publish id-less: %v", err)
	}
	// The valid trailer — the only event that should reach the hub.
	valid, err := json.Marshal(core.IssueStateEvent{ID: "factory-3", Status: "closed"})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := nc.Publish(messaging.IssueStateSubject("factory-3"), valid); err != nil {
		t.Fatalf("Publish valid: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	var got core.IssueStateEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal: %v (data=%q)", err, ev.Data)
	}
	if got.ID != "factory-3" {
		t.Fatalf("first event id = %q, want factory-3 (malformed events should have been dropped)", got.ID)
	}
}

// TestIssueStatePumpStopUnsubscribes confirms the stop func detaches the subscription: after
// stop, a published transition no longer reaches the hub.
func TestIssueStatePumpStopUnsubscribes(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartIssueStatePump(nc, hub)
	if err != nil {
		t.Fatalf("StartIssueStatePump: %v", err)
	}
	stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	payload, err := json.Marshal(core.IssueStateEvent{ID: "factory-9", Status: "open"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := nc.Publish(messaging.IssueStateSubject("factory-9"), payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	select {
	case ev := <-ch:
		t.Fatalf("received event after stop: %+v", ev)
	default:
	}
}

// TestDLQPump proves the dead-letter -> hub bridge end to end over a real in-process server: a
// marshaled core.DLQAlert published on the durable factory.dlq subject arrives at a hub subscriber
// as a "dlq-arrival" SSE event carrying the original payload intact. Like the other pumps it is
// exercised against the actual transport. (The orchestrator publishes via JetStream; a plain core
// publish on the same subject is sufficient to drive the pump, which tails it with a core sub.)
func TestDLQPump(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartDLQPump(nc, hub)
	if err != nil {
		t.Fatalf("StartDLQPump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	want := core.DLQAlert{IssueID: "factory-7", Role: "implementor", Attempt: 3, Reason: "budget exhausted"}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal DLQAlert: %v", err)
	}
	if err := nc.Publish(messaging.SubjectDLQ, payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	if ev.Name != "dlq-arrival" {
		t.Fatalf("event name = %q, want dlq-arrival", ev.Name)
	}
	var got core.DLQAlert
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal DLQAlert: %v (data=%q)", err, ev.Data)
	}
	if got != want {
		t.Fatalf("alert = %+v, want %+v", got, want)
	}
}

// TestDLQPumpDropsMalformed confirms the pump's best-effort guards: a body that is not a
// well-formed alert (and an alert missing its issue id) is dropped rather than broadcast. The
// proof is ordering, exactly as for the issue-state pump: receiving the trailing valid alert first
// proves the bad ones never reached the hub.
func TestDLQPumpDropsMalformed(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartDLQPump(nc, hub)
	if err != nil {
		t.Fatalf("StartDLQPump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	// Not JSON at all — dropped.
	if err := nc.Publish(messaging.SubjectDLQ, []byte("not json")); err != nil {
		t.Fatalf("Publish malformed: %v", err)
	}
	// Well-formed JSON but no issue id — dropped (the id is what the operator acts on).
	if err := nc.Publish(messaging.SubjectDLQ, []byte(`{"reason":"orphan"}`)); err != nil {
		t.Fatalf("Publish id-less: %v", err)
	}
	// The valid trailer — the only alert that should reach the hub.
	valid, err := json.Marshal(core.DLQAlert{IssueID: "factory-3", Reason: "retries exhausted"})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := nc.Publish(messaging.SubjectDLQ, valid); err != nil {
		t.Fatalf("Publish valid: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	var got core.DLQAlert
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal: %v (data=%q)", err, ev.Data)
	}
	if got.IssueID != "factory-3" {
		t.Fatalf("first alert id = %q, want factory-3 (malformed alerts should have been dropped)", got.IssueID)
	}
}

// TestDLQPumpStopUnsubscribes confirms the stop func detaches the subscription: after stop, a
// published escalation no longer reaches the hub.
func TestDLQPumpStopUnsubscribes(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartDLQPump(nc, hub)
	if err != nil {
		t.Fatalf("StartDLQPump: %v", err)
	}
	stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	payload, err := json.Marshal(core.DLQAlert{IssueID: "factory-9", Reason: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := nc.Publish(messaging.SubjectDLQ, payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	select {
	case ev := <-ch:
		t.Fatalf("received event after stop: %+v", ev)
	default:
	}
}

// TestMergeStatePump proves the orchestrator->hub bridge end to end over a real in-process
// server: a marshaled core.MergeStateEvent published on a candidate's merge-state subject arrives
// at a hub subscriber as a "merge-state" SSE event carrying the original payload intact, and is
// also recorded into the merge-queue buffer the view reads. Like the other pumps it is exercised
// against the actual transport.
func TestMergeStatePump(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	mq := NewMergeQueue(10)
	stop, err := StartMergeStatePump(nc, hub, mq)
	if err != nil {
		t.Fatalf("StartMergeStatePump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	want := core.MergeStateEvent{
		ID:    "factory-7",
		State: core.MergeStateReGating,
		Role:  "integrate",
		Epic:  "factory-1",
		TS:    time.Unix(1700000000, 0).UTC(),
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal MergeStateEvent: %v", err)
	}
	if err := nc.Publish(messaging.MergeStateSubject(want.ID), payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	if ev.Name != "merge-state" {
		t.Fatalf("event name = %q, want merge-state", ev.Name)
	}
	var got core.MergeStateEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal MergeStateEvent: %v (data=%q)", err, ev.Data)
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}

	// The pump also fed the buffer the view reads.
	snap := mq.Snapshot()
	if len(snap) != 1 || snap[0].ID != want.ID || snap[0].State != want.State {
		t.Fatalf("buffer snapshot = %+v, want one factory-7 re-gating row", snap)
	}
}

// TestMergeStatePumpDropsMalformed confirms the pump's best-effort guards: a body that is not a
// well-formed event (and an event missing its id) is dropped rather than broadcast or buffered.
// The proof is ordering, exactly as for the issue-state pump: receiving the trailing valid event
// first proves the bad ones never reached the hub.
func TestMergeStatePumpDropsMalformed(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	mq := NewMergeQueue(10)
	stop, err := StartMergeStatePump(nc, hub, mq)
	if err != nil {
		t.Fatalf("StartMergeStatePump: %v", err)
	}
	defer stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	// Not JSON at all — dropped.
	if err := nc.Publish(messaging.MergeStateSubject("factory-1"), []byte("not json")); err != nil {
		t.Fatalf("Publish malformed: %v", err)
	}
	// Well-formed JSON but no id — dropped (the id is the one field the view cannot act without).
	if err := nc.Publish(messaging.MergeStateSubject("factory-2"), []byte(`{"state":"queued"}`)); err != nil {
		t.Fatalf("Publish id-less: %v", err)
	}
	// The valid trailer — the only event that should reach the hub.
	valid, err := json.Marshal(core.MergeStateEvent{ID: "factory-3", State: core.MergeStateLanded})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := nc.Publish(messaging.MergeStateSubject("factory-3"), valid); err != nil {
		t.Fatalf("Publish valid: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ev := recv(t, ch)
	var got core.MergeStateEvent
	if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
		t.Fatalf("unmarshal: %v (data=%q)", err, ev.Data)
	}
	if got.ID != "factory-3" {
		t.Fatalf("first event id = %q, want factory-3 (malformed events should have been dropped)", got.ID)
	}
	// Only the valid event reached the buffer too.
	if snap := mq.Snapshot(); len(snap) != 1 || snap[0].ID != "factory-3" {
		t.Fatalf("buffer = %+v, want only factory-3", snap)
	}
}

// TestMergeStatePumpStopUnsubscribes confirms the stop func detaches the subscription: after
// stop, a published step no longer reaches the hub.
func TestMergeStatePumpStopUnsubscribes(t *testing.T) {
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	hub := NewHub()
	stop, err := StartMergeStatePump(nc, hub, NewMergeQueue(10))
	if err != nil {
		t.Fatalf("StartMergeStatePump: %v", err)
	}
	stop()

	ch, cancel := hub.Subscribe()
	defer cancel()

	payload, err := json.Marshal(core.MergeStateEvent{ID: "factory-9", State: core.MergeStateQueued})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := nc.Publish(messaging.MergeStateSubject("factory-9"), payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	select {
	case ev := <-ch:
		t.Fatalf("received event after stop: %+v", ev)
	default:
	}
}
