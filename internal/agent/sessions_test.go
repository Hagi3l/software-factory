package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/sandbox/lsmanifest"
)

const goManifest = `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":["gopls","serve"],"rootMarkers":["go.mod"]}]}`

// lspSandbox is a fake sandbox that serves the manifest + file reads through Exec and a
// scripted LSP server through OpenSession, so the session manager is driven exactly as it
// would drive a real gopls in a real container — no Docker required.
type lspSandbox struct {
	files    map[string]string // relpath -> content
	manifest string            // "" => no manifest baked in
	workdir  string

	mu       sync.Mutex
	sessions int        // OpenSession call count
	rec      *recorder  // events the fake server observed
}

func (s *lspSandbox) ID() string { return "lsp-sbx" }

func (s *lspSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if cmd.Path == "cat" && len(cmd.Args) == 2 && cmd.Args[0] == "--" {
		p := cmd.Args[1]
		if p == lsmanifest.ManifestPath {
			if s.manifest == "" {
				return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("no manifest")}, nil
			}
			return sandbox.ExecResult{Stdout: []byte(s.manifest)}, nil
		}
		if c, ok := s.files[p]; ok {
			return sandbox.ExecResult{Stdout: []byte(c)}, nil
		}
		return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("no such file")}, nil
	}
	return sandbox.ExecResult{ExitCode: 127}, nil
}

func (s *lspSandbox) Teardown(context.Context) error { return nil }

func (s *lspSandbox) Workdir() string { return s.workdir }

func (s *lspSandbox) OpenSession(_ context.Context, _ sandbox.Command) (sandbox.SessionStream, error) {
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()

	cToSr, cToSw := io.Pipe() // client -> server
	sToCr, sToCw := io.Pipe() // server -> client
	go fakeLangServer(bufio.NewReader(cToSr), sToCw, s.rec)
	return &pipeStream{stdin: cToSw, stdout: sToCr, closers: []io.Closer{cToSw, sToCw}}, nil
}

func (s *lspSandbox) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

type pipeStream struct {
	stdin   io.WriteCloser
	stdout  io.Reader
	closers []io.Closer
	once    sync.Once
}

func (p *pipeStream) Stdin() io.WriteCloser { return p.stdin }
func (p *pipeStream) Stdout() io.Reader     { return p.stdout }
func (p *pipeStream) Close() error {
	p.once.Do(func() {
		for _, c := range p.closers {
			_ = c.Close()
		}
	})
	return nil
}

// recorder captures the document-sync events the fake server observed, so tests can
// assert didOpen/didChange ordering and versions.
type recorder struct {
	mu     sync.Mutex
	events []docEvent
}

type docEvent struct {
	Method  string
	URI     string
	Version int
	Text    string
}

func (r *recorder) add(e docEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []docEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]docEvent(nil), r.events...)
}

