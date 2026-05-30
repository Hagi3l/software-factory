package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// ValidationError is the result of a failed Validate: the complete list of problems
// found, not just the first. In an autonomous pipeline an operator fixes config
// once and re-runs, so surfacing every problem at once beats failing on the first
// (see specs/configuration.md, "validation is a safety feature"). Problems is
// sorted for deterministic output.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d validation problem(s):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// knownPreconditions are the precondition identifiers the harness currently
// understands. The condition-expression language is still OPEN (shell exit-code vs.
// CEL — see specs/configuration.md), so until it is fixed the validator gates
// against this registry: an unrecognized guard is a typo that would otherwise pass
// silently and never hold at runtime. Extend these sets as conditions are added.
var knownPreconditions = map[string]bool{
	"blockers-closed": true,
}

// reservedPostconditions are postcondition identifiers not backed by a configured
// command: the red→green and tests-red proofs (T2.3/T2.4), which the gate realizes as
// built-in check kinds, and human-approved (T2.10), which the ORCHESTRATOR evaluates
// against beads state rather than running in the sandbox at all. Command-check
// postconditions (tests-pass, gosec, …) are deliberately NOT listed here — they are
// defined in harness.yaml's `checks` map and validated against it, so config is the single
// source of truth for what command each one runs (see specs/configuration.md).
var reservedPostconditions = map[string]bool{
	core.PostconditionRedGreen:      true,
	core.PostconditionTestsRed:      true,
	core.PostconditionHumanApproved: true,
}

// reusesAcceptanceTests are the reserved proofs that have no command of their own and
// instead run the acceptance-test command (core.CheckAcceptanceTests) against one or
// more refs. A stage that declares one must register that command, or the gate cannot
// resolve it at run time — caught here at startup rather than mid-run.
var reusesAcceptanceTests = map[string]bool{
	core.PostconditionRedGreen: true,
	core.PostconditionTestsRed: true,
}

// knownMetrics are the metrics that may appear on the left of a comparison
// postcondition such as "mutation>=0.8". An unrecognized metric is a typo that would
// otherwise resolve to no gate check, so it is rejected at startup; the spelling is shared
// with core (and the gate) so config and the gate agree on what a metric is.
var knownMetrics = map[string]bool{
	core.MetricMutation: true,
}

// Validate is the startup gate: it checks the loaded configuration for the
// cross-file and structural problems the loaders deliberately do not (they only
// parse). It treats config validation as a gate on startup, not a nicety — an
// unreachable DAG role, an undefined produces/on_failure target, a missing persona
// file, or a self-looping depth edge is a loud error rather than a surprise mid-run
// (see specs/configuration.md). It returns a *ValidationError listing every problem
// found, or nil if the configuration is sound.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Harness == nil {
		add("harness configuration is missing")
	} else {
		c.validateDAG(add)
		c.validatePolicy(add)
	}
	c.validateSouls(add)
	if c.Infra == nil {
		add("infra configuration is missing")
	} else {
		c.validateModels(add)
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return &ValidationError{Problems: problems}
}

