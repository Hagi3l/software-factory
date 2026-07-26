package controlroom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/core"
)

// recentProv is a query.ProvenanceReader that returns a fixed merged-commit history, so the
// provenance view can be exercised without a git repo. (board_test's fakeProv returns none.)
type recentProv struct{ commits []query.MergedCommit }

func (recentProv) ByIssue(context.Context, string) (core.Provenance, bool, error) {
	return core.Provenance{}, false, nil
}
func (recentProv) DiffByIssue(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (p recentProv) Recent(context.Context, int) ([]query.MergedCommit, error) {
	return p.commits, nil
}

// TestBudgetsNotAttached: with no read model the page is a notice inside the chrome (200) and
// the fragment endpoint answers 503 — never a blank page or a hang.
func TestBudgetsNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/budgets")
	if r.status != http.StatusOK {
		t.Fatalf("/budgets status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("/budgets missing not-attached notice, got: %s", r.body)
	}
	if !strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("/budgets not wrapped in the base layout")
	}
	if frag := get(t, ts, "/budgets/items"); frag.status != http.StatusServiceUnavailable {
		t.Errorf("/budgets/items status = %d, want 503", frag.status)
	}
}

// TestBudgetsRendersTables proves the wired view renders epic + issue burn against the caps:
// the page carries the issue id, the percent meter, and a breach, and the fragment is bare
// (no page chrome) for the htmx swap.
func TestBudgetsRendersTables(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		// One epic with a descendant; the issue is over its per-issue USD cap (a breach).
		{ID: "e1", Role: "planner", Status: "closed", ClosingTokens: 100, ClosingUSD: 1},
		{ID: "h-9", Role: "implementor", Status: "in_progress", EpicID: "e1",
			SpentUSD: 8, ClosingUSD: 5, SpentTokens: 400, ClosingTokens: 100, Attempt: 2},
	}}
	caps := query.BudgetCaps{IssueUSD: 10, IssueTokens: 2000, EpicUSD: 50, EpicTokens: 1000, MaxRetries: 3}
	s := New(Options{Reader: query.NewReader(issues, fakeArts{}, fakeProv{}), BudgetCaps: caps})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/budgets")
	if r.status != http.StatusOK {
		t.Fatalf("/budgets status = %d, want 200", r.status)
	}
	// The breaching issue links to its detail page, and the over-cap meter shows 100%.
	for _, want := range []string{"Epics", "Issues", `href="/issue/h-9"`, "e1", "100%"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/budgets missing %q", want)
		}
	}

	frag := get(t, ts, "/budgets/items")
	if frag.status != http.StatusOK {
		t.Fatalf("/budgets/items status = %d, want 200", frag.status)
	}
	if strings.Contains(strings.ToLower(frag.body), "<!doctype html>") {
		t.Errorf("/budgets/items should be a bare fragment, not a full page")
	}
	if !strings.Contains(frag.body, "h-9") {
		t.Errorf("/budgets/items missing the rendered issue row, got: %s", frag.body)
	}
}

// TestProvenanceNotAttached mirrors the other views' graceful degradation.
func TestProvenanceNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/provenance")
	if r.status != http.StatusOK {
		t.Fatalf("/provenance status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("/provenance missing not-attached notice, got: %s", r.body)
	}
	if frag := get(t, ts, "/provenance/items"); frag.status != http.StatusServiceUnavailable {
		t.Errorf("/provenance/items status = %d, want 503", frag.status)
	}
}

// TestProvenanceRendersChain proves the wired view renders the full commit→issue→soul→model→
// prompt→evidence chain with the prompt and a passing check linking to their raw artifacts,
// and that the fragment is bare for the htmx swap.
func TestProvenanceRendersChain(t *testing.T) {
	prov := recentProv{commits: []query.MergedCommit{{
		Commit: "deadbeefcafe1234",
		Provenance: core.Provenance{
			Issue: "h-1", Soul: "implementor", Model: "claude-opus-4-8",
			PromptSHA: "sha256:promptaaaa", Verified: []string{"acceptance-tests@sha256:gatebbbb"},
		},
	}}}
	s := New(Options{Reader: query.NewReader(&fakeIssues{}, fakeArts{}, prov)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/provenance")
	if r.status != http.StatusOK {
		t.Fatalf("/provenance status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"implementor", "claude-opus-4-8",
		`href="/issue/h-1"`,                  // issue drill-through
		`href="/artifact/sha256:promptaaaa"`, // prompt → raw artifact
		`href="/artifact/sha256:gatebbbb"`,   // verified check → evidence artifact
		"acceptance-tests",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/provenance missing %q", want)
		}
	}

	frag := get(t, ts, "/provenance/items")
	if frag.status != http.StatusOK {
		t.Fatalf("/provenance/items status = %d, want 200", frag.status)
	}
	if strings.Contains(strings.ToLower(frag.body), "<!doctype html>") {
		t.Errorf("/provenance/items should be a bare fragment, not a full page")
	}
}

// TestProvenanceRendersSignatureBadge proves verify-on-read surfaces in the view (T5.10): a
// verified commit shows "signed", an untrusted one "unverified", an unsigned one "unsigned",
// and an unchecked one (no allowed-signers configured) shows no badge — so an unsigned
// deployment's view is unchanged.
func TestProvenanceRendersSignatureBadge(t *testing.T) {
	cases := []struct {
		status   query.SignatureStatus
		wantText string
		absent   bool
	}{
		{query.SignatureVerified, "signed", false},
		{query.SignatureUntrusted, "unverified", false},
		{query.SignatureUnsigned, "unsigned", false},
		{query.SignatureUnchecked, "", true},
	}
	for _, tc := range cases {
		prov := recentProv{commits: []query.MergedCommit{{
			Commit:     "deadbeefcafe1234",
			Provenance: core.Provenance{Issue: "h-1", Soul: "implementor", Model: "m"},
			Signature:  tc.status,
		}}}
		s := New(Options{Reader: query.NewReader(&fakeIssues{}, fakeArts{}, prov)})
		ts := httptest.NewServer(s.Handler())
		body := get(t, ts, "/provenance/items").body
		ts.Close()

		has := strings.Contains(body, ">"+tc.wantText+"<")
		if tc.absent {
			// No badge: none of the three badge labels should appear.
			for _, label := range []string{">signed<", ">unverified<", ">unsigned<"} {
				if strings.Contains(body, label) {
					t.Errorf("status %q: unexpected badge %q in body", tc.status, label)
				}
			}
			continue
		}
		if !has {
			t.Errorf("status %q: missing badge text %q; body:\n%s", tc.status, tc.wantText, body)
		}
	}
}
