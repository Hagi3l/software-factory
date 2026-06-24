// Package anthropic is the Anthropic provider adapter: a thin translation layer
// between the harness's canonical model types and the official Anthropic Go SDK. It
// implements model.Adapter. The package is named anthropic and imports the SDK as
// sdk to avoid the name collision; nothing outside this package sees SDK types.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Loxstomper/harness/internal/model"
)

// defaultMaxTokens is used when a Request leaves MaxTokens unset. The Anthropic API
// requires a positive max_tokens, but the canonical Request treats 0 as "the adapter's
// default" (see model.Request) — budget enforcement is the orchestrator/runner's job
// (plan T1.16), not a wire requirement, so the adapter just supplies a sane ceiling.
const defaultMaxTokens = 4096

// Adapter calls one Anthropic model. It is bound to a single model name at
// construction; the runner builds one per soul.Model via the registry (plan T1.10).
type Adapter struct {
	model  string
	client sdk.Client
	effort string // output_config.effort to send on every call; empty = provider default
}

// New builds an Adapter for the given Anthropic model. Request options configure the
// underlying SDK client — the runner passes the API key (held only by the runner,
// never in config) via option.WithAPIKey; with none, the SDK reads ANTHROPIC_API_KEY
// from the environment. Passing options straight through keeps the adapter thin.
func New(modelName string, opts ...option.RequestOption) *Adapter {
	return &Adapter{model: modelName, client: sdk.NewClient(opts...)}
}

// WithEffort sets the reasoning-effort level (output_config.effort) sent on every call.
// A builder rather than a New parameter so existing callers (and the openai adapter's
// shape) stay unchanged; the registry chains it from the model's config. An empty level
// is a no-op, leaving the model at its provider default. Returns the adapter for chaining.
func (a *Adapter) WithEffort(effort string) *Adapter {
	a.effort = effort
	return a
}

