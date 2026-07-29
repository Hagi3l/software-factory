package agent

import (
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/config"
)

// BudgetFromPolicy maps only the per-issue token budget onto the per-invocation cap;
// usd/wall and the (config-absent) turn dimension are deliberately not carried over.
func TestBudgetFromPolicy(t *testing.T) {
	got := BudgetFromPolicy(config.Policy{
		MaxRetries: 3,
		Budget: config.Budget{
			Tokens: 2_000_000,
			USD:    20,
			Wall:   config.Duration(2 * time.Hour),
		},
		EpicBudget: config.Budget{USD: 200},
		DeadLetter: "factory.dlq",
	})

	if got.MaxTokens != 2_000_000 {
		t.Errorf("MaxTokens = %d, want the per-issue token budget 2_000_000", got.MaxTokens)
	}
	// Turns are the loop's own knob (config has no turn field) — left 0 so New defaults
	// it to DefaultMaxTurns. usd/wall do not become a per-invocation loop cap.
	if got.MaxTurns != 0 {
		t.Errorf("MaxTurns = %d, want 0 (defaulted by New, not from config)", got.MaxTurns)
	}
	if got.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (adapter default; config has no per-call cap)", got.MaxOutputTokens)
	}
}

// A zero token budget stays uncapped on tokens; the loop's turn cap still bounds it.
func TestBudgetFromPolicyZeroBudget(t *testing.T) {
	got := BudgetFromPolicy(config.Policy{})
	if got != (Budget{}) {
		t.Errorf("BudgetFromPolicy(zero) = %+v, want zero Budget (uncapped tokens)", got)
	}
	// New must still turn the uncapped budget into a terminating loop.
	if l := New(func(Invocation) ([]Tool, func(), error) { return nil, nil, nil }, got, nil); l.budget.MaxTurns != DefaultMaxTurns {
		t.Errorf("New defaulted MaxTurns = %d, want DefaultMaxTurns=%d", l.budget.MaxTurns, DefaultMaxTurns)
	}
}
