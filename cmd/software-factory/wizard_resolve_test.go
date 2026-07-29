package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/beads"
	"github.com/Loxstomper/software-factory/internal/controlroom/wizard"
	"github.com/Loxstomper/software-factory/internal/core"
)

// validResolveRequest is a well-formed Resolve draft: one refined spec, no broken links, no seed
// issues (Resolve refines a spec and reopens the stuck issue rather than creating new work).
func validResolveRequest(issueID string) wizard.ResolveRequest {
	return wizard.ResolveRequest{
		IssueID: issueID,
		Summary: "Clarify the orders-export acceptance criteria",
		Specs: []wizard.DraftSpec{{
			Path:    "specs/orders-export.md",
			Content: "# Orders Export\n\nExport as CSV. Reject empty and malformed rows; one header line.\n",
		}},
		Decisions:  []wizard.DecisionRecord{{Point: "Reject malformed rows", Rationale: "The ambiguity that stuck the work."}},
		Transcript: []byte(`[{"role":"user","text":"clarify the criteria"}]`),
	}
}

// TestResolveRejectsBadDraft proves the Resolve consent gate enforces the shared spec contract
// (validateSpecFiles) before any write — no specs, a broken link — so a garbled refinement never
// touches git/beads. Validation runs first, so no git/bd is needed.
func TestResolveRejectsBadDraft(t *testing.T) {
	s := &wizardSeeder{cfg: testConfig(), repo: t.TempDir()}

	if _, err := s.Resolve(context.Background(), wizard.ResolveRequest{IssueID: "h-1"}); err == nil {
		t.Error("Resolve accepted a draft with no spec edit")
	}
	bad := validResolveRequest("h-1")
	bad.Specs[0].Content += "\n[gone](nope.md)\n"
	if _, err := s.Resolve(context.Background(), bad); err == nil {
		t.Error("Resolve accepted a draft with a broken link")
	}
}

