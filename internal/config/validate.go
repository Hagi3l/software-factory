package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
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

// reservedPostconditions are postcondition identifiers backed by a built-in gate
// check kind rather than a configured command: the red→green proof (T2.3) and any
// later special verifications. Command-check postconditions (tests-pass, gosec, …)
// are deliberately NOT listed here — they are defined in harness.yaml's `checks` map
// and validated against it, so config is the single source of truth for what command
// each one runs (see specs/configuration.md).
var reservedPostconditions = map[string]bool{
	core.PostconditionRedGreen: true,
}

// knownMetrics are the metrics that may appear on the left of a comparison
// postcondition such as "mutation>=0.8".
var knownMetrics = map[string]bool{
	"mutation": true,
}

// comparisonOps are recognized in longest-first order so ">=" is matched before ">".
var comparisonOps = []string{">=", "<=", "==", ">", "<"}

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

	for _, name := range sortedKeys(dag) {
		st := dag[name]
		agent := st.Role != ""
		nonAgent := st.Kind != ""
		switch {
		case agent && nonAgent:
			add("stage %q sets both role %q and kind %q; a stage is one or the other", name, st.Role, st.Kind)
		case !agent && !nonAgent:
			add("stage %q sets neither role nor kind; it is neither an agent nor a non-agent stage", name)
		case nonAgent && st.Kind != StageKindHuman && st.Kind != StageKindTrustedMerge:
			add("stage %q has unknown kind %q (want %q or %q)", name, st.Kind, StageKindHuman, StageKindTrustedMerge)
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
			// The red→green proof has no command of its own; it runs the acceptance-test
			// command against the base and the candidate. If a stage declares it, that
			// command must be registered, or the gate cannot resolve it at run time —
			// catch the gap here at startup rather than mid-run (see specs/verification.md).
			if pc == core.PostconditionRedGreen && strings.TrimSpace(c.Harness.Checks[core.CheckAcceptanceTests]) == "" {
				add("stage %q declares the %q proof but no %q command is registered in checks:", name, core.PostconditionRedGreen, core.CheckAcceptanceTests)
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
// numeric threshold, e.g. "mutation>=0.8".
func isMetricComparison(s string) bool {
	for _, op := range comparisonOps {
		if i := strings.Index(s, op); i > 0 {
			metric := strings.TrimSpace(s[:i])
			threshold := strings.TrimSpace(s[i+len(op):])
			if !knownMetrics[metric] {
				return false
			}
			_, err := strconv.ParseFloat(threshold, 64)
			return err == nil
		}
	}
	return false
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
