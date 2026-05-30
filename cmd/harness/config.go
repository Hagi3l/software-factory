package main

import (
	"path/filepath"
	"sort"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/query"
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

// pipelineRoles returns the agent-stage roles in pipeline order — the order their
// stages are reached by walking `produces` edges from the entry stage(s). It is the
// left-to-right column order for the control-room board, whose issues group by role
// (a "stage" key in the DAG carries a `role`; an issue's Role is what it dispatches to).
// Stages with no role (the human requirements stage, the trusted-merge integrate stage)
// contribute no column. A resolve stage is reached by no produces edge, so its role is
// appended after the linear flow. Each role appears once, in first-reached order.
//
// Deterministic: entry stages and any unreached remainder are visited in sorted name
// order, so the column order is stable across runs.
func pipelineRoles(cfg *config.Config) []string {
	if cfg.Harness == nil {
		return nil
	}
	dag := cfg.Harness.DAG

	produced := map[string]bool{}
	for _, st := range dag {
		for _, p := range st.Produces {
			produced[p] = true
		}
	}

	var order []string
	seenRole := map[string]bool{}
	seenStage := map[string]bool{}
	// bfs walks produces edges breadth-first from seeds, emitting each stage's role in
	// level order. Breadth-first (not depth-first) is what places a join stage after
	// *both* of its upstream branches, so the board columns read left-to-right like the
	// flow even when the DAG forks and re-converges.
	bfs := func(seeds []string) {
		queue := append([]string(nil), seeds...)
		for len(queue) > 0 {
			stage := queue[0]
			queue = queue[1:]
			if seenStage[stage] {
				continue
			}
			seenStage[stage] = true
			st, ok := dag[stage]
			if !ok {
				continue
			}
			if st.Role != "" && !seenRole[st.Role] {
				seenRole[st.Role] = true
				order = append(order, st.Role)
			}
			queue = append(queue, st.Produces...)
		}
	}

	// Entry stages (produces-indegree 0), in sorted name order for stability. A resolve
	// stage is also unproduced — the orchestrator spawns it on a conflict, not via a
	// produces edge — but it is not a pipeline entry, so it is excluded here and picked
	// up in the remainder pass, which appends its role after the linear flow.
	var entries, remainder []string
	for name, st := range dag {
		if !produced[name] && st.Kind != config.StageKindResolve {
			entries = append(entries, name)
		}
	}
	sort.Strings(entries)
	bfs(entries)

	for name := range dag {
		if !seenStage[name] {
			remainder = append(remainder, name)
		}
	}
	sort.Strings(remainder)
	bfs(remainder)
	return order
}

// budgetCaps projects the configured termination-guarantee policy into the control room's
// budget-view caps (T4.10): the per-issue cumulative Budget, the per-epic aggregate
// EpicBudget, and the retry cap. The composition root owns this mapping so the read model
// (query) stays free of a config dependency. A nil Harness (never the case on the run path,
// which validates first) yields zero caps — every dimension reads as uncapped.
func budgetCaps(cfg *config.Config) query.BudgetCaps {
	if cfg.Harness == nil {
		return query.BudgetCaps{}
	}
	p := cfg.Harness.Policy
	return query.BudgetCaps{
		IssueTokens: p.Budget.Tokens,
		IssueUSD:    p.Budget.USD,
		IssueWall:   p.Budget.Wall.Duration(),
		EpicTokens:  p.EpicBudget.Tokens,
		EpicUSD:     p.EpicBudget.USD,
		MaxRetries:  p.MaxRetries,
	}
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
