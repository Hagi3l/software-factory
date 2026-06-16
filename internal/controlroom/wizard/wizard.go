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
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
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
	// eventDraft nudges the draft panel to re-fetch (T4.14). It fires only on a turn that
	// emitted a ```draft block, so the proposed spec + seed issues (and the APPROVE button)
	// appear exactly when the planner has something to propose.
	eventDraft = "draft"
	// eventTool is a transient status line emitted while the planner explores the codebase
	// (T4.28): each read tool call broadcasts a humanized label ("read_file foo.go") so the
	// human sees activity during a multi-step exploration turn instead of a frozen spinner.
	// The view shows it as an ephemeral strip, cleared on the terminal eventTurn.
	eventTool = "tool"
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
	// defaultMaxToolTurns bounds the read-only exploration round-trips within ONE human turn
	// (T4.28): the model may call tools, see results, and call again, but only this many times
	// before the turn must conclude with a prose reply. It is a termination guarantee against a
	// model that loops on tool calls forever; the per-turn timeout bounds wall-clock independently.
	// It is the default; the requirements_planner config may raise it (WithMaxToolTurns) for a
	// model that should explore a large codebase deeply.
	defaultMaxToolTurns = 16
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
	adapter      model.Adapter
	persona      string
	maxTokens    int
	maxToolTurns int
	turnTimeout  time.Duration
	log          *slog.Logger
	sandboxCfg   *sandboxConfig // read-only exploration template (T4.28); nil = tools disabled
	projectIndex string         // specs/README.md content for Create grounding (T4.28); "" = none

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

