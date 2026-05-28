package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// gitMerger is the default Merger: it fast-forwards main onto a verified candidate
// branch in the integration repo by moving the ref, without touching a working tree.
// The bootstrap merge queue is a single serialized stream — no rebase, no re-gate —
// so a fast-forward is the whole of integration (see specs/integration.md,
// specs/bootstrap.md). The repo is used purely as a ref store: runners push candidates
// into it (git fetch of a bundle) and the orchestrator advances main here, both via
// ref updates, so no checkout is ever required.
type gitMerger struct {
	bin string
	run gitRunFunc // seam for tests; defaults to exec
}

// gitRunFunc runs a git subcommand in repo and returns trimmed stdout. It is a seam so
// the fast-forward control flow is unit-testable without a real repo; the default
// execs git.
type gitRunFunc func(ctx context.Context, repo string, args ...string) (string, error)

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

// Merge fast-forwards refs/heads/main in repo to the tip of ref and returns the new
// commit. It refuses a non-fast-forward (main is not an ancestor of the candidate):
// in the serialized bootstrap queue main does not move under a candidate, so a
// non-ff means something is wrong rather than a conflict to resolve (rebase/re-gate is
// Phase 3). It is idempotent — re-merging an already-merged candidate is a no-op
// fast-forward — so a redelivered Result is safe to re-accept.
func (m *gitMerger) Merge(ctx context.Context, repo, ref string) (string, error) {
	tip, err := m.run(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve candidate ref %q: %w", ref, err)
	}
	// Refuse anything that is not a fast-forward of main. merge-base --is-ancestor exits
	// 0 when main is an ancestor of (or equal to) the candidate tip.
	if _, err := m.run(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/main", ref); err != nil {
		return "", fmt.Errorf("orchestrator: candidate %q is not a fast-forward of main: %w", ref, err)
	}
	if _, err := m.run(ctx, repo, "update-ref", "refs/heads/main", tip); err != nil {
		return "", fmt.Errorf("orchestrator: fast-forward main to %q: %w", ref, err)
	}
	return tip, nil
}

func (m *gitMerger) exec(ctx context.Context, repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
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
