package views

import (
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/Loxstomper/harness/internal/controlroom/query"
)

// cardVTName builds the per-card view-transition-name (T4.18, control-room.md "The board, in
// motion") so the browser's View Transitions API can pair a card across an htmx column
// re-render and tween its move — no client-side animation library. The name is keyed on the
// issue id (the card's stable identity) and prefixed `card-` so it is always a valid CSS
// custom-ident (a bare beads id like a number-leading suffix could otherwise be rejected).
//
// It returns templ.SafeCSS, which bypasses templ's style-attribute sanitizer (the sanitizer
// does not know view-transition-name and would drop it). Because SafeCSS is emitted verbatim,
// the id is reduced to an identifier-safe charset here — beads ids already are, so this is
// defense in depth against an untrusted id ever reaching a style attribute.
func cardVTName(id string) templ.SafeCSS {
	return templ.SafeCSS("view-transition-name: card-" + cssIdent(id))
}

// cssIdent reduces a string to the CSS custom-ident charset (ASCII letters, digits, hyphen,
// underscore), replacing anything else with an underscore. It keeps cardVTName's verbatim
// SafeCSS output injection-free.
func cssIdent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// epochAttr renders a time as its Unix-seconds string for a card data-* timer anchor, or ""
// for the zero time — the orchestrator had not stamped state_entered_at before T4.16, or a
// read missed it. The ticker (ticker.js) treats an empty/absent anchor as "unknown" and shows
// a dash rather than a duration counted from the epoch.
func epochAttr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.Unix(), 10)
}

// leadTime renders a closed card's lead time — how long the issue took from creation to close
// (control-room.md "The board, in motion"). For a closed issue StateEnteredAt is the close
// moment, so the lead time is StateEnteredAt − CreatedAt: a fixed value, rendered statically
// server-side rather than client-ticked (a closed card has no live clock; the work is done).
// Returns "" when either anchor is missing or the span is non-positive (legacy closes never
// stamped state_entered_at), which the card renders as a dash.
func leadTime(card query.IssueCard) string {
	if card.StateEnteredAt.IsZero() || card.CreatedAt.IsZero() {
		return ""
	}
	d := card.StateEnteredAt.Sub(card.CreatedAt)
	if d <= 0 {
		return ""
	}
	return fmtDuration(d)
}

// fmtDuration renders a duration as the same compact h/m/s string the board's client ticker
// uses (ticker.js fmtDuration) — kept in lockstep so a static (closed) card and a live card
// read identically: "45s", "3m12s", "2h05m", "1d04h".
func fmtDuration(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return strconv.Itoa(s) + "s"
	case s < 3600:
		return strconv.Itoa(s/60) + "m" + pad2(s%60) + "s"
	case s < 86400:
		return strconv.Itoa(s/3600) + "h" + pad2((s%3600)/60) + "m"
	default:
		return strconv.Itoa(s/86400) + "d" + pad2((s%86400)/3600) + "h"
	}
}

// pad2 left-pads a sub-unit count to two digits, matching ticker.js's padStart(2,'0').
func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// stateLabel maps a beads status to the word the card's time-in-state timer is prefixed with
// (control-room.md "The board, in motion": working/queued/blocked). It lives server-side so
// the status→label mapping has one home and the Alpine ticker need only advance the duration.
func stateLabel(status string) string {
	switch status {
	case "in_progress":
		return "working"
	case "blocked":
		return "blocked"
	case "closed":
		return "closed"
	default:
		return "queued"
	}
}
