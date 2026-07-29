package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/lsp"
	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// writeToolByName builds the semantic write tools over the given Sessions + ledger and
// returns the one named.
func writeToolByName(t *testing.T, s *Sessions, ledger *TransformLedger, name string) Tool {
	t.Helper()
	for _, tl := range SemanticWriteTools(s, ledger) {
		if tl.Def().Name == name {
			return tl
		}
	}
	t.Fatalf("no semantic write tool named %q", name)
	return nil
}

func TestSemanticWriteToolSet(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	got := map[string]bool{}
	for _, tl := range SemanticWriteTools(s, nil) {
		got[tl.Def().Name] = true
		if !json.Valid(tl.Def().Params) {
			t.Errorf("tool %s has invalid JSON schema params: %s", tl.Def().Name, tl.Def().Params)
		}
	}
	for _, want := range []string{"rename", "code_action"} {
		if !got[want] {
			t.Errorf("missing semantic write tool %q", want)
		}
	}
}

// --- rename: semantic path ---------------------------------------------------------

func TestRenameSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)
	ledger := NewTransformLedger()

	out := invoke(t, writeToolByName(t, s, ledger, "rename"), `{"path":"a.go","line":3,"character":6,"new_name":"hello"}`)
	if out.IsError {
		t.Fatalf("rename IsError: %q", out.Content)
	}
	if strings.Contains(out.Content, "unverified") {
		t.Fatalf("semantic rename must not be labeled unverified: %q", out.Content)
	}
	if !strings.Contains(out.Content, "via the language server") {
		t.Fatalf("rename content = %q", out.Content)
	}
	// The edit (0-based 2:5..2:10 = "greet") must have been applied to the worktree file.
	if got := sb.file("a.go"); got != "package main\n\nfunc hello() {}\n" {
		t.Fatalf("a.go after rename = %q", got)
	}
	// The applied edit must have been re-synced into the running session (didChange).
	sb.rec.waitFor(t, func(evs []docEvent) bool {
		for _, e := range evs {
			if e.Method == "didChange" && e.URI == "file:///work/a.go" && strings.Contains(e.Text, "func hello()") {
				return true
			}
		}
		return false
	})
	// The mechanism must be recorded as semantic.
	recs := ledger.take()
	if len(recs) != 1 || recs[0].Mechanism != core.TransformMechanismSemantic || recs[0].Tool != "rename" || recs[0].Files != 1 {
		t.Fatalf("transform record = %+v", recs)
	}
}

func TestRenameArgValidation(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	cases := []struct{ args, want string }{
		{`{"line":3,"character":6,"new_name":"x"}`, "path is required"},
		{`{"path":"a.go","character":6,"new_name":"x"}`, "line is required"},
		{`{"path":"a.go","line":3,"new_name":"x"}`, "character is required"},
		{`{"path":"a.go","line":3,"character":6}`, "new_name is required"},
		{`{"path":"a.go","line":3,"character":6,"new_name":"2bad"}`, "not a valid identifier"},
		{`{"path":"a.go","line":3,"character":6,"new_name":"a-b"}`, "not a valid identifier"},
	}
	for _, c := range cases {
		out := invoke(t, writeToolByName(t, s, nil, "rename"), c.args)
		if !out.IsError || !strings.Contains(out.Content, c.want) {
			t.Errorf("rename(%s) = %q (IsError=%v), want contains %q", c.args, out.Content, out.IsError, c.want)
		}
	}
}

// --- rename: loud text-floor degrade ----------------------------------------------

// renameFallbackSandbox is a no-SessionOpener sandbox (so rename degrades) that serves a
// file with the identifier in code, a string literal, and a comment, lists it via grep,
// and records writes.
func renameFallbackSandbox(content string) *scriptedSandbox {
	files := map[string]string{"a.go": content}
	return &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		switch cmd.Path {
		case "cat":
			return sandbox.ExecResult{Stdout: []byte(files[cmd.Args[len(cmd.Args)-1]])}, nil
		case "grep":
			return sandbox.ExecResult{Stdout: []byte("./a.go\n")}, nil
		case "sh": // writeFile
			files[cmd.Args[3]] = string(cmd.Stdin)
			return sandbox.ExecResult{}, nil
		}
		return sandbox.ExecResult{ExitCode: 127}, nil
	}}
}

func TestRenameDegradesToTextRename(t *testing.T) {
	content := "package main\n\nfunc greet() string { return \"greet\" } // greet helper\n"
	sb := renameFallbackSandbox(content)
	s := NewSessions(sb, nil) // not a SessionOpener => no semantic layer
	t.Cleanup(s.Close)
	ledger := NewTransformLedger()

	out := invoke(t, writeToolByName(t, s, ledger, "rename"), `{"path":"a.go","line":3,"character":6,"new_name":"hello"}`)
	if out.IsError {
		t.Fatalf("text rename IsError: %q", out.Content)
	}
	if !strings.Contains(out.Content, "[unverified") || !strings.Contains(out.Content, "TEXT rename") {
		t.Fatalf("text rename must warn loudly, got %q", out.Content)
	}
	// All three word-boundary occurrences (code, string, comment) are rewritten.
	var wrote string
	for _, c := range sb.commands() {
		if c.Path == "sh" {
			wrote = string(c.Stdin)
		}
	}
	if strings.Contains(wrote, "greet") || !strings.Contains(wrote, "func hello()") {
		t.Fatalf("text rename worktree result = %q", wrote)
	}
	// The mechanism is recorded as TEXT, with the precision note (3 matches, 2 risky).
	recs := ledger.take()
	if len(recs) != 1 || recs[0].Mechanism != core.TransformMechanismText {
		t.Fatalf("transform record = %+v", recs)
	}
	if recs[0].Edits != 3 || !strings.Contains(recs[0].Note, "2 inside comments or string literals") {
		t.Fatalf("text rename note = %q (edits=%d)", recs[0].Note, recs[0].Edits)
	}
}

