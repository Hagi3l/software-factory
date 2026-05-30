package controlroom

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/live"
)

// waitFor polls cond until it holds or the deadline passes, failing with msg otherwise.
// Used to synchronize on the asynchronous connect/disconnect of an SSE client without a
// fixed sleep.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}

// TestEventsStreamsBroadcasts is the substrate's end-to-end contract: a browser GET to
// /events opens an SSE stream, an event broadcast on the hub is written to that stream
// as a proper SSE frame, and disconnecting the client unsubscribes it from the hub.
func TestEventsStreamsBroadcasts(t *testing.T) {
	hub := live.NewHub()
	s := New(Options{Version: "test", Events: hub})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// The handler subscribes asynchronously; wait until it has before broadcasting so
	// the event cannot be missed.
	waitFor(t, func() bool { return hub.Len() == 1 }, "client did not subscribe")

	// Read the stream on a goroutine; collect lines until the frame's blank terminator.
	frames := make(chan string, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			if line == "\n" && b.Len() > 0 { // blank line ends an SSE frame
				frames <- b.String()
				return
			}
			b.WriteString(line)
			if err != nil {
				return
			}
		}
	}()

	hub.Broadcast(live.Event{Name: "agent-event", Data: `{"agentId":"inv-7"}`})

	select {
	case frame := <-frames:
		if !strings.Contains(frame, "event: agent-event") {
			t.Errorf("frame missing event line: %q", frame)
		}
		if !strings.Contains(frame, `data: {"agentId":"inv-7"}`) {
			t.Errorf("frame missing data line: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive an SSE frame")
	}

	// Disconnecting the client must unsubscribe it from the hub.
	cancel()
	_ = resp.Body.Close()
	waitFor(t, func() bool { return hub.Len() == 0 }, "client did not unsubscribe on disconnect")
}

// TestEventsUnavailableWithoutHub confirms the standalone path: with no hub wired (a
// plain `harness serve`, no running factory) the endpoint answers 503 instead of
// holding open a stream that would never emit.
func TestEventsUnavailableWithoutHub(t *testing.T) {
	ts := newTestServer(t) // built without Options.Events
	r := get(t, ts, "/events")
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", r.status)
	}
}
