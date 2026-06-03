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

	"github.com/Loxstomper/harness/internal/core"
)

// errRebaseConflict signals that a verified candidate could not be cleanly rebased onto
// the current main tip: another branch landed first and the two collide. Retrying cannot
// fix it (the conflict is deterministic), so the orchestrator escalates rather than
// redelivering; sandboxed, agent-driven resolution is a later increment (see
// specs/integration.md). Callers detect it with errors.Is.
var errRebaseConflict = errors.New("orchestrator: candidate conflicts with current main and cannot be cleanly rebased")

// errReGateFailed signals that the rebased result failed the re-gate (specs/integration.md
// step 3): the candidate was rebased cleanly onto the current main, but re-running the full
// gate suite against the *combined* tree found it broken — the two-green-branches case,
// where two branches each pass their own gate in isolation yet break main together. Unlike
// a conflict this is not necessarily deterministic across a different main, so the
// orchestrator routes a fix issue through the normal retry/budget machinery rather than
// dead-lettering. Callers detect it with errors.Is.
var errReGateFailed = errors.New("orchestrator: rebased result failed re-gate")

// ReGate re-verifies the rebased result before main is advanced (specs/integration.md step
// 3). The merger calls it with a gradable git ref pointing at the rebased tree that will
// land — i.e. against what will actually become main, not the branch as originally
// authored. It returns the provenance to record on the merge commit (citing the re-gate's
// own checks, since the rebased combination is what landed) and whether the result passed.
// A clean rejection (accepted=false) makes Merge return errReGateFailed without advancing
// main, so the caller can route a fix issue; a non-nil error is an infrastructure fault the
// caller retries. A nil ReGate skips re-gating (a fast-forward lands the exact tree the
// branch gate already graded, and pure-git merge tests pass nil).
type ReGate func(ctx context.Context, landedRef string) (prov core.Provenance, accepted bool, err error)

// MergeProgress is an opaque step callback the merger invokes as a candidate moves through the
// merge train's *internal* steps — rebasing (the candidate is being replayed onto the current
// main) and re-gating (the rebased combination is being re-verified). It is how the orchestrator
// announces those mid-merge transitions (T4.24) without the merger knowing anything about NATS:
// the orchestrator passes a closure that publishes a core.MergeStateEvent, the merger just calls
// it at the precise boundary, exactly as it already calls ReGate. The queue-level steps the
// orchestrator observes directly (queued on entry, landed/conflicted/regate-failed on the
// Merge return) are announced by the caller, not through this callback. A nil callback is a
// no-op (pure-git merge tests pass nil), and a state is one of the core.MergeState* constants.
type MergeProgress func(state string)