func TestTextRenameNoIdentifierAtPosition(t *testing.T) {
	sb := renameFallbackSandbox("package main\n\n   {}\n")
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, writeToolByName(t, s, nil, "rename"), `{"path":"a.go","line":3,"character":1,"new_name":"hello"}`)
	if !out.IsError || !strings.Contains(out.Content, "no identifier found") {
		t.Fatalf("rename on non-identifier = %q (IsError=%v)", out.Content, out.IsError)
	}
}

func TestTextRenameGrepFatal(t *testing.T) {
	// A sandbox where grep cannot run at all is a fatal invocation error (the runner
	// redelivers), distinct from "no matches".
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		if cmd.Path == "grep" {
			return sandbox.ExecResult{}, context.DeadlineExceeded
		}
		return sandbox.ExecResult{Stdout: []byte("package main\nvar greet int\n")}, nil
	}}
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	_, err := writeToolByName(t, s, nil, "rename").Invoke(context.Background(), json.RawMessage(`{"path":"a.go","line":2,"character":5,"new_name":"hi"}`))
	if err == nil {
		t.Fatal("expected a fatal error when grep cannot run, got nil")
	}
}

// --- code_action -------------------------------------------------------------------

func TestCodeActionSemantic(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n\nfunc greet() {}\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)
	ledger := NewTransformLedger()

	out := invoke(t, writeToolByName(t, s, ledger, "code_action"), `{"path":"a.go","kind":"source.organizeImports"}`)
	if out.IsError {
		t.Fatalf("code_action IsError: %q", out.Content)
	}
	if !strings.Contains(out.Content, `applied "Organize Imports"`) {
		t.Fatalf("code_action content = %q", out.Content)
	}
	// The action's inline edit prepended "// organized\n" to the file.
	if got := sb.file("a.go"); !strings.HasPrefix(got, "// organized\n") {
		t.Fatalf("a.go after code_action = %q", got)
	}
	recs := ledger.take()
	if len(recs) != 1 || recs[0].Tool != "code_action" || recs[0].Mechanism != core.TransformMechanismSemantic {
		t.Fatalf("transform record = %+v", recs)
	}
}

func TestCodeActionListsWhenNoSelector(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, writeToolByName(t, s, nil, "code_action"), `{"path":"a.go"}`)
	if out.IsError {
		t.Fatalf("listing actions should not be an error: %q", out.Content)
	}
	if !strings.Contains(out.Content, "Organize Imports") || !strings.Contains(out.Content, "Run go vet") {
		t.Fatalf("code_action listing = %q", out.Content)
	}
	// Listing must not apply anything.
	if sb.file("a.go") != "package main\n" {
		t.Fatalf("listing must not modify the file: %q", sb.file("a.go"))
	}
}

func TestCodeActionCommandOnlyRefused(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)

	out := invoke(t, writeToolByName(t, s, nil, "code_action"), `{"path":"a.go","title":"Run go vet"}`)
	if !out.IsError || !strings.Contains(out.Content, "no inline edit") {
		t.Fatalf("command-only action = %q (IsError=%v)", out.Content, out.IsError)
	}
}

func TestCodeActionNoServerRefusesLoudly(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	}}
	s := NewSessions(sb, nil) // no SessionOpener
	t.Cleanup(s.Close)

	out := invoke(t, writeToolByName(t, s, nil, "code_action"), `{"path":"a.go","line":1,"character":1}`)
	if !out.IsError || !strings.Contains(out.Content, "no language server available") {
		t.Fatalf("code_action no-server = %q (IsError=%v) — writes must degrade loudly", out.Content, out.IsError)
	}
}

func TestCodeActionPathRequired(t *testing.T) {
	sb := newLSPSandbox(map[string]string{"a.go": "package main\n"})
	s := NewSessions(sb, nil)
	t.Cleanup(s.Close)
	out := invoke(t, writeToolByName(t, s, nil, "code_action"), `{}`)
	if !out.IsError || !strings.Contains(out.Content, "path is required") {
		t.Fatalf("code_action() = %q (IsError=%v)", out.Content, out.IsError)
	}
}

// --- lifecycle folds the ledger into the Result -----------------------------------

