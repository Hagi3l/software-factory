package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	return core.Soul{Name: name, Role: role, Model: "claude-opus-4-7", Persona: f}
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
				"persona: souls/prompts/"+rs.name+".md\n")
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
