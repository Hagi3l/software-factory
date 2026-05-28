package model

// Request is the canonical, provider-agnostic input to one model call. The agent
// builds it from the Brief and the conversation so far and sends it to its runner
// over the broker; the runner selects the provider adapter and API key for the
// invocation's soul, translates the Request to that provider's wire format, calls
// the API, and returns a canonical Response. The agent never names a provider or a
// model and never holds a key — that binding is the soul's, held only by the runner
// (see specs/models.md, specs/components/runner.md).
//
// The system/persona prompt is carried as the leading RoleSystem Message, not a
// separate field: keeping a single representation of conversation history is why
// adapters (not the loop) own the divergence in how each provider expresses a system
// prompt (Anthropic top-level parameter vs. an OpenAI system message — see Role).
//
// These are the fields every tool-calling model needs. Optional capability fields
// (prompt caching, extended thinking / reasoning effort) are layered on by the
// adapter-interface work (plan T1.8): an adapter that supports a capability honors
// the field, one that does not ignores it. Adding such fields later is additive, not
// a schema migration.
type Request struct {
	Messages  []Message // conversation history the model continues, incl. a leading RoleSystem turn
	Tools     []ToolDef // tools the model may call this turn
	MaxTokens int       // output token ceiling; 0 means the adapter/provider default
}
