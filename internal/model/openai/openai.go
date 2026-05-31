// Package openai is the OpenAI-compatible provider adapter: a thin translation
// layer between the harness's canonical model types and the official OpenAI Go
// SDK's Chat Completions API. It implements model.Adapter for OpenAI itself and
// for any server speaking the same wire protocol — Ollama, vLLM, Together, etc. —
// selected by pointing the SDK at a custom base URL (option.WithBaseURL). One
// adapter covers a large surface for little code, which is the whole reason the
// spec (specs/models.md) lists a single "OpenAI-compatible" adapter rather than
// one per vendor. The package is named openai and imports the SDK as sdk to avoid
// the name collision; nothing outside this package sees SDK types.
package openai

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"

	"github.com/Loxstomper/harness/internal/model"
)

// Adapter calls one OpenAI-compatible model. It is bound to a single model name at
// construction; the runner builds one per soul.Model via the registry (plan T1.10).
type Adapter struct {
	model  string
	client sdk.Client
}

// New builds an Adapter for the given model. Request options configure the
// underlying SDK client — the runner passes the API key (held only by the runner,
// never in config) via option.WithAPIKey and, for non-OpenAI backends, the server
// address via option.WithBaseURL; with no key option the SDK reads OPENAI_API_KEY
// from the environment. Passing options straight through keeps the adapter thin and
// is what lets one adapter serve OpenAI, Ollama, vLLM, etc.
func New(modelName string, opts ...option.RequestOption) *Adapter {
	return &Adapter{model: modelName, client: sdk.NewClient(opts...)}
}

// Complete satisfies model.Adapter. It always uses the streaming API (streaming is
// first-class — see specs/models.md), accumulates the streamed chunks into the final
// completion, forwards text deltas to onEvent for the live view, and returns the
// assembled canonical Response.
func (a *Adapter) Complete(ctx context.Context, req model.Request, onEvent model.StreamHandler) (model.Response, error) {
	params, err := a.toParams(req)
	if err != nil {
		return model.Response{}, err
	}

	stream := a.client.Chat.Completions.NewStreaming(ctx, params)
	var acc sdk.ChatCompletionAccumulator
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if onEvent == nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			onEvent(model.StreamEvent{TextDelta: delta.Content})
		}
		// "Thinking" models stream their chain of thought on a non-standard delta field
		// the SDK has no typed accessor for, so it lands in JSON.ExtraFields: Ollama and
		// most local servers use `reasoning`, DeepSeek/vLLM use `reasoning_content`. Either
		// is surfaced on the reasoning channel so an all-tool-call turn is still observable.
		if r := reasoningDelta(delta.JSON.ExtraFields); r != "" {
			onEvent(model.StreamEvent{ReasoningDelta: r})
		}
	}
	if err := stream.Err(); err != nil {
		return model.Response{}, fmt.Errorf("openai: stream: %w", err)
	}
	return fromCompletion(acc.ChatCompletion), nil
}

// reasoningDelta pulls a streamed reasoning fragment out of a chunk delta's extra
// (non-schema) fields. There is no agreed wire field for reasoning on Chat Completions,
// so different OpenAI-compatible servers use different keys; we check the two in the
// wild (`reasoning` — Ollama; `reasoning_content` — DeepSeek/vLLM) and return the first
// that carries a non-empty JSON string. A field whose raw value is not a JSON string is
// ignored rather than guessed at — matching the best-effort nature of the live stream.
func reasoningDelta(extra map[string]respjson.Field) string {
	for _, key := range []string{"reasoning", "reasoning_content"} {
		// Extra (non-schema) fields report Valid()==false — the schema doesn't track them —
		// so gate on the raw value instead: present and a non-null JSON string.
		raw := extra[key].Raw()
		if raw == "" || raw == "null" {
			continue
		}
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// toParams translates a canonical Request into SDK request params.
func (a *Adapter) toParams(req model.Request) (sdk.ChatCompletionNewParams, error) {
	params := sdk.ChatCompletionNewParams{
		Model: a.model,
		// OpenAI only reports token usage on a stream when explicitly asked; the runner
		// needs it to tally budgets (plan T1.16), so always request it.
		StreamOptions: sdk.ChatCompletionStreamOptionsParam{IncludeUsage: sdk.Bool(true)},
	}
	// Unlike Anthropic, OpenAI does not require an output cap, so the canonical "0 =
	// adapter default" means "let the server use its own default" — leave the field
	// unset rather than inventing a ceiling. Termination is guaranteed by runner-side
	// budget enforcement, not this knob. max_completion_tokens (not the deprecated
	// max_tokens) is used: it is the field current OpenAI models accept — including the
	// o-series reasoning models that reject max_tokens — and vLLM honors it too.
	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = sdk.Int(int64(req.MaxTokens))
	}
	if req.System != "" {
		params.Messages = append(params.Messages, sdk.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		msgs, err := toMessageParams(m)
		if err != nil {
			return sdk.ChatCompletionNewParams{}, err
		}
		params.Messages = append(params.Messages, msgs...)
	}
	for _, t := range req.Tools {
		tp, err := toToolParam(t)
		if err != nil {
			return sdk.ChatCompletionNewParams{}, err
		}
		params.Tools = append(params.Tools, tp)
	}
	return params, nil
}

// toMessageParams maps one canonical Message to one or more SDK messages. A RoleTool
// message expands to several SDK messages because OpenAI carries each tool result as
// its own message with the dedicated tool role (keyed by tool_call_id), whereas the
// canonical form batches results — the inverse of Anthropic, which nests tool_result
// blocks inside a single user message.
func toMessageParams(m model.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case model.RoleUser:
		return []sdk.ChatCompletionMessageParamUnion{sdk.UserMessage(m.Text)}, nil

	case model.RoleAssistant:
		var asst sdk.ChatCompletionAssistantMessageParam
		if m.Text != "" {
			asst.Content.OfString = sdk.String(m.Text)
		}
		for _, tc := range m.ToolCalls {
			// OpenAI requires arguments to be a JSON-parseable string; an empty
			// canonical Args (no-argument call) becomes "{}" so the wire payload stays valid.
			args := string(tc.Args)
			if args == "" {
				args = "{}"
			}
			asst.ToolCalls = append(asst.ToolCalls, sdk.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: args,
					},
				},
			})
		}
		return []sdk.ChatCompletionMessageParamUnion{{OfAssistant: &asst}}, nil

	case model.RoleTool:
		// OpenAI has no is_error flag on a tool message (unlike Anthropic); the error
		// signal, if any, lives in the textual content the model reads.
		out := make([]sdk.ChatCompletionMessageParamUnion, 0, len(m.ToolResults))
		for _, tr := range m.ToolResults {
			out = append(out, sdk.ToolMessage(tr.Content, tr.ToolCallID))
		}
		return out, nil

	default:
		return nil, fmt.Errorf("openai: unsupported message role %q", m.Role)
	}
}

