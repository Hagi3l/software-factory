package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Role and StopReason values are a contract: adapters map them to/from provider
// wire formats and observability logs/branches on them. Pin the exact strings so
// an accidental rename of an underlying value can't silently change routing.
func TestRoleWireValues(t *testing.T) {
	want := map[Role]string{
		RoleUser:      "user",
		RoleAssistant: "assistant",
		RoleTool:      "tool",
	}
	for role, wire := range want {
		if string(role) != wire {
			t.Errorf("Role wire value = %q, want %q", string(role), wire)
		}
	}
}

func TestStopReasonWireValues(t *testing.T) {
	want := map[StopReason]string{
		StopEndTurn:       "end_turn",
		StopToolUse:       "tool_use",
		StopMaxTokens:     "max_tokens",
		StopStopSequence:  "stop_sequence",
		StopContentFilter: "content_filter",
	}
	for sr, wire := range want {
		if string(sr) != wire {
			t.Errorf("StopReason wire value = %q, want %q", string(sr), wire)
		}
	}
}

func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	return out
}

// Canonical types cross the sandbox↔runner boundary as JSON via the broker, so
// they must survive a JSON round-trip without data loss — including the raw-JSON
// Args/Params fields. This guards against anyone adding a field that isn't
// JSON-serializable (a func, channel, etc.).
func TestCanonicalTypesJSONRoundTrip(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Text: "calling a tool",
		ToolCalls: []ToolCall{{
			ID:   "call_1",
			Name: "read_file",
			Args: json.RawMessage(`{"path":"main.go"}`),
		}},
		ToolResults: []ToolResult{{
			ToolCallID: "call_1",
			Content:    "package main",
			IsError:    false,
		}},
	}
	if got := roundTrip(t, msg); !reflect.DeepEqual(msg, got) {
		t.Errorf("Message round-trip mismatch:\n got %+v\nwant %+v", got, msg)
	}

	resp := Response{
		Text:  "done",
		Usage: Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3},
		Stop:  StopEndTurn,
	}
	if got := roundTrip(t, resp); !reflect.DeepEqual(resp, got) {
		t.Errorf("Response round-trip mismatch:\n got %+v\nwant %+v", got, resp)
	}

	td := ToolDef{Name: "read_file", Description: "read a file", Params: JSONSchema(`{"type":"object"}`)}
	if got := roundTrip(t, td); !reflect.DeepEqual(td, got) {
		t.Errorf("ToolDef round-trip mismatch:\n got %+v\nwant %+v", got, td)
	}
}
