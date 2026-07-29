package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
)

// These fixtures mirror the example configs in specs/configuration.md verbatim
// (including the underscore-separated token budget and the "2h"/"30m" durations),
// so the tests double as a contract check that the documented YAML actually loads.
const harnessYAML = `
dag:
  requirements: { kind: human }
  plan:         { role: planner, kind: plan, on_failure: plan, produces: [author-tests] }
  author-tests: { role: test-author,  produces: [implement] }
  implement:    { role: implementor,
                  precondition:  blockers-closed,
                  postcondition: [tests-red-then-green],
                  on_failure:    implement,
                  produces:      [qa] }
  qa:           { role: security,
                  postcondition: [tests-pass, "mutation>=0.8", gosec, govulncheck, license-scan],
                  on_failure:    implement,
                  produces:      [integrate] }
  integrate:    { kind: trusted-merge }

checks:
  tests-pass:   go test ./...
  gosec:        gosec ./...
  govulncheck:  govulncheck ./...
  license-scan: go-licenses check ./...
  mutation:     gremlins unleash --output /tmp/m.json && jq -r .efficacy /tmp/m.json

policy:
  max_retries: 3
  budget:      { tokens: 2_000_000, usd: 20, wall: 2h }
  epic_budget: { usd: 200 }
  dead_letter: factory.dlq
`

const soulYAML = `
name:    implementor-go
role:    implementor
model:   claude-opus-4-7
persona: souls/prompts/implementor-go.md
tools:   [fs, shell, git]
sandbox: go-toolchain
selector: { lang: go }
`

const infraYAML = `
sandbox:
  backend: docker
  egress:  broker-only
  limits:  { cpu: 2, mem: 2Gi, wall: 30m }
  profiles:
    go-toolchain:
      image: factory/go-toolchain:dev
nats:
  url: nats://localhost:4222
  jetstream: { replicas: 1, max_age: 168h }
broker:
  allowlist: [llm-api, nats, package-mirror, git]
artifacts:
  backend: files
  path: ./.software-factory/artifacts
otel:
  endpoint: localhost:4317
models:
  claude-opus-4-7: { provider: anthropic }
  gpt-4o:          { provider: openai }
  llama-3.3-70b:   { provider: openai-compat, endpoint: http://ollama:11434/v1 }
`

func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "souls", "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir souls: %v", err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("factory.yaml", harnessYAML)
	write(filepath.Join("souls", "implementor-go.yaml"), soulYAML)
	write(filepath.Join("souls", "prompts", "implementor-go.md"), "# persona\n")
	write("infra.dev.yaml", infraYAML)
	return dir
}

func TestLoadHarness(t *testing.T) {
	dir := writeTree(t)
	h, err := LoadHarness(filepath.Join(dir, "factory.yaml"))
	if err != nil {
		t.Fatalf("LoadHarness: %v", err)
	}

	// Non-agent stages carry Kind, not Role.
	if got := h.DAG["requirements"]; got.Kind != "human" || got.Role != "" {
		t.Errorf("requirements = %+v, want kind=human role empty", got)
	}
	if got := h.DAG["integrate"]; got.Kind != "trusted-merge" {
		t.Errorf("integrate kind = %q, want trusted-merge", got.Kind)
	}

	// The plan stage is the hybrid: an agent stage (it names a role) that is ungated
	// (kind=plan, no postcondition), producing author-tests issues from the planner's
	// proposals.
	if got := h.DAG["plan"]; got.Kind != "plan" || got.Role != "planner" || len(got.Postcondition) != 0 ||
		!reflect.DeepEqual(got.Produces, []string{"author-tests"}) {
		t.Errorf("plan = %+v, want kind=plan role=planner no-postcondition produces=[author-tests]", got)
	}

	// Agent stage with the full guard set: precondition, postcondition list,
	// on_failure route, and produces depth.
	impl := h.DAG["implement"]
	if impl.Role != "implementor" {
		t.Errorf("implement role = %q, want implementor", impl.Role)
	}
	if impl.Precondition != "blockers-closed" {
		t.Errorf("implement precondition = %q, want blockers-closed", impl.Precondition)
	}
	if !reflect.DeepEqual(impl.Postcondition, []string{"tests-red-then-green"}) {
		t.Errorf("implement postcondition = %v", impl.Postcondition)
	}
	if impl.OnFailure != "implement" {
		t.Errorf("implement on_failure = %q, want implement", impl.OnFailure)
	}
	if !reflect.DeepEqual(impl.Produces, []string{"qa"}) {
		t.Errorf("implement produces = %v, want [qa]", impl.Produces)
	}
	if want := []string{"tests-pass", "mutation>=0.8", "gosec", "govulncheck", "license-scan"}; !reflect.DeepEqual(h.DAG["qa"].Postcondition, want) {
		t.Errorf("qa postcondition = %v, want %v", h.DAG["qa"].Postcondition, want)
	}

	// The check registry maps each command-check postcondition to its shell command,
	// the bridge the gate resolves against (see specs/configuration.md). The qa stage's
	// three independent scanners (SAST / vulnerability / dependency-license) are plain
	// command checks alongside tests-pass and the mutation measurement command.
	wantChecks := map[string]string{
		"tests-pass":   "go test ./...",
		"gosec":        "gosec ./...",
		"govulncheck":  "govulncheck ./...",
		"license-scan": "go-licenses check ./...",
		"mutation":     "gremlins unleash --output /tmp/m.json && jq -r .efficacy /tmp/m.json",
	}
	if !reflect.DeepEqual(h.Checks, wantChecks) {
		t.Errorf("checks = %v, want %v", h.Checks, wantChecks)
	}

	// Policy: the underscore-separated token literal and the "2h" duration are the
	// two parse hazards the spec's syntax relies on.
	p := h.Policy
	if p.MaxRetries != 3 {
		t.Errorf("max_retries = %d, want 3", p.MaxRetries)
	}
	if p.Budget.Tokens != 2_000_000 {
		t.Errorf("budget tokens = %d, want 2000000 (underscore literal)", p.Budget.Tokens)
	}
	if p.Budget.USD != 20 {
		t.Errorf("budget usd = %v, want 20", p.Budget.USD)
	}
	if p.Budget.Wall.Duration() != 2*time.Hour {
		t.Errorf("budget wall = %v, want 2h", p.Budget.Wall.Duration())
	}
	if p.EpicBudget.USD != 200 {
		t.Errorf("epic_budget usd = %v, want 200", p.EpicBudget.USD)
	}
	if p.DeadLetter != "factory.dlq" {
		t.Errorf("dead_letter = %q, want factory.dlq", p.DeadLetter)
	}
}

