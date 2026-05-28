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
	Soul      string   // the soul that produced the candidate
	Model     string   // the model that drove it
	Issue     string   // the beads issue this change answers
	PromptSHA string   // artifact-store hash of the exact prompt the invocation ran with
	Verified  []string // names of the gate checks that passed
}

// Trailer renders the provenance block exactly as specs/security.md and
// specs/integration.md specify: two lines of pipe-separated fields. Empty fields render
// as "(none)" rather than blank so a degraded record (e.g. a prompt that failed to
// harvest) stays self-describing instead of looking truncated.
func (p Provenance) Trailer() string {
	return fmt.Sprintf("Soul: %s | Model: %s\nIssue: %s | Prompt-SHA: %s | Verified: %s",
		orNone(p.Soul), orNone(p.Model), orNone(p.Issue), orNone(p.PromptSHA), orNone(strings.Join(p.Verified, ",")))
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
		Issue:     issue.ID,
		PromptSHA: res.Evidence.PromptSHA,
		Verified:  verifiedChecks(report),
	}
	if soul, ok := o.soulForRole(issue.Role); ok {
		prov.Soul = soul.Name
		prov.Model = soul.Model
	}
	return prov
}

// verifiedChecks returns the names of the checks that passed in the gate report. The
// report's Passed is already true at this point (the candidate was accepted), so every
// recorded check passed; listing them is what the trailer's "Verified:" field carries.
func verifiedChecks(report gate.Report) []string {
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		if c.Passed {
			names = append(names, c.Name)
		}
	}
	return names
}
