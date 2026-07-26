package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// validConfig returns a sound configuration mirroring the canonical specs example,
// with souls covering every agent role. Individual tests mutate the returned value
// to provoke a single problem.
func validConfig() *Config {
	return &Config{
		Root: "", // personas are absolute (existing files) in these tests
		Harness: &Harness{
			DAG: map[string]Stage{
				"requirements": {Kind: "human"},
				"plan":         {Role: "planner", Kind: StageKindPlan, Produces: []string{"author-tests"}},
				"author-tests": {Role: "test-author", Postcondition: []string{"tests-red"}, Produces: []string{"implement"}},
				"implement": {
					Role:          "implementor",
					Precondition:  "blockers-closed",
					Postcondition: []string{"tests-red-then-green"},
					OnFailure:     "implement",
					Produces:      []string{"qa"},
				},
				"qa": {
					Role:          "security",
					Postcondition: []string{"tests-pass", "mutation>=0.8", "gosec", "govulncheck", "license-scan"},
					OnFailure:     "implement",
					Produces:      []string{"integrate"},
				},
				"integrate": {Kind: "trusted-merge"},
			},
			Checks: map[string]string{
				"tests-pass":   "go test ./...",
				"gosec":        "gosec ./...",
				"govulncheck":  "govulncheck ./...",
				"license-scan": "go-licenses check ./...",
				"mutation":     "gremlins unleash --output /tmp/m.json && jq -r .efficacy /tmp/m.json",
			},
			Policy: Policy{MaxRetries: 3, DeadLetter: "harness.dlq"},
		},
		Infra: &Infra{
			Models: map[string]ModelProvider{
				"claude-opus-4-7": {Provider: "anthropic"},
			},
			Sandbox: SandboxConfig{
				Backend:  BackendDocker,
				Profiles: map[string]SandboxProfile{"go-toolchain": {Image: "harness/go-toolchain:dev"}},
			},
		},
	}
}

// soul builds a soul whose persona points at a real file so the persona-existence
// check passes. Tests that want the persona check to fail clear Persona afterward.
func soul(t *testing.T, name, role string) core.Soul {
	t.Helper()
	f := filepath.Join(t.TempDir(), name+".md")
	if err := os.WriteFile(f, []byte("# persona\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Soul{Name: name, Role: role, Model: "claude-opus-4-7", Persona: f, Sandbox: "go-toolchain"}
}

// fullSouls returns one soul per agent role in validConfig's DAG.
func fullSouls(t *testing.T) []core.Soul {
	t.Helper()
	return []core.Soul{
		soul(t, "planner", "planner"),
		soul(t, "test-author", "test-author"),
		soul(t, "implementor", "implementor"),
		soul(t, "security", "security"),
	}
}

// problems runs Validate and returns the problem list, requiring that validation
// failed. fragment, if non-empty, must appear in at least one problem.
func problems(t *testing.T, c *Config) []string {
	t.Helper()
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate passed, want failure")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want *ValidationError", err)
	}
	return ve.Problems
}

func mustContain(t *testing.T, probs []string, fragment string) {
	t.Helper()
	for _, p := range probs {
		if strings.Contains(p, fragment) {
			return
		}
	}
	t.Fatalf("no problem contained %q; got %v", fragment, probs)
}

// The canonical config with a soul for every role must validate cleanly — this is
// the contract that the documented example is sound.
func TestValidateHappyPath(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A role used by a stage with no soul to fulfill it must fail.
func TestValidateRoleWithoutSoul(t *testing.T) {
	c := validConfig()
	c.Souls = []core.Soul{
		soul(t, "planner", "planner"),
		soul(t, "test-author", "test-author"),
		soul(t, "implementor", "implementor"),
		// no soul for "security"
	}
	mustContain(t, problems(t, c), `dag role "security"`)
}

// A soul whose role no stage uses is dead config and must fail.
func TestValidateSoulRoleUnused(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), soul(t, "ghost", "nonexistent-role"))
	mustContain(t, problems(t, c), `role "nonexistent-role" which no dag stage uses`)
}

func TestValidateUndefinedProducesTarget(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.Produces = []string{"nowhere"}
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), `produces undefined stage "nowhere"`)
}

func TestValidateUndefinedOnFailureTarget(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.OnFailure = "gone"
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), `on_failure target "gone" is undefined`)
}

func TestValidateUnknownPrecondition(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["implement"]
	st.Precondition = "made-up-guard"
	c.Harness.DAG["implement"] = st
	mustContain(t, problems(t, c), `precondition "made-up-guard"`)
}

func TestValidateUnknownPostcondition(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.Postcondition = []string{"tests-pass", "coverage>=0.9"} // coverage is not a known metric
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), `postcondition "coverage>=0.9"`)
}

// A command-check postcondition with no entry in the `checks` registry is a typo
// that would have nothing to run at the gate, so validation must reject it. This is
// the configuration-time half of bridging postconditions to gate checks.
func TestValidateCommandCheckWithoutRegistryEntry(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.Postcondition = []string{"lint-pass"} // no checks: entry defines it
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), `postcondition "lint-pass"`)
}

