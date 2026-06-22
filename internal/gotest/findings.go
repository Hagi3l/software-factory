package gotest

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// locRE matches the `foo_test.go:42:` location prefix Go prints on a t.Error/t.Fatal line
// (indented), and the `./x.go:3:1:` form a compiler diagnostic uses. We capture file and
// line so the finding anchors at the source, which is what the agent and the verification
// view need to navigate — a raw dump leaves the human to eyeball it.
var locRE = regexp.MustCompile(`([\w./\-]+\.go):(\d+)(?::\d+)?:`)

// raceMarker / raceWarning bound a `-race` stanza in a test's output. The interleaving of
// reads/writes IS the evidence, so we keep the whole stanza verbatim (jitter-free given a
// fixed run) rather than summarizing it away.
const (
	raceWarning = "WARNING: DATA RACE"
	raceRule    = "data race"
)

// failureFinding builds the finding for a single failed test from its accumulated output
// lines. It dispatches on the *kind* of failure so each keeps the right signal: a race
// stanza verbatim, a panic/timeout message without its goroutine dump, an assertion with
// its diff. A failed test always yields a finding (a bare t.Fail() still reports), so there
// is no "nothing to report" path.
func failureFinding(test string, lines []string) core.Finding {
	body := strings.Join(lines, "")

	switch {
	case strings.Contains(body, raceWarning):
		return raceFinding(test, lines)
	case panicTimeoutLine(lines) != "":
		return panicTimeoutFinding(test, lines)
	default:
		return assertionFinding(test, lines)
	}
}

// assertionFinding is the common case: a t.Error/t.Fatal failure. The first
// location-bearing line gives File:Line and is the Message; the remaining failure-body
// lines (minus the RUN/PASS/FAIL chrome) are the Detail (the assertion diff). The test
// name is the Rule, so "findings not shrinking" can track a specific test across retries.
func assertionFinding(test string, lines []string) core.Finding {
	f := core.Finding{Rule: test}
	var detail []string
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\n")
		if isChrome(line) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if f.Message == "" {
			f.Message = trimmed
			if file, ln, ok := matchLoc(line); ok {
				f.File, f.Line = file, ln
				// Drop the `file:line:` prefix from the message — the location is already
				// in File/Line, so repeating it is noise in the head line.
				f.Message = strings.TrimSpace(stripLoc(trimmed))
			}
		}
		detail = append(detail, trimmed)
	}
	if f.Message == "" {
		// No printed body (a bare `t.Fail()`): the failure itself is the message.
		f.Message = "test failed"
	}
	// Detail beyond the message earns its place only when it adds something; a one-line
	// failure says everything in the Message, so leave Detail empty there.
	if len(detail) > 1 {
		f.Detail = strings.Join(detail, "\n")
	}
	return f
}

// raceFinding keeps the `WARNING: DATA RACE` stanza verbatim in Detail. The race is the
// finding; its read/write/goroutine interleaving is the one thing a one-line message
// cannot carry, so we preserve exactly the lines between the stanza's `====` rules.
func raceFinding(test string, lines []string) core.Finding {
	stanza := raceStanza(lines)
	f := core.Finding{
		Severity: "error",
		Rule:     raceRule,
		Message:  "data race detected in " + test,
		Detail:   strings.Join(stanza, "\n"),
	}
	// Anchor at the first source location named inside the stanza, when present.
	for _, l := range stanza {
		if file, ln, ok := matchLoc(l); ok {
			f.File, f.Line = file, ln
			break
		}
	}
	return f
}

// raceStanza extracts the verbatim race report: everything from the first `WARNING: DATA
// RACE` to the matching closing `====` rule. Keeping it intact (not per-line filtered) is
// the point — the interleaving must read exactly as the race detector emitted it.
func raceStanza(lines []string) []string {
	var out []string
	in := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\n")
		switch {
		case strings.Contains(line, raceWarning):
			in = true
			out = append(out, line)
		case in:
			out = append(out, line)
			// A `====` rule with the warning already captured closes the stanza; stop at
			// the first close so a second race report (same test) does not double the noise.
			if isRaceRule(line) && len(out) > 1 {
				return out
			}
		}
	}
	return out
}

