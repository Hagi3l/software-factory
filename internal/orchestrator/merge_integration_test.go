package orchestrator

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestGitMergerIntegration drives the real git binary: it builds a repo with a main
// branch and a candidate branch ahead of it, merges main onto the candidate, and asserts
// main now points at a trusted provenance commit on top of the candidate (same tree,
// candidate tip as parent, the provenance trailer in its message, the harness identity).
// It also asserts a re-merge is an idempotent no-op and a non-fast-forward is refused.
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

	prov := Provenance{Soul: "implementor-go", Model: "claude-opus-4-7", Issue: "iss-1", PromptSHA: "sha256:9af", Verified: []string{"build", "test"}}
	m := NewGitMerger("")
	commit, err := m.Merge(context.Background(), repo, "candidate/iss-1", prov)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// main is a NEW provenance commit on top of the candidate, not the agent's tip.
	if commit == candidateTip {
		t.Errorf("main did not advance past the candidate tip; provenance commit was not created")
	}
	if got := git("rev-parse", "refs/heads/main"); got != commit {
		t.Errorf("main = %s, want the provenance commit %s", got, commit)
	}
	// Its parent is the candidate tip (candidate history preserved below the trailer).
	if parent := git("rev-parse", commit+"^"); parent != candidateTip {
		t.Errorf("provenance commit parent = %s, want candidate tip %s", parent, candidateTip)
	}
	// Same tree as the candidate: the merge adds provenance, not file changes.
	if git("rev-parse", commit+"^{tree}") != git("rev-parse", candidateTip+"^{tree}") {
		t.Error("provenance commit tree differs from the candidate tree")
	}
	// The trailer is in the message, and the commit is authored by the harness identity.
	msg := git("log", "-1", "--format=%B", commit)
	if !strings.Contains(msg, "Soul: implementor-go | Model: claude-opus-4-7") ||
		!strings.Contains(msg, "Issue: iss-1 | Prompt-SHA: sha256:9af | Verified: build,test") {
		t.Errorf("provenance trailer missing from commit message; got:\n%s", msg)
	}
	if author := git("log", "-1", "--format=%an", commit); author != provenanceCommitterName {
		t.Errorf("provenance commit author = %q, want %q", author, provenanceCommitterName)
	}

	// Idempotent: merging again is a no-op that returns the same main commit.
	again, err := m.Merge(context.Background(), repo, "candidate/iss-1", prov)
	if err != nil {
		t.Errorf("re-merge of an already-merged candidate failed: %v", err)
	}
	if again != commit {
		t.Errorf("re-merge returned %s, want the unchanged main %s", again, commit)
	}
	if got := git("rev-parse", "refs/heads/main"); got != commit {
		t.Errorf("main moved on a redundant re-merge: %s != %s", got, commit)
	}

	// Non-fast-forward: a branch that diverges from main must be refused.
	git("checkout", "-q", "-b", "divergent", candidateTip+"~1")
	git("commit", "-q", "--allow-empty", "-m", "divergent work")
	if _, err := m.Merge(context.Background(), repo, "divergent", prov); err == nil {
		t.Error("Merge accepted a non-fast-forward branch")
	}
}
