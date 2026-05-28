package gate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// --- fakes -------------------------------------------------------------------

// scriptedSandbox returns a canned ExecResult per `sh -c` command string, records the
// commands it ran and how many times it was torn down. If execErr is set every Exec
// fails to run (modeling a dead sandbox).
type scriptedSandbox struct {
	id        string
	results   map[string]sandbox.ExecResult
	execErr   error
	execed    []string
	teardowns int
}

func (s *scriptedSandbox) ID() string { return s.id }

func (s *scriptedSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if s.execErr != nil {
		return sandbox.ExecResult{}, s.execErr
	}
	key := ""
	if cmd.Path == "sh" && len(cmd.Args) == 2 {
		key = cmd.Args[1]
	}
	s.execed = append(s.execed, key)
	return s.results[key], nil
}

func (s *scriptedSandbox) Teardown(context.Context) error { s.teardowns++; return nil }

type fakeBackend struct {
	sb          *scriptedSandbox
	provErr     error
	gotSpec     sandbox.Spec
	provisioned int
}

func (b *fakeBackend) Provision(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	b.provisioned++
	b.gotSpec = spec
	if b.provErr != nil {
		return nil, b.provErr
	}
	return b.sb, nil
}

func testCandidate() Candidate {
	return Candidate{
		Repo:    "/repo",
		Ref:     core.CandidateBranch("issue-1"),
		Profile: "go-toolchain",
		Limits:  config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(30 * time.Minute)},
	}
}

// --- tests -------------------------------------------------------------------

// All checks pass: the report is green, the verification sandbox is seeded with the
// candidate branch ref (a clean checkout distinct from the producer's), and it is
// always torn down.
func TestRunAllChecksPass(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0, Stdout: []byte("built")},
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok")},
	}}
	be := &fakeBackend{sb: sb}
	g := New(be, []Check{{Name: "build", Cmd: "make build"}, {Name: "test", Cmd: "make test-unit"}}, t.TempDir(), nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("report.Passed = false, want true")
	}
	if len(report.Checks) != 2 {
		t.Fatalf("ran %d checks, want 2", len(report.Checks))
	}
	if !report.Checks[0].Passed || string(report.Checks[0].Stdout) != "built" {
		t.Errorf("build check = %+v, want passed with captured stdout", report.Checks[0])
	}
	// The verification sandbox must be seeded from the candidate branch, not the base.
	if be.gotSpec.Workspace.BaseRef != core.CandidateBranch("issue-1") {
		t.Errorf("seeded ref = %q, want the candidate branch", be.gotSpec.Workspace.BaseRef)
	}
	if be.gotSpec.Profile != "go-toolchain" {
		t.Errorf("profile = %q, want go-toolchain", be.gotSpec.Profile)
	}
	if be.gotSpec.Broker.Network != "unix" || be.gotSpec.Broker.Address == "" {
		t.Errorf("broker endpoint = %+v, want a unix socket", be.gotSpec.Broker)
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1 (always reaped)", sb.teardowns)
	}
}

// A failing check fails the gate and stops the run (fail-fast): a failed build means
// the test never runs.
func TestRunStopsAtFirstFailure(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 2, Stderr: []byte("compile error")},
		"make test-unit": {ExitCode: 0},
	}}
	be := &fakeBackend{sb: sb}
	g := New(be, []Check{{Name: "build", Cmd: "make build"}, {Name: "test", Cmd: "make test-unit"}}, t.TempDir(), nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Errorf("report.Passed = true, want false")
	}
	if len(report.Checks) != 1 {
		t.Fatalf("ran %d checks, want 1 (fail-fast after build)", len(report.Checks))
	}
	if report.Checks[0].ExitCode != 2 || string(report.Checks[0].Stderr) != "compile error" {
		t.Errorf("build result = %+v, want exit 2 with captured stderr", report.Checks[0])
	}
	if len(sb.execed) != 1 || sb.execed[0] != "make build" {
		t.Errorf("commands run = %v, want only [make build]", sb.execed)
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1", sb.teardowns)
	}
}

// A sandbox that cannot run a check at all is a gate infrastructure failure (error),
// not a gate verdict — the orchestrator retries it rather than routing on_failure.
func TestRunExecErrorIsInfraFailure(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", execErr: errors.New("sandbox is gone")}
	be := &fakeBackend{sb: sb}
	g := New(be, []Check{{Name: "build", Cmd: "make build"}}, t.TempDir(), nil)

	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run returned nil error for a dead sandbox, want an error")
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1 (reaped even on infra failure)", sb.teardowns)
	}
}

// A gate with no checks would pass everything — that is a configuration error.
func TestRunNoChecksErrors(t *testing.T) {
	be := &fakeBackend{sb: &scriptedSandbox{id: "gate-sb"}}
	g := New(be, nil, t.TempDir(), nil)
	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run accepted a gate with no checks, want an error")
	}
	if be.provisioned != 0 {
		t.Errorf("provisioned %d sandboxes for a checkless gate, want 0", be.provisioned)
	}
}

// A provisioning failure surfaces as an error (the gate could not produce a verdict).
func TestRunProvisionFailure(t *testing.T) {
	be := &fakeBackend{provErr: errors.New("no host capacity")}
	g := New(be, []Check{{Name: "build", Cmd: "make build"}}, t.TempDir(), nil)
	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run returned nil error when provisioning failed, want an error")
	}
}
