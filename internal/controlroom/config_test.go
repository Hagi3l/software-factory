package controlroom

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
)

// configTestCfg is a small validated-shape config for the Config view server test.
func configTestCfg() *config.Config {
	return &config.Config{
		Root: "/srv/harness/config",
		Harness: &config.Harness{
			DAG: map[string]config.Stage{
				"plan":      {Role: "planner", Kind: config.StageKindPlan, OnFailure: "plan", Produces: []string{"implement"}},
				"implement": {Role: "implementor", Postcondition: []string{"tests-red-then-green"}, Produces: []string{"integrate"}},
				"integrate": {Kind: config.StageKindTrustedMerge, Postcondition: []string{"human-approved"}},
			},
			Checks: map[string]string{"tests-pass": "make test-unit"},
			Policy: config.Policy{MaxRetries: 3, Profile: config.ProfileTrustedDev, TCBPaths: []string{"config/**"}},
		},
		Souls: []core.Soul{
			{Name: "planner-go", Role: "planner", Model: "claude-opus-4-8", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/planner.md", Selector: map[string]string{"lang": "go"}},
		},
		Infra: &config.Infra{
			Sandbox:   config.SandboxConfig{Backend: config.BackendDocker, Egress: "broker-only", Profiles: map[string]config.SandboxProfile{"go-toolchain": {Image: "harness/go-toolchain@sha256:abc"}}},
			NATS:      config.NATSConfig{URL: "nats://secret-host:4222"},
			Broker:    config.BrokerConfig{Allowlist: []string{"llm-api", "git"}},
			Artifacts: config.ArtifactsConfig{Backend: "files", Path: "/var/secret/artifacts"},
			Models:    map[string]config.ModelProvider{"claude-opus-4-8": {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 15, OutputPerMTok: 75}}},
		},
	}
}

// warningConfigCfg is a validated config that trips the producer/verifier model-diversity advisory
// (T2.13): the implementor and its downstream security gate both resolve to the anthropic family,
// so config.Warnings() is non-empty and the Config view must surface an advisories section.
func warningConfigCfg() *config.Config {
	return &config.Config{
		Root: "/srv/harness/config",
		Harness: &config.Harness{
			DAG: map[string]config.Stage{
				"implement": {Role: "implementor", Postcondition: []string{"tests-red-then-green"}, Produces: []string{"qa"}},
				"qa":        {Role: "security", Postcondition: []string{"tests-pass"}, Produces: []string{"integrate"}},
				"integrate": {Kind: config.StageKindTrustedMerge, Postcondition: []string{"human-approved"}},
			},
			Checks: map[string]string{"tests-pass": "make test-unit"},
			Policy: config.Policy{MaxRetries: 3, Profile: config.ProfileTrustedDev, TCBPaths: []string{"config/**"}},
		},
		Souls: []core.Soul{
			{Name: "implementor-go", Role: "implementor", Model: "claude-opus-4-8", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/impl.md"},
			{Name: "security-go", Role: "security", Model: "claude-sonnet-4-6", Sandbox: "go-toolchain", Persona: "/srv/harness/config/souls/prompts/sec.md"},
		},
		Infra: &config.Infra{
			Sandbox:   config.SandboxConfig{Backend: config.BackendDocker, Egress: "broker-only", Profiles: map[string]config.SandboxProfile{"go-toolchain": {Image: "harness/go-toolchain@sha256:abc"}}},
			Broker:    config.BrokerConfig{Allowlist: []string{"llm-api"}},
			Artifacts: config.ArtifactsConfig{Backend: "files", Path: "/var/artifacts"},
			Models: map[string]config.ModelProvider{
				"claude-opus-4-8":   {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 15, OutputPerMTok: 75}},
				"claude-sonnet-4-6": {Provider: "anthropic", Cost: config.ModelCost{InputPerMTok: 3, OutputPerMTok: 15}},
			},
		},
	}
}

