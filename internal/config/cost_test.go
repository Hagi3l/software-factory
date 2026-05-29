package config

import (
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// TestModelCostUSD pins the tokens→USD conversion the orchestrator enforces the dollar
// budget against (T3.8). Each dimension bills at its own per-million rate and the four
// terms sum, so a mistake in one rate or the per-million divisor would silently mis-meter
// spend — the figure a budget breach is judged on.
func TestModelCostUSD(t *testing.T) {
	cost := ModelCost{
		InputPerMTok:      15,
		OutputPerMTok:     75,
		CacheWritePerMTok: 18.75,
		CacheReadPerMTok:  1.5,
	}
	u := core.Usage{
		InputTokens:         1_000_000, // $15
		OutputTokens:        2_000_000, // $150
		CacheCreationTokens: 1_000_000, // $18.75
		CacheReadTokens:     4_000_000, // $6
	}
	got := cost.USD(u) // 15 + 150 + 18.75 + 6 = 189.75
	if got < 189.749 || got > 189.751 {
		t.Errorf("USD = %v, want 189.75 (each dimension priced at its own per-million rate)", got)
	}
}

// TestModelCostUSDEmpty proves an unpriced model contributes $0: a model with no cost
// block (or a deployment that prices nothing) adds nothing to USD accounting, so spend
// stays bounded by the token and retry caps, which never depend on the cost table (T3.8).
func TestModelCostUSDEmpty(t *testing.T) {
	if got := (ModelCost{}).USD(core.Usage{InputTokens: 9_999_999, OutputTokens: 9_999_999}); got != 0 {
		t.Errorf("USD = %v, want 0 for an empty cost table", got)
	}
}