func TestSubmitFoldsTransforms(t *testing.T) {
	ledger := NewTransformLedger()
	ledger.Record(core.TransformRecord{Tool: "rename", Mechanism: core.TransformMechanismText, Files: 1, Edits: 2})

	brk := &recordingBroker{pushCommit: "c1"}
	tools := LifecycleTools(lifecycleBrief(), brk, ledger)
	out := invoke(t, lcToolByName(t, tools, "submit"), `{"summary":"done"}`)
	if out.Result == nil {
		t.Fatal("submit produced no Result")
	}
	if len(out.Result.Transforms) != 1 || out.Result.Transforms[0].Mechanism != core.TransformMechanismText {
		t.Fatalf("submit Result.Transforms = %+v", out.Result.Transforms)
	}
}

// --- unit helpers ------------------------------------------------------------------

func TestApplyTextEdits(t *testing.T) {
	// Two non-overlapping edits applied independently of order.
	content := "alpha beta gamma"
	edits := []lsp.TextEdit{
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 5}}, NewText: "ONE"},
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 11}, End: lsp.Position{Line: 0, Character: 16}}, NewText: "THREE"},
	}
	if got := applyTextEdits(content, edits); got != "ONE beta THREE" {
		t.Fatalf("applyTextEdits = %q", got)
	}
	// A multi-line edit.
	ml := "package main\n\nfunc greet() {}\n"
	one := []lsp.TextEdit{{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 10}}, NewText: "hello"}}
	if got := applyTextEdits(ml, one); got != "package main\n\nfunc hello() {}\n" {
		t.Fatalf("applyTextEdits multiline = %q", got)
	}
}

func TestPositionToOffset(t *testing.T) {
	content := "ab\ncd\nef"
	cases := []struct {
		pos  lsp.Position
		want int
	}{
		{lsp.Position{Line: 0, Character: 0}, 0},
		{lsp.Position{Line: 1, Character: 1}, 4},  // 'd'
		{lsp.Position{Line: 2, Character: 2}, 8},  // end of file
		{lsp.Position{Line: 0, Character: 99}, 2}, // column past line end clamps to line end
		{lsp.Position{Line: 9, Character: 0}, 8},  // line past EOF clamps to len
	}
	for _, c := range cases {
		if got := positionToOffset(content, c.pos); got != c.want {
			t.Errorf("positionToOffset(%+v) = %d, want %d", c.pos, got, c.want)
		}
	}
}

func TestRiskyMatch(t *testing.T) {
	cases := []struct {
		line string
		col  int
		want bool
	}{
		{"func greet() {}", 5, false},    // code
		{`x := "greet"`, 7, true},        // inside a string literal
		{"// greet helper", 3, true},     // after a line comment
		{`f("greet") // x`, 4, true},     // inside the string before the comment
		{`f("done") // greet`, 14, true}, // after the comment, string already closed
		{`a + b // ok`, 0, false},        // before any comment/string
	}
	for _, c := range cases {
		if got := riskyMatch(c.line, c.col); got != c.want {
			t.Errorf("riskyMatch(%q,%d) = %v, want %v", c.line, c.col, got, c.want)
		}
	}
}

func TestIsIdentifier(t *testing.T) {
	for _, s := range []string{"hello", "Hello2", "_x", "x_y_z"} {
		if !isIdentifier(s) {
			t.Errorf("isIdentifier(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "2x", "a-b", "a b", "a.b", "x!"} {
		if isIdentifier(s) {
			t.Errorf("isIdentifier(%q) = true, want false", s)
		}
	}
}

func TestSelectAction(t *testing.T) {
	actions := []lsp.CodeAction{
		{Title: "Organize Imports", Kind: "source.organizeImports"},
		{Title: "Add import \"fmt\"", Kind: "quickfix"},
		{Title: "Add import \"os\"", Kind: "quickfix"},
	}
	// Exact title.
	if a, _ := selectAction(actions, "Organize Imports", ""); a == nil || a.Title != "Organize Imports" {
		t.Fatal("exact title did not select")
	}
	// Kind prefix matching exactly one.
	if a, _ := selectAction(actions, "", "source.organizeImports"); a == nil || a.Kind != "source.organizeImports" {
		t.Fatal("kind prefix did not select")
	}
	// Ambiguous kind (two quickfix) => nil.
	if a, _ := selectAction(actions, "", "quickfix"); a != nil {
		t.Fatal("ambiguous kind must not select")
	}
	// Ambiguous substring title => nil.
	if a, _ := selectAction(actions, "Add import", ""); a != nil {
		t.Fatal("ambiguous substring must not select")
	}
	// No selector, multiple actions => nil (list).
	if a, _ := selectAction(actions, "", ""); a != nil {
		t.Fatal("no selector with multiple actions must list, not apply")
	}
	// No selector, single action => that action.
	if a, _ := selectAction(actions[:1], "", ""); a == nil {
		t.Fatal("no selector with a single action must select it")
	}
}

func TestUTF16Len(t *testing.T) {
	if utf16Len("abc") != 3 {
		t.Fatal("ascii length wrong")
	}
	if utf16Len("a\U0001F600b") != 4 { // emoji is a surrogate pair (2 units)
		t.Fatalf("surrogate length = %d, want 4", utf16Len("a\U0001F600b"))
	}
}
