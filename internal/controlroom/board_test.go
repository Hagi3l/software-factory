package controlroom

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/core"
)

// cardEntered is harness-1's fixed state-entry anchor; the timer test asserts it renders as a
// Unix-epoch data attribute the client-side ticker advances.
var cardEntered = time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)

// fakeIssues is a query.IssueReader backed by a fixed slice — enough to drive the board
// (which only reads ListAll) without a bd binary.
type fakeIssues struct{ all []core.Issue }

func (f *fakeIssues) ListAll(context.Context) ([]core.Issue, error) { return f.all, nil }
func (f *fakeIssues) List(_ context.Context, status string) ([]core.Issue, error) {
	var out []core.Issue
	for _, i := range f.all {
		if i.Status == status {
			out = append(out, i)
		}
	}
	return out, nil
}
func (f *fakeIssues) Get(_ context.Context, id string) (core.Issue, error) {
	for _, i := range f.all {
		if i.ID == id {
			return i, nil
		}
	}
	return core.Issue{}, io.EOF
}

// fakeArts / fakeProv satisfy the other two ports; the board does not touch them.
type fakeArts struct{}

func (fakeArts) Has(context.Context, string) (bool, error)          { return false, nil }
func (fakeArts) Get(context.Context, string) (io.ReadCloser, error) { return nil, io.EOF }

type fakeProv struct{}

func (fakeProv) ByIssue(context.Context, string) (core.Provenance, bool, error) {
	return core.Provenance{}, false, nil
}
func (fakeProv) DiffByIssue(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (fakeProv) Recent(context.Context, int) ([]query.MergedCommit, error) { return nil, nil }

// boardIssues is the shared board fixture: one in-flight, one blocked, one closed issue —
// reused by the board and live-invocation (T4.21) server tests.
func boardIssues() []core.Issue {
	return []core.Issue{
		{ID: "harness-1", Title: "Build the thing", Status: "in_progress", Role: "implementor", Attempt: 1, Spec: "specs/x.md", StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-24 * time.Hour)},
		{ID: "harness-2", Title: "Write the tests", Status: "blocked", Role: "test-author", Attempt: 2},
		{ID: "harness-3", Title: "Plan the epic", Status: "closed", Role: "planner", StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-2 * time.Hour)},
	}
}

func boardReader() *query.Reader {
	return query.NewReader(&fakeIssues{all: boardIssues()}, fakeArts{}, fakeProv{})
}

// mergedProv is a ProvenanceReader fake that reports an issue as merged with a transcript hash —
// the precondition the invocation view's Replay handoff is gated on (T4.21).
type mergedProv struct{ transcripts map[string]string }

func (m mergedProv) ByIssue(_ context.Context, id string) (core.Provenance, bool, error) {
	if h, ok := m.transcripts[id]; ok {
		return core.Provenance{Transcript: h}, true, nil
	}
	return core.Provenance{}, false, nil
}
func (mergedProv) DiffByIssue(context.Context, string) (string, bool, error) { return "", false, nil }
func (mergedProv) Recent(context.Context, int) ([]query.MergedCommit, error) { return nil, nil }

func boardServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := New(Options{
		Version:    "test",
		Reader:     boardReader(),
		StageOrder: []string{"planner", "test-author", "implementor", "security"},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestBoardRendersColumnsInPipelineOrder is T4.4's core contract: every issue appears as a
// card under its role column, columns read left-to-right in the supplied pipeline order,
// and the page wires itself to the SSE substrate for live refresh.
func TestBoardRendersColumnsInPipelineOrder(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Build the thing", "Write the tests", "Plan the epic", // every card
		"harness-1", "harness-2", "harness-3", // ids
		"planner", "test-author", "implementor", "security", // role column headers — every declared stage
		"attempt 2",              // the retried card surfaces its generation
		`sse-connect="/events"`,  // wired to the T4.3 substrate
		`hx-get="/board/cards"`,  // live fragment refresh target
		`href="/static/app.css"`, // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board page missing %q", want)
		}
	}
	// Pipeline order, in full: every declared stage renders left-to-right, including the
	// empty 'security' column (the board shows the whole pipeline, not just occupied stages).
	if pos(r.body, "planner") > pos(r.body, "test-author") ||
		pos(r.body, "test-author") > pos(r.body, "implementor") ||
		pos(r.body, "implementor") > pos(r.body, ">security<") {
		t.Errorf("columns not in pipeline order: %q", colOrder(r.body))
	}
	if !strings.Contains(r.body, ">security<") {
		t.Errorf("empty 'security' column should still render as a column")
	}
}

