package agent

import "github.com/Loxstomper/software-factory/internal/config"

// BudgetFromPolicy derives a per-invocation Budget from the operator's termination
// policy (factory.yaml). It is the single source of truth for translating config
// into the loop's caps, so cmd/software-factory (plan T1.21) wires the loop with
// agent.New(tools, agent.BudgetFromPolicy(cfg.Harness.Policy), log) rather than
// re-deriving the mapping at the call site.
//
// The translation is deliberately narrow, because the two budget scopes the specs
// define (see specs/workflow.md "two levels") are NOT the same thing:
//
//   - config.Policy.Budget is the *per-issue, cumulative* budget — the ceiling
//     summed across every invocation in the on_failure feedback loop. Enforcing
//     it across retries (and dead-lettering a breach) is the orchestrator's job
//     (plan T1.19), which tallies Usage from the broker.
//   - agent.Budget is the *per-invocation* cap — the loop's own halting guarantee
//     for one invocation.
//
// Only the token dimension carries over: the per-issue token budget becomes the
// per-invocation hard ceiling, so a single invocation can never consume more than
// the whole issue is ever allowed (a runaway invocation is bounded even before the
// orchestrator's finer cumulative accounting kicks in). A zero token budget stays
// zero — uncapped on tokens, with MaxTurns still bounding the loop.
//
// The other dimensions are intentionally not mapped here:
//
//   - turns: the spec lists turns as a per-invocation dimension, but the per-issue
//     policy budget models only tokens/usd/wall; the turn cap is the loop's own knob
//     and defaults to DefaultMaxTurns (see New). The operator's per-invocation turn
//     knob lives on the *soul* (core.Soul.MaxTurns, yaml max_tool_turns), applied per
//     invocation in Loop.Invoke — not derived from policy here.
//   - wall: enforced by the sandbox watchdog (plan T1.6), not the loop.
//   - usd: needs a per-model cost table to convert tokens→dollars; deferred.
//
// MaxOutputTokens is left 0 (the adapter default): config carries no per-call
// output ceiling, and the cumulative MaxTokens cap is what bounds spend.
func BudgetFromPolicy(p config.Policy) Budget {
	return Budget{MaxTokens: p.Budget.Tokens}
}
