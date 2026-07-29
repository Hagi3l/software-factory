package agent

import (
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// gosecJSON is a canned `gosec -fmt=json` report with one finding — the scanner output whose
// raw form is worst, so the self-check must return the compact finding, not the dump.
const gosecJSON = `{"Issues":[{"severity":"HIGH","confidence":"HIGH","rule_id":"G401","details":"Use of weak cryptographic primitive","file":"crypto.go","line":"7","code":"h := md5.New()"}],"Stats":{"files":1}}`

// TestRunGateRunsEveryCheckAndReturnsFindings drives run_gate over a two-check suite — a
// passing acceptance-test run and a failing gosec scan — and asserts it runs both commands,
// returns the compact findings (not the raw dumps), tallies pass/fail, and flags the failure.
func TestRunGateRunsEveryCheckAndReturnsFindings(t *testing.T) {
	const testsCmd = "go test -json ./..."
	const gosecCmd = "gosec -fmt=json ./..."
	sb := &scriptedSandbox{respond: func(cmd sandbox.Command) (sandbox.ExecResult, error) {
		line := cmd.Args[len(cmd.Args)-1]
		switch line {
		case testsCmd:
			return sandbox.ExecResult{ExitCode: 0, Stdout: []byte(passJSON)}, nil
		case gosecCmd:
			return sandbox.ExecResult{ExitCode: 1, Stdout: []byte(gosecJSON)}, nil
		default:
			t.Fatalf("unexpected command: %q", line)
			return sandbox.ExecResult{}, nil
		}
	}}
	ledger := NewTestEvidenceLedger()
	checks := map[string]string{"tests-pass": testsCmd, "gosec": gosecCmd}

	out := invoke(t, RunGateTool(sb, checks, ledger), `{}`)

	// Both checks ran, each via `sh -c <cmd>`.
	if cmds := sb.commands(); len(cmds) != 2 {
		t.Fatalf("expected 2 execs, got %d", len(cmds))
	}
	// The gosec finding is present in compact form; the raw dump fields are not.
	if !strings.Contains(out.Content, "crypto.go:7") || !strings.Contains(out.Content, "G401") {
		t.Fatalf("gosec finding missing from output:\n%s", out.Content)
	}
	if strings.Contains(out.Content, "Stats") || strings.Contains(out.Content, `"confidence"`) {
		t.Fatalf("raw gosec json leaked into output:\n%s", out.Content)
	}
	// The tally names the failure, and IsError flags it as actionable feedback.
	if !strings.Contains(out.Content, "1 passed, 1 failed") {
		t.Fatalf("tally wrong:\n%s", out.Content)
	}
	if !out.IsError {
		t.Fatal("expected IsError when a check failed")
	}
	// Raw output of both checks is harvested as evidence (cited by content address).
	if !strings.Contains(out.Content, "tests-pass=sha256:") || !strings.Contains(out.Content, "gosec=sha256:") {
		t.Fatalf("evidence hashes not cited:\n%s", out.Content)
	}
}

// TestRunGateAllCleanIsNotError proves a fully-passing suite is not flagged an error and reads
// as clean.
func TestRunGateAllCleanIsNotError(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte(passJSON)}, nil
	}}
	out := invoke(t, RunGateTool(sb, map[string]string{"tests-pass": "go test -json ./..."}, NewTestEvidenceLedger()), `{}`)
	if out.IsError {
		t.Fatalf("clean suite flagged as error:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "1 passed, 0 failed") {
		t.Fatalf("tally wrong:\n%s", out.Content)
	}
}

// TestRunGateNoChecksConfigured proves the tool degrades cleanly when no command checks exist.
func TestRunGateNoChecksConfigured(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		t.Fatal("no command should run when no checks are configured")
		return sandbox.ExecResult{}, nil
	}}
	out := invoke(t, RunGateTool(sb, nil, NewTestEvidenceLedger()), `{}`)
	if out.IsError || !strings.Contains(out.Content, "no command checks") {
		t.Fatalf("unexpected output for empty suite:\n%s", out.Content)
	}
}
