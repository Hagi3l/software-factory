package scanners

import (
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// license-scan is `go-licenses check` (the kernel's configured command). Unlike the other
// three scanners it has no JSON mode: it emits human lines, all on stderr, mixing glog
// diagnostic lines (a timestamped `Emmdd HH:MM:SS.ffffff <pid> file.go:NN] ...` prefix)
// with the actual policy verdicts. The glog lines are pure jitter — timestamp + pid change
// every run — so they are dropped entirely; we key off the three stable verdict templates
// go-licenses prints (confirmed against the installed binary's format strings):
//
//	"<Type> license type <name> found for library <module>"   (e.g. Forbidden / Restricted)
//	"Not allowed license <name> found for library <module>"
//	"Unknown license type  found for library <module>"        (empty name => unknown)
//
// All three end in "found for library <module>", which is the anchor we split on.
const licenseAnchor = " found for library "

// ParseLicenseScan parses `go-licenses check` text output into findings: one per offending
// module, `{Rule=module path, Message=disallowed-license description, Severity="error"}`.
//
//   - Rule is the module path — the stable identifier of *what* violated policy and the
//     unit a fix acts on (drop the dep, or vendor an allowed-license alternative).
//   - Message restates the verdict ("Forbidden license type GPL-3.0", "license not
//     allowed: AGPL-3.0", "unknown license type") so the agent sees which license tripped
//     policy without the raw log.
//   - There is no Detail: a license verdict's whole signal is module + license; the file
//     it lives in is not actionable (you cannot edit a dependency's LICENSE).
//   - Severity is "error": every line go-licenses prints here is a hard policy failure.
//
// The glog header (`E0622 ...] ...`) on the license-not-found diagnostic line is jitter
// and is skipped — only lines containing the verdict anchor become findings. A blob with
// none (a clean scan) yields empty findings; the gate still grades on the exit code.
func ParseLicenseScan(raw []byte) core.Findings {
	var findings core.Findings
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, licenseAnchor)
		if idx < 0 {
			continue
		}
		// Skip a glog-prefixed diagnostic that merely *mentions* a library in prose (the
		// "Failed to find license for <lib>: ..." error). The real verdict lines have no
		// glog prefix; a glog line starts with a severity letter + 4 digits then a space
		// (e.g. "E0622 ").
		if isGlogLine(line) {
			continue
		}
		module := strings.TrimSpace(line[idx+len(licenseAnchor):])
		if module == "" {
			continue
		}
		findings = append(findings, core.Finding{
			Severity: "error",
			Rule:     module,
			Message:  licenseMessage(line[:idx]),
		})
	}
	return findings
}

// licenseMessage normalizes the verdict prefix (everything before " found for library ")
// into a stable one-line description. It recognizes the three known templates and falls
// back to the raw prefix for any future template, so an unrecognized but well-formed line
// still becomes a finding rather than being silently dropped.
func licenseMessage(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	switch {
	case strings.HasPrefix(prefix, "Unknown license type"):
		return "unknown license type"
	case strings.HasPrefix(prefix, "Not allowed license "):
		name := strings.TrimSpace(strings.TrimPrefix(prefix, "Not allowed license"))
		return "license not allowed: " + name
	default:
		// "<Type> license type <name>" => "<type> license: <name>"
		if i := strings.Index(prefix, " license type "); i >= 0 {
			kind := strings.ToLower(strings.TrimSpace(prefix[:i]))
			name := strings.TrimSpace(prefix[i+len(" license type "):])
			return kind + " license: " + name
		}
		return prefix
	}
}

// isGlogLine reports whether a line carries a glog header (a severity letter F/E/W/I
// followed by 4 digits and a space, e.g. "E0622 "), which marks a timestamped diagnostic
// to be dropped rather than a clean verdict.
func isGlogLine(line string) bool {
	if len(line) < 6 {
		return false
	}
	switch line[0] {
	case 'F', 'E', 'W', 'I':
	default:
		return false
	}
	for i := 1; i <= 4; i++ {
		if line[i] < '0' || line[i] > '9' {
			return false
		}
	}
	return line[5] == ' '
}
