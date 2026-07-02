package core

import (
	"fmt"
	"strings"
)

// Provenance is the SLSA-style attribution recorded on every merge to main: which soul
// and model produced the change, for which issue, under which exact prompt, and which
// gate checks verified it. Because no human reviews the code, this trailer IS the
// accountability — it makes every autonomous change traceable (see specs/security.md,
// specs/integration.md). Prompt-SHA is a content address into the artifact store, so the
// cited prompt cannot be silently altered after the fact.
//
// Soul is the implementor; TestsSoul is the independent author of the acceptance tests.
// Recording both on the commit makes producer ≠ verifier (see specs/verification.md)
// auditable from the trailer alone, not merely enforced at runtime — a change with no
// author-tests stage in its lineage carries Tests-Soul: (none), the same self-describing
// degradation as a missing Traceability.
//
// It lives in core because both sides of the trailer must agree on one format: the
// orchestrator renders it onto the integration commit (the write side), and the control
// room reads it back off git to drive the provenance and issue-detail views (the read
// side, specs/observability.md, specs/control-room.md). Keeping render (Trailer/
// CommitMessage) and parse (ParseCommitMessage) adjacent in a single type is what keeps
// the two sides from drifting — there is no second copy of the format to maintain.
type Provenance struct {
	Soul         string   // the soul that produced the candidate (the implementor)
	TestsSoul    string   // the soul that authored the acceptance tests (the independent test author)
	Model        string   // the model that drove it
	Issue        string   // the beads issue this change answers
	PromptSHA    string   // artifact-store hash of the exact prompt the invocation ran with
	Verified     []string // gate checks that passed, each cited as name@<evidence-hash> when persisted
	Traceability string   // artifact-store hash of the author-tests test↔spec traceability map
	Transcript   string   // artifact-store hash of the full agent conversation (the replayable decision trail)

	// ExploreModel and ExploreTranscript record the explore tool's nested read-only sub-loop:
	// the second model that ran in the same sandbox and the artifact-store hash of its own
	// (separately harvested) conversation. Recording BOTH the parent and the pinned explorer
	// model makes the tier the exploration ran under auditable rather than a hidden side-channel
	// (specs/models.md "Helper souls", specs/components/agent.md rule 5). They render as an extra
	// trailer line ONLY when explore actually ran, so a change that used no explore keeps the
	// exact two-line trailer it always had (the read side treats the line as optional).
	ExploreModel      string
	ExploreTranscript string

	// Subject is the human-readable commit subject line (the issue's title), purely
	// cosmetic so a merge reads like ordinary project history ("Add single-use share
	// link") rather than "Integrate <id>". It is NOT part of the audited trailer: it is
	// not signed-for in the SLSA sense (the Issue id on the trailer is the durable
	// reference), and ParseCommitMessage does not recover it — the round trip is over the
	// trailer only. Empty falls back to "Integrate <Issue>", so a record with no title
	// stays self-describing.
	Subject string
}

// Trailer renders the provenance block exactly as specs/security.md and
// specs/integration.md specify: two lines of pipe-separated fields. Empty fields render
// as "(none)" rather than blank so a degraded record (e.g. a prompt that failed to
// harvest, a change merged without an author-tests stage and so no traceability map, or an
// invocation whose transcript could not be harvested) stays self-describing instead of
// looking truncated.
func (p Provenance) Trailer() string {
	trailer := fmt.Sprintf("Soul: %s | Model: %s | Tests-Soul: %s\nIssue: %s | Prompt-SHA: %s | Verified: %s | Traceability: %s | Transcript: %s",
		orNone(p.Soul), orNone(p.Model), orNone(p.TestsSoul), orNone(p.Issue), orNone(p.PromptSHA),
		orNone(strings.Join(p.Verified, ",")), orNone(p.Traceability), orNone(p.Transcript))
	// Append the explore line only when a nested read-only sub-loop actually ran (either field
	// set), so a change that used no explore stays byte-for-byte identical to the historical
	// two-line trailer — backward compatibility for every pre-explore commit and its round trip.
	if p.ExploreModel != "" || p.ExploreTranscript != "" {
		trailer += fmt.Sprintf("\nExplorer-Model: %s | Explore-Transcript: %s", orNone(p.ExploreModel), orNone(p.ExploreTranscript))
	}
	return trailer
}

