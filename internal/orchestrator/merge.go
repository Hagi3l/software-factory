package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// errRebaseConflict signals that a verified candidate could not be cleanly rebased onto
// the current main tip: another branch landed first and the two collide. Retrying cannot
// fix it (the conflict is deterministic), so the orchestrator escalates rather than
// redelivering; sandboxed, agent-driven resolution is a later increment (see
// specs/integration.md). Callers detect it with errors.Is.
var errRebaseConflict = errors.New("orchestrator: candidate conflicts with current main and cannot be cleanly rebased")

// gitMerger is the default Merger. It lands a verified candidate on main as a serialized
// merge queue (a merge train): each candidate is rebased onto the CURRENT main tip before
// a trusted provenance commit is written on top, so independently-based green branches
// integrate one at a time onto whatever main has become (see specs/integration.md).
//
// The repo is the integration repo runners push candidates into. A clean rebase is a
// deterministic git computation on objects already present there, so the trusted layer
// performs it directly (no untrusted code runs during a rebase — the candidate's own
// hooks/filters are never installed); only conflict resolution that needs an agent runs
// sandboxed. The rebased result is what will actually land, so it is the tree a re-gate
// must grade (the re-gate against the rebased result is a following increment).
type gitMerger struct {
	bin string
	run gitRunFunc // seam for tests; defaults to exec
}

// gitRunFunc runs a git subcommand in dir (passed as -C dir) and returns trimmed stdout.
// It is a seam so the merge-queue control flow is unit-testable without a real repo; the
// default execs git. dir is the integration repo for most calls and the scratch rebase
// worktree for the rebase itself.
type gitRunFunc func(ctx context.Context, dir string, args ...string) (string, error)

// NewGitMerger builds the default git-backed Merger. bin is the git executable (default
// "git", resolved on PATH).
func NewGitMerger(bin string) Merger {
	if bin == "" {
		bin = "git"
	}
	m := &gitMerger{bin: bin}
	m.run = m.exec
	return m
}

// Merge lands a verified candidate on main and returns the new main commit.
//
// The serialized queue means main may have moved since the candidate branched (another
// candidate merged first). So the candidate is rebased onto the current main tip, then a
// trusted provenance commit is written on top of the rebased result — same tree (no file
// changes), parent the rebased tip, authored by the harness identity, with the provenance
// trailer as its message — and main is advanced to it. main's tip is therefore always a
// trusted, attributable commit, and advancing to it stays within fast-forward semantics
// by construction: after the rebase, main is an ancestor of the rebased tip (a plain
// fast-forward would move main to the agent's own commit, leaving no trusted commit to
// carry provenance — hence the provenance commit; see specs/security.md,
// specs/integration.md).
//
// A rebase conflict (the candidate textually collides with what already merged) returns
// errRebaseConflict — it needs resolution, not a retry.
//
// It is idempotent: a provenance commit for this issue already in main's history means a
// prior accept already landed it (whether by a clean fast-forward or a rebase), so a
// redelivered accept is a no-op that returns the current main. Keying idempotency on the
// issue id in the trailer is robust where an ancestor check is not: a rebase rewrites the
// candidate's commits to new SHAs, so the original candidate tip is not an ancestor of a
// main it merged onto via rebase.
func (m *gitMerger) Merge(ctx context.Context, repo, ref string, prov Provenance) (string, error) {
	mainTip, err := m.run(ctx, repo, "rev-parse", "--verify", "refs/heads/main")
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve main tip: %w", err)
	}
	tip, err := m.run(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve candidate ref %q: %w", ref, err)
	}

	// Already merged? A provenance commit citing this issue in main's history means a prior
	// accept landed it; re-accepting must not stack a second provenance commit.
	if prov.Issue != "" {
		if existing, _ := m.run(ctx, repo, "log", "refs/heads/main", "--fixed-strings",
			"--grep=Issue: "+prov.Issue+" |", "--format=%H", "-n", "1"); existing != "" {
			return mainTip, nil
		}
	}

	// The tip to land: if main is already an ancestor of the candidate, the candidate sits
	// on top of main and lands as-is. Otherwise main moved under it — rebase the candidate
	// onto the current main so the result is, again, a fast-forward of main.
	landed := tip
	if _, ffErr := m.run(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/main", ref); ffErr != nil {
		rebased, cleanup, conflict, rerr := m.rebaseOntoMain(ctx, repo, ref)
		if cleanup != nil {
			// Defer removal until after main is advanced, so the rebased commits stay
			// anchored (reachable) when commit-tree/update-ref run below.
			defer cleanup()
		}
		if conflict {
			return "", errRebaseConflict
		}
		if rerr != nil {
			return "", rerr
		}
		landed = rebased
	}

	// Nothing new to integrate (the candidate's changes are already in main, e.g. a rebase
	// that replayed only already-applied commits): no-op rather than an empty provenance
	// commit.
	if landed == mainTip {
		return mainTip, nil
	}

	tree, err := m.run(ctx, repo, "rev-parse", "--verify", landed+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve landed tree for %q: %w", ref, err)
	}
	// commit-tree writes a commit with the rebased tree and the rebased tip as its sole
	// parent. Identity is forced via -c so the integration commit is the harness's, not
	// whatever git config the host carries.
	commit, err := m.run(ctx, repo,
		"-c", "user.name="+provenanceCommitterName,
		"-c", "user.email="+provenanceCommitterEmail,
		"commit-tree", tree, "-p", landed, "-m", prov.CommitMessage())
	if err != nil {
		return "", fmt.Errorf("orchestrator: write provenance commit for %q: %w", ref, err)
	}
	if _, err := m.run(ctx, repo, "update-ref", "refs/heads/main", commit); err != nil {
		return "", fmt.Errorf("orchestrator: advance main to provenance commit for %q: %w", ref, err)
	}
	return commit, nil
}

