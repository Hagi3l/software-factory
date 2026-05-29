package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
)

// costedConfig is kernelConfig wired with a priced model so a failure-routing test can
// exercise the tokens→USD conversion: the implementor soul names model "opus", and the
// infra registry prices it at full Opus rates. maxRetries is left high so only the budget
// (not the retry cap) can terminate the chain in these tests.
func costedConfig(maxRetries int) *config.Config {
	cfg := kernelConfig(maxRetries)
	cfg.Souls = []core.Soul{{Name: "implementor-go", Role: "implement", Sandbox: "go-toolchain", Model: "opus"}}
	cfg.Infra = &config.Infra{Models: map[string]config.ModelProvider{
		"opus": {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 15, OutputPerMTok: 75}},
	}}
	return cfg
}

// TestRouteThreadsAndPricesCumulativeSpend proves a routed failure adds the just-finished
// invocation's spend — tokens from Result.Usage, dollars priced through the selected
// model's cost table — onto the running chain total and threads the sum onto the fix
// issue, exactly as Attempt is threaded. This is the accumulation half of the per-issue
// budget (T3.8): the orchestrator must carry spend across the on_failure loop to enforce a
// cumulative cap, not a per-attempt one (see specs/workflow.md).
func TestRouteThreadsAndPricesCumulativeSpend(t *testing.T) {
	bd := newFakeBeads()
	iss := inProgress("iss-1", "implement", 0)
	iss.SpentTokens = 1000 // earlier attempts in this chain
	iss.SpentUSD = 0.05
	bd.put(iss)

	o, _ := newOrch(t, costedConfig(3), bd, &fakeGate{}, &fakeMerger{}) // ample retries, no budget cap

	// StatusFailed routes without a gate. Usage 10k input + 2k output = 12k tokens, priced
	// 10000*15/1e6 + 2000*75/1e6 = 0.15 + 0.15 = $0.30.
	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed,
		Usage: core.Usage{InputTokens: 10000, OutputTokens: 2000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 0 {
		t.Fatalf("blocked = %v, want none (under budget)", blocked)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	got := applied[0].Issue
	if got.SpentTokens != 13000 {
		t.Errorf("SpentTokens = %d, want 13000 (1000 prior + 12000 this attempt)", got.SpentTokens)
	}
	if got.SpentUSD < 0.349 || got.SpentUSD > 0.351 {
		t.Errorf("SpentUSD = %v, want ~0.35 (0.05 prior + 0.30 this attempt)", got.SpentUSD)
	}
}

// TestRouteDeadLettersOnTokenBudgetBreach proves the cumulative token budget terminates a
// chain the retry cap would still allow: with retries to spare, a running token total that
// reaches policy.budget.tokens dead-letters the issue rather than spawning another attempt.
// This is the gap the retry cap alone leaves open — a spec the factory cannot satisfy could
// burn unbounded tokens within the cap (see specs/workflow.md termination).
func TestRouteDeadLettersOnTokenBudgetBreach(t *testing.T) {
	bd := newFakeBeads()
	iss := inProgress("iss-1", "implement", 0) // retry cap (5) nowhere near hit
	iss.SpentTokens = 8000
	bd.put(iss)

	cfg := kernelConfig(5)
	cfg.Harness.Policy.Budget = config.Budget{Tokens: 10000}
	o, nc := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	// 8000 prior + 5000 this = 13000 >= 10000 cap.
	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed, Usage: core.Usage{InputTokens: 5000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-1" {
		t.Errorf("blocked = %v, want [iss-1] (token budget exhausted)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no fix issue past the budget", applied)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert dlqAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if alert.IssueID != "iss-1" || !strings.Contains(alert.Reason, "token budget") {
		t.Errorf("alert = %+v, want a token-budget reason", alert)
	}
}

// TestRouteDeadLettersOnUSDBudgetBreach proves the dollar budget terminates a chain once
// the priced cumulative spend reaches policy.budget.usd. It exercises the full path the
// token-budget test does not: usage priced through the model cost table, then compared to
// the USD cap (T3.8).
func TestRouteDeadLettersOnUSDBudgetBreach(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))

	cfg := costedConfig(5)
	cfg.Harness.Policy.Budget = config.Budget{USD: 0.20}
	o, nc := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	// priced 10000*15/1e6 + 2000*75/1e6 = $0.30 >= $0.20 cap.
	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed,
		Usage: core.Usage{InputTokens: 10000, OutputTokens: 2000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 {
		t.Errorf("blocked = %v, want the issue dead-lettered (USD budget exhausted)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no fix issue past the budget", applied)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert dlqAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if !strings.Contains(alert.Reason, "USD budget") {
		t.Errorf("alert reason = %q, want a USD-budget reason", alert.Reason)
	}
}

// TestRouteUnpricedModelAccruesNoUSD proves token accounting is independent of the cost
// table: with no infra overlay (so no model price), a routed failure still threads the
// token spend forward but prices $0. A deployment that configures no costs is still bounded
// by the token and retry caps — the USD dimension simply contributes nothing (T3.8).
func TestRouteUnpricedModelAccruesNoUSD(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, _ := newOrch(t, kernelConfig(3), bd, &fakeGate{}, &fakeMerger{}) // no Infra → no cost table

	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed,
		Usage: core.Usage{InputTokens: 7000, OutputTokens: 3000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	if applied[0].Issue.SpentTokens != 10000 {
		t.Errorf("SpentTokens = %d, want 10000 (token accounting works with no cost table)", applied[0].Issue.SpentTokens)
	}
	if applied[0].Issue.SpentUSD != 0 {
		t.Errorf("SpentUSD = %v, want 0 (no cost table → unpriced)", applied[0].Issue.SpentUSD)
	}
}