// The red→green proof has no command of its own; it reuses the acceptance-test command
// (tests-pass). A stage that declares the proof without that command registered would be
// unresolvable at the gate, so validation must reject it at startup (T2.3).
func TestValidateRedGreenWithoutAcceptanceCommand(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	delete(c.Harness.Checks, "tests-pass") // implement still declares tests-red-then-green
	mustContain(t, problems(t, c), `stage "implement" declares the "tests-red-then-green" proof but no "tests-pass" command`)
}

// The tests-red proof (author-tests) likewise reuses the acceptance-test command; a
// stage that declares it without that command registered is unresolvable at the gate,
// so validation must reject it at startup (T2.4).
func TestValidateTestsRedWithoutAcceptanceCommand(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	delete(c.Harness.Checks, "tests-pass") // author-tests still declares tests-red
	mustContain(t, problems(t, c), `stage "author-tests" declares the "tests-red" proof but no "tests-pass" command`)
}

// A registered check with an empty command would silently fail every candidate, so an
// empty command is a validation error.
func TestValidateEmptyCheckCommand(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Checks["tests-pass"] = "   "
	mustContain(t, problems(t, c), `check "tests-pass" has an empty command`)
}

// A comparison against a known metric with a numeric threshold is accepted; a
// malformed threshold is not.
func TestValidateComparisonCondition(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.Postcondition = []string{"mutation>=not-a-number"}
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), `postcondition "mutation>=not-a-number"`)
}

// A metric postcondition (e.g. "mutation>=0.8") binds to the measurement command
// registered under its metric name; the gate runs that command and grades the score it
// prints against the threshold. A stage that declares the comparison without that command
// registered is unresolvable at the gate, so validation must reject it at startup (T2.7).
func TestValidateMetricWithoutCommand(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	delete(c.Harness.Checks, "mutation") // qa still declares mutation>=0.8
	mustContain(t, problems(t, c), `stage "qa" declares the "mutation>=0.8" metric postcondition but no "mutation" command`)
}

// independent_checks names the command checks the gate keeps running past a failure so one
// qa pass aggregates every scanner finding (T2.12). A well-formed list of registered command
// checks validates cleanly.
func TestValidateIndependentChecksValid(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.IndependentChecks = []string{"gosec", "govulncheck", "license-scan"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid independent_checks rejected: %v", err)
	}
}

// Only plain command checks may be marked independent: a reserved proof, an unregistered
// name, a metric's measurement command, and a duplicate are each a config fault caught at
// startup (the gate would otherwise silently ignore the misclassification).
func TestValidateIndependentChecksRejections(t *testing.T) {
	cases := map[string]struct {
		names    []string
		fragment string
	}{
		"reserved proof": {[]string{"tests-red-then-green"}, `reserved proof "tests-red-then-green"`},
		"unregistered":   {[]string{"nope"}, `references "nope", which is not a command in checks:`},
		"metric command": {[]string{"mutation"}, `lists "mutation", a metric measurement command`},
		"duplicate":      {[]string{"gosec", "gosec"}, `lists "gosec" more than once`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Souls = fullSouls(t)
			c.Harness.IndependentChecks = tc.names
			mustContain(t, problems(t, c), tc.fragment)
		})
	}
}

// An absent integration block, an explicit per-item, and an explicit epic are all sound; the
// default (absent) and explicit per-item must behave identically (Mode() == per-item).
func TestValidateIntegrationModeValid(t *testing.T) {
	for name, mode := range map[string]*Integration{
		"absent":           nil,
		"empty":            {Mode: ""},
		"explicit peritem": {Mode: IntegrationPerItem},
		"explicit epic":    {Mode: IntegrationEpic},
	} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Souls = fullSouls(t)
			c.Harness.Integration = mode
			if err := c.Validate(); err != nil {
				t.Fatalf("valid integration.mode rejected: %v", err)
			}
		})
	}
}

// Default and explicit per-item resolve to the same mode; an unset block must not read as epic.
func TestHarnessModeDefaultsPerItem(t *testing.T) {
	if got := (&Harness{}).Mode(); got != IntegrationPerItem {
		t.Errorf("absent integration block Mode() = %q, want %q", got, IntegrationPerItem)
	}
	if got := (&Harness{Integration: &Integration{Mode: IntegrationEpic}}).Mode(); got != IntegrationEpic {
		t.Errorf("explicit epic Mode() = %q, want %q", got, IntegrationEpic)
	}
}

// An unrecognized mode is a loud config fault, not a silent fall-through to per-item.
func TestValidateIntegrationModeUnknown(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Integration = &Integration{Mode: "epics"}
	mustContain(t, problems(t, c), `integration.mode "epics" is unknown`)
}

// A produces self-loop is a depth cycle and must fail.
func TestValidateProducesSelfLoop(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["qa"]
	st.Produces = []string{"qa"}
	c.Harness.DAG["qa"] = st
	mustContain(t, problems(t, c), "cycle")
}

