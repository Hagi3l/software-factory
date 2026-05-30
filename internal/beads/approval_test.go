package beads

import (
	"context"
	"strings"
	"testing"
)

// TestAwaitApprovalParksWithMetadata pins the write shape of parking an integrate candidate:
// the issue is blocked and durably carries the candidate ref and provenance so the approval
// can be bound and replayed (T2.10).
func TestAwaitApprovalParksWithMetadata(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.AwaitApproval(context.Background(), "harness-1", "candidate/harness-1", `{"Issue":"harness-1"}`); err != nil {
		t.Fatalf("AwaitApproval: %v", err)
	}
	got := strings.Join((*calls)[0], " ")
	if !strings.Contains(got, "update harness-1 --status blocked") {
		t.Errorf("args = %q, want status blocked", got)
	}
	if !strings.Contains(got, "--set-metadata candidate_ref=candidate/harness-1") {
		t.Errorf("args = %q, want candidate_ref metadata", got)
	}
	if !strings.Contains(got, "--set-metadata parked_prov=") {
		t.Errorf("args = %q, want parked_prov metadata", got)
	}
}

// An empty candidate ref is rejected: there is nothing to approve, so the park must not run.
func TestAwaitApprovalRejectsEmptyCandidate(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.AwaitApproval(context.Background(), "harness-1", "", "prov"); err == nil {
		t.Error("AwaitApproval accepted an empty candidate ref")
	}
	if len(*calls) != 0 {
		t.Errorf("AwaitApproval shelled out despite an empty candidate ref: %v", *calls)
	}
}

// A parked issue carrying no provenance still parks (a degraded record, never a dropped park).
func TestAwaitApprovalOmitsEmptyProvenance(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.AwaitApproval(context.Background(), "harness-1", "candidate/harness-1", ""); err != nil {
		t.Fatalf("AwaitApproval: %v", err)
	}
	if got := strings.Join((*calls)[0], " "); strings.Contains(got, "parked_prov=") {
		t.Errorf("args = %q, want no parked_prov when provenance is empty", got)
	}
}

// RecordApproval stamps who approved and which candidate sha, without changing status — the
// resume drives blocked→closed through the merge path, not here.
func TestRecordApproval(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.RecordApproval(context.Background(), "harness-1", "candidate/harness-1", "lochie"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	got := strings.Join((*calls)[0], " ")
	if strings.Contains(got, "--status") {
		t.Errorf("args = %q, want no status change (metadata-only)", got)
	}
	if !strings.Contains(got, "--set-metadata approved_ref=candidate/harness-1") || !strings.Contains(got, "--set-metadata approver=lochie") {
		t.Errorf("args = %q, want approved_ref + approver metadata", got)
	}
}

// The read path decodes the parking metadata back into core.Issue (the inverse of
// AwaitApproval), so the orchestrator's approval handler and the CLI see the parked state.
func TestGetDecodesApprovalState(t *testing.T) {
	c, _ := fakeClient(`[{"id":"harness-1","title":"t","status":"blocked","metadata":{"role":"security","candidate_ref":"candidate/harness-1","parked_prov":"{\"Issue\":\"harness-1\"}"}}]`, nil)
	got, err := c.Get(context.Background(), "harness-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CandidateRef != "candidate/harness-1" {
		t.Errorf("CandidateRef = %q, want candidate/harness-1", got.CandidateRef)
	}
	if got.ParkedProvenance != `{"Issue":"harness-1"}` {
		t.Errorf("ParkedProvenance = %q", got.ParkedProvenance)
	}
}
