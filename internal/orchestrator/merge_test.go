package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// testProvenance is a representative provenance record for merger tests.
func testProvenance() core.Provenance {
	return core.Provenance{
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

// isGrep reports whether args is the idempotency log --grep probe.
func isGrep(args []string) bool {
	return len(args) > 0 && args[0] == "log" && hasArg(args, "--fixed-strings")
}

// scriptedGit records git invocations and replies per subcommand, so the merge-queue
// control flow is testable without a real repo.
func scriptedGit(reply func(args []string) (string, error)) (*gitMerger, *[][]string) {
	var calls [][]string
	m := &gitMerger{bin: "git"}
	m.run = func(_ context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, append([]string{dir}, args...))
		return reply(args)
	}
	return m, &calls
}

// TestGitMergerWritesProvenanceCommit drives the fast-forward-able case: main is still an
// ancestor of the candidate (its base has not moved), so no rebase is needed and a trusted
// provenance commit is written on top of the candidate tip.
func TestGitMergerWritesProvenanceCommit(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "", nil // no provenance commit for this issue yet
		}
		if x, y, ok := isAncestor(args); ok {
			if x == "refs/heads/main" && y == "candidate/iss-1" {
				return "", nil // main is an ancestor of the candidate: lands as-is, no rebase
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
			if args[len(args)-1] == "refs/heads/main" {
				return "mainhead", nil
			}
			return "abc123", nil // candidate tip
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(), nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// main must advance to the new trusted provenance commit, not the agent's tip.
	if commit != "prov999" {
		t.Errorf("commit = %q, want prov999 (the provenance commit)", commit)
	}

	var commitTree, updateRef []string
	for _, c := range *calls {
		if hasArg(c, "rebase") || hasArg(c, "worktree") {
			t.Errorf("rebased a fast-forward-able candidate; call = %v", c)
		}
		switch {
		case hasArg(c, "commit-tree"):
			commitTree = c
		case len(c) > 1 && c[1] == "update-ref":
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

// TestGitMergerRebasesWhenBaseMoved drives the merge-queue case: main has moved under the
// candidate (another branch merged first), so the candidate is rebased onto the current
// main in a scratch worktree and the provenance commit is written on the rebased tip.
func TestGitMergerRebasesWhenBaseMoved(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "", nil // not merged
		}
		if x, y, ok := isAncestor(args); ok && x == "refs/heads/main" && y == "candidate/iss-1" {
			return "", errors.New("exit status 1") // main is NOT an ancestor → rebase needed
		}
		if hasArg(args, "rebase") {
			return "", nil // clean rebase
		}
		if hasArg(args, "worktree") {
			return "", nil
		}
		if hasArg(args, "commit-tree") {
			return "prov-rebased", nil
		}
		switch args[0] {
		case "rev-parse":
			switch {
			case strings.HasSuffix(args[len(args)-1], "^{tree}"):
				return "rebasedtree", nil
			case args[len(args)-1] == "refs/heads/main":
				return "newmain", nil
			case args[len(args)-1] == "HEAD":
				return "rebasedtip", nil // the rebased tip in the worktree
			}
			return "candtip", nil // candidate tip
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(), nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if commit != "prov-rebased" {
		t.Errorf("commit = %q, want prov-rebased", commit)
	}

	var addedWorktree, rebased, commitTree []string
	for _, c := range *calls {
		switch {
		case hasSeq(c, "worktree", "add"):
			addedWorktree = c
		case hasArg(c, "rebase") && !hasArg(c, "--abort"):
			rebased = c
		case hasArg(c, "commit-tree"):
			commitTree = c
		}
	}
	if addedWorktree == nil {
		t.Errorf("no scratch worktree was added for the rebase; calls = %v", *calls)
	}
	if rebased == nil || !hasArg(rebased, "refs/heads/main") {
		t.Errorf("candidate was not rebased onto main; got %v", rebased)
	}
	// The provenance commit is built on the REBASED tip and tree, not the original candidate.
	if commitTree == nil || !hasArg(commitTree, "rebasedtree") || !hasSeq(commitTree, "-p", "rebasedtip") {
		t.Errorf("provenance commit not built on the rebased result; got %v", commitTree)
	}
}

// TestGitMergerReGatesRebasedResult: when the candidate is rebased onto a moved main, the
// merger publishes the rebased result under a clonable temp ref, hands that ref to the
// ReGate (specs/integration.md step 3), and — on a passing verdict — writes the provenance
// the ReGate returns (citing the combination's own checks, since the rebased tree is what
// lands) then deletes the temp ref once main has advanced.
func TestGitMergerReGatesRebasedResult(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "", nil
		}
		if x, y, ok := isAncestor(args); ok && x == "refs/heads/main" && y == "candidate/iss-1" {
			return "", errors.New("exit status 1") // main is NOT an ancestor → rebase needed
		}
		if hasArg(args, "rebase") || hasArg(args, "worktree") {
			return "", nil
		}
		if hasArg(args, "commit-tree") {
			return "prov-rebased", nil
		}
		switch args[0] {
		case "rev-parse":
			switch {
			case strings.HasSuffix(args[len(args)-1], "^{tree}"):
				return "rebasedtree", nil
			case args[len(args)-1] == "refs/heads/main":
				return "newmain", nil
			case args[len(args)-1] == "HEAD":
				return "rebasedtip", nil
			}
			return "candtip", nil
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	regateProv := testProvenance()
	regateProv.Verified = []string{"regate-build", "regate-test"} // distinct from the branch gate
	var gotRef string
	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(),
		func(_ context.Context, landedRef string) (core.Provenance, bool, error) {
			gotRef = landedRef
			return regateProv, true, nil
		})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if commit != "prov-rebased" {
		t.Errorf("commit = %q, want prov-rebased", commit)
	}
	// The re-gate graded the PUBLISHED ref (a clonable refs/heads/ branch a verification
	// sandbox clone can fetch), not the original candidate.
	if gotRef != "refs/heads/integration/iss-1" {
		t.Errorf("re-gate ref = %q, want refs/heads/integration/iss-1 (the published rebased result)", gotRef)
	}

	var commitTree, published, deletedRef []string
	for _, c := range *calls {
		switch {
		case hasArg(c, "commit-tree"):
			commitTree = c
		case hasSeq(c, "update-ref", "refs/heads/integration/iss-1"):
			published = c
		case hasSeq(c, "update-ref", "-d"):
			deletedRef = c
		}
	}
	if commitTree == nil {
		t.Fatalf("no commit-tree call; calls = %v", *calls)
	}
	// The provenance commit carries the RE-GATE's trailer, since the combination is what landed.
	if msg := argAfter(commitTree, "-m"); !strings.Contains(msg, "Verified: regate-build,regate-test") {
		t.Errorf("provenance commit does not cite the re-gate's checks; got %q", msg)
	}
	if published == nil {
		t.Errorf("rebased result was not published under a clonable ref; calls = %v", *calls)
	}
	if deletedRef == nil {
		t.Errorf("temp ref was not cleaned up after main advanced; calls = %v", *calls)
	}
}

// TestGitMergerReGateRejectionAbortsMerge: when the ReGate rejects the rebased result (the
// two-green-branches case — the combination is broken), Merge returns errReGateFailed and
// never advances main (no provenance commit, no update-ref on main) so the caller can route
// a fix; the published temp ref is still cleaned up on the way out.
func TestGitMergerReGateRejectionAbortsMerge(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "", nil
		}
		if x, y, ok := isAncestor(args); ok && x == "refs/heads/main" && y == "candidate/iss-1" {
			return "", errors.New("exit status 1") // rebase needed
		}
		if hasArg(args, "rebase") || hasArg(args, "worktree") {
			return "", nil
		}
		switch args[0] {
		case "rev-parse":
			if args[len(args)-1] == "refs/heads/main" {
				return "newmain", nil
			}
			if args[len(args)-1] == "HEAD" {
				return "rebasedtip", nil
			}
			return "candtip", nil
		case "update-ref":
			return "", nil
		}
		return "", errors.New("unexpected")
	})

	_, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(),
		func(_ context.Context, _ string) (core.Provenance, bool, error) {
			return core.Provenance{}, false, nil // the combination failed the re-gate
		})
	if !errors.Is(err, errReGateFailed) {
		t.Fatalf("Merge err = %v, want errReGateFailed", err)
	}
	var deleted bool
	for _, c := range *calls {
		if hasArg(c, "commit-tree") {
			t.Errorf("a provenance commit was written despite a failed re-gate; call = %v", c)
		}
		if hasSeq(c, "update-ref", "refs/heads/main") {
			t.Errorf("main was advanced despite a failed re-gate; call = %v", c)
		}
		if hasSeq(c, "update-ref", "-d") {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("temp ref not cleaned up after the aborted merge; calls = %v", *calls)
	}
}

// TestGitMergerRebaseConflictReported: a rebase that fails to apply (a collision with what
// already merged) is reported as errRebaseConflict and mutates nothing on main.
func TestGitMergerRebaseConflictReported(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "", nil
		}
		if x, y, ok := isAncestor(args); ok && x == "refs/heads/main" && y == "candidate/iss-1" {
			return "", errors.New("exit status 1") // rebase needed
		}
		if hasArg(args, "rebase") && !hasArg(args, "--abort") {
			return "", errors.New("CONFLICT (content): merge conflict") // the rebase conflicts
		}
		if hasArg(args, "rebase") || hasArg(args, "worktree") {
			return "", nil // --abort, worktree remove/prune
		}
		if args[0] == "rev-parse" {
			if args[len(args)-1] == "refs/heads/main" {
				return "newmain", nil
			}
			return "candtip", nil
		}
		return "", errors.New("unexpected")
	})

	_, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(), nil)
	if !errors.Is(err, errRebaseConflict) {
		t.Fatalf("Merge err = %v, want errRebaseConflict", err)
	}
	for _, c := range *calls {
		if hasArg(c, "commit-tree") || (len(c) > 1 && c[1] == "update-ref") {
			t.Errorf("repo was mutated on a rebase conflict; call = %v", c)
		}
		if hasArg(c, "rebase") && hasArg(c, "--abort") {
			return // aborted the conflicted rebase: good
		}
	}
	t.Error("the conflicted rebase was not aborted")
}

// TestGitMergerIdempotentReMerge: a candidate whose issue already has a provenance commit
// in main's history is a no-op — robust whether the prior merge fast-forwarded or rebased,
// since it keys on the issue id in the trailer rather than commit ancestry.
func TestGitMergerIdempotentReMerge(t *testing.T) {
	m, calls := scriptedGit(func(args []string) (string, error) {
		if isGrep(args) {
			return "prov-existing", nil // a provenance commit for iss-1 is already on main
		}
		if args[0] == "rev-parse" {
			if args[len(args)-1] == "refs/heads/main" {
				return "mainhead", nil
			}
			return "abc123", nil
		}
		return "", errors.New("unexpected")
	})

	commit, err := m.Merge(context.Background(), "/repo", "candidate/iss-1", testProvenance(), nil)
	if err != nil {
		t.Fatalf("Merge (idempotent): %v", err)
	}
	if commit != "mainhead" {
		t.Errorf("commit = %q, want current main head mainhead", commit)
	}
	for _, c := range *calls {
		if hasArg(c, "commit-tree") || hasArg(c, "rebase") || (len(c) > 1 && c[1] == "update-ref") {
			t.Errorf("re-merge of an already-merged candidate mutated the repo; calls = %v", *calls)
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
