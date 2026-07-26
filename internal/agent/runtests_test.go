package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/sandbox"
)

// failJSON is a canned `go test -json` stream for one failing test, with the jitter
// (timestamps, elapsed) gotest.Parse is meant to strip. The self-check must turn this into
// a compact finding, never feed the raw stream back to the model.
const failJSON = `{"Time":"2026-06-22T11:14:33.69Z","Action":"run","Package":"scratch","Test":"TestAdd"}
{"Time":"2026-06-22T11:14:33.69Z","Action":"output","Package":"scratch","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Time":"2026-06-22T11:14:33.69Z","Action":"output","Package":"scratch","Test":"TestAdd","Output":"    fail_test.go:9: Add() = 4, want 5\n"}
{"Time":"2026-06-22T11:14:33.69Z","Action":"output","Package":"scratch","Test":"TestAdd","Output":"--- FAIL: TestAdd (0.00s)\n"}
{"Time":"2026-06-22T11:14:33.69Z","Action":"fail","Package":"scratch","Test":"TestAdd","Elapsed":0}
{"Time":"2026-06-22T11:14:33.69Z","Action":"fail","Package":"scratch","Elapsed":0}
`

const passJSON = `{"Time":"2026-06-22T11:14:33.69Z","Action":"run","Package":"scratch","Test":"TestAdd"}
{"Time":"2026-06-22T11:14:33.69Z","Action":"pass","Package":"scratch","Test":"TestAdd","Elapsed":0}
{"Time":"2026-06-22T11:14:33.69Z","Action":"pass","Package":"scratch","Elapsed":0}
`

func TestRunTestsReturnsFindingsNotRawDump(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		// A failing run exits non-zero; the tool must not treat that exit as a fatal error.
		return sandbox.ExecResult{ExitCode: 1, Stdout: []byte(failJSON)}, nil
	}}
	ledger := NewTestEvidenceLedger()
	out := invoke(t, RunTestsTool(sb, ledger), `{"scope":"./internal/foo/..."}`)

	// The argv must be `go test -json <scope>` with the scope passed positionally.
	cmds := sb.commands()
	if len(cmds) != 1 {
		t.Fatalf("expected 1 exec, got %d", len(cmds))
	}
	if got := append([]string{cmds[0].Path}, cmds[0].Args...); strings.Join(got, " ") != "go test -json ./internal/foo/..." {
		t.Fatalf("argv = %q", got)
	}

	// The compact finding must be present; the raw dump (RUN lines, elapsed) must not.
	if !strings.Contains(out.Content, "fail_test.go:9") || !strings.Contains(out.Content, "Add() = 4, want 5") {
		t.Fatalf("findings missing from output:\n%s", out.Content)
	}
	if strings.Contains(out.Content, "=== RUN") || strings.Contains(out.Content, "--- FAIL") {
		t.Fatalf("raw test dump leaked into output:\n%s", out.Content)
	}
	// Findings present => the model should see this as a failure (feedback to fix).
	if !out.IsError {
		t.Fatalf("expected IsError on a failing run")
	}
}

func TestRunTestsHarvestsRawEvidenceByContentAddress(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 1, Stdout: []byte(failJSON)}, nil
	}}
	ledger := NewTestEvidenceLedger()
	out := invoke(t, RunTestsTool(sb, ledger), `{}`)

	// The hash cited inline must be the store's content address of the raw stream...
	sum := sha256.Sum256([]byte(failJSON))
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.Contains(out.Content, wantHash) {
		t.Fatalf("evidence hash %s not cited in output:\n%s", wantHash, out.Content)
	}
	// ...and the ledger must hold the exact raw bytes under that same hash for the runner
	// to harvest (the raw json is evidence, kept by hash, not returned inline).
	ev := ledger.Evidence()
	raw, ok := ev[wantHash]
	if !ok {
		t.Fatalf("raw evidence not harvested under %s; have %v", wantHash, keys(ev))
	}
	if string(raw) != failJSON {
		t.Fatalf("harvested evidence does not match raw stream")
	}
}

func TestRunTestsDefaultsScopeToWholeModule(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte(passJSON)}, nil
	}}
	out := invoke(t, RunTestsTool(sb, nil), `{}`)

	cmds := sb.commands()
	if got := cmds[0].Args; strings.Join(got, " ") != "test -json ./..." {
		t.Fatalf("default scope argv = %q, want ./...", got)
	}
	// A clean pass: no findings, not an error, and an explicit "passed" line — never silence.
	if out.IsError {
		t.Fatalf("clean pass must not be IsError")
	}
	if !strings.Contains(out.Content, "tests passed: 0 findings") {
		t.Fatalf("missing clear pass line:\n%s", out.Content)
	}
}

func TestRunTestsNilLedgerStillCitesHash(t *testing.T) {
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: []byte(passJSON)}, nil
	}}
	// A nil ledger (no harvest sink) must not panic and must still cite the hash.
	out := invoke(t, RunTestsTool(sb, nil), `{}`)
	sum := sha256.Sum256([]byte(passJSON))
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.Contains(out.Content, wantHash) {
		t.Fatalf("nil-ledger run did not cite hash %s:\n%s", wantHash, out.Content)
	}
}

func TestRunTestsBuildFailureBecomesFinding(t *testing.T) {
	// A raw, non-JSON build failure printed straight to stdout must surface as a finding,
	// never crash the parser and never read as a misleading green.
	const buildFail = "# scratch\n./broken.go:3:1: syntax error: unexpected }\n"
	sb := &scriptedSandbox{respond: func(sandbox.Command) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: 1, Stdout: []byte(buildFail)}, nil
	}}
	out := invoke(t, RunTestsTool(sb, nil), `{}`)
	if !out.IsError {
		t.Fatalf("a build failure must be reported as an error, not a pass:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "syntax error") {
		t.Fatalf("build error signal missing:\n%s", out.Content)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
