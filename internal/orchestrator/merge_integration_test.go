package orchestrator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// TestGitMergerIntegration drives the real git binary through the serialized merge queue:
// it builds an integration repo and three candidates that all branch from the same base,
// then merges them one at a time and asserts the merge-train behavior — a fast-forward-able
// candidate gets a provenance commit on top; a candidate whose base has moved is rebased
// onto the current main and combined linearly; a candidate that collides with what already
// merged is reported as a conflict; and a redelivered accept is an idempotent no-op.
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
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	isAncestorRef := func(anc, desc string) bool {
		return exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", anc, desc).Run() == nil
	}

	git("init", "-q")
	git("checkout", "-q", "-b", "main")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	m0 := git("rev-parse", "main")

	// Candidate A off main: adds a new file (no textual overlap with the others).
	git("checkout", "-q", "-b", "candidate/iss-1", m0)
	write("a.txt", "A\n")
	git("add", "-A")
	git("commit", "-q", "-m", "A work")
	aTip := git("rev-parse", "candidate/iss-1")

	// Candidate B off the SAME base: edits base.txt; rebases cleanly over A (disjoint change).
	git("checkout", "-q", "-b", "candidate/iss-2", m0)
	write("base.txt", "base\nfrom B\n")
	git("add", "-A")
	git("commit", "-q", "-m", "B work")

	// Candidate C off the SAME base: edits base.txt differently; conflicts once B has merged.
	git("checkout", "-q", "-b", "candidate/iss-3", m0)
	write("base.txt", "base\nfrom C\n")
	git("add", "-A")
	git("commit", "-q", "-m", "C work")

	// Candidate D off the SAME base: adds a disjoint file so it rebases cleanly over main,
	// used to drive a re-gate REJECTION (the two-green-branches case) end-to-end.
	git("checkout", "-q", "-b", "candidate/iss-4", m0)
	write("d.txt", "D\n")
	git("add", "-A")
	git("commit", "-q", "-m", "D work")

	// Detach HEAD so update-ref is the only thing that can move main.
	git("checkout", "-q", "--detach", m0)

	m := NewGitMerger("")
	provFor := func(id string) core.Provenance {
		return core.Provenance{Soul: "implementor-go", Model: "claude-opus-4-7", Issue: id, PromptSHA: "sha256:9af", Verified: []string{"build", "test"}}
	}

	// --- A: base unmoved → fast-forward-able; a trusted provenance commit on the candidate.
	cA, err := m.Merge(context.Background(), repo, "candidate/iss-1", "refs/heads/main", provFor("iss-1"), nil, nil)
	if err != nil {
		t.Fatalf("merge A: %v", err)
	}
	if cA == aTip {
		t.Error("main did not advance past the candidate tip; no provenance commit was created")
	}
	if got := git("rev-parse", "refs/heads/main"); got != cA {
		t.Errorf("main = %s, want A's provenance commit %s", got, cA)
	}
	if parent := git("rev-parse", cA+"^"); parent != aTip {
		t.Errorf("A provenance parent = %s, want candidate tip %s", parent, aTip)
	}
	if author := git("log", "-1", "--format=%an", cA); author != provenanceCommitterName {
		t.Errorf("A provenance author = %q, want harness identity %q", author, provenanceCommitterName)
	}
	if msg := git("log", "-1", "--format=%B", cA); !strings.Contains(msg, "Issue: iss-1 | Prompt-SHA: sha256:9af | Verified: build,test") {
		t.Errorf("A trailer missing from commit message; got:\n%s", msg)
	}

	// --- B: main moved under it (A merged first) → rebased onto main, re-gated against the
	// rebased result, then a provenance commit. The rebase is what makes the final advance a
	// fast-forward again. A real ReGate runs here: it must be handed a resolvable ref whose
	// tree is the *combination* (A's file + B's edit) — i.e. what will actually land, not the
	// branch as authored — and the provenance it returns is what gets committed.
	var regatedRef, regatedBase string
	regateProvB := provFor("iss-2")
	regateProvB.Verified = []string{"build@sha256:re", "test@sha256:re"} // the re-gate's own checks
	cB, err := m.Merge(context.Background(), repo, "candidate/iss-2", "refs/heads/main", provFor("iss-2"),
		func(_ context.Context, landedRef string) (core.Provenance, bool, error) {
			regatedRef = git("rev-parse", "--verify", landedRef) // must resolve: the rebased result is published
			regatedBase = git("show", landedRef+":base.txt")     // must be the combined tree
			return regateProvB, true, nil
		}, nil)
	if err != nil {
		t.Fatalf("merge B (should rebase cleanly over A): %v", err)
	}
	if regatedRef == "" {
		t.Error("re-gate was not handed a resolvable ref for the rebased result")
	}
	if regatedBase != "base\nfrom B" {
		t.Errorf("re-gate saw base.txt = %q, want the combined tree's B edit", regatedBase)
	}
	if msg := git("log", "-1", "--format=%B", "refs/heads/main"); !strings.Contains(msg, "Verified: build@sha256:re,test@sha256:re") {
		t.Errorf("merge did not record the re-gate's provenance; got:\n%s", msg)
	}
	// The published temp ref is deleted once main has advanced (it is redundant — main reaches it).
	if exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/integration/iss-2").Run() == nil {
		t.Error("the published rebased-result ref was not cleaned up after main advanced")
	}
	if got := git("rev-parse", "refs/heads/main"); got != cB {
		t.Errorf("main = %s, want B's provenance commit %s", got, cB)
	}
	if !isAncestorRef(cA, cB) {
		t.Error("main did not stay linear: A's commit is not an ancestor of B's merged result")
	}
	// The merged tree carries BOTH branches' work: A's new file and B's edit to base.txt.
	tree := git("ls-tree", "-r", "--name-only", cB)
	if !strings.Contains(tree, "a.txt") || !strings.Contains(tree, "base.txt") {
		t.Errorf("merged tree missing combined work; got files:\n%s", tree)
	}
	if got := git("show", cB+":base.txt"); got != "base\nfrom B" {
		t.Errorf("merged base.txt = %q, want B's edit", got)
	}

	// --- C: edits base.txt where B already did → rebase conflict, reported not retried, and
	// main is left untouched.
	if _, err := m.Merge(context.Background(), repo, "candidate/iss-3", "refs/heads/main", provFor("iss-3"), nil, nil); !errors.Is(err, errRebaseConflict) {
		t.Fatalf("merge C err = %v, want errRebaseConflict", err)
	}
	if got := git("rev-parse", "refs/heads/main"); got != cB {
		t.Errorf("main moved on a conflicting candidate: %s != %s", got, cB)
	}

	// --- D: rebases cleanly over main (disjoint file) but the re-gate REJECTS the rebased
	// result (the two-green-branches case). Merge returns errReGateFailed, main is untouched,
	// and the published temp ref is cleaned up — the orchestrator routes a fix from here.
	if _, err := m.Merge(context.Background(), repo, "candidate/iss-4", "refs/heads/main", provFor("iss-4"),
		func(_ context.Context, _ string) (core.Provenance, bool, error) {
			return core.Provenance{}, false, nil // the combination broke something
		}, nil); !errors.Is(err, errReGateFailed) {
		t.Fatalf("merge D err = %v, want errReGateFailed", err)
	}
	if got := git("rev-parse", "refs/heads/main"); got != cB {
		t.Errorf("main moved on a re-gate-rejected candidate: %s != %s", got, cB)
	}
	if exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/integration/iss-4").Run() == nil {
		t.Error("the published rebased-result ref was not cleaned up after a rejected re-gate")
	}

	// --- Idempotent: re-merging A (whose provenance commit is still in main's history below
	// B) is a no-op that returns the current main, even though A's tip is no longer an
	// ancestor of main via a simple chain.
	again, err := m.Merge(context.Background(), repo, "candidate/iss-1", "refs/heads/main", provFor("iss-1"), nil, nil)
	if err != nil {
		t.Errorf("re-merge of an already-merged candidate failed: %v", err)
	}
	if again != cB {
		t.Errorf("re-merge returned %s, want the unchanged main %s", again, cB)
	}
	if got := git("rev-parse", "refs/heads/main"); got != cB {
		t.Errorf("main moved on a redundant re-merge: %s != %s", got, cB)
	}
}

