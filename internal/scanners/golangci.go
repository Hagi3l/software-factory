// Package scanners holds the per-tool finding adapters for the spec-independent gate
// scanners — golangci-lint, gosec, govulncheck, license-scan. Each adapter is a pure
// parser: it takes a scanner's machine-readable output ([]byte) and returns
// core.Findings, the compact, signal-dense form that enters an agent's context, the gate
// verdict, and the verification view instead of the raw multi-thousand-line dump (see
// specs/verification.md "Findings: structured evidence, not the grade" and Phase 9 of
// IMPLEMENTATION_PLAN.md).
//
// These adapters are additive, not the grade: a check still passes/fails on its tool's
// exit code, and a command with no adapter still grades on its exit code with the raw
// output as evidence. So every parser here is total — it never panics and degrades to
// "what it could parse, or empty" on a truncated or non-JSON blob, because a malformed
// scanner report must not break a gate that already has the exit-code verdict.
//
// Two properties every parser holds, both load-bearing for prefix caching and the
// "findings not shrinking across attempts" signal:
//   - Determinism: identical input yields findings whose Format() is byte-identical
//     across re-parses (the canonical order lives in core.Findings.Format; the parsers
//     just never inject run-varying text).
//   - Jitter stripping: no timestamps, elapsed times, scanner version strings, pids, or
//     run-varying log prefixes ever reach a Finding. The raw dump keeps that noise; the
//     findings carry only the durable signal.
package scanners

import (
	"encoding/json"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// golangciReport is the subset of golangci-lint's `--output.json.path stdout` document we
// need. The full document also carries a large `Report.Linters` block (every linter, with
// its enabled state) which is pure configuration noise — none of it is a finding, so we
// drop it. We decode only `Issues`.
type golangciReport struct {
	Issues []golangciIssue `json:"Issues"`
}

// golangciIssue is one lint issue. golangci-lint nests the location under `Pos` and names
// the firing linter `FromLinter`; `Text` is the human message and `Severity` is the
// (often empty) per-linter severity. `SourceLines`/`Replacement`/CWE-style fields are not
// carried — the file:line + linter + message is the whole signal a fix needs.
type golangciIssue struct {
	FromLinter string `json:"FromLinter"`
	Text       string `json:"Text"`
	Severity   string `json:"Severity"`
	Pos        struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
	} `json:"Pos"`
}

// ParseGolangciLint parses golangci-lint's JSON output (`--output.json.path stdout`) into
// findings: one per issue, `{File, Line, Rule=linter name, Message=issue text,
// Severity}`. The linter name goes in Rule (it is the stable identifier of what fired);
// the issue text — the actionable sentence — is the Message. There is no Detail: a lint
// issue's whole signal is its one-line text, so adding a verbatim Detail would only
// re-state it.
//
// Robustness: golangci-lint prints a trailing human "N issues." line after the JSON
// object on stdout, so the byte slice is not a single clean JSON document. We decode only
// the first JSON value via a streaming decoder and ignore any trailing tokens; a blob with
// no decodable object at all yields empty findings (the gate still has the exit code).
func ParseGolangciLint(raw []byte) core.Findings {
	var rep golangciReport
	// A streaming decoder stops after the first complete value, so the trailing
	// "N issues." line (not valid JSON) does not fail the parse. A genuinely malformed or
	// empty blob leaves rep zero-valued, and we return no findings.
	if err := json.NewDecoder(strings.NewReader(strings.TrimSpace(string(raw)))).Decode(&rep); err != nil {
		return nil
	}
	findings := make(core.Findings, 0, len(rep.Issues))
	for _, iss := range rep.Issues {
		findings = append(findings, core.Finding{
			File:     iss.Pos.Filename,
			Line:     iss.Pos.Line,
			Severity: strings.ToLower(strings.TrimSpace(iss.Severity)),
			Rule:     iss.FromLinter,
			Message:  strings.TrimSpace(iss.Text),
		})
	}
	return findings
}
