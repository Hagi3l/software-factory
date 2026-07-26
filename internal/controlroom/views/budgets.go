package views

import (
	"strconv"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/query"
)

// These are text helpers for the budgets view — number/duration/cap formatting only, never
// CSS class strings. The Tailwind scanner only reads the .templ files (and their generated
// _templ.go), so all tinting lives in templ switches; a class literal here would never be
// compiled into the stylesheet. See views/budgets.templ.

// formatTokens renders a token count with thousands separators so a six-figure burn reads at
// a glance (123,456 not 123456). Spend is the headline number on this view, so legibility
// earns the few lines.
func formatTokens(n int) string {
	s := strconv.Itoa(n)
	neg := ""
	if n < 0 {
		neg, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return neg + s
	}
	// Walk from the right inserting a comma every three digits.
	var b []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, c)
	}
	return neg + string(b)
}

// formatDuration renders a wall-clock burn compactly. Zero reads as a literal "0s" (a real
// no-burn cell), distinct from an uncapped ceiling which capDur renders as ∞.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	// Round to the second above a second — sub-second precision is noise for a budget view.
	if d >= time.Second {
		d = d.Round(time.Second)
	}
	return d.String()
}

// capTokens / capUSD / capDur / capRetries render a configured ceiling, showing ∞ for an
// uncapped (zero) dimension so a row reads "1,234 / ∞" rather than the misleading "1,234 / 0".
func capTokens(cap int) string {
	if cap <= 0 {
		return "∞"
	}
	return formatTokens(cap)
}

func capUSD(cap float64) string {
	if cap <= 0 {
		return "∞"
	}
	return formatUSD(cap)
}

func capDur(cap time.Duration) string {
	if cap <= 0 {
		return "∞"
	}
	return formatDuration(cap)
}

func capRetries(cap int) string {
	if cap <= 0 {
		return "∞"
	}
	return strconv.Itoa(cap)
}

// capsSummary is the one-line ceiling banner for the page header, so a human sees the limits
// the bars are measured against without reading the config. Uncapped dimensions are omitted
// to keep it short; an all-uncapped policy reads as "no budget caps configured".
func capsSummary(c query.BudgetCaps) string {
	var issue []string
	if c.IssueTokens > 0 {
		issue = append(issue, formatTokens(c.IssueTokens)+" tok")
	}
	if c.IssueUSD > 0 {
		issue = append(issue, formatUSD(c.IssueUSD))
	}
	if c.IssueWall > 0 {
		issue = append(issue, formatDuration(c.IssueWall))
	}
	if c.MaxRetries > 0 {
		issue = append(issue, strconv.Itoa(c.MaxRetries)+" retries")
	}
	var epic []string
	if c.EpicTokens > 0 {
		epic = append(epic, formatTokens(c.EpicTokens)+" tok")
	}
	if c.EpicUSD > 0 {
		epic = append(epic, formatUSD(c.EpicUSD))
	}

	parts := make([]string, 0, 2)
	if len(issue) > 0 {
		parts = append(parts, "issue ≤ "+join(issue))
	}
	if len(epic) > 0 {
		parts = append(parts, "epic ≤ "+join(epic))
	}
	if len(parts) == 0 {
		return "no budget caps configured"
	}
	return join2(parts, "  ·  ")
}

// tokenBreakdown renders the in/out/cache split behind a token total as a compact secondary
// line (e.g. "120,000 in · 1,500 out · 1,400 cached"). It is the detail behind the Tokens
// meter — what lets an operator see *why* a burn is high: input dominating with no cached
// tokens is the signature of uncached context re-transmission. Cache read and creation are
// folded into one "cached" figure (both are prompt-cache traffic billed at reduced rates);
// a kind with zero tokens is dropped so a no-cache row stays "N in · N out". Returns "" when
// there is no breakdown to show (a fresh issue), so the caller renders nothing.
func tokenBreakdown(in, out, cacheRead, cacheCreate int) string {
	var parts []string
	if in > 0 {
		parts = append(parts, formatTokens(in)+" in")
	}
	if out > 0 {
		parts = append(parts, formatTokens(out)+" out")
	}
	if c := cacheRead + cacheCreate; c > 0 {
		parts = append(parts, formatTokens(c)+" cached")
	}
	return join(parts)
}

func join(parts []string) string { return join2(parts, " · ") }
func join2(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
