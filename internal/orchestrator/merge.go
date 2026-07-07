package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	bin        string
	signingKey string     // path to the SSH private key the provenance commit is signed with; "" = unsigned (T5.10)
	run        gitRunFunc // seam for tests; defaults to exec
}

// gitRunFunc runs a git subcommand in dir (passed as -C dir) and returns trimmed stdout.
// It is a seam so the merge-queue control flow is unit-testable without a real repo; the
// default execs git. dir is the integration repo for most calls and the scratch rebase
// worktree for the rebase itself.
type gitRunFunc func(ctx context.Context, dir string, args ...string) (string, error)

// MergerOption configures the default git-backed Merger.
type MergerOption func(*gitMerger)

// WithSigningKey makes the merger SSH-sign every provenance commit it writes with the
// harness identity key at keyPath (gpg.format=ssh). An empty path is a no-op, so the
// caller can pass the configured key unconditionally and signing simply stays off when
// none is set (T5.10, specs/security.md). The key is the harness's private SSH key; only
// the trusted provenance commit on top of the candidate is signed — the agent's own
// candidate commits below it are never signed (they are untrusted by construction).
func WithSigningKey(keyPath string) MergerOption {
	return func(m *gitMerger) { m.signingKey = keyPath }
}

// NewGitMerger builds the default git-backed Merger. bin is the git executable (default
// "git", resolved on PATH). Options configure signing (WithSigningKey).
func NewGitMerger(bin string, opts ...MergerOption) Merger {
	if bin == "" {
		bin = "git"
	}
	m := &gitMerger{bin: bin}
	for _, o := range opts {
		o(m)
	}
	m.run = m.exec
	return m
}

