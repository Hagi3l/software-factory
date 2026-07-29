package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
	"github.com/Loxstomper/software-factory/internal/messaging"
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
	var alert core.DLQAlert
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
	var alert core.DLQAlert
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

// --- T3.8b: cumulative wall-clock --------------------------------------------

// TestRouteThreadsCumulativeWall proves a routed failure adds the just-finished invocation's
// elapsed wall (Result.Elapsed, stamped by the runner) onto the chain's running total and
// threads the sum onto the fix issue, exactly as it threads SpentTokens/SpentUSD. This is the
// accumulation half of the cumulative wall budget (T3.8b): the orchestrator must carry wall
// across the on_failure loop to enforce a cumulative cap, not a per-attempt one.
func TestRouteThreadsCumulativeWall(t *testing.T) {
	bd := newFakeBeads()
	iss := inProgress("iss-1", "implement", 0)
	iss.SpentWall = 30 * time.Second // earlier attempts in this chain
	bd.put(iss)
	o, _ := newOrch(t, kernelConfig(3), bd, &fakeGate{}, &fakeMerger{}) // ample retries, no wall cap

	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed, Elapsed: 20 * time.Second}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 0 {
		t.Fatalf("blocked = %v, want none (no wall cap)", blocked)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	if applied[0].Issue.SpentWall != 50*time.Second {
		t.Errorf("SpentWall = %s, want 50s (30s prior + 20s this attempt)", applied[0].Issue.SpentWall)
	}
}

// TestRouteDeadLettersOnWallBudgetBreach proves the cumulative wall budget terminates a chain
// the retry cap would still allow: with retries to spare, a running wall total that reaches
// policy.budget.wall dead-letters the issue rather than spawning another attempt. This is the
// cross-loop wall cap, distinct from the per-invocation sandbox ceiling (see specs/workflow.md).
func TestRouteDeadLettersOnWallBudgetBreach(t *testing.T) {
	bd := newFakeBeads()
	iss := inProgress("iss-1", "implement", 0) // retry cap (5) nowhere near hit
	iss.SpentWall = 90 * time.Second
	bd.put(iss)

	cfg := kernelConfig(5)
	cfg.Harness.Policy.Budget = config.Budget{Wall: config.Duration(2 * time.Minute)}
	o, nc := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	// 90s prior + 45s this = 135s >= 120s cap.
	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed, Elapsed: 45 * time.Second}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-1" {
		t.Errorf("blocked = %v, want [iss-1] (wall budget exhausted)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no fix issue past the wall budget", applied)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert core.DLQAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if !strings.Contains(alert.Reason, "wall budget") {
		t.Errorf("alert reason = %q, want a wall-budget reason", alert.Reason)
	}
}

// --- T3.8b: epic budget (aggregate read) -------------------------------------

// TestRouteThreadsEpicIDFromRoot proves EpicID threads forward onto a fix issue. A root seed
// carries no EpicID (it is its own epic), so its fix carries the root's own id — the epicOf
// fallback, mirroring how Base falls back to the pipeline base. From there every descendant of
// the epic shares that id, which is the key the epic budget aggregates over (see epicOf).
func TestRouteThreadsEpicIDFromRoot(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0)) // a root: no EpicID
	o, _ := newOrch(t, kernelConfig(3), bd, &fakeGate{}, &fakeMerger{})

	res := core.Result{IssueID: "iss-1", Status: core.StatusFailed}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	if applied[0].Issue.EpicID != "iss-1" {
		t.Errorf("EpicID = %q, want iss-1 (root's own id via epicOf fallback)", applied[0].Issue.EpicID)
	}
}

// TestHandleResultStampsClosingSpendWhenEpicBudgetConfigured proves the orchestrator records an
// issue's own-invocation marginal (ClosingTokens/USD) when an epic budget is configured — the
// per-issue figure the epic aggregate sums — and skips the write entirely when none is, so a
// config not using the feature pays nothing for it.
func TestHandleResultStampsClosingSpendWhenEpicBudgetConfigured(t *testing.T) {
	ctx := context.Background()
	usage := core.Usage{InputTokens: 10000, OutputTokens: 2000} // 12k tokens, priced $0.30 by costedConfig

	t.Run("configured: stamped", func(t *testing.T) {
		bd := newFakeBeads()
		bd.put(inProgress("iss-1", "implement", 0))
		cfg := costedConfig(5)
		cfg.Harness.Policy.EpicBudget = config.Budget{USD: 100} // configured but ample
		o, _ := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})

		if _, err := o.handleResult(ctx, core.Result{IssueID: "iss-1", Status: core.StatusFailed, Usage: usage}); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		got, _ := bd.Get(ctx, "iss-1")
		if got.ClosingTokens != 12000 {
			t.Errorf("ClosingTokens = %d, want 12000 (marginal stamped)", got.ClosingTokens)
		}
		if got.ClosingUSD < 0.299 || got.ClosingUSD > 0.301 {
			t.Errorf("ClosingUSD = %v, want ~0.30 (marginal priced and stamped)", got.ClosingUSD)
		}
	})

	t.Run("unconfigured: not stamped", func(t *testing.T) {
		bd := newFakeBeads()
		bd.put(inProgress("iss-1", "implement", 0))
		o, _ := newOrch(t, costedConfig(5), bd, &fakeGate{}, &fakeMerger{}) // no epic budget

		if _, err := o.handleResult(ctx, core.Result{IssueID: "iss-1", Status: core.StatusFailed, Usage: usage}); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		got, _ := bd.Get(ctx, "iss-1")
		if got.ClosingTokens != 0 || got.ClosingUSD != 0 {
			t.Errorf("ClosingTokens/USD = %d/%v, want 0/0 (no epic budget → no stamp)", got.ClosingTokens, got.ClosingUSD)
		}
	})
}