// TestGitMergerEpicTargetIntegration drives the real git binary through the merge queue in
// epic mode (T7.3): two sibling candidates of one epic integrate onto an epic/<id> branch that
// does not exist yet, and the real `main` must NEVER move. It proves the full retargeting — the
// merger creates the epic branch off main on first use, fast-forwards the first child onto it,
// then rebases + lands the second onto the moved epic branch — all while main stays put (the
// real main advances only at the epic's terminal merge, T7.4). This is the atomic-landing
// invariant: child integration touches the epic branch, never main.
func TestGitMergerEpicTargetIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping epic merge integration test")
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
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	exists := func(ref string) bool {
		return exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Run() == nil
	}

	git("init", "-q")
	git("checkout", "-q", "-b", "main")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	m0 := git("rev-parse", "main")

	// Two children of the same epic, both off main: A adds a file, B edits base.txt. Disjoint,
	// so B rebases cleanly over A on the epic branch.
	git("checkout", "-q", "-b", "candidate/iss-1", m0)
	write("a.txt", "A\n")
	git("add", "-A")
	git("commit", "-q", "-m", "A work")
	git("checkout", "-q", "-b", "candidate/iss-2", m0)
	write("base.txt", "base\nfrom B\n")
	git("add", "-A")
	git("commit", "-q", "-m", "B work")
	git("checkout", "-q", "--detach", m0) // only update-ref may move refs now

	const epicRef = "refs/heads/epic/feat-1"
	if exists(epicRef) {
		t.Fatal("epic branch unexpectedly exists before any integration")
	}

	m := NewGitMerger("")
	provFor := func(id string) core.Provenance {
		return core.Provenance{Soul: "implementor-go", Model: "claude-opus-4-7", Issue: id, PromptSHA: "sha256:9af", Verified: []string{"build", "test"}}
	}

	// Child A onto the not-yet-existent epic branch: the merger creates epic/feat-1 off main and
	// lands a provenance commit on it. main must not move.
	cA, err := m.Merge(context.Background(), repo, "candidate/iss-1", epicRef, provFor("iss-1"), nil, nil)
	if err != nil {
		t.Fatalf("merge A onto epic: %v", err)
	}
	if !exists(epicRef) {
		t.Fatal("epic branch was not created off main on first integration")
	}
	if git("rev-parse", "main") != m0 {
		t.Error("main advanced during child integration; epic-mode children must touch only the epic branch")
	}
	if git("rev-parse", epicRef) != cA {
		t.Errorf("epic branch tip = %q, want the child-A provenance commit %q", git("rev-parse", epicRef), cA)
	}

	// Child B rebases onto the moved epic branch (A already landed) and re-gates; the re-gate
	// must see the COMBINED tree (A's a.txt + B's base.txt edit), and only the epic branch moves.
	var regatedBase, regatedHasA string
	cB, err := m.Merge(context.Background(), repo, "candidate/iss-2", epicRef, provFor("iss-2"),
		func(_ context.Context, landedRef string) (core.Provenance, bool, error) {
			regatedBase = git("show", landedRef+":base.txt") // B's edit
			regatedHasA = git("show", landedRef+":a.txt")    // A's file, present after rebase
			return provFor("iss-2"), true, nil
		}, nil)
	if err != nil {
		t.Fatalf("merge B onto epic: %v", err)
	}
	if regatedBase != "base\nfrom B" {
		t.Errorf("re-gate saw base.txt = %q, want B's edit on the combined tree", regatedBase)
	}
	if regatedHasA != "A" {
		t.Errorf("re-gate combined tree missing A's file; a.txt = %q", regatedHasA)
	}
	if git("rev-parse", "main") != m0 {
		t.Error("main advanced during the second child integration; it must stay quiescent during an epic")
	}
	if git("rev-parse", epicRef) != cB {
		t.Error("epic branch did not advance to the child-B provenance commit")
	}
	// The epic branch now carries both children linearly; main is still the lone base commit.
	if log := git("log", "--oneline", epicRef); !strings.Contains(log, "A work") || !strings.Contains(log, "B work") {
		t.Errorf("epic branch missing a child's work:\n%s", log)
	}
}

