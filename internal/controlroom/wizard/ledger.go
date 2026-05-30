package wizard

import (
	"encoding/json"
	"strings"
)

// The alignment ledger (T4.13, specs/control-room.md "The alignment ledger") is a lightly-
// structured, latest-wins snapshot of where a requirements conversation stands: each item is
// a point that is either *agreed* or still *open*, with a one-line rationale and — for an
// unsettled fork — the options under consideration. It is a working aid, not a durable object
// model: nothing here is persisted (that is the consent-gated APPROVE work). The planner is
// the single source of truth — it re-emits the COMPLETE ledger each turn as a trailing fenced
// ```ledger JSON block appended after its prose, which the engine parses out (parseLedger),
// stores on the session, and the view renders. Chip clicks and freeform both funnel back
// through Send so the planner re-emits the ledger reflecting the choice; there is no parallel
// client-side mutable model.
type LedgerItem struct {
	Question  string
	Status    string // ledgerStatusAgreed or ledgerStatusOpen
	Rationale string
	Options   []LedgerOption // nil for a settled (non-fork) point
}

// LedgerOption is one selectable fork choice with its tradeoff. Selected marks the choice the
// human funneled through the planner (rendered as the active chip).
type LedgerOption struct {
	Label    string
	Tradeoff string
	Selected bool
}

const (
	ledgerStatusAgreed = "agreed"
	ledgerStatusOpen   = "open"
	// ledgerFence opens the trailing fenced block the planner appends. The closing fence is a
	// bare ``` so the block can carry arbitrary JSON without colliding with the opener.
	ledgerFence = "```ledger"
)

// ledgerWire is the JSON wire shape the planner emits inside the fenced block: an array of
// these objects. It is decoded then normalized into the exported LedgerItem/LedgerOption — the
// wire form is intentionally separate so the rendered types carry no json concerns.
type ledgerWire struct {
	Question  string `json:"question"`
	Status    string `json:"status"`
	Rationale string `json:"rationale"`
	Options   []struct {
		Label    string `json:"label"`
		Tradeoff string `json:"tradeoff"`
		Selected bool   `json:"selected"`
	} `json:"options"`
}

// parseLedger extracts the trailing ```ledger fenced JSON block from a planner reply and
// returns the parsed, normalized items plus the prose with that block removed. It degrades
// gracefully: with no block, a malformed/empty block, or zero valid items it returns
// (nil, prose) — so the conversation falls back to plain chat and a ledger-less turn never
// errors and never clobbers a prior ledger (the caller only overwrites when items != nil).
func parseLedger(reply string) ([]LedgerItem, string) {
	prose, raw, ok := cutLedgerBlock(reply)
	if !ok {
		return nil, reply
	}

	var wire []ledgerWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &wire); err != nil {
		return nil, prose
	}

	items := make([]LedgerItem, 0, len(wire))
	for _, w := range wire {
		q := strings.TrimSpace(w.Question)
		if q == "" {
			continue // an item with no question is not a ledger entry
		}
		it := LedgerItem{
			Question:  q,
			Status:    normalizeStatus(w.Status),
			Rationale: strings.TrimSpace(w.Rationale),
		}
		for _, o := range w.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				continue // an option with no label cannot be a chip
			}
			it.Options = append(it.Options, LedgerOption{
				Label:    label,
				Tradeoff: strings.TrimSpace(o.Tradeoff),
				Selected: o.Selected,
			})
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, prose
	}
	return items, prose
}

// cutLedgerBlock splits a reply at the LAST ```ledger fence — see cutFencedBlock for the
// framing rules.
func cutLedgerBlock(reply string) (prose, raw string, ok bool) {
	return cutFencedBlock(reply, ledgerFence)
}

// cutFencedBlock splits a reply at the LAST occurrence of fence (e.g. ```ledger or ```draft):
// raw is the text between that fence and the next closing ``` after it; prose is the
// right-trimmed text before the fence. ok is false when there is no opening fence or no closing
// fence after it (an unterminated block is treated as absent so a mid-stream snapshot never
// half-parses). The closing fence is a bare ``` so the block can carry arbitrary JSON without
// colliding with the opener. The ledger and draft blocks use distinct openers and are extracted
// independently, so their order in the reply does not matter.
func cutFencedBlock(reply, fence string) (prose, raw string, ok bool) {
	open := strings.LastIndex(reply, fence)
	if open < 0 {
		return "", "", false
	}
	rest := reply[open+len(fence):]
	// The JSON starts after the newline that follows the opening fence (tolerate none).
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	closeIdx := strings.Index(rest, "```")
	if closeIdx < 0 {
		return "", "", false
	}
	raw = rest[:closeIdx]
	prose = strings.TrimRight(reply[:open], " \t\r\n")
	return prose, raw, true
}

// displayProse returns the text before the EARLIEST structured-block fence (the ledger or the
// draft block), right-trimmed, so neither accumulating JSON block ever flashes in the live
// token stream or lands in the stored transcript. With no fence the reply is returned unchanged.
func displayProse(reply string) string {
	cut := len(reply)
	for _, fence := range []string{ledgerFence, draftFence} {
		if i := strings.Index(reply, fence); i >= 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimRight(reply[:cut], " \t\r\n")
}

// normalizeStatus maps the wire status to the canonical ledger status: case-insensitive
// "agreed" stays agreed, everything else (including blank) is open — open is the safe default
// for an unrecognized value so an item is never silently treated as settled.
func normalizeStatus(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), ledgerStatusAgreed) {
		return ledgerStatusAgreed
	}
	return ledgerStatusOpen
}
