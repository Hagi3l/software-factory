package config

import (
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// setRoleModel points every soul fulfilling a role at a model, by role rather than slice
// index so the tests don't couple to fullSouls' ordering.
func setRoleModel(souls []core.Soul, role, model string) {
	for i := range souls {
		if souls[i].Role == role {
			souls[i].Model = model
		}
	}
}

// mustWarn asserts at least one advisory contains fragment.
func mustWarn(t *testing.T, ws []string, fragment string) {
	t.Helper()
	for _, w := range ws {
		if strings.Contains(w, fragment) {
			return
		}
	}
	t.Fatalf("no warning contained %q; got %v", fragment, ws)
}

// mustNotWarn asserts there are no advisories at all.
func mustNotWarn(t *testing.T, ws []string) {
	t.Helper()
	if len(ws) != 0 {
		t.Fatalf("expected no warnings, got %v", ws)
	}
}

// setStagePostcondition replaces a stage's postconditions (DAG values are structs, so the
// whole value must be reassigned).
func setStagePostcondition(c *Config, name string, pc []string) {
	st := c.Harness.DAG[name]
	st.Postcondition = pc
	c.Harness.DAG[name] = st
}

// A package_proxy set without the package-proxy destination allowlisted is dead config —
// fetches are still denied — so it warns (non-fatal); allowlisting it clears the advisory.
func TestWarnPackageProxyNotAllowlisted(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Broker.PackageProxy = "https://proxy.golang.org"
	c.Infra.Broker.Allowlist = []string{"llm-api", "git"} // package-proxy missing
	mustWarn(t, c.Warnings(), "package_proxy is set")

	c.Infra.Broker.Allowlist = []string{"git", DestPackageProxy}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "package_proxy is set") {
			t.Errorf("allowlisted package-proxy must not warn, got %q", w)
		}
	}
}

func TestWarnGitRemoteNotAllowlisted(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Git.Remote = "https://github.com/acme/widgets.git"
	c.Infra.Broker.Allowlist = []string{"llm-api", "nats"} // git missing
	mustWarn(t, c.Warnings(), "git.remote is set")

	c.Infra.Broker.Allowlist = []string{"llm-api", DestGitPush}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "git.remote is set") {
			t.Errorf("allowlisted git must not warn, got %q", w)
		}
	}
}

// The canonical config runs both the implementor (producer) and the security reviewer
// (qa verifier) on the anthropic provider, so validate must advise the shared family —
// this is the N-version diversity warning T2.13 adds (specs/verification.md).
func TestWarnSameProviderProducerVerifier(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t) // every soul defaults to claude-opus-4-7 (anthropic)
	ws := c.Warnings()
	mustWarn(t, ws, `producer role "implementor"`)
	mustWarn(t, ws, `verifier role "security"`)
	mustWarn(t, ws, `family "anthropic"`)
}

// A verifier on a different provider than the producer is exactly the recommended
// configuration, so it must draw no advisory.
func TestNoWarnDifferentProvider(t *testing.T) {
	c := validConfig()
	c.Infra.Models["gpt-4o"] = ModelProvider{Provider: ProviderOpenAI}
	souls := fullSouls(t)
	setRoleModel(souls, "security", "gpt-4o") // verifier now openai, producer stays anthropic
	c.Souls = souls
	mustNotWarn(t, c.Warnings())
}

// With no gate stage downstream of the producer there is no verifier to compare against,
// so nothing is advised (here the qa stage's gate postconditions are stripped, leaving no
// candidate-grading check anywhere).
func TestNoWarnNoGateStage(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	setStagePostcondition(c, "qa", nil)
	mustNotWarn(t, c.Warnings())
}

// With no producer stage (no red→green proof anywhere) there is nothing to pair a gate
// verifier with, so nothing is advised.
func TestNoWarnNoProducer(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	setStagePostcondition(c, "implement", nil)
	mustNotWarn(t, c.Warnings())
}

// The advisory is non-fatal by construction: a same-family producer/verifier config is
// still sound (Validate returns nil) and only Warnings surfaces the concern. This is the
// contract that lets harness validate print it and still exit 0.
func TestWarnNeverFailsValidate(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t) // same-provider producer+verifier
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate must stay nil for a same-family config; got %v", err)
	}
	if len(c.Warnings()) == 0 {
		t.Fatal("expected a model-diversity advisory for the same-family config")
	}
}

