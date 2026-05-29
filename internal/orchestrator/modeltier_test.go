package orchestrator

import (
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// TestModelTierResolvedPerIssue is the T3.4 contract guard: the model an invocation runs
// under is the *selected soul's* model, resolved per issue — there is no global default.
// Two souls fulfill one role at different tiers (frontier vs cheap) with distinct
// selectors; an issue's tags pick one, and both the Brief the runner resolves its adapter
// from (brief.Soul.Model) and the provenance trailer that lands on main carry that soul's
// model. This is what makes "cheap models serve easy roles, frontier the hard ones" real
// and auditable: change the soul's model in config and the per-issue model follows, with
// the choice recorded in the merge commit's provenance.
func TestModelTierResolvedPerIssue(t *testing.T) {
	frontier := core.Soul{Name: "impl-frontier", Role: "implement", Model: "claude-opus-4-8", Selector: map[string]string{"tier": "high"}}
	cheap := core.Soul{Name: "impl-cheap", Role: "implement", Model: "claude-haiku-4-5", Selector: map[string]string{"tier": "low"}}
	o := orchWithSouls(frontier, cheap)

	cases := []struct {
		tier      string
		wantModel string
	}{
		{"high", "claude-opus-4-8"},
		{"low", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			issue := core.Issue{ID: "iss-1", Title: "x", Role: "implement", Tags: map[string]string{"tier": tc.tier}}

			soul, ok := o.selectSoul(issue)
			if !ok {
				t.Fatalf("selectSoul: no soul for tier %q", tc.tier)
			}
			// The Brief carries the selected soul whole; the runner resolves its provider
			// adapter from brief.Soul.Model (internal/runner/runner.go), so the model the
			// invocation actually runs under is this field.
			brief := o.buildBrief(issue, config.Stage{Role: "implement"}, soul)
			if brief.Soul.Model != tc.wantModel {
				t.Errorf("brief.Soul.Model = %q, want %q", brief.Soul.Model, tc.wantModel)
			}
			// Provenance records the same model on the merge trailer, so the tier choice is
			// auditable per merged commit. Empty Result/Report: only the soul/model path matters.
			prov := o.provenanceFor(issue, core.Result{}, gate.Report{})
			if prov.Model != tc.wantModel {
				t.Errorf("provenance Model = %q, want %q", prov.Model, tc.wantModel)
			}
			if prov.Soul != soul.Name {
				t.Errorf("provenance Soul = %q, want %q", prov.Soul, soul.Name)
			}
		})
	}
}
