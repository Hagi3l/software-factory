package core

import (
	"strconv"
	"strings"
)

// Condition identifiers that are shared conventions between config validation (which
// accepts them as known postconditions) and the gate (which realizes them as checks).
// They live in core so neither config nor gate owns the spelling — the same reason
// CandidateBranch does — keeping the config-time and run-time halves of the
// postcondition→check bridge in agreement (see specs/configuration.md, specs/verification.md).
const (
	// PostconditionRedGreen is the reserved red→green proof: the gate runs the
	// acceptance tests against the pre-implementation base (which must FAIL) and the
	// candidate (which must PASS), proving the tests are not vacuously green and
	// actually exercise the new behavior (see specs/verification.md "Red→green proof").
	// Unlike a command-check postcondition it has no `checks` entry of its own; it
	// reuses the CheckAcceptanceTests command, run against both refs.
	PostconditionRedGreen = "tests-red-then-green"

	// PostconditionTestsRed is the reserved "tests must be red" proof for the
	// author-tests stage: the gate runs the acceptance tests against the stage's own
	// candidate, which must FAIL because no implementation exists yet. It proves the
	// test author produced real, executing acceptance tests that genuinely fail — not a
	// vacuous, always-green, or non-compiling suite — so a wasted implement attempt is
	// never spent on a bad test candidate (producer ≠ verifier applied to the author
	// stage; see specs/verification.md, specs/workflow.md). Like the red→green proof it
	// has no `checks` entry of its own — it reuses the CheckAcceptanceTests command, run
	// once against the candidate — and is the natural complement to the implementor's
	// red→green proof, which later re-confirms the same candidate is red as its base.
	PostconditionTestsRed = "tests-red"

	// CheckAcceptanceTests is the registry key for the project's acceptance-test
	// command (e.g. `go test ./...`). It is the command a command-check stage runs to
	// grade a candidate green, and the same command the red→green and tests-red proofs
	// run. Keeping the acceptance tests under one key makes that command a single source
	// of truth shared by the `qa` stage's `tests-pass` check, the `implement` stage's
	// red→green proof, and the `author-tests` stage's tests-red proof (see
	// specs/configuration.md).
	CheckAcceptanceTests = "tests-pass"

	// PostconditionHumanApproved is the reserved postcondition that holds only when a
	// human has explicitly approved the issue's CURRENT candidate. Unlike every other
	// postcondition it is evaluated by the ORCHESTRATOR, not run as a check in the
	// verification sandbox: it reads orchestrator/beads state (an approval recorded on the
	// issue, bound to the candidate sha), not the repository, so it carries no `checks`
	// entry. It is satisfied by `harness approve <issue>` (denied by `harness reject
	// <issue>`); the approval is bound to the candidate sha so a re-gate after a change
	// invalidates a stale one. It fails CLOSED and, unlike a command/proof/metric check,
	// its failure does NOT route on_failure (it burns no retry) — the issue parks in an
	// awaiting-approval escalation until a human approves (→ merge) or rejects (→ fix /
	// back to spec). It is the gate that realizes the trusted-dev transition and the
	// permanent TCB-review boundary (see specs/configuration.md, specs/bootstrap.md). It is
	// valid only on the integrate (trusted-merge) stage, which is where a produced
	// candidate exists to approve.
	PostconditionHumanApproved = "human-approved"

	// MetricMutation is the metric usable on the left of a comparison postcondition such
	// as "mutation>=0.8": the mutation score a mutation-testing tool achieves against the
	// candidate (the fraction of injected faults the acceptance tests caught). Mutation
	// testing mechanically attacks the "weak test" problem — tests that pass but assert
	// nothing meaningful — and is, with the red→green proof, one of the two first-class
	// postconditions that make no-human-review defensible (see specs/verification.md).
	//
	// A metric comparison resolves its measurement command from the check registry under
	// this same key (the way a command-check postcondition resolves under its own name):
	// "mutation>=0.8" runs checks["mutation"] in the verification sandbox, and the gate
	// compares the score that command prints against the threshold. Keeping the tool
	// invocation in config — not hardcoded in the gate — keeps the gate tool-agnostic: it
	// reads a number, not a gremlins report. The threshold lives in the postcondition
	// expression itself, so "what score gates" is config, not code.
	MetricMutation = "mutation"
)

// ComparisonOps are the operators recognized in a metric-comparison postcondition such
// as "mutation>=0.8". They are ordered longest-first so ">=" is matched before ">" when
// scanning a postcondition string. Shared by config validation and the gate so the two
// agree on what a comparison looks like.
var ComparisonOps = []string{">=", "<=", "==", ">", "<"}

// ParseMetricComparison splits a metric-comparison postcondition "<metric><op><threshold>"
// (e.g. "mutation>=0.8") into its parts. ok is false when s is not a well-formed
// comparison — no recognized operator, an empty metric, or a non-numeric threshold — so a
// caller can fall through to treating s as a bare identifier (a reserved proof or a
// command-check name). It does not judge whether the metric is *known*; that is config's
// concern (an unknown metric is a validation error), keeping the gate generic over
// whatever the validated config asks for.
func ParseMetricComparison(s string) (metric, op string, threshold float64, ok bool) {
	for _, o := range ComparisonOps {
		if i := strings.Index(s, o); i > 0 {
			metric = strings.TrimSpace(s[:i])
			v, err := strconv.ParseFloat(strings.TrimSpace(s[i+len(o):]), 64)
			if err != nil {
				return "", "", 0, false
			}
			return metric, o, v, true
		}
	}
	return "", "", 0, false
}

// CompareMetric reports whether `value <op> threshold` holds for one of ComparisonOps. An
// unrecognized operator returns false (fail-closed): the gate only reaches this with an op
// ParseMetricComparison produced, so an unknown one would be a programming error, and a
// metric check that cannot be evaluated must never be treated as passing.
func CompareMetric(value float64, op string, threshold float64) bool {
	switch op {
	case ">=":
		return value >= threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	case ">":
		return value > threshold
	case "<":
		return value < threshold
	}
	return false
}
