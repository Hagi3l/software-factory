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
	model       string
	client      sdk.Client
	caching     bool   // send a cache_control breakpoint on the stable prefix (opt-in; see WithPromptCaching)
	effort      string // reasoning-effort level to send on every call; empty = provider default (see WithEffort)
	effortParam string // wire form for effort: "reasoning" (reasoning:{effort}) or "verbosity" (top-level)
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

// WithPromptCaching opts this model into prompt caching (specs/models.md "Optional
// capability fields"). A builder rather than a New parameter so existing callers stay
// unchanged; the registry chains it from the model's prompt_caching config. It is off by
// default and a no-op when off, because the OpenAI-compatible surface is mixed: OpenAI- and
// DeepSeek-style backends cache automatically (no marker needed) while a strict local server
// may reject an unknown field — so the cache_control marker is sent only where a backend
// both needs and accepts it (an Anthropic model behind an OpenAI-compatible gateway such as
// OpenRouter, which forwards the marker and sticky-routes to keep the cache warm). Config
// validation restricts the flag to provider: openai-compat. Returns the adapter for chaining.
func (a *Adapter) WithPromptCaching(on bool) *Adapter {
	a.caching = on
	return a
}

// WithEffort configures the reasoning-effort control this adapter sends to an openai-compat
// backend that accepts one (specs/models.md "Optional capability fields"). effort is the
// level (low|medium|high|xhigh|max); param selects the wire field — "reasoning" for
// OpenRouter's unified reasoning:{effort} (OpenAI o-series, DeepSeek, Gemini-thinking, Claude
// pre-4.6), "verbosity" for the top-level field Claude 4.6+/5 map to output_config.effort
// (their reasoning.effort is a no-op). A builder like WithPromptCaching so existing callers
// stay unchanged; the registry chains it from the model's effort/effort_param config. Empty
// effort is a no-op (provider default). Returns the adapter for chaining.
func (a *Adapter) WithEffort(effort, param string) *Adapter {
	a.effort, a.effortParam = effort, param
	return a
}

