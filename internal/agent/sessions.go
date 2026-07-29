package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/Loxstomper/software-factory/internal/lsp"
	"github.com/Loxstomper/software-factory/internal/sandbox"
	"github.com/Loxstomper/software-factory/internal/sandbox/lsmanifest"
)

// ErrNoSemanticSession is returned by a Sessions query when no language server can serve
// the file: the sandbox has no streamed-session support, no manifest is baked into the
// image, or the file's language has no entry. It is the signal the semantic tools (T6.2/
// T6.3) read to degrade — silently to grep for reads, loudly for writes (specs/
// components/agent.md "reads degrade silently, writes degrade loudly").
var ErrNoSemanticSession = errors.New("agent: no semantic session for file")

// Sessions is the per-invocation LSP session manager (Phase 6, T6.1): one warm
// language-server session per language, launched lazily on the first semantic call and
// kept in sync with the worktree by the edit tools, then torn down with the invocation.
//
// It owns the sandbox/worktree specifics the generic lsp.Client does not: resolving the
// server from the IMAGE's baked manifest (so the tool layer is image-agnostic and grows
// no per-language branch), reading file content out of the sandbox for didOpen, and
// building the file:// URIs gopls speaks. The "prefer semantic, fall back to text" rule
// is structural — a sandbox with no SessionOpener yields ErrNoSemanticSession from every
// query, so the agent never picks the mechanism.
//
// It implements editNotifier: every edit_file/write_file notifies it so a running
// session's overlay never goes stale (this couples the edit tools to the session by
// design, not as a bolt-on). Safe for concurrent use.
type Sessions struct {
	exec   sandbox.Sandbox       // reads the manifest + file contents via Exec
	opener sandbox.SessionOpener // nil when the sandbox can't run streamed sessions
	root   string                // absolute worktree root inside the sandbox
	log    *slog.Logger

	mu       sync.Mutex
	manifest *lsmanifest.Manifest
	mfErr    error
	mfDone   bool
	servers  map[string]*serverSession // keyed by languageId
	closed   bool
}

// serverSession is one running language server (its lsp.Client + the stream behind it)
// plus the set of documents it has open and at which version, so edits become didChange
// rather than a stale re-open.
type serverSession struct {
	client *lsp.Client
	stream sandbox.SessionStream
	langID string

	omu  sync.Mutex
	open map[string]int // uri -> last version sent
}

// NewSessions builds a manager over a sandbox. If the sandbox supports streamed sessions
// (SessionOpener), semantic queries are live; otherwise every query degrades to
// ErrNoSemanticSession and edits are no-ops. A nil logger discards.
func NewSessions(sb sandbox.Sandbox, log *slog.Logger) *Sessions {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Sessions{exec: sb, log: log, servers: make(map[string]*serverSession)}
	if op, ok := sb.(sandbox.SessionOpener); ok {
		s.opener = op
		s.root = op.Workdir()
	}
	return s
}

// NotifyEdit keeps running sessions in sync after an edit. It deliberately does NOT
// launch a server: a server is launched only on the first *semantic* call (the spec's
// lazy-launch), and a server launched later reads current disk content at didOpen, so an
// edit before any semantic call needs no notification. Best-effort: a sync failure is
// logged, never surfaced to the edit tool (a stale overlay must not fail an edit).
func (s *Sessions) NotifyEdit(ctx context.Context, relPath, content string) {
	if s.opener == nil {
		return
	}
	s.mu.Lock()
	if s.closed || len(s.servers) == 0 {
		s.mu.Unlock()
		return
	}
	// A server exists, so the manifest is already loaded; resolve the file's language
	// from it and find the matching running session.
	var ss *serverSession
	if s.manifest != nil {
		if srv, ok := s.manifest.ResolveExtension(relPath); ok {
			ss = s.servers[srv.LanguageID]
		}
	}
	s.mu.Unlock()
	if ss == nil {
		return
	}
	if err := ss.sync(s.uriFor(relPath), content); err != nil {
		s.log.DebugContext(ctx, "agent: lsp didChange failed", "path", relPath, "err", err)
	}
}

// Close tears every session down: an orderly LSP shutdown/exit, then the stream (which
// kills the server process). Idempotent. The sandbox teardown is the backstop, so a
// failure here is swallowed — Close is a courtesy to release host-side resources before
// the container is reaped.
func (s *Sessions) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	servers := s.servers
	s.servers = make(map[string]*serverSession)
	s.mu.Unlock()

	for _, ss := range servers {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = ss.client.Shutdown(sctx)
		cancel()
		_ = ss.client.Close() // closes the underlying stream (kills the process)
	}
}

