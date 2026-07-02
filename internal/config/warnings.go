package config

import (
	"fmt"
	"sort"

	"github.com/Loxstomper/harness/internal/core"
)

// Warnings returns non-fatal advisories about the configuration: properties that are
// weaker than recommended but not invalid, so a config that trips one is still sound
// (Validate returns nil) and harness validate surfaces the advisory without failing
// (exit 0). The split from Validate is deliberate — Validate gates startup on faults
// that would break at run time (an unreachable role, an undefined target), whereas a
// warning is the operator's call to heed or ignore. Model assignment is exactly such a
// call: config is the pipeline, so the harness *recommends* N-version producer/verifier
// diversity but never forces it (see specs/verification.md, "Model diversity is
// configured, not mandated"). The list is sorted and de-duplicated for deterministic
// output; nil when there is nothing to advise.
func (c *Config) Warnings() []string {
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	c.warnModelDiversity(warn)
	c.warnPackageProxy(warn)
	c.warnGitPush(warn)
	c.warnEffortNoOp(warn)

	if len(warnings) == 0 {
		return nil
	}
	sort.Strings(warnings)
	// De-duplicate: two producer stages reaching the same gate would otherwise emit the
	// identical advisory twice. The slice is already sorted, so equal strings are adjacent.
	uniq := warnings[:1]
	for _, w := range warnings[1:] {
		if w != uniq[len(uniq)-1] {
			uniq = append(uniq, w)
		}
	}
	return uniq
}

// warnPackageProxy advises when broker.package_proxy is configured but the package-proxy
// egress destination is not in the allowlist: the proxy URL is then dead config — the
// broker denies every package fetch deny-by-default, so a sandbox that needs a new
// dependency fails even though a proxy was named. It is advisory, not fatal: a deployment
// might intentionally keep package fetch off (relying solely on the baked module cache)
// while leaving the proxy URL in place for a later flip. See specs/security.md Control 2.
func (c *Config) warnPackageProxy(warn func(string, ...any)) {
	if c.Infra == nil {
		return
	}
	b := c.Infra.Broker
	if b.PackageProxy != "" && !b.PackageProxyAllowed() {
		warn("broker.package_proxy is set to %q but %q is not in broker.allowlist, so package fetches are denied (deny-by-default); add %q to the allowlist to enable dependency fetching, or drop package_proxy",
			b.PackageProxy, DestPackageProxy, DestPackageProxy)
	}
}

// warnGitPush advises when git.remote is configured but the git egress destination is not
// in the allowlist: the remote is then dead config — the broker denies the candidate-branch
// push deny-by-default, so every invocation's submit fails even though a remote was named.
// Advisory, not fatal, mirroring warnPackageProxy: a deployment might stage a remote ahead
// of flipping git egress on. See specs/security.md Control 3.
func (c *Config) warnGitPush(warn func(string, ...any)) {
	if c.Infra == nil {
		return
	}
	if c.Infra.Git.Remote != "" && !c.Infra.Broker.GitPushAllowed() {
		warn("git.remote is set to %q but %q is not in broker.allowlist, so the candidate-branch push is denied (deny-by-default); add %q to the allowlist to enable pushing, or drop git.remote",
			c.Infra.Git.Remote, DestGitPush, DestGitPush)
	}
}

// warnEffortNoOp advises when an openai-compat model in the Anthropic family carries
// effort via effort_param: reasoning. It is a valid config (Validate passes), but on Claude
// 4.6+/5 over OpenRouter reasoning.effort is a silent no-op — the level only lands via the
// top-level verbosity field — so a "reasoning" transport there quietly does nothing. This is
// the one gap the explicit-effort_param rule can't close (a wrong explicit value, not a
// missing one), so it is surfaced as an advisory. The family is inferred from the slug the
// same way as the diversity check (ModelFamily); the heuristic drives only this warning,
// never the routing, so an over-eager flag on a genuine pre-4.6 Claude model is harmless to
// ignore. See specs/models.md "Optional capability fields".
func (c *Config) warnEffortNoOp(warn func(string, ...any)) {
	if c.Infra == nil {
		return
	}
	for name, mp := range c.Infra.Models {
		if mp.Effort == "" || mp.EffortParam != EffortParamReasoning {
			continue
		}
		if mp.ModelFamily(name) == ProviderAnthropic {
			warn("model %q sets effort_param: %s but is an Anthropic model; Claude 4.6+/5 ignore reasoning.effort (a silent no-op) and take the effort level via the top-level verbosity field — set effort_param: %s (harmless to ignore if this is a pre-4.6 Claude model)",
				name, EffortParamReasoning, EffortParamVerbosity)
		}
	}
}