func TestLoadSouls(t *testing.T) {
	dir := writeTree(t)
	souls, err := LoadSouls(filepath.Join(dir, "souls"))
	if err != nil {
		t.Fatalf("LoadSouls: %v", err)
	}
	if len(souls) != 1 {
		t.Fatalf("got %d souls, want 1", len(souls))
	}
	s := souls[0]
	if s.Name != "implementor-go" || s.Role != "implementor" || s.Model != "claude-opus-4-7" {
		t.Errorf("soul identity = %+v", s)
	}
	if !reflect.DeepEqual(s.Tools, []string{"fs", "shell", "git"}) {
		t.Errorf("soul tools = %v", s.Tools)
	}
	if !reflect.DeepEqual(s.Selector, map[string]string{"lang": "go"}) {
		t.Errorf("soul selector = %v, want {lang: go}", s.Selector)
	}
}

// LoadSouls must sort by name so the set is deterministic regardless of the
// filesystem's directory ordering.
func TestLoadSoulsSortedByName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		body := "name: " + name + "\nrole: implementor\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	souls, err := LoadSouls(dir)
	if err != nil {
		t.Fatalf("LoadSouls: %v", err)
	}
	got := []string{souls[0].Name, souls[1].Name, souls[2].Name}
	if !reflect.DeepEqual(got, []string{"alpha", "mu", "zeta"}) {
		t.Errorf("soul order = %v, want sorted", got)
	}
}

// A missing souls directory is not an error — it yields no souls and lets validate
// report the absence precisely.
func TestLoadSoulsMissingDir(t *testing.T) {
	souls, err := LoadSouls(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadSouls missing dir: %v", err)
	}
	if len(souls) != 0 {
		t.Errorf("got %d souls, want 0", len(souls))
	}
}

