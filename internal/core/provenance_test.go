package core

import (
	"reflect"
	"strings"
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
		"with explore sub-loop (both explorer fields recorded)": {
			Soul:              "implementor-go",
			Model:             "claude-opus-4-7",
			Issue:             "harness-4",
			Verified:          []string{"build"},
			ExploreModel:      "claude-haiku",
			ExploreTranscript: "sha256:ee",
		},
		"explore ran but its transcript failed to persist (model still recorded)": {
			Soul:         "implementor-go",
			Model:        "claude-opus-4-7",
			Issue:        "harness-5",
			ExploreModel: "claude-haiku",
			// ExploreTranscript empty → renders Explore-Transcript: (none) but the line is
			// still emitted because the model is set — the degrade-loudly path.
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

// TestCommitMessageSubject pins the cosmetic subject behavior: the issue title becomes the
// commit subject when threaded through, and an absent title falls back to "Integrate
// <Issue>" so the message stays valid and self-describing. Either way the trailer below is
// unchanged and still parses — the subject is never part of the audited round trip.
func TestCommitMessageSubject(t *testing.T) {
	withTitle := Provenance{Subject: "Add single-use share link", Issue: "vault-12", Soul: "impl", Model: "m"}
	first, _, _ := splitFirstLine(withTitle.CommitMessage())
	if first != "Add single-use share link" {
		t.Errorf("subject = %q, want the issue title", first)
	}

	noTitle := Provenance{Issue: "vault-12", Soul: "impl", Model: "m"}
	first, _, _ = splitFirstLine(noTitle.CommitMessage())
	if first != "Integrate vault-12" {
		t.Errorf("fallback subject = %q, want %q", first, "Integrate vault-12")
	}

	// The subject does not pollute the trailer: the record still round-trips over the
	// trailer regardless of the subject above it.
	if got, ok := ParseCommitMessage(withTitle.CommitMessage()); !ok || got.Issue != "vault-12" {
		t.Errorf("ParseCommitMessage with a subject = (%+v, %v), want the trailer to parse with Issue vault-12", got, ok)
	}
}

// TestTrailerExploreLineOptional pins the backward-compatibility guarantee: a change that ran
// no explore renders the historical two-line trailer unchanged (no Explorer-Model line), while
// one that did ran gets exactly one extra line recording both the pinned model and the
// transcript hash — so the cheap-tier comprehension is auditable without disturbing every
// pre-explore commit (specs/models.md "Helper souls", specs/components/agent.md rule 5).
func TestTrailerExploreLineOptional(t *testing.T) {
	noExplore := Provenance{Soul: "impl", Model: "m", Issue: "i"}
	if strings.Contains(noExplore.Trailer(), "Explorer-Model") {
		t.Errorf("no-explore trailer must not carry an Explorer-Model line:\n%s", noExplore.Trailer())
	}
	if got := strings.Count(noExplore.Trailer(), "\n"); got != 1 {
		t.Errorf("no-explore trailer line count: %d newlines, want 1 (two lines)", got)
	}

	withExplore := Provenance{Soul: "impl", Model: "m", Issue: "i", ExploreModel: "cheap", ExploreTranscript: "sha256:ab"}
	want := "Explorer-Model: cheap | Explore-Transcript: sha256:ab"
	trailer := withExplore.Trailer()
	if !strings.HasSuffix(trailer, "\n"+want) {
		t.Errorf("explore trailer last line = %q, want it to end with %q", trailer, want)
	}
}

// TestFeatureTrailerOmitsProducerFieldsAndRoundTrips pins the whole-feature (epic terminal-merge)
// layer that fixes BUG-2 (T15.4). The headline commit on main must NOT render the producer fields
// (Soul/Model/Tests-Soul/…) as "(none)" — the epic root is a plan issue with no soul of its own —
// so FeatureTrailer omits them entirely and instead names the feature and aggregates its children
// (id@integration-hash) plus the union of verified checks. The read side must recover the aggregate
// unchanged (Children + Verified + Issue), so the control-room provenance view renders a real record.
func TestFeatureTrailerOmitsProducerFieldsAndRoundTrips(t *testing.T) {
	p := Provenance{
		Issue:    "feat-1",
		Subject:  "Add single-use share link",
		Children: []string{"iss-2@abc123", "iss-3@def456"},
		Verified: []string{"build", "gosec", "test"},
	}
	trailer := p.FeatureTrailer()

	// The failure BUG-2 reported: producer fields shown as "(none)" on the headline commit.
	for _, forbidden := range []string{"Soul:", "Model:", "Tests-Soul:", "(none)"} {
		if strings.Contains(trailer, forbidden) {
			t.Errorf("feature trailer must omit producer fields, found %q:\n%s", forbidden, trailer)
		}
	}
	// It carries the feature identity and the child aggregate.
	want := "Issue: feat-1 | Children: iss-2@abc123,iss-3@def456 | Verified: build,gosec,test"
	if trailer != want {
		t.Errorf("feature trailer =\n %q\nwant\n %q", trailer, want)
	}
	// The idempotency probe greps for "Issue: <epic> |"; that substring must survive.
	if !strings.Contains(trailer, "Issue: feat-1 |") {
		t.Errorf("feature trailer lost the idempotency-probe substring:\n%s", trailer)
	}

	// Read side: the aggregate round-trips over FeatureCommitMessage (Subject is cosmetic, not
	// recovered — the same contract as the per-item trailer).
	got, ok := ParseCommitMessage(p.FeatureCommitMessage())
	if !ok {
		t.Fatalf("ParseCommitMessage(feature) reported no trailer:\n%s", p.FeatureCommitMessage())
	}
	want2 := Provenance{Issue: "feat-1", Children: []string{"iss-2@abc123", "iss-3@def456"}, Verified: []string{"build", "gosec", "test"}}
	if !reflect.DeepEqual(got, want2) {
		t.Errorf("feature round trip:\n got %+v\nwant %+v", got, want2)
	}

	// Subject falls back to "Integrate <Issue>" when no title was threaded (the plan issue had none).
	noTitle := Provenance{Issue: "feat-1", Children: []string{"iss-2@abc123"}}
	if first, _, _ := splitFirstLine(noTitle.FeatureCommitMessage()); first != "Integrate feat-1" {
		t.Errorf("feature fallback subject = %q, want %q", first, "Integrate feat-1")
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