// warnModelDiversity advises when a verifier role shares a model provider — the family
// proxy — with the producer whose work it grades. The producer is the stage gated by the
// red→green proof (the implementor must turn the independently-authored acceptance tests
// from red to green); its verifiers are the gate stages downstream of it in the produces
// DAG (the qa gate re-runs the tests, mutation, and scanners independently). When the two
// resolve to the same provider they share correlated blind spots, which weakens the
// N-version independence verification.md recommends — but model choice is the operator's,
// so this is advisory, never fatal (it is why the warning channel exists, T2.13).
//
// Family is the org that trained the weights (the correlated-blind-spot unit), derived per
// model by ModelFamily: an explicit `family:`, else the vendor prefix of a "vendor/model"
// aggregator slug ("deepseek/deepseek-v4-pro" → "deepseek"), else the provider tag. This is
// what makes the check accurate behind a gateway like OpenRouter, where every model shares
// one openai-compat provider yet the slug still names distinct vendors — so a deepseek
// producer and an anthropic verifier (both openai-compat) correctly read as different
// families. The only residual blind spot is a *bare-slug* openai-compat model (an Ollama
// endpoint serving several models under non-vendored ids), which falls back to the provider
// tag; the message says so and points at `family:` to disambiguate.
//
// Verifiers are scoped to the producer's produces-descendants, not "every gate stage", so
// (a) a non-gate stage inserted between implement and qa does not hide the overlap, and
// (b) the conflict-spawned resolve stage — gated, but not produced by implement and not
// an independent reviewer of the implementor — is correctly not treated as its verifier.
func (c *Config) warnModelDiversity(warn func(string, ...any)) {
	if c.Harness == nil || c.Infra == nil {
		return
	}
	dag := c.Harness.DAG
	for _, pname := range sortedKeys(dag) {
		pst := dag[pname]
		if !c.isProducerStage(pst) {
			continue
		}
		producerFamilies := c.roleFamilies(pst.Role)
		if len(producerFamilies) == 0 {
			continue
		}
		for _, vname := range sortedSet(c.downstreamStages(pname)) {
			vst := dag[vname]
			if !c.isGateStage(vst) || vst.Role == pst.Role {
				continue
			}
			verifierFamilies := c.roleFamilies(vst.Role)
			for _, fam := range sharedKeys(producerFamilies, verifierFamilies) {
				note := ""
				if fam == ProviderOpenAICompat {
					note = " (these are bare-slug openai-compat models whose vendor can't be inferred from the slug, so they fall back to the provider tag and read as one family — set `family:` on the registry entry to declare the real vendor)"
				}
				warn("producer role %q and verifier role %q both resolve to model family %q; a same-family producer and verifier share correlated blind spots, weakening the N-version independent verification specs/verification.md recommends — point the verifier at a different model family%s",
					pst.Role, vst.Role, fam, note)
			}
		}
	}
}

// isProducerStage reports whether a stage is a producer in the N-version sense: an agent
// stage gated by the red→green proof, i.e. the implementor that must turn the
// independently-authored acceptance tests green. The proof postcondition is the stable,
// principled signal for "the implement stage" (verification.md flips implement to it).
func (c *Config) isProducerStage(st Stage) bool {
	if st.Role == "" {
		return false
	}
	for _, pc := range st.Postcondition {
		if pc == core.PostconditionRedGreen {
			return true
		}
	}
	return false
}

// isGateStage reports whether a stage runs a verification gate: an agent stage with a
// postcondition that grades the candidate in the clean sandbox via a command check or a
// metric comparison (the qa/resolve stages). The reserved proofs (tests-red, red→green)
// and the orchestrator-evaluated human-approved are not such gates.
func (c *Config) isGateStage(st Stage) bool {
	if st.Role == "" {
		return false
	}
	for _, pc := range st.Postcondition {
		if c.runsGateCheck(pc) {
			return true
		}
	}
	return false
}

// runsGateCheck reports whether a postcondition is a candidate-grading gate check — a
// command check defined in the checks registry or a metric comparison — as opposed to a
// reserved proof or the orchestrator-evaluated human-approved gate. It is the predicate
// that separates the independent verifier gate (qa) from the proofs that grade the tests
// themselves (tests-red, red→green).
func (c *Config) runsGateCheck(pc string) bool {
	if reservedPostconditions[pc] {
		return false
	}
	if c.Harness != nil {
		if _, ok := c.Harness.Checks[pc]; ok {
			return true
		}
	}
	return isMetricComparison(pc)
}

// roleFamilies is the set of model families the souls fulfilling a role resolve to, via
// core.Soul.Model → the infra model registry → ModelProvider.ModelFamily (the trainer org,
// not the gateway provider — see warnModelDiversity). A role may map to several souls
// (selector-based), so it is a set; a soul whose model is unregistered (already a Validate
// error) or whose entry yields no family (no provider and no inferable vendor) is skipped.
func (c *Config) roleFamilies(role string) map[string]bool {
	fams := map[string]bool{}
	for _, s := range c.Souls {
		if s.Role != role {
			continue
		}
		if mp, ok := c.Infra.Models[s.Model]; ok {
			if fam := mp.ModelFamily(s.Model); fam != "" {
				fams[fam] = true
			}
		}
	}
	return fams
}

// downstreamStages returns the set of stage names reachable from start by following
// produces edges (excluding start itself). Edges to undefined stages are ignored (Validate
// reports those); the visited set makes it safe even if the produces graph has a cycle
// (also a Validate error, but this must not hang on a not-yet-rejected config).
func (c *Config) downstreamStages(start string) map[string]bool {
	dag := c.Harness.DAG
	seen := map[string]bool{}
	queue := append([]string(nil), dag[start].Produces...)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if _, ok := dag[n]; !ok || seen[n] {
			continue
		}
		seen[n] = true
		queue = append(queue, dag[n].Produces...)
	}
	return seen
}

// sharedKeys returns the sorted intersection of two string sets.
func sharedKeys(a, b map[string]bool) []string {
	var shared []string
	for p := range a {
		if b[p] {
			shared = append(shared, p)
		}
	}
	sort.Strings(shared)
	return shared
}

// sortedSet returns the keys of a string set in sorted order.
func sortedSet(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