// Merge lands a verified candidate on the integration target and returns the new target tip
// commit. target is the fully-qualified branch the candidate integrates onto: refs/heads/main
// in per-item mode, or the epic branch refs/heads/epic/<id> in epic mode (specs/integration.md,
// T7.3). Everywhere this used to read "main", it now reads target; the epic branch is created
// off main on first use (the only place integration branches are written). The real main
// advances only later, at the epic's terminal merge (T7.4).
//
// The serialized queue means the target may have moved since the candidate branched (another
// candidate merged first). So the candidate is rebased onto the current target tip, then a
// trusted provenance commit is written on top of the rebased result — same tree (no file
// changes), parent the rebased tip, authored by the harness identity, with the provenance
// trailer as its message — and the target is advanced to it. The target's tip is therefore
// always a trusted, attributable commit, and advancing to it stays within fast-forward
// semantics by construction: after the rebase, the target is an ancestor of the rebased tip (a
// plain fast-forward would move the target to the agent's own commit, leaving no trusted commit
// to carry provenance — hence the provenance commit; see specs/security.md,
// specs/integration.md).
//
// A rebase conflict (the candidate textually collides with what already merged) returns
// errRebaseConflict — it needs resolution, not a retry.
//
// It is idempotent: a provenance commit for this issue already in the target's history means a
// prior accept already landed it (whether by a clean fast-forward or a rebase), so a
// redelivered accept is a no-op that returns the current target tip. Keying idempotency on the
// issue id in the trailer is robust where an ancestor check is not: a rebase rewrites the
// candidate's commits to new SHAs, so the original candidate tip is not an ancestor of a
// target it merged onto via rebase.
//
// When a rebase occurs, the rebased result is re-gated before it lands (specs/integration.md
// step 3) via the regate callback: the merger publishes the rebased tree under a temporary
// ref and asks regate to verify it, landing only an accepted result and recording the
// provenance regate returns. A fast-forward skips the re-gate — it lands the exact tree the
// branch gate already verified, so there is nothing new to grade. A nil regate also skips it.
func (m *gitMerger) Merge(ctx context.Context, repo, ref, target string, prov core.Provenance, regate ReGate, progress MergeProgress) (string, error) {
	if progress == nil {
		progress = func(string) {} // no-op so the emit sites below need no nil guard
	}
	if target == "" {
		target = "refs/heads/main" // defensive: a caller that names no target lands on main (per-item)
	}
	// Ensure the integration target exists. In per-item mode target is refs/heads/main, which
	// always exists (no-op). In epic mode it is the epic branch refs/heads/epic/<id>, created
	// off main the first time a child of the epic integrates — this is the only place
	// integration branches are written, so branch creation lives here with the rest of the
	// merge-queue git plumbing (specs/integration.md, T7.3). Idempotent: once the branch exists
	// the rev-parse succeeds and nothing is created.
	if _, err := m.run(ctx, repo, "rev-parse", "--verify", "--quiet", target+"^{commit}"); err != nil {
		mainTip, berr := m.run(ctx, repo, "rev-parse", "--verify", "refs/heads/main")
		if berr != nil {
			return "", fmt.Errorf("orchestrator: resolve main tip to create integration target %q: %w", target, berr)
		}
		if _, cerr := m.run(ctx, repo, "update-ref", target, mainTip); cerr != nil {
			return "", fmt.Errorf("orchestrator: create integration target %q off main: %w", target, cerr)
		}
	}
	targetTip, err := m.run(ctx, repo, "rev-parse", "--verify", target)
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve integration target %q tip: %w", target, err)
	}
	tip, err := m.run(ctx, repo, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("orchestrator: resolve candidate ref %q: %w", ref, err)
	}

	// Already merged? A provenance commit citing this issue in the target's history means a
	// prior accept landed it; re-accepting must not stack a second provenance commit.
	if prov.Issue != "" {
		if existing, _ := m.run(ctx, repo, "log", target, "--fixed-strings",
			"--grep=Issue: "+prov.Issue+" |", "--format=%H", "-n", "1"); existing != "" {
			return targetTip, nil
		}
	}

	// The tip to land: if the target is already an ancestor of the candidate, the candidate
	// sits on top of the target and lands as-is. Otherwise the target moved under it — rebase
	// the candidate onto the current target so the result is, again, a fast-forward of it.
	landed := tip
	landedRef := ref
	rebased := false
	if _, ffErr := m.run(ctx, repo, "merge-base", "--is-ancestor", target, ref); ffErr != nil {
		// The target moved under the candidate: it must be rebased onto the current tip before
		// it can land. Announce the step before the (potentially slow, possibly conflicting)
		// rebase so the merge-queue view shows it in flight, not only after it resolves.
		progress(core.MergeStateRebasing)
		rebasedTip, tmpRef, cleanup, conflict, rerr := m.rebaseOnto(ctx, repo, ref, target, prov.Issue)
		if cleanup != nil {
			// Defer the temp-ref deletion until after the target is advanced, so the rebased
			// commits stay anchored (reachable) through the re-gate, commit-tree and
			// update-ref below; once the target reaches them the ref is redundant.
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

	// Nothing new to integrate (the candidate's changes are already in the target, e.g. a
	// rebase that replayed only already-applied commits): no-op rather than an empty
	// provenance commit.
	if landed == targetTip {
		return targetTip, nil
	}

	// Step 3: re-gate the rebased result against the tree that will actually land before
	// advancing the target. Only a rebase can make the landed tree differ from the one the
	// branch gate already graded (a fast-forward lands that exact tree), so re-gating is
	// confined to the rebase path — that is where the two-green-branches breakage can hide.
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
	// whatever git config the host carries; when a signing key is configured the same
	// command SSH-signs the commit, so the target's tip is cryptographically attributable to
	// the harness, not merely labeled with its name (T5.10, specs/security.md).
	args := []string{
		"-c", "user.name=" + provenanceCommitterName,
		"-c", "user.email=" + provenanceCommitterEmail,
	}
	commitArgs := []string{"commit-tree", tree, "-p", landed, "-m", prov.CommitMessage()}
	if m.signingKey != "" {
		// gpg.format=ssh + user.signingkey point git at the harness SSH key; -S (a commit-tree
		// flag, so it precedes the tree) requests the signature. Forced via -c, like the
		// identity, so the host's own git config cannot redirect signing to a different key.
		args = append(args, "-c", "gpg.format=ssh", "-c", "user.signingkey="+m.signingKey)
		commitArgs = []string{"commit-tree", "-S", tree, "-p", landed, "-m", prov.CommitMessage()}
	}
	commit, err := m.run(ctx, repo, append(args, commitArgs...)...)
	if err != nil {
		return "", fmt.Errorf("orchestrator: write provenance commit for %q: %w", ref, err)
	}
	if _, err := m.run(ctx, repo, "update-ref", target, commit); err != nil {
		return "", fmt.Errorf("orchestrator: advance integration target %q to provenance commit for %q: %w", target, ref, err)
	}
	return commit, nil
}

// MergeEpic lands a drained epic atomically: a two-parent merge commit advancing main exactly
// once for the whole feature (specs/integration.md "The terminal merge is a merge commit"). The
// orchestrator calls it on the slow sweep when an epic's subtree has drained — every issue closed
// and nothing in flight (sweepEpicCompletion, T7.4) — having already verified the feature child by
// child as each rebased onto the epic branch and re-gated (the whole-feature gate is emergent, so
// the terminal step adds no gate in v1, where main is quiescent).
//
// The commit's first parent is the current main tip and its second parent is the epic branch tip,
// so main's first-parent history reads as one commit per feature while every per-child provenance
// commit stays reachable under the second parent (the two-tier provenance the spec requires). Its
// tree is the epic branch's tree: in v1 the epic branch was cut from main and main has not moved,
// so that tree already is the complete, re-gated feature — nothing new is introduced at the merge.
// The subject is the feature's (epic root's) title and the trailer carries the whole-feature
// provenance, signed with the harness key when one is configured, exactly like a per-item
// provenance commit.
//
// It is idempotent on the epic id: a merge commit citing it already on main means a prior sweep
// (or a redelivery) landed the feature, so it returns that commit with merged=false and writes
// nothing — the sweep re-runs every slow tick against a root that stays closed, so this is the
// steady state, not an error. merged=false with a nil error also covers the defensive case where
// the epic branch never advanced past main (no child ever integrated), which a real drain cannot
// produce but which must never write an empty merge commit.
func (m *gitMerger) MergeEpic(ctx context.Context, repo, epicRef, target string, prov core.Provenance) (string, bool, error) {
	if target == "" {
		target = "refs/heads/main" // defensive: an unnamed target lands the feature on main
	}
	mainTip, err := m.run(ctx, repo, "rev-parse", "--verify", target)
	if err != nil {
		return "", false, fmt.Errorf("orchestrator: resolve terminal-merge target %q: %w", target, err)
	}
	epicTip, err := m.run(ctx, repo, "rev-parse", "--verify", epicRef)
	if err != nil {
		return "", false, fmt.Errorf("orchestrator: resolve epic branch %q for terminal merge: %w", epicRef, err)
	}
	// Already landed? A merge commit citing this epic id on main means a prior terminal merge ran;
	// the slow sweep is at-least-once, so a repeat is expected — return that commit, write nothing.
	if prov.Issue != "" {
		if existing, _ := m.run(ctx, repo, "log", target, "--fixed-strings",
			"--grep=Issue: "+prov.Issue+" |", "--format=%H", "-n", "1"); existing != "" {
			return existing, false, nil
		}
	}
	// Nothing to land (the epic branch never advanced past main): no-op rather than an empty merge
	// commit. A genuine drain always landed at least one child onto the epic branch, so this is
	// purely defensive against a misfire.
	if epicTip == mainTip {
		return mainTip, false, nil
	}
	// Enrich the bare {Issue, Subject} the sweep passes into the whole-feature provenance layer:
	// aggregate the epic branch's per-child provenance commits (child ids + integration-commit
	// hashes + the union of their verified checks), so main's headline commit carries a real
	// feature record instead of an all-"(none)" per-item trailer (T15.4, BUG-2). Done only here,
	// on the real landing path (after both no-op guards), and never fatal — a git fault degrades
	// to the bare layer, the full accountability still lives on the reachable per-child commits.
	prov = m.featureProvenance(ctx, repo, mainTip, epicTip, prov)

	tree, err := m.run(ctx, repo, "rev-parse", "--verify", epicRef+"^{tree}")
	if err != nil {
		return "", false, fmt.Errorf("orchestrator: resolve epic tree for terminal merge of %q: %w", epicRef, err)
	}
	// commit-tree with TWO parents builds the merge commit. Identity is forced via -c (the harness
	// owns main's tip), and a configured key SSH-signs it — the same plumbing as the per-item
	// provenance commit, only with a second -p for the epic branch tip.
	args := []string{
		"-c", "user.name=" + provenanceCommitterName,
		"-c", "user.email=" + provenanceCommitterEmail,
	}
	commitArgs := []string{"commit-tree", tree, "-p", mainTip, "-p", epicTip, "-m", prov.FeatureCommitMessage()}
	if m.signingKey != "" {
		args = append(args, "-c", "gpg.format=ssh", "-c", "user.signingkey="+m.signingKey)
		commitArgs = []string{"commit-tree", "-S", tree, "-p", mainTip, "-p", epicTip, "-m", prov.FeatureCommitMessage()}
	}
	commit, err := m.run(ctx, repo, append(args, commitArgs...)...)
	if err != nil {
		return "", false, fmt.Errorf("orchestrator: write epic terminal merge commit for %q: %w", epicRef, err)
	}
	if _, err := m.run(ctx, repo, "update-ref", target, commit); err != nil {
		return "", false, fmt.Errorf("orchestrator: advance %q to epic terminal merge for %q: %w", target, epicRef, err)
	}
	return commit, true, nil
}

// featureProvenance builds the whole-feature provenance layer for an epic terminal merge by
// reading the epic branch itself — the single source of truth, no separate durable record needed
// (specs/integration.md "The whole-feature layer … is assembled from the children"). Every child
// integrated onto the epic branch by writing a trusted per-child provenance commit (with the
// agent's own candidate commits below it as ancestors). Walking mainTip..epicTip and keeping the
// commits whose message parses as a provenance trailer naming a NON-root issue recovers exactly
// those children: their issue ids, their integration-commit hashes (the %H of the provenance
// commit), and the union of the gate-check names that verified them. base carries the epic id +
// feature title; this returns it with Children and an aggregate Verified filled in. It never
// fails the landing — any git error degrades to the bare layer (BUG-2 is a rendering gap; the
// per-child commits remain the full, reachable record either way).
func (m *gitMerger) featureProvenance(ctx context.Context, repo, mainTip, epicTip string, base core.Provenance) core.Provenance {
	hashes, err := m.run(ctx, repo, "rev-list", mainTip+".."+epicTip)
	if err != nil {
		return base
	}
	childHash := map[string]string{} // child issue id -> its integration (provenance) commit hash
	var order []string               // child ids in first-seen order (deduped; sorted below for determinism)
	checks := map[string]bool{}      // union of passed gate-check NAMES across the children
	for _, hash := range strings.Fields(hashes) {
		msg, err := m.run(ctx, repo, "show", "-s", "--format=%B", hash)
		if err != nil {
			continue
		}
		cp, ok := core.ParseCommitMessage(msg)
		// Keep only per-child provenance commits: a parsed trailer naming an issue other than the
		// epic root. The agents' candidate commits below don't parse as trailers; the epic root is
		// a plan issue with no commit of its own on the branch, but guard against it defensively.
		if !ok || cp.Issue == "" || cp.Issue == base.Issue {
			continue
		}
		if _, seen := childHash[cp.Issue]; !seen {
			childHash[cp.Issue] = hash
			order = append(order, cp.Issue)
		}
		for _, v := range cp.Verified {
			name, _, _ := strings.Cut(v, "@") // Verified entries are name@<evidence-hash>; the feature summary keeps names
			if name != "" {
				checks[name] = true
			}
		}
	}
	children := make([]string, 0, len(order))
	for _, id := range order {
		children = append(children, id+"@"+childHash[id])
	}
	sort.Strings(children)
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	base.Children = children
	base.Verified = names
	return base
}

// rebaseOnto replays the candidate's commits onto the current target tip (refs/heads/main, or
// the epic branch in epic mode — T7.3) in a scratch detached worktree, publishes the rebased
// result under a temporary ref, and returns the rebased tip plus that ref. The worktree
// isolates the rebase from the integration repo's own checkout (the target is moved by ref
// update, never by checkout). On any rebase failure it aborts and reports conflict=true (the
// candidate collides with what already merged onto the target).
//
// The rebased result is published as a branch under refs/heads/ so the re-gate's sandbox —
// which seeds by cloning the integration repo, and a clone fetches only refs/heads/* and
// tags — can check it out; this is also what keeps the rebased commits reachable after the
// scratch worktree is removed (done eagerly here, since the ref now anchors them). cleanup
// deletes that temp ref and is returned only on the success path; the caller defers it
// until after the target has advanced, at which point the target reaches the commits and the
// ref is redundant. On conflict/error the worktree is torn down internally and no ref is left.
func (m *gitMerger) rebaseOnto(ctx context.Context, repo, ref, target, issueID string) (landed, tmpRef string, cleanup func(), conflict bool, err error) {
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
	// Rebase onto the target. Identity is forced because rebase re-commits. A failure is treated
	// as a conflict: abort so the worktree carries no half-applied state, tear it down, then
	// signal.
	if _, rerr := m.run(ctx, wt,
		"-c", "user.name="+provenanceCommitterName,
		"-c", "user.email="+provenanceCommitterEmail,
		"rebase", target); rerr != nil {
		_, _ = m.run(ctx, wt, "rebase", "--abort")
		removeWorktree()
		return "", "", nil, true, nil //nolint:nilerr // a rebase failure is a conflict, signaled via conflict=true, not the error channel (which is reserved for infrastructure faults)
	}
	landed, err = m.run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		removeWorktree()
		return "", "", nil, false, fmt.Errorf("orchestrator: resolve rebased tip for %q: %w", ref, err)
	}
	// Anchor the rebased commits under a clonable ref, then drop the worktree. The git
	// plumbing here (update-ref, and the -d in cleanup) needs the *fully-qualified*
	// refs/heads/integration/<id> form — update-ref does not DWIM. But the re-gate's sandbox
	// seeds by cloning the integration repo and running `git checkout <ref>`, and a clone has
	// no local refs/heads/* — only remote-tracking origin/* — so the verbatim fully-qualified
	// form fails to resolve there (the "pathspec did not match" loop that hung any multi-child
	// rebase). The *short* branch name integration/<id> DWIM-resolves to origin/integration/<id>
	// in the clone, exactly as the candidate gate's candidate/<id> already does — so that is
	// what we hand back as the re-gate's ref. (T7.1, specs/integration.md.)
	fqRef := integrationRef(issueID)
	if _, err := m.run(ctx, repo, "update-ref", fqRef, landed); err != nil {
		removeWorktree()
		return "", "", nil, false, fmt.Errorf("orchestrator: publish rebased result ref %q: %w", fqRef, err)
	}
	removeWorktree()
	cleanup = func() { _, _ = m.run(ctx, repo, "update-ref", "-d", fqRef) }
	return landed, integrationBranch(issueID), cleanup, false, nil
}

