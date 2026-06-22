package scanners

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// gosecReport is the subset of `gosec -fmt=json` we parse. The full document also carries
// `Stats` (files/lines/found counts), `GosecVersion`, and `Golang errors` — all of which
// is either run-varying (the version string is build-stamped jitter) or a roll-up of the
// issues we already keep, so none of it reaches a finding. We decode only `Issues`.
type gosecReport struct {
	Issues []gosecIssue `json:"Issues"`
}

// gosecIssue is one SAST hit. gosec reports `line` and `column` as *strings* (and `line`
// can be a range like "11-13"), so they are decoded as strings and normalized here. The
// `code` field is a small source excerpt around the hit — the one gosec-specific essential
// a one-line message buries: it shows the agent exactly which expression tripped the rule
// (which call, which import) without it re-reading the file, so we preserve it verbatim in
// Detail.
type gosecIssue struct {
	Severity string `json:"severity"`
	RuleID   string `json:"rule_id"`
	Details  string `json:"details"`
	File     string `json:"file"`
	Code     string `json:"code"`
	Line     string `json:"line"`
}

// ParseGosec parses `gosec -fmt=json` output into findings: one per issue,
// `{File, Line, Rule=rule_id, Severity=severity lowercased, Message=details, Detail=code
// snippet}`. The code excerpt is kept in Detail because it is the tool-specific signal a
// bare "Use of weak cryptographic primitive" message loses — the agent sees the offending
// line without another file read. gosec's severity (MEDIUM/HIGH/LOW) is lowercased to the
// finding convention.
//
// Robustness: a non-JSON or truncated blob yields empty findings — the gate still grades
// on the exit code (gosec exits non-zero on any finding or tool error, both fail closed).
func ParseGosec(raw []byte) core.Findings {
	var rep gosecReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil
	}
	findings := make(core.Findings, 0, len(rep.Issues))
	for _, iss := range rep.Issues {
		findings = append(findings, core.Finding{
			File:     iss.File,
			Line:     parseGosecLine(iss.Line),
			Severity: strings.ToLower(strings.TrimSpace(iss.Severity)),
			Rule:     iss.RuleID,
			Message:  strings.TrimSpace(iss.Details),
			Detail:   strings.TrimRight(iss.Code, "\n"),
		})
	}
	return findings
}

// parseGosecLine turns gosec's string line field into an int. gosec may emit a range
// ("11-13") for a multi-line construct; we take the first line (where the fix begins). An
// unparseable value yields 0 (location-less but still a valid finding).
func parseGosecLine(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