// WithMaxToolTurns sets the cap on read-only exploration round-trips within one human turn
// (0 = defaultMaxToolTurns). Raising it lets the planner read more of a large codebase before it
// must conclude with a prose reply; the per-turn timeout still bounds wall-clock independently.
func WithMaxToolTurns(n int) Option {
	return func(p *Planner) {
		if n > 0 {
			p.maxToolTurns = n
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

// WithSandbox enables read-only codebase exploration (T4.28): each Create session may provision
// a fresh, read-only, zero-network sandbox seeded from repo at baseRef and give the planner the
// agent's read tools over it, so it grounds specs + seed issues in the real code. backend/limits
// come straight from the resolved infra config; image is the concrete artifact ResolveImage
// produced for profile; sockDir is where the per-session deny-all broker socket is bound. Absent
// this option a session has no tools and behaves exactly as a pure-conversation planner.
func WithSandbox(backend sandbox.Backend, repo, profile, image, baseRef string, limits config.SandboxLimits, sockDir string) Option {
	return func(p *Planner) {
		if backend == nil || profile == "" {
			return
		}
		p.sandboxCfg = &sandboxConfig{
			backend: backend,
			repo:    repo,
			profile: profile,
			image:   image,
			baseRef: baseRef,
			limits:  limits,
			sockDir: sockDir,
			// log is filled from the planner's final logger when New() copies this template
			// onto a session (option order does not guarantee p.log is set yet here).
		}
	}
}

// maxProjectIndexRunes caps the spec index folded into the system prompt + greeting, so a large
// README never bloats every turn's context. The index is short by design (it is an index); this
// is a guard, not an expected limit.
const maxProjectIndexRunes = 8000

// WithProjectIndex grounds new Create sessions in the project's spec index (specs/README.md),
// read host-side by the composition root and passed in here (the wizard package itself does no
// filesystem I/O). When non-empty, each Create session opens with an orientation message in the
// transcript and folds the index into the planner's system prompt, so its very first reply is
// grounded in what already exists rather than assuming a blank slate. Empty (no specs/README.md)
// leaves the wizard a blank-slate conversation exactly as before. Resolve sessions are unaffected
// (they already carry their own escalation grounding).
func WithProjectIndex(readme string) Option {
	return func(p *Planner) { p.projectIndex = logSnippet(readme, maxProjectIndexRunes) }
}

// NewPlanner builds a requirements planner over a resolved model adapter and persona text.
// The composition root resolves the configured model to an adapter (via the infra registry),
// reads the persona file, and (for T4.28) reads specs/README.md, passing each in as text — so
// this package itself does no filesystem I/O. It references config types only as plain data
// (SandboxLimits on the exploration template); the canonical model layer remains its core.
func NewPlanner(adapter model.Adapter, persona string, opts ...Option) *Planner {
	p := &Planner{
		adapter:      adapter,
		persona:      persona,
		maxToolTurns: defaultMaxToolTurns,
		turnTimeout:  defaultTurnTimeout,
		log:          slog.New(slog.NewTextHandler(discard{}, nil)),
		sessions:     make(map[string]*Session),
		maxSessions:  defaultMaxSessions,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// New creates a fresh, empty conversation session and registers it. When the session cap is
// reached the oldest session is evicted (best-effort working state — see defaultMaxSessions).
func (p *Planner) New() *Session {
	persona := p.persona
	var opening []model.Message
	// When grounded in a project's spec index, fold it into the system prompt (so the first reply
	// is oriented) and seed an opening assistant message (so the human lands on a note about what
	// is going on, not a blank chat). Both gate on the index being present, so an ungrounded
	// wizard is byte-for-byte the prior blank-slate conversation.
	if p.projectIndex != "" {
		persona += projectGrounding(p.projectIndex)
		opening = []model.Message{{Role: model.RoleAssistant, Text: createGreeting(p.sandboxCfg != nil)}}
	}
	return p.register(&Session{
		ID:          newID(),
		hub:         live.NewHub(),
		adapter:      p.adapter,
		persona:      persona,
		maxTokens:    p.maxTokens,
		maxToolTurns: p.maxToolTurns,
		turnTimeout:  p.turnTimeout,
		log:          p.log,
		sandboxCfg:   p.sessionSandboxCfg(),
		messages:     opening,
	})
}

// projectGrounding folds the project's spec index into the system prompt so the planner starts a
// Create session oriented in the existing project. It rides the system channel (background
// context), so the transcript stays the human↔planner conversation while the model still reasons
// against the index.
func projectGrounding(index string) string {
	var b strings.Builder
	b.WriteString("\n\n## Project context (read at session start)\n\n")
	b.WriteString("You are working in an EXISTING project, not a blank slate. Its spec index — ")
	b.WriteString("`specs/README.md` — is reproduced below so you start oriented in what already ")
	b.WriteString("exists. Use it to ground your questions and your draft; follow its links with ")
	b.WriteString("your exploration tools when relevant. Do not assume the human has read it.\n\n")
	b.WriteString("----- specs/README.md -----\n")
	b.WriteString(index)
	if !strings.HasSuffix(index, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("----- end specs/README.md -----\n")
	return b.String()
}

// createGreeting is the orientation message seeded into a grounded Create session's transcript,
// so the human opens to a note on what is going on (the planner has read the project's spec index,
// and — when exploration is enabled — can read the codebase as they go) rather than a blank chat.
func createGreeting(canExplore bool) string {
	var b strings.Builder
	b.WriteString("Hi — I'm the requirements planner. I've reviewed this project's spec index ")
	b.WriteString("(`specs/README.md`) to ground myself in what already exists")
	if canExplore {
		b.WriteString(", and I can read the codebase as we go")
	}
	b.WriteString(".\n\nTell me what you'd like built. I'll probe for examples, edge cases, what to ")
	b.WriteString("reject, and what's out of scope — then draft the spec and seed issues for your ")
	b.WriteString("approval. Nothing is written until you approve.")
	return b.String()
}

// sessionSandboxCfg returns the per-session copy of the exploration template (T4.28) with the
// planner's resolved logger filled in, or nil when exploration is disabled. Returning a copy
// keeps each session's config independent and lets the logger be resolved after option order.
func (p *Planner) sessionSandboxCfg() *sandboxConfig {
	if p.sandboxCfg == nil {
		return nil
	}
	c := *p.sandboxCfg
	c.log = p.log
	return &c
}

// register installs a freshly built session in the bounded map (evicting the oldest past the
// cap) and returns it. Shared by New (a blank Create session) and NewResolve (a session
// pre-grounded in a dead-lettered issue), so both obey the same eviction discipline.
func (p *Planner) register(s *Session) *Session {
	p.mu.Lock()
	for len(p.order) >= p.maxSessions {
		oldest := p.order[0]
		p.order = p.order[1:]
		evicted := p.sessions[oldest]
		delete(p.sessions, oldest)
		// Tear down the evicted session's exploration sandbox off the lock (teardown does
		// Docker work and must not block registration). Safe against an in-flight turn: the
		// running turn captured its *explorer locally, and Teardown mid-Exec is contractual.
		if evicted != nil {
			go evicted.teardownExplorer()
		}
	}
	p.sessions[s.ID] = s
	p.order = append(p.order, s.ID)
	p.mu.Unlock()
	return s
}

// Shutdown tears down every live session's exploration sandbox, draining the planner. The
// composition root defers it so the host does not leak containers when the control room stops.
// It is best-effort and bounded per sandbox by teardownTimeout; ctx only bounds the overall
// drain. After Shutdown the planner holds no sessions.
func (p *Planner) Shutdown(ctx context.Context) {
	p.mu.Lock()
	sessions := make([]*Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.sessions = make(map[string]*Session)
	p.order = nil
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, s := range sessions {
			wg.Add(1)
			go func(s *Session) { defer wg.Done(); s.teardownExplorer() }(s)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
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
	maxTokens    int
	maxToolTurns int
	turnTimeout  time.Duration
	log          *slog.Logger

	// issueID is the dead-lettered issue a Resolve-mode session is unsticking (empty for a
	// blank Create session). It is set server-side at mint by NewResolve and read back by the
	// resolve consent gate, so the browser approves "resolve session S" and the server commits
	// against the issue *it* bound — never an issue id the browser supplies (T4.15).
	issueID string

	// sandboxCfg is the read-only exploration template (T4.28), copied from the planner at
	// mint; nil disables tools entirely. provMu serializes the (costly) lazy provision so a
	// Docker boot never blocks rendering reads that take s.mu. explorer is the live read-only
	// stack, built on first tool call and reused across turns, guarded by s.mu.
	sandboxCfg *sandboxConfig
	provMu     sync.Mutex
	explorer   *explorer

	mu          sync.Mutex
	messages    []model.Message
	busy        bool
	turnStarted time.Time    // when the in-flight turn began (set in Send); anchors the live elapsed timer
	ledger      []LedgerItem // latest-wins alignment-ledger snapshot (T4.13), guarded by mu
	draft       Draft        // latest-wins drafted spec + seed issues (T4.14), guarded by mu

	// readCount counts the read tool calls executed within the current turn, used only to label
	// the live activity line ("reading the codebase · 3 read"). Touched solely by the turn
	// goroutine (reset in run, incremented in dispatch), so it needs no lock.
	readCount int
}

// ensureExplorer returns the session's live exploration stack, provisioning it lazily on first
// use. It returns (nil, nil) when exploration is disabled (no sandboxCfg) — the caller then
// advertises no tools. The costly Provision runs under provMu (NOT s.mu) so it never blocks
// Messages()/Ledger() rendering; at most one turn runs per session (the busy flag), so this is
// only racing a concurrent teardown, which the re-check under s.mu handles.
func (s *Session) ensureExplorer(ctx context.Context) (*explorer, error) {
	s.mu.Lock()
	cfg := s.sandboxCfg
	exp := s.explorer
	s.mu.Unlock()
	if cfg == nil {
		return nil, nil
	}
	if exp != nil {
		return exp, nil
	}

	s.provMu.Lock()
	defer s.provMu.Unlock()
	// Re-check: a turn may have provisioned (or a teardown nilled) while we waited on provMu.
	s.mu.Lock()
	exp = s.explorer
	s.mu.Unlock()
	if exp != nil {
		return exp, nil
	}

	// Provisioning boots a Docker sandbox (clone + run + copy), which takes real seconds and
	// happens before any tool can run — so surface it on both channels: a UI status line (the
	// dots would otherwise sit frozen) and an info log (the terminal would otherwise be silent).
	s.hub.Broadcast(live.Event{Name: eventTool, Data: html.EscapeString("preparing the codebase sandbox…")})
	s.log.Info("wizard: provisioning exploration sandbox", "session", s.ID, "profile", cfg.profile, "base_ref", cfg.baseRef)
	start := time.Now()
	built, err := buildExplorer(ctx, *cfg)
	if err != nil {
		s.log.Error("wizard: exploration sandbox provisioning failed", "session", s.ID, "elapsed", time.Since(start).String(), "err", err)
		return nil, err
	}
	s.log.Info("wizard: exploration sandbox ready", "session", s.ID, "elapsed", time.Since(start).String())
	s.mu.Lock()
	s.explorer = built
	s.mu.Unlock()
	return built, nil
}

// teardownExplorer tears down the session's exploration sandbox if one was provisioned, exactly
// once. It swaps the field to nil under s.mu and runs cleanup off the lock (cleanup does Docker
// work). Idempotent and nil-safe, so eviction, Shutdown, and a normal session close can all
// call it without coordination.
func (s *Session) teardownExplorer() {
	s.mu.Lock()
	exp := s.explorer
	s.explorer = nil
	s.mu.Unlock()
	if exp != nil {
		exp.cleanup()
	}
}

// Hub is the session's SSE hub — the control-room stream handler subscribes to it and the
// browser receives the live `delta`/`turn` events for this conversation only (each session
// has its own hub, so one human's stream never leaks to another).
func (s *Session) Hub() *live.Hub { return s.hub }

// ResolveIssue returns the dead-lettered issue id a Resolve-mode session is unsticking, or ""
// for a blank Create session. The resolve consent gate reads it to commit against the issue the
// server bound at mint, not one the browser names (T4.15).
func (s *Session) ResolveIssue() string { return s.issueID }

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

// Draft returns a copy of the latest drafted spec + seed issues (T4.14) the planner emitted —
// what the APPROVE consent gate would write. The copy is safe to render without holding the
// lock; it is empty (Draft.Empty()) until the planner emits its first ```draft block.
func (s *Session) Draft() Draft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draft.clone()
}

// Transcript returns the conversation as JSON — the replayable provenance record the Seeder
// stores in the artifact store on APPROVE (specs-process.md: "the conversation transcript ...
// in the artifact store ... the 'why'"). It is the user/assistant turns only (the persona
// system prompt is never part of the record), captured as the displayed prose with any
// structured ledger/draft blocks already stripped (run stores the prose, not the raw reply).
func (s *Session) Transcript() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	type turn struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	turns := make([]turn, 0, len(s.messages))
	for _, m := range s.messages {
		turns = append(turns, turn{Role: string(m.Role), Text: m.Text})
	}
	b, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return nil
	}
	return b
}

// ForkAnswer is one human resolution to an open fork in a batch submit (T4.27,
// specs/control-room.md "Forks are surfaced and answered in batches"): which fork (Question — the
// fork's STABLE identity, re-resolved against the latest ledger snapshot by exact text match) and
// the resolution. Identity is the question, not a positional index: the planner re-emits the whole
// ledger every turn and may reorder or drop forks, so a position captured when the form rendered
// can land on the wrong fork (or past the end) by the time the batch posts — keying on the question
// maps each answer to the right fork or drops it cleanly when that fork is gone, never silently
// mis-applies. The three first-class moves are mutually exclusive, applied in precedence order:
// Discuss (flag "let's discuss", with an optional Note on what gives the human pause) wins; then
// free Text (the human types the answer, folding in nuance the canned options missed); then a
// chosen option (OptIdx >= 0, a chip pick — an index WITHIN the resolved fork's options, which are
// far more stable than fork order). An answer carrying none of these is dropped.
type ForkAnswer struct {
	Question string // the fork's question text — its stable identity across ledger re-emissions
	OptIdx   int    // chosen option index within the resolved fork, or < 0 for none
	Text     string
	Discuss  bool
	Note     string
}

// Answer funnels a batch of fork resolutions back through the conversation as ONE user turn that
// enumerates each answered fork by its number + question, so the planner attributes every answer
// unambiguously and reconciles the whole batch on its next turn — including noticing that one
// answer made another fork moot. Chip picks, free text, and "let's discuss" flags all become
// lines in that single turn; the planner re-emits the ledger reflecting them (the planner stays
// the single source of truth — there is no client-side ledger mutation). Each answer is resolved
// to its fork by Question (exact match against the latest ledger), NOT by position: the loop walks
// the current ledger so the fork numbers and order in the message always match what the human now
// sees, and an answer whose question is no longer present is dropped cleanly (the fork was settled
// or removed since the form rendered) rather than mis-attributed to whatever now sits at its old
// index. An empty batch (nothing resolvable) is a no-op returning "". On success it returns the
// message it sent.
func (s *Session) Answer(answers []ForkAnswer) string {
	s.mu.Lock()
	ledger := slices.Clone(s.ledger)
	s.mu.Unlock()

	// Index the submitted answers by their fork's question — the stable identity we re-resolve
	// against the latest ledger below (last write wins on the rare duplicate).
	byQuestion := make(map[string]ForkAnswer, len(answers))
	for _, a := range answers {
		if q := strings.TrimSpace(a.Question); q != "" {
			byQuestion[q] = a
		}
	}

	var lines []string
	for i, it := range ledger {
		a, ok := byQuestion[strings.TrimSpace(it.Question)]
		if !ok {
			continue // no answer submitted for this fork
		}
		n := i + 1 // 1-based fork number in the CURRENT ledger — the id the planner attributes by
		switch {
		case a.Discuss:
			line := fmt.Sprintf("%d. %q → let's discuss", n, it.Question)
			if note := strings.TrimSpace(a.Note); note != "" {
				line += ": " + note
			}
			lines = append(lines, line)
		case strings.TrimSpace(a.Text) != "":
			lines = append(lines, fmt.Sprintf("%d. %q → %s", n, it.Question, strings.TrimSpace(a.Text)))
		case a.OptIdx >= 0 && a.OptIdx < len(it.Options):
			lines = append(lines, fmt.Sprintf("%d. %q → I choose: %s.", n, it.Question, it.Options[a.OptIdx].Label))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	msg := "Here are my answers to the open forks:\n" + strings.Join(lines, "\n")
	s.Send(msg)
	return msg
}

// Busy reports whether a reply turn is currently in flight.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// TurnElapsed reports how many whole seconds the in-flight turn has been running, or 0 when no
// turn is in flight. It anchors the live activity line's client-ticked elapsed timer: the view
// renders this at fetch time and the browser ticks up from it, so a slow turn visibly progresses
// (the "it's working, not hung" signal) without the server re-rendering each second.
func (s *Session) TurnElapsed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy {
		return 0
	}
	return int(time.Since(s.turnStarted).Seconds())
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
	s.turnStarted = time.Now()
	s.messages = append(s.messages, model.Message{Role: model.RoleUser, Text: userText})
	s.mu.Unlock()

	go s.run()
	return true
}

// run executes one human turn to completion. With exploration disabled it is a single trusted
// adapter.Complete (pure conversation, as before). With exploration enabled it is a bounded
// loop: the model may call read-only tools against the sandboxed checkout, see the results, and
// call again, until it replies with prose — only that final prose is recorded. It always clears
// busy and broadcasts the terminal `turn` nudge, even on error, so a failed turn never wedges
// the session and the transcript still refreshes (rendering the error note).
func (s *Session) run() {
	ctx, cancel := context.WithTimeout(context.Background(), s.turnTimeout)
	defer cancel()

	s.readCount = 0 // fresh per-turn count for the activity line's "N read" accumulation

	s.mu.Lock()
	msgs := slices.Clone(s.messages)
	s.mu.Unlock()

	// The planner always carries its OUTPUT tools (update_ledger/propose_draft) — they are how it
	// emits structured state (T4.29), whether or not it can explore. Read-only EXPLORATION action
	// tools are added only when this session has a sandbox configured; without one the planner is a
	// pure conversation that still emits a ledger/draft via the output tools.
	defs := plannerOutputToolDefs()
	if s.sandboxCfg != nil {
		defs = append(defs, readOnlyToolDefs()...)
	}

	turn, err := s.converse(ctx, msgs, defs)
	prose := turn.prose
	if err != nil {
		s.log.Error("wizard: model turn failed", "session", s.ID, "err", err)
		prose = fmt.Sprintf("The requirements planner hit an error and could not reply: %v\n\nPlease try again.", err)
	}

	s.mu.Lock()
	// The text reply is pure prose now — the ledger/draft ride the tool channel — so it is
	// recorded verbatim (no fence-stripping). A turn that emitted neither output tool leaves the
	// prior snapshots untouched (ledgerSet/draftSet stay false).
	s.messages = append(s.messages, model.Message{Role: model.RoleAssistant, Text: prose})
	if turn.ledgerSet {
		s.ledger = turn.ledger
	}
	if turn.draftSet {
		s.draft = turn.draft
	}
	s.busy = false
	s.mu.Unlock()

	// Terminal nudge: the transcript re-fetches and renders the finalized reply, which also
	// resets the live delta target. Emitted last so the refetch sees the appended message; the
	// ledger/draft nudges follow so their panels refresh only when this turn updated them.
	s.hub.Broadcast(live.Event{Name: eventTurn, Data: ""})
	if turn.ledgerSet {
		s.hub.Broadcast(live.Event{Name: eventLedger, Data: ""})
	}
	if turn.draftSet {
		s.hub.Broadcast(live.Event{Name: eventDraft, Data: ""})
	}
}

// plannerTurn is what one human turn resolves to: the final prose reply plus the latest-wins
// structured state harvested from the planner's output tool calls. ledgerSet/draftSet report
// whether this turn updated each snapshot, so run() overwrites only what changed — a turn that
// touched neither leaves the prior snapshots intact.
type plannerTurn struct {
	prose     string
	ledger    []LedgerItem
	ledgerSet bool
	draft     Draft
	draftSet  bool
}

// plannerOutputToolDefs returns the planner's OUTPUT tools — update_ledger and propose_draft —
// the schema-validated channel it emits structured state on (T4.29, control-room.md "The
// alignment ledger"). They are pure-output tools, distinct from the read-only exploration action
// tools (readOnlyToolDefs): emitting one records state and never triggers a round-trip.
func plannerOutputToolDefs() []model.ToolDef {
	return []model.ToolDef{updateLedgerToolDef(), proposeDraftToolDef()}
}

// draftNudge is the one-shot corrective the converse loop injects when the model's concluding
// prose announces a draft but the turn emitted no propose_draft call (see converse). It reminds —
// it does not command: a draft is recorded only by the tool call, so if the model is genuinely
// ready it should call propose_draft now, and if it is not it should keep to questions + the
// ledger. That phrasing keeps a false-positive harmless (a re-evaluation, never a forced draft).
const draftNudge = "A draft is recorded ONLY when you call the propose_draft tool — prose describing " +
	"it does nothing on its own, and your last reply described a draft without emitting the call. " +
	"If intent has genuinely converged and you would recommend approving it, emit the propose_draft " +
	"tool call now. If it has not converged, keep to prose + the ledger instead."

// announcesDraft reports whether the planner's prose reads as an announcement that it is about to
// (or just did) produce the draft — the "narrates the action instead of taking it" pattern the
// draft backstop catches. It is a deliberately small, lowercased phrase match: only a backstop, so
// a miss merely reverts to the prior behavior (prose shown, no draft) and a spurious hit is
// absorbed by draftNudge's gentle wording. The phrases are the committal forms ("let me propose
// the draft", "draft the spec"), not a bare mention of the word "draft".
func announcesDraft(prose string) bool {
	p := strings.ToLower(prose)
	for _, phrase := range []string{
		"propose the draft",
		"propose the full draft",
		"propose the spec",
		"let me draft",
		"let me propose",
		"i'll draft",
		"i will draft",
		"draft the spec",
		"draft the full spec",
		"seed the issues",
		"seed issues",
	} {
		if strings.Contains(p, phrase) {
			return true
		}
	}
	return false
}

// converse runs the model to a prose conclusion for one human turn. Two kinds of tool call flow
// back. EXPLORATION (read-only action) calls — present only when the session has a sandbox — are
// dispatched against it and their results fed back, driving another round-trip; that is what the
// loop iterates on. OUTPUT calls (update_ledger/propose_draft) carry the structured state and are
// harvested latest-wins; they NEVER add a round-trip — a call rides whatever turn it arrives on,
// so a reply that emits only prose + output calls concludes immediately (T4.29). The turn ends on
// the first response with no exploration call; that response's text is the prose the human reads
// (intermediate exploration preambles are suppressed). The prose streams to the hub as it arrives
// (coalesced `delta` events) and is pure now — no fence-stripping. Bounded by maxToolTurns (and
// the caller's per-turn timeout).
func (s *Session) converse(ctx context.Context, msgs []model.Message, defs []model.ToolDef) (plannerTurn, error) {
	var out plannerTurn
	nudged := false // the draft backstop fires at most once per human turn (termination)
	for turn := 1; turn <= s.maxToolTurns; turn++ {
		// Accumulate this turn's reply and re-broadcast the cumulative (HTML-escaped) prose as it
		// grows, coalesced by deltaFlushRunes. A fresh builder per turn so an intermediate
		// preamble is replaced by the final reply rather than concatenated. Cumulative (not
		// incremental) so a dropped SSE frame self-heals on the next — matching the hub's drops.
		var b strings.Builder
		lastLen := 0
		onEvent := func(ev model.StreamEvent) {
			if ev.TextDelta == "" {
				return
			}
			b.WriteString(ev.TextDelta)
			if n := utf8.RuneCountInString(b.String()); n-lastLen >= deltaFlushRunes {
				lastLen = n
				s.hub.Broadcast(live.Event{Name: eventDelta, Data: html.EscapeString(b.String())})
			}
		}

		// Log each model round-trip so a multi-step (and, with this model, multi-second) turn is
		// never a silent terminal: you can see the call go out and how long it took to come back.
		callStart := time.Now()
		s.log.Info("wizard: model call", "session", s.ID, "round", turn, "messages", len(msgs), "tools", len(defs))
		resp, err := s.adapter.Complete(ctx, model.Request{
			System:    s.persona,
			Messages:  msgs,
			Tools:     defs,
			MaxTokens: s.maxTokens,
		}, onEvent)
		if err != nil {
			return out, err
		}
		s.log.Info("wizard: model replied", "session", s.ID, "round", turn,
			"elapsed", time.Since(callStart).String(), "tool_calls", len(resp.ToolCalls), "reply_chars", len(resp.Text))

		// Partition this round's calls: harvest the output calls (latest-wins) and ack them; queue
		// the exploration calls. Output calls never force a round-trip, so a response carrying only
		// output calls (or none) concludes the turn — its text is the prose.
		var actionCalls []model.ToolCall
		var outputAcks []model.ToolResult
		for _, tc := range resp.ToolCalls {
			switch tc.Name {
			case toolUpdateLedger:
				outputAcks = append(outputAcks, s.harvestLedger(&out, tc))
			case toolProposeDraft:
				outputAcks = append(outputAcks, s.harvestDraft(&out, tc))
			default:
				actionCalls = append(actionCalls, tc)
			}
		}

		if len(actionCalls) == 0 {
			// Draft backstop: the model frequently *narrates* the draft as a closing action
			// ("let me propose the draft") without emitting the propose_draft call — and since a
			// prose-only turn concludes here, that promised draft would silently never appear, so
			// the human re-asks and the model re-narrates forever. When the concluding prose
			// announces a draft yet this turn carried no draft, inject one corrective nudge and let
			// the model act. Capped to once per human turn so it can never loop: if the model still
			// declines, we fall through to returning the prose. The nudge is gentle (it reminds, it
			// does not force) so a false positive just prompts a re-evaluation, never a half-baked
			// draft.
			if !nudged && !out.draftSet && announcesDraft(resp.Text) {
				nudged = true
				msgs = append(msgs, model.Message{Role: model.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
				if len(outputAcks) > 0 {
					// Keep the tool-call history well-formed: any output call this turn (e.g. an
					// update_ledger riding alongside the prose) still needs its result fed back.
					msgs = append(msgs, model.Message{Role: model.RoleTool, ToolResults: outputAcks})
				}
				msgs = append(msgs, model.Message{Role: model.RoleUser, Text: draftNudge})
				continue
			}
			out.prose = resp.Text
			return out, nil
		}

		// Exploration round: dispatch the reads and ack EVERY call (reads and output, so the
		// follow-up request's tool-call history is well-formed), feed back, loop. Kept only in the
		// local msgs slice — never appended to s.messages, so the cross-turn history stays clean
		// prose. dispatch does not read msgs, so evaluating both messages in one append is safe.
		results := append(s.dispatch(ctx, actionCalls), outputAcks...)
		msgs = append(msgs,
			model.Message{Role: model.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls},
			model.Message{Role: model.RoleTool, ToolResults: results})
	}
	return out, fmt.Errorf("exploration did not converge within %d tool round-trips", s.maxToolTurns)
}

// harvestLedger decodes an update_ledger output call into the turn's latest-wins ledger snapshot
// and returns the ack the model sees (consumed only when a concurrent exploration call forces a
// follow-up). A decode failure or an empty ledger leaves the prior snapshot untouched (ledgerSet
// stays false) — degrade gracefully, never clobber — and acks an error so the model can correct.
func (s *Session) harvestLedger(out *plannerTurn, tc model.ToolCall) model.ToolResult {
	items, err := parseLedgerArgs(tc.Args)
	if err != nil {
		s.log.Warn("wizard: update_ledger args did not decode", "session", s.ID, "err", err)
		return model.ToolResult{ToolCallID: tc.ID, Content: "update_ledger arguments did not decode as JSON: " + err.Error(), IsError: true}
	}
	if len(items) == 0 {
		return model.ToolResult{ToolCallID: tc.ID, Content: "update_ledger recorded no items (each fork needs a non-empty question)", IsError: true}
	}
	out.ledger, out.ledgerSet = items, true
	s.log.Debug("wizard: ledger harvested", "session", s.ID, "items", len(items))
	return model.ToolResult{ToolCallID: tc.ID, Content: fmt.Sprintf("ledger recorded (%d item(s))", len(items))}
}

// harvestDraft decodes a propose_draft output call into the turn's latest-wins draft snapshot and
// returns the ack. A decode failure or an empty draft leaves the prior snapshot untouched and acks
// an error, mirroring harvestLedger.
func (s *Session) harvestDraft(out *plannerTurn, tc model.ToolCall) model.ToolResult {
	d, err := parseDraftArgs(tc.Args)
	if err != nil {
		s.log.Warn("wizard: propose_draft args rejected", "session", s.ID, "err", err)
		return model.ToolResult{ToolCallID: tc.ID, Content: "propose_draft arguments rejected: " + err.Error(), IsError: true}
	}
	out.draft, out.draftSet = d, true
	s.log.Debug("wizard: draft harvested", "session", s.ID, "specs", len(d.Specs), "issues", len(d.Issues))
	return model.ToolResult{ToolCallID: tc.ID, Content: fmt.Sprintf("draft recorded (%d spec(s), %d issue(s))", len(d.Specs), len(d.Issues))}
}

// dispatch executes the model's read-only tool calls against the session's exploration sandbox
// (provisioned lazily on the first call) and returns one ToolResult per call, in order. It
// never returns a fatal error: an unavailable sandbox, an unknown tool, or a tool that errors
// each become an IsError result the model can react to, so a failed exploration degrades to a
// normal reply rather than crashing the background conversation goroutine. The explorer is
// captured implicitly via ensureExplorer; a concurrent eviction that tears the sandbox down
// mid-Exec surfaces as a tool error here, which is the correct "I lost access" behavior.
func (s *Session) dispatch(ctx context.Context, calls []model.ToolCall) []model.ToolResult {
	exp, err := s.ensureExplorer(ctx)
	results := make([]model.ToolResult, 0, len(calls))
	for _, tc := range calls {
		s.readCount++
		// Label the live activity line with the read and a running per-turn count, so a
		// multi-step exploration reads as accumulating progress ("read_file foo.go · 3 read")
		// rather than a single line that merely flickers between files.
		label := fmt.Sprintf("%s · %d read", humanizeToolCall(tc), s.readCount)
		s.hub.Broadcast(live.Event{Name: eventTool, Data: html.EscapeString(label)})
		switch {
		case err != nil:
			results = append(results, model.ToolResult{ToolCallID: tc.ID, Content: "could not access the codebase: " + err.Error(), IsError: true})
		case exp == nil:
			results = append(results, model.ToolResult{ToolCallID: tc.ID, Content: "codebase exploration is not available", IsError: true})
		default:
			results = append(results, dispatchOne(ctx, exp, tc))
		}
	}
	return results
}

// dispatchOne runs a single tool call against the explorer, converting every failure into an
// IsError ToolResult (the conversation must never crash on a bad call).
func dispatchOne(ctx context.Context, exp *explorer, tc model.ToolCall) model.ToolResult {
	tool, ok := exp.byName[tc.Name]
	if !ok {
		return model.ToolResult{ToolCallID: tc.ID, Content: "unknown tool: " + tc.Name, IsError: true}
	}
	out, err := tool.Invoke(ctx, tc.Args)
	if err != nil {
		return model.ToolResult{ToolCallID: tc.ID, Content: "tool error: " + err.Error(), IsError: true}
	}
	return model.ToolResult{ToolCallID: tc.ID, Content: out.Content, IsError: out.IsError}
}

// humanizeToolCall renders a short label for a read tool call for the live exploration status
// strip ("read_file internal/foo.go", `search "func New"`, "find_symbol Planner"). It
// best-effort extracts the salient argument from the call's JSON; on any miss it falls back to
// the bare tool name. Display only — never parsed back.
func humanizeToolCall(tc model.ToolCall) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Name    string `json:"name"`
		Query   string `json:"query"`
	}
	_ = json.Unmarshal(tc.Args, &a)
	switch {
	case a.Path != "":
		return tc.Name + " " + a.Path
	case a.Pattern != "":
		return tc.Name + " " + strconv.Quote(a.Pattern)
	case a.Name != "":
		return tc.Name + " " + a.Name
	case a.Query != "":
		return tc.Name + " " + strconv.Quote(a.Query)
	default:
		return tc.Name
	}
}

// logSnippet returns s trimmed and truncated to max runes for a single readable log line,
// collapsing nothing but appending an ellipsis marker when it had to cut. Rune-safe so a
// multibyte tail is never sliced mid-character.
func logSnippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…(truncated)"
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
