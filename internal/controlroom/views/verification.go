package views

import (
	"strconv"

	"github.com/Loxstomper/harness/internal/core"
)

// checkKindLabel renders a gate-check kind as a human phrase. The serialized kinds
// (core.GateCheckCommand/RedGreen/TestsRed/Metric) are terse wire tokens; this is the
// only place they become legible, kept as a plain text helper (no class literals) so it
// stays out of the Tailwind scanner's way. An unknown kind passes through verbatim rather
// than collapsing to a blank, so a forward-added kind still names itself.
func checkKindLabel(kind string) string {
	switch kind {
	case core.GateCheckCommand:
		return "Scanner / command"
	case core.GateCheckRedGreen:
		return "Red→green proof"
	case core.GateCheckTestsRed:
		return "Tests-red proof"
	case core.GateCheckMetric:
		return "Metric"
	default:
		return kind
	}
}

// metricSummary renders a metric check's measured score against its threshold — the
// mutation-score-vs-0.8 line the verification view turns on. An unparsed score (the check
// ran but its output could not be read as a number) is named honestly rather than shown as
// a misleading 0.00, mirroring the "self-describing, never silently blank" posture the gate
// evidence takes (specs/verification.md).
func metricSummary(m *core.GateMetricOutcome) string {
	if m == nil {
		return ""
	}
	if !m.Parsed {
		return "score unparsed (need " + m.Op + " " + fmtScore(m.Threshold) + ")"
	}
	return fmtScore(m.Score) + " " + m.Op + " " + fmtScore(m.Threshold)
}

// fmtScore renders a metric value to two decimals — enough to read a mutation fraction
// (0.82) without the noise of full float precision.
func fmtScore(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// transformFallbackCount counts the transformations (T6.3) that fell back to the text floor — the
// imprecise ones the verification view flags. It is the "how much of this candidate's editing was
// done blind to the AST" number, surfaced in the section header so a reviewer sees it before
// scanning the rows. Semantic transformations (the language server's own edits) are not counted.
func transformFallbackCount(recs []core.TransformRecord) int {
	n := 0
	for _, r := range recs {
		if r.Mechanism == core.TransformMechanismText {
			n++
		}
	}
	return n
}

// transformBlast renders a transformation's blast radius — the files it touched and edits it made —
// as a compact "N files · M edits" string for the row's right rail, so the scale of each change
// reads at a glance alongside its mechanism.
func transformBlast(t core.TransformRecord) string {
	return strconv.Itoa(t.Files) + " files · " + strconv.Itoa(t.Edits) + " edits"
}
