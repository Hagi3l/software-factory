package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// semanticToolByName builds the semantic read tools over the given Sessions and returns the
// one named.
func semanticToolByName(t *testing.T, s *Sessions, name string) Tool {
	t.Helper()
	for _, tl := range SemanticReadTools(s) {
		if tl.Def().Name == name {
			return tl
		}
	}
	t.Fatalf("no semantic tool named %q", name)
	return nil
}

func TestSemanticToolSet(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	got := map[string]bool{}
	for _, tl := range SemanticReadTools(s) {
		got[tl.Def().Name] = true
		if !json.Valid(tl.Def().Params) {
			t.Errorf("tool %s has invalid JSON schema params: %s", tl.Def().Name, tl.Def().Params)
		}
	}
	for _, want := range []string{"find_symbol", "references", "definition", "implementation", "hover", "diagnostics"} {
		if !got[want] {
			t.Errorf("missing semantic tool %q", want)
		}
	}
}

// --- semantic success path (against the scripted in-memory LSP server) -------------

func TestFindSymbolSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "find_symbol"), `{"name":"greet"}`)
	if out.IsError {
		t.Fatalf("find_symbol IsError: %q", out.Content)
	}
	// Kind 12 = Function; URI maps back to worktree-relative a.go; 0-based 2:5 -> 1-based 3:6.
	if !strings.Contains(out.Content, "Function greet — a.go:3:6") {
		t.Fatalf("find_symbol content = %q", out.Content)
	}
	if strings.Contains(out.Content, "unverified") {
		t.Fatalf("semantic result must not be labeled unverified: %q", out.Content)
	}
}

func TestReferencesSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "references"), `{"path":"a.go","line":3,"character":6}`)
	if out.IsError {
		t.Fatalf("references IsError: %q", out.Content)
	}
	// Two refs, paths relative, positions 1-based (0-based 2:5 -> 3:6, 9:1 -> 10:2).
	if !strings.Contains(out.Content, "a.go:3:6") || !strings.Contains(out.Content, "b.go:10:2") {
		t.Fatalf("references content = %q", out.Content)
	}
}

func TestDefinitionSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "definition"), `{"path":"a.go","line":3,"character":6}`)
	if out.IsError {
		t.Fatalf("definition IsError: %q", out.Content)
	}
	// Fake definition returns 0-based line 7 -> 1-based 8.
	if !strings.Contains(out.Content, "a.go:8:1") {
		t.Fatalf("definition content = %q", out.Content)
	}
}

func TestHoverSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "hover"), `{"path":"a.go","line":3,"character":6}`)
	if out.IsError {
		t.Fatalf("hover IsError: %q", out.Content)
	}
	if !strings.Contains(out.Content, "func greet(name string) string") {
		t.Fatalf("hover content = %q", out.Content)
	}
}

func TestDiagnosticsSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "diagnostics"), `{"path":"a.go"}`)
	if out.IsError {
		t.Fatalf("diagnostics IsError: %q", out.Content)
	}
	// The fake pushes one severity-2 (warning) "unused" diagnostic on didOpen.
	if !strings.Contains(out.Content, "a.go:1:1: warning: unused") {
		t.Fatalf("diagnostics content = %q", out.Content)
	}
}

func TestImplementationSemanticEmpty(t *testing.T) {
	// The fake returns null (default case) for implementation => "no results", not an error.
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "implementation"), `{"path":"a.go","line":3,"character":6}`)
	if out.IsError || !strings.Contains(out.Content, "no results") {
		t.Fatalf("implementation content = %q (IsError=%v)", out.Content, out.IsError)
	}
}

// --- argument validation -----------------------------------------------------------

func TestSemanticArgValidation(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	cases := []struct {
		tool, args, want string
	}{
		{"find_symbol", `{}`, "name is required"},
		{"references", `{"line":3,"character":6}`, "path is required"},
		{"references", `{"path":"a.go","character":6}`, "line is required"},
		{"references", `{"path":"a.go","line":3}`, "character is required"},
		{"definition", `{"path":"a.go","line":0,"character":1}`, "line is required"},
		{"diagnostics", `{}`, "path is required"},
	}
	for _, c := range cases {
		out := invoke(t, semanticToolByName(t, s, c.tool), c.args)
		if !out.IsError || !strings.Contains(out.Content, c.want) {
			t.Errorf("%s(%s) = %q (IsError=%v), want contains %q", c.tool, c.args, out.Content, out.IsError, c.want)
		}
	}
}

// --- silent degrade to the text floor (no language server) -------------------------

// grepSandbox is a no-SessionOpener sandbox (so every semantic query degrades) that serves
// a file for the identifier read and answers grep with a scripted match.
type grepSandbox struct {
	*scriptedSandbox
}

func newGrepSandbox(file, grepOut string) *grepSandbox {
	sb := &scriptedSandbox{}
	sb.respond = func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		switch cmd.Path {
		case "cat":
			return sandbox.ExecResult{Stdout: []byte(file)}, nil
		case "grep":
			if grepOut == "" {
				return sandbox.ExecResult{ExitCode: 1}, nil
			}
			return sandbox.ExecResult{Stdout: []byte(grepOut)}, nil
		}
		return sandbox.ExecResult{ExitCode: 127}, nil
	}
	return &grepSandbox{scriptedSandbox: sb}
}

