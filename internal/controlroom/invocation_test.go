package controlroom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/controlroom/live"
	"github.com/Loxstomper/software-factory/internal/controlroom/query"
)

// invocationServer builds a server with the board reader (so issue headers + budget meters
// resolve) plus a live buffer pre-loaded with two invocations' events, so the scoped-feed
// filtering (T4.20's issue id on the buffer) is exercised.
func invocationServer(t *testing.T) *httptest.Server {
	t.Helper()
	act := live.NewActivity(16)
	act.Record("inv-a", "harness-1", "implementor", []byte(`{"type":"tool","delta":"run_tests"}`))
	act.Record("inv-b", "harness-3", "planner", []byte(`{"type":"token","delta":"other invocation"}`))
	s := New(Options{
		Version:    "test",
		Reader:     boardReader(),
		Activity:   act,
		StageOrder: []string{"planner", "test-author", "implementor", "security"},
		BudgetCaps: query.BudgetCaps{IssueUSD: 100, MaxRetries: 3},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestInvocationPageRendersHeaderMeterAndScopedFeed is T4.21's core contract: the header
// (id/role/title), the budget meter, the scoped feed (only this invocation's rows), and the
// SSE wiring — refetching the body fragment on issue-state AND agent-event nudges.
func TestInvocationPageRendersHeaderMeterAndScopedFeed(t *testing.T) {
	ts := invocationServer(t)
	r := get(t, ts, "/invocation/harness-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"harness-1", "implementor", "Build the thing", // header
		"Budget", "run_tests", // meter section + this invocation's activity row
		`hx-get="/invocation/harness-1/items"`, // live body fragment target
		"sse:issue-state",                      // crisp transition nudge
		"sse:agent-event",                      // per-turn progress nudge
		`href="/issue/harness-1"`,              // forensic drill-back in the header
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("invocation page missing %q", want)
		}
	}
	// The other invocation's activity must not leak into this scoped feed.
	if strings.Contains(r.body, "other invocation") {
		t.Errorf("scoped feed leaked another invocation's activity: %q", r.body)
	}
}

// TestInvocationBodyFragmentIsBare proves the live fragment is the body only (meter + feed) —
// no page chrome — so an htmx swap replaces it in place rather than nesting a whole page.
func TestInvocationBodyFragmentIsBare(t *testing.T) {
	ts := invocationServer(t)
	r := get(t, ts, "/invocation/harness-1/items")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(strings.ToLower(r.body), "<!doctype") || strings.Contains(r.body, "<html") {
		t.Errorf("fragment must not include the page chrome: %q", r.body)
	}
	for _, want := range []string{"Budget", "run_tests"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

// TestInvocationTerminalHandoff proves a closed, merged invocation stops claiming to be live and
// offers the Replay handoff — the bounded live-detail exception (control-room.md "Rendering").
// harness-3 is closed in the board fixture; a transcript on its provenance makes Replay resolve.
func TestInvocationTerminalHandoff(t *testing.T) {
	act := live.NewActivity(8)
	s := New(Options{
		Reader: query.NewReader(
			&fakeIssues{all: boardIssues()},
			fakeArts{},
			mergedProv{transcripts: map[string]string{"harness-3": "sha256:abc"}},
		),
		Activity: act,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/invocation/harness-3")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "has finished") {
		t.Errorf("terminal invocation missing the finished banner: %q", r.body)
	}
	if !strings.Contains(r.body, `href="/replay/harness-3"`) {
		t.Errorf("merged terminal invocation missing the Replay handoff: %q", r.body)
	}
}

// TestInvocationWithoutActivity covers the standalone path: with no live buffer the page renders
// the not-attached notice (200, in chrome) and the body fragment 503s.
func TestInvocationWithoutActivity(t *testing.T) {
	ts := newTestServer(t) // built without Options.Activity
	page := get(t, ts, "/invocation/harness-1")
	if page.status != http.StatusOK {
		t.Fatalf("/invocation status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Not attached") {
		t.Errorf("/invocation missing the not-attached notice: %q", page.body)
	}
	frag := get(t, ts, "/invocation/harness-1/items")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/invocation/{id}/items status = %d, want 503", frag.status)
	}
}
