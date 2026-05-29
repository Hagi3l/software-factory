package orchestrator

import (
	"fmt"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// harness identity stamped on the trusted provenance commit. Generated code is authored
// by the untrusted agent; the integration commit that lands it on main is authored by
// the trusted layer, so it carries the harness's own identity rather than any agent's.
// Signing this identity with a real key is an OPEN question (see specs/security.md).
const (
	provenanceCommitterName  = "harness"
	provenanceCommitterEmail = "harness@localhost"
)

// Provenance is the SLSA-style attribution recorded on every merge to main: which soul
// and model produced the change, for which issue, under which exact prompt, and which
// gate checks verified it. Because no human reviews the code, this trailer IS the
// accountability — it makes every autonomous change traceable (see specs/security.md,
// specs/integration.md). Prompt-SHA is a content address into the artifact store, so the
// cited prompt cannot be silently altered after the fact.
type Provenance struct {
	Soul         string   // the soul that produced the candidate
	Model        string   // the model that drove it
	Issue        string   // the beads issue this change answers
	PromptSHA    string   // artifact-store hash of the exact prompt the invocation ran with
	Verified     []string // gate checks that passed, each cited as name@<evidence-hash> when persisted
	Traceability string   // artifact-store hash of the author-tests test↔spec traceability map
}

// Trailer renders the provenance block exactly as specs/security.md and
// specs/integration.md specify: two lines of pipe-separated fields. Empty fields render
// as "(none)" rather than blank so a degraded record (e.g. a prompt that failed to
// harvest, or a change merged without an author-tests stage and so no traceability map)
// stays self-describing instead of looking truncated.
func (p Provenance) Trailer() string {
	return fmt.Sprintf("Soul: %s | Model: %s\nIssue: %s | Prompt-SHA: %s | Verified: %s | Traceability: %s",
		orNone(p.Soul), orNone(p.Model), orNone(p.Issue), orNone(p.PromptSHA),
		orNone(strings.Join(p.Verified, ",")), orNone(p.Traceability))
}

// CommitMessage is the full message for the integration commit: a one-line subject plus
// the provenance trailer, separated by a blank line per git convention.
func (p Provenance) CommitMessage() string {
	return fmt.Sprintf("Integrate %s\n\n%s", orNone(p.Issue), p.Trailer())
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// provenanceFor assembles the provenance record for an accepted issue: the soul/model
// from config, the issue id, the harvested Prompt-SHA from the Result's evidence, and
// the names of the checks the gate verified.
func (o *Orchestrator) provenanceFor(issue core.Issue, res core.Result, report gate.Report) Provenance {
	prov := Provenance{
		Issue:        issue.ID,
		PromptSHA:    res.Evidence.PromptSHA,
		Verified:     verifiedChecks(report),
		Traceability: issue.TraceMap,
	}
	if soul, ok := o.soulForRole(issue.Role); ok {
		prov.Soul = soul.Name
		prov.Model = soul.Model
	}
	return prov
}

// traceMapHash returns the artifact-store hash of a Result's harvested test↔spec
// traceability map, or "" if the Result carries none (most roles) or the runner could not
// persist it. The runner stores the map under core.ArtifactKindTraceabilityMap and clears
// the structured form, so the hash on Evidence is the canonical reference the orchestrator
// threads forward (see specs/verification.md).
func traceMapHash(res core.Result) string {
	for _, a := range res.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindTraceabilityMap {
			return a.Hash
		}
	}
	return ""
}

// verifiedChecks renders the checks that passed in the gate report for the trailer's
// "Verified:" field. The report's Passed is already true at this point (the candidate
// was accepted), so every recorded check passed. Each is cited as name@<evidence-hash>
// — the hash pointing into the artifact store at the persisted stdout/stderr — so a
// merged commit's verification is auditable down to the exact captured output, not just
// a list of names. When the gate could not persist a check's evidence (no store, or a
// failed write) the hash is empty and the check degrades to a bare name, mirroring how
// the trailer renders a missing Prompt-SHA: self-describing rather than silently wrong.
func verifiedChecks(report gate.Report) []string {
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		if !c.Passed {
			continue
		}
		if c.Evidence.Hash != "" {
			names = append(names, c.Name+"@"+c.Evidence.Hash)
		} else {
			names = append(names, c.Name)
		}
	}
	return names
}