func TestFindSymbolDegradesToGrep(t *testing.T) {
	sb := newGrepSandbox("", "a.go:3:func greet() {}\n")
	s := NewSessions(sb, nil) // scriptedSandbox is not a SessionOpener => no semantic layer
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "find_symbol"), `{"name":"greet"}`)
	if !strings.HasPrefix(out.Content, "[unverified") {
		t.Fatalf("degraded find_symbol must be labeled unverified, got %q", out.Content)
	}
	if !strings.Contains(out.Content, "func greet()") {
		t.Fatalf("degraded find_symbol content = %q", out.Content)
	}
	// The fallback grep must search for the whole symbol name with word boundaries.
	var sawGrep bool
	for _, c := range sb.commands() {
		if c.Path == "grep" && contains(c.Args, `\bgreet\b`) {
			sawGrep = true
		}
	}
	if !sawGrep {
		t.Fatalf("expected a bounded grep for the symbol, got %+v", sb.commands())
	}
}

func TestReferencesDegradesToGrepIdentifierAtPosition(t *testing.T) {
	// No semantic layer: references must read the file, extract the identifier at the
	// position, and grep for it — labeled unverified.
	file := "package main\n\nfunc greet() { greet() }\n"
	sb := newGrepSandbox(file, "a.go:3:func greet() { greet() }\n")
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	// Line 3, column 6 sits on "greet" (1-based: "func g" -> g at col 6).
	out := invoke(t, semanticToolByName(t, s, "references"), `{"path":"a.go","line":3,"character":6}`)
	if !strings.HasPrefix(out.Content, "[unverified") {
		t.Fatalf("degraded references must be labeled unverified, got %q", out.Content)
	}
	var sawGrep bool
	for _, c := range sb.commands() {
		if c.Path == "grep" && contains(c.Args, `\bgreet\b`) {
			sawGrep = true
		}
	}
	if !sawGrep {
		t.Fatalf("expected a bounded grep for the identifier at the position, got %+v", sb.commands())
	}
}

func TestDegradeNoIdentifierAtPosition(t *testing.T) {
	// A position that is not on an identifier can't be grepped — report a recoverable error
	// rather than guessing.
	sb := newGrepSandbox("package main\n\n   {}\n", "")
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "definition"), `{"path":"a.go","line":3,"character":1}`)
	if !out.IsError || !strings.Contains(out.Content, "no identifier found") {
		t.Fatalf("definition on non-identifier = %q (IsError=%v)", out.Content, out.IsError)
	}
}

func TestDiagnosticsDegradesGracefully(t *testing.T) {
	sb := newGrepSandbox("package main\n", "")
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, semanticToolByName(t, s, "diagnostics"), `{"path":"a.go"}`)
	if out.IsError {
		t.Fatalf("diagnostics degrade should not be a tool error: %q", out.Content)
	}
	if !strings.Contains(out.Content, "unverified") || !strings.Contains(out.Content, "run") {
		t.Fatalf("diagnostics degrade should point at `run`, got %q", out.Content)
	}
}

func TestGrepFallbackExecErrorIsFatal(t *testing.T) {
	// A sandbox that cannot run grep at all is a fatal invocation error (the runner
	// redelivers), distinct from "no matches".
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		if cmd.Path == "grep" {
			return sandbox.ExecResult{}, context.DeadlineExceeded
		}
		return sandbox.ExecResult{Stdout: []byte("package main\nvar greet int\n")}, nil
	}}
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	_, err := semanticToolByName(t, s, "find_symbol").Invoke(context.Background(), json.RawMessage(`{"name":"greet"}`))
	if err == nil {
		t.Fatal("expected a fatal error when grep cannot run, got nil")
	}
}

// --- unit-level helpers ------------------------------------------------------------

func TestIdentifierAt(t *testing.T) {
	content := "package main\n\nfunc greet(name string) {}\n"
	cases := []struct {
		line, col int
		want      string
	}{
		{3, 6, "greet"},   // 'g' of greet
		{3, 10, "greet"},  // 't' of greet
		{3, 11, "greet"},  // '(' just past greet -> caret on trailing edge
		{3, 12, "name"},   // 'n' of name
		{3, 1, "func"},    // 'f' of func
		{1, 1, "package"}, // first line
		{2, 1, ""},        // blank line
		{99, 1, ""},       // out of range
	}
	for _, c := range cases {
		if got := identifierAt(content, c.line, c.col); got != c.want {
			t.Errorf("identifierAt(%d,%d) = %q, want %q", c.line, c.col, got, c.want)
		}
	}
}

func TestRelForURI(t *testing.T) {
	cases := []struct {
		root, uri, want string
	}{
		{"/work", "file:///work/a.go", "a.go"},
		{"/work", "file:///work/internal/x/y.go", "internal/x/y.go"},
		{"/work/", "file:///work/a.go", "a.go"},
		{"/work", "file:///other/a.go", "/other/a.go"}, // outside root: bare path, never lost
		{"", "file:///work/a.go", "/work/a.go"},
	}
	for _, c := range cases {
		if got := relForURI(c.root, c.uri); got != c.want {
			t.Errorf("relForURI(%q,%q) = %q, want %q", c.root, c.uri, got, c.want)
		}
	}
}

func TestSymbolKindName(t *testing.T) {
	if symbolKindName(12) != "Function" || symbolKindName(23) != "Struct" || symbolKindName(11) != "Interface" {
		t.Fatal("known SymbolKind labels wrong")
	}
	if symbolKindName(0) != "Symbol" || symbolKindName(99) != "Symbol" {
		t.Fatal("unknown SymbolKind must degrade to Symbol")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
