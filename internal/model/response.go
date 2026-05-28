package model

// StopReason is the normalized reason a model stopped generating, mapped from each
// provider's own finish reason so the agent loop and budget enforcement can branch
// uniformly: tool_use drives another loop iteration, end_turn signals completion,
// and max_tokens is a budget/truncation signal (see specs/models.md, specs/workflow.md).
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"       // model finished its turn naturally
	StopToolUse       StopReason = "tool_use"       // model wants to call tools; loop continues
	StopMaxTokens     StopReason = "max_tokens"     // hit the output token cap
	StopStopSequence  StopReason = "stop_sequence"  // hit a configured stop sequence
	StopContentFilter StopReason = "content_filter" // provider filtered the output
)

// Usage is normalized token accounting for one model call. Providers report tokens
// differently, so normalizing here lets the runner enforce budgets uniformly and
// account for prompt caching — a large cost saver on the long, repetitive prompts
// of an agent loop (see specs/models.md, specs/workflow.md).
type Usage struct {
	InputTokens         int // prompt tokens billed at full rate
	OutputTokens        int // tokens generated
	CacheCreationTokens int // input tokens written to the prompt cache
	CacheReadTokens     int // input tokens served from the prompt cache
}

// Response is the canonical result of one model call: the assembled final turn the
// agent loop acts on. Streaming is layered on top in the adapter interface (plan
// T1.8); this struct is what remains once a turn is complete.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	Stop      StopReason
}