// Complete satisfies model.Adapter. It always uses the streaming API (streaming is
// first-class — see specs/models.md), accumulates the streamed events into the final
// message, forwards text deltas to onEvent for the live view, and returns the
// assembled canonical Response.
func (a *Adapter) Complete(ctx context.Context, req model.Request, onEvent model.StreamHandler) (model.Response, error) {
	params, err := a.toParams(req)
	if err != nil {
		return model.Response{}, err
	}

	// effort rides as output_config.effort, injected as a request-body field. It is GA on
	// /v1/messages (no beta header), so it sets cleanly on the non-beta streaming call; the
	// canonical Request stays provider-agnostic (the adapter layers the field on per
	// specs/models.md). Omitted entirely when unset, so default behavior is byte-identical.
	var reqOpts []option.RequestOption
	if a.effort != "" {
		reqOpts = append(reqOpts, option.WithJSONSet("output_config", map[string]any{"effort": a.effort}))
	}

	stream := a.client.Messages.NewStreaming(ctx, params, reqOpts...)
	var acc sdk.Message
	for stream.Next() {
		event := stream.Current()
		if err := acc.Accumulate(event); err != nil {
			return model.Response{}, fmt.Errorf("anthropic: accumulate stream event: %w", err)
		}
		if onEvent == nil {
			continue
		}
		if delta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent); ok {
			switch d := delta.Delta.AsAny().(type) {
			case sdk.TextDelta:
				if d.Text != "" {
					onEvent(model.StreamEvent{TextDelta: d.Text})
				}
			case sdk.ThinkingDelta:
				// Extended-thinking blocks stream on their own delta type; surface them on
				// the reasoning channel so the live feed labels "thinking" distinctly and a
				// tool-only turn still shows the model reasoning (see specs/models.md).
				if d.Thinking != "" {
					onEvent(model.StreamEvent{ReasoningDelta: d.Thinking})
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return model.Response{}, fmt.Errorf("anthropic: stream: %w", err)
	}
	return fromMessage(&acc), nil
}

// toParams translates a canonical Request into SDK request params.
func (a *Adapter) toParams(req model.Request) (sdk.MessageNewParams, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := sdk.MessageNewParams{
		Model:     a.model,
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		mp, err := toMessageParam(m)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		params.Messages = append(params.Messages, mp)
	}
	for _, t := range req.Tools {
		tp, err := toToolParam(t)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, tp)
	}
	applyCaching(&params)
	return params, nil
}

// applyCaching marks the prompt's stable prefix and growing tail cacheable. The Anthropic
// adapter caches by default (specs/models.md "Optional capability fields"): the agent loop
// re-sends a large stable prefix every turn — the persona in System and the Brief (ambient
// specs + spec) in the first message — and grows only at the tail, so without caching each
// turn re-pays full input price for the whole prefix, the single largest cost on the loop.
// Two ephemeral breakpoints capture it:
//   - the first message's first block pins the stable prefix (tools+system+Brief), which is
//     byte-identical every turn and so a cache read after the first turn; and
//   - the last message's last block is the moving breakpoint the provider auto-advances —
//     its prefix is the whole conversation so far, so each turn reads the previous turn's
//     prefix and the ~1.25x cache write bills only the new tail.
//
// A breakpoint below the provider's minimum cacheable size is silently ignored (no error),
// so it is always safe to mark unconditionally. Cache read/write token counts come back
// normalized in Usage via fromMessage.
func applyCaching(params *sdk.MessageNewParams) {
	msgs := params.Messages
	if len(msgs) == 0 {
		// Degenerate (no messages): pin the system prefix on its own if one is present.
		if n := len(params.System); n > 0 {
			params.System[n-1].CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		return
	}
	if first := msgs[0].Content; len(first) > 0 {
		if cc := first[0].GetCacheControl(); cc != nil {
			*cc = sdk.NewCacheControlEphemeralParam()
		}
	}
	if last := msgs[len(msgs)-1].Content; len(last) > 0 {
		if cc := last[len(last)-1].GetCacheControl(); cc != nil {
			*cc = sdk.NewCacheControlEphemeralParam()
		}
	}
}

// toMessageParam maps one canonical Message to one SDK message. Anthropic has no tool
// role: tool results are carried as content blocks inside a user message, which is why
// a RoleTool message becomes a user message of tool_result blocks.
func toMessageParam(m model.Message) (sdk.MessageParam, error) {
	switch m.Role {
	case model.RoleUser:
		var blocks []sdk.ContentBlockParamUnion
		if m.Text != "" {
			blocks = append(blocks, sdk.NewTextBlock(m.Text))
		}
		return sdk.NewUserMessage(blocks...), nil

	case model.RoleAssistant:
		var blocks []sdk.ContentBlockParamUnion
		if m.Text != "" {
			blocks = append(blocks, sdk.NewTextBlock(m.Text))
		}
		for _, tc := range m.ToolCalls {
			// tc.Args is raw JSON; NewToolUseBlock marshals the input, and json.RawMessage
			// marshals to itself, so the model's arguments cross unchanged.
			blocks = append(blocks, sdk.NewToolUseBlock(tc.ID, tc.Args, tc.Name))
		}
		return sdk.NewAssistantMessage(blocks...), nil

	case model.RoleTool:
		var blocks []sdk.ContentBlockParamUnion
		for _, tr := range m.ToolResults {
			blocks = append(blocks, sdk.NewToolResultBlock(tr.ToolCallID, tr.Content, tr.IsError))
		}
		return sdk.NewUserMessage(blocks...), nil

	default:
		return sdk.MessageParam{}, fmt.Errorf("anthropic: unsupported message role %q", m.Role)
	}
}

// toToolParam maps a canonical ToolDef to an SDK tool. The canonical Params is a full
// JSON Schema object; Anthropic's input_schema wants properties/required split out, so
// the schema is decomposed and any remaining top-level keys are preserved verbatim via
// ExtraFields so nothing in the author's schema is silently dropped.
func toToolParam(t model.ToolDef) (sdk.ToolUnionParam, error) {
	schema, err := toInputSchema(t.Params)
	if err != nil {
		return sdk.ToolUnionParam{}, fmt.Errorf("anthropic: tool %q input schema: %w", t.Name, err)
	}
	tool := sdk.ToolParam{Name: t.Name, InputSchema: schema}
	if t.Description != "" {
		tool.Description = sdk.String(t.Description)
	}
	return sdk.ToolUnionParam{OfTool: &tool}, nil
}

func toInputSchema(raw model.JSONSchema) (sdk.ToolInputSchemaParam, error) {
	var schema sdk.ToolInputSchemaParam
	if len(raw) == 0 {
		return schema, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return schema, err
	}
	if props, ok := m["properties"]; ok {
		schema.Properties = props
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	delete(m, "type")
	delete(m, "properties")
	delete(m, "required")
	if len(m) > 0 {
		schema.ExtraFields = m
	}
	return schema, nil
}

// fromMessage assembles a canonical Response from a completed SDK message: text blocks
// concatenate into Text, tool_use blocks become ToolCalls, and usage/stop reason are
// normalized.
func fromMessage(msg *sdk.Message) model.Response {
	var resp model.Response
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			text.WriteString(b.Text)
		case sdk.ToolUseBlock:
			resp.ToolCalls = append(resp.ToolCalls, model.ToolCall{
				ID:   b.ID,
				Name: b.Name,
				Args: b.Input,
			})
		}
	}
	resp.Text = text.String()
	resp.Stop = fromStopReason(msg.StopReason)
	resp.Usage = model.Usage{
		InputTokens:         int(msg.Usage.InputTokens),
		OutputTokens:        int(msg.Usage.OutputTokens),
		CacheCreationTokens: int(msg.Usage.CacheCreationInputTokens),
		CacheReadTokens:     int(msg.Usage.CacheReadInputTokens),
	}
	return resp
}

// fromStopReason normalizes Anthropic's finish reason to the canonical set. A refusal
// (the model declining to answer) maps to the content-filter signal — both mean the
// turn produced no usable content for a non-budget reason. Any future or server-tool
// reason (e.g. pause_turn) passes through as its raw string rather than being lost; it
// simply won't match the values the loop branches on.
func fromStopReason(s sdk.StopReason) model.StopReason {
	switch s {
	case sdk.StopReasonEndTurn:
		return model.StopEndTurn
	case sdk.StopReasonMaxTokens:
		return model.StopMaxTokens
	case sdk.StopReasonStopSequence:
		return model.StopStopSequence
	case sdk.StopReasonToolUse:
		return model.StopToolUse
	case sdk.StopReasonRefusal:
		return model.StopContentFilter
	default:
		return model.StopReason(s)
	}
}

// compile-time check that the adapter satisfies the canonical interface.
var _ model.Adapter = (*Adapter)(nil)
