package query

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
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

	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa", "integrate"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if board.Total != 5 {
		t.Errorf("Total = %d, want 5", board.Total)
	}
	// Every declared stage is kept in order — plan/integrate render as empty columns rather
	// than vanishing; weird (undeclared, but present in data) is appended alphabetically;
	// unassigned last.
	var stages []string
	for _, c := range board.Columns {
		stages = append(stages, c.Stage)
	}
	if want := []string{"plan", "implement", "qa", "integrate", "weird", unassignedStage}; !reflect.DeepEqual(stages, want) {
		t.Errorf("column order = %v, want %v", stages, want)
	}
	// The empty declared stages carry zero cards.
	if got := len(board.Columns[0].Cards); got != 0 {
		t.Errorf("plan column cards = %d, want 0 (empty declared stage)", got)
	}
	// implement column cards are sorted by id.
	impl := board.Columns[1]
	if len(impl.Cards) != 2 || impl.Cards[0].ID != "h-1" || impl.Cards[1].ID != "h-3" {
		t.Errorf("implement cards = %+v, want [h-1 h-3] in order", impl.Cards)
	}
}

// TestBoardRendersFullPipelineWhenEmpty proves a board with no issues still renders the full
// declared pipeline as empty count-0 columns (the skeleton), so the operator reads the shape
// of the factory at rest — specs/control-room.md "Columns are the pipeline, not the data".
func TestBoardRendersFullPipelineWhenEmpty(t *testing.T) {
	r := NewReader(&fakeIssues{}, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa", "integrate"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if board.Total != 0 {
		t.Errorf("Total = %d, want 0", board.Total)
	}
	var stages []string
	for _, c := range board.Columns {
		stages = append(stages, c.Stage)
		if len(c.Cards) != 0 {
			t.Errorf("stage %q has %d cards, want 0", c.Stage, len(c.Cards))
		}
	}
	if want := []string{"plan", "implement", "qa", "integrate"}; !reflect.DeepEqual(stages, want) {
		t.Errorf("column order = %v, want %v", stages, want)
	}
}

// TestBoardCardCarriesTimerAnchors proves the board projection threads the two timer anchors
// (StateEnteredAt, CreatedAt) onto the card verbatim — the view emits them as epoch data-*
// attributes for the client-ticked per-card timers (T4.18).
func TestBoardCardCarriesTimerAnchors(t *testing.T) {
	entered := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Role: "implement", Status: "in_progress", StateEnteredAt: entered, CreatedAt: created},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"implement"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	card := board.Columns[0].Cards[0]
	if !card.StateEnteredAt.Equal(entered) {
		t.Errorf("card.StateEnteredAt = %v, want %v", card.StateEnteredAt, entered)
	}
	if !card.CreatedAt.Equal(created) {
		t.Errorf("card.CreatedAt = %v, want %v", card.CreatedAt, created)
	}
}

// TestBoardCardWallBudget is the data half of control-room.md's "the in-progress timer tints
// toward its budget.wall ceiling": Board stamps each card's cumulative wall burn (core.Issue.SpentWall,
// the cross-loop wall the orchestrator enforces) against the per-issue wall cap, using the same
// meterPct/meterOver as the Budgets view — so the board tint and the budget table can never disagree.
// A breach (over the cap) and a near-cap (≥ the warn line) both surface; the view tints off these.
func TestBoardCardWallBudget(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-ok", Role: "implement", Status: "in_progress", SpentWall: 30 * time.Minute},    // 25% of 2h
		{ID: "h-warn", Role: "implement", Status: "in_progress", SpentWall: 105 * time.Minute}, // 87% of 2h
		{ID: "h-over", Role: "implement", Status: "in_progress", SpentWall: 150 * time.Minute}, // 125% of 2h → over
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"implement"}, false, BudgetCaps{IssueWall: 2 * time.Hour})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	cards := map[string]IssueCard{}
	for _, c := range board.Columns[0].Cards {
		cards[c.ID] = c
	}
	if c := cards["h-ok"]; !c.WallCapped || c.WallOver || c.WallPct != 25 {
		t.Errorf("h-ok = capped %v over %v pct %d, want true/false/25", c.WallCapped, c.WallOver, c.WallPct)
	}
	if c := cards["h-warn"]; !c.WallCapped || c.WallOver || c.WallPct != 87 {
		t.Errorf("h-warn = capped %v over %v pct %d, want true/false/87", c.WallCapped, c.WallOver, c.WallPct)
	}
	if c := cards["h-over"]; !c.WallCapped || !c.WallOver {
		t.Errorf("h-over = capped %v over %v, want true/true", c.WallCapped, c.WallOver)
	}
}