// --- semantic operations (the engine the T6.2/T6.3 tools wrap) --------------------

// Definition returns where the symbol at (line,char) is defined. line/char are 0-based
// LSP positions.
func (s *Sessions) Definition(ctx context.Context, relPath string, line, char int) ([]lsp.Location, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.Definition(ctx, uri, line, char)
}

// References returns all references to the symbol at (line,char).
func (s *Sessions) References(ctx context.Context, relPath string, line, char int, includeDecl bool) ([]lsp.Location, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.References(ctx, uri, line, char, includeDecl)
}

// Implementation returns implementations of the interface/method at (line,char).
func (s *Sessions) Implementation(ctx context.Context, relPath string, line, char int) ([]lsp.Location, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.Implementation(ctx, uri, line, char)
}

// Hover returns type/signature/doc text for the symbol at (line,char).
func (s *Sessions) Hover(ctx context.Context, relPath string, line, char int) (lsp.Hover, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return lsp.Hover{}, err
	}
	return ss.client.Hover(ctx, uri, line, char)
}

// DocumentSymbol returns the symbols declared in a file.
func (s *Sessions) DocumentSymbol(ctx context.Context, relPath string) ([]lsp.Symbol, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.DocumentSymbol(ctx, uri)
}

// Diagnostics returns the compile/type problems the server published for a file (it
// blocks for the first batch after opening, bounded by ctx).
func (s *Sessions) Diagnostics(ctx context.Context, relPath string) ([]lsp.Diagnostic, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.Diagnostics(ctx, uri)
}

// Rename computes the project-wide WorkspaceEdit to rename the symbol at (line,char).
// Applying the edit to the worktree is the transformation tool's job (T6.3).
func (s *Sessions) Rename(ctx context.Context, relPath string, line, char int, newName string) (lsp.WorkspaceEdit, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return lsp.WorkspaceEdit{}, err
	}
	return ss.client.Rename(ctx, uri, line, char, newName)
}