// A *bare-slug* openai-compat model carries no vendor in its registry key, so its family
// falls back to the provider tag — two distinct such models then read as one family. The
// advisory names this residual blind spot and points at `family:` to disambiguate (the
// aggregator-slug case, where the vendor IS in the key, is handled accurately — see
// TestNoWarnDifferentVendorBehindOneGateway).
func TestWarnBareSlugOpenAICompatNotesVendorBlindSpot(t *testing.T) {
	c := validConfig()
	c.Infra.Models["local-a"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: "http://a:11434/v1"}
	c.Infra.Models["local-b"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: "http://b:11434/v1"}
	souls := fullSouls(t)
	setRoleModel(souls, "implementor", "local-a")
	setRoleModel(souls, "security", "local-b")
	c.Souls = souls
	ws := c.Warnings()
	mustWarn(t, ws, `family "openai-compat"`)
	mustWarn(t, ws, "set `family:` on the registry entry")
}

// The headline OpenRouter fix: two models served by ONE openai-compat gateway (same
// provider, same endpoint) but naming DIFFERENT vendors in their "vendor/model" slugs are
// genuinely different families, so the producer/verifier diversity check must NOT warn —
// the old provider-keyed heuristic falsely flagged this.
func TestNoWarnDifferentVendorBehindOneGateway(t *testing.T) {
	c := validConfig()
	const ep = "https://openrouter.ai/api/v1"
	c.Infra.Models["deepseek/deepseek-v4-flash"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep}
	c.Infra.Models["anthropic/claude-3.5-sonnet"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep}
	souls := fullSouls(t)
	setRoleModel(souls, "implementor", "deepseek/deepseek-v4-flash")
	setRoleModel(souls, "security", "anthropic/claude-3.5-sonnet")
	c.Souls = souls
	mustNotWarn(t, c.Warnings())
}

// Conversely, two slugs from the SAME vendor behind one gateway (deepseek flash vs pro —
// the vault demo's actual shape) share a family and must warn, named by the vendor prefix
// rather than the gateway provider.
func TestWarnSameVendorBehindOneGateway(t *testing.T) {
	c := validConfig()
	const ep = "https://openrouter.ai/api/v1"
	c.Infra.Models["deepseek/deepseek-v4-flash"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep}
	c.Infra.Models["deepseek/deepseek-v4-pro"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep}
	souls := fullSouls(t)
	setRoleModel(souls, "implementor", "deepseek/deepseek-v4-flash")
	setRoleModel(souls, "security", "deepseek/deepseek-v4-pro")
	c.Souls = souls
	mustWarn(t, c.Warnings(), `family "deepseek"`)
}

// warnEffortNoOp: an Anthropic-family openai-compat model that carries effort via
// effort_param: reasoning is valid config (Validate passes) but a silent no-op on Claude 4.6+/5
// — reasoning.effort is ignored there — so the advisory flags it and points at verbosity. The
// entry is unreferenced by any soul, so only the effort advisory can fire.
func TestWarnEffortReasoningNoOpOnAnthropicSlug(t *testing.T) {
	c := validConfig()
	c.Infra.Models["anthropic/claude-sonnet-5"] = ModelProvider{
		Provider: ProviderOpenAICompat, Endpoint: "https://openrouter.ai/api/v1",
		Effort: "medium", EffortParam: EffortParamReasoning,
	}
	mustWarn(t, c.Warnings(), "silent no-op")
}

// The correct transport (verbosity) for the same Anthropic slug draws no advisory.
func TestNoWarnEffortVerbosityOnAnthropicSlug(t *testing.T) {
	c := validConfig()
	c.Infra.Models["anthropic/claude-sonnet-5"] = ModelProvider{
		Provider: ProviderOpenAICompat, Endpoint: "https://openrouter.ai/api/v1",
		Effort: "medium", EffortParam: EffortParamVerbosity,
	}
	mustNotWarn(t, c.Warnings())
}