// A multi-stage produces cycle must fail and the stages caught in it are
// unreachable from any root.
func TestValidateProducesCycle(t *testing.T) {
	c := &Config{
		Harness: &Harness{
			DAG: map[string]Stage{
				"a": {Role: "ra", Produces: []string{"b"}},
				"b": {Role: "rb", Produces: []string{"a"}},
			},
			Policy: Policy{DeadLetter: "harness.dlq"},
		},
		Infra: &Infra{Models: map[string]ModelProvider{"m": {Provider: "anthropic"}}},
	}
	probs := problems(t, c)
	mustContain(t, probs, "cycle")
	mustContain(t, probs, "unreachable")
}

func TestValidateStageNeitherRoleNorKind(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.DAG["limbo"] = Stage{}
	mustContain(t, problems(t, c), `stage "limbo" sets neither role nor kind`)
}

func TestValidateStageBothRoleAndKind(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.DAG["weird"] = Stage{Role: "planner", Kind: "human"}
	mustContain(t, problems(t, c), `sets both role`)
}

// A plan stage is the one kind that coexists with a role — it dispatches to a planner
// soul — so a kind:plan stage with no role is a config fault (nothing to dispatch).
func TestValidatePlanStageWithoutRole(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["plan"]
	st.Role = ""
	c.Harness.DAG["plan"] = st
	mustContain(t, problems(t, c), `stage "plan" has kind "plan" but no role`)
}

// A plan stage is not sandbox-gated (the planner writes no candidate to grade), so
// declaring a postcondition on it is a config fault.
func TestValidatePlanStageWithPostcondition(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	st := c.Harness.DAG["plan"]
	st.Postcondition = []string{"tests-pass"}
	c.Harness.DAG["plan"] = st
	mustContain(t, problems(t, c), `stage "plan" has kind "plan" and a postcondition`)
}

// An unrecognized kind is a typo that would otherwise be dispatched (or not) by accident.
func TestValidateUnknownStageKind(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.DAG["odd"] = Stage{Kind: "teleport"}
	mustContain(t, problems(t, c), `stage "odd" has unknown kind "teleport"`)
}

func TestValidateMissingPersona(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[0].Persona = "/no/such/persona/file.md"
	c.Souls = souls
	mustContain(t, problems(t, c), "does not exist")
}

func TestValidateEmptyPersona(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[0].Persona = ""
	c.Souls = souls
	mustContain(t, problems(t, c), "has no persona path")
}

func TestValidateSelectorEmptyValue(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[0].Selector = map[string]string{"lang": ""}
	c.Souls = souls
	mustContain(t, problems(t, c), "has an empty value")
}

func TestValidateDuplicateSoulName(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	dup := soul(t, "planner", "test-author") // same name as souls[0]
	souls = append(souls, dup)
	c.Souls = souls
	mustContain(t, problems(t, c), `defined more than once`)
}

// Two souls fulfilling one role must have distinguishing selectors. Identical selectors
// make one unreachable (selectSoul would always pick the same one), which is a config
// fault, not a valid specialization.
func TestValidateDuplicateSelectorForRole(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	a := soul(t, "impl-a", "implementor")
	a.Selector = map[string]string{"lang": "go"}
	b := soul(t, "impl-b", "implementor")
	b.Selector = map[string]string{"lang": "go"} // same role + same selector as impl-a
	souls = append(souls, a, b)
	c.Souls = souls
	mustContain(t, problems(t, c), "with the same selector")
}

// Two souls for one role with *different* selectors are a legitimate specialization and
// must validate cleanly.
func TestValidateDistinctSelectorsForRole(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	a := soul(t, "impl-go", "implementor")
	a.Selector = map[string]string{"lang": "go"}
	b := soul(t, "impl-rust", "implementor")
	b.Selector = map[string]string{"lang": "rust"}
	// Replace the single implementor soul with the two specialized ones.
	var kept []core.Soul
	for _, s := range souls {
		if s.Role != "implementor" {
			kept = append(kept, s)
		}
	}
	kept = append(kept, a, b)
	c.Souls = kept
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with distinct selectors: %v", err)
	}
}

func TestValidateModelNotInRegistry(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[0].Model = "ghost-model"
	c.Souls = souls
	mustContain(t, problems(t, c), `model "ghost-model" which the infra model registry does not define`)
}

func TestValidateOpenAICompatNeedsEndpoint(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["local"] = ModelProvider{Provider: "openai-compat"} // no endpoint
	mustContain(t, problems(t, c), "has no endpoint")
}

func TestValidateEffortRejectsUnknownLevel(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["fast-opus"] = ModelProvider{Provider: "anthropic", Effort: "turbo"}
	mustContain(t, problems(t, c), `invalid effort "turbo"`)
}

// effort on openai-compat is now allowed, but the heterogeneous surface means the operator
// must declare the wire form (effort_param) explicitly — a missing one is a hard error, not a
// silent no-op at run time.
func TestValidateEffortOnOpenAICompatRequiresParam(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", Effort: "medium"}
	mustContain(t, problems(t, c), "sets effort on provider openai-compat but no effort_param")
}

func TestValidateEffortParamVerbosityOnOpenAICompatPasses(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", Effort: "medium", EffortParam: "verbosity"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate returned %v, want nil for openai-compat effort+effort_param entry", err)
	}
}

func TestValidateEffortParamRejectsUnknown(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", Effort: "medium", EffortParam: "loud"}
	mustContain(t, problems(t, c), `invalid effort_param "loud"`)
}

