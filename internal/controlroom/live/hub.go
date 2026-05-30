// Package live is the control room's SSE substrate (specs/control-room.md "Live
// updates", T4.3): a small, content-agnostic pipe that fans NATS events out to many
// browsers over Server-Sent Events. It is the live-update foundation the Board (T4.4)
// and Activity feed (T4.5) attach to — those views decide *what* to render; this
// package only decides *how* events travel: one upstream stream, many SSE clients.
//
// The split is deliberate. The Hub knows nothing about HTTP or NATS, so it is trivial
// to unit-test; Stream knows only the SSE wire format; the pump (pump.go) is the sole
// piece that touches NATS. A view layered on top never reaches past the Hub.
package live

import "sync"

// Event is one Server-Sent Event. Name maps to the SSE `event:` field, which the htmx
// SSE extension dispatches on (`sse-swap="<name>"` / `hx-trigger="sse:<name>"`); an
// empty Name yields the default "message" event. Data maps to the `data:` field and is
// opaque to this package — a pre-rendered HTML fragment to swap, or a JSON datum a view
// re-renders from is equally fine.
type Event struct {
	Name string
	Data string
}

// subscriber is one connected SSE client's mailbox. The channel is buffered so a brief
// browser stall does not immediately drop events, and Broadcast never blocks on it.
type subscriber struct {
	ch chan Event
}

// Hub fans a single stream of Events out to every connected SSE client. It is the
// pub/sub seam between the one NATS subscription (the pump) and the N browser
// connections (the SSE handler): subscribe on connect, broadcast on each upstream
// event, unsubscribe on disconnect. Safe for concurrent use.
type Hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	buf  int
}

// NewHub returns an empty Hub. The per-subscriber buffer (32) absorbs a transient
// browser stall; past it, Broadcast drops rather than blocks (see Broadcast).
func NewHub() *Hub {
	return &Hub{subs: make(map[*subscriber]struct{}), buf: 32}
}

// Subscribe registers a new SSE client and returns its event channel plus a cancel
// func that unregisters it and closes the channel. The caller must call cancel exactly
// once it is done reading (deferring it in the HTTP handler is the intended use);
// cancel is idempotent. Events that arrive after cancel are not delivered.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, h.buf)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			// Safe to close after the delete: Broadcast holds h.mu for the whole
			// iteration, so it can never be mid-send on a subscriber we have already
			// removed from the map.
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// Broadcast delivers ev to every current subscriber without blocking: if a
// subscriber's buffer is full (a browser that has stalled or wedged), its copy is
// dropped rather than stalling the pump and every other client. This mirrors the
// best-effort, fire-and-forget nature of the core-NATS agent events feeding it —
// losing a live event is harmless (specs/messaging.md). Slow consumers degrade only
// their own stream.
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
		}
	}
}

// Len reports the number of connected subscribers. Used by the wiring (and tests) to
// observe connect/disconnect; not part of the data path.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
