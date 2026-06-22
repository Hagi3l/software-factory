package gate

import (
	"github.com/Loxstomper/harness/internal/checkfindings"
	"github.com/Loxstomper/harness/internal/core"
)

// adapterFor returns the per-tool finding parser for a check, or nil when no kernel adapter
// matches (the graceful fallback: the check still grades on its exit code with its raw
// output as evidence — it just carries no structured findings). The proofs (red→green,
// tests-red) reuse the acceptance-test command — `go test` — so their candidate output
// parses with the go-test adapter regardless of the proof's own check name; that is why the
// kind is matched before the name. The adapter selection itself lives in the shared
// internal/checkfindings leaf, so the gate and the producer's run_gate self-check resolve the
// identical parser for a check (specs/verification.md "Producer self-checks").
func adapterFor(check Check) func([]byte) core.Findings {
	switch check.kind {
	case redGreenProof, redProof:
		return checkfindings.GoTest
	}
	return checkfindings.ByName(check.Name)
}

// findingsFor parses a check's captured output into structured findings with the check's
// per-tool adapter. The machine-readable output the adapters expect (`go test -json`,
// `gosec -fmt=json`, …) goes to stdout, so stdout is parsed; stderr is the fallback only
// when stdout is empty (a tool that wrote everything to stderr, e.g. a build that never got
// to emit json). A check with no adapter, or whose output the adapter cannot parse (the
// command was not run in machine-readable mode), yields no findings — never an error — so
// the raw output still travels as gate evidence and the exit-code grade is unchanged.
func findingsFor(check Check, stdout, stderr []byte) core.Findings {
	parse := adapterFor(check)
	if parse == nil {
		return nil
	}
	out := stdout
	if len(out) == 0 {
		out = stderr
	}
	return parse(out)
}
