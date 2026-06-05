package wizard

import (
	"encoding/json"
	"testing"
)

// TestParseLedgerArgs proves a well-formed update_ledger payload is decoded into normalized
// items: the {"items":[…]} object shape (a tool call's args are always a top-level object), with
// each fork's fields and option chips carried through.
func TestParseLedgerArgs(t *testing.T) {
	args := json.RawMessage(`{"items":[` +
		`{"question":"Which datastore?","status":"open","rationale":"Driven by query shape.",` +
		`"options":[{"label":"Postgres","tradeoff":"mature ops","selected":true},` +
		`{"label":"SQLite","tradeoff":"single-node","selected":false}]}]}`)
	items, err := parseLedgerArgs(args)
	if err != nil {
		t.Fatalf("parseLedgerArgs: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.Question != "Which datastore?" || it.Status != ledgerStatusOpen || it.Rationale != "Driven by query shape." {
		t.Errorf("item fields wrong: %+v", it)
	}
	if len(it.Options) != 2 {
		t.Fatalf("options = %d, want 2", len(it.Options))
	}
	if it.Options[0].Label != "Postgres" || it.Options[0].Tradeoff != "mature ops" || !it.Options[0].Selected {
		t.Errorf("option[0] wrong: %+v", it.Options[0])
	}
	if it.Options[1].Selected {
		t.Errorf("option[1] should not be selected: %+v", it.Options[1])
	}
}

// TestParseLedgerArgsMalformed proves args that do not decode as JSON return an error (which the
// engine acks back to the model / logs) rather than a silent mis-parse — the failure class T4.29
// moved to the schema boundary. The error is the caller's signal to leave the prior ledger intact.
func TestParseLedgerArgsMalformed(t *testing.T) {
	if _, err := parseLedgerArgs(json.RawMessage(`{not valid json]`)); err == nil {
		t.Error("parseLedgerArgs returned nil error for malformed JSON")
	}
}

// TestParseLedgerArgsSkipsEmpty proves items with an empty question and options with an empty
// label are skipped, and a payload yielding zero valid items returns an empty (not errored) slice
// — the engine treats that as "no update" so a prior ledger is never clobbered.
func TestParseLedgerArgsSkipsEmpty(t *testing.T) {
	args := json.RawMessage(`{"items":[` +
		`{"question":"  ","status":"open"},` +
		`{"question":"Real?","status":"agreed","options":[{"label":""},{"label":"Keep"}]}]}`)
	items, err := parseLedgerArgs(args)
	if err != nil {
		t.Fatalf("parseLedgerArgs: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (the empty-question item is skipped)", len(items))
	}
	if items[0].Question != "Real?" || items[0].Status != ledgerStatusAgreed {
		t.Errorf("item wrong: %+v", items[0])
	}
	if len(items[0].Options) != 1 || items[0].Options[0].Label != "Keep" {
		t.Errorf("options = %+v, want only the labeled one", items[0].Options)
	}

	// A payload with only invalid items decodes fine but yields zero items.
	only, err := parseLedgerArgs(json.RawMessage(`{"items":[{"question":""}]}`))
	if err != nil {
		t.Fatalf("parseLedgerArgs (only-invalid): %v", err)
	}
	if len(only) != 0 {
		t.Errorf("items = %+v, want empty when no valid items remain", only)
	}
}

// TestParseLedgerArgsStatusNormalize proves status is normalized case-insensitively over the four
// states (T4.27): agreed/discussing/deferred map to themselves (any case), and an unknown or
// blank value falls back to open. The schema constrains the enum, but normalizeStatus stays as
// belt-and-suspenders since the schema does not guarantee a weak model respects it.
func TestParseLedgerArgsStatusNormalize(t *testing.T) {
	args := json.RawMessage(`{"items":[` +
		`{"question":"a","status":"AGREED"},{"question":"b","status":"weird"},{"question":"c","status":""},` +
		`{"question":"d","status":"Discussing"},{"question":"e","status":"DEFERRED"}]}`)
	items, err := parseLedgerArgs(args)
	if err != nil {
		t.Fatalf("parseLedgerArgs: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("items = %d, want 5", len(items))
	}
	if items[0].Status != ledgerStatusAgreed {
		t.Errorf("AGREED should normalize to agreed, got %q", items[0].Status)
	}
	if items[1].Status != ledgerStatusOpen || items[2].Status != ledgerStatusOpen {
		t.Errorf("unknown/blank status should normalize to open, got %q %q", items[1].Status, items[2].Status)
	}
	if items[3].Status != ledgerStatusDiscussing {
		t.Errorf("Discussing should normalize to discussing, got %q", items[3].Status)
	}
	if items[4].Status != ledgerStatusDeferred {
		t.Errorf("DEFERRED should normalize to deferred, got %q", items[4].Status)
	}
}

// TestParseLedgerArgsMultipleItems proves several items round-trip in order.
func TestParseLedgerArgsMultipleItems(t *testing.T) {
	args := json.RawMessage(`{"items":[{"question":"q1","status":"open"},{"question":"q2","status":"agreed","rationale":"done"}]}`)
	items, err := parseLedgerArgs(args)
	if err != nil {
		t.Fatalf("parseLedgerArgs: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Question != "q1" || items[1].Question != "q2" || items[1].Rationale != "done" {
		t.Errorf("items out of order or wrong: %+v", items)
	}
}

// TestUpdateLedgerToolDef proves the output-tool definition is well-formed: the canonical name,
// and a Params blob that is valid JSON Schema encoding the four-state status enum (the schema is
// what enforces shape at the model boundary, so it must parse and carry the enum).
func TestUpdateLedgerToolDef(t *testing.T) {
	def := updateLedgerToolDef()
	if def.Name != toolUpdateLedger {
		t.Errorf("name = %q, want %q", def.Name, toolUpdateLedger)
	}
	var schema map[string]any
	if err := json.Unmarshal(def.Params, &schema); err != nil {
		t.Fatalf("Params is not valid JSON: %v", err)
	}
	// Drill into items → properties → status → enum and confirm all four states are present.
	enum := schema["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["status"].(map[string]any)["enum"].([]any)
	want := map[string]bool{ledgerStatusOpen: true, ledgerStatusAgreed: true, ledgerStatusDiscussing: true, ledgerStatusDeferred: true}
	if len(enum) != len(want) {
		t.Fatalf("status enum = %v, want the four states", enum)
	}
	for _, v := range enum {
		if !want[v.(string)] {
			t.Errorf("unexpected enum value %q", v)
		}
	}
}

// TestLedgerItemAnswerable proves the answerable predicate (T4.27): open and discussing forks
// invite input; agreed and deferred are terminal/read-only.
func TestLedgerItemAnswerable(t *testing.T) {
	cases := map[string]bool{
		ledgerStatusOpen:       true,
		ledgerStatusDiscussing: true,
		ledgerStatusAgreed:     false,
		ledgerStatusDeferred:   false,
	}
	for status, want := range cases {
		if got := (LedgerItem{Status: status}).Answerable(); got != want {
			t.Errorf("Answerable(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestApprovalDecisions proves the soft approval gate (T4.27): a discussing item blocks (decisions
// nil, the item returned in blocked); otherwise plain open forks are auto-deferred and recorded
// alongside the agreed decisions.
func TestApprovalDecisions(t *testing.T) {
	// A discussing item blocks approval — nothing is finalized.
	blocking := []LedgerItem{
		{Question: "Decided", Status: ledgerStatusAgreed},
		{Question: "Mulling", Status: ledgerStatusDiscussing},
	}
	decisions, blocked := ApprovalDecisions(blocking)
	if decisions != nil {
		t.Errorf("decisions = %+v, want nil when an item is discussing", decisions)
	}
	if len(blocked) != 1 || blocked[0].Question != "Mulling" {
		t.Fatalf("blocked = %+v, want the discussing item", blocked)
	}

	// With no discussing item, plain open forks auto-defer and land as recorded open items; agreed
	// forks land as decisions.
	converged := []LedgerItem{
		{Question: "Auth?", Status: ledgerStatusAgreed, Rationale: "Out of scope."},
		{Question: "Caching?", Status: ledgerStatusOpen, Rationale: "Punt to v2."},
		{Question: "Already deferred", Status: ledgerStatusDeferred},
	}
	decisions, blocked = ApprovalDecisions(converged)
	if blocked != nil {
		t.Fatalf("blocked = %+v, want nil for a converged ledger", blocked)
	}
	if len(decisions) != 3 {
		t.Fatalf("decisions = %d, want 3 (agreed + auto-deferred open + already-deferred)", len(decisions))
	}
	if decisions[0].Point != "Auth?" || decisions[0].Deferred {
		t.Errorf("decision[0] = %+v, want the agreed decision", decisions[0])
	}
	if decisions[1].Point != "Caching?" || !decisions[1].Deferred {
		t.Errorf("decision[1] = %+v, want the auto-deferred open fork recorded", decisions[1])
	}
	if !decisions[2].Deferred {
		t.Errorf("decision[2] = %+v, want the already-deferred fork recorded", decisions[2])
	}

	// autoDeferOpen does not mutate the caller's slice.
	if converged[1].Status != ledgerStatusOpen {
		t.Errorf("ApprovalDecisions mutated the source ledger: %+v", converged[1])
	}
}
