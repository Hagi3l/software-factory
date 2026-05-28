package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testProvenance is a representative provenance record for merger tests.
func testProvenance() Provenance {
	return Provenance{
		Soul:      "implementor-go",
		Model:     "claude-opus-4-7",
		Issue:     "iss-1",
		PromptSHA: "sha256:9af",
		Verified:  []string{"build", "test"},
	}
}

// isAncestor reports whether a merge-base --is-ancestor call is asking "is X an ancestor
// of Y", returning (X, Y, true) when the args match that shape.
func isAncestor(args []string) (x, y string, ok bool) {
	if len(args) == 4 && args[0] == "merge-base" && args[1] == "--is-ancestor" {
		return args[2], args[3], true
	}
	return "", "", false
}

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

func TestGitMergerWritesProvenanceCommit(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if x, y, ok := isAncestor(args); ok {
			switch {
			case x == "candidate/iss-1" && y == "refs/heads/main":
				return "", errors.New("exit status 1") // not yet merged
			case x == "refs/heads/main" && y == "candidate/iss-1":
				return "", nil // main is an ancestor: fast-forward is legal
			}
			return "", errors.New("unexpected merge-base")
		}
		if hasArg(args, "commit-tree") {
			return "prov999", nil
		}
		switch args[0] {
		case "rev-parse":
			if strings.HasSuffix(args[len(args)-1], "^{tree}") {
				return "tree123", nil
			}
			return "abc123", nil // candidate tip
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// main must advance to the new trusted provenance commit, not the agent's tip.
	if commit != "prov999" {
		t.Errorf("commit = %q, want prov999 (the provenance commit)", commit)
	}

	var commitTree, updateRef []string
	for _, c := range *calls {
		switch c[1] {
		case "commit-tree", "-c": // commit-tree call carries leading -c identity flags
			if hasArg(c, "commit-tree") {
				commitTree = c
			}
		case "update-ref":
			updateRef = c
		}
	}
	if commitTree == nil {
		t.Fatalf("no commit-tree call; calls = %v", *calls)
	}
	// The commit must carry the candidate tree, the candidate tip as parent, and the
	// provenance trailer in its message; and be authored by the harness identity.
	if !hasArg(commitTree, "tree123") || !hasSeq(commitTree, "-p", "abc123") {
		t.Errorf("commit-tree missing tree/parent; got %v", commitTree)
	}
	if !hasSeq(commitTree, "-c", "user.name="+provenanceCommitterName) {
		t.Errorf("commit-tree not authored by harness identity; got %v", commitTree)
	}
	msg := argAfter(commitTree, "-m")
	if !strings.Contains(msg, "Soul: implementor-go | Model: claude-opus-4-7") ||
		!strings.Contains(msg, "Prompt-SHA: sha256:9af | Verified: build,test") {
		t.Errorf("commit-tree message missing provenance trailer; got %q", msg)
	}
	if len(updateRef) < 4 || updateRef[2] != "refs/heads/main" || updateRef[3] != "prov999" {
		t.Errorf("update-ref did not move main to the provenance commit; got %v", updateRef)
	}
}

func TestGitMergerIdempotentReMerge(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if x, y, ok := isAncestor(args); ok && x == "candidate/iss-1" && y == "refs/heads/main" {
			return "", nil // already merged: candidate tip is an ancestor of main
		}
		if args[0] == "rev-parse" {
			if args[len(args)-1] == "refs/heads/main" {
				return "mainhead", nil
			}
			return "abc123", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance())
	if err != nil {
		t.Fatalf("Merge (idempotent): %v", err)
	}
	if commit != "mainhead" {
		t.Errorf("commit = %q, want current main head mainhead", commit)
	}
	for _, c := range *calls {
		if c[1] == "commit-tree" || c[1] == "update-ref" {
			t.Errorf("re-merge of an already-merged candidate mutated the repo; calls = %v", *calls)
		}
	}
}

func TestGitMergerRefusesNonFastForward(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if _, _, ok := isAncestor(args); ok {
			return "", errors.New("exit status 1") // neither already-merged nor a fast-forward
		}
		if args[0] == "rev-parse" {
			return "abc123", nil
		}
		return "", errors.New("unexpected")
	})

	if _, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance()); err == nil {
		t.Fatal("Merge accepted a non-fast-forward candidate")
	}
	// It must refuse before touching the ref.
	for _, c := range *calls {
		if c[1] == "update-ref" || c[1] == "commit-tree" {
			t.Errorf("repo was mutated on a non-fast-forward; calls = %v", *calls)
		}
	}
}

// hasArg reports whether args contains v.
func hasArg(args []string, v string) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

// hasSeq reports whether args contains a, immediately followed by b.
func hasSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// argAfter returns the argument immediately following flag, or "".
func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestGitMergerDefaultBin(t *testing.T) {
	if m, ok := NewGitMerger("").(*gitMerger); !ok || m.bin != "git" {
		t.Errorf("NewGitMerger(\"\") did not default bin to git")
	}
}
