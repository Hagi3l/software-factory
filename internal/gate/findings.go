package gate

import (
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gotest"
	"github.com/Loxstomper/harness/internal/scanners"
)

// The kernel-shipped per-tool finding adapters, keyed by the registry name of the check
// whose command runs the tool. The gate grades a command on its exit code (tool-agnostic),
// but it *also* parses each tool's machine-readable output into structured findings — the
// compact, signal-dense form that enters the verdict, the verification view, and a retry
// Brief instead of the raw multi-thousand-line dump (see specs/verification.md "Findings:
// structured evidence, not the grade"). The extraction is built-in infra, not a persona
// nudge, and selected by the check's identity, so the registry stays a plain name→command
// map and a deployment adds a scanner without writing a parser. These names are the kernel's
// canonical check keys (specs/configuration.md, specs/verification.md "Independent
// scanners"); a deployment that names a check differently simply gets the graceful fallback
// below.
const (
	checkGolangciLint = "golangci-lint"
	checkGosec        = "gosec"
	checkGovulncheck  = "govulncheck"
	checkLicenseScan  = "license-scan"
)

// adapterFor returns the per-tool finding parser for a check, or nil when no kernel adapter
// matches (the graceful fallback: the check still grades on its exit code with its raw
// output as evidence — it just carries no structured findings). The proofs (red→green,
// tests-red) reuse the acceptance-test command — `go test` — so their candidate output
// parses with the go-test adapter regardless of the proof's own check name; that is why the
// kind is matched before the name.
func adapterFor(check Check) func([]byte) core.Findings {
	switch check.kind {
	case redGreenProof, redProof:
		return goTestFindings
	}
	switch check.Name {
	case core.CheckAcceptanceTests:
		return goTestFindings
	case checkGolangciLint:
		return scanners.ParseGolangciLint
	case checkGosec:
		return scanners.ParseGosec
	case checkGovulncheck:
		return scanners.ParseGovulncheck
	case checkLicenseScan:
		return scanners.ParseLicenseScan
	}
	return nil
}

// goTestFindings runs the go-test adapter, but only when the output is ndjson (the shape
// `go test -json` emits). A check configured with a *human-format* test command (e.g. a
// `make` target that prints the plain `ok / FAIL` summary, or routes `-json` to a file) is
// graded identically by the gate — on its exit code — but its output is not machine-readable,
// and the parser's compile-failure path would misread that plain text as a single bogus
// "build" finding. The guard keeps the fallback honest: non-machine-readable output yields no
// findings, never a fabricated one. A genuine build failure at the gate is surfaced by the
// build precondition (T9.4), not here, so nothing is lost by declining to guess.
func goTestFindings(out []byte) core.Findings {
	if !looksLikeNDJSON(out) {
		return nil
	}
	return gotest.Parse(out)
}

// looksLikeNDJSON reports whether the first non-whitespace byte is '{' — a cheap, allocation-
// free test for the `go test -json` stream shape (every line is a JSON object). It is
// deliberately lenient (it does not validate the whole stream) because gotest.Parse already
// tolerates a JSON stream with a trailing non-JSON build block; it only rejects output that
// is plainly not JSON at all.
func looksLikeNDJSON(out []byte) bool {
	for _, b := range out {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
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
