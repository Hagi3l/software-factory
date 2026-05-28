package core

// Soul is an agent's identity: declarative, stateless config reconstituted fresh
// on every invocation. It carries no cross-task memory — all durable state lives
// in beads, git, and the specs (see specs/components/agent.md).
//
// Souls fulfil Roles; a Role may map to several souls, chosen per issue by a
// config selector. The fields mirror the soul declaration in
// specs/configuration.md, so that loading a soul YAML populates this struct
// directly.
type Soul struct {
	Name    string   // unique soul name, e.g. "implementor-go"
	Role    string   // the DAG stage it serves, e.g. "implement"
	Model   string   // model identifier; the runner resolves it to a provider adapter
	Persona string   // path to the markdown prompt file that defines its behaviour
	Tools   []string // enabled capability names
	Sandbox string   // sandbox profile name
}
