package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
)

// orchWithSouls builds a bare Orchestrator carrying just the souls, which is all
// selectSoul reads — no NATS/beads/gate needed (selection is pure config + the issue).
func orchWithSouls(souls ...core.Soul) *Orchestrator {
	return &Orchestrator{opts: Options{Config: &config.Config{Harness: &config.Harness{}, Souls: souls}}}
}

func TestSelectSoul(t *testing.T) {
	goImpl := core.Soul{Name: "implementor-go", Role: "implement", Selector: map[string]string{"lang": "go"}}
	rustImpl := core.Soul{Name: "implementor-rust", Role: "implement", Selector: map[string]string{"lang": "rust"}}
	defImpl := core.Soul{Name: "implementor-default", Role: "implement"} // no selector = catch-all
	goHigh := core.Soul{Name: "implementor-go-high", Role: "implement", Selector: map[string]string{"lang": "go", "tier": "high"}}

	cases := []struct {
		name  string
		souls []core.Soul
		issue core.Issue
		want  string // chosen soul name, or "" for no match
	}{
		{
			// The kernel case: one soul per role with a selector, issue untagged. The single
			// soul is used unconditionally (trivial 1:1, no tags or selector ceremony).
			name:  "single soul used regardless of tags",
			souls: []core.Soul{goImpl},
			issue: core.Issue{Role: "implement"},
			want:  "implementor-go",
		},
		{
			name:  "tags pick the matching soul among several",
			souls: []core.Soul{goImpl, rustImpl},
			issue: core.Issue{Role: "implement", Tags: map[string]string{"lang": "rust"}},
			want:  "implementor-rust",
		},
		{
			name:  "most specific selector wins over a default",
			souls: []core.Soul{defImpl, goImpl},
			issue: core.Issue{Role: "implement", Tags: map[string]string{"lang": "go"}},
			want:  "implementor-go",
		},
		{
			name:  "default catch-all wins when nothing specific matches",
			souls: []core.Soul{defImpl, goImpl},
			issue: core.Issue{Role: "implement", Tags: map[string]string{"lang": "python"}},
			want:  "implementor-default",
		},
		{
			name:  "most specific of multiple matching selectors wins",
			souls: []core.Soul{goImpl, goHigh},
			issue: core.Issue{Role: "implement", Tags: map[string]string{"lang": "go", "tier": "high"}},
			want:  "implementor-go-high",
		},
		{
			name:  "no soul matches and no default -> not dispatchable",
			souls: []core.Soul{goImpl, rustImpl},
			issue: core.Issue{Role: "implement", Tags: map[string]string{"lang": "python"}},
			want:  "",
		},
		{
			name:  "no soul for the role -> not dispatchable",
			souls: []core.Soul{goImpl},
			issue: core.Issue{Role: "qa"},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := orchWithSouls(tc.souls...)
			soul, ok := o.selectSoul(tc.issue)
			if tc.want == "" {
				if ok {
					t.Fatalf("selectSoul = %q, want no match", soul.Name)
				}
				return
			}
			if !ok || soul.Name != tc.want {
				t.Fatalf("selectSoul = (%q, %v), want (%q, true)", soul.Name, ok, tc.want)
			}
		})
	}
}

// TestScheduleReadyDispatchesSelectedSoul proves selection flows end-to-end: with two
// souls for the implement role, a tagged ready issue is dispatched with a Brief carrying
// the soul whose selector its tags match.
func TestScheduleReadyDispatchesSelectedSoul(t *testing.T) {
	cfg := kernelConfig(2)
	// kernelConfig ships one untagged-selector soul for implement; give it a selector and
	// add a second soul so selection is non-trivial.
	cfg.Souls = []core.Soul{
		{Name: "implementor-go", Role: "implement", Sandbox: "go-toolchain", Selector: map[string]string{"lang": "go"}},
		{Name: "implementor-rust", Role: "implement", Sandbox: "rust-toolchain", Selector: map[string]string{"lang": "rust"}},
	}
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement", Tags: map[string]string{"lang": "rust"}}}
	o, nc := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})

	sub, err := nc.SubscribeSync(messaging.WorkSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe work: %v", err)
	}
	o.scheduleReady(context.Background())

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no work published: %v", err)
	}
	var brief core.Brief
	if err := json.Unmarshal(msg.Data, &brief); err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	if brief.Soul.Name != "implementor-rust" {
		t.Errorf("dispatched soul = %q, want implementor-rust (matched by tag lang=rust)", brief.Soul.Name)
	}
}

// Selection is deterministic for a specificity tie: souls are loaded Name-sorted, so the
// lowest Name wins. (A config with two same-role souls sharing a selector is rejected by
// validation; this only pins the ordering for distinct-but-equal-size selectors.)
func TestSelectSoulDeterministicTie(t *testing.T) {
	a := core.Soul{Name: "impl-a", Role: "implement", Selector: map[string]string{"region": "us"}}
	b := core.Soul{Name: "impl-b", Role: "implement", Selector: map[string]string{"team": "core"}}
	// Tags satisfy both single-key selectors; equal specificity -> lowest Name (impl-a).
	o := orchWithSouls(a, b)
	issue := core.Issue{Role: "implement", Tags: map[string]string{"region": "us", "team": "core"}}
	if soul, ok := o.selectSoul(issue); !ok || soul.Name != "impl-a" {
		t.Fatalf("selectSoul = %q (ok=%v), want impl-a (deterministic lowest-name tie-break)", soul.Name, ok)
	}
}