// effort_param is meaningless on native anthropic (one wire form) — rejected so config stays honest.
func TestValidateEffortParamRejectedOnAnthropic(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["opus"] = ModelProvider{Provider: "anthropic", Effort: "medium", EffortParam: "verbosity"}
	mustContain(t, problems(t, c), "effort_param but provider is anthropic")
}

func TestValidateEffortRejectedOnNativeOpenAI(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["gpt"] = ModelProvider{Provider: "openai", Effort: "medium"}
	mustContain(t, problems(t, c), "native openai is not yet wired")
}

// idle_timeout is enforced by the streaming openai adapter, so it passes on openai-compat...
func TestValidateIdleTimeoutOnOpenAICompatPasses(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", IdleTimeout: Duration(90 * time.Second)}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate returned %v, want nil for openai-compat idle_timeout entry", err)
	}
}

// ...is rejected on native anthropic (not wired there) rather than silently doing nothing...
func TestValidateIdleTimeoutRejectedOnAnthropic(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["opus"] = ModelProvider{Provider: "anthropic", IdleTimeout: Duration(90 * time.Second)}
	mustContain(t, problems(t, c), "sets idle_timeout but provider is")
}

// ...and a negative duration is a config error.
func TestValidateIdleTimeoutRejectsNegative(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", IdleTimeout: Duration(-1)}
	mustContain(t, problems(t, c), "negative idle_timeout")
}

func TestValidateEffortParamWithoutEffort(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["compat"] = ModelProvider{Provider: "openai-compat", Endpoint: "https://x", EffortParam: "verbosity"}
	mustContain(t, problems(t, c), "sets effort_param but no effort")
}

func TestValidateEffortValidLevelOnAnthropicPasses(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["opus-medium"] = ModelProvider{Provider: "anthropic", Effort: "medium"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate returned %v, want nil for valid anthropic+effort entry", err)
	}
}

// prompt_caching is an openai-compat-only marker (the native anthropic adapter caches
// unconditionally, native openai auto-caches) — setting it elsewhere is a config mistake the
// gate must reject so it never silently no-ops.
func TestValidatePromptCachingRejectedOffOpenAICompat(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["cached-opus"] = ModelProvider{Provider: "anthropic", PromptCaching: true}
	mustContain(t, problems(t, c), "prompt_caching but provider")
}

func TestValidatePromptCachingOnOpenAICompatPasses(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Models["cached-compat"] = ModelProvider{
		Provider: "openai-compat", Endpoint: "https://openrouter.ai/api/v1", PromptCaching: true,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate returned %v, want nil for openai-compat + prompt_caching", err)
	}
}

// The telemetry endpoint is validated at the startup gate so a typo becomes a loud error
// rather than silently-dropped exports — the contract telemetry.Setup relies on. The three
// valid forms (off / stdout / host:port) must pass; anything else must fail.
func TestValidateOTelEndpointValidForms(t *testing.T) {
	for _, ep := range []string{"", "stdout", "localhost:4317", "127.0.0.1:4317", "[::1]:4317"} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.OTel = OTelConfig{Endpoint: ep}
		if err := c.Validate(); err != nil {
			t.Errorf("endpoint %q: Validate returned %v, want nil", ep, err)
		}
	}
}

func TestValidateOTelEndpointRejectsMalformed(t *testing.T) {
	// A bare hostname (no port), an empty host, and an empty port are all unusable as an
	// OTLP/gRPC collector address and must be caught, not passed through to a lazy dial.
	for _, ep := range []string{"not-a-host-port", "localhost", ":4317", "host:"} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.OTel = OTelConfig{Endpoint: ep}
		mustContain(t, problems(t, c), "otel.endpoint")
	}
}

