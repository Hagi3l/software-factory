package controlroom

import (
	"net/http"
	"net/http/httptest"
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
	// Config is a static snapshot, not a feed: it has no self-refetch fragment (the only
	// sse-connect on the page is the layout's status bar, shared by every view).
	if strings.Contains(r.body, `hx-get="/config`) {
		t.Errorf("/config should not refetch itself — config is restart-static")
	}
	// The nav highlights the active view.
	if !strings.Contains(r.body, `href="/config"`) {
		t.Errorf("/config nav entry missing")
	}
}