// validateDAG checks the workflow graph in isolation: stage shape, that
// produces/on_failure targets and guards resolve, and that the depth (produces)
// graph is acyclic with no orphaned components.
func (c *Config) validateDAG(add func(string, ...any)) {
	dag := c.Harness.DAG
	if len(dag) == 0 {
		add("dag is empty")
		return
	}

	// The spec-slice depth is a link-hop count, so a negative value is meaningless; 0 (a
	// minimal one-file slice) and up are valid (see Harness.SpecDepth, internal/spec).
	if c.Harness.SpecDepth < 0 {
		add("spec_depth is %d; it must be >= 0 (link hops from the referenced spec file)", c.Harness.SpecDepth)
	}

	for _, name := range sortedKeys(dag) {
		st := dag[name]
		hasRole := st.Role != ""
		switch st.Kind {
		case "":
			if !hasRole {
				add("stage %q sets neither role nor kind; it is neither an agent nor a non-agent stage", name)
			}
		case StageKindPlan:
			// A plan stage is an agent stage (it dispatches to a planner soul, so it must
			// name a role) but is NOT sandbox-gated: its output is the child issues it
			// proposes, validated structurally by the orchestrator, so it must declare no
			// postcondition (see specs/workflow.md). It is the one kind that coexists with a role.
			if !hasRole {
				add("stage %q has kind %q but no role; a plan stage dispatches to a planner soul", name, st.Kind)
			}
			if len(st.Postcondition) > 0 {
				add("stage %q has kind %q and a postcondition; a plan stage is not sandbox-gated and must declare none", name, st.Kind)
			}
		case StageKindResolve:
			// A resolve stage handles merge-conflict resolution. Like plan it is an agent
			// stage (it dispatches to a merge-resolver soul, so it must name a role), but
			// UNLIKE plan it IS sandbox-gated: the agent rebases the conflicting candidate
			// onto main and the orchestrator re-verifies the resolved tree in a clean sandbox
			// before it can land (producer != verifier). So it must declare a postcondition —
			// the suite that re-verifies the rebased result, the two-green-branches guard — or
			// the gate has nothing to grade. It is spawned by the orchestrator on a rebase
			// conflict, not reached through a produces edge (see specs/integration.md).
			if !hasRole {
				add("stage %q has kind %q but no role; a resolve stage dispatches to a merge-resolver soul", name, st.Kind)
			}
			if len(st.Postcondition) == 0 {
				add("stage %q has kind %q but no postcondition; a resolve stage is gated and must declare the suite that re-verifies the resolved result", name, st.Kind)
			}
		case StageKindHuman:
			if hasRole {
				add("stage %q sets both role %q and kind %q; a stage is one or the other", name, st.Role, st.Kind)
			}
			if len(st.Postcondition) > 0 {
				add("stage %q has kind %q and a postcondition; a human stage runs no gate and must declare none", name, st.Kind)
			}
		case StageKindTrustedMerge:
			if hasRole {
				add("stage %q sets both role %q and kind %q; a stage is one or the other", name, st.Role, st.Kind)
			}
			// A trusted-merge stage is the orchestrator's own inline merge — it runs no
			// verification sandbox, so the only postcondition it can carry is human-approved,
			// which the orchestrator evaluates against beads state. A command/proof/metric
			// check here would have no gate to run it, so reject one rather than let it
			// silently never run (see specs/configuration.md, PostconditionHumanApproved).
			for _, pc := range st.Postcondition {
				if pc != core.PostconditionHumanApproved {
					add("stage %q has kind %q with postcondition %q; a trusted-merge stage runs no gate, so only %q is allowed", name, st.Kind, pc, core.PostconditionHumanApproved)
				}
			}
		default:
			add("stage %q has unknown kind %q (want %q, %q, %q or %q)", name, st.Kind, StageKindHuman, StageKindTrustedMerge, StageKindPlan, StageKindResolve)
		}

		for _, target := range st.Produces {
			if _, ok := dag[target]; !ok {
				add("stage %q produces undefined stage %q", name, target)
			}
		}
		if st.OnFailure != "" {
			if _, ok := dag[st.OnFailure]; !ok {
				add("stage %q on_failure target %q is undefined", name, st.OnFailure)
			}
		}

		if st.Precondition != "" && !knownCondition(st.Precondition, knownPreconditions) {
			add("stage %q precondition %q is not a known condition", name, st.Precondition)
		}
		for _, pc := range st.Postcondition {
			if !c.knownPostcondition(pc) {
				add("stage %q postcondition %q is not a known condition (no command in checks:, not a known metric or reserved proof)", name, pc)
				continue
			}
			// human-approved is orchestrator-evaluated and meaningful only on the integrate
			// (trusted-merge) stage, where a produced candidate exists to approve. On an agent
			// stage there is no merge to gate and the gate would try to resolve it as a command
			// check and fail; reject it here so the misplacement is caught at startup.
			if pc == core.PostconditionHumanApproved && st.Kind != StageKindTrustedMerge {
				add("stage %q declares %q but is not a trusted-merge stage; human approval gates integrate only", name, pc)
			}
			// The red→green and tests-red proofs have no command of their own; they run
			// the acceptance-test command against the base and/or the candidate. If a stage
			// declares one, that command must be registered, or the gate cannot resolve it
			// at run time — catch the gap here at startup rather than mid-run (see
			// specs/verification.md).
			if reusesAcceptanceTests[pc] && strings.TrimSpace(c.Harness.Checks[core.CheckAcceptanceTests]) == "" {
				add("stage %q declares the %q proof but no %q command is registered in checks:", name, pc, core.CheckAcceptanceTests)
			}
			// A metric postcondition (e.g. "mutation>=0.8") binds to the measurement command
			// registered under its metric name; the gate runs that command and grades the score
			// it prints against the threshold. A missing command is unresolvable at the gate, so
			// catch it here at startup the way the reused-acceptance-command gap is caught. Only
			// reached for a known postcondition, so an unknown metric is reported above, not here.
			if metric, _, _, ok := core.ParseMetricComparison(pc); ok {
				if strings.TrimSpace(c.Harness.Checks[metric]) == "" {
					add("stage %q declares the %q metric postcondition but no %q command is registered in checks:", name, pc, metric)
				}
			}
		}
	}

	// Every registered check must carry a command; an empty one would exit non-zero
	// (or, worse, vacuously pass) and silently fail every candidate at the gate.
	for cname, cmd := range c.Harness.Checks {
		if strings.TrimSpace(cmd) == "" {
			add("check %q has an empty command", cname)
		}
	}

	c.validateDepthGraph(add)
}

