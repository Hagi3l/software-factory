package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
)

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestEntryRole proves seed infers the pipeline's single entry stage (plan -> planner,
// the agent stage no other stage produces) so an operator need not name it, and that it
// refuses to guess when the DAG is ambiguous or rootless.
func TestEntryRole(t *testing.T) {
	cfg := loadTestConfig(t)
	role, err := entryRole(cfg)
	if err != nil {
		t.Fatalf("entryRole: %v", err)
	}
	if role != "planner" {
		t.Fatalf("entryRole = %q, want %q", role, "planner")
	}

	// A resolve stage (kind: resolve) is also unproduced — the orchestrator spawns it on a
	// conflict, not through a produces edge — but it is NOT a pipeline entry, so it must be
	// excluded: plan + resolve must still resolve to the single entry `planner`, not error.
	withResolve := &config.Config{Harness: &config.Harness{DAG: map[string]config.Stage{
		"plan":      {Role: "planner", Kind: config.StageKindPlan, Produces: []string{"implement"}},
		"implement": {Role: "implementor", Produces: []string{"integrate"}},
		"integrate": {Kind: config.StageKindTrustedMerge},
		"resolve":   {Role: "merge-resolver", Kind: config.StageKindResolve, Produces: []string{"integrate"}},
	}}}
	if r, rerr := entryRole(withResolve); rerr != nil || r != "planner" {
		t.Fatalf("entryRole with a resolve stage = (%q,%v), want (planner,nil) — resolve is not a pipeline entry", r, rerr)
	}

	// Two unproduced agent stages -> ambiguous, must error.
	ambiguous := &config.Config{Harness: &config.Harness{DAG: map[string]config.Stage{
		"a": {Role: "ra"},
		"b": {Role: "rb"},
	}}}
	if _, err := entryRole(ambiguous); err == nil {
		t.Fatal("entryRole on ambiguous DAG: want error, got nil")
	}

	// Every agent stage is produced by another -> no entry, must error.
	rootless := &config.Config{Harness: &config.Harness{DAG: map[string]config.Stage{
		"a": {Role: "ra", Produces: []string{"b"}},
		"b": {Role: "rb", Produces: []string{"a"}},
	}}}
	if _, err := entryRole(rootless); err == nil {
		t.Fatal("entryRole on rootless DAG: want error, got nil")
	}
}

func TestAgentRolesAndRoleIsAgentStage(t *testing.T) {
	cfg := loadTestConfig(t)

	roles := agentRoles(cfg)
	want := []string{"implementor", "merge-resolver", "planner", "security", "test-author"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("agentRoles = %v, want %v", roles, want)
	}
	if !roleIsAgentStage(cfg, "merge-resolver") {
		t.Fatal("merge-resolver should be an agent stage (the resolve role)")
	}
	if !roleIsAgentStage(cfg, "implementor") {
		t.Fatal("implementor should be an agent stage")
	}
	if !roleIsAgentStage(cfg, "planner") {
		t.Fatal("planner should be an agent stage (the plan role)")
	}
	if !roleIsAgentStage(cfg, "test-author") {
		t.Fatal("test-author should be an agent stage")
	}
	if !roleIsAgentStage(cfg, "security") {
		t.Fatal("security should be an agent stage (the qa role)")
	}
	if roleIsAgentStage(cfg, "integrate") {
		t.Fatal("integrate is a trusted-merge stage (no role), not an agent stage")
	}
	if roleIsAgentStage(cfg, "nope") {
		t.Fatal("unknown role should not be an agent stage")
	}
}

// TestPipelineRoles proves the board column order follows the DAG's produces edges, not
// alphabetical role order: the shipped pipeline is plan->author-tests->implement->qa, so
// the columns read planner, test-author, implementor, security, with the out-of-band
// resolve role (reached by no produces edge) appended last. This is what makes the board
// read left-to-right like the flow rather than scrambling the stages.
func TestPipelineRoles(t *testing.T) {
	cfg := loadTestConfig(t)
	roles := pipelineRoles(cfg)
	want := []string{"planner", "test-author", "implementor", "security", "merge-resolver"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("pipelineRoles = %v, want %v", roles, want)
	}

	// Roleless stages (human requirements, trusted-merge integrate) contribute no column,
	// and each role appears exactly once even when a diamond re-converges.
	diamond := &config.Config{Harness: &config.Harness{DAG: map[string]config.Stage{
		"requirements": {Kind: config.StageKindHuman, Produces: []string{"plan"}},
		"plan":         {Role: "planner", Kind: config.StageKindPlan, Produces: []string{"a", "b"}},
		"a":            {Role: "ra", Produces: []string{"join"}},
		"b":            {Role: "rb", Produces: []string{"join"}},
		"join":         {Role: "rj", Produces: []string{"integrate"}},
		"integrate":    {Kind: config.StageKindTrustedMerge},
	}}}
	got := pipelineRoles(diamond)
	want = []string{"planner", "ra", "rb", "rj"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pipelineRoles(diamond) = %v, want %v", got, want)
	}

	if r := pipelineRoles(&config.Config{}); r != nil {
		t.Fatalf("pipelineRoles(no harness) = %v, want nil", r)
	}
}

// TestResolvePersonas confirms persona paths become absolute and still point at a
// real file — the in-process agent loop reads this path directly off the host, so a
// relative path would break the moment the process runs from another directory.
func TestResolvePersonas(t *testing.T) {
	cfg := loadTestConfig(t)
	resolvePersonas(cfg)
	for _, s := range cfg.Souls {
		if !filepath.IsAbs(s.Persona) {
			t.Fatalf("soul %q persona not absolute: %q", s.Name, s.Persona)
		}
		if _, err := os.Stat(s.Persona); err != nil {
			t.Fatalf("soul %q persona missing: %v", s.Name, err)
		}
	}
}

// TestAgentRolesDistinct guards the de-dup + sort, which determines runner consumer
// binding.
func TestAgentRolesDistinct(t *testing.T) {
	cfg := &config.Config{Souls: []core.Soul{
		{Name: "b", Role: "z"},
		{Name: "a", Role: "a"},
		{Name: "c", Role: "z"}, // duplicate role
		{Name: "d", Role: ""},  // no role
	}}
	roles := agentRoles(cfg)
	if len(roles) != 2 || roles[0] != "a" || roles[1] != "z" {
		t.Fatalf("agentRoles = %v, want [a z]", roles)
	}
}
