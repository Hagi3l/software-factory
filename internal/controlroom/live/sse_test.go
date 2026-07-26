package live

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteEvent(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{
			name: "default event, single line",
			ev:   Event{Data: "hello"},
			want: "data: hello\n\n",
		},
		{
			name: "named event",
			ev:   Event{Name: "agent-event", Data: "hello"},
			want: "event: agent-event\ndata: hello\n\n",
		},
		{
			name: "multi-line data splits into data: lines",
			ev:   Event{Name: "e", Data: "a\nb"},
			want: "event: e\ndata: a\ndata: b\n\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := WriteEvent(&b, c.ev); err != nil {
				t.Fatalf("WriteEvent: %v", err)
			}
			if b.String() != c.want {
				t.Fatalf("frame = %q, want %q", b.String(), c.want)
			}
		})
	}
}

func TestStreamWritesEventsThenStopsOnChannelClose(t *testing.T) {
	rec := httptest.NewRecorder()
	events := make(chan Event, 2)
	events <- Event{Name: "agent-event", Data: "one"}
	events <- Event{Data: "two"}
	close(events) // Stream returns nil when the channel closes

	if err := Stream(context.Background(), rec, events, 0); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	want := "event: agent-event\ndata: one\n\ndata: two\n\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
	if !rec.Flushed {
		t.Fatal("Stream did not flush")
	}
}

func TestStreamStopsOnContextCancel(t *testing.T) {
	rec := httptest.NewRecorder()
	events := make(chan Event)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Stream(ctx, rec, events, 0) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned %v, want nil on cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after context cancel")
	}
}

func TestStreamHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	events := make(chan Event)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { _ = Stream(ctx, rec, events, 5*time.Millisecond); close(done) }()

	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done

	if !strings.Contains(rec.Body.String(), ": ping\n\n") {
		t.Fatalf("expected a heartbeat comment, got %q", rec.Body.String())
	}
}

// nonFlushWriter is an http.ResponseWriter that cannot flush, to prove Stream refuses a
// writer that would buffer the live feed.
type nonFlushWriter struct{ h http.Header }

func (n *nonFlushWriter) Header() http.Header         { return n.h }
func (n *nonFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlushWriter) WriteHeader(int)             {}

func TestStreamRequiresFlusher(t *testing.T) {
	w := &nonFlushWriter{h: http.Header{}}
	if err := Stream(context.Background(), w, make(chan Event), 0); err != ErrStreamingUnsupported {
		t.Fatalf("err = %v, want ErrStreamingUnsupported", err)
	}
}
