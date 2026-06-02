// Package controlroom is the web UI: the human's read-only window into the factory and
// their only place to act (specs/control-room.md). This file is the T4.1 scaffold — the
// HTTP server, the embedded-asset pipeline, and the base layout — onto which the
// data-backed views (Board, DAG, Activity, DLQ, Budgets, Provenance) and the wizard are
// built in later Phase 4 tasks.
//
// It is a server-driven hypermedia app: templ renders typed HTML server-side, htmx swaps
// fragments over the wire, Alpine handles small client state, and Tailwind supplies the
// CSS — all three front-end assets embedded so a deployed harness is a single binary with
// no runtime toolchain. The server holds no factory state itself; it is a rendering shell
// that later tasks wire to beads, the artifact store, and NATS.
package controlroom

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/Loxstomper/harness/internal/controlroom/assets"
	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/controlroom/views"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
)

// sseHeartbeat is the idle interval at which an open SSE stream gets a comment line, to
// keep intermediaries from reaping the connection and to surface a dead client as a
// write error. Comfortably under the ~60s a typical proxy idle-times out a stream.
const sseHeartbeat = 25 * time.Second

// Options configures a Server. All fields are optional: Version is stamped into the
// landing page for provenance, Logger defaults to a stderr text logger, and Events —
// when set — turns on the live SSE feed at GET /events. Events is nil for a standalone
// `harness serve` (no running factory to tail); `harness run --serve-addr` supplies a
// hub fed from the run's in-process NATS.
//
// Reader is the read model behind the data views (the Board, T4.4, and later DLQ /
// detail / provenance). It is nil for a standalone serve with no stores to read, in
// which case those views render a "not attached to a factory" notice. StageOrder is the
// pipeline role order the board lays its columns out in (left-to-right flow); empty is
// tolerated — the board falls back to alphabetical column order.
type Options struct {
	Version    string
	Logger     *slog.Logger
	Events     *live.Hub
	Activity   *live.Activity
	Reader     *query.Reader
	StageOrder []string
	// BudgetCaps are the configured termination-guarantee ceilings the Budgets view (T4.10)
	// measures burn against (config.Harness.Policy → query.BudgetCaps). Passed in by the
	// composition root so the read model stays free of a config dependency, like StageOrder.
	BudgetCaps query.BudgetCaps
	// Planner is the trusted, non-sandboxed requirements planner behind the Create-Task
	// wizard (T4.12). It is nil for a standalone `harness serve` or a config that omits the
	// requirements_planner block, in which case /create renders a "wizard disabled" notice
	// and its data endpoints 503 — mirroring how a nil Reader degrades the data views.
	Planner *wizard.Planner
	// Seeder commits an approved wizard draft (T4.14): it writes the drafted specs to git,
	// the decisions sidecar, stores the conversation transcript, and creates the seed issues
	// through the single-writer beads path. It is the consent-gated write seam — the one place
	// the wizard touches the durable stores. nil disables APPROVE (a standalone `harness serve`
	// has no repo to write); /create still elicits intent but cannot commit it.
	Seeder wizard.Seeder
	// Resolver commits an approved Resolve-mode draft (T4.15): it refines the spec and returns
	// the dead-lettered issue to the ready pool. nil disables the Resolve consent gate (like a
	// nil Seeder for Create). Resolve mode is the wizard pre-loaded from a dead-lettered issue.
	Resolver wizard.Resolver
	// Repo and SpecDepth are the repository root and the spec-slice link-traversal depth
	// (config.Harness.SpecDepth). They are supplied by the composition root — not held by the
	// Reader — so the read model stays free of a filesystem/config dependency, mirroring how
	// StageOrder and BudgetCaps are threaded. Resolve mode needs them to resolve the spec slice
	// it pre-loads (query.ResolveContext) and to compute the blast radius (query.BlastRadius).
	Repo      string
	SpecDepth int
}

// Server renders the control room. It is an http.Handler (via Handler) so its routes can
// be exercised with httptest without binding a socket, and it offers ListenAndServe for
// the `harness serve` command with context-driven graceful shutdown.
type Server struct {
	mux        *http.ServeMux
	log        *slog.Logger
	version    string
	events     *live.Hub
	activity   *live.Activity
	reader     *query.Reader
	stageOrder []string
	budgetCaps query.BudgetCaps
	planner    *wizard.Planner
	seeder     wizard.Seeder
	resolver   wizard.Resolver
	repo       string
	specDepth  int
}

