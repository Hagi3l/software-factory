package query

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// --- fakes for the three ports ---

type fakeIssues struct {
	all     []core.Issue
	getErr  error
	listErr error
	allErr  error
}

func (f *fakeIssues) Get(_ context.Context, id string) (core.Issue, error) {
	if f.getErr != nil {
		return core.Issue{}, f.getErr
	}
	for _, i := range f.all {
		if i.ID == id {
			return i, nil
		}
	}
	return core.Issue{}, errors.New("not found")
}

func (f *fakeIssues) List(_ context.Context, status string) ([]core.Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []core.Issue
	for _, i := range f.all {
		if i.Status == status {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeIssues) ListAll(_ context.Context) ([]core.Issue, error) {
	if f.allErr != nil {
		return nil, f.allErr
	}
	return f.all, nil
}

type fakeArts struct {
	present map[string]string // hash -> content
}

func (f *fakeArts) Has(_ context.Context, hash string) (bool, error) {
	_, ok := f.present[hash]
	return ok, nil
}

func (f *fakeArts) Get(_ context.Context, hash string) (io.ReadCloser, error) {
	c, ok := f.present[hash]
	if !ok {
		return nil, errors.New("absent")
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

type fakeProv struct {
	byIssue map[string]core.Provenance
	diff    map[string]string
	recent  []MergedCommit
}

func (f *fakeProv) ByIssue(_ context.Context, id string) (core.Provenance, bool, error) {
	p, ok := f.byIssue[id]
	return p, ok, nil
}

func (f *fakeProv) DiffByIssue(_ context.Context, id string) (string, bool, error) {
	d, ok := f.diff[id]
	return d, ok, nil
}

func (f *fakeProv) Recent(_ context.Context, _ int) ([]MergedCommit, error) {
	return f.recent, nil
}

// --- Board ---

func TestBoardGroupsByStageInOrder(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-3", Role: "implement", Status: "in_progress"},
		{ID: "h-1", Role: "implement", Status: "open"},
		{ID: "h-2", Role: "qa", Status: "open"},
		{ID: "h-4", Role: "weird", Status: "open"}, // present but not in stageOrder
		{ID: "h-5", Role: "", Status: "open"},      // unassigned
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa", "integrate"})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if board.Total != 5 {
		t.Errorf("Total = %d, want 5", board.Total)
	}
	// plan/integrate are empty so are skipped; weird is appended alphabetically; unassigned last.
	var stages []string
	for _, c := range board.Columns {
		stages = append(stages, c.Stage)
	}
	if want := []string{"implement", "qa", "weird", unassignedStage}; !reflect.DeepEqual(stages, want) {
		t.Errorf("column order = %v, want %v", stages, want)
	}
	// implement column cards are sorted by id.
	impl := board.Columns[0]
	if len(impl.Cards) != 2 || impl.Cards[0].ID != "h-1" || impl.Cards[1].ID != "h-3" {
		t.Errorf("implement cards = %+v, want [h-1 h-3] in order", impl.Cards)
	}
}

func TestBoardListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Board(context.Background(), nil); err == nil {
		t.Fatal("Board swallowed a ListAll error")
	}
}

// --- DeadLetters ---

func TestDeadLettersReturnsBlocked(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-2", Status: "blocked", Role: "implement"},
		{ID: "h-1", Status: "blocked", Role: "qa"},
		{ID: "h-3", Status: "open"},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	dl, err := r.DeadLetters(context.Background())
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(dl) != 2 || dl[0].ID != "h-1" || dl[1].ID != "h-2" {
		t.Errorf("dead letters = %+v, want blocked issues h-1,h-2 sorted", dl)
	}
}

// TestDeadLettersCarriesTriageFields proves the DLQ projection surfaces the spend and
// retry generation a human triages on — a budget breach and an exhausted retry cap are the
// two non-escalation dead-letter causes, so spend/attempt must ride through verbatim from
// the issue, not be dropped to the bare card.
func TestDeadLettersCarriesTriageFields(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Title: "burned budget", Status: "blocked", Role: "implement",
			Spec: "specs/orders.md", Attempt: 3, SpentTokens: 120_000, SpentUSD: 1.2345},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	dl, err := r.DeadLetters(context.Background())
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	want := []DeadLetter{{
		ID: "h-1", Title: "burned budget", Role: "implement", Spec: "specs/orders.md",
		Attempt: 3, SpentTokens: 120_000, SpentUSD: 1.2345,
	}}
	if !reflect.DeepEqual(dl, want) {
		t.Errorf("dead letter =\n%+v\nwant\n%+v", dl, want)
	}
}

func TestDeadLettersListError(t *testing.T) {
	r := NewReader(&fakeIssues{listErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.DeadLetters(context.Background()); err == nil {
		t.Fatal("DeadLetters swallowed a List error")
	}
}

// --- IssueDetail ---

func TestIssueDetailMergedStitchesEvidence(t *testing.T) {
	issue := core.Issue{ID: "h-1", Title: "do it", Status: "closed", Role: "implement"}
	prov := core.Provenance{
		Soul: "implementor-go", Model: "claude", Issue: "h-1",
		PromptSHA:    "sha256:prompt",
		Verified:     []string{"build@sha256:bb", "gosec"}, // gosec degraded: bare name, no hash
		Traceability: "sha256:trace",
		Transcript:   "sha256:tx",
	}
	arts := &fakeArts{present: map[string]string{
		"sha256:prompt": "the prompt",
		"sha256:tx":     "the conversation",
		"sha256:trace":  "the map",
		"sha256:bb":     "build output",
		// no entry for the gosec citation (it has no hash anyway)
	}}
	r := NewReader(&fakeIssues{all: []core.Issue{issue}}, arts, &fakeProv{
		byIssue: map[string]core.Provenance{"h-1": prov},
		diff:    map[string]string{"h-1": "diff --git a/x b/x\n+added"},
	})

	d, err := r.IssueDetail(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("IssueDetail: %v", err)
	}
	if !d.Merged {
		t.Error("Merged = false, want true")
	}
	// The transcript (the replayable decision trail) is cited right after the prompt, so a
	// human drilling in can reach the full conversation, not just the opening prompt.
	want := []ArtifactLink{
		{Label: "Prompt", Kind: core.ArtifactKindPrompt, Hash: "sha256:prompt", Available: true},
		{Label: "Transcript", Kind: core.ArtifactKindTranscript, Hash: "sha256:tx", Available: true},
		{Label: "Traceability", Kind: core.ArtifactKindTraceabilityMap, Hash: "sha256:trace", Available: true},
		{Label: "build", Kind: core.ArtifactKindGateEvidence, Hash: "sha256:bb", Available: true},
		{Label: "gosec", Kind: core.ArtifactKindGateEvidence, Hash: "", Available: false},
	}
	if !reflect.DeepEqual(d.Evidence, want) {
		t.Errorf("evidence =\n%+v\nwant\n%+v", d.Evidence, want)
	}
	// The candidate diff that landed is stitched in for a merged issue.
	if d.Diff != "diff --git a/x b/x\n+added" {
		t.Errorf("diff = %q, want the landed candidate diff", d.Diff)
	}
}

func TestIssueDetailUnmergedUsesIssueTraceMap(t *testing.T) {
	issue := core.Issue{ID: "h-9", Status: "in_progress", Role: "qa", TraceMap: "sha256:tm"}
	arts := &fakeArts{present: map[string]string{"sha256:tm": "map"}}
	r := NewReader(&fakeIssues{all: []core.Issue{issue}}, arts, &fakeProv{}) // fakeProv has no entry -> not merged

	d, err := r.IssueDetail(context.Background(), "h-9")
	if err != nil {
		t.Fatalf("IssueDetail: %v", err)
	}
	if d.Merged {
		t.Error("Merged = true, want false (no provenance)")
	}
	if len(d.Evidence) != 1 || d.Evidence[0].Label != "Traceability" || !d.Evidence[0].Available {
		t.Errorf("evidence = %+v, want a single available Traceability link", d.Evidence)
	}
}

func TestIssueDetailGetError(t *testing.T) {
	r := NewReader(&fakeIssues{getErr: errors.New("boom")}, &fakeArts{}, &fakeProv{})
	if _, err := r.IssueDetail(context.Background(), "h-1"); err == nil {
		t.Fatal("IssueDetail swallowed a Get error")
	}
}

// --- Artifact + RecentProvenance passthroughs ---

func TestArtifactStreamsContent(t *testing.T) {
	arts := &fakeArts{present: map[string]string{"sha256:x": "hello evidence"}}
	r := NewReader(&fakeIssues{}, arts, &fakeProv{})
	rc, err := r.Artifact(context.Background(), "sha256:x")
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello evidence" {
		t.Errorf("content = %q", b)
	}
}

func TestRecentProvenancePassesThrough(t *testing.T) {
	want := []MergedCommit{{Commit: "abc", Provenance: core.Provenance{Issue: "h-1"}}}
	r := NewReader(&fakeIssues{}, &fakeArts{}, &fakeProv{recent: want})
	got, err := r.RecentProvenance(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentProvenance: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- Budgets ---

// Epic burn aggregates each member's OWN marginal spend (ClosingTokens/ClosingUSD) under its
// epic — the same sum the orchestrator enforces against — with the root folding into its own
// epic via core.EpicOf. Summing the marginal (never the chain-cumulative Spent*) is what keeps
// a fan-out from double-counting shared ancestry.
func TestBudgetsAggregatesEpicMarginalSpend(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		// Root seed e1 (no EpicID → its own epic) + two descendants carrying EpicID e1.
		{ID: "e1", ClosingTokens: 100, ClosingUSD: 1.0},
		{ID: "c1", EpicID: "e1", ClosingTokens: 200, ClosingUSD: 2.0},
		{ID: "c2", EpicID: "e1", ClosingTokens: 50, ClosingUSD: 0.5},
		// A separate root with its own marginal.
		{ID: "e2", ClosingTokens: 10, ClosingUSD: 0.1},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	b, err := r.Budgets(context.Background(), BudgetCaps{EpicTokens: 1000, EpicUSD: 5})
	if err != nil {
		t.Fatalf("Budgets: %v", err)
	}
	if len(b.Epics) != 2 {
		t.Fatalf("epics = %d, want 2", len(b.Epics))
	}
	// Ordered USD desc, so e1 (3.5) comes before e2 (0.1).
	e1 := b.Epics[0]
	if e1.EpicID != "e1" || e1.Issues != 3 || e1.Tokens != 350 || e1.USD != 3.5 {
		t.Errorf("epic e1 = %+v, want {e1, 3 issues, 350 tok, $3.5}", e1)
	}
	// 350/1000 = 35%, under cap so not over.
	if e1.TokenPct != 35 || e1.TokenOver {
		t.Errorf("e1 token meter = %d%% over=%v, want 35%% not-over", e1.TokenPct, e1.TokenOver)
	}
}

// A breach is flagged and the meter clamps to 100%. The per-issue burn is the chain-cumulative
// Spent* plus this attempt's marginal Closing* — the quantity the per-issue budget bounds.
func TestBudgetsPerIssueBurnAndBreach(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Role: "implement", Status: "in_progress",
			SpentUSD: 8, ClosingUSD: 5, SpentTokens: 800, ClosingTokens: 500, Attempt: 3},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	b, err := r.Budgets(context.Background(), BudgetCaps{IssueUSD: 10, IssueTokens: 2000, MaxRetries: 3})
	if err != nil {
		t.Fatalf("Budgets: %v", err)
	}
	row := b.Issues[0]
	if row.USD != 13 || row.Tokens != 1300 {
		t.Errorf("burn = $%.0f / %d tok, want $13 / 1300 (Spent+Closing)", row.USD, row.Tokens)
	}
	// $13 over the $10 cap → breach, meter clamped to 100%.
	if !row.USDOver || row.USDPct != 100 {
		t.Errorf("usd meter = %d%% over=%v, want 100%% breached", row.USDPct, row.USDOver)
	}
	// Attempt 3 at MaxRetries 3 → retry cap exhausted.
	if !row.RetryOver {
		t.Error("RetryOver = false, want true (attempt == max)")
	}
	// Tokens 1300/2000 = 65%, under cap.
	if row.TokenOver || row.TokenPct != 65 {
		t.Errorf("token meter = %d%% over=%v, want 65%% not-over", row.TokenPct, row.TokenOver)
	}
}

// An uncapped dimension (zero cap) is never a breach and reports a zero fill — the view shows
// it as ∞, not a misleading 0% bar.
func TestBudgetsUncappedDimensionNeverBreaches(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-1", ClosingUSD: 999, SpentUSD: 999}}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	b, err := r.Budgets(context.Background(), BudgetCaps{}) // all caps zero
	if err != nil {
		t.Fatalf("Budgets: %v", err)
	}
	row := b.Issues[0]
	if row.USDOver || row.USDPct != 0 {
		t.Errorf("uncapped usd meter = %d%% over=%v, want 0%% not-over", row.USDPct, row.USDOver)
	}
}

func TestBudgetsListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Budgets(context.Background(), BudgetCaps{}); err == nil {
		t.Fatal("Budgets swallowed a ListAll error")
	}
}
