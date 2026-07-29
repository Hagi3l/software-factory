package orchestrator

import (
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
)

// A passed check whose evidence was persisted is cited as name@<hash> in the trailer,
// so a merged commit's verification points into the artifact store at the exact
// captured output, not merely a list of names (see specs/components/artifact-store.md).
func TestVerifiedChecksCitesEvidenceHashes(t *testing.T) {
	report := gate.Report{Passed: true, Checks: []gate.CheckResult{
		{Name: "build", Passed: true, Evidence: core.ArtifactRef{Kind: "gate-evidence", Hash: "sha256:aaa"}},
		{Name: "test", Passed: true, Evidence: core.ArtifactRef{Kind: "gate-evidence", Hash: "sha256:bbb"}},
	}}
	got := verifiedChecks(report)
	if len(got) != 2 || got[0] != "build@sha256:aaa" || got[1] != "test@sha256:bbb" {
		t.Fatalf("verifiedChecks = %v, want [build@sha256:aaa test@sha256:bbb]", got)
	}
}

// When a check's evidence failed to persist (empty hash) it degrades to a bare name
// rather than rendering a dangling "@" — a self-describing degraded record, mirroring
// how the trailer renders a missing Prompt-SHA.
func TestVerifiedChecksDegradesWithoutEvidence(t *testing.T) {
	report := gate.Report{Passed: true, Checks: []gate.CheckResult{
		{Name: "build", Passed: true, Evidence: core.ArtifactRef{Hash: "sha256:aaa"}},
		{Name: "test", Passed: true}, // evidence write failed: no hash
	}}
	got := verifiedChecks(report)
	if len(got) != 2 || got[0] != "build@sha256:aaa" || got[1] != "test" {
		t.Fatalf("verifiedChecks = %v, want [build@sha256:aaa test]", got)
	}
}