// New builds the server and registers every route. Routes are registered eagerly so a
// constructed Server is immediately serveable and fully testable.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	s := &Server{
		mux:        http.NewServeMux(),
		log:        log,
		version:    opts.Version,
		events:     opts.Events,
		activity:   opts.Activity,
		reader:     opts.Reader,
		stageOrder: opts.StageOrder,
		budgetCaps: opts.BudgetCaps,
		planner:    opts.Planner,
		seeder:     opts.Seeder,
		resolver:   opts.Resolver,
		repo:       opts.Repo,
		specDepth:  opts.SpecDepth,
	}
	s.routes()
	return s
}

// routes wires the URL surface. Exact-match patterns (Go 1.22+ method routing) mean any
// unregistered path falls through to the mux's built-in 404 rather than silently
// rendering the home page.
func (s *Server) routes() {
	// Embedded static assets (compiled CSS + vendored JS), served from the binary.
	s.mux.Handle("GET /static/", cacheStatic(http.FileServerFS(assets.FS)))

	// Liveness probe — no dependencies, so it answers even before the data layer (T4.2)
	// is wired; useful for tests and for ops once the server fronts real components.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	// Live event stream. The board and activity feed (T4.4/T4.5) attach to this via the
	// htmx SSE extension; it is the substrate, registered whether or not a hub is wired
	// (it answers 503 when standalone) so the URL surface is stable.
	s.mux.HandleFunc("GET /events", s.handleEvents)

	// Landing page.
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, views.Home(s.version))
	})

	// Data-backed views, registered before the placeholder loop claims their path.
	s.mux.HandleFunc("GET /board", s.handleBoard)            // T4.4 — kanban over beads
	s.mux.HandleFunc("GET /board/cards", s.handleBoardCards) // the htmx/SSE live fragment
	s.mux.HandleFunc("GET /dag", s.handleDAG)                // T4.6 — issue dependency graph
	s.mux.HandleFunc("GET /dag/svg", s.handleDAGSVG)         // the htmx/SSE live SVG fragment
	s.mux.HandleFunc("GET /activity", s.handleActivity)      // T4.5 — live agent feed
	s.mux.HandleFunc("GET /activity/items", s.handleActivityItems)
	s.mux.HandleFunc("GET /issue/{id}", s.handleIssue)         // T4.7 — issue / invocation detail
	s.mux.HandleFunc("GET /replay/{id}", s.handleReplay)       // T4.11 — reconstructed decision trail
	s.mux.HandleFunc("GET /artifact/{hash}", s.handleArtifact) // raw evidence content (untrusted)
	s.mux.HandleFunc("GET /dlq", s.handleDLQ)                  // T4.8 — dead-letter queue (action surface)
	s.mux.HandleFunc("GET /dlq/items", s.handleDLQItems)       // the htmx/SSE live fragment
	s.mux.HandleFunc("GET /budgets", s.handleBudgets)          // T4.10 — burn vs caps, per epic/issue
	s.mux.HandleFunc("GET /budgets/items", s.handleBudgetsItems)
	s.mux.HandleFunc("GET /provenance", s.handleProvenance) // T4.10 — merged-commit provenance chain
	s.mux.HandleFunc("GET /provenance/items", s.handleProvenanceItems)
	s.mux.HandleFunc("GET /status/bar", s.handleStatusBar) // T4.19 — layout status-bar live fragment

	// Create-Task wizard (T4.12) — the human's action surface. GET /create mints a fresh
	// conversation session and renders the page; the per-session SSE stream carries the live
	// reply; POST seeds a turn; the messages fragment is the `turn`-nudge re-fetch target.
	s.mux.HandleFunc("GET /create", s.handleCreate)
	s.mux.HandleFunc("GET /create/stream/{session}", s.handleCreateStream)
	s.mux.HandleFunc("GET /create/messages/{session}", s.handleCreateMessages)
	s.mux.HandleFunc("POST /create/message", s.handleCreateMessage)
	s.mux.HandleFunc("GET /create/ledger/{session}", s.handleCreateLedger)       // T4.13 — alignment-ledger panel fragment
	s.mux.HandleFunc("POST /create/ledger/select", s.handleCreateLedgerSelect)   // T4.13 — chip click funnels through the planner
	s.mux.HandleFunc("GET /create/draft/{session}", s.handleCreateDraft)         // T4.14 — drafted spec + seed issues panel fragment
	s.mux.HandleFunc("POST /create/approve", s.handleCreateApprove)              // T4.14 — the consent gate: commit the approved draft

	// Resolve mode (T4.15) — the wizard pre-loaded from a dead-lettered issue. The page mints a
	// session grounded in the escalation + spec slice + transcript; the conversation, ledger, and
	// draft reuse the per-session /create/* endpoints (a Resolve session is just a Session), so
	// the only Resolve-specific routes are the page, the blast-radius preview fragment, and the
	// resolve consent gate (which refines the spec and reopens the issue).
	s.mux.HandleFunc("GET /resolve/{id}", s.handleResolve)
	s.mux.HandleFunc("GET /resolve/blast/{session}", s.handleResolveBlast)
	s.mux.HandleFunc("POST /resolve/approve", s.handleResolveApprove)

	// Every remaining navigation destination resolves to a placeholder until its
	// data-backed view lands; registering them from views.NavItems keeps the navigation
	// and the routes a single source of truth. `implemented` excludes the views wired
	// above so the mux is not asked to register a duplicate pattern.
	implemented := map[string]bool{
		"board": true, "dag": true, "activity": true, "dlq": true,
		"budgets": true, "provenance": true, "create": true,
	}
	for _, item := range views.NavItems {
		if implemented[item.Key] {
			continue
		}
		s.mux.HandleFunc("GET "+item.Href, func(w http.ResponseWriter, r *http.Request) {
			s.render(w, r, views.Placeholder(item.Label, item.Key))
		})
	}
}

