package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// scriptedGit records git invocations and replies per subcommand, so the merger's
// fast-forward control flow is testable without a real repo.
func scriptedGit(reply func(args []string) (string, error)) (*gitMerger, *[][]string) {
	var calls [][]string
	m := &gitMerger{bin: "git"}
	m.run = func(_ context.Context, repo string, args ...string) (string, error) {
		calls = append(calls, append([]string{repo}, args...))
		return reply(args)
	}
	return m, &calls
}

func TestGitMergerFastForwards(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "merge-base": // --is-ancestor refs/heads/main <ref> -> exit 0
			return "", nil
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if commit != "abc123" {
		t.Errorf("commit = %q, want abc123", commit)
	}
	// The update-ref must move main to the resolved candidate tip.
	var sawUpdate bool
	for _, c := range *calls {
		if c[1] == "update-ref" && c[2] == "refs/heads/main" && c[3] == "abc123" {
			sawUpdate = true
		}
	}
	if !sawUpdate {
		t.Errorf("no update-ref of main to the candidate tip; calls = %v", *calls)
	}
}

func TestGitMergerRefusesNonFastForward(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "merge-base":
			return "", errors.New("exit status 1") // main is not an ancestor
		}
		return "", errors.New("unexpected")
	})

	if _, err := m.Merge(context.Background(), "/repo", "candidate/iss-1"); err == nil {
		t.Fatal("Merge accepted a non-fast-forward candidate")
	}
	// It must refuse before touching the ref.
	for _, c := range *calls {
		if c[1] == "update-ref" {
			t.Errorf("update-ref was called on a non-fast-forward; calls = %v", *calls)
		}
	}
}

func TestGitMergerDefaultBin(t *testing.T) {
	if m, ok := NewGitMerger("").(*gitMerger); !ok || m.bin != "git" {
		t.Errorf("NewGitMerger(\"\") did not default bin to git")
	}
}
