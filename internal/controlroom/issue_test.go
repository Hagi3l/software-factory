package controlroom

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/core"
)

// The detail view stitches all three stores, so its fakes are richer than the board's:
// detailIssues serves issues by id, detailProv reports merge provenance for the merged
// one, and detailArts holds the content the evidence links resolve to.
type detailIssues struct{ m map[string]core.Issue }

func (d detailIssues) Get(_ context.Context, id string) (core.Issue, error) {
	if i, ok := d.m[id]; ok {
		return i, nil
	}
	return core.Issue{}, fmt.Errorf("issue %s: not found", id)
}
func (d detailIssues) List(context.Context, string) ([]core.Issue, error) { return nil, nil }
func (d detailIssues) ListAll(context.Context) ([]core.Issue, error)      { return nil, nil }

type detailArts struct{ content map[string]string }

func (d detailArts) Has(_ context.Context, h string) (bool, error) {
	_, ok := d.content[h]
	return ok, nil
}
func (d detailArts) Get(_ context.Context, h string) (io.ReadCloser, error) {
	if c, ok := d.content[h]; ok {
		return io.NopCloser(strings.NewReader(c)), nil
	}
	return nil, fmt.Errorf("artifact %s: not found", h)
}

type detailProv struct {
	byIssue map[string]core.Provenance
	diff    map[string]string
}

func (d detailProv) ByIssue(_ context.Context, id string) (core.Provenance, bool, error) {
	if p, ok := d.byIssue[id]; ok {
		return p, true, nil
	}
	return core.Provenance{}, false, nil
}
func (d detailProv) DiffByIssue(_ context.Context, id string) (string, bool, error) {
	if diff, ok := d.diff[id]; ok {
		return diff, true, nil
	}
	return "", false, nil
}
func (detailProv) Recent(context.Context, int) ([]query.MergedCommit, error) { return nil, nil }

const (
	promptHash     = "sha256:promptaaaaaaaaaa"
	transcriptHash = "sha256:transcriptccccc"
	gateHash       = "sha256:gatebbbbbbbbbbbb"
	traceHash      = "sha256:tracedddddddddd"
)

