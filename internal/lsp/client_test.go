package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a scripted language server over two in-memory pipes. It speaks the same
// Content-Length framing as a real server, so it drives the *real* Client end to end.
type fakeServer struct {
	t      *testing.T
	in     *bufio.Reader // client -> server (the server reads requests here)
	out    io.Writer     // server -> client (the server writes responses here)
	wmu    sync.Mutex
	gotMu  sync.Mutex
	gotCfg []byte // the client's reply to our workspace/configuration request
}

// newClientWithServer wires a Client to a fakeServer over two io.Pipes and runs the
// server loop in a goroutine. The returned cleanup closes everything.
func newClientWithServer(t *testing.T, handler func(s *fakeServer, method string, id *int, params json.RawMessage)) (*Client, *fakeServer) {
	t.Helper()
	cToSr, cToSw := io.Pipe() // client stdin  -> server stdin
	sToCr, sToCw := io.Pipe() // server stdout -> client stdout

	srv := &fakeServer{t: t, in: bufio.NewReader(cToSr), out: sToCw}
	closer := closerFunc(func() error {
		_ = cToSw.Close()
		_ = sToCw.Close()
		return nil
	})
	c := New(sToCr, cToSw, closer, nil)

	go func() {
		for {
			payload, err := readFrame(srv.in)
			if err != nil {
				return
			}
			var in struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(payload, &in); err != nil {
				continue
			}
			if in.Method == "" && in.ID != nil {
				// This is the client's REPLY to a server->client request we sent.
				srv.gotMu.Lock()
				srv.gotCfg = append([]byte(nil), in.Result...)
				srv.gotMu.Unlock()
				continue
			}
			handler(srv, in.Method, in.ID, in.Params)
		}
	}()
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func (s *fakeServer) write(v any) {
	s.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		s.t.Fatalf("server marshal: %v", err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return
	}
	_, _ = s.out.Write(body)
}

// respond writes a JSON-RPC result for a request id.
func (s *fakeServer) respond(id int, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// notify writes a server->client notification.
func (s *fakeServer) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// requestConfiguration sends a workspace/configuration request to the client and asserts
// the client answered with an array of the right length (defaults).
func (s *fakeServer) requestConfiguration(id int, items int) {
	s.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "workspace/configuration",
		"params": map[string]any{"items": make([]any, items)},
	})
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func loc(uri string, line int) Location {
	return Location{URI: uri, Range: Range{Start: Position{Line: line}, End: Position{Line: line, Character: 3}}}
}

// standardHandler answers the common request set so individual tests stay short.
func standardHandler(s *fakeServer, method string, id *int, params json.RawMessage) {
	switch method {
	case "initialize":
		s.respond(*id, map[string]any{"capabilities": map[string]any{}})
	case "shutdown":
		s.respond(*id, nil)
	case "textDocument/didOpen":
		// Push diagnostics asynchronously, as a real server does after an open.
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &p)
		s.notify("textDocument/publishDiagnostics", map[string]any{
			"uri": p.TextDocument.URI,
			"diagnostics": []map[string]any{
				{"range": Range{}, "severity": 1, "message": "undefined: Bar", "source": "compiler"},
			},
		})
	case "textDocument/definition":
		s.respond(*id, []Location{loc("file:///workspace/a.go", 10)})
	case "textDocument/references":
		s.respond(*id, []Location{loc("file:///workspace/a.go", 10), loc("file:///workspace/b.go", 2)})
	case "textDocument/implementation":
		// Return the LocationLink shape to exercise normalization.
		s.respond(*id, []map[string]any{
			{"targetUri": "file:///workspace/impl.go", "targetRange": Range{Start: Position{Line: 5}}},
		})
	case "textDocument/hover":
		s.respond(*id, map[string]any{"contents": map[string]any{"kind": "markdown", "value": "func Foo() error"}})
	case "textDocument/documentSymbol":
		s.respond(*id, []map[string]any{
			{"name": "Foo", "kind": 12, "range": Range{}, "selectionRange": Range{}, "children": []map[string]any{
				{"name": "inner", "kind": 13, "range": Range{}, "selectionRange": Range{}},
			}},
		})
	case "workspace/symbol":
		s.respond(*id, []map[string]any{
			{"name": "Foo", "kind": 12, "location": loc("file:///workspace/a.go", 10)},
		})
	case "textDocument/rename":
		s.respond(*id, map[string]any{"changes": map[string]any{
			"file:///workspace/a.go": []TextEdit{{Range: Range{}, NewText: "Renamed"}},
		}})
	case "textDocument/codeAction":
		s.respond(*id, []map[string]any{
			{"title": "Organize Imports", "kind": "source.organizeImports", "edit": map[string]any{
				"changes": map[string]any{"file:///workspace/a.go": []TextEdit{{NewText: "import x"}}},
			}},
		})
	default:
		if id != nil {
			s.respond(*id, nil) // never leave a request hanging
		}
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestInitializeAndShutdown(t *testing.T) {
	c, srv := newClientWithServer(t, standardHandler)
	if err := c.Initialize(ctx(t), "file:///workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// After init, the server pokes the client with a configuration request; the client
	// must answer with a same-length array of defaults so the server never stalls.
	srv.requestConfiguration(9001, 1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.gotMu.Lock()
		got := srv.gotCfg
		srv.gotMu.Unlock()
		if got != nil {
			if strings.TrimSpace(string(got)) != "[null]" {
				t.Fatalf("configuration reply = %s, want [null]", got)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Shutdown(ctx(t)); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestDefinitionAndReferences(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	defs, err := c.Definition(ctx(t), "file:///workspace/a.go", 1, 4)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if len(defs) != 1 || defs[0].URI != "file:///workspace/a.go" {
		t.Fatalf("definition = %+v", defs)
	}
	refs, err := c.References(ctx(t), "file:///workspace/a.go", 1, 4, true)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("references = %+v, want 2", refs)
	}
}

func TestImplementationNormalizesLocationLink(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	impls, err := c.Implementation(ctx(t), "file:///workspace/a.go", 1, 4)
	if err != nil {
		t.Fatalf("implementation: %v", err)
	}
	if len(impls) != 1 || impls[0].URI != "file:///workspace/impl.go" || impls[0].Range.Start.Line != 5 {
		t.Fatalf("implementation = %+v, want normalized targetUri/targetRange", impls)
	}
}

func TestHoverText(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	h, err := c.Hover(ctx(t), "file:///workspace/a.go", 1, 4)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if h.Text() != "func Foo() error" {
		t.Fatalf("hover text = %q", h.Text())
	}
}

func TestDocumentSymbolFlattensHierarchy(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	syms, err := c.DocumentSymbol(ctx(t), "file:///workspace/a.go")
	if err != nil {
		t.Fatalf("documentSymbol: %v", err)
	}
	if len(syms) != 2 || syms[0].Name != "Foo" || syms[1].Name != "inner" {
		t.Fatalf("documentSymbol = %+v, want Foo + nested inner flattened", syms)
	}
	if syms[0].Location.URI != "file:///workspace/a.go" {
		t.Fatalf("hierarchical symbol missing document URI: %+v", syms[0])
	}
}

func TestWorkspaceSymbol(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	syms, err := c.WorkspaceSymbol(ctx(t), "Foo")
	if err != nil {
		t.Fatalf("workspace/symbol: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "Foo" || syms[0].Location.URI == "" {
		t.Fatalf("workspace/symbol = %+v", syms)
	}
}

func TestRenameAndCodeAction(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	we, err := c.Rename(ctx(t), "file:///workspace/a.go", 1, 4, "Renamed")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	files := we.Files()
	if edits := files["file:///workspace/a.go"]; len(edits) != 1 || edits[0].NewText != "Renamed" {
		t.Fatalf("rename edits = %+v", files)
	}
	actions, err := c.CodeAction(ctx(t), "file:///workspace/a.go", Range{})
	if err != nil {
		t.Fatalf("codeAction: %v", err)
	}
	if len(actions) != 1 || actions[0].Title != "Organize Imports" || actions[0].Edit == nil {
		t.Fatalf("codeAction = %+v", actions)
	}
}

func TestDiagnosticsWaitsForPublish(t *testing.T) {
	c, _ := newClientWithServer(t, standardHandler)
	uri := "file:///workspace/a.go"
	// didOpen triggers the server to push diagnostics; Diagnostics must block until then.
	if err := c.DidOpen(uri, "go", "package main\n", 1); err != nil {
		t.Fatalf("didOpen: %v", err)
	}
	diags, err := c.Diagnostics(ctx(t), uri)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != "undefined: Bar" || diags[0].Severity != 1 {
		t.Fatalf("diagnostics = %+v", diags)
	}
}

func TestCallAfterTransportDeath(t *testing.T) {
	c, srv := newClientWithServer(t, func(*fakeServer, string, *int, json.RawMessage) {})
	// Kill the transport: closing the server's write end gives the client's reader EOF.
	_ = srv.out.(*io.PipeWriter).Close()
	// A call now must fail fast (ErrClosed), not hang.
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Definition(cctx, "file:///workspace/a.go", 0, 0); err == nil {
		t.Fatal("expected error after transport death, got nil")
	}
}
