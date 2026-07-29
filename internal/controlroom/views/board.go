package views

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/Loxstomper/software-factory/internal/controlroom/query"
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

// cardStyle is the card's inline style: always the view-transition-name (cardVTName) so a moved
// card animates, plus — when the card belongs to a multi-issue epic (card.EpicID set) — the
// epic's color published as the `--epic` CSS custom property and a left-border tint reading it.
// Publishing the hue once as a custom property is the *one color source* (T7.8): the tint here,
// the badge dot, and the client-drawn lineage thread all read var(--epic), so the JS overlay
// never re-implements the Go FNV hash. The hue is a deterministic hash of the epic id (T7.6,
// control-room.md "Epics on the board") — registry-free, stable across restarts, grouping a
// feature's work visually and disambiguating simultaneous epics. Color is never the *sole*
// channel (the epic badge text is the robust identifier) — a redundant cue per the spec's
// accessibility note. It returns templ.SafeCSS (bypassing templ's style sanitizer, which would
// strip view-transition-name): every interpolated value is injection-free — the id is reduced to
// the CSS ident charset and the hue is an integer.
func cardStyle(card query.IssueCard) templ.SafeCSS {
	css := string(cardVTName(card.ID))
	if card.EpicID != "" {
		css += "; --epic: " + epicHue(card.EpicID) + "; border-left-width: 3px; border-left-color: var(--epic)"
	}
	return templ.SafeCSS(css)
}

// epicDotStyle fills the epic badge's dot from the card's --epic custom property (cardStyle) — so
// the badge dot and the left-border tint read the one color source (T7.8). It is a constant,
// injection-free declaration, returned as SafeCSS so templ's sanitizer does not strip the var().
func epicDotStyle() templ.SafeCSS { return templ.SafeCSS("background-color: var(--epic)") }

// epicHue maps an epic id to a stable, well-distributed CSS hsl() color string. The hue is an
// FNV-1a hash of the id modulo 360 (so it is deterministic and registry-free — the same epic is
// the same color across restarts and across cards); saturation/lightness are fixed for a legible
// tint on the dark surface. Only an integer 0..359 is interpolated, so the result is injection-free
// for the SafeCSS path in cardStyle.
func epicHue(epicID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(epicID))
	return "hsl(" + strconv.Itoa(int(h.Sum32()%360)) + ", 70%, 60%)"
}

// epicProgressWidth renders the hero card's progress bar fill as a percent-width inline style —
// integrated / total of an epic's issues (T7.6). Injection-free (only a clamped integer is
// interpolated), so it is safe as SafeCSS. A zero-issue epic (impossible in practice — the root
// itself counts) renders an empty bar rather than dividing by zero.
func epicProgressWidth(e *query.EpicSummary) templ.SafeCSS {
	pct := 0
	if e != nil && e.Total > 0 {
		pct = e.Integrated * 100 / e.Total
	}
	return templ.SafeCSS("width: " + strconv.Itoa(pct) + "%")
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

// wallTimerWarnPct is the near-cap threshold (percent) at which a card's live timer tints amber
// toward its budget.wall ceiling — the same 80% warn line the Budgets view's budgetPct uses, so
// the two surfaces agree on what counts as "about to breach".
const wallTimerWarnPct = 80

// timerRowClass is the live timer row's class, tinting it toward the card's budget.wall ceiling
// (control-room.md "The board, in motion": the in-progress timer tints toward its budget.wall
// ceiling — a live "about to breach" signal). It stays the default faint until the cumulative wall
// burn nears its cap — amber at the 80% warn line, rose once over — so a card about to be
// dead-lettered on wall stands out at a glance. An uncapped wall (no budget.wall configured) never
// tints. The tint reads off the same meters as the Budgets view (WallPct/WallOver), so the board
// and the budget table can never disagree on a breach.
func timerRowClass(card query.IssueCard) string {
	const base = "mt-2 flex items-center justify-between text-xs "
	return base + wallTint(card)
}

// timerStateNumClass is the time-in-state duration span's class. It normally reads text-muted (a
// shade brighter than the faint row, to emphasize the number), but drops the override when the row
// is wall-tinted so the duration inherits the amber/rose alert color rather than fighting it.
func timerStateNumClass(card query.IssueCard) string {
	if wallTint(card) == "text-faint" {
		return "tabular-nums text-muted"
	}
	return "tabular-nums"
}

// wallTint maps a card's wall-budget burn to a text color token: text-faint (default — uncapped,
// or comfortably under), text-st-warn (amber, ≥80% of cap), text-st-blocked (rose, over cap). The
// class literals are returned as plain strings the templ class attribute concatenates; they are
// also written verbatim in budgets.templ's budgetPct, so the Tailwind scanner already compiles them.
func wallTint(card query.IssueCard) string {
	if !card.WallCapped {
		return "text-faint"
	}
	switch {
	case card.WallOver:
		return "text-st-blocked"
	case card.WallPct >= wallTimerWarnPct:
		return "text-st-warn"
	default:
		return "text-faint"
	}
}

// wallTitle is the live timer's hover tooltip when a wall cap is configured — "wall NN% of
// budget" — so the budget.wall burn is legible on inspection and the tint's meaning is carried
// by text too, not color alone (the spec's accessibility note). Only rendered when WallCapped.
func wallTitle(card query.IssueCard) string {
	return "wall " + strconv.Itoa(card.WallPct) + "% of budget"
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
