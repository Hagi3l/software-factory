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
	"github.com/Loxstomper/harness/internal/controlroom/views"
)

// Options configures a Server. All fields are optional: Version is stamped into the
// landing page for provenance, and Logger defaults to a stderr text logger.
type Options struct {
	Version string
	Logger  *slog.Logger
}

// Server renders the control room. It is an http.Handler (via Handler) so its routes can
// be exercised with httptest without binding a socket, and it offers ListenAndServe for
// the `harness serve` command with context-driven graceful shutdown.
type Server struct {
	mux     *http.ServeMux
	log     *slog.Logger
	version string
}

// New builds the server and registers every route. Routes are registered eagerly so a
// constructed Server is immediately serveable and fully testable.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	s := &Server{mux: http.NewServeMux(), log: log, version: opts.Version}
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

	// Landing page.
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, views.Home(s.version))
	})

	// Each navigation destination resolves to a real page. These are placeholders until
	// their data-backed views land (T4.2+); registering them from views.NavItems keeps
	// the navigation and the routes a single source of truth.
	for _, item := range views.NavItems {
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
