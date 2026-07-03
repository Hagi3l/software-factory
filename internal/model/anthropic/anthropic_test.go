package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Loxstomper/harness/internal/model"
)

// captureRT records the outgoing request body, then fails the round-trip — enough to
// assert what the adapter put on the wire without standing up a full SSE responder.
type captureRT struct{ body []byte }

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		c.body, _ = io.ReadAll(r.Body)
	}
	return nil, errors.New("captured")
}

// effortBody drives one Complete through the capturing transport and returns the request
// body the adapter sent. The Complete error is expected (the transport never replies).
func effortBody(t *testing.T, effort string) map[string]any {
	t.Helper()
	rt := &captureRT{}
	a := New("claude-opus-4-8",
		option.WithAPIKey("x"),
		option.WithHTTPClient(&http.Client{Transport: rt}),
		option.WithMaxRetries(0),
	).WithEffort(effort)
	_, _ = a.Complete(context.Background(), model.Request{
		MaxTokens: 16,
		Messages:  []model.Message{{Role: model.RoleUser, Text: "hi"}},
	}, nil)
	if len(rt.body) == 0 {
		t.Fatal("no request body captured")
	}
	var got map[string]any
	if err := json.Unmarshal(rt.body, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	return got
}

// A configured effort rides as output_config.effort on the request body; an unset effort
// adds nothing, so default behavior is byte-identical.
func TestEffortOnWire(t *testing.T) {
	with := effortBody(t, "medium")
	oc, ok := with["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing or wrong type: %v", with["output_config"])
	}
	if oc["effort"] != "medium" {
		t.Errorf("output_config.effort = %v, want medium", oc["effort"])
	}

	without := effortBody(t, "")
	if _, present := without["output_config"]; present {
		t.Errorf("output_config should be absent when effort unset, got %v", without["output_config"])
	}
}

