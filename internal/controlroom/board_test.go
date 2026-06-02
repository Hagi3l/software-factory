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

func boardReader() *query.Reader {
	return query.NewReader(&fakeIssues{all: []core.Issue{
		{ID: "harness-1", Title: "Build the thing", Status: "in_progress", Role: "implementor", Attempt: 1, Spec: "specs/x.md", StateEnteredAt: cardEntered, CreatedAt: cardEntered.Add(-24 * time.Hour)},
		{ID: "harness-2", Title: "Write the tests", Status: "blocked", Role: "test-author", Attempt: 2},
		{ID: "harness-3", Title: "Plan the epic", Status: "closed", Role: "planner"},
	}}, fakeArts{}, fakeProv{})
}

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
		"planner", "test-author", "implementor", // role column headers
		"attempt 2",              // the retried card surfaces its generation
		`sse-connect="/events"`,  // wired to the T4.3 substrate
		`hx-get="/board/cards"`,  // live fragment refresh target
		`href="/static/app.css"`, // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board page missing %q", want)
		}
	}
	// Pipeline order: planner before test-author before implementor; security is empty so
	// it is skipped (the board reflects the data, no empty columns).
	if pos(r.body, "planner") > pos(r.body, "test-author") || pos(r.body, "test-author") > pos(r.body, "implementor") {
		t.Errorf("columns not in pipeline order: %q", colOrder(r.body))
	}
	if strings.Contains(r.body, ">security<") {
		t.Errorf("empty 'security' column should be skipped")
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

// TestBoardCardsLinkToDetail proves every card is a drill-through into the issue/invocation
// detail view (T4.7) — the board is triage, the detail page is where the brief and evidence
// are read.
func TestBoardCardsLinkToDetail(t *testing.T) {
	ts := boardServer(t)
	r := get(t, ts, "/board")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`href="/issue/harness-1"`,
		`href="/issue/harness-2"`,
		`href="/issue/harness-3"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("board card missing detail link %q", want)
		}
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
		"transition:true",                       // View Transitions opt-in for animated moves
		`id="card-harness-1"`,                    // stable identity per card
		"view-transition-name: card-harness-1",  // the pairing key the browser tweens on
		`x-data="cardTicker()"`,                  // the client-ticked timer
		`data-state-since="` + since + `"`,       // time-in-state anchor (StateEnteredAt)
		`data-created="` + created + `"`,         // total-time anchor (CreatedAt)
		"working",                                // status→label for the in_progress card
		`<script src="/static/ticker.js"`,        // the ticker is loaded
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
