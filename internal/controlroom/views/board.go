package views

import (
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
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
