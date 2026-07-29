package controlroom

import (
	"net/http"
	"strings"
	"testing"
)

// TestReplayPageRenders proves T4.11's page renders the reconstructed trail for merged work:
// detailServer's factory-1 carries a transcript hash whose (one-turn) content is in the store,
// so the page shows the decision-trail section, a turn card, and the raw-transcript and
// back-to-issue links. It reuses the detailServer fixture so the read fakes stay in one place.
func TestReplayPageRenders(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/replay/factory-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"Replay — Merged work",        // header (issue title)
		"factory-1",                   // id
		"Decision trail",              // the trail section
		"Turn 0",                      // the single turn card
		"/artifact/" + transcriptHash, // raw-bytes link to the transcript
		"/issue/factory-1",            // back to the detail page
		`href="/static/app.css"`,      // inside the base layout chrome
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("replay page missing %q", want)
		}
	}
}

// TestReplayNoTranscriptNotice covers a known issue with no reachable transcript (factory-2 is
// in flight — no provenance): the page still renders the issue header but shows the
// "none captured" notice instead of a trail, and never errors.
func TestReplayNoTranscriptNotice(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/replay/factory-2")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "In flight") {
		t.Errorf("replay page missing the issue header for known in-flight work")
	}
	if !strings.Contains(r.body, "No decision trail was captured") {
		t.Errorf("replay page missing the no-transcript notice")
	}
	if strings.Contains(r.body, "Decision trail</h2>") {
		t.Errorf("an issue with no transcript must not render a trail section")
	}
}

// TestReplayWithoutReader covers the standalone path: with no read model the page shows the
// not-attached notice (200, in chrome), mirroring the issue-detail page.
func TestReplayWithoutReader(t *testing.T) {
	ts := newTestServer(t) // no Options.Reader
	r := get(t, ts, "/replay/factory-1")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("replay missing the not-attached notice")
	}
}

// TestReplayUnknown proves an unknown id renders an in-chrome notice naming the id rather than
// a blank 500 — the same "never 500 blank" handling the detail page uses.
func TestReplayUnknown(t *testing.T) {
	ts := detailServer(t)
	r := get(t, ts, "/replay/nope")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Could not load replay for nope") {
		t.Errorf("missing the not-found notice: %q", r.body)
	}
}

// TestIssueDetailLinksToReplayWhenTranscript proves the drill-through: merged work with a
// transcript surfaces the replay link, while in-flight work (no transcript) does not.
func TestIssueDetailLinksToReplayWhenTranscript(t *testing.T) {
	ts := detailServer(t)

	merged := get(t, ts, "/issue/factory-1")
	if !strings.Contains(merged.body, "/replay/factory-1") || !strings.Contains(merged.body, "Replay decision trail") {
		t.Errorf("merged issue detail missing the replay drill-through link")
	}

	inflight := get(t, ts, "/issue/factory-2")
	if strings.Contains(inflight.body, "/replay/factory-2") {
		t.Errorf("in-flight issue (no transcript) must not show a replay link")
	}
}