// render writes a templ component as the response body, logging (but not exposing) any
// render error. A mid-render failure may have already written a partial body and status,
// so we cannot rewrite headers — logging is the honest outcome.
func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("controlroom: render failed", "path", r.URL.Path, "err", err)
	}
}

// handleEvents serves the live SSE feed. It subscribes the connecting browser to the
// hub, then streams every broadcast event until the client disconnects or the server
// shuts down (both cancel r.Context() — see ListenAndServe's BaseContext). With no hub
// wired (standalone `harness serve`) there is nothing to tail, so it answers 503 rather
// than holding open a stream that would never emit.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		http.Error(w, "live events unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Defeat proxy/response buffering that would otherwise hold frames back and break
	// the live feel (nginx honors this; harmless elsewhere).
	h.Set("X-Accel-Buffering", "no")

	sub, cancel := s.events.Subscribe()
	defer cancel()

	if err := live.Stream(r.Context(), w, sub, sseHeartbeat); err != nil {
		s.log.Debug("controlroom: sse stream closed", "path", r.URL.Path, "err", err)
	}
}

// handleBoard renders the full kanban page. With no read model wired (standalone
// `harness serve`) it shows a "not attached" notice rather than an empty board; a board
// read error renders the same chrome with the error so the page never 500s blank. The
// live columns refresh themselves from handleBoardCards over SSE.
func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.BoardMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to see live work."))
		return
	}
	board, err := s.reader.Board(r.Context(), s.stageOrder)
	if err != nil {
		s.log.Error("controlroom: board read failed", "err", err)
		s.render(w, r, views.BoardMessage("Could not load the board: "+err.Error()))
		return
	}
	s.render(w, r, views.BoardPage(board))
}