// TestConfigSurfacesAdvisories: a config that trips a non-fatal advisory (here producer/verifier
// model-family overlap, T2.13) renders an advisories section with the warning text — the same
// signal `harness validate` prints at startup, now visible in the control room where the operator
// inspects the running factory. A clean config (configTestCfg) renders no such section.
func TestConfigSurfacesAdvisories(t *testing.T) {
	s := New(Options{Config: warningConfigCfg(), Env: "dev"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config")
	if r.status != http.StatusOK {
		t.Fatalf("/config status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"advisories",                          // the section heading
		"producer role",                       // the diversity advisory
		"verifier role",                       // names the verifier role
		"implementor",                         // the producer role
		"security",                            // the verifier role
		"weakening the N-version independent",  // the rationale
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/config missing advisory content %q", want)
		}
	}

	// A clean config renders no advisories section at all.
	clean := New(Options{Config: configTestCfg(), Env: "dev"})
	cts := httptest.NewServer(clean.Handler())
	t.Cleanup(cts.Close)
	cr := get(t, cts, "/config")
	if strings.Contains(cr.body, "advisories") {
		t.Errorf("clean config should render no advisories section, got: %s", cr.body)
	}
}

// TestConfigNotAttached: with no in-process config (a standalone `harness serve`) the page is a
// notice inside the chrome (200), mirroring the Reader-backed views' graceful degradation.
func TestConfigNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config")
	if r.status != http.StatusOK {
		t.Fatalf("/config status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("/config missing not-attached notice, got: %s", r.body)
	}
	if !strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("/config not wrapped in the base layout")
	}
}

// TestConfigRenders: the wired view renders the identity strip, the role-flow SVG, the stages /
// checks / souls / policy sections, and redacts topology while keeping the allowlist + digests.
func TestConfigRenders(t *testing.T) {
	s := New(Options{Config: configTestCfg(), Env: "dev"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config")
	if r.status != http.StatusOK {
		t.Fatalf("/config status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"infra.dev.yaml",        // identity strip — active overlay
		"trusted-dev",           // autonomy profile
		"<svg",                  // role-flow graph
		`data-node="implement"`, // a stage node
		"make test-unit",        // check command
		"planner-go",            // resolved soul
		"sha256:abc",            // kept image digest
		"llm-api",               // kept egress allowlist
		"«redacted»",            // masked topology
		"config/**",             // TCB path
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/config missing %q", want)
		}
	}
	// The masked secrets must never reach the wire (rendered or raw fold).
	for _, secret := range []string{"secret-host", "/var/secret/artifacts"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("/config leaked %q", secret)
		}
	}
	// Config is a static snapshot, not a feed: nothing on the page auto-refetches. The lazy
	// persona folds DO hx-get, but only on user expand (click once) — never on a timer or SSE.
	if strings.Contains(r.body, `hx-trigger="every`) {
		t.Errorf("/config should not poll — config is restart-static")
	}
	if strings.Contains(r.body, `hx-get="/config/souls/`) && !strings.Contains(r.body, `hx-trigger="click once"`) {
		t.Errorf("/config persona fold must be lazy (click once), not auto-loaded")
	}
	// The persona body is fetched lazily, so the prompt text must NOT be inlined in the page.
	if strings.Contains(r.body, "loading…") == false {
		t.Errorf("/config persona fold placeholder missing — body should be lazily loaded")
	}
	// The nav highlights the active view.
	if !strings.Contains(r.body, `href="/config"`) {
		t.Errorf("/config nav entry missing")
	}
}

// personaTestCfg writes real persona files under a temp config root and returns a config whose
// soul + requirements planner point at them, so the persona route can read actual bytes.
func personaTestCfg(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "souls", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) string {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	soulPersona := write("souls/prompts/planner.md", "# Planner soul\nYou decompose work into a plan.\n")
	rpPersona := write("souls/prompts/requirements.md", "# Requirements planner\nYou elicit testable intent.\n")
	return &config.Config{
		Root: root,
		Harness: &config.Harness{
			RequirementsPlanner: &config.RequirementsPlanner{Model: "claude-opus-4-8", Persona: rpPersona},
		},
		Souls: []core.Soul{
			{Name: "planner-go", Role: "planner", Model: "claude-opus-4-8", Persona: soulPersona},
		},
	}
}

// TestPersonaRouteServesSoul: the lazy persona fragment returns the soul's persona file verbatim
// as inert escaped text — the literal system prompt the agent boots from.
func TestPersonaRouteServesSoul(t *testing.T) {
	s := New(Options{Config: personaTestCfg(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config/souls/planner-go/persona")
	if r.status != http.StatusOK {
		t.Fatalf("persona status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "You decompose work into a plan.") {
		t.Errorf("persona body missing soul prompt, got: %s", r.body)
	}
	// It is a bare fragment (a <pre>), not the whole page chrome.
	if strings.Contains(r.body, "<nav") || strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("persona route should return a fragment, not the full page")
	}
}

// TestPersonaRouteServesRequirementsPlanner: the reserved planner key resolves to the
// requirements planner persona, even though it is not a soul.
func TestPersonaRouteServesRequirementsPlanner(t *testing.T) {
	s := New(Options{Config: personaTestCfg(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config/souls/requirements-planner/persona")
	if r.status != http.StatusOK {
		t.Fatalf("persona status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "You elicit testable intent.") {
		t.Errorf("requirements planner persona missing, got: %s", r.body)
	}
}

// TestPersonaRouteUnknownName: a name not in the declared roster 404s — the route reads only
// files the validated config names, never a path built from the URL (no traversal).
func TestPersonaRouteUnknownName(t *testing.T) {
	s := New(Options{Config: personaTestCfg(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	for _, name := range []string{"nope", "..%2f..%2fetc%2fpasswd"} {
		r := get(t, ts, "/config/souls/"+name+"/persona")
		if r.status != http.StatusNotFound {
			t.Errorf("persona for %q status = %d, want 404", name, r.status)
		}
	}
}

// TestPersonaRouteNotAttached: standalone `harness serve` (no in-process config) 404s rather
// than reading from a nil config.
func TestPersonaRouteNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/config/souls/planner-go/persona")
	if r.status != http.StatusNotFound {
		t.Errorf("persona not-attached status = %d, want 404", r.status)
	}
}
