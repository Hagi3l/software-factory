package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Loxstomper/harness/internal/sandbox"
)

// scriptedSandbox routes each Exec to a responder keyed off the command, recording every
// command so tests can assert the exact argv the tools built.
type scriptedSandbox struct {
	mu      sync.Mutex
	calls   []sandbox.Command
	respond func(cmd sandbox.Command) (sandbox.ExecResult, error)
}

func (s *scriptedSandbox) ID() string { return "sb" }
func (s *scriptedSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, cmd)
	s.mu.Unlock()
	return s.respond(cmd)
}
func (s *scriptedSandbox) Teardown(context.Context) error { return nil }

func (s *scriptedSandbox) commands() []sandbox.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sandbox.Command(nil), s.calls...)
}

// toolByName finds a workspace tool for assertions.
func toolByName(t *testing.T, sb sandbox.Sandbox, name string) Tool {
	t.Helper()
	for _, tl := range WorkspaceTools(sb, nil) {
		if tl.Def().Name == name {
			return tl
		}
	}
	t.Fatalf("no workspace tool named %q", name)
	return nil
}

func invoke(t *testing.T, tool Tool, args string) Outcome {
	t.Helper()
	out, err := tool.Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("tool %s: unexpected fatal error: %v", tool.Def().Name, err)
	}
	return out
}

func TestWorkspaceToolSet(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	}}
	got := map[string]bool{}
	for _, tl := range WorkspaceTools(sb, nil) {
		got[tl.Def().Name] = true
		if !json.Valid(tl.Def().Params) {
			t.Errorf("tool %s has invalid JSON schema params", tl.Def().Name)
		}
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search", "run"} {
		if !got[want] {
			t.Errorf("missing workspace tool %q", want)
		}
	}
}

func TestReadFile(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte("package main\n")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "read_file"), `{"path":"main.go"}`)
	if out.IsError || out.Content != "package main\n" {
		t.Errorf("read_file = %+v, want package main", out)
	}
	cmd := sb.commands()[0]
	if cmd.Path != "cat" || strings.Join(cmd.Args, " ") != "-- main.go" {
		t.Errorf("read_file command = %+v, want cat -- main.go", cmd)
	}
}

func TestReadFileMissingIsError(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("cat: nope: No such file")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "read_file"), `{"path":"nope"}`)
	if !out.IsError || !strings.Contains(out.Content, "No such file") {
		t.Errorf("missing read_file = %+v, want IsError with stderr", out)
	}
}

func TestWriteFile(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	}}
	out := invoke(t, toolByName(t, sb, "write_file"), `{"path":"pkg/a.go","content":"hello"}`)
	if out.IsError || !strings.Contains(out.Content, "wrote pkg/a.go") {
		t.Errorf("write_file = %+v", out)
	}
	cmd := sb.commands()[0]
	if cmd.Path != "sh" || string(cmd.Stdin) != "hello" {
		t.Errorf("write_file command = %+v, want sh with stdin=hello", cmd)
	}
	// Path is a positional arg ($1), never interpolated into the script.
	if cmd.Args[len(cmd.Args)-1] != "pkg/a.go" {
		t.Errorf("write_file path arg = %q, want pkg/a.go", cmd.Args[len(cmd.Args)-1])
	}
}

func TestEditFileUnique(t *testing.T) {
	const before = "a = 1\nb = 2\n"
	var wrote string
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		if cmd.Path == "cat" {
			return sandbox.ExecResult{Stdout: []byte(before)}, nil
		}
		wrote = string(cmd.Stdin) // the write
		return sandbox.ExecResult{}, nil
	}}
	out := invoke(t, toolByName(t, sb, "edit_file"), `{"path":"x","old_string":"b = 2","new_string":"b = 3"}`)
	if out.IsError {
		t.Fatalf("edit_file = %+v, want success", out)
	}
	if wrote != "a = 1\nb = 3\n" {
		t.Errorf("edit_file wrote %q, want b=3 substituted", wrote)
	}
}

