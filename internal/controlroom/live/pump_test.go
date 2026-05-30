package live

import (
	"encoding/json"
	"testing"

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
	stop, err := StartAgentEventPump(nc, hub)
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
	stop, err := StartAgentEventPump(nc, hub)
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
