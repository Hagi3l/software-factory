package model

import "encoding/json"

// JSONSchema is a JSON Schema document describing a tool's parameters. It is
// carried verbatim through the canonical layer and handed to the provider adapter
// as-is, so a tool author writes its parameter schema once regardless of which
// provider ultimately answers (see specs/models.md).
type JSONSchema = json.RawMessage

// ToolDef is the canonical definition of a tool the model may call. Tools are
// declared once in this form; each provider adapter translates them into that
// provider's function-calling format. The canonical definition is what makes a
// tool portable across models.
type ToolDef struct {
	Name        string
	Description string
	Params      JSONSchema // JSON Schema for the tool's arguments
}

// ToolCall is the model's request to invoke a tool. Args is the raw JSON the model
// produced; it is decoded and validated by the tool implementation, never here —
// the canonical layer stays provider- and tool-agnostic.
type ToolCall struct {
	ID   string          // provider-assigned id, echoed back in the matching ToolResult
	Name string          // the ToolDef.Name being called
	Args json.RawMessage // arguments as raw JSON
}

// ToolResult is the outcome of executing a ToolCall, fed back to the model on the
// next turn. ToolCallID ties it to its ToolCall so an adapter can render it in the
// provider's format (Anthropic tool_result vs. OpenAI tool role), and IsError lets
// the model see a failure and recover rather than the loop hiding it.
type ToolResult struct {
	ToolCallID string // the ToolCall.ID this result answers
	Content    string // textual result returned to the model
	IsError    bool   // true if the tool failed
}