// gitMerger is the default Merger. It lands a verified candidate on main as a serialized
// merge queue (a merge train): each candidate is rebased onto the CURRENT main tip before
// a trusted provenance commit is written on top, so independently-based green branches
// integrate one at a time onto whatever main has become (see specs/integration.md).
//
// The repo is the integration repo runners push candidates into. A clean rebase is a
// deterministic git computation on objects already present there, so the trusted layer
// performs it directly (no untrusted code runs during a rebase — the candidate's own
// hooks/filters are never installed); only conflict resolution that needs an agent runs
// sandboxed. The rebased result is what will actually land, so it is the tree the re-gate
// grades before main advances (specs/integration.md step 3): the merger publishes the
// rebased result under a temporary ref and hands it to the caller's ReGate.
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
//
// When a rebase occurs, the rebased result is re-gated before it lands (specs/integration.md
// step 3) via the regate callback: the merger publishes the rebased tree under a temporary
// ref and asks regate to verify it, landing only an accepted result and recording the
// provenance regate returns. A fast-forward skips the re-gate — it lands the exact tree the
// branch gate already verified, so there is nothing new to grade. A nil regate also skips it.
func (m *gitMerger) Merge(ctx context.Context, repo, ref string, prov core.Provenance, regate ReGate, progress MergeProgress) (string, error) {
	if progress == nil {
		progress = func(string) {} // no-op so the emit sites below need no nil guard
	}
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
	landedRef := ref
	rebased := false
	if _, ffErr := m.run(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/main", ref); ffErr != nil {
		// main moved under the candidate: it must be rebased onto the current tip before it can
		// land. Announce the step before the (potentially slow, possibly conflicting) rebase so
		// the merge-queue view shows it in flight, not only after it resolves.
		progress(core.MergeStateRebasing)
		rebasedTip, tmpRef, cleanup, conflict, rerr := m.rebaseOntoMain(ctx, repo, ref, prov.Issue)
		if cleanup != nil {
			// Defer the temp-ref deletion until after main is advanced, so the rebased
			// commits stay anchored (reachable) through the re-gate, commit-tree and
			// update-ref below; once main reaches them the ref is redundant.
			defer cleanup()
		}
		if conflict {
			return "", errRebaseConflict
		}
		if rerr != nil {
			return "", rerr
		}
		landed = rebasedTip
		landedRef = tmpRef
		rebased = true
	}

	// Nothing new to integrate (the candidate's changes are already in main, e.g. a rebase
	// that replayed only already-applied commits): no-op rather than an empty provenance
	// commit.
	if landed == mainTip {
		return mainTip, nil
	}

	// Step 3: re-gate the rebased result against the tree that will actually land before
	// advancing main. Only a rebase can make the landed tree differ from the one the branch
	// gate already graded (a fast-forward lands that exact tree), so re-gating is confined to
	// the rebase path — that is where the two-green-branches breakage can hide.
	if rebased && regate != nil {
		// Announce the re-gate before running it: this is the step where two independently-green
		// branches can break together, the one the merge-queue view most wants to surface live.
		progress(core.MergeStateReGating)
		regated, accepted, gerr := regate(ctx, landedRef)
		if gerr != nil {
			return "", fmt.Errorf("orchestrator: re-gate rebased result for %q: %w", ref, gerr)
		}
		if !accepted {
			return "", errReGateFailed
		}
		prov = regated
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
// detached worktree, publishes the rebased result under a temporary ref, and returns the
// rebased tip plus that ref. The worktree isolates the rebase from the integration repo's
// own checkout (main is moved by ref update, never by checkout). On any rebase failure it
// aborts and reports conflict=true (the candidate collides with what already merged).
//
// The rebased result is published as a branch under refs/heads/ so the re-gate's sandbox —
// which seeds by cloning the integration repo, and a clone fetches only refs/heads/* and
// tags — can check it out; this is also what keeps the rebased commits reachable after the
// scratch worktree is removed (done eagerly here, since the ref now anchors them). cleanup
// deletes that temp ref and is returned only on the success path; the caller defers it
// until after main has advanced, at which point main reaches the commits and the ref is
// redundant. On conflict/error the worktree is torn down internally and no ref is left.
func (m *gitMerger) rebaseOntoMain(ctx context.Context, repo, ref, issueID string) (landed, tmpRef string, cleanup func(), conflict bool, err error) {
	parent, err := os.MkdirTemp("", "harness-rebase-")
	if err != nil {
		return "", "", nil, false, fmt.Errorf("orchestrator: create rebase worktree dir: %w", err)
	}
	// git worktree add requires a non-existent path; use a subdir of the temp parent.
	wt := filepath.Join(parent, "wt")
	removeWorktree := func() {
		_, _ = m.run(ctx, repo, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(parent)
		_, _ = m.run(ctx, repo, "worktree", "prune")
	}

	if _, err := m.run(ctx, repo, "worktree", "add", "--detach", wt, ref); err != nil {
		removeWorktree()
		return "", "", nil, false, fmt.Errorf("orchestrator: add rebase worktree for %q: %w", ref, err)
	}
	// Rebase onto main. Identity is forced because rebase re-commits. A failure is treated
	// as a conflict: abort so the worktree carries no half-applied state, tear it down, then
	// signal.
	if _, rerr := m.run(ctx, wt,
		"-c", "user.name="+provenanceCommitterName,
		"-c", "user.email="+provenanceCommitterEmail,
		"rebase", "refs/heads/main"); rerr != nil {
		_, _ = m.run(ctx, wt, "rebase", "--abort")
		removeWorktree()
		return "", "", nil, true, nil //nolint:nilerr // a rebase failure is a conflict, signaled via conflict=true, not the error channel (which is reserved for infrastructure faults)
	}
	landed, err = m.run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		removeWorktree()
		return "", "", nil, false, fmt.Errorf("orchestrator: resolve rebased tip for %q: %w", ref, err)
	}
	// Anchor the rebased commits under a clonable ref, then drop the worktree.
	tmpRef = integrationRef(issueID)
	if _, err := m.run(ctx, repo, "update-ref", tmpRef, landed); err != nil {
		removeWorktree()
		return "", "", nil, false, fmt.Errorf("orchestrator: publish rebased result ref %q: %w", tmpRef, err)
	}
	removeWorktree()
	cleanup = func() { _, _ = m.run(ctx, repo, "update-ref", "-d", tmpRef) }
	return landed, tmpRef, cleanup, false, nil
}

// integrationRef is the temporary branch the rebased result is published under so the
// re-gate's sandbox clone (which fetches refs/heads/*) can check it out; it is deleted once
// main has advanced. Keyed by issue id: integration is serialized so at most one is in
// flight, but per-issue naming keeps a ref leaked by a crash self-identifying and lets the
// next attempt for that issue overwrite it cleanly.
func integrationRef(issueID string) string {
	if issueID == "" {
		issueID = "pending"
	}
	return "refs/heads/integration/" + issueID
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
