package core

// Soul is an agent's identity: declarative, stateless config reconstituted fresh
// on every invocation. It carries no cross-task memory — all durable state lives
// in beads, git, and the specs (see specs/components/agent.md).
//
// Souls fulfill Roles; a Role may map to several souls, chosen per issue by
// matching the issue's tags against Selector. The fields mirror the soul
// declaration in specs/configuration.md, so that loading a soul YAML populates
// this struct directly — the yaml tags are the load contract.
type Soul struct {
	Name     string            `yaml:"name"`     // unique soul name, e.g. "implementor-go"
	Role     string            `yaml:"role"`     // the DAG stage it serves, e.g. "implement"
	Model    string            `yaml:"model"`    // model identifier; the runner resolves it to a provider adapter
	Persona  string            `yaml:"persona"`  // path to the markdown prompt file that defines its behavior
	Tools    []string          `yaml:"tools"`    // enabled capability names
	Sandbox  string            `yaml:"sandbox"`  // sandbox profile name
	Selector map[string]string `yaml:"selector"` // tag match that picks this soul for an issue, e.g. {lang: go}
}

// Matches reports whether this soul's Selector is satisfied by an issue's tags: every
// selector key must be present in tags with the same value (a subset test). An empty
// selector matches any issue, so a soul with no selector is a catch-all default for its
// role. This is the predicate the orchestrator uses to pick which soul fulfills a role
// when several do; the most-specific (largest) matching selector wins, so a specialized
// soul is preferred over a default (see specs/configuration.md, orchestrator.selectSoul).
func (s Soul) Matches(tags map[string]string) bool {
	for k, v := range s.Selector {
		if tags[k] != v {
			return false
		}
	}
	return true
}
