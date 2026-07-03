package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Loxstomper/harness/internal/model"
)

// toParams is the request-translation half of the adapter and is pure (no network),
// so it is tested by marshaling the produced SDK params to the wire JSON and asserting
// the shape OpenAI will receive. This catches role/tool-call/tool-definition mapping
// mistakes without a live call.
func TestToParamsWireShape(t *testing.T) {
	a := New("gpt-4o")
	req := model.Request{
		System:    "be terse",
		MaxTokens: 100,
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "hello"},
			{Role: model.RoleAssistant, Text: "calling", ToolCalls: []model.ToolCall{
				{ID: "call_1", Name: "read_file", Args: json.RawMessage(`{"path":"x"}`)},
			}},
			{Role: model.RoleTool, ToolResults: []model.ToolResult{
				{ToolCallID: "call_1", Content: "data", IsError: false},
				{ToolCallID: "call_2", Content: "more", IsError: false},
			}},
		},
		Tools: []model.ToolDef{{
			Name:        "read_file",
			Description: "reads",
			Params:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}

	params, err := a.toParams(req)
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal wire json: %v", err)
	}

	if got["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", got["model"])
	}
	if got["max_completion_tokens"] != float64(100) {
		t.Errorf("max_completion_tokens = %v, want 100", got["max_completion_tokens"])
	}
	// Streaming usage must be requested so the runner can tally budgets.
	if so := asMap(t, got["stream_options"]); so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true", so["include_usage"])
	}

	// System becomes a leading system message (OpenAI has no separate system channel).
	msgs := asSlice(t, got["messages"])
	if len(msgs) != 5 {
		t.Fatalf("messages len = %d, want 5 (system + user + assistant + 2 tool results)", len(msgs))
	}

	if m0 := asMap(t, msgs[0]); m0["role"] != "system" || m0["content"] != "be terse" {
		t.Errorf("messages[0] = %v, want system 'be terse'", m0)
	}
	if m1 := asMap(t, msgs[1]); m1["role"] != "user" || m1["content"] != "hello" {
		t.Errorf("messages[1] = %v, want user 'hello'", m1)
	}

	// [2] assistant with text + a tool_calls entry (function with stringified args).
	m2 := asMap(t, msgs[2])
	if m2["role"] != "assistant" || m2["content"] != "calling" {
		t.Errorf("messages[2] role/content = %v", m2)
	}
	tcs := asSlice(t, m2["tool_calls"])
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls len = %d, want 1", len(tcs))
	}
	tc := asMap(t, tcs[0])
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Errorf("tool_call = %v, want id=call_1 type=function", tc)
	}
	fn := asMap(t, tc["function"])
	if fn["name"] != "read_file" || fn["arguments"] != `{"path":"x"}` {
		t.Errorf("tool_call.function = %v, want name=read_file arguments stringified", fn)
	}

	// [3],[4] each tool result becomes its own message with the tool role.
	m3 := asMap(t, msgs[3])
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" || m3["content"] != "data" {
		t.Errorf("messages[3] = %v, want tool result for call_1", m3)
	}
	m4 := asMap(t, msgs[4])
	if m4["role"] != "tool" || m4["tool_call_id"] != "call_2" || m4["content"] != "more" {
		t.Errorf("messages[4] = %v, want tool result for call_2", m4)
	}

	// Tool definition: OpenAI takes the JSON Schema verbatim as function.parameters.
	tools := asSlice(t, got["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool := asMap(t, tools[0])
	if tool["type"] != "function" {
		t.Errorf("tool.type = %v, want function", tool["type"])
	}
	def := asMap(t, tool["function"])
	if def["name"] != "read_file" || def["description"] != "reads" {
		t.Errorf("function def = %v, want name=read_file description=reads", def)
	}
	schema := asMap(t, def["parameters"])
	if asMap(t, schema["properties"])["path"] == nil {
		t.Errorf("parameters.properties missing 'path': %v", schema)
	}
	if req := asSlice(t, schema["required"]); len(req) != 1 || req[0] != "path" {
		t.Errorf("parameters.required = %v, want [path]", schema["required"])
	}
}

// OpenAI does not require an output cap, so an unset MaxTokens must leave
// max_completion_tokens off the wire entirely (let the server use its default),
// rather than inventing a ceiling the way the Anthropic adapter must.
func TestToParamsOmitsMaxTokensWhenUnset(t *testing.T) {
	a := New("gpt-4o")
	params, err := a.toParams(model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Errorf("max_completion_tokens present (%v), want omitted when Request leaves it 0", got["max_completion_tokens"])
	}
	if _, ok := got["max_tokens"]; ok {
		t.Errorf("deprecated max_tokens present (%v), want only max_completion_tokens ever set", got["max_tokens"])
	}
}

// An empty canonical Args (a no-argument tool call) must still produce valid JSON on
// the wire, since OpenAI parses the arguments string.
func TestToParamsEmptyToolArgsBecomesObject(t *testing.T) {
	a := New("gpt-4o")
	params, err := a.toParams(model.Request{Messages: []model.Message{
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Name: "noargs"}}},
	}})
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	b, _ := json.Marshal(params)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tc := asMap(t, asSlice(t, asMap(t, asSlice(t, got["messages"])[0])["tool_calls"])[0])
	if asMap(t, tc["function"])["arguments"] != "{}" {
		t.Errorf("empty args = %v, want \"{}\"", asMap(t, tc["function"])["arguments"])
	}
}