// validateDepthGraph checks the produces edges form a reachable acyclic definition.
// Depth is declarative and must be a DAG — only the role flow may cycle (via
// on_failure, where each retry is a new issue); a produces cycle would mean a stage
// is its own ancestor, which can never terminate (see specs/workflow.md). Edges to
// undefined stages are skipped here (already reported by validateDAG).
func (c *Config) validateDepthGraph(add func(string, ...any)) {
	dag := c.Harness.DAG

	if cycle := findProducesCycle(dag); cycle != nil {
		add("dag produces edges form a cycle: %s", strings.Join(cycle, " -> "))
	}

	// Reachability: roots are stages nothing produces. In a sound (acyclic)
	// definition every stage is reachable from a root; a stage reachable only
	// through a cycle is unreachable and reported here alongside the cycle.
	indeg := make(map[string]int, len(dag))
	for name := range dag {
		indeg[name] = 0
	}
	for _, st := range dag {
		for _, target := range st.Produces {
			if _, ok := dag[target]; ok {
				indeg[target]++
			}
		}
	}

	visited := make(map[string]bool, len(dag))
	var queue []string
	for _, name := range sortedKeys(dag) {
		if indeg[name] == 0 {
			queue = append(queue, name)
			visited[name] = true
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, target := range dag[name].Produces {
			if _, ok := dag[target]; ok && !visited[target] {
				visited[target] = true
				queue = append(queue, target)
			}
		}
	}
	for _, name := range sortedKeys(dag) {
		if !visited[name] {
			add("stage %q is unreachable through produces edges", name)
		}
	}
}

// validatePolicy checks the autonomy profile and its TCB-boundary globs (T2.10). An
// unrecognized profile would silently fall through to autonomous semantics, and a malformed
// TCB glob would silently never match (so a TCB diff would skip the approval gate it must
// hit) — both are config faults caught here at startup, not surprises mid-run. Under
// trusted-dev every integrate must carry the human-approved gate, or "a human reviews every
// diff" is not actually enforced; that cross-check between policy and the DAG lives here
// because it needs both (see specs/configuration.md, specs/bootstrap.md).
func (c *Config) validatePolicy(add func(string, ...any)) {
	p := c.Harness.Policy
	switch p.Profile {
	case "", ProfileAutonomous, ProfileTrustedDev:
		// "" defaults to autonomous (the permissive profile); both named profiles are valid.
	default:
		add("policy.profile %q is unknown (want %q or %q)", p.Profile, ProfileTrustedDev, ProfileAutonomous)
	}
	for _, pat := range p.TCBPaths {
		if strings.TrimSpace(pat) == "" {
			add("policy.tcb_paths contains an empty glob")
			continue
		}
		if !validateGlob(pat) {
			add("policy.tcb_paths glob %q is malformed", pat)
		}
	}

	if p.Profile == ProfileTrustedDev {
		for _, name := range sortedKeys(c.Harness.DAG) {
			st := c.Harness.DAG[name]
			if st.Kind != StageKindTrustedMerge {
				continue
			}
			if !hasHumanApproved(st.Postcondition) {
				add("policy.profile is %q but trusted-merge stage %q has no %q postcondition; trusted-dev requires human approval on every integrate", ProfileTrustedDev, name, core.PostconditionHumanApproved)
			}
		}
	}
}

// hasHumanApproved reports whether a stage's postconditions include the human-approved gate.
func hasHumanApproved(postconditions []string) bool {
	for _, pc := range postconditions {
		if pc == core.PostconditionHumanApproved {
			return true
		}
	}
	return false
}

// validateSouls checks the role<->soul binding both ways and per-soul
// well-formedness: every agent-stage role is fulfilled by at least one soul, every
// soul fulfills a role some stage uses, names are unique, selectors are well-formed,
// and persona files exist on disk.
func (c *Config) validateSouls(add func(string, ...any)) {
	rolesUsed := map[string][]string{} // role -> stage names that reference it
	if c.Harness != nil {
		for _, name := range sortedKeys(c.Harness.DAG) {
			if role := c.Harness.DAG[name].Role; role != "" {
				rolesUsed[role] = append(rolesUsed[role], name)
			}
		}
	}

	soulsByRole := map[string]int{}
	seenName := map[string]bool{}
	// selectorOwner maps role -> canonical selector -> the first soul that declared it, so
	// two souls fulfilling one role with identical selectors are rejected: selection would
	// pick the same one every time (Name tie-break) and the other could never be reached,
	// which is a config fault, not a valid specialization (see orchestrator.selectSoul).
	selectorOwner := map[string]map[string]string{}
	for _, s := range c.Souls {
		switch {
		case s.Name == "":
			add("a soul has an empty name")
		case seenName[s.Name]:
			add("soul name %q is defined more than once", s.Name)
		default:
			seenName[s.Name] = true
		}

		if s.Role == "" {
			add("soul %q has an empty role", s.Name)
		} else {
			soulsByRole[s.Role]++
			if _, used := rolesUsed[s.Role]; !used && c.Harness != nil {
				add("soul %q declares role %q which no dag stage uses", s.Name, s.Role)
			}
			sel := canonicalSelector(s.Selector)
			if selectorOwner[s.Role] == nil {
				selectorOwner[s.Role] = map[string]string{}
			}
			if other, dup := selectorOwner[s.Role][sel]; dup {
				add("souls %q and %q both fulfill role %q with the same selector — one can never be selected", other, s.Name, s.Role)
			} else {
				selectorOwner[s.Role][sel] = s.Name
			}
		}

		for k, v := range s.Selector {
			if k == "" {
				add("soul %q has a selector with an empty key", s.Name)
			} else if v == "" {
				add("soul %q selector key %q has an empty value", s.Name, k)
			}
		}

		c.validatePersona(s, add)
	}

	for _, role := range sortedRoleKeys(rolesUsed) {
		if soulsByRole[role] == 0 {
			add("dag role %q (stage(s) %s) resolves to no soul", role, strings.Join(rolesUsed[role], ", "))
		}
	}
}

// canonicalSelector renders a soul selector as a stable string (sorted `k=v` pairs) so
// two selectors can be compared for equality regardless of map iteration order. The empty
// selector canonicalizes to "" — the catch-all default; two default souls for one role
// therefore collide, which is the intended rejection.
func canonicalSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + sel[k]
	}
	return strings.Join(parts, ",")
}