func TestEditFileNotFound(t *testing.T) {
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte("a = 1\n")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "edit_file"), `{"path":"x","old_string":"zzz","new_string":"q"}`)
	if !out.IsError || !strings.Contains(out.Content, "not found") {
		t.Errorf("edit_file missing old = %+v, want IsError not found", out)
	}
}

func TestEditFileNotUnique(t *testing.T) {
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte("x\nx\n")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "edit_file"), `{"path":"f","old_string":"x","new_string":"y"}`)
	if !out.IsError || !strings.Contains(out.Content, "not unique") {
		t.Errorf("edit_file non-unique = %+v, want IsError not unique", out)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	var wrote string
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		if cmd.Path == "cat" {
			return sandbox.ExecResult{Stdout: []byte("x x x")}, nil
		}
		wrote = string(cmd.Stdin)
		return sandbox.ExecResult{}, nil
	}}
	out := invoke(t, toolByName(t, sb, "edit_file"), `{"path":"f","old_string":"x","new_string":"y","replace_all":true}`)
	if out.IsError || wrote != "y y y" {
		t.Errorf("edit_file replace_all wrote %q (out %+v), want y y y", wrote, out)
	}
}

func TestSearchNoMatches(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 1}, nil // grep: no lines selected
	}}
	out := invoke(t, toolByName(t, sb, "search"), `{"pattern":"frob"}`)
	if out.IsError || out.Content != "no matches" {
		t.Errorf("search no-match = %+v, want non-error 'no matches'", out)
	}
}

func TestSearchHits(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte("a.go:3:frobnicate()")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "search"), `{"pattern":"frob","path":"internal"}`)
	if out.IsError || !strings.Contains(out.Content, "frobnicate") {
		t.Errorf("search hits = %+v", out)
	}
	cmd := sb.commands()[0]
	if cmd.Path != "grep" || cmd.Args[len(cmd.Args)-1] != "internal" {
		t.Errorf("search command = %+v, want grep under internal", cmd)
	}
}

func TestRunSurfacesExitCode(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 2, Stdout: []byte("FAIL"), Stderr: []byte("boom")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "run"), `{"command":"go test ./..."}`)
	if !out.IsError || !strings.Contains(out.Content, "exit code 2") || !strings.Contains(out.Content, "boom") {
		t.Errorf("run failed-cmd = %+v, want exit code 2 + stderr", out)
	}
	cmd := sb.commands()[0]
	if cmd.Path != "sh" || cmd.Args[0] != "-c" || cmd.Args[1] != "go test ./..." {
		t.Errorf("run command = %+v, want sh -c 'go test ./...'", cmd)
	}
}

func TestRunSuccess(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte("ok")}, nil
	}}
	out := invoke(t, toolByName(t, sb, "run"), `{"command":"go build ./..."}`)
	if out.IsError || out.Content != "ok" {
		t.Errorf("run success = %+v, want ok", out)
	}
}

func TestInvalidArgs(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, nil
	}}
	out := invoke(t, toolByName(t, sb, "read_file"), `{not json`)
	if !out.IsError || !strings.Contains(out.Content, "could not parse") {
		t.Errorf("bad args = %+v, want IsError parse failure", out)
	}
	// Missing required field is also a model-correctable error, not fatal.
	out = invoke(t, toolByName(t, sb, "read_file"), `{}`)
	if !out.IsError || !strings.Contains(out.Content, "required") {
		t.Errorf("missing path = %+v, want IsError required", out)
	}
}

func TestExecErrorIsFatal(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New("sandbox dead")
	}}
	_, err := toolByName(t, sb, "run").Invoke(context.Background(), json.RawMessage(`{"command":"ls"}`))
	if err == nil {
		t.Fatal("run with dead sandbox: want fatal error, got nil")
	}
}

func TestOutputTruncated(t *testing.T) {
	big := strings.Repeat("a", maxToolOutput+100)
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte(big)}, nil
	}}
	out := invoke(t, toolByName(t, sb, "read_file"), `{"path":"big"}`)
	if !strings.HasSuffix(out.Content, "[output truncated]") {
		t.Errorf("large output was not truncated")
	}
}