// integrationBranch is the short branch name the rebased result is published under, keyed by
// issue id: integration is serialized so at most one is in flight, but per-issue naming keeps
// a ref leaked by a crash self-identifying and lets the next attempt for that issue overwrite
// it cleanly. This short form (integration/<id>) is what the re-gate is handed: its sandbox
// seeds by cloning the integration repo and running `git checkout`, and a clone exposes
// branches only as remote-tracking origin/* refs, so the short name DWIM-resolves to
// origin/integration/<id> there (the fully-qualified refs/heads/ form does not — T7.1),
// exactly as the candidate gate uses candidate/<id>.
func integrationBranch(issueID string) string {
	if issueID == "" {
		issueID = "pending"
	}
	return "integration/" + issueID
}

// integrationRef is the fully-qualified ref the rebased result is published under (so the
// clone, which fetches refs/heads/*, carries it as origin/integration/<id>). This form is for
// git plumbing that does not DWIM — update-ref to publish it and update-ref -d to delete it
// once main has advanced. The re-gate's checkout uses the short integrationBranch form instead.
func integrationRef(issueID string) string {
	return "refs/heads/" + integrationBranch(issueID)
}

func (m *gitMerger) exec(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, m.bin, full...) // #nosec G204 -- m.bin is the configured git binary; full is the trusted, merge-path-built arg list.
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
