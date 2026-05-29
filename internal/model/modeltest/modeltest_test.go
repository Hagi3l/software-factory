package modeltest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/model/modeltest"
	"github.com/Loxstomper/harness/internal/model/registry"
)

// TestServerDrivesRealAdapter is the wire-contract check: the fake server is consumed
// by the *production* OpenAI-compatible adapter (resolved through the real registry, as
// a run would), proving the scripted SSE stream decodes into the canonical Response the
// agent loop reads. If the streamed chunk shape ever drifts from what the SDK
// accumulator expects, this fails here rather than deep inside the e2e spine test.
func TestServerDrivesRealAdapter(t *testing.T) {
	srv := modeltest.NewServer(t, []modeltest.Turn{
		{ToolCalls: []modeltest.ToolCall{{ID: "call_1", Name: "run", Args: `{"command":"echo hi"}`}}},
		{Text: "all done"},
	})

	reg, err := registry.New(map[string]config.ModelProvider{
		"fake": {Provider: config.ProviderOpenAICompat, Endpoint: srv.URL()},
	})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	adapter, err := reg.Adapter("fake")
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}

	// Turn 1: a tool call.
	resp, err := adapter.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Text: "go"}},
	}, nil)
	if err != nil {
		t.Fatalf("Complete turn 1: %v", err)
	}
	if resp.Stop != model.StopToolUse {
		t.Errorf("turn 1 stop = %q, want %q", resp.Stop, model.StopToolUse)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("turn 1 tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "run" {
		t.Errorf("tool call = %+v, want id=call_1 name=run", tc)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(tc.Args, &args); err != nil {
		t.Fatalf("decode tool args %q: %v", tc.Args, err)
	}
	if args.Command != "echo hi" {
		t.Errorf("tool args command = %q, want %q", args.Command, "echo hi")
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("turn 1 usage = %+v, want input=10 output=5", resp.Usage)
	}

	// Turn 2: plain text, natural stop.
	resp, err = adapter.Complete(context.Background(), model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Text: "go"}},
	}, nil)
	if err != nil {
		t.Fatalf("Complete turn 2: %v", err)
	}
	if resp.Stop != model.StopEndTurn {
		t.Errorf("turn 2 stop = %q, want %q", resp.Stop, model.StopEndTurn)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("turn 2 tool calls = %d, want 0", len(resp.ToolCalls))
	}
	if resp.Text != "all done" {
		t.Errorf("turn 2 text = %q, want %q", resp.Text, "all done")
	}

	if got := srv.Requests(); got != 2 {
		t.Errorf("server saw %d requests, want 2", got)
	}
}