// rebaseOntoMain replays the candidate's commits onto the current main tip in a scratch
// detached worktree and returns the rebased tip. The worktree isolates the rebase from
// the integration repo's own checkout (main is moved by ref update, never by checkout).
// On any rebase failure it aborts and reports conflict=true (the candidate collides with
// what already merged). cleanup removes the worktree and is returned even on failure; the
// caller defers it until after main is advanced so the rebased commits stay anchored.
func (m *gitMerger) rebaseOntoMain(ctx context.Context, repo, ref string) (landed string, cleanup func(), conflict bool, err error) {
	parent, err := os.MkdirTemp("", "harness-rebase-")
	if err != nil {
		return "", nil, false, fmt.Errorf("orchestrator: create rebase worktree dir: %w", err)
	}
	// git worktree add requires a non-existent path; use a subdir of the temp parent.
	wt := filepath.Join(parent, "wt")
	cleanup = func() {
		_, _ = m.run(ctx, repo, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(parent)
		_, _ = m.run(ctx, repo, "worktree", "prune")
	}

	if _, err := m.run(ctx, repo, "worktree", "add", "--detach", wt, ref); err != nil {
		return "", cleanup, false, fmt.Errorf("orchestrator: add rebase worktree for %q: %w", ref, err)
	}
	// Rebase onto main. Identity is forced because rebase re-commits. A failure is treated
	// as a conflict: abort so the worktree carries no half-applied state, then signal.
	if _, rerr := m.run(ctx, wt,
		"-c", "user.name="+provenanceCommitterName,
		"-c", "user.email="+provenanceCommitterEmail,
		"rebase", "refs/heads/main"); rerr != nil {
		_, _ = m.run(ctx, wt, "rebase", "--abort")
		return "", cleanup, true, nil //nolint:nilerr // a rebase failure is a conflict, signaled via conflict=true, not the error channel (which is reserved for infrastructure faults)
	}
	landed, err = m.run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", cleanup, false, fmt.Errorf("orchestrator: resolve rebased tip for %q: %w", ref, err)
	}
	return landed, cleanup, false, nil
}

func (m *gitMerger) exec(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, m.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
