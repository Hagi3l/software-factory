package query

import (
	"context"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// fakeGraph is a fake WorkGraphSnapshot — the orchestrator's work-graph projection seam.
type fakeGraph struct {
	all []core.Issue
	err error
}

func (f *fakeGraph) Snapshot(_ context.Context) ([]core.Issue, error) { return f.all, f.err }

// TestProjectionIssueReader proves the projection-backed IssueReader (T8.4) serves the live
// work-state reads from the work-graph snapshot: ListAll returns the whole graph, List filters by
// status (including a comma-separated set), and Get finds an issue — falling back to the durable
// beads reader for an id the projection does not hold.
func TestProjectionIssueReader(t *testing.T) {
	ctx := context.Background()
	graph := &fakeGraph{all: []core.Issue{
		{ID: "a", Status: "open", Role: "implement"},
		{ID: "b", Status: "blocked", Role: "qa"},
		{ID: "c", Status: "in_progress", Role: "implement"},
	}}
	fallback := &fakeIssues{all: []core.Issue{{ID: "old", Status: "closed", Role: "qa"}}}
	r := NewProjectionIssueReader(graph, fallback)

	all, err := r.ListAll(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAll = (%d issues, %v), want 3 and no error", len(all), err)
	}

	blocked, err := r.List(ctx, "blocked")
	if err != nil || len(blocked) != 1 || blocked[0].ID != "b" {
		t.Fatalf("List(blocked) = (%v, %v), want [b]", blocked, err)
	}

	set, err := r.List(ctx, "open,in_progress")
	if err != nil || len(set) != 2 {
		t.Fatalf("List(open,in_progress) = (%d, %v), want 2", len(set), err)
	}

	hit, err := r.Get(ctx, "a")
	if err != nil || hit.ID != "a" {
		t.Fatalf("Get(a) = (%v, %v), want issue a", hit, err)
	}

	// A miss in the projection falls back to the durable beads reader (forensic deep-link).
	miss, err := r.Get(ctx, "old")
	if err != nil || miss.ID != "old" {
		t.Fatalf("Get(old) = (%v, %v), want fallback to beads issue old", miss, err)
	}

	// No fallback + miss is an explicit error, never a silent empty issue.
	bare := NewProjectionIssueReader(graph, nil)
	if _, err := bare.Get(ctx, "nope"); err == nil {
		t.Error("Get of an unknown id with no fallback should error, not return a zero issue")
	}
}

// TestReaderLiveVsForensicSplit proves NewReaderWithLive routes the LIVE work-state views (board,
// dead-letter, status) through the live reader and the FORENSIC pages (budgets) through the durable
// beads reader (T8.4, specs/observability.md "The live read model" vs specs/control-room.md
// "Historical/forensic"). The two readers hold disjoint issues, so a view reading the wrong source
// is unambiguous.
func TestReaderLiveVsForensicSplit(t *testing.T) {
	ctx := context.Background()
	live := &fakeIssues{all: []core.Issue{
		{ID: "live-open", Status: "open", Role: "implement", Title: "L"},
		{ID: "live-blocked", Status: "blocked", Role: "qa", Title: "stuck", DeadLetterReason: "needs spec"},
	}}
	forensic := &fakeIssues{all: []core.Issue{
		{ID: "for-1", Status: "open", Role: "implement", Title: "F", ClosingUSD: 5},
	}}
	r := NewReaderWithLive(live, forensic, &fakeArts{}, &fakeProv{})

	// Board reads the LIVE reader: it shows live-* and never the forensic-only issue.
	board, err := r.Board(ctx, []string{"implement", "qa"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	ids := map[string]bool{}
	for _, col := range board.Columns {
		for _, c := range col.Cards {
			ids[c.ID] = true
		}
	}
	if !ids["live-open"] || !ids["live-blocked"] {
		t.Errorf("Board missing live issues: %v", ids)
	}
	if ids["for-1"] {
		t.Error("Board read the forensic reader — it must read the live projection")
	}

	// Dead-letter reads the LIVE reader: the blocked issue with its reason.
	dls, err := r.DeadLetters(ctx)
	if err != nil || len(dls) != 1 || dls[0].ID != "live-blocked" || dls[0].Reason != "needs spec" {
		t.Fatalf("DeadLetters = (%v, %v), want the live blocked issue with its reason", dls, err)
	}

	// Status reads the LIVE reader: queue depth counts the one live open issue (live-blocked is an
	// escalation, for-1 is forensic-only and must not be seen here).
	st, err := r.Status(ctx, BudgetCaps{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.QueueDepth != 1 || st.OpenEscalations != 1 {
		t.Errorf("Status = {queue %d, escalations %d}, want {1, 1} from the live reader", st.QueueDepth, st.OpenEscalations)
	}

	// Budgets reads the FORENSIC reader: it aggregates the durable beads spend, not the projection.
	bud, err := r.Budgets(ctx, BudgetCaps{})
	if err != nil {
		t.Fatalf("Budgets: %v", err)
	}
	var sawForensic bool
	for _, row := range bud.Issues {
		if row.ID == "for-1" {
			sawForensic = true
		}
		if row.ID == "live-open" {
			t.Error("Budgets read the live reader — it must read the durable forensic reader")
		}
	}
	if !sawForensic {
		t.Error("Budgets did not read the forensic reader")
	}
}