// toToolParam maps a canonical ToolDef to an SDK function tool. OpenAI takes the full
// JSON Schema verbatim as the function's parameters object (a map), so — unlike the
// Anthropic adapter, which splits properties/required out — the schema passes through
// whole with nothing dropped.
func toToolParam(t model.ToolDef) (sdk.ChatCompletionToolUnionParam, error) {
	fn := sdk.FunctionDefinitionParam{Name: t.Name}
	if t.Description != "" {
		fn.Description = sdk.String(t.Description)
	}
	if len(t.Params) > 0 {
		var params map[string]any
		if err := json.Unmarshal(t.Params, &params); err != nil {
			return sdk.ChatCompletionToolUnionParam{}, fmt.Errorf("openai: tool %q parameters: %w", t.Name, err)
		}
		fn.Parameters = params
	}
	return sdk.ChatCompletionToolUnionParam{
		OfFunction: &sdk.ChatCompletionFunctionToolParam{Function: fn},
	}, nil
}

// fromCompletion assembles a canonical Response from a completed SDK completion: the
// first choice's text, its tool_calls, the finish reason, and normalized usage.
func fromCompletion(c sdk.ChatCompletion) model.Response {
	var resp model.Response
	if len(c.Choices) > 0 {
		choice := c.Choices[0]
		resp.Text = choice.Message.Content
		for _, tc := range choice.Message.ToolCalls {
			// We only ever define function tools; a non-function ("custom") tool call
			// carries no Function payload, so skip it rather than emit an empty ToolCall.
			if tc.Type != "" && tc.Type != "function" {
				continue
			}
			resp.ToolCalls = append(resp.ToolCalls, model.ToolCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
		resp.Stop = fromFinishReason(choice.FinishReason)
	}
	// OpenAI's prompt_tokens already includes any cached tokens (cached is a subset),
	// so CacheReadTokens is reported here for visibility but is not additive to
	// InputTokens. OpenAI has no separate cache-creation billing, so CacheCreationTokens
	// stays 0 — the divergence from Anthropic (where cache reads are billed separately
	// and excluded from input_tokens) is normalized away by the canonical Usage shape.
	resp.Usage = model.Usage{
		InputTokens:     int(c.Usage.PromptTokens),
		OutputTokens:    int(c.Usage.CompletionTokens),
		CacheReadTokens: int(c.Usage.PromptTokensDetails.CachedTokens),
	}
	return resp
}

// fromFinishReason normalizes OpenAI's finish_reason to the canonical set. The legacy
// function_call reason maps to tool_use alongside the current tool_calls. Any future or
// undocumented reason passes through as its raw string rather than being lost; it
// simply won't match the values the agent loop branches on.
func fromFinishReason(r string) model.StopReason {
	switch r {
	case "stop":
		return model.StopEndTurn
	case "length":
		return model.StopMaxTokens
	case "tool_calls", "function_call":
		return model.StopToolUse
	case "content_filter":
		return model.StopContentFilter
	default:
		return model.StopReason(r)
	}
}

// compile-time check that the adapter satisfies the canonical interface.
var _ model.Adapter = (*Adapter)(nil)
