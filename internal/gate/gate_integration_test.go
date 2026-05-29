package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// TestGateIntegration drives the gate against a real docker sandbox + git checkout.
// It proves the verifier is seeded with a clean checkout of the CANDIDATE branch
// (distinct from the producer's worktree and from main) and that build/test exit codes
// become the gate verdict. Skips when docker/git are unavailable.
func TestGateIntegration(t *testing.T) {
	requireDockerAndGit(t)

	repo := seedRepoWithCandidate(t)
	cand := func(postconditions ...string) Candidate {
		return Candidate{
			Repo:           repo,
			Ref:            core.CandidateBranch("issue-1"),
			Postconditions: postconditions,
			Profile:        "busybox:latest",
			Limits:         config.SandboxLimits{CPU: 1, Mem: "64Mi", Wall: config.Duration(2 * time.Minute)},
		}
	}
	be := sandbox.NewDockerBackend()
	ctx := context.Background()

	// marker.txt exists only on the candidate branch, so grep succeeding proves the
	// verification sandbox checked out the candidate, not main. A green build+test → pass.
	t.Run("passing candidate", func(t *testing.T) {
		g := New(be, Registry{
			"build": "grep -q candidate marker.txt",
			"test":  "true",
		}, t.TempDir(), nil)
		report, err := g.Run(ctx, cand("build", "test"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !report.Passed {
			t.Errorf("report.Passed = false, want true; checks=%+v", report.Checks)
		}
		if len(report.Checks) != 2 {
			t.Errorf("ran %d checks, want 2", len(report.Checks))
		}
	})

	// A non-zero check exit fails the gate and stops the run.
	t.Run("failing candidate", func(t *testing.T) {
		g := New(be, Registry{
			"build": "true",
			"test":  "exit 7",
		}, t.TempDir(), nil)
		report, err := g.Run(ctx, cand("build", "test"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if report.Passed {
			t.Errorf("report.Passed = true, want false")
		}
		if n := len(report.Checks); n != 2 || report.Checks[1].ExitCode != 7 {
			t.Errorf("checks = %+v, want test exit 7", report.Checks)
		}
	})
}

// requireDockerAndGit skips unless a reachable docker daemon and git are present.
func requireDockerAndGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping gate integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping gate integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable; skipping: %v", err)
	}
}

// seedRepoWithCandidate builds a repo whose main has hello.txt and whose
// candidate/issue-1 branch additionally has marker.txt. HEAD is left on main, so a gate
// that finds marker.txt must have explicitly checked out the candidate branch.
func seedRepoWithCandidate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	run("init", "-q", "-b", "main")
	write("hello.txt", "seeded\n")
	run("add", "hello.txt")
	run("commit", "-q", "-m", "seed")

	run("checkout", "-q", "-b", core.CandidateBranch("issue-1"))
	write("marker.txt", "candidate\n")
	run("add", "marker.txt")
	run("commit", "-q", "-m", "candidate work")

	run("checkout", "-q", "main") // leave HEAD on main; the gate must check out the candidate
	return dir
}