// TestResolveCommitsAndReopens is the Resolve consent gate end to end against real git + beads + an
// artifact store (T4.15): it refines the spec, commits it with the decisions sidecar, stores the
// transcript, and returns the dead-lettered issue to the ready pool (open, with its stale spec pin
// cleared) so it is re-dispatched against the clarified spec.
func TestResolveCommitsAndReopens(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")

	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	ctx := context.Background()

	// A dead-lettered issue: created, pinned to an old spec version, then blocked with a reason.
	created, err := bd.Apply(ctx, []core.Proposal{{Issue: core.Issue{
		Title: "Export orders as CSV", Role: "planner", Spec: "specs/orders-export.md",
	}}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	id := created[0].ID
	if err := bd.PinSpecHash(ctx, id, "sha256:oldversion"); err != nil {
		t.Fatalf("PinSpecHash: %v", err)
	}
	if err := bd.Block(ctx, id, "agent escalated: needs-spec-clarification"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	s := newWizardSeeder(testConfig(), repo, bd, store, nil)
	res, err := s.Resolve(ctx, validResolveRequest(id))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The refined spec + sidecar are committed (clean working tree).
	if status := runCmd(t, repo, "git", "status", "--porcelain", "--", "specs"); strings.TrimSpace(status) != "" {
		t.Errorf("specs not committed (dirty): %q", status)
	}
	if res.Commit == "" {
		t.Error("no commit sha returned")
	}
	// The transcript is stored.
	if res.TranscriptRef == "" || func() bool { ok, _ := store.Has(ctx, res.TranscriptRef); return !ok }() {
		t.Errorf("transcript not stored (ref=%q)", res.TranscriptRef)
	}

	// The dead-lettered issue is reopened against the clarified spec: status open, stale pin cleared.
	if res.ReopenedIssue != id {
		t.Errorf("ReopenedIssue = %q, want %q", res.ReopenedIssue, id)
	}
	got, err := bd.Get(ctx, id)
	if err != nil {
		t.Fatalf("read back reopened issue: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("reopened issue status = %q, want open (back in the ready pool)", got.Status)
	}
	if got.SpecHash != "" {
		t.Errorf("reopened issue still pinned to %q; Reissue must clear the stale pin so the next dispatch re-resolves the edited slice", got.SpecHash)
	}
}

// TestResolveEpicCommitsOntoEpicBranch is the bug fix's contract: under integration.mode: epic a
// Resolve refinement must land on the active epic branch (where the feature's siblings integrate),
// never on main — committing to main mid-epic would advance it before the single terminal merge and
// break the one-feature-one-landing guarantee. It also proves the refinement parents on the epic
// tip and preserves the children's already-integrated work: a sibling's code committed onto the
// epic branch before the Resolve is still present on the branch afterwards (the refinement did not
// orphan it by re-cutting from main).
func TestResolveEpicCommitsOntoEpicBranch(t *testing.T) {
	requireGitBd(t)
	repo := t.TempDir()
	gitInitWithMain(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")

	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	ctx := context.Background()
	s := newWizardSeeder(epicConfig(), repo, bd, store, nil)

	// Open the epic: Seed creates the root issue and the epic/<id> branch with the spec (T7.5).
	seedRes, err := s.Seed(ctx, validSeedRequest())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	epic := seedRes.Issues[0].ID
	branch := "epic/" + epic

	// Simulate a sibling integrating code onto the epic branch (via a worktree so the main checkout
	// is untouched), advancing the branch tip. This is the work the refinement must not orphan.
	wt := t.TempDir()
	runCmd(t, repo, "git", "worktree", "add", "-q", wt, branch)
	mustWrite(t, filepath.Join(wt, "internal", "child.go"), "package internal // integrated by a child\n")
	runCmd(t, wt, "git", "add", "internal/child.go")
	runCmd(t, wt, "git", "commit", "-q", "-m", "child: integrated code onto the epic branch")
	runCmd(t, repo, "git", "worktree", "remove", "--force", wt)
	childTip := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/"+branch))

	// Dead-letter the root so Resolve can reopen it (Reissue requires a blocked issue).
	if err := bd.Block(ctx, epic, "agent escalated: needs-spec-clarification"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	mainBefore := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/main"))

	res, err := s.Resolve(ctx, validResolveRequest(epic))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// main did not move — the refinement is held off main until the terminal merge.
	if mainAfter := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/main")); mainAfter != mainBefore {
		t.Errorf("main moved on epic Resolve: %s -> %s (must stay quiescent)", mainBefore, mainAfter)
	}
	// The refinement is the epic branch's new tip, parented on the sibling's commit (not main) —
	// so the children's work is preserved in history, not orphaned.
	if tip := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/"+branch)); tip != res.Commit {
		t.Errorf("epic branch tip = %s, want the Resolve commit %s", tip, res.Commit)
	}
	if parent := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", branch+"^")); parent != childTip {
		t.Errorf("Resolve commit parent = %s, want the epic tip %s (must build on the children's work)", parent, childTip)
	}
	// The sibling's integrated code is still on the epic branch (preserved in the snapshot tree).
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", branch+":internal/child.go").Run(); err != nil {
		t.Error("sibling's integrated code orphaned by the Resolve commit (must be preserved on the epic branch)")
	}
	// The refined spec is on the epic branch with its new content, and never on main.
	if out := runCmd(t, repo, "git", "show", branch+":specs/orders-export.md"); !strings.Contains(out, "Reject empty and malformed rows") {
		t.Errorf("refined spec missing from epic branch: %q", out)
	}
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", "main:specs/orders-export.md").Run(); err == nil {
		t.Error("spec leaked onto main (must be epic-branch-only until the terminal merge)")
	}
	// The spec is readable in the working tree, so the orchestrator can resolve the slice.
	if _, err := os.Stat(filepath.Join(repo, "specs", "orders-export.md")); err != nil {
		t.Errorf("spec not in the working tree for resolution: %v", err)
	}
	// The dead-lettered issue is reopened.
	if res.ReopenedIssue != epic {
		t.Errorf("ReopenedIssue = %q, want %q", res.ReopenedIssue, epic)
	}
	if got, gerr := bd.Get(ctx, epic); gerr != nil {
		t.Fatalf("read back reopened issue: %v", gerr)
	} else if got.Status != "open" {
		t.Errorf("reopened issue status = %q, want open", got.Status)
	}
}
