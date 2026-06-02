package core

import (
	"reflect"
	"testing"
)

// TestProvenanceRoundTrip is the core guarantee that lets the orchestrator write a trailer
// and the control room read it back without a second copy of the format: rendering a
// Provenance and parsing the result must reproduce the original exactly, including the
// name@<hash> citation form and the (none)→"" mapping for absent fields.
func TestProvenanceRoundTrip(t *testing.T) {
	cases := map[string]Provenance{
		"full": {
			Soul:         "implementor-go",
			TestsSoul:    "test-author-go",
			Model:        "claude-opus-4-7",
			Issue:        "harness-1",
			PromptSHA:    "sha256:9af",
			Verified:     []string{"build@sha256:aa", "test@sha256:bb"},
			Traceability: "sha256:cc",
			Transcript:   "sha256:dd",
		},
		"no tests-soul (no author-tests stage in lineage)": {
			Soul:     "implementor-go",
			Model:    "claude-opus-4-7",
			Issue:    "harness-3",
			Verified: []string{"build"},
			// TestsSoul empty → renders Tests-Soul: (none), parses back to "".
		},
		"no traceability or prompt": {
			Soul:     "implementor-go",
			Model:    "claude-opus-4-7",
			Issue:    "harness-2",
			Verified: []string{"build"},
		},
		"empty": {},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := ParseCommitMessage(want.CommitMessage())
			if !ok {
				t.Fatalf("ParseCommitMessage(%q) reported no trailer", want.CommitMessage())
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip:\n got %+v\nwant %+v\ntrailer:\n%s", got, want, want.CommitMessage())
			}
		})
	}
}

// TestTrailerCitesTestsSoul pins the spec's exact first-line layout (security.md /
// integration.md): the implementor and the independent test author are recorded side by
// side, so producer ≠ verifier is auditable from the commit alone.
func TestTrailerCitesTestsSoul(t *testing.T) {
	p := Provenance{Soul: "implementor-go", TestsSoul: "test-author-go", Model: "m", Issue: "i"}
	want := "Soul: implementor-go | Model: m | Tests-Soul: test-author-go"
	first, _, _ := splitFirstLine(p.Trailer())
	if first != want {
		t.Errorf("trailer first line = %q, want %q", first, want)
	}
}

func splitFirstLine(s string) (first, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// TestParseCommitMessageRejectsNonProvenance guards the read path's leniency: an ordinary
// commit message — even one whose prose contains a colon — must not be mistaken for a
// provenance trailer, so the control room never invents attribution for a hand-authored or
// pre-provenance commit.
func TestParseCommitMessageRejectsNonProvenance(t *testing.T) {
	for _, msg := range []string{
		"",
		"fix: a normal commit subject\n\nwith a body: that has colons",
		"WIP",
		"Note: this is not a trailer line",
	} {
		if _, ok := ParseCommitMessage(msg); ok {
			t.Errorf("ParseCommitMessage(%q) = ok, want false", msg)
		}
	}
}

// TestParseTrailerToleratesExtraLines proves a real integration commit (subject + blank +
// trailer, possibly with trailing body) parses to just the provenance fields.
func TestParseTrailerToleratesExtraLines(t *testing.T) {
	msg := "Integrate harness-7\n\n" +
		"Soul: qa-soul | Model: claude-haiku\n" +
		"Issue: harness-7 | Prompt-SHA: sha256:dead | Verified: gosec@sha256:ev | Traceability: (none)\n" +
		"\nSigned-off-by: someone\n"
	got, ok := ParseCommitMessage(msg)
	if !ok {
		t.Fatal("expected a trailer to be found")
	}
	want := Provenance{
		Soul:      "qa-soul",
		Model:     "claude-haiku",
		Issue:     "harness-7",
		PromptSHA: "sha256:dead",
		Verified:  []string{"gosec@sha256:ev"},
		// Traceability is (none) → empty.
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