// effortOpts returns the per-request options that carry the reasoning-effort control to an
// openai-compat backend, or nil when effort is unset. The effort level rides as a non-schema
// body field (Chat Completions has no native one), the field chosen by effort_param:
// "reasoning" → OpenRouter's unified reasoning:{effort}; "verbosity" → the top-level verbosity
// field Claude 4.6+/5 map to output_config.effort (their reasoning.effort is a silent no-op).
// Config validation guarantees effort_param is set and valid whenever effort is, so an unknown
// param never reaches here. See specs/models.md "Optional capability fields".
func (a *Adapter) effortOpts() []option.RequestOption {
	if a.effort == "" {
		return nil
	}
	switch a.effortParam {
	case "verbosity":
		return []option.RequestOption{option.WithJSONSet("verbosity", a.effort)}
	case "reasoning":
		return []option.RequestOption{option.WithJSONSet("reasoning", map[string]any{"effort": a.effort})}
	}
	return nil
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

	stream := a.client.Chat.Completions.NewStreaming(ctx, params, a.effortOpts()...)
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
	markedBrief := false
	for i, m := range req.Messages {
		// Two cache_control breakpoints capture the loop's re-sent context (specs/models.md
		// "Prompt caching"), mirroring the Anthropic adapter's applyCaching:
		//   - the Brief (first user message) pins the STABLE PREFIX — persona in System + the
		//     spec/ambient context here — which is byte-identical every turn, so a cache read
		//     from turn 2 on; and
		//   - the LAST message pins the MOVING TAIL — its prefix is the whole conversation so
		//     far, so each turn reads everything before the delta and pays the cache-write
		//     premium only on the new tail. Without this second breakpoint the accumulated
		//     tool results (which dominate a deep run's input) re-bill at full price every turn.
		// Turn 1 the Brief is also the last message; one breakpoint suffices (nothing follows),
		// so the Brief branch's `continue` correctly skips the tail marking that turn.
		if a.caching && !markedBrief && m.Role == model.RoleUser {
			params.Messages = append(params.Messages, cachedUserMessage(m.Text))
			markedBrief = true
			continue
		}
		if a.caching && i == len(req.Messages)-1 {
			msgs, err := cachedMessageParams(m)
			if err != nil {
				return sdk.ChatCompletionNewParams{}, err
			}
			params.Messages = append(params.Messages, msgs...)
			continue
		}
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

// cachedMessageParams maps the loop's LAST canonical message like toMessageParams, but marks
// its final SDK content block with an ephemeral cache_control breakpoint — the moving tail
// breakpoint of specs/models.md "Prompt caching". A RoleTool message expands to one SDK tool
// message per result, so only the LAST result carries the breakpoint (it is the newest content
// and its prefix is the whole conversation). RoleUser reuses cachedUserMessage. RoleAssistant
// never appears last in the agent loop (an assistant turn is always followed by tool results or
// a nudge before the next model call), so it falls back to the uncached form rather than growing
// a cached-assistant path that would never run.
func cachedMessageParams(m model.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case model.RoleUser:
		return []sdk.ChatCompletionMessageParamUnion{cachedUserMessage(m.Text)}, nil
	case model.RoleTool:
		out := make([]sdk.ChatCompletionMessageParamUnion, 0, len(m.ToolResults))
		for i, tr := range m.ToolResults {
			if i == len(m.ToolResults)-1 {
				out = append(out, cachedToolMessage(tr.Content, tr.ToolCallID))
			} else {
				out = append(out, sdk.ToolMessage(tr.Content, tr.ToolCallID))
			}
		}
		return out, nil
	default:
		return toMessageParams(m)
	}
}

// cachedUserMessage builds a user message whose text part carries an ephemeral
// cache_control breakpoint. cache_control is not a native Chat Completions field, so it
// rides via the SDK's extra-fields escape hatch (SetExtraFields) on a structured content
// part — the wire form an OpenAI-compatible gateway forwarding an Anthropic model expects.
// Used to mark the Brief's stable prefix (and, when the Brief is the only message, the tail)
// cacheable. See specs/models.md "Optional capability fields".
func cachedUserMessage(text string) sdk.ChatCompletionMessageParamUnion {
	part := sdk.ChatCompletionContentPartTextParam{Text: text}
	part.SetExtraFields(map[string]any{"cache_control": map[string]any{"type": "ephemeral"}})
	return sdk.ChatCompletionMessageParamUnion{
		OfUser: &sdk.ChatCompletionUserMessageParam{
			Content: sdk.ChatCompletionUserMessageParamContentUnion{
				OfArrayOfContentParts: []sdk.ChatCompletionContentPartUnionParam{{OfText: &part}},
			},
		},
	}
}

// cachedToolMessage builds a tool-result message whose text carries an ephemeral cache_control
// breakpoint — the moving tail breakpoint when the loop's last message is a batch of tool
// results. The SDK's generic ToolMessage takes a content-parts array, so the breakpoint rides
// the same SetExtraFields escape hatch cachedUserMessage uses; the string content is otherwise
// byte-equivalent to sdk.ToolMessage(content, id)'s.
func cachedToolMessage(content, toolCallID string) sdk.ChatCompletionMessageParamUnion {
	part := sdk.ChatCompletionContentPartTextParam{Text: content}
	part.SetExtraFields(map[string]any{"cache_control": map[string]any{"type": "ephemeral"}})
	return sdk.ToolMessage([]sdk.ChatCompletionContentPartTextParam{part}, toolCallID)
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
	// InputTokens. Native OpenAI has no separate cache-creation billing, so CacheCreationTokens
	// is 0 there; a gateway forwarding an Anthropic model (where a cache write bills ~1.25x)
	// reports the write count in a non-schema usage field, which cacheCreationTokens recovers
	// so the runner can price it. The divergence from Anthropic (where cache reads are billed
	// separately and excluded from input_tokens) is normalized away by the canonical Usage shape.
	resp.Usage = model.Usage{
		InputTokens:         int(c.Usage.PromptTokens),
		OutputTokens:        int(c.Usage.CompletionTokens),
		CacheReadTokens:     int(c.Usage.PromptTokensDetails.CachedTokens),
		CacheCreationTokens: cacheCreationTokens(c.Usage),
	}
	return resp
}

// cacheCreationTokens pulls the cache-WRITE token count out of a usage payload. Native
// OpenAI has no separate cache-creation billing (cached_tokens are a subset of prompt_tokens,
// surfaced as CacheReadTokens), but a gateway forwarding an Anthropic model — where a cache
// write bills ~1.25x — reports the write count in a non-schema usage field. There is no agreed
// key, so (mirroring reasoningDelta) we check the ones seen in the wild on both the usage
// object and its prompt-token details and return the first that parses to a positive int.
// Surfacing it lets the runner price the write in USD rather than undercounting cost as if
// every cache miss were free. See specs/models.md.
func cacheCreationTokens(u sdk.CompletionUsage) int {
	for _, extra := range []map[string]respjson.Field{u.JSON.ExtraFields, u.PromptTokensDetails.JSON.ExtraFields} {
		for _, key := range []string{"cache_creation_input_tokens", "cache_write_tokens", "cache_creation_tokens"} {
			raw := extra[key].Raw()
			if raw == "" || raw == "null" {
				continue
			}
			var n int
			if json.Unmarshal([]byte(raw), &n) == nil && n > 0 {
				return n
			}
		}
	}
	return 0
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
