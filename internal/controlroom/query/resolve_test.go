package query

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
)

// writeSpecs materializes a slash-relative path -> content map under a fresh temp repo and
// returns the root — the on-disk spec tree BlastRadius/ResolveContext resolve slices from.
func writeSpecs(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestBlastRadiusInFlightMembership proves the preview reports exactly the in-flight issues whose
// spec slice *includes* an edited path — the recompileSpecDelta predicate, run read-only (T4.15).
// An issue whose slice does not reach the edit, and one with no pin, are left out, mirroring the
// sweep's conservatism.
func TestBlastRadiusInFlightMembership(t *testing.T) {
	repo := writeSpecs(t, map[string]string{
		"specs/a.md": "# A\nsee [b](b.md)\n", // a's slice includes b at depth 1
		"specs/b.md": "# B\n",
		"specs/c.md": "# C\n", // unrelated
	})
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Role: "implement", Status: "in_progress", Spec: "specs/a.md", SpecHash: "sha256:pin1"}, // slice includes b → affected
		{ID: "h-2", Role: "qa", Status: "in_progress", Spec: "specs/c.md", SpecHash: "sha256:pin2"},        // slice excludes b → not
		{ID: "h-3", Role: "implement", Status: "in_progress", Spec: "specs/a.md", SpecHash: ""},            // no pin → skipped
		{ID: "h-4", Role: "implement", Status: "open", Spec: "specs/a.md", SpecHash: "sha256:pin4"},        // not in_progress → skipped
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	br, err := r.BlastRadius(context.Background(), repo, 1, nil, []string{"specs/b.md"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if len(br.InFlight) != 1 || br.InFlight[0].ID != "h-1" {
		t.Fatalf("InFlight = %+v, want only h-1 (its slice includes the edited specs/b.md)", br.InFlight)
	}
	if br.InFlight[0].SpecHash != "sha256:pin1" {
		t.Errorf("InFlight hash = %q, want the pinned version about to change", br.InFlight[0].SpecHash)
	}
	if len(br.EditedSpecs) != 1 || br.EditedSpecs[0] != "specs/b.md" {
		t.Errorf("EditedSpecs = %v, want [specs/b.md]", br.EditedSpecs)
	}
}

// TestBlastRadiusMergedGrouping proves merged drift is reported per (epic, spec-path) unit — the
// recompileMergedDelta dedupe — once, with the closed-member count, and only when a member carries
// a pin (an unpinned closed group could not drift).
func TestBlastRadiusMergedGrouping(t *testing.T) {
	repo := writeSpecs(t, map[string]string{
		"specs/a.md": "# A\nsee [b](b.md)\n",
		"specs/b.md": "# B\n",
	})
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Status: "closed", EpicID: "e1", Spec: "specs/a.md", SpecHash: "sha256:z1"},
		{ID: "h-2", Status: "closed", EpicID: "e1", Spec: "specs/a.md", SpecHash: "sha256:z2"}, // same group as h-1
		{ID: "h-3", Status: "closed", EpicID: "e2", Spec: "specs/a.md", SpecHash: ""},          // unpinned group → skipped
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	br, err := r.BlastRadius(context.Background(), repo, 1, nil, []string{"specs/b.md"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if len(br.Merged) != 1 {
		t.Fatalf("Merged = %+v, want one (epic, path) group (e1/specs/a.md), deduped across h-1+h-2", br.Merged)
	}
	g := br.Merged[0]
	if g.Epic != "e1" || g.Spec != "specs/a.md" || g.Members != 2 {
		t.Errorf("Merged group = %+v, want {e1 specs/a.md 2}", g)
	}
}

// TestBlastRadiusAmbientEditTouchesAllPinned proves editing an ambient spec (T3.14) drifts
// EVERY pinned issue regardless of its governing spec — ambient files ride in every slice, so
// the recompile sweeps re-hash them all. h-2's slice does not include the edited path, but the
// edit IS an ambient file, so it is affected anyway; the unpinned h-3 is still skipped (no
// version to diff). The preview must match the ambient-aware sweeps.
func TestBlastRadiusAmbientEditTouchesAllPinned(t *testing.T) {
	repo := writeSpecs(t, map[string]string{
		"specs/conventions.md": "# Conventions\n",
		"specs/a.md":           "# A\n",
		"specs/c.md":           "# C\n",
	})
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Role: "implement", Status: "in_progress", Spec: "specs/a.md", SpecHash: "sha256:pin1"},
		{ID: "h-2", Role: "qa", Status: "in_progress", Spec: "specs/c.md", SpecHash: "sha256:pin2"}, // unrelated spec, still drifts
		{ID: "h-3", Role: "implement", Status: "in_progress", Spec: "specs/a.md", SpecHash: ""},     // no pin → skipped
		{ID: "h-4", Status: "closed", EpicID: "e1", Spec: "specs/a.md", SpecHash: "sha256:z1"},      // merged group, also re-derived
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	br, err := r.BlastRadius(context.Background(), repo, 1, []string{"specs/conventions.md"}, []string{"specs/conventions.md"})
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if len(br.InFlight) != 2 {
		t.Fatalf("InFlight = %+v, want both pinned in-flight issues (an ambient edit drifts all)", br.InFlight)
	}
	if len(br.Merged) != 1 || br.Merged[0].Epic != "e1" {
		t.Fatalf("Merged = %+v, want the e1 group re-derived by the ambient edit", br.Merged)
	}
}

// TestBlastRadiusEmptyEditNoWork proves an empty draft (no edited paths) assesses nothing — the
// preview reads as "nothing to assess yet" rather than scanning every issue.
func TestBlastRadiusEmptyEditNoWork(t *testing.T) {
	r := NewReader(&fakeIssues{all: []core.Issue{{ID: "h-1", Status: "in_progress", Spec: "specs/a.md", SpecHash: "x"}}}, &fakeArts{}, &fakeProv{})
	br, err := r.BlastRadius(context.Background(), t.TempDir(), 1, nil, nil)
	if err != nil {
		t.Fatalf("BlastRadius: %v", err)
	}
	if len(br.InFlight) != 0 || len(br.Merged) != 0 || len(br.EditedSpecs) != 0 {
		t.Errorf("empty edit produced a non-empty blast radius: %+v", br)
	}
}

func TestBlastRadiusListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.BlastRadius(context.Background(), t.TempDir(), 1, nil, []string{"specs/a.md"}); err == nil {
		t.Fatal("BlastRadius swallowed a ListAll error")
	}
}

// TestResolveContextAssemblesEscalation proves Resolve pre-load stitches the issue, the current
// spec slice, and the transcript reference — the escalation + spec slice + transcript the wizard
// opens with (T4.15).
func TestResolveContextAssemblesEscalation(t *testing.T) {
	repo := writeSpecs(t, map[string]string{"specs/a.md": "# Orders\nThe acceptance criteria are ambiguous.\n"})
	issues := &fakeIssues{all: []core.Issue{{
		ID: "h-1", Title: "stuck", Status: "blocked", Role: "implement",
		Spec: "specs/a.md", DeadLetterReason: "agent escalated: needs-spec-clarification",
		Transcript: "sha256:tx",
	}}}
	arts := &fakeArts{present: map[string]string{"sha256:tx": "the conversation"}}
	r := NewReader(issues, arts, &fakeProv{})

	rc, err := r.ResolveContext(context.Background(), repo, 1, "h-1")
	if err != nil {
		t.Fatalf("ResolveContext: %v", err)
	}
	if rc.Issue.ID != "h-1" || rc.Issue.DeadLetterReason == "" {
		t.Errorf("issue/reason not carried through: %+v", rc.Issue)
	}
	if rc.Spec != "specs/a.md" || rc.SpecSlice == "" {
		t.Errorf("spec slice not resolved: spec=%q slice=%q", rc.Spec, rc.SpecSlice)
	}
	if rc.TranscriptHash != "sha256:tx" || !rc.TranscriptAvailable {
		t.Errorf("transcript reference not resolved: hash=%q available=%v", rc.TranscriptHash, rc.TranscriptAvailable)
	}
}

// TestResolveContextDegrades proves the spec slice and transcript are best-effort: a missing spec
// file and a transcript not in the store degrade to empty/unavailable rather than failing the page.
func TestResolveContextDegrades(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{
		ID: "h-1", Status: "blocked", Spec: "specs/gone.md", Transcript: "sha256:missing",
	}}}
	r := NewReader(issues, &fakeArts{present: map[string]string{}}, &fakeProv{})

	rc, err := r.ResolveContext(context.Background(), t.TempDir(), 1, "h-1")
	if err != nil {
		t.Fatalf("ResolveContext must not fail on a missing slice/transcript: %v", err)
	}
	if rc.SpecSlice != "" {
		t.Errorf("SpecSlice = %q, want empty for an unresolvable spec", rc.SpecSlice)
	}
	if rc.TranscriptHash != "sha256:missing" || rc.TranscriptAvailable {
		t.Errorf("transcript = %q/%v, want the hash retained but marked unavailable", rc.TranscriptHash, rc.TranscriptAvailable)
	}
}

func TestResolveContextUnknownIssueErrors(t *testing.T) {
	r := NewReader(&fakeIssues{getErr: errors.New("no such issue")}, &fakeArts{}, &fakeProv{})
	if _, err := r.ResolveContext(context.Background(), t.TempDir(), 1, "nope"); err == nil {
		t.Fatal("ResolveContext must surface an unreadable issue")
	}
}

// TestDeadLettersCarriesReason proves the orchestrator's escalation classification rides through
// to the DLQ projection (T4.15) so the queue states the cause, not just spend/attempt.
func TestDeadLettersCarriesReason(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Status: "blocked", DeadLetterReason: "epic USD budget exhausted"},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	dl, err := r.DeadLetters(context.Background())
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(dl) != 1 || dl[0].Reason != "epic USD budget exhausted" {
		t.Errorf("dead letter reason = %+v, want the orchestrator's classification surfaced", dl)
	}
}
