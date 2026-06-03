package wizard

import "testing"

// TestParseDraftExtractsBlock proves a well-formed trailing ```draft block is parsed into a
// Draft: the summary, spec files (content preserved verbatim, including newlines), and seed
// issues with their fields and dependency edges.
func TestParseDraftExtractsBlock(t *testing.T) {
	reply := "Here is what I propose.\n\n```draft\n" +
		`{"summary":"Add CSV export","specs":[{"path":"specs/export.md","content":"# Export\n\nDetails.\n"}],` +
		`"issues":[{"title":"Export orders","body":"Build it.","role":"planner","spec":"specs/export.md","key":"exp","depends_on":["other"]}]}` +
		"\n```"
	d, ok := parseDraft(reply)
	if !ok {
		t.Fatal("parseDraft returned ok=false for a well-formed block")
	}
	if d.Summary != "Add CSV export" {
		t.Errorf("summary = %q", d.Summary)
	}
	if len(d.Specs) != 1 || d.Specs[0].Path != "specs/export.md" {
		t.Fatalf("specs wrong: %+v", d.Specs)
	}
	if d.Specs[0].Content != "# Export\n\nDetails.\n" {
		t.Errorf("spec content not preserved verbatim: %q", d.Specs[0].Content)
	}
	if len(d.Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(d.Issues))
	}
	is := d.Issues[0]
	if is.Title != "Export orders" || is.Body != "Build it." || is.Role != "planner" || is.Spec != "specs/export.md" || is.Key != "exp" {
		t.Errorf("issue fields wrong: %+v", is)
	}
	if len(is.DependsOn) != 1 || is.DependsOn[0] != "other" {
		t.Errorf("depends_on wrong: %+v", is.DependsOn)
	}
}

// TestParseDraftDegrades proves the parser never errors and never returns a partial draft for a
// missing, malformed, or empty block — so a draft-less turn falls back to plain chat and never
// clobbers a prior draft (the caller overwrites only on ok).
func TestParseDraftDegrades(t *testing.T) {
	cases := map[string]string{
		"no block":          "Just prose, no draft.",
		"malformed json":    "Prose.\n```draft\n{not json}\n```",
		"unterminated":      "Prose.\n```draft\n{\"summary\":\"x\"}",
		"empty array":       "Prose.\n```draft\n{}\n```",
		"no path no issues": "Prose.\n```draft\n{\"summary\":\"x\",\"specs\":[{\"content\":\"c\"}],\"issues\":[{}]}\n```",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			if d, ok := parseDraft(reply); ok {
				t.Errorf("parseDraft returned ok=true for %s: %+v", name, d)
			}
		})
	}
}

// TestParseDraftDropsIncompleteEntries proves a spec missing its path/content or an issue missing
// its title is dropped, while valid siblings survive — a robust parse rather than an all-or-nothing
// reject.
func TestParseDraftDropsIncompleteEntries(t *testing.T) {
	reply := "Prose.\n```draft\n" +
		`{"summary":"s","specs":[{"path":"specs/a.md","content":"A"},{"path":"","content":"x"},{"path":"specs/b.md","content":""}],` +
		`"issues":[{"title":"keep"},{"title":""}]}` +
		"\n```"
	d, ok := parseDraft(reply)
	if !ok {
		t.Fatal("ok=false")
	}
	if len(d.Specs) != 1 || d.Specs[0].Path != "specs/a.md" {
		t.Errorf("incomplete specs not dropped: %+v", d.Specs)
	}
	if len(d.Issues) != 1 || d.Issues[0].Title != "keep" {
		t.Errorf("titleless issue not dropped: %+v", d.Issues)
	}
}

// TestDisplayProseCutsBothFences proves the live-stream prose is cut at the EARLIEST of the
// ledger/draft fences, in either order, so neither accumulating JSON block ever flashes in the
// token stream or lands in the stored transcript.
func TestDisplayProseCutsBothFences(t *testing.T) {
	ledgerFirst := "The prose.\n```ledger\n[]\n```\n```draft\n{}\n```"
	draftFirst := "The prose.\n```draft\n{}\n```\n```ledger\n[]\n```"
	for name, reply := range map[string]string{"ledger first": ledgerFirst, "draft first": draftFirst} {
		if got := displayProse(reply); got != "The prose." {
			t.Errorf("%s: displayProse = %q, want %q", name, got, "The prose.")
		}
	}
	if got := displayProse("no fences here"); got != "no fences here" {
		t.Errorf("no-fence reply altered: %q", got)
	}
}

// TestFinalizedDecisions proves the decisions sidecar is derived from the RESOLVED ledger items
// (T4.13/T4.27): agreed items become decisions (a selected fork option folded into the point
// text), deferred items become recorded open items (Deferred=true, question only — no option
// folded), and still-open/discussing items are excluded — so the ledger, not a parallel block,
// is the single source of the sidecar.
func TestFinalizedDecisions(t *testing.T) {
	items := []LedgerItem{
		{Question: "Auth in v1?", Status: ledgerStatusAgreed, Rationale: "Out of scope."},
		{Question: "Datastore?", Status: ledgerStatusAgreed, Rationale: "Ops familiarity.",
			Options: []LedgerOption{{Label: "Postgres", Selected: true}, {Label: "SQLite"}}},
		{Question: "Still open?", Status: ledgerStatusOpen, Rationale: "TBD."},
		{Question: "Under discussion?", Status: ledgerStatusDiscussing, Rationale: "Mulling."},
		{Question: "Rate limiting?", Status: ledgerStatusDeferred, Rationale: "Punt to v2.",
			Options: []LedgerOption{{Label: "Token bucket", Selected: true}}},
	}
	got := FinalizedDecisions(items)
	if len(got) != 3 {
		t.Fatalf("decisions = %d, want 3 (open + discussing excluded): %+v", len(got), got)
	}
	if got[0].Point != "Auth in v1?" || got[0].Rationale != "Out of scope." || got[0].Deferred {
		t.Errorf("decision[0] wrong: %+v", got[0])
	}
	if got[1].Point != "Datastore? → Postgres" {
		t.Errorf("selected option not folded into point: %q", got[1].Point)
	}
	// A deferred fork is recorded by its question alone (no option folded) with Deferred set.
	if got[2].Point != "Rate limiting?" || !got[2].Deferred {
		t.Errorf("decision[2] = %+v, want the deferred fork recorded by question only", got[2])
	}
}