// TestBoardCardsFragmentIsBare proves the live fragment is the columns only — no <html>
// chrome — so an htmx swap replaces the columns in place rather than nesting a whole page.
func TestBoardCardsFragmentIsBare(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board/cards")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(strings.ToLower(r.body), "<!doctype") || strings.Contains(r.body, "<html") {
		t.Errorf("fragment must not include the page chrome: %q", r.body)
	}
	for _, want := range []string{"Build the thing", "implementor", "harness-1"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

// TestBoardWithoutReader covers the standalone path: with no read model the page renders a
// "not attached" notice (still 200, still in the chrome), and the data fragment 503s.
func TestBoardWithoutReader(t *testing.T) {
	ts := newTestServer(t) // built without Options.Reader
	page := get(t, ts, "/board")
	if page.status != http.StatusOK {
		t.Fatalf("/board status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Not attached") {
		t.Errorf("/board missing the not-attached notice: %q", page.body)
	}

	frag := get(t, ts, "/board/cards")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/board/cards status = %d, want 503", frag.status)
	}
}

// TestBoardCardsLinkToInvocation proves every card is a drill-through into the live invocation
// view (T4.21, control-room.md "drill from a board card (the agent currently working it)") — the
// board is triage, the invocation view is where a human watches that worker think (and it hands
// off to the forensic issue/Replay drill on termination).
func TestBoardCardsLinkToInvocation(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`href="/invocation/harness-1"`,
		`href="/invocation/harness-2"`,
		`href="/invocation/harness-3"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board card missing invocation link %q", want)
		}
	}
}

// TestBoardAutoScroll is T4.30's contract: the board marks its work frontier and wires the
// auto-scroll that follows it (control-room.md "Follow the frontier"). The shared fixture has a
// closed planner card, a blocked test-author card, and an in-flight implementor card, so the
// frontier is test-author (the leftmost column with incomplete work — blocked counts), not the
// all-closed planner column. The toggle (header) and the script must be present, and only the
// frontier column carries data-board-focus.
func TestBoardAutoScroll(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`<script src="/static/board-autoscroll.js"`, // the driver is loaded
		`id="board-autoscroll-toggle"`,              // the header toggle exists
		"data-board-focus",                          // a frontier column is marked
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board page missing %q", want)
		}
	}
	// Exactly one column is the frontier.
	if n := strings.Count(r.body, "data-board-focus"); n != 1 {
		t.Errorf("data-board-focus count = %d, want exactly 1", n)
	}
	// The frontier is the test-author column (blocked card), not the all-closed planner column.
	// The data-board-focus attribute lives in the test-author column's opening <div>, which sits
	// after the planner header and before the test-author header (its own header span).
	focus := pos(r.body, "data-board-focus")
	if focus < pos(r.body, ">planner<") || focus > pos(r.body, ">test-author<") {
		t.Errorf("frontier marker not on the test-author column (the leftmost incomplete stage)")
	}
}

// TestBoardInMotion is T4.18's contract: the board refreshes off the typed issue-state event
// (not the coarse agent-event), opts the swap into View Transitions, gives every card a stable
// id + view-transition-name so a moved card animates, and emits the two epoch anchors + the
// Alpine ticker the per-card timers tick from client-side. The activity feed (a separate view)
// keeps agent-event — not asserted here.
func TestBoardInMotion(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	since := strconv.FormatInt(cardEntered.Unix(), 10)
	created := strconv.FormatInt(cardEntered.Add(-24*time.Hour).Unix(), 10)
	for _, want := range []string{
		`hx-trigger="sse:issue-state throttle:2s, every 15s"`, // crisp refresh off the typed event
		"transition:true",                      // View Transitions opt-in for animated moves
		`id="card-harness-1"`,                  // stable identity per card
		"view-transition-name: card-harness-1", // the pairing key the browser tweens on
		`x-data="cardTicker()"`,                // the client-ticked timer
		`data-state-since="` + since + `"`,     // time-in-state anchor (StateEnteredAt)
		`data-created="` + created + `"`,       // total-time anchor (CreatedAt)
		"working",                              // status→label for the in_progress card
		`<script src="/static/ticker.js"`,      // the ticker is loaded
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board page missing %q", want)
		}
	}
	// The board's own live trigger must be the crisp issue-state event, not the coarse
	// agent-event firehose. Scope the check to the #board element: the layout's status bar
	// (T4.19) legitimately uses agent-event for its active-agents count, so a whole-page
	// substring check would now false-positive on the chrome.
	i := strings.Index(r.body, `id="board"`)
	if i < 0 {
		t.Fatalf("board page missing the #board element")
	}
	boardTag := r.body[i:]
	if j := strings.Index(boardTag, ">"); j >= 0 {
		boardTag = boardTag[:j]
	}
	if strings.Contains(boardTag, "sse:agent-event") {
		t.Errorf("board still wired to the coarse agent-event trigger")
	}
	if !strings.Contains(boardTag, "sse:issue-state") {
		t.Errorf("board not wired to the issue-state trigger")
	}
}

// TestClosedCardShowsStaticLeadTime is the contract for a finished card: a closed issue has no
// live clock (the work is done, so a ticking counter would only inflate meaninglessly). It
// renders a single static lead time — how long it took, StateEnteredAt − CreatedAt — and carries
// neither the Alpine ticker nor the timer anchors that would make it tick. The closed fixture
// (harness-3) was created 2h before it closed.
func TestClosedCardShowsStaticLeadTime(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board/cards")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	// Isolate the closed card so the live in_progress card's ticker/anchors don't false-pass
	// the negative assertions below.
	i := strings.Index(r.body, `id="card-harness-3"`)
	if i < 0 {
		t.Fatalf("closed card harness-3 not rendered")
	}
	card := r.body[i:]
	if j := strings.Index(card[len(`id="card-harness-3"`):], `id="card-harness-`); j >= 0 {
		card = card[:len(`id="card-harness-3"`)+j] // stop before the next card, if any follows
	}
	if !strings.Contains(card, "took") || !strings.Contains(card, "2h00m") {
		t.Errorf("closed card should show static lead time 'took 2h00m', got: %q", card)
	}
	if strings.Contains(card, "cardTicker()") {
		t.Errorf("closed card must not carry the live ticker")
	}
	if strings.Contains(card, "data-state-since") || strings.Contains(card, "data-created") {
		t.Errorf("closed card must not carry live timer anchors")
	}
}

