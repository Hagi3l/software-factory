package orchestrator

import (
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// harness identity stamped on the trusted provenance commit. Generated code is authored
// by the untrusted agent; the integration commit that lands it on main is authored by
// the trusted layer, so it carries the harness's own identity rather than any agent's.
// When a signing key is configured the merger SSH-signs this commit with the harness's
// key (config.SigningConfig, T5.10), so the identity is cryptographically vouched for and
// not merely a label; the email here is the principal an allowed-signers file maps to the
// harness public key for verify-on-read (see specs/security.md, internal/orchestrator/merge.go).
const (
	provenanceCommitterName  = "harness"
	provenanceCommitterEmail = "harness@localhost"
)

// The provenance record itself — the Provenance type, its Trailer/CommitMessage
// rendering, and the inverse ParseCommitMessage — lives in package core, because the
// control room reads the same trailer back off git (specs/observability.md). This file
// keeps only the orchestrator-side assembly: turning an accepted issue + gate report into
// a core.Provenance, and the trusted committer identity stamped on the merge.

// provenanceFor assembles the provenance record for an accepted issue: the soul/model
// from config, the issue id, the issue title (rendered as the commit subject), the
// harvested Prompt-SHA from the Result's evidence, and the names of the checks the gate
// verified.
func (o *Orchestrator) provenanceFor(issue core.Issue, res core.Result, report gate.Report) core.Provenance {
	prov := core.Provenance{
		Issue:        issue.ID,
		// The issue title becomes the commit subject so main's history reads like an
		// ordinary project's ("Add single-use share link"), not "Integrate <id>". Purely
		// cosmetic — the durable reference stays the Issue id on the trailer below.
		Subject:      issue.Title,
		PromptSHA:    res.Evidence.PromptSHA,
		Verified:     verifiedChecks(report),
		Traceability: issue.TraceMap,
		Transcript:   transcriptHash(res),
		// The explore sub-loop's audit trail: the pinned explorer model the runner stamped when
		// the sub-loop actually ran, and its own harvested transcript hash. Both empty on the
		// common no-explore invocation, so the trailer stays two lines; recorded together they
		// make the cheap-tier comprehension auditable (specs/models.md "Helper souls", T12.4).
		ExploreModel:      res.ExploreModel,
		ExploreTranscript: exploreTranscriptHash(res),
		// The independent test author, threaded onto this issue from the author-tests stage
		// (like TraceMap). Soul below is the implementor (this issue's own producing soul);
		// recording both makes producer ≠ verifier auditable from the trailer (T4.22,
		// specs/verification.md). An issue with no author-tests stage in its lineage carries
		// none, rendering as Tests-Soul: (none).
		TestsSoul: issue.TestsSoul,
	}
	if soul, ok := o.selectSoul(issue); ok {
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

// transcriptHash returns the artifact-store hash of a Result's harvested agent transcript
// — the full broker-captured conversation the runner stores under core.ArtifactKindTranscript
// — or "" if the runner could not persist it. The transcript is the replayable decision
// trail (specs/observability.md); citing it by hash in the merge trailer is what makes it
// reachable from the read stores (the control-room issue-detail / replay views), since the
// orchestrator otherwise consumes the Result without retaining its evidence references.
func transcriptHash(res core.Result) string {
	for _, a := range res.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindTranscript {
			return a.Hash
		}
	}
	return ""
}

// exploreTranscriptHash returns the artifact-store hash of a Result's harvested explore
// transcript — the explore tool's nested read-only sub-loop conversation the runner stores
// under core.ArtifactKindExploreTranscript, separately from the main transcript — or "" if the
// invocation ran no explore (most invocations) or the runner could not persist it. Threaded
// into the merge trailer beside the parent Transcript so the cheap-tier comprehension is
// reachable from the read stores, the same way transcriptHash surfaces the parent's (T12.4).
func exploreTranscriptHash(res core.Result) string {
	for _, a := range res.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindExploreTranscript {
			return a.Hash
		}
	}
	return ""
}

// transformLogHash returns the artifact-store hash of a Result's harvested transformation log
// — the JSON []core.TransformRecord the runner stores under core.ArtifactKindTransformLog,
// recording the mechanism (semantic vs text floor) of each semantic write (T6.3) — or "" if
// the invocation ran no semantic write tools (most Results) or the runner could not persist it.
// Stamped onto the issue (StampTransformLog) so the verification view can weigh a candidate's
// text-fallback transformations, the same way traceMapHash/transcriptHash are surfaced.
func transformLogHash(res core.Result) string {
	for _, a := range res.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindTransformLog {
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