func detailServer(t *testing.T) *httptest.Server {
	t.Helper()
	issues := detailIssues{m: map[string]core.Issue{
		// harness-1 is merged: provenance is present, evidence comes from the trailer.
		"harness-1": {
			ID: "harness-1", Title: "Merged work", Status: "closed", Role: "implementor",
			Spec: "specs/x.md", SpecHash: "sha256:speccccccccc", Base: "harness-0-candidate",
			Attempt: 2, SpentTokens: 1234, SpentUSD: 0.0456, Body: "Implement the widget.",
		},
		// harness-2 is in flight: no provenance, evidence falls back to the threaded map.
		"harness-2": {
			ID: "harness-2", Title: "In flight", Status: "in_progress", Role: "test-author",
			TraceMap: traceHash,
		},
	}}
	arts := detailArts{content: map[string]string{
		promptHash:     "you are an implementor; build the widget",
		transcriptHash: `[{"request":{},"response":{}}]`,
		gateHash:       "PASS: tests-pass\nok\tpkg\t0.1s",
	}}
	prov := detailProv{
		byIssue: map[string]core.Provenance{
			"harness-1": {
				Soul: "go-implementor", Model: "claude-test", Issue: "harness-1",
				PromptSHA:  promptHash,
				Transcript: transcriptHash,
				// One check with persisted evidence (a link), one bare (ran, no evidence).
				Verified: []string{"tests-pass@" + gateHash, "mutation"},
			},
		},
		diff: map[string]string{
			"harness-1": "diff --git a/widget.go b/widget.go\n+func Widget() {}",
		},
	}
	s := New(Options{Version: "test", Reader: query.NewReader(issues, arts, prov)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestIssueDetailMergedRendersEvidence is T4.7's core contract for landed work: the brief,
// budget, retries, and merge provenance render, and each cited artifact becomes a
// click-through (or, for a bare-name check, shows that it ran without a dead link).
func TestIssueDetailMergedRendersEvidence(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/issue/harness-1")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Merged work",             // title
		"harness-1",               // id
		">merged<",                // the merged badge
		"implementor",             // role
		"specs/x.md",              // spec path (the brief)
		"Implement the widget.",   // the body brief
		"1234 tokens",             // budget spend
		"$0.0456",                 // budget spend in dollars
		"go-implementor",          // provenance soul
		"claude-test",             // provenance model
		"tests-pass",                  // a verified gate check label
		"/artifact/" + promptHash,     // the prompt link points at the content endpoint
		"/artifact/" + transcriptHash, // the transcript (replayable decision trail) link too
		"Transcript",                  // its label
		"/artifact/" + gateHash,       // the gate-evidence link too
		"mutation",                    // the bare-name check still shows
		"no evidence persisted",       // …without a link
		"Candidate diff",              // the diff section heading
		"diff --git a/widget.go",      // the landed candidate diff, rendered inline
		`href="/static/app.css"`,      // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("issue detail missing %q", want)
		}
	}
	// The attempt counter (a retry happened) renders its value.
	if !strings.Contains(r.body, ">2<") {
		t.Errorf("issue detail missing attempt count")
	}
}

// TestIssueDetailInFlightFallsBackToTraceMap covers the not-yet-merged path: no provenance
// section, but the traceability map threaded onto the issue still surfaces as evidence.
func TestIssueDetailInFlightFallsBackToTraceMap(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/issue/harness-2")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "In flight") {
		t.Errorf("issue detail missing title")
	}
	if strings.Contains(r.body, ">merged<") {
		t.Errorf("in-flight issue must not show the merged badge")
	}
	// Match the section heading, not the word — "Provenance" is also a nav link in the chrome.
	if strings.Contains(r.body, ">Provenance</h2>") {
		t.Errorf("in-flight issue must not render a provenance section")
	}
	if !strings.Contains(r.body, "Traceability") {
		t.Errorf("in-flight issue missing the threaded traceability evidence")
	}
	// No candidate diff exists until the work lands, so the diff section must be absent.
	if strings.Contains(r.body, "Candidate diff") {
		t.Errorf("in-flight issue must not render a candidate-diff section")
	}
	// The trace artifact is not in the store fake, so the link degrades to unavailable
	// rather than a dead href.
	if strings.Contains(r.body, "/artifact/"+traceHash) {
		t.Errorf("unavailable evidence must not render a click-through link")
	}
}

// TestIssueDetailUnknown proves an unknown id renders an in-chrome notice (naming the id),
// not a blank 500 — the same "never 500 blank" handling the board uses.
func TestIssueDetailUnknown(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/issue/nope")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Could not load issue nope") {
		t.Errorf("missing the not-found notice: %q", r.body)
	}
}

// TestIssueDetailWithoutReader covers the standalone path: the page shows the not-attached
// notice (200, in chrome) and the artifact data endpoint 503s.
func TestIssueDetailWithoutReader(t *testing.T) {
	ts := newTestServer(t) // no Options.Reader
	page := get(t, ts, "/issue/harness-1")
	if page.status != http.StatusOK {
		t.Fatalf("/issue status = %d, want 200", page.status)
	}
	if !strings.Contains(page.body, "Not attached") {
		t.Errorf("/issue missing the not-attached notice")
	}
	frag := get(t, ts, "/artifact/"+promptHash)
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/artifact status = %d, want 503", frag.status)
	}
}

// TestArtifactStreamsContent proves the evidence endpoint returns the raw bytes as inert
// text/plain with nosniff — the security contract for serving untrusted agent output. The
// colon in the content-address survives path routing.
func TestArtifactStreamsContent(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/artifact/"+promptHash)
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if nosniff := r.header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	if r.body != "you are an implementor; build the widget" {
		t.Errorf("artifact body = %q", r.body)
	}
}

// TestArtifactNotFound proves an unresolvable hash 404s rather than 500ing or hanging.
func TestArtifactNotFound(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/artifact/sha256:doesnotexist")
	if r.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.status)
	}
}
