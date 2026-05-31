package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
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