// CodeAction returns the server's offered actions for a range in a file.
func (s *Sessions) CodeAction(ctx context.Context, relPath string, rng lsp.Range) ([]lsp.CodeAction, error) {
	ss, uri, err := s.prepareFile(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return ss.client.CodeAction(ctx, uri, rng)
}

// WorkspaceSymbol searches project-wide for symbols by name (the find_symbol engine). It
// is not file-anchored, so it launches the server for the given languageId.
func (s *Sessions) WorkspaceSymbol(ctx context.Context, langID, query string) ([]lsp.Symbol, error) {
	srv := s.resolveLang(ctx, langID)
	if srv == nil {
		return nil, ErrNoSemanticSession
	}
	ss, err := s.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	return ss.client.WorkspaceSymbol(ctx, query)
}

// --- launch / resolution ----------------------------------------------------------

// prepareFile resolves the file's language, launches its server if needed, and ensures
// the document is open with current content before a query runs.
func (s *Sessions) prepareFile(ctx context.Context, relPath string) (*serverSession, string, error) {
	srv := s.resolveExt(ctx, relPath)
	if srv == nil {
		return nil, "", ErrNoSemanticSession
	}
	ss, err := s.session(ctx, srv)
	if err != nil {
		return nil, "", err
	}
	uri := s.uriFor(relPath)
	if err := ss.ensureOpen(ctx, s, relPath, uri); err != nil {
		return nil, "", err
	}
	return ss, uri, nil
}

// resolveExt returns the manifest server for a file by extension, or nil if semantic
// support is unavailable for any reason (no opener, no manifest, no entry).
func (s *Sessions) resolveExt(ctx context.Context, relPath string) *lsmanifest.Server {
	if s.opener == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.manifestLocked(ctx)
	if m == nil {
		return nil
	}
	if srv, ok := m.ResolveExtension(relPath); ok {
		return srv
	}
	return nil
}

func (s *Sessions) resolveLang(ctx context.Context, langID string) *lsmanifest.Server {
	if s.opener == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.manifestLocked(ctx)
	if m == nil {
		return nil
	}
	if srv, ok := m.ResolveLanguageID(langID); ok {
		return srv
	}
	return nil
}

// manifestLocked reads and parses the image's baked manifest once, caching the result
// (success or failure). Caller holds s.mu. A missing/invalid manifest is a degrade, not
// an error — it leaves the manager semantic-less and every query falls back.
func (s *Sessions) manifestLocked(ctx context.Context) *lsmanifest.Manifest {
	if s.mfDone {
		return s.manifest
	}
	s.mfDone = true
	res, err := s.exec.Exec(ctx, sandbox.Command{Path: "cat", Args: []string{"--", lsmanifest.ManifestPath}})
	if err != nil {
		s.mfErr = err
		s.log.DebugContext(ctx, "agent: read language-server manifest", "err", err)
		return nil
	}
	if res.ExitCode != 0 {
		s.mfErr = fmt.Errorf("manifest %s not present (exit %d)", lsmanifest.ManifestPath, res.ExitCode)
		s.log.DebugContext(ctx, "agent: no language-server manifest in image", "path", lsmanifest.ManifestPath)
		return nil
	}
	m, err := lsmanifest.Parse(res.Stdout)
	if err != nil {
		s.mfErr = err
		s.log.WarnContext(ctx, "agent: invalid language-server manifest", "err", err)
		return nil
	}
	s.manifest = m
	return m
}

// session returns the running session for a server, launching (and initializing) it on
// first use. Holds s.mu across the launch handshake so two concurrent first-calls don't
// race two servers into existence; queries run lock-free once it returns.
func (s *Sessions) session(ctx context.Context, srv *lsmanifest.Server) (*serverSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrNoSemanticSession
	}
	if ss := s.servers[srv.LanguageID]; ss != nil {
		return ss, nil
	}

	stream, err := s.opener.OpenSession(ctx, sandbox.Command{Path: srv.Command[0], Args: srv.Command[1:]})
	if err != nil {
		return nil, fmt.Errorf("agent: launch %s server: %w", srv.LanguageID, err)
	}
	client := lsp.New(stream.Stdout(), stream.Stdin(), stream, s.log)
	if err := client.Initialize(ctx, s.rootURI()); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("agent: initialize %s server: %w", srv.LanguageID, err)
	}
	ss := &serverSession{client: client, stream: stream, langID: srv.LanguageID, open: make(map[string]int)}
	s.servers[srv.LanguageID] = ss
	s.log.DebugContext(ctx, "agent: launched language server", "language", srv.LanguageID)
	return ss, nil
}

// readFile reads a worktree file's content out of the sandbox (for the didOpen overlay).
func (s *Sessions) readFile(ctx context.Context, relPath string) (string, error) {
	res, err := s.exec.Exec(ctx, sandbox.Command{Path: "cat", Args: []string{"--", relPath}})
	if err != nil {
		return "", fmt.Errorf("agent: read %s for session: %w", relPath, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("agent: read %s for session: exit %d", relPath, res.ExitCode)
	}
	return string(res.Stdout), nil
}

func (s *Sessions) rootURI() string { return "file://" + s.root }
func (s *Sessions) uriFor(rel string) string {
	return "file://" + path.Join(s.root, rel)
}

// --- per-session document bookkeeping ---------------------------------------------

// ensureOpen opens a document on the session with current disk content if it is not open
// yet. Idempotent and race-safe (a concurrent open wins; we do not re-open).
func (ss *serverSession) ensureOpen(ctx context.Context, s *Sessions, relPath, uri string) error {
	ss.omu.Lock()
	if _, open := ss.open[uri]; open {
		ss.omu.Unlock()
		return nil
	}
	ss.omu.Unlock()

	content, err := s.readFile(ctx, relPath)
	if err != nil {
		return err
	}

	ss.omu.Lock()
	if _, open := ss.open[uri]; open {
		ss.omu.Unlock()
		return nil // raced with another open
	}
	ss.open[uri] = 1
	ss.omu.Unlock()
	return ss.client.DidOpen(uri, ss.langID, content, 1)
}

// sync applies an edit to the session: didChange if the doc is open, else didOpen. The
// version monotonically increases, as LSP requires.
func (ss *serverSession) sync(uri, content string) error {
	ss.omu.Lock()
	v, open := ss.open[uri]
	v++
	ss.open[uri] = v
	ss.omu.Unlock()
	if !open {
		return ss.client.DidOpen(uri, ss.langID, content, v)
	}
	return ss.client.DidChange(uri, content, v)
}

// compile-time proof Sessions is a usable editNotifier for the workspace edit tools.
var _ editNotifier = (*Sessions)(nil)