// panicTimeoutFinding keeps the panic/timeout message and the triggering test but DROPS the
// goroutine dump. The dump is pure noise for the agent (hundreds of runtime frames), while
// the `panic: ...` / `test timed out` line plus the test name is the entire actionable
// signal — exactly the trade a raw dump gets wrong by keeping everything.
func panicTimeoutFinding(test string, lines []string) core.Finding {
	msg := panicTimeoutLine(lines)
	return core.Finding{
		Severity: "error",
		Rule:     test,
		Message:  msg,
		// Detail intentionally empty: the goroutine dump that would go here is dropped.
	}
}

// panicTimeoutLine returns the panic/timeout headline if the output contains one, else "".
// It recognizes a runtime panic, the test-timeout panic, and the `*** Test killed` form.
func panicTimeoutLine(lines []string) string {
	for _, raw := range lines {
		line := strings.TrimSpace(strings.TrimRight(raw, "\n"))
		switch {
		case strings.HasPrefix(line, "panic: test timed out"),
			strings.HasPrefix(line, "panic:"),
			strings.HasPrefix(line, "*** Test killed"),
			strings.Contains(line, "test timed out"):
			return line
		}
	}
	return ""
}

// buildFinding surfaces a structured build failure (build-output events) as one finding.
// The compiler diagnostic — not "FAIL [build failed]" — is the signal, so we anchor at the
// first `file:line` it names and keep the full diagnostic block as Detail. No test ran, so
// there is no test name; Rule is "build".
func buildFinding(lines []string) core.Finding {
	// Drop the leading `# pkg` banner from the head message but keep it in Detail for
	// context; the first real diagnostic line is the message.
	f := core.Finding{Severity: "error", Rule: "build", Message: "build failed"}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if f.Message == "build failed" && strings.TrimSpace(l) != "" {
			f.Message = strings.TrimSpace(l)
			if file, ln, ok := matchLoc(l); ok {
				f.File, f.Line = file, ln
			}
		}
	}
	f.Detail = strings.Join(lines, "\n")
	return f
}

// rawBuildFinding handles a build failure that never made it into test2json's JSON at all
// (a raw `# pkg` / `./x.go:3: undefined: Foo` block on stdout). A non-JSON line must never
// crash the parser; instead it becomes the finding, because that compiler error is the
// signal CLAUDE.md sends a human to the `.stderr` file for.
func rawBuildFinding(lines []string) core.Finding {
	f := core.Finding{Severity: "error", Rule: "build", Message: "build failed"}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if f.Message == "build failed" && strings.TrimSpace(l) != "" {
			f.Message = strings.TrimSpace(l)
			if file, ln, ok := matchLoc(l); ok {
				f.File, f.Line = file, ln
			}
		}
	}
	f.Detail = strings.Join(lines, "\n")
	return f
}

// isChrome reports whether a line is go test's structural noise (RUN/PASS/FAIL/CONT/PAUSE
// markers) rather than failure content. Stripping it is what makes a finding signal-dense.
func isChrome(line string) bool {
	t := strings.TrimSpace(line)
	for _, p := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- PASS", "--- FAIL", "--- SKIP", "PASS", "FAIL", "ok ", "ok\t"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// isRaceRule reports whether a line is the `==================` rule that brackets a race
// stanza.
func isRaceRule(line string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= 4 && strings.Trim(t, "=") == ""
}

// matchLoc extracts a `file.go:line` location from a line, if any.
func matchLoc(line string) (file string, lineNo int, ok bool) {
	m := locRE.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return strings.TrimPrefix(m[1], "./"), n, true
}

// stripLoc removes the leading `file.go:line:` (or `file.go:line:col:`) prefix from a
// message, leaving just the human text.
func stripLoc(s string) string {
	return locRE.ReplaceAllString(s, "")
}