// handleBoardCards returns just the columns fragment — the htmx swap target the board
// page re-fetches on an SSE signal (throttled) and a periodic backstop. It is a data
// endpoint, so with no read model it answers 503 (it is never wired without one), and a
// read error is a 500: htmx leaves the last good columns in place rather than swapping in
// an error, so the live board degrades to "stale" not "broken".
func (s *Server) handleBoardCards(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "board unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	board, err := s.reader.Board(r.Context(), s.stageOrder)
	if err != nil {
		s.log.Error("controlroom: board fragment read failed", "err", err)
		http.Error(w, "could not refresh the board\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.BoardColumns(board))
}

// handleDAG renders the full issue-dependency-graph page (T4.6). With no read model wired
// (standalone `harness serve`) it shows a "not attached" notice rather than an empty graph; a
// DAG read error renders the same chrome with the reason so the page never 500s blank,
// mirroring the board. The graph refreshes itself from handleDAGSVG over SSE.
func (s *Server) handleDAG(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.DAGMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to see live work."))
		return
	}
	g, err := s.reader.DAG(r.Context())
	if err != nil {
		s.log.Error("controlroom: dag read failed", "err", err)
		s.render(w, r, views.DAGMessage("Could not load the DAG: "+err.Error()))
		return
	}
	s.render(w, r, views.DAGPage(g))
}

// handleDAGSVG returns just the rendered graph fragment — the htmx swap target the DAG page
// re-fetches on an SSE signal (throttled) and a periodic backstop. It is a data endpoint, so
// with no read model it answers 503 (it is never wired without one), and a read error is a
// 500: htmx leaves the last good graph in place rather than swapping in an error, so the live
// graph degrades to "stale" not "broken".
func (s *Server) handleDAGSVG(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "dag unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	g, err := s.reader.DAG(r.Context())
	if err != nil {
		s.log.Error("controlroom: dag fragment read failed", "err", err)
		http.Error(w, "could not refresh the dag\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.DAGGraph(g))
}

// handleActivity renders the full activity-feed page. Answers a friendly notice when
// no activity buffer is wired (standalone `harness serve` with no live source).
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if s.activity == nil {
		s.render(w, r, views.ActivityMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to see live agent activity."))
		return
	}
	s.render(w, r, views.ActivityPage(s.activity.Recent()))
}

// handleActivityItems renders just the feed rows fragment for htmx/SSE refresh.
func (s *Server) handleActivityItems(w http.ResponseWriter, r *http.Request) {
	if s.activity == nil {
		http.Error(w, "activity unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	s.render(w, r, views.ActivityList(s.activity.Recent()))
}

// handleIssue renders the issue / invocation detail page (T4.7) — the drill-target the
// board, dead-letter queue, and provenance views link into. With no read model wired
// (standalone `harness serve`) it shows the not-attached notice; an unknown id or a read
// fault renders the same chrome with the reason rather than a blank 500, mirroring the
// board's "never 500 blank" handling. The page is a forensic snapshot, so it is plainly
// rendered with no live refresh.
func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.IssueDetailMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to inspect issues."))
		return
	}
	id := r.PathValue("id")
	detail, err := s.reader.IssueDetail(r.Context(), id)
	if err != nil {
		s.log.Error("controlroom: issue detail read failed", "id", id, "err", err)
		s.render(w, r, views.IssueDetailMessage("Could not load issue "+id+": "+err.Error()))
		return
	}
	s.render(w, r, views.IssueDetailPage(detail))
}

// handleReplay renders the invocation replay page (T4.11) — the reconstructed decision
// trail drilled into from the issue-detail page. It mirrors handleIssue exactly: with no
// read model wired (standalone `harness serve`) it shows the not-attached notice, and an
// unknown id or read fault renders the same chrome with the reason rather than a blank 500.
// A *known* issue with no reachable transcript is not an error — Replay returns Available=
// false and the page renders an in-chrome notice. Like the detail page it is a forensic
// snapshot, so it is plainly rendered with no live refresh.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.ReplayNotAttached("Not attached to a running factory — start the control room with `harness run --serve-addr` to replay invocations."))
		return
	}
	id := r.PathValue("id")
	rep, err := s.reader.Replay(r.Context(), id)
	if err != nil {
		s.log.Error("controlroom: replay read failed", "id", id, "err", err)
		s.render(w, r, views.ReplayNotAttached("Could not load replay for "+id+": "+err.Error()))
		return
	}
	s.render(w, r, views.ReplayPage(rep))
}

// handleArtifact streams an evidence artifact's raw content by hash — the click-through
// target of the detail view's evidence links (the prompt, the traceability map, a gate
// check's captured output). The content is UNTRUSTED agent output, so it is served as
// text/plain with X-Content-Type-Options: nosniff: the browser must never be coaxed into
// interpreting a harvested transcript or gate log as HTML/script. With no read model it
// 503s (a data endpoint, like /board/cards); an unresolvable hash 404s.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "artifact unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	hash := r.PathValue("hash")
	rc, err := s.reader.Artifact(r.Context(), hash)
	if err != nil {
		s.log.Debug("controlroom: artifact fetch failed", "hash", hash, "err", err)
		http.Error(w, "artifact not found\n", http.StatusNotFound)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Debug("controlroom: artifact stream interrupted", "hash", hash, "err", err)
	}
}

// activeAgentWindow bounds how recently an agent must have emitted an event to still count as
// "active" on the status bar. The bar is a health glance, not a precise gauge, so a generous
// trailing window keeps a momentarily-quiet agent (between model turns) counted.
const activeAgentWindow = 90 * time.Second

