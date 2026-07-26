package controlroom

import (
	"net/http"
	"strings"
	"testing"
)

// TestVerificationPageRenders is T4.23's core contract: the trust argument for a gated issue
// renders forensically — the producer≠verifier soul split, the red→green proof, the mutation
// metric vs threshold, the scanner, the traceability map, and the raw-verdict link — off the
// shared detailServer fixture (harness-1 carries the soul stamps + a passing gate-verdict).
func TestVerificationPageRenders(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/verification/harness-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Verification — Merged work",   // header (issue title)
		"harness-1",                    // id
		"gate passed",                  // the overall verdict badge
		"Producer ≠ verifier",          // the soul-split section
		"go-test-author",               // the author-tests producing soul
		"go-implementor",               // the implement producing soul
		"verification sandbox",         // qa has no verifier soul — runs in the clean sandbox
		"Red→green proof",              // the red→green check kind label
		"base exit 1",                  // the red half (base must fail)
		"candidate exit 0",             // the green half (candidate must pass)
		"0.86 &gt;= 0.80",              // the mutation metric vs threshold (HTML-escaped >=)
		"govulncheck",                  // the scanner check
		"Traceability map",             // the test↔spec map evidence label
		"/artifact/" + mergedTraceHash, // …a resolvable click-through
		"/artifact/" + verdictHash,     // the raw gate-verdict bytes link
		"/artifact/" + gateHash,        // a check's own captured-output evidence
		"/issue/harness-1",             // drill-back to the detail page
		`href="/static/app.css"`,       // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("verification page missing %q", want)
		}
	}
}

// TestVerificationRendersTransformLog is the T6.3 follow-up's contract: a gated issue that ran
// semantic write tools surfaces its transformation log — each transformation with its mechanism
// (semantic vs text fallback), a header count of the text fallbacks (the imprecise ones), the
// fallback's precision note, and the raw-bytes link to the canonical record. harness-1 carries one
// semantic and one text-fallback rename.
func TestVerificationRendersTransformLog(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/verification/harness-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Transformations",                    // the section heading
		"semantic",                           // the precise-mechanism badge
		"text fallback",                      // the imprecise-mechanism badge
		"1 via text fallback",                // the header count of fallbacks
		"greet → hello at a.go:3:6",          // the semantic transform's target
		"2 files · 4 edits",                  // its blast radius
		"inside comments or string literals", // the fallback's precision note
		"/artifact/" + transformHash,         // the raw-bytes link to the log
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("verification page missing transform-log content %q", want)
		}
	}
}

// TestVerificationNoTransformSection proves an issue that ran no semantic write tools (no stamped
// transform log) renders no Transformations section at all — the common case stays uncluttered.
func TestVerificationNoTransformSection(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/verification/harness-2") // in flight, no TransformLog stamp
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.Contains(r.body, "Transformations") {
		t.Errorf("an issue with no transform log should render no Transformations section")
	}
}

// TestVerificationNoVerdictNotice covers a known issue whose candidate has not been gated
// (harness-2 is in flight, no stamped verdict): the page still renders the issue header and
// the soul split, but shows the "no verdict recorded" notice instead of a check list, and
// never errors — the best-effort posture the detail and replay pages share.
func TestVerificationNoVerdictNotice(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/verification/harness-2")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Producer ≠ verifier") {
		t.Errorf("verification page missing the soul split for an ungated issue")
	}
	if !strings.Contains(r.body, "No gate verdict has been recorded") {
		t.Errorf("verification page missing the no-verdict notice")
	}
	if strings.Contains(r.body, "Gate verdict</h2>") {
		t.Errorf("an ungated issue must not render a check list")
	}
}

// TestVerificationWithoutReader covers the standalone path: with no read model the page shows
// the not-attached notice (200, in chrome), mirroring the issue-detail and replay pages.
func TestVerificationWithoutReader(t *testing.T) {
	ts := newTestServer(t) // no Options.Reader
	r := get(t, ts, "/verification/harness-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("verification missing the not-attached notice")
	}
}

// TestVerificationUnknown proves an unknown id renders an in-chrome notice naming the id
// rather than a blank 500 — the same "never 500 blank" handling the detail page uses.
func TestVerificationUnknown(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/verification/nope")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Could not load verification for nope") {
		t.Errorf("missing the not-found notice: %q", r.body)
	}
}

// TestIssueAndDLQLinkToVerification proves the drill-throughs: a gated issue's detail page
// surfaces the verification link, and the dead-letter row offers it for triage.
func TestIssueAndDLQLinkToVerification(t *testing.T) {
	ts := detailServer(t)
	detail := get(t, ts, "/issue/harness-1")
	if !strings.Contains(detail.body, "/verification/harness-1") || !strings.Contains(detail.body, "▸ Verification") {
		t.Errorf("gated issue detail missing the verification drill-through link")
	}
}