// TestBoardCardWallUncapped proves an unconfigured budget.wall leaves the card untinted: a percent
// of no cap is meaningless, so WallCapped is false and the timer never tints — the same uncapped-
// dimension behavior the Budgets view has, kept consistent so the board never invents a breach.
func TestBoardCardWallUncapped(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Role: "implement", Status: "in_progress", SpentWall: 99 * time.Hour},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"implement"}, false, BudgetCaps{}) // no IssueWall
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	c := board.Columns[0].Cards[0]
	if c.WallCapped || c.WallOver || c.WallPct != 0 {
		t.Errorf("uncapped card tinted: capped %v over %v pct %d", c.WallCapped, c.WallOver, c.WallPct)
	}
}

// TestBoardFrontier exercises the work-frontier rule that drives the board's auto-scroll
// (control-room.md "Follow the frontier", T4.30): exactly one column is Focus, and it is the
// leftmost column holding any incomplete (non-closed) card — blocked counts, so a blocked card
// pulls focus — else the rightmost column when every card is closed. "Leftmost" is positional
// over the rendered columns, so it follows stageOrder.
func TestBoardFrontier(t *testing.T) {
	stageOrder := []string{"plan", "implement", "qa", "integrate"}
	focused := func(b Board) string {
		for _, c := range b.Columns {
			if c.Focus {
				return c.Stage
			}
		}
		return ""
	}
	count := func(b Board) int {
		n := 0
		for _, c := range b.Columns {
			if c.Focus {
				n++
			}
		}
		return n
	}

	cases := []struct {
		name   string
		issues []core.Issue
		want   string
	}{
		{
			name: "leftmost incomplete, skipping a fully-closed earlier column",
			issues: []core.Issue{
				{ID: "h-1", Role: "plan", Status: "closed"},    // plan all done
				{ID: "h-2", Role: "implement", Status: "open"}, // ← leftmost incomplete
				{ID: "h-3", Role: "qa", Status: "in_progress"},
			},
			want: "implement",
		},
		{
			name: "a blocked card counts as incomplete and pulls focus",
			issues: []core.Issue{
				{ID: "h-1", Role: "plan", Status: "closed"},
				{ID: "h-2", Role: "implement", Status: "blocked"}, // ← blocked is incomplete
				{ID: "h-3", Role: "qa", Status: "closed"},
			},
			want: "implement",
		},
		{
			name: "everything closed → rightmost column",
			issues: []core.Issue{
				{ID: "h-1", Role: "plan", Status: "closed"},
				{ID: "h-2", Role: "qa", Status: "closed"},
			},
			want: "integrate", // last declared column
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(&fakeIssues{all: tc.issues}, &fakeArts{}, &fakeProv{})
			board, err := r.Board(context.Background(), stageOrder, false, BudgetCaps{})
			if err != nil {
				t.Fatalf("Board: %v", err)
			}
			if n := count(board); n != 1 {
				t.Fatalf("focused columns = %d, want exactly 1", n)
			}
			if got := focused(board); got != tc.want {
				t.Errorf("frontier = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBoardFrontierEmpty proves a board with no columns has no focus (and does not panic).
func TestBoardFrontierEmpty(t *testing.T) {
	r := NewReader(&fakeIssues{}, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), nil, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, c := range board.Columns {
		if c.Focus {
			t.Errorf("empty board marked column %q as focus", c.Stage)
		}
	}
}

func TestBoardListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Board(context.Background(), nil, false, BudgetCaps{}); err == nil {
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

// --- Invocation (T4.21) ---

// An in-flight invocation: not terminal, no replay handoff, and a budget meter that matches the
// per-issue burn the Budgets view computes (same shared row builder).
func TestInvocationInFlight(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Title: "build it", Role: "implement", Status: "in_progress", Spec: "specs/x.md",
			SpentUSD: 8, ClosingUSD: 5, SpentTokens: 800, ClosingTokens: 500, Attempt: 2},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	inv, err := r.Invocation(context.Background(), "h-1", BudgetCaps{IssueUSD: 10, IssueTokens: 2000, MaxRetries: 3})
	if err != nil {
		t.Fatalf("Invocation: %v", err)
	}
	if inv.Terminal || inv.ReplayAvailable {
		t.Errorf("in-flight invocation Terminal=%v ReplayAvailable=%v, want both false", inv.Terminal, inv.ReplayAvailable)
	}
	if inv.Role != "implement" || inv.Spec != "specs/x.md" || inv.Title != "build it" {
		t.Errorf("header = %+v, want role/spec/title threaded", inv)
	}
	// Budget meter mirrors the Budgets view's per-issue burn ($13/1300 over the $10 cap → breach).
	if inv.Budget.USD != 13 || inv.Budget.Tokens != 1300 || !inv.Budget.USDOver {
		t.Errorf("budget = %+v, want $13/1300tok breached", inv.Budget)
	}
}

// A merged invocation with a retained transcript is terminal and offers the Replay handoff.
func TestInvocationTerminalMergedOffersReplay(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-9", Status: "closed", Role: "implement"}}}
	prov := &fakeProv{byIssue: map[string]core.Provenance{"h-9": {Transcript: "sha256:abc"}}}
	r := NewReader(issues, &fakeArts{}, prov)
	inv, err := r.Invocation(context.Background(), "h-9", BudgetCaps{})
	if err != nil {
		t.Fatalf("Invocation: %v", err)
	}
	if !inv.Terminal {
		t.Error("closed invocation Terminal=false, want true")
	}
	if !inv.ReplayAvailable {
		t.Error("merged-with-transcript ReplayAvailable=false, want true")
	}
}

// A blocked (dead-lettered) invocation with no transcript stamped yet is terminal but offers no
// Replay handoff — there is nothing to resolve, so the page points at issue detail instead.
func TestInvocationBlockedNoReplay(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-3", Status: "blocked", Role: "qa"}}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})
	inv, err := r.Invocation(context.Background(), "h-3", BudgetCaps{})
	if err != nil {
		t.Fatalf("Invocation: %v", err)
	}
	if !inv.Terminal || inv.ReplayAvailable {
		t.Errorf("blocked invocation Terminal=%v ReplayAvailable=%v, want true/false", inv.Terminal, inv.ReplayAvailable)
	}
}

// A blocked (dead-lettered) invocation whose transcript was stamped on the issue (every
// disposition) is terminal AND offers the Replay handoff — the most useful target, since the
// human is here to understand why the run escalated. The handoff surfaces without a merge trailer.
func TestInvocationBlockedWithStampOffersReplay(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-4", Status: "blocked", Role: "qa", Transcript: "sha256:stamped"}}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{}) // never merged
	inv, err := r.Invocation(context.Background(), "h-4", BudgetCaps{})
	if err != nil {
		t.Fatalf("Invocation: %v", err)
	}
	if !inv.Terminal || !inv.ReplayAvailable {
		t.Errorf("blocked-with-stamp Terminal=%v ReplayAvailable=%v, want true/true", inv.Terminal, inv.ReplayAvailable)
	}
}

func TestInvocationGetError(t *testing.T) {
	r := NewReader(&fakeIssues{getErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Invocation(context.Background(), "h-1", BudgetCaps{}); err == nil {
		t.Fatal("Invocation swallowed a Get error")
	}
}

// --- Epic chrome on the board (T7.6) ---

// findCard locates a card by id across all columns, or nil. The board scatters an epic's issues
// across stage columns, so epic assertions must look the card up by id rather than by position.
func findCard(b Board, id string) *IssueCard {
	for ci := range b.Columns {
		for i := range b.Columns[ci].Cards {
			if b.Columns[ci].Cards[i].ID == id {
				return &b.Columns[ci].Cards[i]
			}
		}
	}
	return nil
}

// epicBoardIssues is a single feature: a root seed (feat-1, no EpicID so it is its own epic) and
// two children that thread the root's id — one integrated (the durable marker, T8.3), one still in
// flight. The root closed at decomposition (closed but NOT integrated → excluded from progress).
// Marginal Closing* spend is stamped on every issue (root included) so the hero's aggregate matches
// what the Budgets view sums — spend aggregates over all, progress counts only the integrated marker.
func epicBoardIssues() []core.Issue {
	return []core.Issue{
		{ID: "feat-1", Title: "Add the vault", Role: "plan", Status: "closed", ClosingTokens: 100, ClosingUSD: 1.0},
		{ID: "feat-1.1", Title: "API", Role: "implement", Status: "closed", EpicID: "feat-1", Integrated: true, ClosingTokens: 50, ClosingUSD: 0.5},
		{ID: "feat-1.2", Title: "UI", Role: "qa", Status: "in_progress", EpicID: "feat-1", ClosingTokens: 0, ClosingUSD: 0},
	}
}

// TestBoardEpicModeBadgeAndHero is T7.6's core contract under integration.mode: epic — every card
// of a feature carries the shared epic identity (the badge/tint key), and only the epic *root*
// card is the hero, carrying the children-integrated/total progress and the aggregate spend vs the
// epic_budget cap. The feature is mid-flight (one child in_progress, nothing landed on main), so
// the hero state is integrating.
func TestBoardEpicModeBadgeAndHero(t *testing.T) {
	r := NewReader(&fakeIssues{all: epicBoardIssues()}, &fakeArts{}, &fakeProv{})
	caps := BudgetCaps{EpicTokens: 1000, EpicUSD: 10}
	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa"}, true, caps)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if !board.EpicMode {
		t.Error("Board.EpicMode = false, want true")
	}
	// Every issue of the epic shares the root's id as its epic identity.
	for _, id := range []string{"feat-1", "feat-1.1", "feat-1.2"} {
		c := findCard(board, id)
		if c == nil {
			t.Fatalf("card %s missing", id)
		}
		if c.EpicID != "feat-1" {
			t.Errorf("card %s EpicID = %q, want feat-1", id, c.EpicID)
		}
	}
	// Only the root carries the hero summary.
	root := findCard(board, "feat-1")
	if root.Epic == nil {
		t.Fatal("root card carries no Epic hero summary")
	}
	if child := findCard(board, "feat-1.1"); child.Epic != nil {
		t.Error("child card must not carry a hero summary")
	}
	e := root.Epic
	// One child carries the integrated marker, one is in flight; the closed root is excluded (it
	// closed at decomposition, never integrated). So progress is 1/2, never 2/3 from counting the
	// closed root (T8.3, specs/integration.md "Integrated vs. closed").
	if e.Integrated != 1 || e.Total != 2 {
		t.Errorf("progress = %d/%d, want 1/2", e.Integrated, e.Total)
	}
	if e.Tokens != 150 || e.USD != 1.5 {
		t.Errorf("spend = %d tok / $%.2f, want 150 / $1.50", e.Tokens, e.USD)
	}
	if e.TokenCap != 1000 || e.USDCap != 10 {
		t.Errorf("caps = %d / $%.2f, want 1000 / $10", e.TokenCap, e.USDCap)
	}
	if e.State != EpicStateIntegrating {
		t.Errorf("state = %q, want integrating (nothing landed on main yet)", e.State)
	}
}

// TestBoardEpicProgressExcludesRootAndSupersededBeads is T8.3's progress-accounting contract: the
// roll-up counts the durable `integrated` marker, never `closed`, and excludes the epic root and
// any closed-but-not-integrated bead (a superseded on_failure retry or an advanced intermediate
// stage). The fixture is one feature whose two children each spawned a superseded attempt before
// one integrated and one stayed in flight — so a naive closed-count would read 4/6, but the honest
// progress is 1/2 (specs/integration.md "Integrated vs. closed"). Spend
// still aggregates over EVERY bead (root and corpses burned tokens) so it matches the Budgets view.
func TestBoardEpicProgressExcludesRootAndSupersededBeads(t *testing.T) {
	issues := []core.Issue{
		// Root: closed at decomposition, never integrated → excluded from progress, counted in spend.
		{ID: "feat-1", Role: "plan", Status: "closed", ClosingTokens: 100},
		// Child A: a failed first attempt (closed, not integrated → superseded, excluded) and the
		// retry that integrated (closed + marker → counts as 1/1).
		{ID: "feat-1.a0", Role: "implement", Status: "closed", EpicID: "feat-1", ClosingTokens: 40},
		{ID: "feat-1.a1", Role: "qa", Status: "closed", EpicID: "feat-1", Integrated: true, ClosingTokens: 60},
		// Child B: an advanced intermediate stage (closed, not integrated → excluded) and its current
		// in-flight stage (counts toward total, not yet integrated).
		{ID: "feat-1.b0", Role: "test-author", Status: "closed", EpicID: "feat-1", ClosingTokens: 30},
		{ID: "feat-1.b1", Role: "implement", Status: "in_progress", EpicID: "feat-1", ClosingTokens: 0},
	}
	r := NewReader(&fakeIssues{all: issues}, &fakeArts{}, &fakeProv{})
	caps := BudgetCaps{EpicTokens: 1000, EpicUSD: 10}
	board, err := r.Board(context.Background(), []string{"plan", "test-author", "implement", "qa"}, true, caps)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	e := findCard(board, "feat-1").Epic
	if e == nil {
		t.Fatal("root card carries no Epic hero summary")
	}
	if e.Integrated != 1 || e.Total != 2 {
		t.Errorf("progress = %d/%d, want 1/2 (one integrated child, one in flight; root + superseded excluded)", e.Integrated, e.Total)
	}
	// Spend is the sum over every bead of the epic (100+40+60+30+0), unaffected by the count filter.
	if e.Tokens != 230 {
		t.Errorf("Tokens = %d, want 230 (spend aggregates over ALL beads, not the filtered set)", e.Tokens)
	}
}

// TestBoardEpicHeroDoneOnTerminalMerge proves the hero flips to done only when the epic's terminal
// merge has landed on main — the durable provenance signal, not mere subtree drain. The fake
// provenance reports the epic id (feat-1) as merged, so the hero reads done.
func TestBoardEpicHeroDoneOnTerminalMerge(t *testing.T) {
	prov := &fakeProv{byIssue: map[string]core.Provenance{"feat-1": {Issue: "feat-1"}}}
	r := NewReader(&fakeIssues{all: epicBoardIssues()}, &fakeArts{}, prov)
	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa"}, true, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	root := findCard(board, "feat-1")
	if root.Epic == nil {
		t.Fatal("root card carries no Epic hero summary")
	}
	if root.Epic.State != EpicStateDone {
		t.Errorf("state = %q, want done (terminal merge landed)", root.Epic.State)
	}
}

// TestBoardPerItemModeGroupingButNoHero proves T7.8's decouple: in per-item mode a *multi-issue*
// epic still carries the grouping chrome (the shared EpicID badge/tint key on every card) because
// that is pure observability, but no card carries the hero summary — the hero's integrating→done
// lifecycle is epic-mode-only. epicBoardIssues is a real 3-issue fan-out, so all three group.
func TestBoardPerItemModeGroupingButNoHero(t *testing.T) {
	r := NewReader(&fakeIssues{all: epicBoardIssues()}, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if board.EpicMode {
		t.Error("Board.EpicMode = true in per-item mode")
	}
	for _, id := range []string{"feat-1", "feat-1.1", "feat-1.2"} {
		c := findCard(board, id)
		if c.EpicID != "feat-1" {
			t.Errorf("card %s EpicID = %q, want feat-1 (grouping is decoupled from mode)", id, c.EpicID)
		}
		if c.Epic != nil {
			t.Errorf("card %s carries a hero summary in per-item mode (hero is epic-mode-only)", id)
		}
	}
}

// TestBoardSingleIssueEpicStaysBare proves the grouping chrome gates on a genuine multi-issue
// fan-out: a lone, directly-seeded issue (its own single-issue epic) gets neither badge nor
// thread, in either mode — there the chrome would only be noise (T7.8).
func TestBoardSingleIssueEpicStaysBare(t *testing.T) {
	issues := []core.Issue{
		{ID: "solo-1", Title: "A lone task", Role: "implement", Status: "in_progress"},
		{ID: "solo-2", Title: "Another lone task", Role: "plan", Status: "closed"},
	}
	for _, epicMode := range []bool{false, true} {
		r := NewReader(&fakeIssues{all: issues}, &fakeArts{}, &fakeProv{})
		board, err := r.Board(context.Background(), []string{"plan", "implement"}, epicMode, BudgetCaps{})
		if err != nil {
			t.Fatalf("Board(epicMode=%v): %v", epicMode, err)
		}
		for _, id := range []string{"solo-1", "solo-2"} {
			c := findCard(board, id)
			if c.EpicID != "" || c.ParentID != "" {
				t.Errorf("epicMode=%v: lone issue %s has grouping chrome (EpicID=%q, ParentID=%q)", epicMode, id, c.EpicID, c.ParentID)
			}
		}
	}
}

// TestBoardWaitsForSiblingEdges proves the board's dashed "waits-for" overlay data (T10.4): a card's
// WaitsFor carries the subset of its blocked-by edges (core.Issue.DependsOn) that target an
// intra-epic sibling, EXCLUDING its lineage producer (the parent-plan link, == ParentID) so a
// producer edge is never double-drawn, and excluding any blocker outside the epic.
func TestBoardWaitsForSiblingEdges(t *testing.T) {
	issues := []core.Issue{
		{ID: "feat-1", Title: "root", Role: "plan", Status: "closed"},
		// store-layer child: blocked-by the plan only. The plan is its ParentID, so it is excluded
		// — the store child has no waits-for edge.
		{ID: "store", Title: "store", Role: "implement", Status: "in_progress", EpicID: "feat-1", DependsOn: []string{"feat-1"}},
		// handler child: blocked-by the plan (parent link) AND the store sibling (ordering). Only the
		// store edge is a waits-for; the plan link is its ParentID and drops out.
		{ID: "handler", Title: "handler", Role: "implement", Status: "open", EpicID: "feat-1", DependsOn: []string{"feat-1", "store"}},
		// a blocker outside the epic must not surface as a sibling waits-for edge.
		{ID: "lonely", Title: "lonely", Role: "implement", Status: "open", EpicID: "feat-1", DependsOn: []string{"ext-99"}},
	}
	r := NewReader(&fakeIssues{all: issues}, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"plan", "implement"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	want := map[string][]string{"feat-1": nil, "store": nil, "handler": {"store"}, "lonely": nil}
	for id, ws := range want {
		c := findCard(board, id)
		if c == nil {
			t.Fatalf("card %s missing", id)
		}
		if !equalStrs(c.WaitsFor, ws) {
			t.Errorf("card %s WaitsFor = %v, want %v", id, c.WaitsFor, ws)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBoardLineageParentID proves the lineage thread's edge target (ParentID) is derived with no
// new beads data (T7.8): a card whose Base is a candidate branch threads to that producer's id; a
// top-level decomposition child (no candidate base) threads to the epic root; the root has none.
func TestBoardLineageParentID(t *testing.T) {
	issues := []core.Issue{
		{ID: "feat-1", Title: "root", Role: "plan", Status: "closed"},
		// A top-level decomposition child: no candidate base → threads to the epic root.
		{ID: "feat-1.1", Title: "child", Role: "implement", Status: "closed", EpicID: "feat-1"},
		// A produced next-stage issue: its Base names its predecessor's verified candidate.
		{ID: "feat-1.2", Title: "produced", Role: "qa", Status: "in_progress", EpicID: "feat-1", Base: core.CandidateBranch("feat-1.1")},
	}
	r := NewReader(&fakeIssues{all: issues}, &fakeArts{}, &fakeProv{})
	board, err := r.Board(context.Background(), []string{"plan", "implement", "qa"}, false, BudgetCaps{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	want := map[string]string{"feat-1": "", "feat-1.1": "feat-1", "feat-1.2": "feat-1.1"}
	for id, parent := range want {
		c := findCard(board, id)
		if c == nil {
			t.Fatalf("card %s missing", id)
		}
		if c.ParentID != parent {
			t.Errorf("card %s ParentID = %q, want %q", id, c.ParentID, parent)
		}
	}
}
