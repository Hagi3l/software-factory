package controlroom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/core"
)

// TestStatusBarFragment renders the live status-bar fragment off a read model: the bare fragment
// (no page chrome) carries the five glance fields and tints the escalation count when work is
// stuck. The escalation/queue derivation is the query layer's job (covered there); this asserts
// the served surface.
func TestStatusBarFragment(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Status: "open"},
		{ID: "h-2", Status: "in_progress"},
		{ID: "h-3", Status: "open"}, // queue depth = 3
		{ID: "h-4", Status: "blocked"},
		{ID: "h-5", Status: "blocked"}, // escalations = 2
	}}
	s := New(Options{Reader: query.NewReader(issues, nil, nil)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/status/bar")
	if r.status != http.StatusOK {
		t.Fatalf("/status/bar status = %d, want 200", r.status)
	}
	if strings.Contains(strings.ToLower(r.body), "<!doctype html>") {
		t.Errorf("/status/bar should be a bare fragment, not a full page: %s", r.body)
	}
	for _, want := range []string{"queue", "agents", "escalations", "budget", "last merge", ">3<"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/status/bar missing %q\nbody: %s", want, r.body)
		}
	}
	// Escalations > 0 is tinted rose so a stuck factory stands out.
	if !strings.Contains(r.body, "text-rose-300") {
		t.Errorf("/status/bar should tint a non-zero escalation count rose\nbody: %s", r.body)
	}
}

// TestStatusBarActiveAgents proves the "active agents" figure is filled from the in-memory
// activity buffer (not beads): two distinct agents seen recently render as 2.
func TestStatusBarActiveAgents(t *testing.T) {
	act := live.NewActivity(16)
	act.Record("inv-1", []byte(`{"type":"token","delta":"a"}`))
	act.Record("inv-2", []byte(`{"type":"token","delta":"b"}`))

	// No open/blocked issues, so the only "2" in the bar is the agent count.
	s := New(Options{Reader: query.NewReader(&fakeIssues{}, nil, nil), Activity: act})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/status/bar")
	if r.status != http.StatusOK {
		t.Fatalf("/status/bar status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, ">2<") {
		t.Errorf("/status/bar should show 2 active agents\nbody: %s", r.body)
	}
}

// TestStatusBarNoReader503 confirms the data endpoint degrades like the others: with no read model
// wired (standalone `harness serve`) it answers 503, so htmx leaves the layout's neutral
// placeholder bar in place — the spec's "degrades to a static bar".
func TestStatusBarNoReader503(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/status/bar")
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("/status/bar status = %d, want 503", r.status)
	}
}

// TestLayoutIncludesStatusBar confirms the status bar rides every page (it is in the layout
// chrome): the home page carries the SSE-connected shell that lazy-loads and live-refreshes the
// bar, including the dlq-arrival nudge, and references the escalation-notification script.
func TestLayoutIncludesStatusBar(t *testing.T) {
	ts := newTestServer(t)
	r := get(t, ts, "/")
	if r.status != http.StatusOK {
		t.Fatalf("/ status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`id="status-bar"`,
		`hx-get="/status/bar"`,
		"sse:dlq-arrival",
		"/static/alerts.js",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("layout missing status-bar wiring %q", want)
		}
	}
}
