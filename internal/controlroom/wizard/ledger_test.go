package wizard

import "testing"

// TestParseLedgerExtractsBlock proves a well-formed trailing ```ledger block is parsed into
// items and stripped from the prose: the returned prose is the text before the fence (right-
// trimmed), and the items carry the normalized fields.
func TestParseLedgerExtractsBlock(t *testing.T) {
	reply := "Here is where we stand.\n\n```ledger\n" +
		`[{"question":"Which datastore?","status":"open","rationale":"Driven by query shape.",` +
		`"options":[{"label":"Postgres","tradeoff":"mature ops","selected":true},` +
		`{"label":"SQLite","tradeoff":"single-node","selected":false}]}]` +
		"\n```"
	items, prose := parseLedger(reply)
	if prose != "Here is where we stand." {
		t.Errorf("prose = %q, want the text before the fence", prose)
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

// TestParseLedgerNoBlock proves a reply with no ```ledger block returns nil items and the
// original reply unchanged — the conversation degrades to plain chat.
func TestParseLedgerNoBlock(t *testing.T) {
	reply := "Just a plain message with no ledger."
	items, prose := parseLedger(reply)
	if items != nil {
		t.Errorf("items = %+v, want nil for a reply with no block", items)
	}
	if prose != reply {
		t.Errorf("prose = %q, want the original reply unchanged", prose)
	}
}

// TestParseLedgerMalformedJSON proves a block with malformed JSON returns nil items but still
// strips the block from the prose, so a bad snapshot neither errors nor leaks raw JSON.
func TestParseLedgerMalformedJSON(t *testing.T) {
	reply := "Prose part.\n\n```ledger\n{not valid json]\n```"
	items, prose := parseLedger(reply)
	if items != nil {
		t.Errorf("items = %+v, want nil for malformed JSON", items)
	}
	if prose != "Prose part." {
		t.Errorf("prose = %q, want the block stripped", prose)
	}
}

// TestParseLedgerSkipsEmpty proves items with an empty question and options with an empty
// label are skipped, and that a block yielding zero valid items returns (nil, prose).
func TestParseLedgerSkipsEmpty(t *testing.T) {
	reply := "P.\n```ledger\n" +
		`[{"question":"  ","status":"open"},` +
		`{"question":"Real?","status":"agreed","options":[{"label":""},{"label":"Keep"}]}]` +
		"\n```"
	items, prose := parseLedger(reply)
	if prose != "P." {
		t.Errorf("prose = %q", prose)
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

	// A block with only invalid items yields nil items but a stripped prose.
	only := "Pre.\n```ledger\n" + `[{"question":""}]` + "\n```"
	gotItems, gotProse := parseLedger(only)
	if gotItems != nil {
		t.Errorf("items = %+v, want nil when no valid items remain", gotItems)
	}
	if gotProse != "Pre." {
		t.Errorf("prose = %q, want the block stripped", gotProse)
	}
}

// TestParseLedgerStatusNormalize proves status is normalized case-insensitively: anything that
// is not "agreed" (any case) becomes "open".
func TestParseLedgerStatusNormalize(t *testing.T) {
	reply := "x\n```ledger\n" +
		`[{"question":"a","status":"AGREED"},{"question":"b","status":"weird"},{"question":"c","status":""}]` +
		"\n```"
	items, _ := parseLedger(reply)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].Status != ledgerStatusAgreed {
		t.Errorf("AGREED should normalize to agreed, got %q", items[0].Status)
	}
	if items[1].Status != ledgerStatusOpen || items[2].Status != ledgerStatusOpen {
		t.Errorf("unknown/blank status should normalize to open, got %q %q", items[1].Status, items[2].Status)
	}
}

// TestParseLedgerMultipleItems proves several items round-trip in order.
func TestParseLedgerMultipleItems(t *testing.T) {
	reply := "ok\n```ledger\n" +
		`[{"question":"q1","status":"open"},{"question":"q2","status":"agreed","rationale":"done"}]` +
		"\n```"
	items, _ := parseLedger(reply)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Question != "q1" || items[1].Question != "q2" || items[1].Rationale != "done" {
		t.Errorf("items out of order or wrong: %+v", items)
	}
}

// TestDisplayProse proves the live-stream prose is cut at the FIRST fence (so the JSON never
// flashes mid-stream), and an unfenced reply is returned unchanged.
func TestDisplayProse(t *testing.T) {
	if got := displayProse("Hello there.\n```ledger\n[half written"); got != "Hello there." {
		t.Errorf("displayProse cut wrong: %q", got)
	}
	if got := displayProse("No fence here"); got != "No fence here" {
		t.Errorf("displayProse should return unfenced reply unchanged, got %q", got)
	}
}

// TestCutLedgerBlock proves the low-level split: it finds the last fence, returns the JSON
// between fences and the right-trimmed prose, and reports ok=false for missing fences.
func TestCutLedgerBlock(t *testing.T) {
	prose, raw, ok := cutLedgerBlock("Prose.\n\n```ledger\n[1,2,3]\n```")
	if !ok {
		t.Fatal("ok = false for a well-formed block")
	}
	if prose != "Prose." {
		t.Errorf("prose = %q", prose)
	}
	if got := trim(raw); got != "[1,2,3]" {
		t.Errorf("raw = %q, want the JSON between fences", got)
	}

	if _, _, ok := cutLedgerBlock("no fence at all"); ok {
		t.Error("ok = true with no opening fence")
	}
	if _, _, ok := cutLedgerBlock("p\n```ledger\n[unterminated"); ok {
		t.Error("ok = true with no closing fence")
	}
}

// trim is a tiny local helper so the test asserts on the JSON payload ignoring surrounding
// whitespace the split intentionally leaves intact.
func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
