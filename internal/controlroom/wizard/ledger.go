package wizard

import (
	"encoding/json"
	"strings"
)

// The alignment ledger (T4.13/T4.27, specs/control-room.md "The alignment ledger") is a
// lightly-structured, latest-wins snapshot of where a requirements conversation stands: each
// item is a fork in one of four states — `open` (the start state), `agreed` (decided),
// `discussing` (the human flagged it and wants the planner to go deeper — the only non-terminal
// resolution), or `deferred` (knowingly left for later — terminal, counts as resolved) — with a
// one-line rationale and, for an unsettled fork, the options under consideration. It is a working
// aid, not a durable object model: nothing here is persisted (that is the consent-gated APPROVE
// work). The planner is the single source of truth — it re-emits the COMPLETE ledger each turn as
// a trailing fenced ```ledger JSON block appended after its prose, which the engine parses out
// (parseLedger), stores on the session, and the view renders. Chip picks, free text, and
// "let's discuss" flags all funnel back through Send (Answer) so the planner re-emits the ledger
// reflecting them; there is no parallel client-side mutable model ("dumb ledger, smart planner").
type LedgerItem struct {
	Question  string
	Status    string // one of the ledgerStatus* states
	Rationale string
	Options   []LedgerOption // nil for a settled (non-fork) point
}

// Answerable reports whether a fork still invites human input: `open` (not yet resolved) and
// `discussing` (the human flagged it but has not decided) are answerable; `agreed` and
// `deferred` are terminal and render read-only. The view uses this to decide which forks get
// chips + a free-text box + a "let's discuss" control, and the soft approval gate uses the
// two answerable states to decide what blocks (discussing) or is auto-deferred (open).
func (it LedgerItem) Answerable() bool {
	return it.Status == ledgerStatusOpen || it.Status == ledgerStatusDiscussing
}

// LedgerOption is one selectable fork choice with its tradeoff. Selected marks the choice the
// human funneled through the planner (rendered as the active chip).
type LedgerOption struct {
	Label    string
	Tradeoff string
	Selected bool
}

const (
	// The four item states (T4.27). open → start state; agreed/deferred → terminal (both count
	// as *resolved*); discussing → the only non-terminal resolution (the human flagged it).
	ledgerStatusOpen       = "open"
	ledgerStatusAgreed     = "agreed"
	ledgerStatusDiscussing = "discussing"
	ledgerStatusDeferred   = "deferred"
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

// normalizeStatus maps the wire status to one of the four canonical ledger states (T4.27),
// case-insensitively: agreed/discussing/deferred map to themselves, and everything else
// (including blank or an unrecognized value) falls back to open — open is the safe default so
// an item is never silently treated as settled, matching T4.13's degrade-gracefully contract.
func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ledgerStatusAgreed:
		return ledgerStatusAgreed
	case ledgerStatusDiscussing:
		return ledgerStatusDiscussing
	case ledgerStatusDeferred:
		return ledgerStatusDeferred
	default:
		return ledgerStatusOpen
	}
}

// ApprovalDecisions is the soft approval gate (T4.27, specs/control-room.md "Approval is gated
// on a converged ledger") evaluated server-side over the latest-wins ledger snapshot at the
// moment of consent. The gate is soft, not a lock:
//
//   - Any item still `discussing` BLOCKS approval — it is returned in blocked, and decisions is
//     nil. A discussing item is one the human *actively* flagged, so it is never auto-deferred
//     out from under them; they must consciously downgrade it to agreed or deferred first. This
//     keeps the human's own loop terminating without ever silently dropping a flagged decision.
//   - Otherwise every still-`open` fork is auto-converted to `deferred` (nothing vanishes
//     silently — the human may approve with open forks remaining, and they land in the sidecar
//     as "deliberately left open"), and decisions is the FinalizedDecisions over that converted
//     ledger: agreed forks as settled decisions, deferred forks as recorded open items.
//
// blocked is non-nil only when approval is refused; decisions is non-nil (possibly empty) only
// when it is allowed. The caller surfaces blocked as a notice and commits decisions otherwise.
func ApprovalDecisions(items []LedgerItem) (decisions []DecisionRecord, blocked []LedgerItem) {
	for _, it := range items {
		if it.Status == ledgerStatusDiscussing {
			blocked = append(blocked, it)
		}
	}
	if len(blocked) > 0 {
		return nil, blocked
	}
	return FinalizedDecisions(autoDeferOpen(items)), nil
}

// autoDeferOpen returns a copy of the ledger with every still-`open` item converted to
// `deferred` — the soft-gate behavior on APPROVE. Only Status is changed, so a shallow clone
// (sharing the Options slices) is safe.
func autoDeferOpen(items []LedgerItem) []LedgerItem {
	out := make([]LedgerItem, len(items))
	for i, it := range items {
		if it.Status == ledgerStatusOpen {
			it.Status = ledgerStatusDeferred
		}
		out[i] = it
	}
	return out
}
