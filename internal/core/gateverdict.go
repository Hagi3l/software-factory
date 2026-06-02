package core

// GateVerdict is the assembled, content-addressed record of one gate run — the verdict
// the orchestrator acted on, kept rather than discarded. A gate already produces this
// internally (per-check pass/fail, the red→green base-vs-candidate pair, the mutation
// score against its threshold, each scanner's exit), but historically the orchestrator
// *acted* on it and threw everything but the final disposition away. Because the gate
// carries 100% of the assurance a human reviewer would, the verdict that justified a
// decision is exactly what is worth keeping (see specs/verification.md "The gate verdict
// is recorded").
//
// It lives in core because both sides agree on one shape: the gate harvests it to the
// artifact store under ArtifactKindGateVerdict (the write side), and the control room's
// verification view reads it back to render the trust argument as a forensic snapshot —
// red→green, mutation score, scanners, the producing-soul split — without the gate being
// live (the read side, specs/control-room.md). It is recorded for **every** gate run, pass
// or fail: a rejected candidate's verdict is as worth auditing as an accepted one (it is
// what a human triaging the dead-letter queue needs).
//
// The bulky per-check *output* is not inlined here — each check's Evidence is the hash of
// its own gate-evidence record (the captured stdout/stderr), so this record is the index
// over them, not a copy. A check whose evidence could not be persisted carries an empty
// Evidence, mirroring how the provenance trailer degrades a bare check name.
type GateVerdict struct {
	Passed bool               `json:"passed"`
	Checks []GateCheckOutcome `json:"checks"`
}

// GateCheckOutcome is one check's outcome within a verdict. Kind labels how the check was
// graded (one of the GateCheck* constants), so the view can render a red proof's nonzero
// exit as a pass rather than a contradiction. ExitCode is the candidate run's exit (or the
// single run for a command/metric/red proof). Base and Metric carry the kind-specific
// detail — non-nil only for the kind that produces it — so the assembled record holds
// everything the verification view needs without re-reading each evidence blob.
type GateCheckOutcome struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	// Evidence is the artifact-store hash of this check's captured-output record
	// (ArtifactKindGateEvidence), or "" when it could not be persisted.
	Evidence string `json:"evidence,omitempty"`
	// Base is the red half of a red→green proof: the acceptance tests run against the
	// pre-implementation base, which must FAIL. Non-nil only for a GateCheckRedGreen.
	Base *GateRunOutcome `json:"base,omitempty"`
	// Metric is the graded result of a metric check. Non-nil only for a GateCheckMetric.
	Metric *GateMetricOutcome `json:"metric,omitempty"`
}

// GateRunOutcome captures the base run of a red→green proof: the exit code the acceptance
// tests produced against the pre-implementation base (which must be nonzero for the proof
// to pass). The captured output itself stays in the check's gate-evidence record.
type GateRunOutcome struct {
	ExitCode int `json:"exit_code"`
}

// GateMetricOutcome is the graded result of a metric check (e.g. mutation>=0.8): the score
// the measurement command printed, whether it parsed (false fails closed — an unverifiable
// score is not a passing one), and the comparison it was graded against.
type GateMetricOutcome struct {
	Score     float64 `json:"score"`
	Parsed    bool    `json:"parsed"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
}

// GateCheck* are the verdict's kind labels — the stable, serialized spelling of how a
// check was graded. They mirror the gate's internal checkKind but live in core so the
// write side (the gate building the record) and the read side (the verification view)
// agree on one vocabulary, the same single-source discipline the artifact kinds use.
const (
	// GateCheckCommand is an ordinary command check, graded on a zero exit (a scanner is one).
	GateCheckCommand = "command"
	// GateCheckRedGreen is the red→green proof: tests fail on the base, pass on the candidate.
	GateCheckRedGreen = "red-green"
	// GateCheckTestsRed is the tests-red proof: the acceptance tests must FAIL on the candidate.
	GateCheckTestsRed = "tests-red"
	// GateCheckMetric is a metric comparison (e.g. mutation>=0.8) graded against a threshold.
	GateCheckMetric = "metric"
)
