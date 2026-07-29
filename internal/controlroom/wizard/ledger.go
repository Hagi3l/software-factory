package wizard

import (
	"encoding/json"
	"strings"

	"github.com/Loxstomper/software-factory/internal/model"
)

// The alignment ledger (T4.13/T4.27, specs/control-room.md "The alignment ledger") is a
// lightly-structured, latest-wins snapshot of where a requirements conversation stands: each
// item is a fork in one of four states — `open` (the start state), `agreed` (decided),
// `discussing` (the human flagged it and wants the planner to go deeper — the only non-terminal
// resolution), or `deferred` (knowingly left for later — terminal, counts as resolved) — with a
// one-line rationale and, for an unsettled fork, the options under consideration. It is a working
// aid, not a durable object model: nothing here is persisted (that is the consent-gated APPROVE
// work). The planner is the single source of truth — it re-emits the COMPLETE ledger each turn as
// the schema-validated arguments of an `update_ledger` tool call (T4.29: structured state rides
// the tool channel, not parsed prose), which the engine harvests (parseLedgerArgs), stores on the
// session, and the view renders. Chip picks, free text, and "let's discuss" flags all funnel back
// through Send (Answer) so the planner re-emits the ledger reflecting them; there is no parallel
// client-side mutable model ("dumb ledger, smart planner").
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
	// toolUpdateLedger is the name of the output tool the planner calls to emit the complete
	// ledger as schema-validated arguments (T4.29).
	toolUpdateLedger = "update_ledger"
)

// ledgerWire is the JSON wire shape of one fork inside the update_ledger call's arguments. It is
// decoded then normalized into the exported LedgerItem/LedgerOption — the wire form is
// intentionally separate so the rendered types carry no json concerns.
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

// updateLedgerArgs is the argument shape of the update_ledger output tool: the COMPLETE alignment
// ledger as a JSON object with a single `items` array. A tool call's arguments are always a
// top-level object, so — unlike the old fenced block, which a model emitted as a bare array, a
// one-key wrapper, or a lone object interchangeably — the schema fixes the shape to {"items":[…]}
// at the model boundary. That is the T4.29 robustness win: the lenient three-way shape-guessing
// (and the smart-quote / trailing-comma normalization) the fenced-block decoder needed is gone.
type updateLedgerArgs struct {
	Items []ledgerWire `json:"items"`
}

// updateLedgerToolDef is the output-tool definition the planner calls to record the ledger. It is
// a pure-output tool (it records structured state), distinct from the read-only exploration action
// tools (readOnlyToolDefs): harvesting it never triggers another model round-trip — the call rides
// whatever turn it arrives on. The status enum is encoded in the schema; normalizeStatus stays as
// belt-and-suspenders since the schema constrains shape, not that a weak model respects the enum.
func updateLedgerToolDef() model.ToolDef {
	return model.ToolDef{
		Name: toolUpdateLedger,
		Description: "Record the COMPLETE current alignment ledger — every decision fork and its state. " +
			"Re-emit the whole ledger each reply (latest wins; only your most recent call is kept). " +
			"This records state only: it does not end the conversation or trigger codebase exploration.",
		Params: json.RawMessage(`{
			"type": "object",
			"properties": {
				"items": {
					"type": "array",
					"description": "The complete ledger — every fork, re-sent in full each turn.",
					"items": {
						"type": "object",
						"properties": {
							"question": {"type": "string", "description": "The decision point, phrased as a question."},
							"status": {"type": "string", "enum": ["open", "agreed", "discussing", "deferred"], "description": "open=not yet resolved; agreed=decided (mark the chosen option selected); discussing=human flagged it for deeper discussion (non-terminal); deferred=knowingly left for later (terminal)."},
							"rationale": {"type": "string", "description": "One line: why."},
							"options": {
								"type": "array",
								"description": "Selectable fork choices; empty for a settled non-fork point.",
								"items": {
									"type": "object",
									"properties": {
										"label": {"type": "string"},
										"tradeoff": {"type": "string", "description": "The choice's consequence, in plain language."},
										"selected": {"type": "boolean", "description": "true for the chosen option of an agreed fork."}
									},
									"required": ["label"]
								}
							}
						},
						"required": ["question", "status"]
					}
				}
			},
			"required": ["items"]
		}`),
	}
}

// parseLedgerArgs decodes an update_ledger call's arguments into normalized ledger items. It needs
// no lenient shape-guessing: the schema fixes the wire shape, so a payload that does not decode is
// an error here (acked back to the model) rather than a silently mis-parsed block — the failure
// class T4.29 set out to eliminate. It still drops items with no question and options with no
// label and normalizes the status enum. The returned slice may be empty (no valid items); the
// caller treats that as "no update" so a prior ledger is never clobbered.
func parseLedgerArgs(args json.RawMessage) ([]LedgerItem, error) {
	var a updateLedgerArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	return ledgerItemsFromWire(a.Items), nil
}

// ledgerItemsFromWire normalizes decoded wire forks into rendered LedgerItems: trims fields,
// normalizes the status enum, and drops a fork with no question or an option with no label.
func ledgerItemsFromWire(wire []ledgerWire) []LedgerItem {
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
	return items
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
