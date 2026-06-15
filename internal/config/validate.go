package config

import (
	"fmt"
	"net"
	"net/url"
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
	c.validateRequirementsPlanner(add)
	if c.Infra == nil {
		add("infra configuration is missing")
	} else {
		c.validateModels(add)
		c.validateSandbox(add)
		c.validateNATS(add)
		c.validateOTel(add)
		c.validateArtifacts(add)
		c.validateSigning(add)
		c.validateBroker(add)
		c.validateGit(add)
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

// validateRequirementsPlanner checks the optional requirements-planner block (T4.12): if
// present, it must name a model the infra registry defines (the composition root resolves
// it to an adapter, exactly like a soul's model — an unregistered one would fail when the
// wizard first dispatches) and a persona file that exists on disk (the planner's entire
// elicitation behavior, like a soul persona). It fulfills no DAG role, so unlike a soul it
// is NOT cross-checked against the dag — it is the trusted, non-sandboxed requirements
// stage realized as the control-room wizard, not a dispatchable agent stage. Absent, the
// wizard is simply disabled and nothing is checked.
func (c *Config) validateRequirementsPlanner(add func(string, ...any)) {
	if c.Harness == nil || c.Harness.RequirementsPlanner == nil {
		return
	}
	rp := c.Harness.RequirementsPlanner
	if rp.Model == "" {
		add("requirements_planner has no model")
	} else if c.Infra != nil {
		if _, ok := c.Infra.Models[rp.Model]; !ok {
			add("requirements_planner references model %q which the infra model registry does not define", rp.Model)
		}
	}
	if rp.Persona == "" {
		add("requirements_planner has no persona path")
	} else if _, err := os.Stat(c.RequirementsPlannerPersonaPath()); err != nil {
		add("requirements_planner persona file %q does not exist", c.RequirementsPlannerPersonaPath())
	}
	if rp.MaxTokens < 0 {
		add("requirements_planner max_tokens is %d; it must be >= 0", rp.MaxTokens)
	}
	if rp.MaxToolTurns < 0 {
		add("requirements_planner max_tool_turns is %d; it must be >= 0", rp.MaxToolTurns)
	}
	if rp.TurnTimeout < 0 {
		add("requirements_planner turn_timeout is %s; it must be >= 0", rp.TurnTimeout.Duration())
	}
	// Read-only codebase exploration (T4.28): a configured sandbox_profile must resolve to a
	// declared infra sandbox profile, exactly like a soul's sandbox. An unknown profile would
	// otherwise only surface when the wizard first tries to provision its read-only sandbox.
	if rp.SandboxProfile != "" {
		if c.Infra == nil || c.Infra.Sandbox.Profiles == nil {
			add("requirements_planner sandbox_profile %q is set but no infra sandbox profiles are defined", rp.SandboxProfile)
		} else if _, ok := c.Infra.Sandbox.Profiles[rp.SandboxProfile]; !ok {
			add("requirements_planner sandbox_profile %q is not defined in the infra sandbox profiles", rp.SandboxProfile)
		}
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

// validateSandbox checks the sandbox backend is known and that every soul's logical
// sandbox profile resolves to a concrete artifact for that backend. The runner and gate
// resolve soul.Sandbox to an image (docker/gvisor) or rootfs (firecracker) via
// SandboxConfig.Profiles when they build the sandbox spec; an unregistered profile, or
// one missing the active backend's field, would silently degrade to booting the bare
// profile name as an image (see SandboxConfig.ResolveImage), so it is caught here at
// startup the same way a missing model registry entry or check command is — config is
// the single source of truth, and a typo must fail loud, not boot a surprise image.
func (c *Config) validateSandbox(add func(string, ...any)) {
	backend := c.Infra.Sandbox.Backend
	switch backend {
	case "", BackendDocker, BackendGVisor, BackendFirecracker:
		// "" is tolerated (the field is informational for the test-injected backend); the
		// three named backends are valid. ResolveImage treats anything non-firecracker as
		// image-shaped, so the field check below uses the same rule.
	default:
		add("sandbox.backend %q is unknown (want %q, %q, or %q)", backend, BackendDocker, BackendGVisor, BackendFirecracker)
	}

	field, kind := "image", "container image"
	if backend == BackendFirecracker {
		field, kind = "rootfs", "rootfs"
	}
	for _, s := range c.Souls {
		if s.Sandbox == "" {
			add("soul %q has no sandbox profile", s.Name)
			continue
		}
		p, ok := c.Infra.Sandbox.Profiles[s.Sandbox]
		if !ok {
			add("soul %q references sandbox profile %q which sandbox.profiles does not define", s.Name, s.Sandbox)
			continue
		}
		artifact := p.Image
		if backend == BackendFirecracker {
			artifact = p.Rootfs
		}
		if strings.TrimSpace(artifact) == "" {
			add("sandbox profile %q (used by soul %q) has no %q for the %q backend (%s)", s.Sandbox, s.Name, field, backend, kind)
		}
	}
}

// otelEndpointStdout is the one non-address endpoint value config accepts: it selects the
// stdout exporter (offline dev). It mirrors telemetry.EndpointStdout, which owns the
// *behavior* — duplicated as a bare literal here only to avoid pulling the heavy OTel SDK
// (telemetry's transitive deps) into this foundational config package just for one string.
const otelEndpointStdout = "stdout"

// validateNATS checks the messaging endpoint and JetStream knobs the infra overlay
// surfaces (nats.url, nats.jetstream). Like the other infra checks it catches an
// environment-specific typo loud and early rather than as an opaque connect/stream
// failure mid-run. The url selects the deployment shape: empty = the embedded
// in-process server (the bootstrap/dev default, location transparency); set = an
// external cluster the run dials instead (distributed, T5.8). See specs/messaging.md,
// specs/configuration.md.
func (c *Config) validateNATS(add func(string, ...any)) {
	n := c.Infra.NATS
	// When set, every comma-separated endpoint must be a dialable nats URL or host:port.
	if n.URL != "" {
		for _, ep := range strings.Split(n.URL, ",") {
			ep = strings.TrimSpace(ep)
			if !validNATSEndpoint(ep) {
				add("nats.url endpoint %q must be a nats:// URL or host:port (leave nats.url empty for the embedded in-process server)", ep)
			}
		}
	}
	js := n.JetStream
	if js.Replicas < 0 {
		add("nats.jetstream.replicas must be >= 0 (0 or 1 = a single replica)")
	}
	// >1 replica needs an external cluster of at least that size; the embedded in-process
	// server is single-replica, so replicas>1 with no nats.url is a guaranteed boot failure.
	if js.Replicas > 1 && n.URL == "" {
		add("nats.jetstream.replicas %d requires an external cluster (set nats.url); the embedded in-process server is single-replica", js.Replicas)
	}
	if js.MaxAge < 0 {
		add("nats.jetstream.max_age must not be negative")
	}
}

// validNATSEndpoint accepts the two forms a nats endpoint takes: a scheme URL
// (nats://host[:port], or tls://, ws://, wss:// for TLS/websocket transports) carrying a
// non-empty host, or a bare host:port. A port is optional after a scheme (nats defaults
// to 4222), required in the bare form so a stray "host" is not mistaken for an endpoint.
func validNATSEndpoint(ep string) bool {
	if ep == "" {
		return false
	}
	if i := strings.Index(ep, "://"); i >= 0 {
		scheme, rest := ep[:i], ep[i+3:]
		switch scheme {
		case "nats", "tls", "ws", "wss":
		default:
			return false
		}
		if rest == "" {
			return false
		}
		if strings.Contains(rest, ":") {
			host, port, err := net.SplitHostPort(rest)
			return err == nil && host != "" && port != ""
		}
		return true
	}
	host, port, err := net.SplitHostPort(ep)
	return err == nil && host != "" && port != ""
}

// validateOTel checks the telemetry export endpoint is one telemetry.Setup can act on:
// "" (export off), "stdout" (the offline stdout exporter), or a host:port OTLP/gRPC
// collector address. Catching a malformed endpoint here — at the startup validation gate
// — turns a silently-dropped-exports misconfiguration into a loud, actionable error, the
// same contract telemetry.go documents ("config.Validate enforces this set before Setup
// runs"). See specs/observability.md.
func (c *Config) validateOTel(add func(string, ...any)) {
	ep := c.Infra.OTel.Endpoint
	switch ep {
	case "", otelEndpointStdout:
		// "" disables export; "stdout" prints to the process stdout — both need no address.
	default:
		// Anything else must be a well-formed host:port the OTLP/gRPC exporter can dial.
		// net.SplitHostPort is the stdlib grammar (incl. IPv6 brackets) the embedded NATS
		// listener uses too; require both halves non-empty so ":4317" or "host:" is rejected.
		host, port, err := net.SplitHostPort(ep)
		if err != nil || host == "" || port == "" {
			add("otel.endpoint %q must be empty (off), %q, or a host:port OTLP/gRPC collector address",
				ep, otelEndpointStdout)
		}
	}
}

// Artifact store backend identifiers. These mirror artifact.BackendFiles/BackendS3,
// duplicated as bare literals here only because the artifact package imports config
// (so config cannot import it back without a cycle) — the same posture validateOTel
// takes with the "stdout" sentinel. The artifact package's Open is the authoritative
// constructor-time check; this validation catches a misconfigured s3 backend at the
// startup gate (harness validate) before a store is ever built.
const (
	artifactBackendFiles = "files"
	artifactBackendS3    = "s3"
)

// validateArtifacts checks the artifact store backend is known and that an s3 backend
// names a bucket and a reachable endpoint/region. The files backend's path requirement
// is enforced when the store is opened (artifact.Open also resolves a relative path
// against the repo), so it is deliberately not duplicated here — config validation is
// about catching environment-specific typos loud and early, and the s3 knobs are the
// ones that only exist for a distributed deployment. See specs/components/artifact-store.md.
func (c *Config) validateArtifacts(add func(string, ...any)) {
	a := c.Infra.Artifacts
	switch a.Backend {
	case "", artifactBackendFiles:
		// files (and the empty default): path is checked at Open time.
	case artifactBackendS3:
		if strings.TrimSpace(a.Bucket) == "" {
			add("artifacts.bucket is required for the %q backend", artifactBackendS3)
		}
		// minio needs a concrete endpoint to dial; with no explicit endpoint the backend
		// derives the AWS regional one (s3.<region>.amazonaws.com), so a region is required
		// in that case. A MinIO/non-AWS deployment sets endpoint instead.
		if strings.TrimSpace(a.Endpoint) == "" && strings.TrimSpace(a.Region) == "" {
			add("artifacts.endpoint or artifacts.region is required for the %q backend", artifactBackendS3)
		}
	default:
		add("artifacts.backend %q is unknown (want %q or %q)", a.Backend, artifactBackendFiles, artifactBackendS3)
	}
}

// validateSigning checks the provenance-signing block (T5.10, specs/security.md). Only
// the run-time-breaking shape fault is gated here: signing turned on with no key is a
// guaranteed merge failure (every integration commit would try to sign with nothing),
// so it must fail at the startup gate rather than at the first merge. File existence is
// deliberately NOT checked — the private key is a runtime-provisioned secret (the
// API-key posture), and a missing/unreadable key fails loudly when the merger first runs
// git (fail-closed). AllowedSigners is an independent verify-on-read capability with no
// shape constraint of its own.
func (c *Config) validateSigning(add func(string, ...any)) {
	s := c.Infra.Signing
	if s.Enabled && strings.TrimSpace(s.Key) == "" {
		add("signing.enabled is true but signing.key (the SSH private signing key path) is empty")
	}
}

// validateBroker checks the broker egress block (T5.6). Only the run-time-breaking fault
// is gated here: a package_proxy URL that is set but malformed would make every dependency
// fetch fail at the gate/agent with an opaque dial error, so a typo must fail loud at the
// startup gate instead. The allowlist contents are otherwise free-form tokens the broker
// matches by string (deny-by-default), so an unknown one simply never matches — not a
// fault. Whether package_proxy is set without the package-proxy destination allowlisted (so
// the proxy is configured but fetches are still denied) is an advisory, not fatal — see
// Warnings. See specs/components/runner.md, specs/security.md.
func (c *Config) validateBroker(add func(string, ...any)) {
	proxy := c.Infra.Broker.PackageProxy
	if proxy == "" {
		return
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		add("broker.package_proxy %q must be an http(s) URL with a host (e.g. %s)", proxy, defaultPackageProxy)
	}
}

// validateGit checks the git push block (T5.7, specs/security.md Control 3). Only
// run-time-breaking shape faults are gated: a malformed remote URL would fail every push
// with an opaque git error, and a partial GitHub App block can never mint a token, so both
// must fail loud at the startup gate. The App private-key PATH existence is deliberately
// NOT checked — it is a runtime-provisioned secret (the API-key posture, like the signing
// key), mounted possibly only at run time; a missing key fails closed on the first push.
// Whether git is actually allowlisted (so a configured remote can be reached) is an
// advisory, not fatal — see Warnings.
func (c *Config) validateGit(add func(string, ...any)) {
	g := c.Infra.Git
	if g.Remote != "" && !validGitRemote(g.Remote) {
		add("git.remote %q must be an https://, http://, ssh://, git://, or file:// URL, an scp-style user@host:path, or a local path", g.Remote)
	}
	app := g.GitHubApp
	if app.set() && !app.Active() {
		add("git.github_app is partially configured; app_id, installation_id, repository, and private_key (path) are all required to mint scoped push tokens")
	}
	// A configured authority with nowhere to push is dead config: the token would be minted
	// and immediately have no remote to authenticate against.
	if app.Active() && g.Remote == "" {
		add("git.github_app is configured but git.remote is empty; the minted token has no remote to push to")
	}
	if app.APIBase != "" {
		if u, err := url.Parse(app.APIBase); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add("git.github_app.api_base %q must be an http(s) URL with a host (e.g. %s)", app.APIBase, "https://api.github.com")
		}
	}
}

// validGitRemote accepts the git remote forms a push can target: a scheme URL
// (https/http/ssh/git with a host, or file:// where the host may be empty for a local
// path), an scp-style user@host:path, or a bare local filesystem path. It is deliberately
// lenient — git supports many transports — catching only obvious garbage at the gate.
func validGitRemote(remote string) bool {
	if i := strings.Index(remote, "://"); i >= 0 {
		u, err := url.Parse(remote)
		if err != nil {
			return false
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
			return u.Host != ""
		case "file":
			return u.Path != "" || u.Host != ""
		default:
			return false
		}
	}
	// scp-style "user@host:path" (an @ before the first colon, both sides non-empty).
	if at := strings.Index(remote, "@"); at > 0 {
		if colon := strings.Index(remote[at:], ":"); colon > 1 {
			return true
		}
	}
	// A bare absolute local path is a valid git remote (a local clone/bare repo).
	return strings.HasPrefix(remote, "/")
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
