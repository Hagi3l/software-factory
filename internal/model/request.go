package model

// Request is the canonical, provider-agnostic input to one model call. The agent
// builds it from the Brief and the conversation so far and sends it to its runner
// over the broker; the runner selects the provider adapter and API key for the
// invocation's soul, translates the Request to that provider's wire format, calls
// the API, and returns a canonical Response. The agent never names a provider or a
// model and never holds a key — that binding is the soul's, held only by the runner
// (see specs/models.md, specs/components/runner.md).
//
// System is a distinct field rather than a message in Messages because the inner
// loop builds the request as {system, messages, tools} (see specs/components/agent.md)
// and providers diverge on how they carry it — Anthropic as a top-level parameter,
// OpenAI as a leading system message. Keeping it separate gives one unambiguous home
// for the persona and lets each adapter render it into that provider's system channel.
//
// These are the fields every tool-calling model needs. Optional capability fields
// (prompt caching, extended thinking / reasoning effort) are layered on by the
// per-provider adapters as additive fields an adapter honors or ignores (see
// specs/models.md); adding them later is not a schema migration.
type Request struct {
	System    string    // system / persona instructions, rendered to each provider's system channel
	Messages  []Message // conversation history the model continues (user/assistant/tool turns)
	Tools     []ToolDef // tools the model may call this turn
	MaxTokens int       // output token ceiling; 0 means the adapter's default
}
