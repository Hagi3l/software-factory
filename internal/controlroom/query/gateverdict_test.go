package query

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestGateVerdictRejectedReadsRecordAndSouls proves a rejected (non-merged) candidate's verdict
// is reachable from the issue stamp — the verification view's main use, since a rejected
// candidate has no merge trailer — and that the producer souls come from the issue stamps.
func TestGateVerdictRejectedReadsRecordAndSouls(t *testing.T) {
	rec := core.GateVerdict{Passed: false, Checks: []core.GateCheckOutcome{
		{Name: "tests-red-then-green", Kind: core.GateCheckRedGreen, Passed: false, Base: &core.GateRunOutcome{ExitCode: 1}},
	}}
	issue := core.Issue{
		ID: "h-1", Role: "implementor", Status: "blocked",
		TestsSoul: "test-author-go", ImplementSoul: "implementor-go", GateVerdict: "sha256:v",
	}
	r := NewReader(
		&fakeIssues{all: []core.Issue{issue}},
		&fakeArts{present: map[string]string{"sha256:v": mustJSON(t, rec)}},
		&fakeProv{}, // not merged
	)

	v, err := r.GateVerdict(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if v.Merged {
		t.Error("Merged = true, want false (blocked, never merged)")
	}
	if !v.Available || v.Hash != "sha256:v" {
		t.Fatalf("Available/Hash = %v/%q, want true/sha256:v", v.Available, v.Hash)
	}
	if v.TestsSoul != "test-author-go" || v.ImplementSoul != "implementor-go" {
		t.Errorf("souls = %q/%q, want test-author-go/implementor-go (from the issue stamps)", v.TestsSoul, v.ImplementSoul)
	}
	if v.Verdict.Passed || len(v.Verdict.Checks) != 1 || v.Verdict.Checks[0].Base == nil || v.Verdict.Checks[0].Base.ExitCode != 1 {
		t.Errorf("verdict = %+v, want a failed red→green with base exit 1", v.Verdict)
	}
}

// TestGateVerdictMergedReadsIssueSouls proves a merged issue reports Merged=true and takes the
// producer souls from the issue's own threaded stamps — NOT the merge trailer's Soul, which on
// the shipped pipeline is the qa stage that produced the landed candidate, not the implementor.
func TestGateVerdictMergedReadsIssueSouls(t *testing.T) {
	rec := core.GateVerdict{Passed: true, Checks: []core.GateCheckOutcome{{Name: "tests-pass", Kind: core.GateCheckCommand, Passed: true}}}
	// A merged qa issue carrying the threaded producer souls (test author + implementor).
	issue := core.Issue{ID: "h-2", Role: "security", Status: "closed",
		TestsSoul: "test-author-go", ImplementSoul: "implementor-go", GateVerdict: "sha256:v2"}
	r := NewReader(
		&fakeIssues{all: []core.Issue{issue}},
		&fakeArts{present: map[string]string{"sha256:v2": mustJSON(t, rec)}},
		// The trailer's Soul is the qa/security soul that produced the landed candidate — it must
		// NOT override the threaded implementor stamp.
		&fakeProv{byIssue: map[string]core.Provenance{
			"h-2": {Issue: "h-2", Soul: "security-go", TestsSoul: "test-author-go"},
		}},
	)

	v, err := r.GateVerdict(context.Background(), "h-2")
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if !v.Merged {
		t.Fatal("Merged = false, want true")
	}
	if v.TestsSoul != "test-author-go" || v.ImplementSoul != "implementor-go" {
		t.Errorf("souls = %q/%q, want the threaded test-author-go/implementor-go (not the trailer's qa soul)", v.TestsSoul, v.ImplementSoul)
	}
	if !v.Available || !v.Verdict.Passed {
		t.Errorf("verdict = %+v (available=%v), want a passing record", v.Verdict, v.Available)
	}
}

// TestGateVerdictNoRecord proves an issue with no stamped verdict (its candidate has not been
// gated) yields Available=false with no hash — the view renders a notice rather than failing.
func TestGateVerdictNoRecord(t *testing.T) {
	issue := core.Issue{ID: "h-3", Role: "test-author", Status: "in_progress", TestsSoul: "test-author-go"}
	r := NewReader(&fakeIssues{all: []core.Issue{issue}}, &fakeArts{}, &fakeProv{})

	v, err := r.GateVerdict(context.Background(), "h-3")
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if v.Available || v.Hash != "" {
		t.Errorf("Available/Hash = %v/%q, want false/empty (no verdict stamped)", v.Available, v.Hash)
	}
	if v.TestsSoul != "test-author-go" {
		t.Errorf("TestsSoul = %q, want test-author-go (still readable from the issue)", v.TestsSoul)
	}
}

// TestGateVerdictUnresolvableRecord proves a stamped-but-unfetchable verdict degrades to
// Available=false while retaining the Hash (so the view can still offer the raw-bytes link),
// mirroring the replay/detail pages' best-effort posture — never a blank page.
func TestGateVerdictUnresolvableRecord(t *testing.T) {
	issue := core.Issue{ID: "h-4", Role: "implementor", Status: "blocked", GateVerdict: "sha256:gone"}
	r := NewReader(
		&fakeIssues{all: []core.Issue{issue}},
		&fakeArts{present: map[string]string{}}, // hash not present -> Get errors
		&fakeProv{},
	)

	v, err := r.GateVerdict(context.Background(), "h-4")
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if v.Available {
		t.Error("Available = true, want false (record unfetchable)")
	}
	if v.Hash != "sha256:gone" {
		t.Errorf("Hash = %q, want sha256:gone retained for the raw-bytes link", v.Hash)
	}
}

// TestGateVerdictIssueError proves an unreadable issue is fatal (the one hard dependency),
// unlike the best-effort store read.
func TestGateVerdictIssueError(t *testing.T) {
	r := NewReader(&fakeIssues{getErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.GateVerdict(context.Background(), "h-5"); err == nil {
		t.Fatal("GateVerdict returned nil error on an unreadable issue, want an error")
	}
}
