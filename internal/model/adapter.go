package model

import "context"

// StreamEvent is one incremental piece of model output, delivered as it is generated,
// to drive the live "watch an agent think" view where the runner fans these out to
// NATS → SSE (see specs/observability.md). It carries exactly one of two channels: an
// assistant TextDelta (the visible answer) or a ReasoningDelta (the model's chain of
// thought, from Anthropic thinking blocks or an OpenAI-compatible reasoning field).
// Keeping them distinct lets the feed label "thinking" separately from "saying," and —
// crucially — makes a turn that is *all tool calls* still observable: such a turn emits
// no text, but a reasoning model still narrates its plan on the reasoning channel. A
// struct (not a bare string) so further channels can be added without changing the
// Adapter signature.
type StreamEvent struct {
	TextDelta      string // incremental assistant text; empty for non-text events
	ReasoningDelta string // incremental reasoning/thinking text; empty for non-reasoning events
}

// StreamHandler receives StreamEvents in order as a model call streams. It is invoked
// synchronously from the goroutine draining the provider stream, so it must return
// quickly — a slow handler stalls token consumption. A nil handler means the caller
// does not want live streaming; the adapter still drains the stream and returns the
// assembled Response.
type StreamHandler func(StreamEvent)

// Adapter is the provider boundary: it translates a canonical Request into one model
// vendor's wire format, makes the call, streams incremental output to onEvent, and
// returns the assembled canonical Response. One Adapter is bound to one model; the
// runner resolves soul.Model to an Adapter (the registry, plan T1.10) and holds the
// API key, so the agent stays provider-unaware (see specs/models.md).
//
// Streaming is part of the contract from day one, not a retrofit: every adapter
// consumes the provider's streaming API and reports deltas through onEvent, so the
// live view works uniformly regardless of which provider answered. A returned error
// means the call could not complete; a normal stop (including a tool_use turn) is a
// Response with the corresponding StopReason, not an error.
type Adapter interface {
	Complete(ctx context.Context, req Request, onEvent StreamHandler) (Response, error)
}
