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

// Merge lands a verified candidate on main and returns the new main commit. The trailer
// is the whole point: a plain fast-forward would move main to the agent's own commit,
// leaving no trusted commit to carry provenance. So the trusted layer creates a
// provenance commit on top of the candidate — same tree (no file changes), parent the
// candidate tip, authored by the harness identity, with the provenance trailer as its
// message — and moves main to it. main's tip is therefore always a trusted, attributable
// commit, and the candidate's history stays intact below it (see specs/security.md,
// specs/integration.md).
//
// It stays within fast-forward semantics: main must be an ancestor of the candidate (the
// serialized bootstrap queue never moves main under a candidate; a non-ff means something
// is wrong rather than a conflict to resolve — rebase/re-gate is Phase 3). It is
// idempotent: if the candidate is already merged (its tip is an ancestor of main, which
// holds after a prior merge since the provenance commit's parent is the candidate tip), a
// redelivered accept is a no-op that returns the current main.
func (m *gitMerger) Merge(ctx context.Context, repo, ref string, prov Provenance) (string, error) {
	tip, err := m.run(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve candidate ref %q: %w", ref, err)
	}
	// Already merged? If the candidate tip is an ancestor of (or equal to) main, a prior
	// accept already landed it; re-accepting must not stack a second provenance commit.
	if _, err := m.run(ctx, repo, "merge-base", "--is-ancestor", ref, "refs/heads/main"); err == nil {
		head, herr := m.run(ctx, repo, "rev-parse", "--verify", "refs/heads/main")
		if herr != nil {
			return "", fmt.Errorf("orchestrator: resolve main after no-op merge of %q: %w", ref, herr)
		}
		return head, nil
	}
	// Refuse anything that is not a fast-forward of main. merge-base --is-ancestor exits
	// 0 when main is an ancestor of (or equal to) the candidate tip.
	if _, err := m.run(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/main", ref); err != nil {
		return "", fmt.Errorf("orchestrator: candidate %q is not a fast-forward of main: %w", ref, err)
	}

	tree, err := m.run(ctx, repo, "rev-parse", "--verify", ref+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve candidate tree for %q: %w", ref, err)
	}
	// commit-tree writes a commit object with the candidate's tree and the candidate tip
	// as its sole parent. Identity is forced via -c so the integration commit is the
	// harness's, not whatever git config the host happens to carry.
	commit, err := m.run(ctx, repo,
		"-c", "user.name="+provenanceCommitterName,
		"-c", "user.email="+provenanceCommitterEmail,
		"commit-tree", tree, "-p", tip, "-m", prov.CommitMessage())
	if err != nil {
		return "", fmt.Errorf("orchestrator: write provenance commit for %q: %w", ref, err)
	}
	if _, err := m.run(ctx, repo, "update-ref", "refs/heads/main", commit); err != nil {
		return "", fmt.Errorf("orchestrator: advance main to provenance commit for %q: %w", ref, err)
	}
	return commit, nil
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
