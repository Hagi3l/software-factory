// Package checkfindings is the single source for selecting a gate check's per-tool finding
// adapter by the check's registry name. It is imported by *both* the verification gate
// (which parses a check's output into the recorded verdict) and the producer's `run_gate`
// self-check tool (which parses the same checks in the untrusted sandbox before submit) —
// so "the gate checks it" and "I checked it" resolve not just one command but one parser and
// one finding shape (specs/verification.md "Producer self-checks are feedback, not grades").
// It is a dependency-free leaf over core + the per-tool parsers, so neither caller depends on
// the other.
package checkfindings

import (
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gotest"
	"github.com/Loxstomper/harness/internal/scanners"
)

// The kernel's canonical scanner check names — the registry keys whose commands the kernel
// ships a finding adapter for (specs/configuration.md, specs/verification.md "Independent
// scanners"). The acceptance-test check is core.CheckAcceptanceTests ("tests-pass"). A
// deployment that names a check differently simply gets the graceful nil fallback from ByName.
const (
	GolangciLint = "golangci-lint"
	Gosec        = "gosec"
	Govulncheck  = "govulncheck"
	LicenseScan  = "license-scan"
)

// ByName returns the kernel finding adapter for a check's registry name, or nil when no
// adapter matches (the graceful fallback: the check still grades on its exit code with its
// raw output as evidence — it just carries no structured findings).
func ByName(name string) func([]byte) core.Findings {
	switch name {
	case core.CheckAcceptanceTests:
		return GoTest
	case GolangciLint:
		return scanners.ParseGolangciLint
	case Gosec:
		return scanners.ParseGosec
	case Govulncheck:
		return scanners.ParseGovulncheck
	case LicenseScan:
		return scanners.ParseLicenseScan
	}
	return nil
}

// GoTest runs the go-test adapter, but only when the output is ndjson (the shape
// `go test -json` emits). A check configured with a *human-format* test command (e.g. a
// `make` target that prints the plain `ok / FAIL` summary, or routes `-json` to a file) is
// graded identically — on its exit code — but its output is not machine-readable, and
// gotest.Parse's compile-failure path would misread that plain text as a single bogus
// "build" finding. The guard keeps the fallback honest: non-machine-readable output yields no
// findings, never a fabricated one. A genuine build failure at the gate is surfaced by the
// gate's build precondition, not here, so nothing is lost by declining to guess.
func GoTest(out []byte) core.Findings {
	if !looksLikeNDJSON(out) {
		return nil
	}
	return gotest.Parse(out)
}

// Parse runs the adapter for name over a check's captured output: stdout is parsed (the
// machine-readable streams the adapters expect — `go test -json`, `gosec -fmt=json`, … — go
// to stdout), with stderr the fallback only when stdout is empty (a tool that wrote
// everything to stderr). A name with no adapter, or output the adapter cannot parse, yields
// nil — never an error — so the raw output still travels as evidence and the exit-code grade
// is unchanged.
func Parse(name string, stdout, stderr []byte) core.Findings {
	parse := ByName(name)
	if parse == nil {
		return nil
	}
	out := stdout
	if len(out) == 0 {
		out = stderr
	}
	return parse(out)
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