// TestTickerAssetServed proves the ticker script the board references is actually embedded and
// served from /static — a dangling script tag would silently break the live timers.
func TestTickerAssetServed(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/static/ticker.js")
	if r.status != http.StatusOK {
		t.Fatalf("/static/ticker.js status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "cardTicker") {
		t.Errorf("/static/ticker.js missing the cardTicker factory: %q", r.body)
	}
}

// wallBoardServer builds a board with a budget.wall configured over three in-flight cards — one
// comfortably under, one near the cap, one over — so the rendered live timer tints toward the
// ceiling (control-room.md "The board, in motion"). All three are in_progress with the default
// attempt so the only source of a warn/blocked color in a card is the wall tint (the blocked
// status badge and the attempt>1 line, the other users of those tokens, are absent).
func wallBoardServer(t *testing.T) *httptest.Server {
	t.Helper()
	issues := []core.Issue{
		{ID: "wall-ok", Title: "Plenty of wall", Role: "implementor", Status: "in_progress", SpentWall: 10 * time.Minute, StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-20 * time.Minute)},
		{ID: "wall-warn", Title: "Almost out of wall", Role: "implementor", Status: "in_progress", SpentWall: 108 * time.Minute, StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-2 * time.Hour)},
		{ID: "wall-over", Title: "Over wall", Role: "implementor", Status: "in_progress", SpentWall: 3 * time.Hour, StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-3 * time.Hour)},
	}
	s := New(Options{
		Version:    "test",
		Reader:     query.NewReader(&fakeIssues{all: issues}, fakeArts{}, fakeProv{}),
		StageOrder: []string{"implementor"},
		BudgetCaps: query.BudgetCaps{IssueWall: 2 * time.Hour},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// isolateCard slices the rendered HTML for one card out of the board fragment, so an assertion
// about that card's timer tint does not false-pass on a sibling card.
func isolateCard(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="card-`+id+`"`)
	if start < 0 {
		t.Fatalf("card %q not rendered", id)
	}
	rest := body[start+len(`id="card-`+id+`"`):]
	if j := strings.Index(rest, `id="card-`); j >= 0 {
		return body[start : start+len(`id="card-`+id+`"`)+j]
	}
	return body[start:]
}

// TestBoardWallTimerTint is the render half of control-room.md's "the in-progress timer tints
// toward its budget.wall ceiling — a live 'about to breach' signal". A near-cap card's live timer
// goes amber, an over-cap card's goes rose, an under-cap card stays the default faint, and every
// capped card carries the wall-percent tooltip so the signal is carried by text too, not color
// alone. The tint reuses the Budgets view's color tokens, so the two surfaces read the same.
func TestBoardWallTimerTint(t *testing.T) {
	ts := wallBoardServer(t)
	r := get(t, ts, "/board/cards")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}

	warn := isolateCard(t, r.body, "wall-warn")
	if !strings.Contains(warn, "text-st-warn") {
		t.Errorf("near-cap card timer not amber-tinted: %q", warn)
	}
	if !strings.Contains(warn, `title="wall 90% of budget"`) {
		t.Errorf("near-cap card missing the wall tooltip: %q", warn)
	}

	over := isolateCard(t, r.body, "wall-over")
	if !strings.Contains(over, "text-st-blocked") {
		t.Errorf("over-cap card timer not rose-tinted: %q", over)
	}

	ok := isolateCard(t, r.body, "wall-ok")
	if strings.Contains(ok, "text-st-warn") || strings.Contains(ok, "text-st-blocked") {
		t.Errorf("under-cap card timer should stay faint (no tint): %q", ok)
	}
	if !strings.Contains(ok, `title="wall 8% of budget"`) {
		t.Errorf("under-cap (but capped) card missing the wall tooltip: %q", ok)
	}
}