// Export headers carry a backend's auth + routing metadata. A credential value must be an
// ${ENV_VAR} reference (so the secret lives in the environment, never in config), while
// non-credential routing metadata may be a literal — the discipline validateOTelHeaders
// enforces. See specs/observability.md.
func TestValidateOTelHeadersValid(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.OTel = OTelConfig{
		Endpoint: "localhost:5081",
		Headers: map[string]string{
			"organization":  "default",           // routing metadata — literal OK
			"stream-name":   "default",           // routing metadata — literal OK
			"authorization": "${OTEL_OTLP_AUTH}", // credential — must be an env ref
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateOTelHeadersRejectsLiteralCredential(t *testing.T) {
	// A credential-named header (authorization) with a literal value is a secret committed
	// to config — the exact thing the env-ref discipline forbids.
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.OTel = OTelConfig{
		Endpoint: "localhost:5081",
		Headers:  map[string]string{"authorization": "Basic c2VjcmV0"},
	}
	mustContain(t, problems(t, c), "must be an ${ENV_VAR} reference")
}

func TestValidateOTelHeadersRejectsMalformedRef(t *testing.T) {
	// A typo'd reference ("${OTEL_OTLP_AUTH" with no close) would resolve to a useless
	// literal and ship a broken auth header — caught at the gate instead.
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.OTel = OTelConfig{
		Endpoint: "localhost:5081",
		Headers:  map[string]string{"x-api-key": "${OTEL_KEY"},
	}
	mustContain(t, problems(t, c), "malformed ${ENV_VAR} reference")
}

// ResolveHeaders expands ${ENV} references from the environment at the last responsible
// moment; literals pass through and an unset var resolves to empty (fail-closed at the
// backend, not at config load).
func TestResolveHeadersExpandsEnv(t *testing.T) {
	t.Setenv("OTEL_OTLP_AUTH", "Basic resolved-secret")
	o := OTelConfig{Headers: map[string]string{
		"organization":  "default",
		"authorization": "${OTEL_OTLP_AUTH}",
		"x-unset":       "${OTEL_NOT_SET}",
	}}
	got := o.ResolveHeaders()
	if got["organization"] != "default" {
		t.Errorf("literal header changed: %q", got["organization"])
	}
	if got["authorization"] != "Basic resolved-secret" {
		t.Errorf("env ref not expanded: %q", got["authorization"])
	}
	if got["x-unset"] != "" {
		t.Errorf("unset env ref should resolve to empty, got %q", got["x-unset"])
	}
	if (OTelConfig{}).ResolveHeaders() != nil {
		t.Error("no headers should resolve to nil (no WithHeaders option)")
	}
}

// The requirements-planner block is optional, but when present its tuning knobs must be
// non-negative. max_tool_turns (read-only exploration round-trips per human turn) and
// turn_timeout (per-turn wall-clock) join max_tokens as bounds the wizard reads; a negative
// value is meaningless and would otherwise be silently coerced, so it is caught at the gate.
func planner(t *testing.T) *RequirementsPlanner {
	t.Helper()
	f := filepath.Join(t.TempDir(), "requirements-planner.md")
	if err := os.WriteFile(f, []byte("# planner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &RequirementsPlanner{Model: "claude-opus-4-7", Persona: f}
}

func TestValidateRequirementsPlannerValidTuning(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	rp := planner(t)
	rp.MaxTokens = 16384
	rp.MaxToolTurns = 40
	rp.TurnTimeout = Duration(10 * 60 * 1e9) // 10m
	c.Harness.RequirementsPlanner = rp
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRequirementsPlannerNegativeMaxToolTurns(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	rp := planner(t)
	rp.MaxToolTurns = -1
	c.Harness.RequirementsPlanner = rp
	mustContain(t, problems(t, c), "max_tool_turns")
}

func TestValidateRequirementsPlannerNegativeTurnTimeout(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	rp := planner(t)
	rp.TurnTimeout = Duration(-1)
	c.Harness.RequirementsPlanner = rp
	mustContain(t, problems(t, c), "turn_timeout")
}

// The optional prepared-requirement file (prefill) must exist when named: the wizard reads
// it per page load, so a bad path would otherwise only surface as a silently absent insert
// button at the moment the operator relies on it.
func TestValidateRequirementsPlannerPrefillExists(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	rp := planner(t)
	f := filepath.Join(t.TempDir(), "prefill.md")
	if err := os.WriteFile(f, []byte("prepared requirement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rp.Prefill = f
	c.Harness.RequirementsPlanner = rp
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRequirementsPlannerPrefillMissing(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	rp := planner(t)
	rp.Prefill = filepath.Join(t.TempDir(), "nope.md")
	c.Harness.RequirementsPlanner = rp
	mustContain(t, problems(t, c), "prefill")
}

// The artifact store backend is validated at the startup gate so a distributed (s3)
// deployment with a missing bucket/endpoint fails loud at `harness validate` rather than
// silently dropping every harvested artifact at run time. The files default needs no
// extra config here (its path is checked when the store opens).
func TestValidateArtifactsValidForms(t *testing.T) {
	cases := []ArtifactsConfig{
		{}, // empty default → files
		{Backend: "files", Path: "./.harness/artifacts"},     // explicit files
		{Backend: "s3", Bucket: "b", Region: "us-east-1"},    // AWS via region
		{Backend: "s3", Bucket: "b", Endpoint: "minio:9000"}, // MinIO via endpoint
	}
	for _, a := range cases {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Artifacts = a
		if err := c.Validate(); err != nil {
			t.Errorf("artifacts %+v: Validate returned %v, want nil", a, err)
		}
	}
}

func TestValidateArtifactsRejectsMisconfigured(t *testing.T) {
	cases := []struct {
		a    ArtifactsConfig
		want string
	}{
		{ArtifactsConfig{Backend: "s3", Region: "us-east-1"}, "artifacts.bucket"},               // no bucket
		{ArtifactsConfig{Backend: "s3", Bucket: "b"}, "artifacts.endpoint or artifacts.region"}, // no endpoint+region
		{ArtifactsConfig{Backend: "carrier-pigeon"}, "artifacts.backend"},                       // unknown backend
	}
	for _, tc := range cases {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Artifacts = tc.a
		mustContain(t, problems(t, c), tc.want)
	}
}

// Provenance signing (T5.10): only the run-time-breaking shape is gated — signing turned
// on with no key. The key/allowed-signers paths are NOT existence-checked (the key is a
// runtime-provisioned secret, the API-key posture), so a configured-but-absent file is not
// a validation failure. See specs/security.md, internal/config/infra.go.
func TestValidateSigningValidForms(t *testing.T) {
	cases := []SigningConfig{
		{},                                     // unset → off, the default
		{Enabled: false, Key: "/keys/harness"}, // key present but disabled → off, fine
		{Enabled: true, Key: "/keys/harness"},  // signing on with a key
		{Enabled: true, Key: "/keys/harness", AllowedSigners: "/a"}, // sign + verify-on-read
		{AllowedSigners: "/allowed_signers"},                        // verify-only host (no signing)
	}
	for _, s := range cases {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Signing = s
		if err := c.Validate(); err != nil {
			t.Errorf("signing %+v: Validate returned %v, want nil", s, err)
		}
	}
}

func TestValidateSigningRejectsEnabledWithoutKey(t *testing.T) {
	for _, s := range []SigningConfig{
		{Enabled: true},                          // no key at all
		{Enabled: true, Key: "   "},              // whitespace-only key
		{Enabled: true, AllowedSigners: "/only"}, // allowed-signers without a signing key
	} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Signing = s
		mustContain(t, problems(t, c), "signing.key")
	}
}

// Active gates whether the merger signs: enabled AND a key. Either missing means the
// unsigned path (the same commit the merger always wrote).
func TestSigningConfigActive(t *testing.T) {
	cases := []struct {
		s    SigningConfig
		want bool
	}{
		{SigningConfig{}, false},
		{SigningConfig{Enabled: true}, false},
		{SigningConfig{Key: "/k"}, false},
		{SigningConfig{Enabled: true, Key: "/k"}, true},
	}
	for _, tc := range cases {
		if got := tc.s.Active(); got != tc.want {
			t.Errorf("Active(%+v) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// The NATS endpoint + JetStream knobs are validated at the startup gate so a distributed
// (external-cluster) misconfiguration fails loud at `harness validate` rather than as an
// opaque connect/stream error mid-run. Empty url = the embedded in-process server (the
// dev/bootstrap default); a set url is an external cluster (T5.8). See specs/messaging.md.
func TestValidateNATSValidForms(t *testing.T) {
	cases := []NATSConfig{
		{}, // empty url + zero knobs → embedded, single replica
		{JetStream: JetStreamConfig{Replicas: 1, MaxAge: Duration(1)}},                // embedded, explicit single replica
		{URL: "nats://host:4222", JetStream: JetStreamConfig{Replicas: 3}},            // external cluster, 3 replicas
		{URL: "nats://a:4222,nats://b:4222", JetStream: JetStreamConfig{Replicas: 2}}, // cluster list
		{URL: "host:4222"},       // bare host:port
		{URL: "tls://host:4222"}, // TLS transport
		{URL: "nats://host"},     // scheme without port (defaults to 4222)
	}
	for _, n := range cases {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.NATS = n
		if err := c.Validate(); err != nil {
			t.Errorf("nats %+v: Validate returned %v, want nil", n, err)
		}
	}
}

func TestValidateNATSRejects(t *testing.T) {
	cases := []struct {
		n    NATSConfig
		want string
	}{
		{NATSConfig{JetStream: JetStreamConfig{Replicas: -1}}, "nats.jetstream.replicas"},        // negative replicas
		{NATSConfig{JetStream: JetStreamConfig{Replicas: 3}}, "requires an external cluster"},    // >1 replica but embedded (no url)
		{NATSConfig{URL: "ftp://host:4222"}, "nats.url endpoint"},                                // wrong scheme
		{NATSConfig{URL: "localhost"}, "nats.url endpoint"},                                      // bare host, no port
		{NATSConfig{URL: "nats://good:4222,bad"}, "nats.url endpoint"},                           // one bad endpoint in the list
		{NATSConfig{JetStream: JetStreamConfig{MaxAge: Duration(-1)}}, "nats.jetstream.max_age"}, // negative retention
	}
	for _, tc := range cases {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.NATS = tc.n
		mustContain(t, problems(t, c), tc.want)
	}
}

// validNATSEndpoint is the per-endpoint grammar each comma-separated cluster url is checked against.
func TestValidNATSEndpoint(t *testing.T) {
	for _, ep := range []string{"nats://host:4222", "nats://host", "tls://h:1", "ws://h:1", "wss://h:1", "host:4222", "127.0.0.1:4222", "[::1]:4222"} {
		if !validNATSEndpoint(ep) {
			t.Errorf("validNATSEndpoint(%q) = false, want true", ep)
		}
	}
	for _, ep := range []string{"", "localhost", "ftp://h:1", "://h:1", "nats://", "nats://:4222", "host:"} {
		if validNATSEndpoint(ep) {
			t.Errorf("validNATSEndpoint(%q) = true, want false", ep)
		}
	}
}

// A malformed broker.package_proxy is a guaranteed dependency-fetch failure, so it must be
// caught at the startup gate; a well-formed http(s) URL and an unset proxy both pass (T5.6).
func TestValidateBrokerPackageProxy(t *testing.T) {
	for _, bad := range []string{"not-a-url", "ftp://host/x", "https://", "proxy.golang.org"} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Broker.PackageProxy = bad
		mustContain(t, problems(t, c), "broker.package_proxy")
	}
	for _, ok := range []string{"", "https://proxy.golang.org", "http://mirror.internal:8080/mod"} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Broker.PackageProxy = ok
		if err := c.Validate(); err != nil {
			t.Errorf("package_proxy %q: Validate failed: %v", ok, err)
		}
	}
}

func TestValidateGitRemote(t *testing.T) {
	for _, ok := range []string{
		"", "https://github.com/acme/widgets.git", "http://host/r", "ssh://git@host/r",
		"git://host/r", "file:///srv/repo.git", "git@github.com:acme/widgets.git", "/srv/repo.git",
	} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Git.Remote = ok
		if err := c.Validate(); err != nil {
			t.Errorf("git.remote %q: Validate failed: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://", "ftp://host/r", "not a remote"} {
		c := validConfig()
		c.Souls = fullSouls(t)
		c.Infra.Git.Remote = bad
		mustContain(t, problems(t, c), "git.remote")
	}
}

func TestValidateGitHubApp(t *testing.T) {
	fullApp := GitHubAppConfig{AppID: "1", InstallationID: "2", Repository: "o/r", PrivateKey: "/k.pem"}

	// A fully-configured app with a remote validates (key path existence is NOT checked).
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Git.Remote = "https://github.com/o/r.git"
	c.Infra.Git.GitHubApp = fullApp
	if err := c.Validate(); err != nil {
		t.Errorf("full github_app + remote: Validate failed: %v", err)
	}

	// A partial app (missing private_key) is a fault.
	c = validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Git.Remote = "https://github.com/o/r.git"
	partial := fullApp
	partial.PrivateKey = ""
	c.Infra.Git.GitHubApp = partial
	mustContain(t, problems(t, c), "git.github_app is partially configured")

	// A full app with no remote has nowhere to push.
	c = validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Git.GitHubApp = fullApp
	mustContain(t, problems(t, c), "git.github_app is configured but git.remote is empty")

	// A malformed api_base is a fault.
	c = validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Git.Remote = "https://github.com/o/r.git"
	badBase := fullApp
	badBase.APIBase = "ftp://nope"
	c.Infra.Git.GitHubApp = badBase
	mustContain(t, problems(t, c), "git.github_app.api_base")
}

func TestGitHubAppConfigActive(t *testing.T) {
	if (GitHubAppConfig{}).Active() {
		t.Error("empty github_app should not be Active")
	}
	if (GitHubAppConfig{AppID: "1", InstallationID: "2", Repository: "o/r"}).Active() {
		t.Error("github_app missing private_key should not be Active")
	}
	if !(GitHubAppConfig{AppID: "1", InstallationID: "2", Repository: "o/r", PrivateKey: "/k"}).Active() {
		t.Error("fully-configured github_app should be Active")
	}
}

func TestValidateEmptyDAG(t *testing.T) {
	c := validConfig()
	c.Souls = nil
	c.Harness.DAG = map[string]Stage{}
	mustContain(t, problems(t, c), "dag is empty")
}

func TestValidateMissingHarnessAndInfra(t *testing.T) {
	c := &Config{}
	probs := problems(t, c)
	mustContain(t, probs, "harness configuration is missing")
	mustContain(t, probs, "infra configuration is missing")
}

// Validate must report every problem at once, not just the first, and the list
// must be sorted for deterministic operator output.
func TestValidateReportsAllProblemsSorted(t *testing.T) {
	c := validConfig()
	c.Souls = nil // every role now resolves to no soul: 4 problems
	probs := problems(t, c)
	if len(probs) < 2 {
		t.Fatalf("want multiple problems, got %v", probs)
	}
	for i := 1; i < len(probs); i++ {
		if probs[i-1] > probs[i] {
			t.Errorf("problems not sorted: %q before %q", probs[i-1], probs[i])
		}
	}
}

// writeValidTree writes the canonical harness.yaml and infra.dev.yaml plus a soul
// (and persona) for every agent role, so the loaded config validates cleanly.
func writeValidTree(t *testing.T) string {
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
	write("harness.yaml", harnessYAML)
	write("infra.dev.yaml", infraYAML)
	for _, rs := range []struct{ name, role string }{
		{"planner", "planner"},
		{"test-author", "test-author"},
		{"implementor", "implementor"},
		{"security", "security"},
	} {
		write(filepath.Join("souls", "prompts", rs.name+".md"), "# persona\n")
		write(filepath.Join("souls", rs.name+".yaml"),
			"name: "+rs.name+"\nrole: "+rs.role+"\nmodel: claude-opus-4-7\n"+
				"persona: souls/prompts/"+rs.name+".md\nsandbox: go-toolchain\n")
	}
	return dir
}

// Validate loaded from disk: the on-disk canonical fixture (with a full soul set)
// validates, exercising the real Load -> Validate path including persona resolution
// against the config root.
func TestValidateFromLoad(t *testing.T) {
	dir := writeValidTree(t)
	cfg, err := Load(dir, "dev")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on-disk config: %v", err)
	}
}

// A soul whose sandbox profile has no entry in sandbox.profiles would silently boot the
// bare profile name as an image at the runner/gate; validation must reject it at startup,
// the same way a missing model registry entry is rejected.
func TestValidateSoulSandboxProfileUnresolved(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[2].Sandbox = "rust-toolchain" // not defined in sandbox.profiles
	c.Souls = souls
	mustContain(t, problems(t, c), `sandbox profile "rust-toolchain" which sandbox.profiles does not define`)
}

// A soul with no sandbox profile at all cannot be provisioned; validation must catch it.
func TestValidateSoulNoSandbox(t *testing.T) {
	c := validConfig()
	souls := fullSouls(t)
	souls[1].Sandbox = ""
	c.Souls = souls
	mustContain(t, problems(t, c), `has no sandbox profile`)
}

// A profile entry that exists but carries no artifact for the active backend (here an
// image for docker) is as unresolvable as a missing entry.
func TestValidateSandboxProfileMissingBackendField(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Sandbox.Profiles = map[string]SandboxProfile{"go-toolchain": {Rootfs: "/x.ext4"}} // rootfs, but backend is docker
	mustContain(t, problems(t, c), `has no "image" for the "docker" backend`)
}

// An unknown sandbox backend is a typo that would mis-resolve every profile.
func TestValidateUnknownSandboxBackend(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Infra.Sandbox.Backend = "qemu"
	mustContain(t, problems(t, c), `sandbox.backend "qemu" is unknown`)
}

// explorerSoul builds a soul on the reserved explorer role with a real persona file and the
// given tools/selector, for the explore-validation tests.
func explorerSoul(t *testing.T, name string, tools []string, selector map[string]string) core.Soul {
	t.Helper()
	s := soul(t, name, RoleExplorer)
	s.Tools = tools
	s.Selector = selector
	return s
}

// readOnlySubset is a valid explorer allowlist (the comprehension subset).
func readOnlySubset() []string { return append([]string(nil), ReadOnlyToolNames...) }

// The reserved explorer role is exempt from the "role which no dag stage uses" check even when
// explore is disabled: an explorer soul is a helper invoked as a tool, never a DAG stage. So a
// config with an explorer soul and no explore_budget still validates clean.
func TestValidateExplorerRoleExemptFromDAGCheck(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), explorerSoul(t, "explorer", readOnlySubset(), nil))
	if err := c.Validate(); err != nil {
		t.Fatalf("explorer soul with explore disabled should validate clean, got: %v", err)
	}
}

// explore_budget set + a valid explorer soul is the enabled happy path.
func TestValidateExploreHappyPath(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), explorerSoul(t, "explorer", readOnlySubset(), nil))
	c.Harness.Policy.ExploreBudget = ExploreBudget{Tokens: 100_000, Turns: 12}
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled explore with a valid explorer soul should validate clean, got: %v", err)
	}
}

// Turning explore on (explore_budget set) with no explorer soul is a config fault: the feature
// has no soul to run.
func TestValidateExploreBudgetRequiresExplorerSoul(t *testing.T) {
	c := validConfig()
	c.Souls = fullSouls(t)
	c.Harness.Policy.ExploreBudget = ExploreBudget{Tokens: 100_000}
	mustContain(t, problems(t, c), `no soul declares the reserved "explorer" role`)
}

// An explorer soul that lists a non-read-only tool (a writer) breaks the read-only invariant
// and must fail — even the declared allowlist may not name a writer.
func TestValidateExplorerToolsMustBeReadOnly(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), explorerSoul(t, "explorer", []string{"read_file", "write_file"}, nil))
	c.Harness.Policy.ExploreBudget = ExploreBudget{Turns: 8}
	mustContain(t, problems(t, c), `not a read-only comprehension tool`)
}

// An explorer that lists `explore` would recurse; the allowlist must structurally omit it.
func TestValidateExplorerNoRecursion(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), explorerSoul(t, "explorer", []string{"read_file", "explore"}, nil))
	c.Harness.Policy.ExploreBudget = ExploreBudget{Turns: 8}
	mustContain(t, problems(t, c), `must not call explore (no recursion)`)
}

// Negative budget dimensions are nonsense caps and fail closed.
func TestValidateExploreBudgetNegative(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t), explorerSoul(t, "explorer", readOnlySubset(), nil))
	c.Harness.Policy.ExploreBudget = ExploreBudget{Tokens: -1, Turns: -2}
	probs := problems(t, c)
	mustContain(t, probs, "policy.explore_budget.tokens is -1")
	mustContain(t, probs, "policy.explore_budget.turns is -2")
}

// Two explorer souls sharing the same (catch-all) selector can never both be selected — the
// existing selector-duplicate rule applies to the explorer role too. A verify-path explorer
// must be distinguished by a selector (e.g. verify=1).
func TestValidateExplorerDuplicateSelector(t *testing.T) {
	c := validConfig()
	c.Souls = append(fullSouls(t),
		explorerSoul(t, "explorer-a", readOnlySubset(), nil),
		explorerSoul(t, "explorer-b", readOnlySubset(), nil),
	)
	c.Harness.Policy.ExploreBudget = ExploreBudget{Turns: 8}
	mustContain(t, problems(t, c), `both fulfill role "explorer" with the same selector`)
}
