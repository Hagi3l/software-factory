package main

import (
	"path/filepath"
	"sort"

	"github.com/Loxstomper/harness/internal/config"
)

// loadConfig loads the configuration rooted at dir for the named environment,
// resolving dir to an absolute path first so that config.Config.Root — and every
// persona path derived from it — is absolute regardless of the process working
// directory. It does NOT validate; callers that need the startup gate call Validate
// on the result (harness validate, and run/seed before they wire anything up).
func loadConfig(dir, env string) (*config.Config, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return config.Load(absDir, env)
}

// resolvePersonas rewrites each soul's persona to its absolute path. The in-process
// bootstrap agent loop reads the persona file directly off the host (it has not yet
// moved into the sandbox — Phase 5), so the path it receives via the Brief must be
// resolvable irrespective of the working directory. config root is already absolute
// (loadConfig), and PersonaPath returns absolute paths unchanged, so this is
// idempotent. This is wiring the composition root owns because only it knows the
// config root; the orchestrator embeds the soul into the Brief verbatim.
func resolvePersonas(cfg *config.Config) {
	for i := range cfg.Souls {
		cfg.Souls[i].Persona = cfg.PersonaPath(cfg.Souls[i])
	}
}

// agentRoles returns the distinct, sorted set of roles the souls fulfill — the roles
// a runner must serve work subjects for. validate guarantees every soul role is an
// agent stage, so this is exactly the dispatchable role set. Sorted for deterministic
// consumer binding and log output.
func agentRoles(cfg *config.Config) []string {
	seen := map[string]bool{}
	var roles []string
	for _, s := range cfg.Souls {
		if s.Role != "" && !seen[s.Role] {
			seen[s.Role] = true
			roles = append(roles, s.Role)
		}
	}
	sort.Strings(roles)
	return roles
}

// roleIsAgentStage reports whether role is fulfilled by an agent stage in the DAG.
func roleIsAgentStage(cfg *config.Config, role string) bool {
	if cfg.Harness == nil {
		return false
	}
	for _, st := range cfg.Harness.DAG {
		if st.Role == role {
			return true
		}
	}
	return false
}

// entryRole returns the role of the single entry agent stage — an agent stage that
// no other stage produces (produces-indegree 0). In the shipped DAG that is `plan`.
// It errors if there is not exactly one, asking the operator to name the role
// explicitly, so `seed` never guesses which stage a seed issue enters at.
//
// A resolve stage (kind: resolve) also has produces-indegree 0 — it is spawned by the
// orchestrator on a merge conflict, never reached through a produces edge — but it is
// not a pipeline entry, so it is excluded here; otherwise it would falsely make the
// pipeline look ambiguous (two unproduced agent stages: plan and resolve).
func entryRole(cfg *config.Config) (string, error) {
	if cfg.Harness == nil {
		return "", errNoHarness
	}
	produced := map[string]bool{}
	for _, st := range cfg.Harness.DAG {
		for _, p := range st.Produces {
			produced[p] = true
		}
	}
	var roles []string
	for name, st := range cfg.Harness.DAG {
		if st.Role != "" && !produced[name] && st.Kind != config.StageKindResolve {
			roles = append(roles, st.Role)
		}
	}
	sort.Strings(roles)
	switch len(roles) {
	case 1:
		return roles[0], nil
	case 0:
		return "", errNoEntryStage
	default:
		return "", &ambiguousEntryError{roles: roles}
	}
}
