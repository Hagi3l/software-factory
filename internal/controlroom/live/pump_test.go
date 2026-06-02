package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
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

	payload := `{"type":"token","delta":"hi"}`
	if err := nc.Publish(messaging.AgentEventsSubject("inv-1"), []byte(payload)); err != nil {
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
	if got.AgentID != "inv-1" {
		t.Fatalf("AgentID = %q, want inv-1", got.AgentID)
	}
	if string(got.Payload) != payload {
		t.Fatalf("Payload = %q, want %q", string(got.Payload), payload)
	}

	// The same event is recorded into the activity buffer, labeled with the agent id
	// recovered from the subject — so the pump feeds both the live nudge (hub) and the
	// rendered feed (buffer) from one subscription.
	rec := act.Recent()
	if len(rec) != 1 {
		t.Fatalf("activity entries = %d, want 1", len(rec))
	}
	if rec[0].AgentID != "inv-1" || rec[0].Kind != "token" || rec[0].Detail != "hi" {
		t.Fatalf("activity entry = %+v, want inv-1 token 'hi'", rec[0])
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
		ID:     "harness-7",
		Status: "in_progress",
		Role:   "implementor",
		Epic:   "harness-1",
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
	if err := nc.Publish(messaging.IssueStateSubject("harness-1"), []byte("not json")); err != nil {
		t.Fatalf("Publish malformed: %v", err)
	}
	// Well-formed JSON but no id — dropped (the id is the one field a consumer cannot act without).
	if err := nc.Publish(messaging.IssueStateSubject("harness-2"), []byte(`{"status":"open"}`)); err != nil {
		t.Fatalf("Publish id-less: %v", err)
	}
	// The valid trailer — the only event that should reach the hub.
	valid, err := json.Marshal(core.IssueStateEvent{ID: "harness-3", Status: "closed"})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := nc.Publish(messaging.IssueStateSubject("harness-3"), valid); err != nil {
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
	if got.ID != "harness-3" {
		t.Fatalf("first event id = %q, want harness-3 (malformed events should have been dropped)", got.ID)
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

	payload, err := json.Marshal(core.IssueStateEvent{ID: "harness-9", Status: "open"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := nc.Publish(messaging.IssueStateSubject("harness-9"), payload); err != nil {
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