func TestToParamsRejectsUnknownRole(t *testing.T) {
	a := New("gpt-4o")
	if _, err := a.toParams(model.Request{Messages: []model.Message{{Role: "wizard"}}}); err == nil {
		t.Error("expected error for unsupported message role")
	}
}

// fromCompletion is the response-translation half. It is tested by unmarshaling an
// API-shaped payload into the SDK's ChatCompletion (exercising the real parse path)
// and asserting the canonical Response it produces.
func TestFromCompletion(t *testing.T) {
	raw := `{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-4o",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "hello world",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"main.go\"}"}}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19, "prompt_tokens_details": {"cached_tokens": 5}}
	}`
	var c sdk.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal sdk.ChatCompletion: %v", err)
	}

	resp := fromCompletion(c)
	if resp.Text != "hello world" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello world")
	}
	if resp.Stop != model.StopToolUse {
		t.Errorf("Stop = %q, want %q", resp.Stop, model.StopToolUse)
	}
	// cached_tokens is a subset of prompt_tokens for OpenAI, reported but not additive.
	wantUsage := model.Usage{InputTokens: 12, OutputTokens: 7, CacheReadTokens: 5}
	if resp.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, wantUsage)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" {
		t.Errorf("tool call = {%s %s}, want {call_1 read_file}", tc.ID, tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Args, &args); err != nil {
		t.Fatalf("tool call args not valid json: %v", err)
	}
	if args["path"] != "main.go" {
		t.Errorf("tool call args = %v, want {path: main.go}", args)
	}
}

// A response with no choices (some compat servers stream only a usage chunk on an
// empty turn) must not panic and must still surface usage.
func TestFromCompletionNoChoices(t *testing.T) {
	c := sdk.ChatCompletion{Usage: sdk.CompletionUsage{PromptTokens: 3, CompletionTokens: 0}}
	resp := fromCompletion(c)
	if resp.Text != "" || len(resp.ToolCalls) != 0 {
		t.Errorf("empty completion produced text/tools: %+v", resp)
	}
	if resp.Usage.InputTokens != 3 {
		t.Errorf("Usage.InputTokens = %d, want 3", resp.Usage.InputTokens)
	}
}