// validatePersona checks the soul's persona markdown exists on disk, resolving a
// relative path against the config root. The persona is the agent's entire
// behavior; a missing file would boot an agent with no identity.
func (c *Config) validatePersona(s core.Soul, add func(string, ...any)) {
	if s.Persona == "" {
		add("soul %q has no persona path", s.Name)
		return
	}
	path := c.PersonaPath(s)
	if _, err := os.Stat(path); err != nil {
		add("soul %q persona file %q does not exist", s.Name, path)
	}
}

// validateModels checks every soul's declared model resolves in the infra registry
// and that each registry entry is well-formed. The runner resolves soul.Model to a
// provider adapter at call time (see specs/models.md); an unregistered model would
// fail at first dispatch, so it is caught here instead.
func (c *Config) validateModels(add func(string, ...any)) {
	for name, mp := range c.Infra.Models {
		switch mp.Provider {
		case "":
			add("model %q has no provider", name)
		case ProviderAnthropic, ProviderOpenAI:
			// well-formed; no endpoint needed
		case ProviderOpenAICompat:
			if mp.Endpoint == "" {
				add("model %q uses provider %s but has no endpoint", name, ProviderOpenAICompat)
			}
		default:
			add("model %q has unknown provider %q (want one of %s, %s, %s)",
				name, mp.Provider, ProviderAnthropic, ProviderOpenAI, ProviderOpenAICompat)
		}
	}
	for _, s := range c.Souls {
		if s.Model == "" {
			add("soul %q has no model", s.Name)
			continue
		}
		if _, ok := c.Infra.Models[s.Model]; !ok {
			add("soul %q references model %q which the infra model registry does not define", s.Name, s.Model)
		}
	}
}