// waitFor polls until pred is satisfied by the recorded events or the deadline passes.
func (r *recorder) waitFor(t *testing.T, pred func([]docEvent) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred(r.snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for events; got %+v", r.snapshot())
}

// fakeLangServer is a minimal scripted LSP server: it answers initialize/definition/
// shutdown, records didOpen/didChange, and pushes diagnostics after an open.
func fakeLangServer(in *bufio.Reader, out io.Writer, rec *recorder) {
	var wmu sync.Mutex
	write := func(v any) {
		body, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(body))
		_, _ = out.Write(body)
	}
	for {
		payload, err := readLSPFrame(in)
		if err != nil {
			return
		}
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(payload, &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": nil})
		case "textDocument/didOpen":
			var p struct {
				TD struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
					Text    string `json:"text"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if rec != nil {
				rec.add(docEvent{Method: "didOpen", URI: p.TD.URI, Version: p.TD.Version, Text: p.TD.Text})
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{
				"uri":         p.TD.URI,
				"diagnostics": []map[string]any{{"range": map[string]any{}, "severity": 2, "message": "unused"}},
			}})
		case "textDocument/didChange":
			var p struct {
				TD struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
				Changes []struct {
					Text string `json:"text"`
				} `json:"contentChanges"`
			}
			_ = json.Unmarshal(m.Params, &p)
			text := ""
			if len(p.Changes) > 0 {
				text = p.Changes[0].Text
			}
			if rec != nil {
				rec.add(docEvent{Method: "didChange", URI: p.TD.URI, Version: p.TD.Version, Text: text})
			}
		case "textDocument/definition":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": []map[string]any{
				{"uri": "file:///work/a.go", "range": map[string]any{"start": map[string]int{"line": 7, "character": 0}, "end": map[string]int{"line": 7, "character": 3}}},
			}})
		case "textDocument/references":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": []map[string]any{
				{"uri": "file:///work/a.go", "range": map[string]any{"start": map[string]int{"line": 2, "character": 5}, "end": map[string]int{"line": 2, "character": 8}}},
				{"uri": "file:///work/b.go", "range": map[string]any{"start": map[string]int{"line": 9, "character": 1}, "end": map[string]int{"line": 9, "character": 4}}},
			}})
		case "textDocument/hover":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": "func greet(name string) string"},
			}})
		case "workspace/symbol":
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": []map[string]any{
				{"name": "greet", "kind": 12, "location": map[string]any{"uri": "file:///work/a.go", "range": map[string]any{"start": map[string]int{"line": 2, "character": 5}, "end": map[string]int{"line": 2, "character": 10}}}},
			}})
		default:
			if m.ID != nil {
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": nil})
			}
		}
	}
}

// readLSPFrame is the test-side framer (the production one is internal to package lsp).
func readLSPFrame(br *bufio.Reader) ([]byte, error) {
	var length int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			fmt.Sscanf(strings.TrimSpace(value), "%d", &length)
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

func newLSPSandbox(files map[string]string) *lspSandbox {
	return &lspSandbox{files: files, manifest: goManifest, workdir: "/work", rec: &recorder{}}
}

func bg(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestSessionsDegradeWithoutOpener(t *testing.T) {
	// A sandbox that doesn't implement SessionOpener => every query degrades and edits
	// are no-ops (the text-floor fallback path). scriptedSandbox (workspace_test.go) is
	// exactly such a sandbox.
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	}}
	s := NewSessions(sb, nil)
	if _, err := s.Definition(bg(t), "a.go", 0, 0); !errors.Is(err, ErrNoSemanticSession) {
		t.Fatalf("Definition err = %v, want ErrNoSemanticSession", err)
	}
	s.NotifyEdit(bg(t), "a.go", "x") // must not panic
	s.Close()
}

func TestSessionsNoManifestDegrades(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	sb.manifest = "" // image has no language-server manifest
	s := NewSessions(sb, nil)
	if _, err := s.Definition(bg(t), "a.go", 0, 0); !errors.Is(err, ErrNoSemanticSession) {
		t.Fatalf("Definition err = %v, want ErrNoSemanticSession", err)
	}
	if sb.openCount() != 0 {
		t.Fatalf("launched a server despite no manifest (%d)", sb.openCount())
	}
}

func TestSessionsUnknownExtensionDegrades(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"readme.md": "# hi\n"})
	s := NewSessions(sb, nil)
	if _, err := s.Definition(bg(t), "readme.md", 0, 0); !errors.Is(err, ErrNoSemanticSession) {
		t.Fatalf("Definition err = %v, want ErrNoSemanticSession for .md", err)
	}
}

func TestSessionsDefinitionLaunchesAndOpens(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc main() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	defs, err := s.Definition(bg(t), "a.go", 2, 5)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) != 1 || defs[0].Range.Start.Line != 7 {
		t.Fatalf("Definition result = %+v", defs)
	}
	if sb.openCount() != 1 {
		t.Fatalf("OpenSession count = %d, want 1", sb.openCount())
	}
	// The server must have seen didOpen for a.go carrying current disk content, at the
	// absolute in-sandbox URI built from Workdir.
	evs := sb.rec.snapshot()
	if len(evs) == 0 || evs[0].Method != "didOpen" || evs[0].URI != "file:///work/a.go" {
		t.Fatalf("first event = %+v, want didOpen file:///work/a.go", evs)
	}
	if !strings.Contains(evs[0].Text, "func main()") || evs[0].Version != 1 {
		t.Fatalf("didOpen content/version = %+v", evs[0])
	}
}

func TestSessionsLazyLaunchReusesServer(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n", "b.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	if _, err := s.Definition(bg(t), "a.go", 0, 0); err != nil {
		t.Fatalf("Definition a: %v", err)
	}
	if _, err := s.Definition(bg(t), "b.go", 0, 0); err != nil {
		t.Fatalf("Definition b: %v", err)
	}
	if sb.openCount() != 1 {
		t.Fatalf("OpenSession count = %d, want 1 (reused per language)", sb.openCount())
	}
}

func TestSessionsEditSyncsRunningSession(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	// Launch + open a.go.
	if _, err := s.Definition(bg(t), "a.go", 0, 0); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	// An edit to the open doc must become didChange at version 2 with the new content.
	s.NotifyEdit(bg(t), "a.go", "package main\n// edited\n")
	sb.rec.waitFor(t, func(evs []docEvent) bool {
		for _, e := range evs {
			if e.Method == "didChange" && e.URI == "file:///work/a.go" && e.Version == 2 && strings.Contains(e.Text, "edited") {
				return true
			}
		}
		return false
	})
	// An edit to a *new* file on the running session must become didOpen (not silently dropped).
	s.NotifyEdit(bg(t), "b.go", "package main\n")
	sb.rec.waitFor(t, func(evs []docEvent) bool {
		for _, e := range evs {
			if e.Method == "didOpen" && e.URI == "file:///work/b.go" {
				return true
			}
		}
		return false
	})
}

func TestSessionsNotifyBeforeLaunchIsNoop(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n// ondisk\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	// No server is running yet: the edit must NOT launch one (lazy launch is on the
	// first *semantic* call), so nothing is recorded and no session opened.
	s.NotifyEdit(bg(t), "a.go", "package main\n// inmemory\n")
	if sb.openCount() != 0 {
		t.Fatalf("NotifyEdit launched a server (%d); lazy launch must wait for a semantic call", sb.openCount())
	}
	// The later semantic call launches and didOpens with current DISK content (the
	// pre-launch edit needs no notification because the open reads fresh).
	if _, err := s.Definition(bg(t), "a.go", 0, 0); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	evs := sb.rec.snapshot()
	if len(evs) == 0 || evs[0].Method != "didOpen" || !strings.Contains(evs[0].Text, "ondisk") {
		t.Fatalf("didOpen should carry disk content, got %+v", evs)
	}
}

func TestSessionsDiagnostics(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	diags, err := s.Diagnostics(bg(t), "a.go")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != "unused" {
		t.Fatalf("Diagnostics = %+v", diags)
	}
}

func TestSessionsCloseShutsDown(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	if _, err := s.Definition(bg(t), "a.go", 0, 0); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	s.Close()
	// After close, a query must fail (no semantic session), not hang or panic.
	if _, err := s.Definition(bg(t), "a.go", 0, 0); err == nil {
		t.Fatal("expected error after Close, got nil")
	}
	s.Close() // idempotent
}
