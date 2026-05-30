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
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/Loxstomper/harness/internal/controlroom/assets"
	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/controlroom/views"
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
	Reader     *query.Reader
	StageOrder []string
}

// Server renders the control room. It is an http.Handler (via Handler) so its routes can
// be exercised with httptest without binding a socket, and it offers ListenAndServe for
// the `harness serve` command with context-driven graceful shutdown.
type Server struct {
	mux        *http.ServeMux
	log        *slog.Logger
	version    string
	events     *live.Hub
	reader     *query.Reader
	stageOrder []string
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
		reader:     opts.Reader,
		stageOrder: opts.StageOrder,
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
	s.mux.HandleFunc("GET /board", s.handleBoard)             // T4.4 — kanban over beads
	s.mux.HandleFunc("GET /board/cards", s.handleBoardCards)  // the htmx/SSE live fragment

	// Every remaining navigation destination resolves to a placeholder until its
	// data-backed view lands; registering them from views.NavItems keeps the navigation
	// and the routes a single source of truth. `implemented` excludes the views wired
	// above so the mux is not asked to register a duplicate pattern.
	implemented := map[string]bool{"board": true}
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
