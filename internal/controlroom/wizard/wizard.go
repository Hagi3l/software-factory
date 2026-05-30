// Package wizard is the trusted, non-sandboxed requirements planner behind the
// control-room Create-Task wizard (T4.12, specs/control-room.md, specs/workflow.md). It is
// the one place a human is in the loop: a steered conversation that converges on aligned,
// testable intent before any autonomous work is seeded.
//
// Why it is its own package, separate from the sandboxed agent loop: the requirements
// planner "runs no untrusted code — it converses and writes specs + seed issues"
// (workflow.md), so it is correctly *outside* the sandbox and does NOT go through the
// runner/broker. It reuses the canonical model layer (model.Adapter, model.Request) DIRECTLY
// — the same abstraction the brokered agent loop calls, but trusted-side, with no sandbox,
// no tool dispatch, and no candidate branch. That is the whole distinction between the two
// planners the specs are careful not to conflate (the *decomposition* planner is the
// autonomous, sandboxed `plan` stage; this is the interactive, trusted requirements stage).
//
// The package owns only the conversation: session state and the streaming model turn. It
// streams over the control room's SSE substrate (internal/controlroom/live) so the partial
// reply appears token-by-token in the browser. It deliberately does NOT author specs or
// create seed issues — that is the consent-gated T4.14 work; keeping it out here is what
// makes the conversation loop a self-contained, testable unit.
package wizard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/model"
)

// SSE event names the wizard broadcasts on a session's hub. The view binds to both: a
// `delta` event carries the growing assistant reply (the live "typing" stream); a `turn`
// event is a nudge that the reply is complete, so the transcript re-fetches and finalizes it
// (the server-render-a-fragment + htmx re-fetch pattern the other live views use).
const (
	eventDelta = "delta"
	eventTurn  = "turn"
	// eventLedger nudges the alignment-ledger panel to re-fetch (T4.13). It fires only on a
	// turn that emitted a ledger snapshot, so the panel refreshes exactly when the planner
	// updates the ledger (the panel also has a slow periodic backstop in the view).
	eventLedger = "ledger"
)

const (
	// defaultMaxSessions bounds the in-memory session map. Sessions are best-effort working
	// state, not a durable record (the durable transcript is written on APPROVE in T4.14), so
	// the oldest is evicted past this cap rather than growing unbounded across a long-lived
	// control room.
	defaultMaxSessions = 64
	// defaultTurnTimeout bounds one model reply so a wedged provider cannot leak a goroutine
	// forever. The turn runs on a background context (it outlives the POST that started it),
	// so this ceiling is its only guaranteed bound.
	defaultTurnTimeout = 5 * time.Minute
	// deltaFlushRunes coalesces the per-token stream: the cumulative reply is re-broadcast
	// only after it grows by at least this many runes, collapsing a token firehose into a
	// few DOM swaps per reply while staying live. The final `turn` re-fetch always shows the
	// exact full text, so a coalesced (slightly behind) live stream is harmless.
	deltaFlushRunes = 12
)

// Message is one presentation-layer turn of the conversation — the view renders these
// directly, so it is flattened to strings and carries no model types. Role is "user" or
// "assistant" (the persona rides in the request's system channel, never shown).
type Message struct {
	Role string
	Text string
}

// Planner is the requirements planner: it holds the resolved model adapter + persona and
// manages in-memory conversation sessions. One Planner serves the whole control room; each
// human conversation is a Session. Safe for concurrent use.
type Planner struct {
	adapter     model.Adapter
	persona     string
	maxTokens   int
	turnTimeout time.Duration
	log         *slog.Logger

	mu          sync.Mutex
	sessions    map[string]*Session
	order       []string // session ids in creation order, for bounded eviction
	maxSessions int
}

// Option configures a Planner.
type Option func(*Planner)

// WithMaxTokens sets the per-turn output ceiling (0 = the adapter default).
func WithMaxTokens(n int) Option {
	return func(p *Planner) {
		if n > 0 {
			p.maxTokens = n
		}
	}
}

// WithLogger sets the logger (defaults to a discarding logger).
func WithLogger(l *slog.Logger) Option {
	return func(p *Planner) {
		if l != nil {
			p.log = l
		}
	}
}

