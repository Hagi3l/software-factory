package core

// ExploreBudget is the fixed per-call cap on the explore tool's nested read-only sub-loop
// (specs/components/agent.md "Explore", specs/models.md "Helper souls", specs/configuration.md
// `policy.explore_budget`). It is dimensioned in tokens + turns — a sub-loop concept, unlike
// the per-issue Budget's tokens/USD/wall — and is deliberately fixed (not a fraction of the
// parent's remaining budget) so an explore call behaves identically wherever in an invocation
// it is made. A zero value in a dimension leaves that dimension to the loop's own default; a
// wholly-zero ExploreBudget means the feature is off (see Enabled).
//
// It lives in core because it crosses the orchestrator→runner boundary on the Brief (like
// Soul): the trusted dispatch pins it so the runner meters the explorer's model stream against
// it. config.ExploreBudget is a type alias for this, so the config surface and the wire type
// are one thing, not a mapped pair.
type ExploreBudget struct {
	Tokens int `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	Turns  int `yaml:"turns,omitempty" json:"turns,omitempty"`
}

// Enabled reports whether explore is switched on: any positive dimension turns the helper loop
// on, an all-zero block leaves it off. This is the single predicate the runner, the
// orchestrator's Brief builder, and config validation branch on so "omitting the block disables
// explore" is expressed in exactly one place.
func (e ExploreBudget) Enabled() bool { return e.Tokens > 0 || e.Turns > 0 }