// TestRouteDeadLettersOnEpicBudgetBreach proves the cross-issue epic budget is enforced as an
// AGGREGATE: the orchestrator sums every issue's closing spend over the epic — a closed sibling
// plus the just-finished invocation (stamped before the check) — and dead-letters with an
// epic-budget escalation when the sum reaches the cap, even though this issue's own per-issue
// budget and retry cap are nowhere near hit. This is the cap a fan-out's threaded counter could
// not express (it would double-count the shared prefix; see specs/workflow.md "epic_budget").
func TestRouteDeadLettersOnEpicBudgetBreach(t *testing.T) {
	bd := newFakeBeads()
	// A sibling of the same epic already closed having spent $0.18.
	bd.put(core.Issue{ID: "sib-1", Role: "implement", Status: "closed", EpicID: "root-1", ClosingUSD: 0.18})
	iss := inProgress("iss-2", "implement", 0)
	iss.EpicID = "root-1"
	bd.put(iss)

	cfg := costedConfig(5) // priced opus; no per-issue budget (only the epic cap bites)
	cfg.Harness.Policy.EpicBudget = config.Budget{USD: 0.40}
	o, nc := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	// This invocation's marginal is $0.30; epic total 0.18 + 0.30 = 0.48 >= 0.40 cap.
	res := core.Result{IssueID: "iss-2", Status: core.StatusFailed,
		Usage: core.Usage{InputTokens: 10000, OutputTokens: 2000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-2" {
		t.Errorf("blocked = %v, want [iss-2] (epic budget exhausted)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no fix issue past the epic budget", applied)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert core.DLQAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if !strings.Contains(alert.Reason, "epic USD budget") {
		t.Errorf("alert reason = %q, want an epic-budget reason", alert.Reason)
	}
}

// TestRouteUnderEpicBudgetProceeds proves the aggregate check is not over-eager: with the same
// epic spend safely under a higher cap, the failure routes to a fix issue carrying the epic id,
// rather than dead-lettering.
func TestRouteUnderEpicBudgetProceeds(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "sib-1", Role: "implement", Status: "closed", EpicID: "root-1", ClosingUSD: 0.18})
	iss := inProgress("iss-2", "implement", 0)
	iss.EpicID = "root-1"
	bd.put(iss)

	cfg := costedConfig(5)
	cfg.Harness.Policy.EpicBudget = config.Budget{USD: 1.00} // 0.48 total is well under
	o, _ := newOrch(t, cfg, bd, &fakeGate{}, &fakeMerger{})

	res := core.Result{IssueID: "iss-2", Status: core.StatusFailed,
		Usage: core.Usage{InputTokens: 10000, OutputTokens: 2000}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 0 {
		t.Fatalf("blocked = %v, want none (under epic budget)", blocked)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix issue", applied)
	}
	if applied[0].Issue.EpicID != "root-1" {
		t.Errorf("EpicID = %q, want root-1 (threaded onto the fix)", applied[0].Issue.EpicID)
	}
}

// TestAdvanceDeadLettersOnEpicBudgetBreach proves the epic budget also guards the success path:
// when a passing gate would advance to the next AGENT stage but the epic is over budget, the
// orchestrator dead-letters rather than fanning out more sandboxed work. (The terminal trusted
// merge is deliberately not gated — it burns no agent spend — but advancing author-tests →
// implement does.)
func TestAdvanceDeadLettersOnEpicBudgetBreach(t *testing.T) {
	bd := newFakeBeads()
	// A sibling of the epic already over the cap on its own.
	bd.put(core.Issue{ID: "sib", Role: "implement", Status: "closed", EpicID: "root-1", ClosingUSD: 0.50})
	iss := inProgress("at-1", "test-author", 0)
	iss.EpicID = "root-1"
	bd.put(iss)

	cfg := planConfig(5) // author-tests produces implement (an agent stage)
	cfg.Harness.Policy.EpicBudget = config.Budget{USD: 0.40}
	o, nc := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	res := core.Result{IssueID: "at-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("at-1")}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "at-1" {
		t.Errorf("blocked = %v, want [at-1] (epic budget exhausted before advancing)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no produced implement issue past the epic budget", applied)
	}
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none (the dead-lettered issue is blocked, not closed)", closed)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert core.DLQAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if !strings.Contains(alert.Reason, "epic USD budget") {
		t.Errorf("alert reason = %q, want an epic-budget reason", alert.Reason)
	}
}

// TestAcceptPlanThreadsEpicIDOntoChildren proves the orchestrator stamps the epic id onto each
// planner-proposed child (EpicID is the orchestrator's to assign, never trusted from the agent's
// Result). A root plan issue carries no EpicID, so its children inherit the plan issue's own id
// via epicOf — binding the whole decomposition to one epic.
func TestAcceptPlanThreadsEpicIDOntoChildren(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0)) // root: no EpicID
	o, _ := newOrch(t, planConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone,
		Proposes: []core.Proposal{
			{Issue: core.Issue{Title: "slice A", Role: "test-author"}, Key: "a"},
			{Issue: core.Issue{Title: "slice B", Role: "test-author"}, DependsOn: []string{"a"}},
		},
	}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 2 {
		t.Fatalf("applied = %+v, want two children", applied)
	}
	for i, p := range applied {
		if p.Issue.EpicID != "plan-1" {
			t.Errorf("child %d EpicID = %q, want plan-1 (epic root threaded onto the decomposition)", i, p.Issue.EpicID)
		}
	}
}
