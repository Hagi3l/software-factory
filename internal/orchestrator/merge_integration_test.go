package orchestrator

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestGitMergerIntegration drives the real git binary: it builds a repo with a main
// branch and a candidate branch ahead of it, fast-forwards main onto the candidate,
// and asserts main now points at the candidate tip. It also asserts a re-merge is an
// idempotent no-op and a non-fast-forward is refused.
func TestGitMergerIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping merge integration test")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "-q")
	git("checkout", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	// candidate branch one commit ahead of main.
	git("checkout", "-q", "-b", "candidate/iss-1")
	git("commit", "-q", "--allow-empty", "-m", "candidate work")
	candidateTip := git("rev-parse", "candidate/iss-1")
	// leave HEAD off main so update-ref is the only thing that can move it.
	git("checkout", "-q", "candidate/iss-1")

	m := NewGitMerger("")
	commit, err := m.Merge(context.Background(), repo, "candidate/iss-1")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if commit != candidateTip {
		t.Errorf("returned commit %s, want candidate tip %s", commit, candidateTip)
	}
	if got := git("rev-parse", "refs/heads/main"); got != candidateTip {
		t.Errorf("main = %s, want %s after fast-forward", got, candidateTip)
	}

	// Idempotent: merging again (main already == candidate) is a no-op fast-forward.
	if _, err := m.Merge(context.Background(), repo, "candidate/iss-1"); err != nil {
		t.Errorf("re-merge of an already-merged candidate failed: %v", err)
	}

	// Non-fast-forward: a branch that diverges from main must be refused.
	git("checkout", "-q", "-b", "divergent", "HEAD~1")
	git("commit", "-q", "--allow-empty", "-m", "divergent work")
	if _, err := m.Merge(context.Background(), repo, "divergent"); err == nil {
		t.Error("Merge accepted a non-fast-forward branch")
	}
}
