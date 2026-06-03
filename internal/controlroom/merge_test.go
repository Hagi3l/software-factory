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

// mergeServer fronts a Server whose read model holds the candidates referenced by a live
// merge-queue buffer pre-loaded with one row per merge step, so the merge handler is exercised
// against a representative train: an in-flight rebase, a landed row (with a commit), and a
// terminal conflict (the interesting failure row). The fakes live in board_test.go.
func mergeServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := query.NewReader(&fakeIssues{all: []core.Issue{
		{ID: "harness-1", Title: "land me", Role: "integrate", Spec: "specs/a.md"},
		{ID: "harness-2", Title: "rebasing now", Role: "integrate", Spec: "specs/b.md"},
		{ID: "harness-3", Title: "broke on merge", Role: "integrate", Spec: "specs/c.md"},
	}}, fakeArts{}, fakeProv{})
	mq := live.NewMergeQueue(10)
	mq.Record(core.MergeStateEvent{ID: "harness-1", State: core.MergeStateLanded, Role: "integrate", Commit: "abc123def456"})
	mq.Record(core.MergeStateEvent{ID: "harness-2", State: core.MergeStateRebasing, Role: "integrate"})
	mq.Record(core.MergeStateEvent{ID: "harness-3", State: core.MergeStateConflicted, Role: "integrate"})
	s := New(Options{Version: "test", Reader: r, MergeQueue: mq})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestMergeRendersTrain is T4.25's core contract: each integrate candidate appears with its
// current merge step, a landed row links onward to Provenance with its commit, a terminal
// failure links to the issue for its dead-letter/fix routing, and the page wires itself to the
// merge-state SSE event for live refresh.
func TestMergeRendersTrain(t *testing.T) {
	ts := mergeServer(t)
	r := get(t, ts, "/merge")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"land me", "rebasing now", "broke on merge", // titles enriched from beads
		"harness-1", "harness-2", "harness-3", // ids
		"landed", "rebasing", "conflicted", // the per-candidate steps
		"abc123def456"[:12],       // the landed commit, short-hashed
		`href="/provenance"`,      // landed links onward to provenance
		`href="/issue/harness-3"`, // the failed candidate links to its issue (dead-letter/fix)
		"Dead-letter / fix →",     // the failure correlation link
		`sse-connect="/events"`,   // wired to the T4.3 substrate
		`hx-get="/merge/items"`,   // live fragment refresh target
		`sse:merge-state`,         // crisp refresh off the typed merge-state event (T4.24/T4.25)
		`href="/static/app.css"`,  // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("merge page missing %q", want)
		}
	}
}

// TestMergeItemsFragmentIsBare proves the live fragment is the list only — no <html> chrome — so
// an htmx swap replaces the train in place rather than nesting a whole page.
func TestMergeItemsFragmentIsBare(t *testing.T) {
	ts := mergeServer(t)
	r := get(t, ts, "/merge/items")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(strings.ToLower(r.body), "<!doctype") || strings.Contains(r.body, "<html") {
		t.Errorf("fragment must not include the page chrome: %q", r.body)
	}
	for _, want := range []string{"land me", "landed", `href="/issue/harness-1"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
}

// TestMergeEmptyIsCalm proves an empty train renders a calm "empty" notice inside the chrome,
// not an error — the ordinary idle state when nothing is integrating.
func TestMergeEmptyIsCalm(t *testing.T) {
	r := query.NewReader(&fakeIssues{}, fakeArts{}, fakeProv{})
	s := New(Options{Reader: r, MergeQueue: live.NewMergeQueue(10)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	page := get(t, ts, "/merge")
	if page.status != http.StatusOK {
		t.Fatalf("/merge status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "merge train is empty") {
		t.Errorf("/merge empty state missing the calm notice: %q", page.body)
	}
}

// TestMergeWithoutBuffer covers the standalone path: with no live merge-state buffer (a
// standalone `harness serve` has no NATS feed) the page renders a "not attached" notice (still
// 200, still in the chrome), and the data fragment 503s.
func TestMergeWithoutBuffer(t *testing.T) {
	ts := newTestServer(t) // built without Options.MergeQueue / Reader
	page := get(t, ts, "/merge")
	if page.status != http.StatusOK {
		t.Fatalf("/merge status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Not attached") {
		t.Errorf("/merge missing the not-attached notice: %q", page.body)
	}

	frag := get(t, ts, "/merge/items")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/merge/items status = %d, want 503", frag.status)
	}
}