// CommitMessage is the full message for the integration commit: a one-line subject plus
// the provenance trailer, separated by a blank line per git convention. The subject is the
// issue's title when one was threaded through (Subject), falling back to "Integrate
// <Issue>" so a record with no title still produces a valid, self-describing message. The
// trailer below the blank line is unchanged either way — it is the audited part, and the
// read side (ParseCommitMessage) keys off it, never the subject.
func (p Provenance) CommitMessage() string {
	subject := p.Subject
	if subject == "" {
		subject = "Integrate " + orNone(p.Issue)
	}
	return fmt.Sprintf("%s\n\n%s", subject, p.Trailer())
}

// ParseCommitMessage is the inverse of CommitMessage/Trailer: it extracts a Provenance
// from an integration commit's full message and reports whether a provenance trailer was
// found. It is the read side of the format the control room renders from git history.
//
// It is lenient by design — the read path must never fail on a commit it does not
// recognize (a hand-authored merge, a pre-provenance commit, a partially written
// trailer): a message with no recognizable trailer yields ok=false, and an individual
// missing field decodes to "". This mirrors the leniency of the beads metadata reader:
// foreign or malformed records degrade, they do not error. The "(none)" sentinel maps
// back to the empty string so a round trip is exact.
func ParseCommitMessage(msg string) (Provenance, bool) {
	var prov Provenance
	found := false
	for _, line := range strings.Split(msg, "\n") {
		fields, ok := parseTrailerLine(line)
		if !ok {
			continue
		}
		found = true
		for k, v := range fields {
			switch k {
			case "Soul":
				prov.Soul = v
			case "Tests-Soul":
				prov.TestsSoul = v
			case "Model":
				prov.Model = v
			case "Issue":
				prov.Issue = v
			case "Prompt-SHA":
				prov.PromptSHA = v
			case "Traceability":
				prov.Traceability = v
			case "Transcript":
				prov.Transcript = v
			case "Explorer-Model":
				prov.ExploreModel = v
			case "Explore-Transcript":
				prov.ExploreTranscript = v
			case "Verified":
				prov.Verified = splitVerified(v)
			}
		}
	}
	return prov, found
}

// parseTrailerLine recognizes a single trailer line — one or more " | "-separated
// "Key: value" pairs — and returns the decoded fields. It reports ok=false for any line
// that is not a trailer line (the subject, blank lines, body prose) so ParseCommitMessage
// can skip them. A line counts as a trailer line only if every pipe-separated segment is a
// "Key: value" pair AND at least one key is one we recognize, so an arbitrary prose line
// containing a colon is not mistaken for the trailer.
func parseTrailerLine(line string) (map[string]string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, false
	}
	fields := make(map[string]string)
	recognized := false
	for _, seg := range strings.Split(line, " | ") {
		key, val, ok := strings.Cut(seg, ": ")
		if !ok {
			return nil, false
		}
		key = strings.TrimSpace(key)
		val = noneToEmpty(strings.TrimSpace(val))
		fields[key] = val
		switch key {
		case "Soul", "Tests-Soul", "Model", "Issue", "Prompt-SHA", "Verified", "Traceability", "Transcript", "Explorer-Model", "Explore-Transcript":
			recognized = true
		}
	}
	if !recognized {
		return nil, false
	}
	return fields, true
}

// splitVerified decodes the comma-joined "Verified:" field back into its name@<hash>
// entries, dropping empties so a "(none)" (already mapped to "") yields a nil slice
// rather than a one-element slice of "".
func splitVerified(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func noneToEmpty(s string) string {
	if s == "(none)" {
		return ""
	}
	return s
}
