package controlroom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/controlroom/query"
	"github.com/Loxstomper/software-factory/internal/core"
)

// dlqServer fronts a Server whose read model holds a mix of blocked and non-blocked issues,
// so the DLQ handler is exercised against data that must be filtered down to the escalations
// (the fakes live in board_test.go).
func dlqServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := query.NewReader(&fakeIssues{all: []core.Issue{
		{ID: "harness-7", Title: "Cannot satisfy spec", Status: "blocked", Role: "implementor",
			Spec: "specs/orders.md", Attempt: 3, SpentTokens: 120_000, SpentUSD: 1.2345},
		{ID: "harness-2", Title: "Ambiguous acceptance", Status: "blocked", Role: "test-author", Attempt: 1},
		{ID: "harness-1", Title: "In flight", Status: "in_progress", Role: "implementor"}, // not a dead letter
	}}, fakeArts{}, fakeProv{})
	s := New(Options{Version: "test", Reader: r})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestDLQRendersEscalations is T4.8's core contract: only blocked issues appear, each with
// the triage signals a human acts on (spend, attempt, spec), each a drill-through into the
// detail view, and the page wires itself to the SSE substrate for live refresh.
func TestDLQRendersEscalations(t *testing.T) {
	ts := dlqServer(t)
	r := get(t, ts, "/dlq")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Cannot satisfy spec", "Ambiguous acceptance", // both blocked issues
		"harness-7", "harness-2", // their ids
		"specs/orders.md",                // the spec to refine
		"attempt 3",                      // the retry generation
		"120000 tokens",                  // the budget burn surfaced at a glance
		"$1.2345",                        // priced spend
		`href="/issue/harness-7"`,        // drill-through into the detail view
		`href="/verification/harness-7"`, // T4.23 — verification drill for triage
		`sse-connect="/events"`,          // wired to the T4.3 substrate
		`hx-get="/dlq/items"`,            // live fragment refresh target
		`sse:issue-state`,                // crisp refresh off the typed event (T4.18)
		`href="/static/app.css"`,         // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dlq page missing %q", want)
		}
	}
	// The in-flight issue is not an escalation and must not appear.
	if strings.Contains(r.body, "In flight") || strings.Contains(r.body, "harness-1") {
		t.Errorf("dlq must list only blocked issues, leaked an in-flight one: %q", r.body)
	}
}

// TestDLQItemsFragmentIsBare proves the live fragment is the list only — no <html> chrome —
// so an htmx swap replaces the list in place rather than nesting a whole page.
func TestDLQItemsFragmentIsBare(t *testing.T) {
	ts := dlqServer(t)
	r := get(t, ts, "/dlq/items")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(strings.ToLower(r.body), "<!doctype") || strings.Contains(r.body, "<html") {
		t.Errorf("fragment must not include the page chrome: %q", r.body)
	}
	for _, want := range []string{"Cannot satisfy spec", `href="/issue/harness-7"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

// TestDLQEmptyIsReassurance proves an empty queue is the *good* state: it renders a calm
// "nothing needs a human" notice inside the chrome, not an error.
func TestDLQEmptyIsReassurance(t *testing.T) {
	r := query.NewReader(&fakeIssues{all: []core.Issue{
		{ID: "harness-1", Status: "in_progress"},
		{ID: "harness-2", Status: "closed"},
	}}, fakeArts{}, fakeProv{})
	s := New(Options{Reader: r})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	page := get(t, ts, "/dlq")
	if page.status != http.StatusOK {
		t.Fatalf("/dlq status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Nothing needs a human") {
		t.Errorf("/dlq empty state missing reassurance notice: %q", page.body)
	}
}

// TestDLQWithoutReader covers the standalone path: with no read model the page renders a
// "not attached" notice (still 200, still in the chrome), and the data fragment 503s.
func TestDLQWithoutReader(t *testing.T) {
	ts := newTestServer(t) // built without Options.Reader
	page := get(t, ts, "/dlq")
	if page.status != http.StatusOK {
		t.Fatalf("/dlq status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Not attached") {
		t.Errorf("/dlq missing the not-attached notice: %q", page.body)
	}

	frag := get(t, ts, "/dlq/items")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/dlq/items status = %d, want 503", frag.status)
	}
}
