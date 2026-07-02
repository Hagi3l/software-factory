package config

import (
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// setRoleTools points every soul fulfilling a role at a tools allowlist, mirroring
// setRoleModel so the explore-enablement tests don't couple to fullSouls' ordering.
func setRoleTools(souls []core.Soul, role string, tools ...string) {
	for i := range souls {
		if souls[i].Role == role {
			souls[i].Tools = tools
		}
	}
}

// enableExplore switches the feature on with a fixed sub-budget, exactly as
// policy.explore_budget does in a real config.
func enableExplore(c *Config) {
	c.Harness.Policy.ExploreBudget = ExploreBudget{Tokens: 100_000, Turns: 12}
}

// exploreSoul builds an explorer-role soul with a read-only allowlist (so it passes
// validateExplore) and the given selector, so several explorers can coexist without a
// selector clash.
func exploreSoul(t *testing.T, name string, selector map[string]string) core.Soul {
	t.Helper()
	s := soul(t, name, RoleExplorer)
	s.Tools = []string{"read_file", "search"}
	s.Selector = selector
	return s
}

// exploreWarnFragment is a stable substring unique to the verify-path explorer advisory (it
// says "verify-path role", where the model-diversity advisory says "verifier role"), so a test
// can assert the explore advisory's presence/absence independently of the model-diversity one
// (which also fires on the same-family canonical config).
const exploreWarnFragment = "both enable `explore`"

// mustNotWarnFragment asserts no advisory contains fragment (other, unrelated advisories may
// still be present — unlike mustNotWarn, which requires none at all).
func mustNotWarnFragment(t *testing.T, ws []string, fragment string) {
	t.Helper()
	for _, w := range ws {
		if strings.Contains(w, fragment) {
			t.Fatalf("no advisory should contain %q; got %q", fragment, w)
		}
	}
}

// With explore enabled, the producer (implementor) and its verifier (security) both opting into
// `explore`, and only a single-family explorer pool, the verify path cannot be routed to a diverse
// explorer — so it inherits the producer's explorer family and the advisory fires (T12.5, extending
// the T2.13 family-overlap advisory to the explore tool; specs/verification.md, configuration.md).
func TestWarnVerifyPathExplorerSameFamily(t *testing.T) {
	c := validConfig()
	enableExplore(c)
	souls := fullSouls(t)
	setRoleTools(souls, "implementor", "explore")
	setRoleTools(souls, "security", "explore")
	souls = append(souls, exploreSoul(t, "explorer", map[string]string{})) // one anthropic explorer
	c.Souls = souls

	if err := c.Validate(); err != nil {
		t.Fatalf("config should be valid: %v", err)
	}
	ws := c.Warnings()
	mustWarn(t, ws, exploreWarnFragment)
	mustWarn(t, ws, `producer role "implementor"`)
	mustWarn(t, ws, `verify-path role "security"`)
	mustWarn(t, ws, `family "anthropic"`)
}

// A second explorer on a DIFFERENT model family (routed by a verify tag) is exactly the
// recommended fix, so the explore advisory must fall silent — the verify path can now select the
// diverse explorer. (The model-diversity advisory over the souls' own models may still fire, so
// this asserts the explore fragment is absent rather than that there are no advisories at all.)
func TestNoWarnVerifyPathDiverseExplorer(t *testing.T) {
	c := validConfig()
	enableExplore(c)
	c.Infra.Models["gpt-4o"] = ModelProvider{Provider: ProviderOpenAI}
	souls := fullSouls(t)
	setRoleTools(souls, "implementor", "explore")
	setRoleTools(souls, "security", "explore")
	// Producer-path default explorer (anthropic, catch-all) + a diverse verify-path explorer on a
	// different family, routed by a selector tag.
	def := exploreSoul(t, "explorer", map[string]string{})
	verify := exploreSoul(t, "explorer-verify", map[string]string{"verify": "1"})
	verify.Model = "gpt-4o"
	souls = append(souls, def, verify)
	c.Souls = souls

	if err := c.Validate(); err != nil {
		t.Fatalf("config should be valid: %v", err)
	}
	mustNotWarnFragment(t, c.Warnings(), exploreWarnFragment)
}

// Explore off (no policy.explore_budget) means no explorer runs at all, so the advisory must not
// fire even when the producer/verifier souls happen to list `explore` in their allowlists.
func TestNoWarnExploreDisabled(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	setRoleTools(souls, "implementor", "explore")
	setRoleTools(souls, "security", "explore")
	c.Souls = souls
	mustNotWarnFragment(t, c.Warnings(), exploreWarnFragment)
}

// When the verify path does NOT enable `explore`, it never invokes an explorer, so there is no
// shared upstream to correlate — the advisory must not fire even with a single-family explorer pool
// and the producer using explore.
func TestNoWarnVerifyPathExploreNotEnabled(t *testing.T) {
	c := validConfig()
	enableExplore(c)
	souls := fullSouls(t)
	setRoleTools(souls, "implementor", "explore") // producer opts in; verifier does not
	souls = append(souls, exploreSoul(t, "explorer", map[string]string{}))
	c.Souls = souls

	if err := c.Validate(); err != nil {
		t.Fatalf("config should be valid: %v", err)
	}
	mustNotWarnFragment(t, c.Warnings(), exploreWarnFragment)
}

// The advisory is non-fatal: a single-family-explorer config with both paths using explore is
// still sound (Validate returns nil), and only Warnings surfaces the concern — the contract that
// lets harness validate print it and still exit 0.
func TestExploreDiversityNeverFailsValidate(t *testing.T) {
	c := validConfig()
	enableExplore(c)
	souls := fullSouls(t)
	setRoleTools(souls, "implementor", "explore")
	setRoleTools(souls, "security", "explore")
	souls = append(souls, exploreSoul(t, "explorer", map[string]string{}))
	c.Souls = souls
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate must stay nil for a single-family-explorer config; got %v", err)
	}
	mustWarn(t, c.Warnings(), exploreWarnFragment)
}
