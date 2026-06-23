package configview

import (
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
)

// sampleConfig is a representative validated config: the shipped-style role-flow DAG, a check
// registry, a multi-soul role (to exercise specificity ordering), and an infra overlay with
// the sensitive topology fields populated (to exercise redaction).
func sampleConfig() *config.Config {
	return &config.Config{
		Root: "/srv/harness/config",
		Harness: &config.Harness{
			DAG: map[string]config.Stage{
				"requirements": {Kind: config.StageKindHuman},
				"plan":         {Role: "planner", Kind: config.StageKindPlan, Precondition: "blockers-closed", OnFailure: "plan", Produces: []string{"implement"}},
				"implement":    {Role: "implementor", Precondition: "blockers-closed", Postcondition: []string{"tests-red-then-green"}, OnFailure: "implement", Produces: []string{"qa"}},
				"qa":           {Role: "security", Postcondition: []string{"tests-pass", "mutation>=0.8"}, OnFailure: "implement", Produces: []string{"integrate"}},
				"integrate":    {Kind: config.StageKindTrustedMerge, Postcondition: []string{"human-approved"}},
			},
			Checks: map[string]string{
				"tests-pass": "make test-unit",
				"gosec":      "make gosec",
			},
			Policy: config.Policy{
				MaxRetries: 3,
				Budget:     config.Budget{Tokens: 2_000_000, USD: 20},
				EpicBudget: config.Budget{USD: 200},
				DeadLetter: "harness.dlq",
				Profile:    config.ProfileTrustedDev,
				TCBPaths:   []string{"internal/orchestrator/**", "config/**"},
			},
			SpecDepth: 1,
		},
		Souls: []core.Soul{
			// Two souls for "implementor": a catch-all (empty selector) and a specialized one.
			// cfg.Souls is name-sorted; "implementor-default" sorts before "implementor-go".
			{Name: "implementor-default", Role: "implementor", Model: "claude-opus-4-8", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/impl.md"},
			{Name: "implementor-go", Role: "implementor", Model: "claude-opus-4-8", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/impl-go.md", Selector: map[string]string{"lang": "go"}},
			{Name: "planner-go", Role: "planner", Model: "claude-opus-4-8", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/planner.md", Selector: map[string]string{"lang": "go"}},
			{Name: "security-go", Role: "security", Model: "claude-sonnet-4-6", Sandbox: "go-toolchain", Persona: "/abs/out-of-tree/security.md", Selector: map[string]string{"lang": "go"}},
		},
		Infra: &config.Infra{
			Sandbox: config.SandboxConfig{
				Backend: config.BackendDocker,
				Egress:  "broker-only",
				Profiles: map[string]config.SandboxProfile{
					"go-toolchain": {Image: "harness/go-toolchain@sha256:abc123"},
				},
			},
			NATS:      config.NATSConfig{URL: "nats://secret-host:4222"},
			Broker:    config.BrokerConfig{Allowlist: []string{"llm-api", "nats", "git"}},
			Artifacts: config.ArtifactsConfig{Backend: "files", Path: "/var/secret/artifacts"},
			OTel:      config.OTelConfig{Endpoint: "collector.internal:4317"},
			Models: map[string]config.ModelProvider{
				"claude-opus-4-8":   {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 15, OutputPerMTok: 75}},
				"local-llama":       {Provider: "openai-compat", Endpoint: "http://gpu-box:11434/v1"},
				"claude-sonnet-4-6": {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 3, OutputPerMTok: 15}},
			},
		},
	}
}

// TestBuildIdentityAndStages: the identity strip carries root/overlay/profile, and the stages
// table is name-sorted with kinds labeled (an agent stage shows "agent", not blank).
func TestBuildIdentityAndStages(t *testing.T) {
	v := Build(sampleConfig(), "dev")

	if v.Identity.Root != "/srv/harness/config" || v.Identity.Env != "dev" {
		t.Fatalf("identity = %+v", v.Identity)
	}
	if v.Identity.Profile != config.ProfileTrustedDev {
		t.Errorf("profile = %q, want %q", v.Identity.Profile, config.ProfileTrustedDev)
	}
	if !v.Identity.Validated {
		t.Errorf("Validated should be true")
	}

	if len(v.Stages) != 5 {
		t.Fatalf("stages = %d, want 5", len(v.Stages))
	}
	if v.Stages[0].Name != "implement" { // name-sorted: implement, integrate, plan, qa, requirements
		t.Errorf("stages not name-sorted, first = %q", v.Stages[0].Name)
	}
	// implement is an agent stage (no Kind, a Role) → labeled "agent".
	for _, st := range v.Stages {
		if st.Name == "implement" && st.Kind != "agent" {
			t.Errorf("implement kind = %q, want agent", st.Kind)
		}
		if st.Name == "plan" && st.Kind != config.StageKindPlan {
			t.Errorf("plan kind = %q, want plan", st.Kind)
		}
	}
}

// TestBuildSurfacesWarnings: Build projects config.Warnings() onto the view so the control room
// can render the same advisories `harness validate` prints. The sample config's implementor
// (anthropic) and its downstream security gate (anthropic) share a model family, so the
// producer/verifier diversity advisory (T2.13) must appear — the safety signal reaching the UI.
func TestBuildSurfacesWarnings(t *testing.T) {
	v := Build(sampleConfig(), "dev")
	if len(v.Warnings) == 0 {
		t.Fatal("Warnings empty — same-family producer/verifier should advise (T2.13)")
	}
	var found bool
	for _, w := range v.Warnings {
		if strings.Contains(w, `producer role "implementor"`) && strings.Contains(w, `model family "anthropic"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no model-diversity advisory in %v", v.Warnings)
	}
}

// TestBuildNoWarningsWhenClean: a config with no advisory condition projects no Warnings, so the
// view renders no advisories section — a quiet page means a quiet config.
func TestBuildNoWarningsWhenClean(t *testing.T) {
	cfg := sampleConfig()
	// Point the security verifier at a different model family than the implementor, removing the
	// only overlap; with no proxy/remote misconfig either, Warnings is then empty.
	cfg.Infra.Models["local-llama"] = config.ModelProvider{Provider: "openai-compat", Endpoint: "http://gpu-box:11434/v1"}
	for i := range cfg.Souls {
		if cfg.Souls[i].Role == "security" {
			cfg.Souls[i].Model = "local-llama"
		}
	}
	if v := Build(cfg, "dev"); len(v.Warnings) != 0 {
		t.Errorf("clean config should have no warnings, got %v", v.Warnings)
	}
}

// TestSoulSpecificityOrdering: a role with several souls orders most-specific-first, with the
// empty-selector soul flagged catch-all; each soul is resolved against the infra registry.
func TestSoulSpecificityOrdering(t *testing.T) {
	v := Build(sampleConfig(), "dev")

	var impl *RoleRow
	for i := range v.Roles {
		if v.Roles[i].Role == "implementor" {
			impl = &v.Roles[i]
		}
	}
	if impl == nil {
		t.Fatal("no implementor role row")
	}
	if len(impl.Souls) != 2 {
		t.Fatalf("implementor souls = %d, want 2", len(impl.Souls))
	}
	// Most specific (selector lang=go) first; the catch-all (empty selector) last.
	if impl.Souls[0].Name != "implementor-go" || impl.Souls[0].CatchAll {
		t.Errorf("first soul = %+v, want implementor-go non-catch-all", impl.Souls[0])
	}
	if impl.Souls[1].Name != "implementor-default" || !impl.Souls[1].CatchAll {
		t.Errorf("second soul = %+v, want implementor-default catch-all", impl.Souls[1])
	}
	// Resolution: model → provider+cost, sandbox → concrete digest, persona → relative to root.
	got := impl.Souls[0]
	if got.Provider != "anthropic" || got.Cost == "" {
		t.Errorf("model not resolved: %+v", got)
	}
	if got.Image != "harness/go-toolchain@sha256:abc123" {
		t.Errorf("sandbox image not resolved: %q", got.Image)
	}
	if got.PersonaPath != "souls/prompts/impl-go.md" {
		t.Errorf("persona not relativized: %q", got.PersonaPath)
	}
	// An out-of-tree persona is shown verbatim, not mangled.
	for _, r := range v.Roles {
		if r.Role == "security" && r.Souls[0].PersonaPath != "/abs/out-of-tree/security.md" {
			t.Errorf("out-of-tree persona = %q", r.Souls[0].PersonaPath)
		}
	}
}

// TestRedaction: the allowlisted topology fields are masked in both the structured infra view
// and the raw fold; the kept fields (egress allowlist, image digest, provider/cost) stay
// visible; a non-allowlisted field (sandbox backend) is shown — proving redaction is by
// allowlist, not by omission.
func TestRedaction(t *testing.T) {
	v := Build(sampleConfig(), "dev")

	// Structured view: topology masked.
	if v.Infra.NATSURL != redactedMark {
		t.Errorf("nats url not masked: %q", v.Infra.NATSURL)
	}
	if v.Infra.ArtifactsPath != redactedMark {
		t.Errorf("artifacts path not masked: %q", v.Infra.ArtifactsPath)
	}
	if v.Infra.OTelEndpoint != redactedMark {
		t.Errorf("otel endpoint not masked: %q", v.Infra.OTelEndpoint)
	}
	// Kept: egress allowlist + the image digest + backend.
	if strings.Join(v.Infra.Allowlist, ",") != "llm-api,nats,git" {
		t.Errorf("allowlist altered: %v", v.Infra.Allowlist)
	}
	if v.Infra.SandboxBackend != "docker" {
		t.Errorf("backend should be shown: %q", v.Infra.SandboxBackend)
	}
	var imgKept bool
	for _, p := range v.Infra.Profiles {
		if p.Image == "harness/go-toolchain@sha256:abc123" {
			imgKept = true
		}
	}
	if !imgKept {
		t.Errorf("image digest should be kept visible: %+v", v.Infra.Profiles)
	}
	// A model endpoint is topology → masked; provider/cost kept.
	var llama *ModelRow
	for i := range v.Infra.Models {
		if v.Infra.Models[i].Name == "local-llama" {
			llama = &v.Infra.Models[i]
		}
	}
	if llama == nil || llama.Endpoint != redactedMark {
		t.Errorf("model endpoint not masked: %+v", llama)
	}
	if llama.Provider != "openai-compat" {
		t.Errorf("model provider should be kept: %q", llama.Provider)
	}

	// Raw fold: the secret values never appear; the masked marker does; kept values do.
	raw := v.InfraYAML
	for _, secret := range []string{"secret-host", "/var/secret/artifacts", "collector.internal", "gpu-box"} {
		if strings.Contains(raw, secret) {
			t.Errorf("raw infra fold leaked %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(raw, redactedMark) {
		t.Errorf("raw infra fold has no redaction marker:\n%s", raw)
	}
	for _, kept := range []string{"llm-api", "sha256:abc123", "anthropic"} {
		if !strings.Contains(raw, kept) {
			t.Errorf("raw infra fold dropped kept value %q:\n%s", kept, raw)
		}
	}

	// The original config is not mutated by redaction (the run uses the real endpoints).
	cfg := sampleConfig()
	_ = Build(cfg, "dev")
	if cfg.Infra.NATS.URL != "nats://secret-host:4222" {
		t.Errorf("Build mutated the source config's nats url: %q", cfg.Infra.NATS.URL)
	}
	if cfg.Infra.Models["local-llama"].Endpoint != "http://gpu-box:11434/v1" {
		t.Errorf("Build mutated the source config's model endpoint")
	}
}

// TestRoleFlowSVG: the role-flow graph renders to SVG with stage nodes, distinct produces vs
// on_failure edge styling, the stage-kind palette, and XML-escaped dynamic text; self-loops
// (plan→plan) are dropped while cross-stage back-edges (qa→implement) are kept.
func TestRoleFlowSVG(t *testing.T) {
	v := Build(sampleConfig(), "dev")
	svg := v.PipelineSVG
	if svg == "" {
		t.Fatal("empty pipeline svg")
	}
	// Stage nodes are present and NOT click-through (no /issue/ anchor for a stage).
	if !strings.Contains(svg, `data-node="implement"`) {
		t.Errorf("missing implement stage node:\n%s", svg)
	}
	if strings.Contains(svg, "/issue/") {
		t.Errorf("stage nodes should not link to /issue/:\n%s", svg)
	}
	// A produces edge (solid) and an on_failure edge (dashed, with its own marker) both render.
	if !strings.Contains(svg, `data-kind="on_failure"`) || !strings.Contains(svg, "stroke-dasharray") {
		t.Errorf("on_failure edge not styled distinctly:\n%s", svg)
	}
	if !strings.Contains(svg, "dag-arrow-fail") {
		t.Errorf("failure marker not emitted:\n%s", svg)
	}
	// qa→implement is a real cross-stage on_failure back-edge — kept.
	if !strings.Contains(svg, `data-kind="on_failure" data-from="qa" data-to="implement"`) {
		t.Errorf("qa→implement back-edge missing:\n%s", svg)
	}
	// plan→plan is a self-loop — dropped (no edge from plan to plan).
	if strings.Contains(svg, `data-from="plan" data-to="plan"`) {
		t.Errorf("plan self-loop should be dropped:\n%s", svg)
	}
}

// TestPolicyFormatting: uncapped dimensions render as ∞, capped ones with thousands grouping
// and trimmed dollars.
func TestPolicyFormatting(t *testing.T) {
	v := Build(sampleConfig(), "dev")
	if v.Policy.Budget.Tokens != "2,000,000" {
		t.Errorf("tokens = %q, want 2,000,000", v.Policy.Budget.Tokens)
	}
	if v.Policy.Budget.USD != "$20" {
		t.Errorf("usd = %q, want $20", v.Policy.Budget.USD)
	}
	if v.Policy.Budget.Wall != "∞" { // wall unset → uncapped
		t.Errorf("wall = %q, want ∞", v.Policy.Budget.Wall)
	}
	if v.Policy.EpicBudget.Tokens != "∞" {
		t.Errorf("epic tokens = %q, want ∞", v.Policy.EpicBudget.Tokens)
	}
}

// TestBuildNil: a nil config yields a zero view rather than panicking (defensive — the handler
// renders the not-attached notice instead, but Build stays total).
func TestBuildNil(t *testing.T) {
	v := Build(nil, "dev")
	if v.PipelineSVG != "" || len(v.Stages) != 0 || len(v.Roles) != 0 {
		t.Errorf("nil config should yield a zero view, got %+v", v)
	}
}

// TestEmptyProfileDefaults: an unset profile reads as the autonomous default, not blank.
func TestEmptyProfileDefaults(t *testing.T) {
	cfg := sampleConfig()
	cfg.Harness.Policy.Profile = ""
	v := Build(cfg, "dev")
	if !strings.Contains(v.Identity.Profile, config.ProfileAutonomous) {
		t.Errorf("empty profile = %q, want autonomous default", v.Identity.Profile)
	}
}

// TestReqPlannerRowConfigured: a declared requirements planner projects resolved (model joined
// to provider+cost from the registry, persona path shown relative to root) and flagged
// Configured, so the view renders its card beside the soul roster.
func TestReqPlannerRowConfigured(t *testing.T) {
	cfg := sampleConfig()
	cfg.Harness.RequirementsPlanner = &config.RequirementsPlanner{
		Model:   "claude-opus-4-8",
		Persona: "souls/prompts/requirements.md",
	}

	rp := Build(cfg, "dev").ReqPlanner
	if !rp.Configured {
		t.Fatal("ReqPlanner.Configured = false, want true")
	}
	if rp.Key != ReqPlannerKey {
		t.Errorf("Key = %q, want %q", rp.Key, ReqPlannerKey)
	}
	if rp.Provider != "anthropic" || rp.Cost == "" {
		t.Errorf("planner model not resolved: provider=%q cost=%q", rp.Provider, rp.Cost)
	}
	// Persona is shown relative to the config root (resolvePersonas-style absolute under root).
	if rp.PersonaPath != "souls/prompts/requirements.md" {
		t.Errorf("PersonaPath = %q, want relative path", rp.PersonaPath)
	}
}

// TestReqPlannerRowUnconfigured: with no requirements_planner declared (the wizard is disabled),
// the row is a zero/unconfigured value, so the view omits the card.
func TestReqPlannerRowUnconfigured(t *testing.T) {
	if rp := Build(sampleConfig(), "dev").ReqPlanner; rp.Configured {
		t.Errorf("ReqPlanner.Configured = true with no planner declared, want false")
	}
}
