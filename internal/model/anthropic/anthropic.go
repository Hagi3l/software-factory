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
}

// New builds an Adapter for the given Anthropic model. Request options configure the
// underlying SDK client — the runner passes the API key (held only by the runner,
// never in config) via option.WithAPIKey; with none, the SDK reads ANTHROPIC_API_KEY
// from the environment. Passing options straight through keeps the adapter thin.
func New(modelName string, opts ...option.RequestOption) *Adapter {
	return &Adapter{model: modelName, client: sdk.NewClient(opts...)}
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

	stream := a.client.Messages.NewStreaming(ctx, params)
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
			if td, ok := delta.Delta.AsAny().(sdk.TextDelta); ok && td.Text != "" {
				onEvent(model.StreamEvent{TextDelta: td.Text})
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
	return params, nil
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