// TestGitMergerSignsProvenanceCommitIntegration drives the real git binary + ssh-keygen to
// prove the end-to-end signing path (T5.10): a merger built WithSigningKey produces an
// integration commit that git verifies (%G? = G) against an allowed-signers file mapping the
// harness principal to its public key, while a merger with no key leaves an unsigned commit
// (%G? = N). This is the cryptographic half of "the audit trail is the accountability" — main's
// tip is provably the harness's, not merely labeled with its name (specs/security.md).
func TestGitMergerSignsProvenanceCommitIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping signing integration test")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH; skipping signing integration test")
	}
	repo := t.TempDir()
	keyDir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "main")
	mkCandidate := func(branch, file string) {
		git("checkout", "-q", "-b", branch, base)
		if err := os.WriteFile(filepath.Join(repo, file), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", branch+" work")
	}
	mkCandidate("candidate/iss-1", "a.txt")
	mkCandidate("candidate/iss-2", "b.txt")
	git("checkout", "-q", "--detach", base)

	// Generate the harness SSH signing identity. The key comment is irrelevant; the
	// allowed-signers principal must match the committer email the merger stamps.
	keyPath := filepath.Join(keyDir, "harness_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "harness", "-f", keyPath, "-q").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(keyDir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte(provenanceCommitterEmail+" "+string(pub)), 0o600); err != nil {
		t.Fatal(err)
	}

	prov := func(id string) core.Provenance {
		return core.Provenance{Soul: "implementor-go", Model: "claude-opus-4-7", Issue: id, PromptSHA: "sha256:9af", Verified: []string{"build"}}
	}
	// %G? against the allowed-signers file: G = good + recognized principal, N = unsigned.
	gflag := func(commit string) string {
		cmd := exec.Command("git", "-C", repo,
			"-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowed,
			"show", "--no-patch", "--format=%G?", commit)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git show %%G?: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	// Signed merger: the integration commit verifies.
	signed := NewGitMerger("", WithSigningKey(keyPath))
	cSigned, err := signed.Merge(context.Background(), repo, "candidate/iss-1", "refs/heads/main", prov("iss-1"), nil, nil)
	if err != nil {
		t.Fatalf("signed merge: %v", err)
	}
	if g := gflag(cSigned); g != "G" {
		t.Errorf("signed commit %%G? = %q, want \"G\" (good signature, recognized principal)", g)
	}
	// And `git verify-commit` (the canonical check) succeeds against the allowed-signers file.
	vc := exec.Command("git", "-C", repo,
		"-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowed,
		"verify-commit", cSigned)
	if out, err := vc.CombinedOutput(); err != nil {
		t.Errorf("git verify-commit on the signed integration commit failed: %v\n%s", err, out)
	}

	// Unsigned merger: the integration commit carries no signature.
	unsigned := NewGitMerger("")
	cUnsigned, err := unsigned.Merge(context.Background(), repo, "candidate/iss-2", "refs/heads/main", prov("iss-2"), nil, nil)
	if err != nil {
		t.Fatalf("unsigned merge: %v", err)
	}
	if g := gflag(cUnsigned); g != "N" {
		t.Errorf("unsigned commit %%G? = %q, want \"N\" (no signature)", g)
	}
}