// WithMaxSessions overrides the session cap (mainly for tests).
func WithMaxSessions(n int) Option {
	return func(p *Planner) {
		if n > 0 {
			p.maxSessions = n
		}
	}
}

// WithTurnTimeout overrides the per-turn timeout (mainly for tests).
func WithTurnTimeout(d time.Duration) Option {
	return func(p *Planner) {
		if d > 0 {
			p.turnTimeout = d
		}
	}
}

// NewPlanner builds a requirements planner over a resolved model adapter and persona text.
// The composition root resolves the configured model to an adapter (via the infra registry)
// and reads the persona file, so this package depends on neither config nor the filesystem —
// only the canonical model layer.
func NewPlanner(adapter model.Adapter, persona string, opts ...Option) *Planner {
	p := &Planner{
		adapter:     adapter,
		persona:     persona,
		turnTimeout: defaultTurnTimeout,
		log:         slog.New(slog.NewTextHandler(discard{}, nil)),
		sessions:    make(map[string]*Session),
		maxSessions: defaultMaxSessions,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// New creates a fresh, empty conversation session and registers it. When the session cap is
// reached the oldest session is evicted (best-effort working state — see defaultMaxSessions).
func (p *Planner) New() *Session {
	s := &Session{
		ID:          newID(),
		hub:         live.NewHub(),
		adapter:     p.adapter,
		persona:     p.persona,
		maxTokens:   p.maxTokens,
		turnTimeout: p.turnTimeout,
		log:         p.log,
	}
	p.mu.Lock()
	for len(p.order) >= p.maxSessions {
		oldest := p.order[0]
		p.order = p.order[1:]
		delete(p.sessions, oldest)
	}
	p.sessions[s.ID] = s
	p.order = append(p.order, s.ID)
	p.mu.Unlock()
	return s
}

// Get returns the session with the given id, or nil if it is unknown (never created, or
// evicted). A nil return is how the server answers a stale session id.
func (p *Planner) Get(id string) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[id]
}

// Session is one human's conversation with the requirements planner: the running message
// history plus its own SSE hub the browser streams from. Safe for concurrent use; at most
// one reply turn runs at a time (Send rejects a second while one is in flight).
type Session struct {
	ID          string
	hub         *live.Hub
	adapter     model.Adapter
	persona     string
	maxTokens   int
	turnTimeout time.Duration
	log         *slog.Logger

	mu       sync.Mutex
	messages []model.Message
	busy     bool
	ledger   []LedgerItem // latest-wins alignment-ledger snapshot (T4.13), guarded by mu
}

// Hub is the session's SSE hub — the control-room stream handler subscribes to it and the
// browser receives the live `delta`/`turn` events for this conversation only (each session
// has its own hub, so one human's stream never leaks to another).
func (s *Session) Hub() *live.Hub { return s.hub }

// Messages returns a snapshot of the conversation as presentation turns (user/assistant
// only; the persona system prompt is never surfaced). The returned slice is a copy, safe to
// render without holding the lock.
func (s *Session) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.messages))
	for _, m := range s.messages {
		switch m.Role {
		case model.RoleUser:
			out = append(out, Message{Role: "user", Text: m.Text})
		case model.RoleAssistant:
			out = append(out, Message{Role: "assistant", Text: m.Text})
		}
	}
	return out
}

// Ledger returns a copy of the latest alignment-ledger snapshot (T4.13) the planner emitted
// — the items the view renders beside the conversation. The copy is safe to render without
// holding the lock; it is empty until the planner emits its first ```ledger block.
func (s *Session) Ledger() []LedgerItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ledger)
}

// Choose funnels a chip click back through the conversation: it reads the option the human
// picked from the latest ledger and Sends a canned message naming the question and choice, so
// the planner re-emits the ledger reflecting the decision (the planner stays the single source
// of truth — there is no client-side ledger mutation). Out-of-range indices are a no-op (the
// ledger may have moved on since the page was rendered) and return "". On success it returns
// the message it sent.
func (s *Session) Choose(itemIdx, optIdx int) string {
	s.mu.Lock()
	if itemIdx < 0 || itemIdx >= len(s.ledger) {
		s.mu.Unlock()
		return ""
	}
	it := s.ledger[itemIdx]
	if optIdx < 0 || optIdx >= len(it.Options) {
		s.mu.Unlock()
		return ""
	}
	question := it.Question
	label := it.Options[optIdx].Label
	s.mu.Unlock() // release before Send, which takes the lock itself

	msg := fmt.Sprintf("For %q, I choose: %s.", question, label)
	s.Send(msg)
	return msg
}

