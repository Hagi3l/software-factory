package live

import (
	"testing"
	"time"
)

// recv reads one event from ch within a short deadline, failing the test on timeout so
// a broken fan-out surfaces as a clear failure rather than a hang.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func TestHubBroadcastReachesSubscriber(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()

	if h.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after Subscribe", h.Len())
	}

	want := Event{Name: "agent-event", Data: `{"a":1}`}
	h.Broadcast(want)
	if got := recv(t, ch); got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

func TestHubBroadcastFansOutToAll(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Broadcast(Event{Data: "x"})
	if got := recv(t, ch1); got.Data != "x" {
		t.Fatalf("sub1 got %q", got.Data)
	}
	if got := recv(t, ch2); got.Data != "x" {
		t.Fatalf("sub2 got %q", got.Data)
	}
}

func TestHubCancelUnsubscribes(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()

	if h.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after cancel", h.Len())
	}
	// The channel is closed, so a receive returns the zero value with ok=false rather
	// than blocking; a subsequent Broadcast must not deliver to it (or panic).
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
	h.Broadcast(Event{Data: "after-cancel"}) // must not panic on the removed subscriber
}

func TestHubCancelIsIdempotent(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe()
	cancel()
	cancel() // second call must be a no-op, not a double close
}

// TestHubBroadcastNonBlocking proves a wedged subscriber (one that never reads) cannot
// stall Broadcast: once its buffer fills, further events are dropped for it alone. If
// Broadcast blocked, this test would hang and fail the package.
func TestHubBroadcastNonBlocking(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe() // never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < h.buf*4; i++ {
			h.Broadcast(Event{Data: "flood"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a full subscriber buffer")
	}
}