// TestBoardWallNoTintWhenUncapped proves the default board (no budget.wall configured) renders no
// wall tint or tooltip at all — the uncapped-dimension behavior, so the board never invents a
// breach signal where no cap exists.
func TestBoardWallNoTintWhenUncapped(t *testing.T) {
	ts := boardServer(t) // no BudgetCaps
	r := get(t, ts, "/board/cards")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(r.body, "of budget") {
		t.Errorf("uncapped board rendered a wall tooltip: %q", r.body)
	}
}

func pos(s, sub string) int { return strings.Index(s, sub) }

// colOrder is a tiny debugging aid: the role headers in document order.
func colOrder(body string) []string {
	var out []string
	for _, role := range []string{"planner", "test-author", "implementor", "security"} {
		if strings.Contains(body, role) {
			out = append(out, role)
		}
	}
	return out
}

// epicBoardServer builds a board server configured for integration.mode: epic, over a one-feature
// fixture (root feat-1 + two children). It is the server-level (rendered-HTML) counterpart to the
// query-layer epic tests — proving the epic chrome (badge, hero block, progress, state) actually
// reaches the page when the factory lands work epic-atomically (T7.6).
func epicBoardServer(t *testing.T) *httptest.Server {
	t.Helper()
	issues := []core.Issue{
		// The epic root closed at decomposition (never integrated → excluded from progress); one
		// child integrated onto the epic branch (the durable marker, T8.3); one child is still in
		// flight. So the hero reads 1/2 — counting the marker, not the closed root.
		{ID: "feat-1", Title: "Add the vault", Role: "plan", Status: "closed"},
		{ID: "feat-1.1", Title: "API", Role: "implementor", Status: "closed", EpicID: "feat-1", Integrated: true},
		{ID: "feat-1.2", Title: "UI", Role: "test-author", Status: "in_progress", EpicID: "feat-1"},
	}
	s := New(Options{
		Version:    "test",
		Reader:     query.NewReader(&fakeIssues{all: issues}, fakeArts{}, fakeProv{}),
		StageOrder: []string{"plan", "test-author", "implementor"},
		Config:     &config.Config{Harness: &config.Harness{Integration: &config.Integration{Mode: config.IntegrationEpic}}},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestBoardEpicChromeRenders is T7.6's rendered-HTML contract under epic mode: every card shows the
// shared epic badge, and the epic root renders the hero block — the integrating state and the
// children-integrated/total progress (1 integrated of 2 real children — the closed-at-decomposition
// root is excluded, T8.3). The hue-hashed left-border tint rides the card's inline style.
func TestBoardEpicChromeRenders(t *testing.T) {
	ts := epicBoardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"epic feat-1",       // the shared epic badge text (the robust, non-color identifier)
		"border-left-color", // the hue-hashed tint on the card's inline style
		"integrating",       // the hero's feature state (nothing landed on main in the fake prov)
		"integrated",        // the hero's progress label
		"1/2",               // children integrated / total (feat-1.1 integrated; feat-1.2 in flight; root excluded)
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("epic board missing %q", want)
		}
	}
}

// TestBoardNoEpicChromeInPerItem proves the default per-item server renders no epic chrome — the
// shared fixture is three single-issue epics (no shared epic_id) and the mode is per-item, so
// neither a grouping badge nor a hero block appears. This guards against the epic chrome leaking
// onto a board of unrelated lone issues (the grouping gate is a real multi-issue fan-out).
func TestBoardNoEpicChromeInPerItem(t *testing.T) {
	ts := boardServer(t) // no Config → per-item; fixture is lone issues, no shared epic
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, unwanted := range []string{"epic ", "border-left-color", "integrating", "data-epic", "data-parent"} {
		if strings.Contains(r.body, unwanted) {
			t.Errorf("per-item board of lone issues unexpectedly contains epic chrome %q", unwanted)
		}
	}
}

// TestBoardLineageChromeRenders is T7.8's rendered-HTML contract: a multi-issue epic's cards carry
// the lineage data-attributes (data-epic for grouping, data-parent for the thread edge) and the
// single color source — the --epic custom property, read by the tint, the badge dot, and the
// client thread alike (board.go cardStyle). It also wires the lineage.js overlay. The fixture's
// children carry no candidate base, so each threads to the epic root (data-parent="feat-1").
func TestBoardLineageChromeRenders(t *testing.T) {
	ts := epicBoardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`data-epic="feat-1"`,   // the grouping key the overlay reads
		`data-parent="feat-1"`, // the lineage edge: a child threads to the epic root
		"--epic:",              // the one color source published on the card
		"var(--epic)",          // the tint reads the custom property, not a re-hash
		"data-board-scroll",    // the overlay's content-space anchor
		"/static/lineage.js",   // the thread overlay is loaded
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("lineage board missing %q", want)
		}
	}
}
