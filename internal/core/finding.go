package core

import (
	"sort"
	"strconv"
	"strings"
)

// Finding is a single structured result parsed from a gate check's tool output — the
// language-neutral shape every per-tool adapter (a `go test -json` parser, a
// `gosec -fmt=json` parser) emits. It is the compact, signal-dense form of a check's raw
// output: the raw dump still travels as gate evidence (by hash), but the *findings* are
// what enter the agent's context, the gate verdict, the verification view, and a failed
// candidate's retry Brief (see specs/verification.md "Findings: structured evidence, not
// the grade", specs/glossary.md "Finding").
//
// The shape is fixed; the parsing lives in a per-tool adapter, the same
// canonical-interface / thin-adapter split the model layer and the semantic tools use
// (provider adapter : model :: check adapter : tool output). Findings are *evidence*, not
// the grade — pass/fail stays the check's exit code / proof / metric, so the gate stays
// agnostic to any one tool's report.
type Finding struct {
	// File is the source file the finding points at, repo-relative, or "" when the tool
	// reports no location (a build failure, a panic with only a stack, a project-wide
	// vulnerability). Line is the 1-based line within File, or 0 when unknown.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// Severity is the tool's own severity spelling normalized to lowercase (e.g. "error",
	// "high", "warning"), or "" when the tool grades without one (a plain test failure).
	Severity string `json:"severity,omitempty"`
	// Rule is the stable identifier of the rule/check that fired (a linter rule name, a
	// CVE/GO id, a failing test name), or "" when the tool has none.
	Rule string `json:"rule,omitempty"`
	// Message is the one-line human summary — always present; it is the finding's signal.
	Message string `json:"message"`
	// Detail preserves the one tool-specific essential that a one-line message cannot: a
	// test's assertion diff, a vulnerability's call path, a data-race stanza. It is free
	// text (possibly multi-line) kept verbatim so the agent sees what matters, or "" when
	// the message says everything. It must carry no run-to-run jitter (elapsed times,
	// timestamps) — that is stripped at parse time so an unchanged re-run yields
	// byte-identical findings.
	Detail string `json:"detail,omitempty"`
}

// Findings is an ordered set of findings with a deterministic, compact rendering. It is a
// distinct type (not a bare []Finding) so the cache-stable Format below is the one way a
// findings set reaches an agent's context or a verdict — there is no second, divergent
// renderer.
type Findings []Finding

// Format renders the findings as a compact, deterministic, cache-stable block for an
// agent's context (or a verdict's textual summary). It sorts a copy of the set into a
// canonical order so that an unchanged re-run yields byte-identical output — which keeps
// the conversation prefix cacheable and lets a "findings not shrinking across attempts"
// signal mean what it says (specs/verification.md). It assumes jitter (timestamps,
// elapsed times) was already stripped at parse time; sorting handles only ordering jitter
// (the order tools emit findings in is not stable).
//
// Each finding renders as one head line — `file:line [severity] rule: message`, with
// every empty component dropped so a locationless or ruleless finding stays clean —
// followed by its Detail indented under it. An empty set renders as "".
func (fs Findings) Format() string {
	if len(fs) == 0 {
		return ""
	}
	sorted := make(Findings, len(fs))
	copy(sorted, fs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].less(sorted[j]) })

	var b strings.Builder
	for i, f := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f.head())
		if f.Detail != "" {
			for _, line := range strings.Split(strings.TrimRight(f.Detail, "\n"), "\n") {
				b.WriteString("\n    ")
				b.WriteString(line)
			}
		}
	}
	return b.String()
}

// head renders a finding's single summary line: location, severity, rule, message, with
// any empty component omitted so the line never carries dangling separators.
func (f Finding) head() string {
	var parts []string
	if loc := f.location(); loc != "" {
		parts = append(parts, loc)
	}
	if f.Severity != "" {
		parts = append(parts, "["+f.Severity+"]")
	}
	switch {
	case f.Rule != "" && f.Message != "":
		parts = append(parts, f.Rule+": "+f.Message)
	case f.Rule != "":
		parts = append(parts, f.Rule)
	case f.Message != "":
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, " ")
}

// location renders the file:line anchor, dropping the line when it is unknown and the
// whole anchor when the file is.
func (f Finding) location() string {
	if f.File == "" {
		return ""
	}
	if f.Line > 0 {
		return f.File + ":" + strconv.Itoa(f.Line)
	}
	return f.File
}

// less is the canonical sort order: file, then line, then rule, then severity, then
// message. It is a total order over the fields so the sort is deterministic regardless of
// the order the parser emitted findings in.
func (f Finding) less(o Finding) bool {
	if f.File != o.File {
		return f.File < o.File
	}
	if f.Line != o.Line {
		return f.Line < o.Line
	}
	if f.Rule != o.Rule {
		return f.Rule < o.Rule
	}
	if f.Severity != o.Severity {
		return f.Severity < o.Severity
	}
	return f.Message < o.Message
}
