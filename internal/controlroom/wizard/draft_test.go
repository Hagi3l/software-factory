package wizard

import (
	"encoding/json"
	"testing"
)

// TestParseDraftArgs proves a well-formed propose_draft payload is decoded into a Draft: the
// summary, spec files (content preserved verbatim, including newlines), and seed issues with
// their fields and dependency edges.
func TestParseDraftArgs(t *testing.T) {
	args := json.RawMessage(`{"summary":"Add CSV export","specs":[{"path":"specs/export.md","content":"# Export\n\nDetails.\n"}],` +
		`"issues":[{"title":"Export orders","body":"Build it.","role":"planner","spec":"specs/export.md","key":"exp","depends_on":["other"]}]}`)
	d, err := parseDraftArgs(args)
	if err != nil {
		t.Fatalf("parseDraftArgs: %v", err)
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

// TestParseDraftArgsDegrades proves the parser returns an error (never a partial draft) for
// malformed args or a draft proposing neither a spec nor an issue — the engine's signal to leave
// a prior draft intact and ack the model rather than clobbering.
func TestParseDraftArgsDegrades(t *testing.T) {
	cases := map[string]string{
		"malformed json":    `{not json}`,
		"empty object":      `{}`,
		"no path no issues": `{"summary":"x","specs":[{"content":"c"}],"issues":[{}]}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if d, err := parseDraftArgs(json.RawMessage(args)); err == nil {
				t.Errorf("parseDraftArgs returned nil error for %s: %+v", name, d)
			}
		})
	}
}

// TestParseDraftArgsDropsIncompleteEntries proves a spec missing its path/content or an issue
// missing its title is dropped, while valid siblings survive — a robust parse rather than an
// all-or-nothing reject.
func TestParseDraftArgsDropsIncompleteEntries(t *testing.T) {
	args := json.RawMessage(`{"summary":"s","specs":[{"path":"specs/a.md","content":"A"},{"path":"","content":"x"},{"path":"specs/b.md","content":""}],` +
		`"issues":[{"title":"keep"},{"title":""}]}`)
	d, err := parseDraftArgs(args)
	if err != nil {
		t.Fatalf("parseDraftArgs: %v", err)
	}
	if len(d.Specs) != 1 || d.Specs[0].Path != "specs/a.md" {
		t.Errorf("incomplete specs not dropped: %+v", d.Specs)
	}
	if len(d.Issues) != 1 || d.Issues[0].Title != "keep" {
		t.Errorf("titleless issue not dropped: %+v", d.Issues)
	}
}

// TestProposeDraftToolDef proves the output-tool definition is well-formed: the canonical name
// and a Params blob that is valid JSON Schema with the three required top-level fields.
func TestProposeDraftToolDef(t *testing.T) {
	def := proposeDraftToolDef()
	if def.Name != toolProposeDraft {
		t.Errorf("name = %q, want %q", def.Name, toolProposeDraft)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(def.Params, &schema); err != nil {
		t.Fatalf("Params is not valid JSON: %v", err)
	}
	for _, key := range []string{"summary", "specs", "issues"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Errorf("schema missing property %q", key)
		}
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
