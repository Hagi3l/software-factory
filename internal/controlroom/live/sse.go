package live

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrStreamingUnsupported is returned by Stream when the ResponseWriter cannot flush —
// SSE needs each frame pushed to the client immediately, so a buffered writer that
// never flushes would deliver nothing.
var ErrStreamingUnsupported = errors.New("live: response writer does not support streaming")

// WriteEvent encodes one Event as an SSE frame. A non-empty Name becomes an `event:`
// line; the data is emitted as one `data:` line per line of Event.Data, because the SSE
// wire format forbids a raw newline inside a field — splitting keeps multi-line
// payloads (e.g. a JSON object, or an HTML fragment) intact across the wire. The frame
// ends with the blank line that terminates an event.
func WriteEvent(w io.Writer, ev Event) error {
	var b strings.Builder
	if ev.Name != "" {
		b.WriteString("event: ")
		b.WriteString(ev.Name)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// Stream pumps events from a subscriber channel to an SSE client until ctx is canceled
// (client disconnect or server shutdown — the handler derives r.Context() from both),
// the channel closes, or a write fails. It returns nil on a clean stop and the write
// error otherwise.
//
// When heartbeat > 0 an idle connection gets a periodic SSE comment line, which keeps
// intermediaries (proxies, load balancers) from reaping the connection and surfaces a
// dead client promptly as a write error rather than a silently wedged goroutine. The
// caller must have set the SSE response headers before calling Stream; the first Flush
// here commits the 200 + headers so the browser's EventSource opens before any event.
func Stream(ctx context.Context, w http.ResponseWriter, events <-chan Event, heartbeat time.Duration) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return ErrStreamingUnsupported
	}
	flusher.Flush()

	var tick <-chan time.Time
	if heartbeat > 0 {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := WriteEvent(w, ev); err != nil {
				return err
			}
			flusher.Flush()
		case <-tick:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}