// toParams is the request-translation half of the adapter and is pure (no network),
// so it is tested by marshaling the produced SDK params to the wire JSON and asserting
// the shape Anthropic will receive. This catches role/content-block mapping mistakes
// without a live call.
func TestToParamsWireShape(t *testing.T) {
	a := New("claude-opus-4-7")
	req := model.Request{
		System:    "be terse",
		MaxTokens: 100,
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "hello"},
			{Role: model.RoleAssistant, Text: "calling", ToolCalls: []model.ToolCall{
				{ID: "tu1", Name: "read_file", Args: json.RawMessage(`{"path":"x"}`)},
			}},
			{Role: model.RoleTool, ToolResults: []model.ToolResult{
				{ToolCallID: "tu1", Content: "data", IsError: false},
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

	if got["model"] != "claude-opus-4-7" {
		t.Errorf("model = %v, want claude-opus-4-7", got["model"])
	}
	if got["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v, want 100", got["max_tokens"])
	}

	system := asSlice(t, got["system"])
	if len(system) != 1 || asMap(t, system[0])["text"] != "be terse" {
		t.Errorf("system = %v, want one text block 'be terse'", got["system"])
	}

	msgs := asSlice(t, got["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3", len(msgs))
	}

	// [0] user with a text block
	m0 := asMap(t, msgs[0])
	if m0["role"] != "user" {
		t.Errorf("messages[0].role = %v, want user", m0["role"])
	}
	if blk := asMap(t, asSlice(t, m0["content"])[0]); blk["type"] != "text" || blk["text"] != "hello" {
		t.Errorf("messages[0].content[0] = %v, want text 'hello'", blk)
	}

	// [1] assistant with text + tool_use
	m1 := asMap(t, msgs[1])
	if m1["role"] != "assistant" {
		t.Errorf("messages[1].role = %v, want assistant", m1["role"])
	}
	c1 := asSlice(t, m1["content"])
	if len(c1) != 2 {
		t.Fatalf("messages[1].content len = %d, want 2 (text + tool_use)", len(c1))
	}
	tu := asMap(t, c1[1])
	if tu["type"] != "tool_use" || tu["id"] != "tu1" || tu["name"] != "read_file" {
		t.Errorf("tool_use block = %v, want id=tu1 name=read_file", tu)
	}
	if asMap(t, tu["input"])["path"] != "x" {
		t.Errorf("tool_use input = %v, want {path:x}", tu["input"])
	}

	// [2] tool results -> a user message of tool_result blocks (Anthropic has no tool role)
	m2 := asMap(t, msgs[2])
	if m2["role"] != "user" {
		t.Errorf("messages[2].role = %v, want user (tool_result lives in a user message)", m2["role"])
	}
	tr := asMap(t, asSlice(t, m2["content"])[0])
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "tu1" {
		t.Errorf("tool_result block = %v, want type=tool_result tool_use_id=tu1", tr)
	}

	// tool definition
	tools := asSlice(t, got["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool := asMap(t, tools[0])
	if tool["name"] != "read_file" || tool["description"] != "reads" {
		t.Errorf("tool = %v, want name=read_file description=reads", tool)
	}
	schema := asMap(t, tool["input_schema"])
	if asMap(t, schema["properties"])["path"] == nil {
		t.Errorf("input_schema.properties missing 'path': %v", schema)
	}
	if req := asSlice(t, schema["required"]); len(req) != 1 || req[0] != "path" {
		t.Errorf("input_schema.required = %v, want [path]", schema["required"])
	}
}

func TestToParamsDefaultsMaxTokens(t *testing.T) {
	a := New("claude-opus-4-7")
	params, err := a.toParams(model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	if params.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want default %d when Request leaves it 0", params.MaxTokens, defaultMaxTokens)
	}
}

func TestToParamsRejectsUnknownRole(t *testing.T) {
	a := New("claude-opus-4-7")
	if _, err := a.toParams(model.Request{Messages: []model.Message{{Role: "wizard"}}}); err == nil {
		t.Error("expected error for unsupported message role")
	}
}

// fromMessage is the response-translation half. It is tested by unmarshaling an
// API-shaped payload into the SDK's Message (exercising the real parse path) and
// asserting the canonical Response it produces.
func TestFromMessage(t *testing.T) {
	raw := `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude",
		"content": [
			{"type": "thinking", "thinking": "the file is ", "signature": "sig1"},
			{"type": "thinking", "thinking": "the place to look", "signature": "sig2"},
			{"type": "text", "text": "hello "},
			{"type": "text", "text": "world"},
			{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": {"path": "main.go"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 12, "output_tokens": 7, "cache_creation_input_tokens": 4, "cache_read_input_tokens": 3}
	}`
	var msg sdk.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal sdk.Message: %v", err)
	}

	resp := fromMessage(&msg)
	if resp.Text != "hello world" {
		t.Errorf("Text = %q, want %q (text blocks concatenate)", resp.Text, "hello world")
	}
	// Thinking blocks land on the canonical Reasoning (recorded as emitted — the
	// transcript's decision trail, T14.3) and never leak into Text.
	if resp.Reasoning != "the file is the place to look" {
		t.Errorf("Reasoning = %q, want the concatenated thinking blocks", resp.Reasoning)
	}
	if resp.Stop != model.StopToolUse {
		t.Errorf("Stop = %q, want %q", resp.Stop, model.StopToolUse)
	}
	wantUsage := model.Usage{InputTokens: 12, OutputTokens: 7, CacheCreationTokens: 4, CacheReadTokens: 3}
	if resp.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, wantUsage)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Name != "read_file" {
		t.Errorf("tool call = {%s %s}, want {tu_1 read_file}", tc.ID, tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Args, &args); err != nil {
		t.Fatalf("tool call args not valid json: %v", err)
	}
	if args["path"] != "main.go" {
		t.Errorf("tool call args = %v, want {path: main.go}", args)
	}
}

func TestFromStopReason(t *testing.T) {
	cases := map[sdk.StopReason]model.StopReason{
		sdk.StopReasonEndTurn:      model.StopEndTurn,
		sdk.StopReasonMaxTokens:    model.StopMaxTokens,
		sdk.StopReasonStopSequence: model.StopStopSequence,
		sdk.StopReasonToolUse:      model.StopToolUse,
		sdk.StopReasonRefusal:      model.StopContentFilter,
		"pause_turn":               model.StopReason("pause_turn"), // unknown passes through losslessly
	}
	for in, want := range cases {
		if got := fromStopReason(in); got != want {
			t.Errorf("fromStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToInputSchema(t *testing.T) {
	// Empty schema is allowed (a tool may take no arguments).
	if s, err := toInputSchema(nil); err != nil || s.Properties != nil {
		t.Errorf("empty schema: got (%+v, %v), want zero value", s, err)
	}

	raw := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)
	s, err := toInputSchema(raw)
	if err != nil {
		t.Fatalf("toInputSchema: %v", err)
	}
	props, ok := s.Properties.(map[string]any)
	if !ok || props["path"] == nil {
		t.Errorf("Properties = %v, want a map containing 'path'", s.Properties)
	}
	if len(s.Required) != 1 || s.Required[0] != "path" {
		t.Errorf("Required = %v, want [path]", s.Required)
	}
	// Top-level keys beyond type/properties/required must be preserved, not dropped.
	if s.ExtraFields["additionalProperties"] != false {
		t.Errorf("ExtraFields = %v, want additionalProperties:false preserved", s.ExtraFields)
	}
}

// TestCompleteIntegration makes one real, cheap streaming call. It is skipped unless
// ANTHROPIC_API_KEY is set (mirroring the bd/docker integration tests), so it never
// blocks the offline gate but verifies the live wiring — streaming, accumulation, and
// usage — when a key is available.
func TestCompleteIntegration(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live Anthropic call")
	}
	a := New("claude-haiku-4-5-20251001", option.WithAPIKey(key))

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
		t.Error("expected non-zero input tokens")
	}
	if deltas == 0 {
		t.Error("expected at least one streamed text delta")
	}
}

// Prompt caching is on by default for the Anthropic adapter (specs/models.md): toParams must
// place an ephemeral cache_control breakpoint on the first message's first block (pinning the
// stable tools+system+Brief prefix re-sent every turn) and on the last message's last block
// (the moving breakpoint the provider auto-advances over the growing conversation), and leave
// interior blocks unmarked so the four-breakpoint budget is spent only where it pays.
func TestToParamsMarksCacheBreakpoints(t *testing.T) {
	a := New("claude-opus-4-7")
	req := model.Request{
		System: "be terse",
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "the brief"},
			{Role: model.RoleAssistant, Text: "calling", ToolCalls: []model.ToolCall{
				{ID: "tu1", Name: "read", Args: json.RawMessage(`{}`)},
			}},
			{Role: model.RoleTool, ToolResults: []model.ToolResult{{ToolCallID: "tu1", Content: "data"}}},
		},
	}
	params, err := a.toParams(req)
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := asSlice(t, got["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3", len(msgs))
	}

	first := asMap(t, asSlice(t, asMap(t, msgs[0])["content"])[0])
	if cacheType(first) != "ephemeral" {
		t.Errorf("first block cache_control = %v, want ephemeral", first["cache_control"])
	}
	lastContent := asSlice(t, asMap(t, msgs[2])["content"])
	last := asMap(t, lastContent[len(lastContent)-1])
	if cacheType(last) != "ephemeral" {
		t.Errorf("last block cache_control = %v, want ephemeral", last["cache_control"])
	}
	for i, blk := range asSlice(t, asMap(t, msgs[1])["content"]) {
		if m := asMap(t, blk); m["cache_control"] != nil {
			t.Errorf("interior block %d unexpectedly cached: %v", i, m["cache_control"])
		}
	}
}

// cacheType returns the cache_control.type of a marshaled content block, or "" if unmarked.
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
