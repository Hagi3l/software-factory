package scanners

import (
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// govulncheck `-json` is not one document but a stream of concatenated JSON messages, each
// a single-key envelope: {"config":...}, {"SBOM":...}, {"progress":...}, {"osv":...},
// {"finding":...}. The osv messages carry the human description of a vulnerability (id,
// aliases, summary); the finding messages carry where it surfaces in the scanned code,
// reported at up to three increasingly specific levels — module-only, package-level, and
// symbol-level (a finding whose trace's top frame has a `function`). Only a symbol-level
// finding means the vulnerable code is actually *called*; module/package-only findings are
// "informational" (typically stdlib vulns reachable in principle but not on a called
// path). govulncheck's own text report makes exactly this split ("Vulnerability #N:
// called" vs "informational"), and it is the signal: a called vuln is actionable, an
// informational one is noise in an agent's context. So this adapter emits one finding per
// *called* OSV and drops the informational ones — that omission is the point of structured
// findings, not a loss.
type govulnMessage struct {
	OSV     *govulnOSV     `json:"osv"`
	Finding *govulnFinding `json:"finding"`
}

type govulnOSV struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases"`
	Summary string   `json:"summary"`
}

type govulnFinding struct {
	OSV          string             `json:"osv"`
	FixedVersion string             `json:"fixed_version"`
	Trace        []govulnTraceFrame `json:"trace"`
}

type govulnTraceFrame struct {
	Module   string     `json:"module"`
	Version  string     `json:"version"`
	Package  string     `json:"package"`
	Function string     `json:"function"`
	Position *govulnPos `json:"position"`
}

type govulnPos struct {
	Filename string `json:"filename"`
	Line     int    `json:"line"`
}

// ParseGovulncheck parses the govulncheck `-json` message stream into findings: one per
// *called* vulnerability, `{Rule=GO id, Message=OSV summary, Detail=call path + fix,
// File:Line of the call site, Severity="high"}`.
//
//   - Rule is the durable GO id (e.g. GO-2021-0113); any CVE/GHSA aliases ride along in
//     Detail so the agent can cross-reference without them polluting the sort key.
//   - File:Line is the call *site* in the scanned code — the last frame of the trace, the
//     user's own code — so the finding points where a fix lands, not at the library's
//     internals.
//   - Detail preserves the call path (caller -> vulnerable symbol) and the fixed version.
//     This is the govulncheck-specific essential a one-line summary buries: knowing a vuln
//     exists is useless without knowing which of your call sites reaches it and what to
//     upgrade to. The path is rendered deepest-call-last and carries no offsets/columns
//     (run-stable text only).
//   - Severity is set to "high": govulncheck only reports vulnerabilities it considers
//     applicable, and the JSON stream carries no per-finding severity field, so a single
//     stable level keeps Format() deterministic rather than inventing run-varying text.
//
// Robustness: the stream is decoded message-by-message; a truncated tail (a half-written
// final object) stops the scan but keeps every complete message already read, so a cut-off
// dump still yields the findings it managed to emit. A wholly non-JSON blob yields empty.
func ParseGovulncheck(raw []byte) core.Findings {
	dec := json.NewDecoder(strings.NewReader(string(raw)))

	summaries := map[string]govulnOSV{}
	// called maps an OSV id to its symbol-level finding (the actionable one). If govulncheck
	// emits several symbol-level findings for one OSV we keep the first — they share the same
	// vulnerability, and the canonical Format() ordering makes the choice deterministic
	// regardless.
	called := map[string]govulnFinding{}

	for {
		var msg govulnMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			// A malformed/truncated message ends the stream; everything decoded so far stands.
			break
		}
		switch {
		case msg.OSV != nil && msg.OSV.ID != "":
			summaries[msg.OSV.ID] = *msg.OSV
		case msg.Finding != nil && msg.Finding.OSV != "":
			if isCalled(msg.Finding.Trace) {
				if _, seen := called[msg.Finding.OSV]; !seen {
					called[msg.Finding.OSV] = *msg.Finding
				}
			}
		}
	}

	findings := make(core.Findings, 0, len(called))
	for id, f := range called {
		osv := summaries[id]
		site := callSite(f.Trace)
		findings = append(findings, core.Finding{
			File:     site.Filename,
			Line:     site.Line,
			Severity: "high",
			Rule:     id,
			Message:  osv.Summary,
			Detail:   govulnDetail(osv, f),
		})
	}
	return findings
}

// isCalled reports whether a trace is symbol-level — its top (vulnerable) frame names a
// function. Module- and package-level findings (informational) have no function there.
func isCalled(trace []govulnTraceFrame) bool {
	return len(trace) > 0 && trace[0].Function != ""
}

// callSite returns the position of the scanned code that reaches the vulnerability — the
// last frame of the trace (govulncheck orders traces vulnerable-symbol-first,
// caller-last), so the deepest caller is the user's own call site. A frame without a
// position yields a zero value (a location-less but still valid finding).
func callSite(trace []govulnTraceFrame) govulnPos {
	for i := len(trace) - 1; i >= 0; i-- {
		if trace[i].Position != nil {
			return *trace[i].Position
		}
	}
	return govulnPos{}
}

// govulnDetail renders the verbatim-essential block: the CVE/GHSA aliases, the fix, and
// the call path. It is deterministic (no offsets, columns, or timestamps) so an unchanged
// re-scan yields byte-identical Detail.
func govulnDetail(osv govulnOSV, f govulnFinding) string {
	var b strings.Builder
	if len(osv.Aliases) > 0 {
		aliases := append([]string(nil), osv.Aliases...)
		sort.Strings(aliases)
		b.WriteString("aliases: ")
		b.WriteString(strings.Join(aliases, ", "))
		b.WriteByte('\n')
	}
	if f.FixedVersion != "" {
		b.WriteString("fixed in: ")
		b.WriteString(f.FixedVersion)
		b.WriteByte('\n')
	}
	if path := callPath(f.Trace); path != "" {
		b.WriteString("call path: ")
		b.WriteString(path)
	}
	return strings.TrimRight(b.String(), "\n")
}

// callPath renders the trace caller-first, vulnerable-symbol-last (the reading order a
// human expects: "my code -> ... -> the vulnerable function"), using package.function for
// each frame. Frames without a function are skipped (they carry no symbol to name).
func callPath(trace []govulnTraceFrame) string {
	var parts []string
	for i := len(trace) - 1; i >= 0; i-- {
		fr := trace[i]
		if fr.Function == "" {
			continue
		}
		name := fr.Function
		if fr.Package != "" {
			name = fr.Package + "." + fr.Function
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " -> ")
}