// Busy reports whether a reply turn is currently in flight.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// Send records the human's message and starts streaming the planner's reply in the
// background, returning immediately. It returns started=false (recording nothing) when the
// message is blank or a reply is already in flight, so the caller can leave the transcript
// unchanged. The reply streams to the session hub as coalesced `delta` events and a final
// `turn` nudge; the background turn outlives the request that started it (a chat reply takes
// many seconds), bounded by the per-turn timeout.
func (s *Session) Send(userText string) (started bool) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return false
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return false
	}
	s.busy = true
	s.messages = append(s.messages, model.Message{Role: model.RoleUser, Text: userText})
	s.mu.Unlock()

	go s.run()
	return true
}

// run executes one model reply turn: a single trusted adapter.Complete (no broker, no
// sandbox, no tools — pure conversation), streaming the growing reply to the hub. It always
// clears busy and broadcasts the terminal `turn` nudge, even on error, so a failed turn
// never wedges the session and the transcript still refreshes (rendering the error note).
func (s *Session) run() {
	ctx, cancel := context.WithTimeout(context.Background(), s.turnTimeout)
	defer cancel()

	s.mu.Lock()
	msgs := slices.Clone(s.messages)
	s.mu.Unlock()

	// Accumulate the reply and re-broadcast the cumulative (HTML-escaped) text as it grows,
	// coalesced by deltaFlushRunes. Cumulative (not incremental) so a dropped SSE frame
	// self-heals on the next one — matching the hub's best-effort drop semantics.
	var b strings.Builder
	lastLen := 0
	onEvent := func(ev model.StreamEvent) {
		if ev.TextDelta == "" {
			return
		}
		b.WriteString(ev.TextDelta)
		if n := utf8.RuneCountInString(b.String()); n-lastLen >= deltaFlushRunes {
			lastLen = n
			// Stream only the prose: suppress the trailing ```ledger JSON block so its raw text
			// never flashes in the live stream as it accumulates (the panel renders it instead).
			s.hub.Broadcast(live.Event{Name: eventDelta, Data: html.EscapeString(displayProse(b.String()))})
		}
	}

	resp, err := s.adapter.Complete(ctx, model.Request{
		System:    s.persona,
		Messages:  msgs,
		MaxTokens: s.maxTokens,
	}, onEvent)

	reply := resp.Text
	if err != nil {
		s.log.Error("wizard: model turn failed", "session", s.ID, "err", err)
		reply = fmt.Sprintf("The requirements planner hit an error and could not reply: %v\n\nPlease try again.", err)
	}

	// Split the trailing ```ledger block off the reply: the transcript records only the prose,
	// and the parsed items become the new latest-wins ledger snapshot. A ledger-less turn (no
	// block, or a malformed one) leaves the prior ledger untouched.
	items, prose := parseLedger(reply)

	s.mu.Lock()
	s.messages = append(s.messages, model.Message{Role: model.RoleAssistant, Text: prose})
	if items != nil {
		s.ledger = items
	}
	s.busy = false
	s.mu.Unlock()

	// Terminal nudge: the transcript re-fetches and renders the finalized reply, which also
	// resets the live delta target. Emitted last so the refetch sees the appended message.
	s.hub.Broadcast(live.Event{Name: eventTurn, Data: ""})
	if items != nil {
		s.hub.Broadcast(live.Event{Name: eventLedger, Data: ""})
	}
}

// newID returns an unguessable session id. Crypto-random rather than a counter so a session
// id is not enumerable across humans even before the control room has auth (auth is OPEN in
// control-room.md). Falls back to nothing fancy — rand.Read on a healthy host never fails.
func newID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// discard is the default logger sink when no logger is supplied.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