func TestLoadInfra(t *testing.T) {
	dir := writeTree(t)
	in, err := LoadInfra(filepath.Join(dir, "infra.dev.yaml"))
	if err != nil {
		t.Fatalf("LoadInfra: %v", err)
	}
	if in.Sandbox.Backend != "docker" || in.Sandbox.Egress != "broker-only" {
		t.Errorf("sandbox = %+v", in.Sandbox)
	}
	if in.Sandbox.Limits.CPU != 2 || in.Sandbox.Limits.Mem != "2Gi" {
		t.Errorf("sandbox limits = %+v", in.Sandbox.Limits)
	}
	if in.Sandbox.Limits.Wall.Duration() != 30*time.Minute {
		t.Errorf("sandbox wall = %v, want 30m", in.Sandbox.Limits.Wall.Duration())
	}
	if in.NATS.URL != "nats://localhost:4222" {
		t.Errorf("nats url = %q", in.NATS.URL)
	}
	if in.NATS.JetStream.Replicas != 1 || in.NATS.JetStream.MaxAge.Duration() != 168*time.Hour {
		t.Errorf("jetstream = %+v", in.NATS.JetStream)
	}
	if want := []string{"llm-api", "nats", "package-mirror", "git"}; !reflect.DeepEqual(in.Broker.Allowlist, want) {
		t.Errorf("broker allowlist = %v, want %v", in.Broker.Allowlist, want)
	}
	if in.Artifacts.Backend != "files" || in.Artifacts.Path != "./.software-factory/artifacts" {
		t.Errorf("artifacts = %+v", in.Artifacts)
	}
	if in.OTel.Endpoint != "localhost:4317" {
		t.Errorf("otel endpoint = %q", in.OTel.Endpoint)
	}
	// The model registry maps soul.Model names to provider adapters; openai-compat
	// additionally carries an endpoint.
	if in.Models["claude-opus-4-7"].Provider != "anthropic" {
		t.Errorf("claude provider = %q", in.Models["claude-opus-4-7"].Provider)
	}
	llama := in.Models["llama-3.3-70b"]
	if llama.Provider != "openai-compat" || llama.Endpoint != "http://ollama:11434/v1" {
		t.Errorf("llama provider = %+v", llama)
	}
}

func TestLoad(t *testing.T) {
	dir := writeTree(t)
	cfg, err := Load(dir, "dev")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Harness == nil || cfg.Infra == nil || len(cfg.Souls) != 1 {
		t.Fatalf("Load incomplete: factory=%v infra=%v souls=%d", cfg.Harness, cfg.Infra, len(cfg.Souls))
	}
	if cfg.Root != dir {
		t.Errorf("Root = %q, want %q", cfg.Root, dir)
	}
	// A relative persona path resolves against the config root.
	want := filepath.Join(dir, "souls", "prompts", "implementor-go.md")
	if got := cfg.PersonaPath(cfg.Souls[0]); got != want {
		t.Errorf("PersonaPath = %q, want %q", got, want)
	}
	if _, err := os.Stat(cfg.PersonaPath(cfg.Souls[0])); err != nil {
		t.Errorf("resolved persona path is not readable: %v", err)
	}
}

// The optional disk limit (documented in specs/components/sandbox.md as
// `disk: 8Gi`) must load; before the Disk field existed, strict parsing rejected it
// as an unknown key.
func TestLoadInfraDiskLimit(t *testing.T) {
	dir := t.TempDir()
	body := "sandbox:\n  limits: { cpu: 1, mem: 1Gi, disk: 8Gi, wall: 10m }\n"
	if err := os.WriteFile(filepath.Join(dir, "infra.dev.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := LoadInfra(filepath.Join(dir, "infra.dev.yaml"))
	if err != nil {
		t.Fatalf("LoadInfra: %v", err)
	}
	if in.Sandbox.Limits.Disk != "8Gi" {
		t.Errorf("disk = %q, want 8Gi", in.Sandbox.Limits.Disk)
	}
}

func TestPersonaPathAbsolute(t *testing.T) {
	cfg := &Config{Root: "/cfg"}
	abs := "/etc/personas/x.md"
	if got := cfg.PersonaPath(core.Soul{Persona: abs}); got != abs {
		t.Errorf("absolute PersonaPath = %q, want unchanged %q", got, abs)
	}
}

// Strict parsing turns a typo'd key into a loud load error instead of a silent
// zero-value, which in an autonomous pipeline would fail badly mid-run.
func TestStrictUnknownKey(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "factory.yaml")
	if err := os.WriteFile(bad, []byte("dag: {}\npolcy: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHarness(bad); err == nil {
		t.Fatal("LoadHarness accepted an unknown top-level key, want error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := LoadHarness(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("LoadHarness accepted a missing file, want error")
	}
	if _, err := LoadInfra(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("LoadInfra accepted a missing file, want error")
	}
}

// An empty document is parsed to the zero value without error; completeness checks
// (e.g. a missing DAG) belong to software-factory validate, not the loader.
func TestEmptyDocument(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "factory.yaml")
	if err := os.WriteFile(empty, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHarness(empty)
	if err != nil {
		t.Fatalf("LoadHarness(empty): %v", err)
	}
	if len(h.DAG) != 0 {
		t.Errorf("empty factory DAG = %v, want empty", h.DAG)
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "infra.dev.yaml")
	if err := os.WriteFile(bad, []byte("sandbox:\n  limits: { wall: not-a-duration }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInfra(bad); err == nil {
		t.Fatal("LoadInfra accepted an invalid duration, want error")
	}
}