// knownCondition reports whether s is a recognized guard: either a bare identifier
// in the given set, or a comparison of a known metric against a numeric threshold
// (e.g. "mutation>=0.8").
func knownCondition(s string, bare map[string]bool) bool {
	return bare[s] || isMetricComparison(s)
}

// knownPostcondition reports whether a postcondition reference resolves to something
// the harness can evaluate: a reserved built-in proof, a command check defined in the
// `checks` registry, or a comparison against a known metric. This is the
// configuration-time half of bridging declared postconditions to gate checks; the
// gate resolves the command checks against the same registry at run time.
func (c *Config) knownPostcondition(pc string) bool {
	if reservedPostconditions[pc] {
		return true
	}
	if c.Harness != nil {
		if _, ok := c.Harness.Checks[pc]; ok {
			return true
		}
	}
	return isMetricComparison(pc)
}

// isMetricComparison reports whether s is a comparison of a known metric against a
// numeric threshold, e.g. "mutation>=0.8". The parse (what a comparison looks like) is
// shared with the gate via core; this layer adds the policy that the metric must be one
// the harness knows how to gate on.
func isMetricComparison(s string) bool {
	metric, _, _, ok := core.ParseMetricComparison(s)
	return ok && knownMetrics[metric]
}

// findProducesCycle returns the first produces cycle as a path of stage names
// (ending with the repeated node), or nil if the depth graph is acyclic. Edges to
// undefined stages are ignored.
func findProducesCycle(dag map[string]Stage) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(dag))
	var stack []string

	var visit func(name string) []string
	visit = func(name string) []string {
		color[name] = gray
		stack = append(stack, name)
		for _, target := range dag[name].Produces {
			if _, ok := dag[target]; !ok {
				continue
			}
			switch color[target] {
			case gray:
				// Found a back edge; slice the stack from the target to close the cycle.
				for i, n := range stack {
					if n == target {
						return append(append([]string{}, stack[i:]...), target)
					}
				}
			case white:
				if cycle := visit(target); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	for _, name := range sortedKeys(dag) {
		if color[name] == white {
			if cycle := visit(name); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]Stage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedRoleKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
