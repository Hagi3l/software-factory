package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
)

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestEntryRole proves seed infers the kernel's single entry stage (implement ->
// implementor) so an operator need not name it, and that it refuses to guess when
// the DAG is ambiguous or rootless.
func TestEntryRole(t *testing.T) {
	cfg := loadTestConfig(t)
	role, err := entryRole(cfg)
	if err != nil {
		t.Fatalf("entryRole: %v", err)
	}
	if role != "implementor" {
		t.Fatalf("entryRole = %q, want %q", role, "implementor")
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
	if len(roles) != 1 || roles[0] != "implementor" {
		t.Fatalf("agentRoles = %v, want [implementor]", roles)
	}
	if !roleIsAgentStage(cfg, "implementor") {
		t.Fatal("implementor should be an agent stage")
	}
	if roleIsAgentStage(cfg, "integrate") {
		t.Fatal("integrate is a trusted-merge stage (no role), not an agent stage")
	}
	if roleIsAgentStage(cfg, "nope") {
		t.Fatal("unknown role should not be an agent stage")
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
