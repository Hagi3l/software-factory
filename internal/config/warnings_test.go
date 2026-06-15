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

// The canonical config runs both the implementor (producer) and the security reviewer
// (qa verifier) on the anthropic provider, so validate must advise the shared family —
// this is the N-version diversity warning T2.13 adds (specs/verification.md).
func TestWarnSameProviderProducerVerifier(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t) // every soul defaults to claude-opus-4-7 (anthropic)
	ws := c.Warnings()
	mustWarn(t, ws, `producer role "implementor"`)
	mustWarn(t, ws, `verifier role "security"`)
	mustWarn(t, ws, `provider "anthropic"`)
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

// Family is keyed on the provider tag, so two distinct openai-compat endpoints read as the
// same family. The advisory says so when the shared provider is openai-compat, naming the
// known imperfection rather than over-engineering a per-endpoint identity.
func TestWarnOpenAICompatNotesEndpointBlindSpot(t *testing.T) {
	c := validConfig()
	c.Infra.Models["local-a"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: "http://a:11434/v1"}
	c.Infra.Models["local-b"] = ModelProvider{Provider: ProviderOpenAICompat, Endpoint: "http://b:11434/v1"}
	souls := fullSouls(t)
	setRoleModel(souls, "implementor", "local-a")
	setRoleModel(souls, "security", "local-b")
	c.Souls = souls
	ws := c.Warnings()
	mustWarn(t, ws, `provider "openai-compat"`)
	mustWarn(t, ws, "openai-compat endpoints read as the same family")
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
	mustWarn(t, c.Warnings(), `provider "anthropic"`)
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