// handleStatusBar returns the layout status-bar fragment (T4.19) — the "is the factory healthy?"
// glance lazy-loaded on every page and refreshed live (see views.StatusBarShell). It is a data
// endpoint, so with no read model wired (standalone `harness serve`) it answers 503 and htmx
// leaves the neutral placeholder in place — the spec's "degrades to a static bar"; a read error
// is a 500 so htmx keeps the last good bar rather than swapping in an error. The active-agent
// count comes from the in-memory activity buffer (0 when none is wired — a hub-less serve still
// renders the rest); everything else is the query.Status read against the configured budget caps.
func (s *Server) handleStatusBar(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "status unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	st, err := s.reader.Status(r.Context(), s.budgetCaps)
	if err != nil {
		s.log.Error("controlroom: status read failed", "err", err)
		http.Error(w, "could not refresh the status bar\n", http.StatusInternalServerError)
		return
	}
	active := 0
	if s.activity != nil {
		active = s.activity.ActiveAgents(activeAgentWindow)
	}
	s.render(w, r, views.StatusBar(st, active))
}

// handleDLQ renders the full dead-letter queue page (T4.8) — the escalations awaiting a
// human, the control room's primary action surface. With no read model wired (standalone
// `harness serve`) it shows a "not attached" notice rather than an empty queue; a read
// error renders the same chrome with the reason so the page never 500s blank, mirroring the
// board. The live list refreshes itself from handleDLQItems over SSE.
func (s *Server) handleDLQ(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.DeadLetterMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to see escalations."))
		return
	}
	items, err := s.reader.DeadLetters(r.Context())
	if err != nil {
		s.log.Error("controlroom: dlq read failed", "err", err)
		s.render(w, r, views.DeadLetterMessage("Could not load the dead-letter queue: "+err.Error()))
		return
	}
	s.render(w, r, views.DeadLetterPage(items))
}