func TestFromFinishReason(t *testing.T) {
	cases := map[string]model.StopReason{
		"stop":           model.StopEndTurn,
		"length":         model.StopMaxTokens,
		"tool_calls":     model.StopToolUse,
		"function_call":  model.StopToolUse,
		"content_filter": model.StopContentFilter,
		"some_new_thing": model.StopReason("some_new_thing"), // unknown passes through losslessly
	}
	for in, want := range cases {
		if got := fromFinishReason(in); got != want {
			t.Errorf("fromFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCompleteIntegration makes one real, cheap streaming call. It is skipped unless
// OPENAI_API_KEY is set (mirroring the Anthropic adapter's gated live test), so it
// never blocks the offline gate but verifies the live wiring — streaming, accumulation,
// and usage — when a key is available. OPENAI_BASE_URL/OPENAI_MODEL override the
// endpoint and model so the same test can hit a local Ollama/vLLM server.
func TestCompleteIntegration(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set; skipping live OpenAI-compatible call")
	}
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-4o-mini"
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	a := New(modelName, opts...)

	var deltas int
	resp, err := a.Complete(context.Background(), model.Request{
		System:    "Reply with exactly the word: pong",
		MaxTokens: 16,
		Messages:  []model.Message{{Role: model.RoleUser, Text: "ping"}},
	}, func(e model.StreamEvent) {
		if e.TextDelta != "" {
			deltas++
		}
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text == "" {
		t.Error("expected non-empty text from live call")
	}
	if resp.Usage.InputTokens == 0 {
		t.Error("expected non-zero input tokens (stream_options.include_usage must be set)")
	}
	if deltas == 0 {
		t.Error("expected at least one streamed text delta")
	}
}

// TestCompleteEmitsEffortWireField proves the adapter puts the configured effort level on the
// wire in the form effort_param selects: the top-level `verbosity` field (Claude 4.6+/5) or the
// unified `reasoning: {effort}` object (everything else), and neither when effort is unset. It
// captures the outgoing request body against a scripted SSE server — deterministic, no network.
func TestCompleteEmitsEffortWireField(t *testing.T) {
	cases := []struct {
		name          string
		effort, param string
		check         func(t *testing.T, body map[string]any)
	}{
		{"verbosity", "medium", "verbosity", func(t *testing.T, b map[string]any) {
			if b["verbosity"] != "medium" {
				t.Errorf("verbosity = %v, want \"medium\"", b["verbosity"])
			}
			if _, ok := b["reasoning"]; ok {
				t.Errorf("reasoning present with verbosity transport: %v", b["reasoning"])
			}
		}},
		{"reasoning", "high", "reasoning", func(t *testing.T, b map[string]any) {
			r, ok := b["reasoning"].(map[string]any)
			if !ok {
				t.Fatalf("reasoning = %v, want an object", b["reasoning"])
			}
			if r["effort"] != "high" {
				t.Errorf("reasoning.effort = %v, want \"high\"", r["effort"])
			}
			if _, ok := b["verbosity"]; ok {
				t.Errorf("verbosity present with reasoning transport: %v", b["verbosity"])
			}
		}},
		{"unset", "", "", func(t *testing.T, b map[string]any) {
			if _, ok := b["verbosity"]; ok {
				t.Errorf("verbosity present with no effort: %v", b["verbosity"])
			}
			if _, ok := b["reasoning"]; ok {
				t.Errorf("reasoning present with no effort: %v", b["reasoning"])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodyCh := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m map[string]any
				_ = json.NewDecoder(r.Body).Decode(&m)
				bodyCh <- m
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, _ := w.(http.Flusher)
				fmt.Fprintf(w, "data: %s\n\n", `{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				if flusher != nil {
					flusher.Flush()
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			a := New("test-model", option.WithBaseURL(srv.URL)).WithEffort(tc.effort, tc.param)
			if _, err := a.Complete(context.Background(), model.Request{
				MaxTokens: 8,
				Messages:  []model.Message{{Role: model.RoleUser, Text: "hi"}},
			}, nil); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			tc.check(t, <-bodyCh)
		})
	}
}

// TestCompleteStreamsReasoningAndText drives the adapter against a scripted SSE server
// that emits the non-standard `reasoning` delta a "thinking" model (e.g. Ollama's qwen)
// streams alongside `content`. It proves the adapter routes each to the right channel —
// reasoning to ReasoningDelta, content to TextDelta — so a turn's chain of thought is
// observable even though the SDK has no typed field for it. Deterministic: no network.
func TestCompleteStreamsReasoningAndText(t *testing.T) {
	frames := []string{
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"Let me"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":" think"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, fr := range frames {
			fmt.Fprintf(w, "data: %s\n\n", fr)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := New("qwen-test", option.WithAPIKey("test"), option.WithBaseURL(srv.URL))

	var text, reasoning strings.Builder
	resp, err := a.Complete(context.Background(),
		model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}},
		func(e model.StreamEvent) {
			text.WriteString(e.TextDelta)
			reasoning.WriteString(e.ReasoningDelta)
		})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reasoning.String() != "Let me think" {
		t.Errorf("reasoning channel = %q, want %q", reasoning.String(), "Let me think")
	}
	if text.String() != "Hi" {
		t.Errorf("text channel = %q, want %q", text.String(), "Hi")
	}
	if resp.Text != "Hi" {
		t.Errorf("resp.Text = %q, want Hi", resp.Text)
	}
	// The reasoning stream also lands on the canonical Response, not just the live
	// feed, so the recorded transcript carries the decision trail (T14.3).
	if resp.Reasoning != "Let me think" {
		t.Errorf("resp.Reasoning = %q, want %q", resp.Reasoning, "Let me think")
	}
}

// With prompt caching enabled, the first user message (the Brief) must go on the wire as a
// structured content array whose text part carries an ephemeral cache_control breakpoint — the
// marker an OpenAI-compatible gateway forwards to an Anthropic backend, whose prefix covers the
// leading system message too. With caching off it stays a plain string, byte-identical to the
// default, so a strict server is never sent a field it may reject. See specs/models.md.
func TestToParamsCachingMarksBrief(t *testing.T) {
	req := model.Request{
		System: "be terse",
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "the brief"},
			{Role: model.RoleAssistant, Text: "ok"},
		},
	}

	on, err := New("anthropic/claude", option.WithBaseURL("https://x")).WithPromptCaching(true).toParams(req)
	if err != nil {
		t.Fatalf("toParams (caching on): %v", err)
	}
	// messages[0] is the leading system message; messages[1] is the Brief.
	brief := asMap(t, wireMessages(t, on)[1])
	if brief["role"] != "user" {
		t.Fatalf("messages[1].role = %v, want user", brief["role"])
	}
	parts := asSlice(t, brief["content"])
	if len(parts) != 1 {
		t.Fatalf("Brief content parts = %d, want 1 array part", len(parts))
	}
	part := asMap(t, parts[0])
	if part["type"] != "text" || part["text"] != "the brief" {
		t.Errorf("Brief text part = %v, want text 'the brief'", part)
	}
	if cacheType(part) != "ephemeral" {
		t.Errorf("Brief cache_control = %v, want ephemeral", part["cache_control"])
	}

	off, err := New("gpt-4o").toParams(req)
	if err != nil {
		t.Fatalf("toParams (caching off): %v", err)
	}
	briefOff := asMap(t, wireMessages(t, off)[1])
	if briefOff["content"] != "the brief" {
		t.Errorf("caching-off Brief content = %v, want plain string 'the brief'", briefOff["content"])
	}
}

// With caching on, the LAST message must also carry a cache_control breakpoint — the moving tail
// (specs/models.md "Prompt caching") that lets each turn read the whole prior conversation from
// cache and pay full price only on the new delta. When the last message is a batch of tool
// results, only the FINAL result carries it (it is the newest content); earlier results in the
// same batch stay plain. The Brief keeps its stable-prefix breakpoint independently.
func TestToParamsCachingMarksTail(t *testing.T) {
	req := model.Request{
		System: "be terse",
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "the brief"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "a", Name: "read_file"}, {ID: "b", Name: "search"}}},
			{Role: model.RoleTool, ToolResults: []model.ToolResult{
				{ToolCallID: "a", Content: "first result"},
				{ToolCallID: "b", Content: "last result"},
			}},
		},
	}

	on, err := New("anthropic/claude", option.WithBaseURL("https://x")).WithPromptCaching(true).toParams(req)
	if err != nil {
		t.Fatalf("toParams (caching on): %v", err)
	}
	msgs := wireMessages(t, on)
	// [0] system, [1] Brief (user), [2] assistant tool calls, [3] tool result a, [4] tool result b.
	first := asMap(t, msgs[3])
	if first["role"] != "tool" || cacheType(first) != "" {
		t.Errorf("non-final tool result should be unmarked, got %v", first)
	}
	last := asMap(t, msgs[4])
	if last["role"] != "tool" {
		t.Fatalf("messages[4].role = %v, want tool", last["role"])
	}
	parts := asSlice(t, last["content"])
	if len(parts) != 1 {
		t.Fatalf("final tool result content parts = %d, want 1 array part", len(parts))
	}
	part := asMap(t, parts[0])
	if part["type"] != "text" || part["text"] != "last result" {
		t.Errorf("final tool result text part = %v, want text 'last result'", part)
	}
	if cacheType(part) != "ephemeral" {
		t.Errorf("final tool result cache_control = %v, want ephemeral", part["cache_control"])
	}
	// The Brief still carries its own stable-prefix breakpoint.
	if cacheType(asMap(t, asSlice(t, asMap(t, msgs[1])["content"])[0])) != "ephemeral" {
		t.Errorf("Brief lost its stable-prefix cache_control")
	}

	// Caching off: the final tool result stays a plain string (a strict server never sees the field).
	off, err := New("gpt-4o").toParams(req)
	if err != nil {
		t.Fatalf("toParams (caching off): %v", err)
	}
	if c := asMap(t, wireMessages(t, off)[4])["content"]; c != "last result" {
		t.Errorf("caching-off final tool result content = %v, want plain string 'last result'", c)
	}
}

// A gateway forwarding an Anthropic model reports the cache-WRITE token count in a non-schema
// usage field (cache_creation_input_tokens); fromCompletion must recover it into
// CacheCreationTokens so the runner prices the ~1.25x write, alongside the cache-READ count it
// already maps from prompt_tokens_details.cached_tokens. See specs/models.md.
func TestFromCompletionMapsCacheWrite(t *testing.T) {
	raw := `{
		"id":"c1","object":"chat.completion","created":1,"model":"anthropic/claude",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,
			"prompt_tokens_details":{"cached_tokens":800},"cache_creation_input_tokens":200}
	}`
	var c sdk.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := model.Usage{InputTokens: 1000, OutputTokens: 5, CacheReadTokens: 800, CacheCreationTokens: 200}
	if got := fromCompletion(c).Usage; got != want {
		t.Errorf("Usage = %+v, want %+v", got, want)
	}
}

// wireMessages marshals built params to JSON and returns the messages array — the shape the
// server actually receives, the only faithful way to assert structured content + extra fields.
func wireMessages(t *testing.T, params sdk.ChatCompletionNewParams) []any {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal wire json: %v", err)
	}
	return asSlice(t, got["messages"])
}

// cacheType returns the cache_control.type of a marshaled content part, or "" if unmarked.
func cacheType(block map[string]any) string {
	cc, ok := block["cache_control"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := cc["type"].(string)
	return s
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %v is not an object", v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v is not an array", v)
	}
	return s
}
