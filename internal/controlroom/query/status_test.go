package query

import (
	"context"
	"errors"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// Status derives the layout bar from a single Budgets read: queue depth (issues neither
// terminal nor escalated), open escalations (blocked), the worst budget-health level, and the
// most recent merged issue. This exercises all four fields off one issue set.
func TestStatusCountsAndHealth(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "a", Status: "open"},        // in flight
		{ID: "b", Status: "in_progress"}, // in flight
		{ID: "c", Status: "closed"},      // terminal — not counted
		{ID: "d", Status: "blocked"},     // escalation
		{ID: "e", Status: "blocked"},     // escalation
	}}
	prov := &fakeProv{recent: []MergedCommit{{Commit: "abc", Provenance: core.Provenance{Issue: "c"}}}}
	r := NewReader(issues, &fakeArts{}, prov)

	st, err := r.Status(context.Background(), BudgetCaps{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.QueueDepth != 2 {
		t.Errorf("QueueDepth = %d, want 2 (open + in_progress)", st.QueueDepth)
	}
	if st.OpenEscalations != 2 {
		t.Errorf("OpenEscalations = %d, want 2 (blocked)", st.OpenEscalations)
	}
	if st.LastMergeIssue != "c" {
		t.Errorf("LastMergeIssue = %q, want c", st.LastMergeIssue)
	}
	// No caps configured → nothing can breach → healthy.
	if st.BudgetHealth != StatusHealthOK {
		t.Errorf("BudgetHealth = %q, want %q", st.BudgetHealth, StatusHealthOK)
	}
}

// A per-issue burn over a configured cap is a breach — the worst level wins, so the whole bar's
// dot goes rose even though other issues are fine.
func TestStatusBudgetBreach(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "a", Status: "in_progress", SpentUSD: 5, ClosingUSD: 0}, // 50% of $10 cap — fine
		{ID: "b", Status: "in_progress", SpentUSD: 12},               // over the $10 cap — breach
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	st, err := r.Status(context.Background(), BudgetCaps{IssueUSD: 10})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.BudgetHealth != StatusHealthBreach {
		t.Errorf("BudgetHealth = %q, want %q", st.BudgetHealth, StatusHealthBreach)
	}
}

// Burn at or above the warn band (80% of a cap) but not over it warns (amber) — between healthy
// and breached.
func TestStatusBudgetWarn(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "a", Status: "in_progress", SpentUSD: 9}, // 90% of $10 cap — warn
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	st, err := r.Status(context.Background(), BudgetCaps{IssueUSD: 10})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.BudgetHealth != StatusHealthWarn {
		t.Errorf("BudgetHealth = %q, want %q", st.BudgetHealth, StatusHealthWarn)
	}
}

// An epic-aggregate breach also drives the dot, not only per-issue burn.
func TestStatusEpicBreach(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "e1", Status: "closed", ClosingUSD: 6},
		{ID: "c1", Status: "closed", EpicID: "e1", ClosingUSD: 6}, // epic e1 sums to $12, over a $10 epic cap
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	st, err := r.Status(context.Background(), BudgetCaps{EpicUSD: 10})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.BudgetHealth != StatusHealthBreach {
		t.Errorf("BudgetHealth = %q, want %q (epic aggregate over cap)", st.BudgetHealth, StatusHealthBreach)
	}
}

// With no merged work yet, LastMergeIssue is empty rather than fabricated.
func TestStatusNoMergeYet(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "a", Status: "open"}}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	st, err := r.Status(context.Background(), BudgetCaps{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.LastMergeIssue != "" {
		t.Errorf("LastMergeIssue = %q, want empty", st.LastMergeIssue)
	}
}

// A ListAll fault surfaces as an error rather than a misleading all-zero bar.
func TestStatusListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Status(context.Background(), BudgetCaps{}); err == nil {
		t.Fatal("Status swallowed a ListAll error")
	}
}
