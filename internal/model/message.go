package model

// Role identifies the author of a Message in canonical conversation history.
// Adapters map these onto each provider's role model — e.g. Anthropic carries the
// system role as a top-level parameter and tool results inside a user message,
// whereas OpenAI uses a dedicated tool role — so the agent loop never has to care.
type Role string

const (
	RoleSystem    Role = "system"    // system/persona instructions
	RoleUser      Role = "user"      // input to the model
	RoleAssistant Role = "assistant" // model output
	RoleTool      Role = "tool"      // carrier for tool results fed back to the model
)

// Message is one turn of canonical conversation history. A single message may
// carry assistant Text together with the ToolCalls it requested, or the
// ToolResults handed back on the following turn; the agent loop appends these as
// it iterates (see specs/components/agent.md, specs/models.md).
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}
