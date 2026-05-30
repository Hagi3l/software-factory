package views

import (
	"fmt"
	"strings"
)

// shortHash renders a content address compactly for display: the digest without its
// algorithm prefix, truncated, so a long sha256 reads as a glanceable token. The full
// hash still drives the link (the evidence anchor's href), so this is purely cosmetic —
// it is a plain string helper, not a class helper, so keeping it in Go is fine (the
// Tailwind scanner only needs to see class literals, which live in the templ markup).
func shortHash(h string) string {
	if h == "" {
		return ""
	}
	digest := h
	if _, after, ok := strings.Cut(h, ":"); ok {
		digest = after
	}
	if len(digest) > 12 {
		digest = digest[:12] + "…"
	}
	return digest
}

// formatUSD renders cumulative spend. Spend is frequently sub-cent (a single cheap
// invocation), so four decimals keep it from collapsing to a misleading $0.00.
func formatUSD(v float64) string {
	return fmt.Sprintf("$%.4f", v)
}

// orDash renders an em-dash for an empty field so a sparse detail grid reads as
// "absent" rather than as a blank cell that looks like a render bug.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