// An explicit `family:` is the operator's declared truth and wins over the slug/provider
// inference — here it collapses two different-vendor slugs into one declared family, so the
// check warns; the inverse (declaring two bare-slug models distinct) is what clears the
// fallback blind spot in TestWarnBareSlugOpenAICompatNotesVendorBlindSpot.
func TestExplicitFamilyOverridesInference(t *testing.T) {
	c := validConfig()
	const ep = "https://openrouter.ai/api/v1"
	c.Infra.Models["deepseek/deepseek-v4-flash"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep, Family: "house"}
	c.Infra.Models["anthropic/claude-3.5-sonnet"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: ep, Family: "house"}
	souls := fullSouls(t)
	setRoleModel(souls, "implementor", "deepseek/deepseek-v4-flash")
	setRoleModel(souls, "security", "anthropic/claude-3.5-sonnet")
	c.Souls = souls
	mustWarn(t, c.Warnings(), `family "house"`)
}

// ModelFamily resolves most-authoritative-first: explicit family, then aggregator-slug
// vendor prefix, then the provider tag — and lowercases the result.
func TestModelFamily(t *testing.T) {
	cases := []struct {
		name  string
		mp    ModelProvider
		model string
		want  string
	}{
		{"explicit family wins", ModelProvider{Provider: ProviderOpenAICompat, Family: "DeepSeek"}, "anything/x", "deepseek"},
		{"slug vendor prefix", ModelProvider{Provider: ProviderOpenAICompat}, "Deepseek/deepseek-v4-pro", "deepseek"},
		{"direct provider, bare slug", ModelProvider{Provider: ProviderAnthropic}, "claude-opus-4-8", "anthropic"},
		{"bare-slug compat falls back to provider", ModelProvider{Provider: ProviderOpenAICompat}, "llama3", "openai-compat"},
		{"leading slash is not a vendor prefix", ModelProvider{Provider: ProviderOpenAI}, "/weird", "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mp.ModelFamily(tc.model); got != tc.want {
				t.Fatalf("ModelFamily(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

// A role may resolve to several souls (selector-based), so the producer/verifier
// comparison is over provider *sets*: an overlap on any shared provider triggers the
// advisory even when the verifier role also carries an independent-family soul.
func TestWarnRoleProviderSetIntersection(t *testing.T) {
	c := validConfig()
	c.Infra.Models["gpt-4o"] = ModelProvider{Provider: ProviderOpenAI}
	souls := fullSouls(t) // implementor: anthropic
	// Two security souls: one shares the producer's anthropic family, one is independent.
	securityShared := soul(t, "security-anthropic", "security")
	securityShared.Selector = map[string]string{"lang": "go"}
	securityIndependent := soul(t, "security-openai", "security")
	securityIndependent.Model = "gpt-4o"
	securityIndependent.Selector = map[string]string{"lang": "rust"}
	// Drop fullSouls' default (empty-selector) security soul to avoid a selector clash.
	var kept []core.Soul
	for _, s := range souls {
		if s.Role != "security" {
			kept = append(kept, s)
		}
	}
	kept = append(kept, securityShared, securityIndependent)
	c.Souls = kept
	if err := c.Validate(); err != nil {
		t.Fatalf("config should be valid: %v", err)
	}
	mustWarn(t, c.Warnings(), `family "anthropic"`)
}

// The conflict-spawned resolve stage is gated but is not produced by implement and does
// not independently review the implementor, so it must not be treated as implement's
// verifier — only the produced qa gate is. Here resolve runs a different family than the
// producer while qa matches it: the warning must fire for qa, and naming resolve at all
// would be a false signal, so assert the advisory is exactly about qa/security.
func TestResolveStageIsNotImplementVerifier(t *testing.T) {
	c := validConfig()
	c.Infra.Models["gpt-4o"] = ModelProvider{Provider: ProviderOpenAI}
	// Add a resolve stage (gated, conflict-spawned — reached by no produces edge).
	c.Harness.DAG["resolve"] = Stage{
		Role:          "merge-resolver",
		Postcondition: []string{"tests-pass", "gosec"},
		OnFailure:     "resolve",
		Produces:      []string{"integrate"},
	}
	souls := fullSouls(t)
	resolver := soul(t, "merge-resolver", "merge-resolver")
	resolver.Model = "gpt-4o" // a different family than the producer
	souls = append(souls, resolver)
	c.Souls = souls
	if err := c.Validate(); err != nil {
		t.Fatalf("config should be valid: %v", err)
	}
	ws := c.Warnings()
	mustWarn(t, ws, `verifier role "security"`)
	for _, w := range ws {
		if strings.Contains(w, "merge-resolver") {
			t.Fatalf("resolve must not be treated as implement's verifier; got %q", w)
		}
	}
}