// handleDLQItems returns just the queue-list fragment — the htmx swap target the DLQ page
// re-fetches on an SSE signal (throttled) and a periodic backstop. It is a data endpoint,
// so with no read model it answers 503 (it is never wired without one), and a read error is
// a 500: htmx leaves the last good list in place rather than swapping in an error, so the
// live queue degrades to "stale" not "broken".
func (s *Server) handleDLQItems(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "dlq unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	items, err := s.reader.DeadLetters(r.Context())
	if err != nil {
		s.log.Error("controlroom: dlq fragment read failed", "err", err)
		http.Error(w, "could not refresh the dead-letter queue\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.DeadLetterList(items))
}

// provenanceLimit bounds the merged-commit history the provenance view loads — enough to
// browse recent merges without walking the whole history of main on every refresh.
const provenanceLimit = 50

// handleBudgets renders the full budgets page (T4.10): burn vs caps, per epic and per issue.
// With no read model wired (standalone `harness serve`) it shows a "not attached" notice; a
// read error renders the same chrome with the reason so the page never 500s blank, mirroring
// the board/DLQ. The tables refresh themselves from handleBudgetsItems over SSE.
func (s *Server) handleBudgets(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.BudgetsMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to see budget burn."))
		return
	}
	b, err := s.reader.Budgets(r.Context(), s.budgetCaps)
	if err != nil {
		s.log.Error("controlroom: budgets read failed", "err", err)
		s.render(w, r, views.BudgetsMessage("Could not load budgets: "+err.Error()))
		return
	}
	s.render(w, r, views.BudgetsPage(b))
}

// handleBudgetsItems returns just the tables fragment — the htmx swap target the budgets page
// re-fetches on an SSE signal (throttled) and a periodic backstop. As a data endpoint it
// answers 503 with no read model and 500 on a read error, so htmx leaves the last good tables
// in place rather than swapping in an error (the live view degrades to "stale", not "broken").
func (s *Server) handleBudgetsItems(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "budgets unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	b, err := s.reader.Budgets(r.Context(), s.budgetCaps)
	if err != nil {
		s.log.Error("controlroom: budgets fragment read failed", "err", err)
		http.Error(w, "could not refresh budgets\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.BudgetsBody(b))
}

// handleProvenance renders the full provenance page (T4.10): recent merged commits traced
// back to issue→soul→model→prompt→evidence. Same not-attached / read-error handling as the
// other Reader-backed views; the list refreshes from handleProvenanceItems over SSE.
func (s *Server) handleProvenance(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		s.render(w, r, views.ProvenanceMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to trace merged commits."))
		return
	}
	commits, err := s.reader.RecentProvenance(r.Context(), provenanceLimit)
	if err != nil {
		s.log.Error("controlroom: provenance read failed", "err", err)
		s.render(w, r, views.ProvenanceMessage("Could not load provenance: "+err.Error()))
		return
	}
	s.render(w, r, views.ProvenancePage(commits))
}

// handleProvenanceItems returns just the commit-list fragment — the htmx swap target over SSE.
// Data endpoint: 503 with no read model, 500 on a read error.
func (s *Server) handleProvenanceItems(w http.ResponseWriter, r *http.Request) {
	if s.reader == nil {
		http.Error(w, "provenance unavailable: control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	commits, err := s.reader.RecentProvenance(r.Context(), provenanceLimit)
	if err != nil {
		s.log.Error("controlroom: provenance fragment read failed", "err", err)
		http.Error(w, "could not refresh provenance\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.ProvenanceList(commits))
}

// handleCreate renders the Create-Task wizard page (T4.12). With no planner wired (a
// standalone `harness serve`, or a config that omits requirements_planner) it shows a notice
// that the wizard is disabled rather than a dead form, mirroring how the data views degrade
// without a Reader. Otherwise it mints a fresh conversation session — each page load starts
// a blank conversation — and renders it; the session id is embedded so the SSE stream, the
// message fragment, and the POST all address this conversation.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		s.render(w, r, views.CreateMessage("The requirements planner is not configured — the Create-Task wizard is available when the control room runs with `harness run --serve-addr` and a requirements_planner is set in harness.yaml."))
		return
	}
	sess := s.planner.New()
	s.render(w, r, views.CreatePage(sess.ID, sess.Messages()))
}

// handleCreateStream serves one conversation's live SSE stream — the wizard's `delta`
// (token-by-token reply) and `turn` (reply-complete nudge) events, scoped to a single
// session's hub so a human's stream never leaks to another. With no planner it 503s (a data
// endpoint, like /events); an unknown/evicted session id 404s rather than holding open a
// stream that would never emit.
func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	sess := s.planner.Get(r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	sub, cancel := sess.Hub().Subscribe()
	defer cancel()

	if err := live.Stream(r.Context(), w, sub, sseHeartbeat); err != nil {
		s.log.Debug("controlroom: wizard sse stream closed", "session", sess.ID, "err", err)
	}
}

// handleCreateMessages returns just the transcript fragment — the htmx swap target the page
// re-fetches on a `turn` SSE nudge (and a slow periodic backstop, so a settled conversation
// converges even if a `delta`/`turn` frame is dropped). Data endpoint: 503 with no planner,
// 404 for an unknown session.
func (s *Server) handleCreateMessages(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	sess := s.planner.Get(r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	s.render(w, r, views.WizardTranscript(sess.Messages()))
}

// handleCreateMessage records the human's message and kicks off the planner's reply, then
// returns the transcript fragment so the just-sent message appears immediately (the reply
// itself streams in over SSE). A blank message or a reply already in flight simply records
// nothing — the returned transcript is unchanged, and the in-flight turn keeps streaming.
// Data endpoint: 503 with no planner, 404 for an unknown session.
func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse the message\n", http.StatusBadRequest)
		return
	}
	sess := s.planner.Get(r.FormValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	sess.Send(r.FormValue("text"))
	s.render(w, r, views.WizardTranscript(sess.Messages()))
}

// handleCreateLedger returns just the alignment-ledger panel fragment (T4.13) — the htmx swap
// target the page re-fetches on a `ledger`/`turn` SSE nudge (and a slow periodic backstop).
// Data endpoint: 503 with no planner, 404 for an unknown session.
func (s *Server) handleCreateLedger(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	sess := s.planner.Get(r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	s.render(w, r, views.LedgerPanel(sess.ID, sess.Ledger()))
}

// handleCreateLedgerSelect records a chip click: it funnels the chosen fork option back
// through the planner (Session.Choose Sends a canned message so the planner re-emits the
// ledger reflecting the decision), then returns the transcript fragment so the human's choice
// shows immediately. The refreshed ledger arrives separately over the `ledger` SSE nudge. An
// out-of-range index is a no-op inside Choose. Data endpoint: 503 with no planner, 400 on a
// malformed form, 404 for an unknown session.
func (s *Server) handleCreateLedgerSelect(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse the selection\n", http.StatusBadRequest)
		return
	}
	sess := s.planner.Get(r.FormValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	itemIdx, _ := strconv.Atoi(r.FormValue("item"))
	optIdx, _ := strconv.Atoi(r.FormValue("option"))
	sess.Choose(itemIdx, optIdx)
	s.render(w, r, views.WizardTranscript(sess.Messages()))
}

// handleCreateDraft returns just the draft panel fragment (T4.14) — the proposed spec files +
// seed issues the planner has drafted, with the APPROVE button when there is something to
// approve. It is the htmx swap target the page re-fetches on a `draft`/`turn` SSE nudge (and a
// slow periodic backstop). Whether APPROVE is wired (a Seeder is present) is passed to the view
// so a standalone serve shows the draft read-only rather than a dead button. Data endpoint: 503
// with no planner, 404 for an unknown session.
func (s *Server) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	sess := s.planner.Get(r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	s.render(w, r, views.DraftPanel(sess.ID, sess.Draft(), s.seeder != nil))
}

// handleCreateApprove is the consent gate (T4.14, specs/control-room.md "Gate on explicit
// human approval"): the human reviews the drafted spec + seed issues and approves, and only
// then is anything written. It commits the SERVER-SIDE draft — the trusted planner's latest
// snapshot — never browser-supplied content, so the human approves exactly what they reviewed
// and the planner stays the sole author of specs/issues. It assembles the SeedRequest from the
// session's draft + the agreed alignment-ledger items (the decisions sidecar) + the
// conversation transcript, and hands it to the Seeder, which performs the git/beads/artifact
// writes. The result fragment (created issue ids + commit, or the error) swaps into the page.
// Data endpoint: 503 with no planner; APPROVE-disabled notice with no seeder; 400 on a
// malformed form; 404 for an unknown session.
func (s *Server) handleCreateApprove(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	if s.seeder == nil {
		s.render(w, r, views.CreateApproveResult(wizard.SeedResult{}, "Approval is unavailable: the control room is not attached to a repository. Run the wizard under `harness run --serve-addr` to commit specs and seed issues."))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse the approval\n", http.StatusBadRequest)
		return
	}
	sess := s.planner.Get(r.FormValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	draft := sess.Draft()
	if draft.Empty() {
		s.render(w, r, views.CreateApproveResult(wizard.SeedResult{}, "Nothing to approve yet — keep the conversation going until the planner drafts a spec and seed issues."))
		return
	}
	res, err := s.seeder.Seed(r.Context(), wizard.SeedRequest{
		Summary:    draft.Summary,
		Specs:      draft.Specs,
		Issues:     draft.Issues,
		Decisions:  wizard.FinalizedDecisions(sess.Ledger()),
		Transcript: sess.Transcript(),
	})
	if err != nil {
		s.log.Warn("controlroom: wizard approval failed", "session", sess.ID, "err", err)
		s.render(w, r, views.CreateApproveResult(wizard.SeedResult{}, "Could not commit the draft: "+err.Error()))
		return
	}
	s.log.Info("controlroom: wizard draft approved", "session", sess.ID, "commit", res.Commit, "issues", len(res.Issues))
	s.render(w, r, views.CreateApproveResult(res, ""))
}

// handleResolve renders the Resolve-mode wizard page (T4.15, specs/control-room.md "Create and
// Resolve are the same component"): the wizard pre-loaded from a dead-lettered issue — the
// escalation, the governing spec slice, and the transcript that raised it. It mints a session
// grounded in that context (NewResolve), so the planner opens already engaged with the
// escalation. With no planner wired it shows the wizard-disabled notice; with no reader it shows
// the not-attached notice (the context comes from the read model); an unknown id or read fault
// renders the same chrome with the reason rather than a blank 500, mirroring handleIssue.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		s.render(w, r, views.ResolveMessage("The requirements planner is not configured — Resolve is available when the control room runs with `harness run --serve-addr` and a requirements_planner is set in harness.yaml."))
		return
	}
	if s.reader == nil {
		s.render(w, r, views.ResolveMessage("Not attached to a running factory — start the control room with `harness run --serve-addr` to resolve dead-lettered work."))
		return
	}
	id := r.PathValue("id")
	rc, err := s.reader.ResolveContext(r.Context(), s.repo, s.specDepth, id)
	if err != nil {
		s.log.Error("controlroom: resolve context read failed", "id", id, "err", err)
		s.render(w, r, views.ResolveMessage("Could not load issue "+id+" to resolve: "+err.Error()))
		return
	}
	sess := s.planner.NewResolve(wizard.ResolveSeed{
		IssueID:   rc.Issue.ID,
		Title:     rc.Issue.Title,
		Role:      rc.Issue.Role,
		Spec:      rc.Spec,
		Reason:    rc.Issue.DeadLetterReason,
		SpecSlice: rc.SpecSlice,
	})
	s.render(w, r, views.ResolvePage(sess.ID, rc, sess.Messages(), s.resolver != nil))
}

// handleResolveBlast returns the blast-radius preview fragment (T4.15): the read-only consequence
// of committing the session's drafted spec edits — the in-flight work the recompile sweep would
// reissue and the merged work it would re-derive (specs/control-room.md "this change re-pins and
// reissues these N in-flight items"). It is the htmx swap target the Resolve page re-fetches on a
// `draft`/`turn` SSE nudge, so the consequence updates as the planner refines the draft. Data
// endpoint: 503 with no planner/reader, 404 for an unknown session, 500 on a read fault.
func (s *Server) handleResolveBlast(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil || s.reader == nil {
		http.Error(w, "resolve unavailable: the control room is not attached to a running factory\n", http.StatusServiceUnavailable)
		return
	}
	sess := s.planner.Get(r.PathValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	draft := sess.Draft()
	paths := make([]string, 0, len(draft.Specs))
	for _, sp := range draft.Specs {
		paths = append(paths, sp.Path)
	}
	br, err := s.reader.BlastRadius(r.Context(), s.repo, s.specDepth, paths)
	if err != nil {
		s.log.Error("controlroom: blast radius read failed", "session", sess.ID, "err", err)
		http.Error(w, "could not compute the blast radius\n", http.StatusInternalServerError)
		return
	}
	s.render(w, r, views.ResolvePanel(sess.ID, draft, s.resolver != nil, br))
}

// handleResolveApprove is the Resolve consent gate (T4.15): the human approves the refined spec,
// and only then is it committed — the spec edit lands, the conversation provenance is stored, and
// the dead-lettered issue is returned to the ready pool to be re-dispatched against the clarified
// spec (the orchestrator's recompile sweep handles the rest of the blast radius). Like the Create
// gate it commits the SERVER-SIDE draft against the issue the server bound at mint
// (sess.ResolveIssue) — never browser-supplied content or issue id. Data endpoint: 503 with no
// planner; resolve-disabled notice with no resolver; 400 on a malformed form; 404 for an unknown
// session; a "nothing to resolve" notice when the draft has no spec edit.
func (s *Server) handleResolveApprove(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil {
		http.Error(w, "wizard unavailable: the requirements planner is not configured\n", http.StatusServiceUnavailable)
		return
	}
	if s.resolver == nil {
		s.render(w, r, views.ResolveApproveResult(wizard.ResolveResult{}, "Resolve is unavailable: the control room is not attached to a repository. Run the wizard under `harness run --serve-addr` to refine specs and reopen work."))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse the approval\n", http.StatusBadRequest)
		return
	}
	sess := s.planner.Get(r.FormValue("session"))
	if sess == nil {
		http.Error(w, "unknown wizard session\n", http.StatusNotFound)
		return
	}
	draft := sess.Draft()
	if len(draft.Specs) == 0 {
		s.render(w, r, views.ResolveApproveResult(wizard.ResolveResult{}, "Nothing to resolve yet — keep the conversation going until the planner drafts a spec refinement."))
		return
	}
	res, err := s.resolver.Resolve(r.Context(), wizard.ResolveRequest{
		IssueID:    sess.ResolveIssue(),
		Summary:    draft.Summary,
		Specs:      draft.Specs,
		Decisions:  wizard.FinalizedDecisions(sess.Ledger()),
		Transcript: sess.Transcript(),
	})
	if err != nil {
		s.log.Warn("controlroom: resolve approval failed", "session", sess.ID, "issue", sess.ResolveIssue(), "err", err)
		s.render(w, r, views.ResolveApproveResult(wizard.ResolveResult{}, "Could not commit the resolution: "+err.Error()))
		return
	}
	s.log.Info("controlroom: resolve draft approved", "session", sess.ID, "commit", res.Commit, "reopened", res.ReopenedIssue)
	s.render(w, r, views.ResolveApproveResult(res, ""))
}

// Handler exposes the routed mux for embedding (e.g. behind middleware) and for httptest.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe binds addr and serves until ctx is canceled, then drains in-flight
// requests within a short grace period. It returns nil on a clean ctx-driven shutdown and
// the underlying error otherwise — matching the run loop's "cancel == clean stop" contract.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Derive every request context from ctx so a shutdown promptly cancels
		// long-lived SSE streams instead of waiting out the drain timeout on them.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.log.Info("controlroom: serving", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		s.log.Info("controlroom: stopped")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// cacheStatic adds a long-lived immutable cache header to embedded assets. They are
// content-fixed for the lifetime of a binary, so aggressive caching is safe and spares
// the network on every page load.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// noopWriter discards log output; used only as the default logger sink when a caller
// constructs a Server without one (e.g. in tests that do not assert on logs).
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
