package agent

import (
	"context"
	"encoding/json"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// Tool is one capability the agent may invoke during its loop. A role's behavior
// comes from its persona plus which tools are enabled, not from a different loop (see
// specs/components/agent.md): every soul runs this same loop over a different tool set.
// Tools split along the trust boundary — workspace tools (read_file/edit_file/run …,
// plan T1.14) run in the sandbox on the worktree; lifecycle tools (submit/escalate/
// request_subtask, plan T1.15) shape the Result. The loop knows only this interface;
// the concrete tools are registered per invocation by the ToolSource.
type Tool interface {
	// Def is the canonical tool definition advertised to the model each turn. Adapters
	// translate it into the provider's function-calling format (see specs/models.md).
	Def() model.ToolDef
	// Invoke runs the tool with the model-supplied JSON arguments. A returned error is
	// fatal to the invocation (the runner redelivers the work); a tool that merely
	// "failed" (a build error, a bad path) reports that in Outcome.IsError + Content so
	// the model can react and try again. A non-nil Outcome.Result ends the loop with
	// that Result — this is how submit/escalate terminate the invocation.
	Invoke(ctx context.Context, args json.RawMessage) (Outcome, error)
}

// Outcome is what a Tool returns: the text fed back to the model as the tool result,
// whether that text represents a failure, and — for the lifecycle tools — a terminal
// Result that ends the loop.
type Outcome struct {
	// Content is the tool-result text appended to the conversation for the next turn.
	Content string
	// IsError marks the result as a failure so the model treats it as such (the
	// canonical ToolResult.IsError); it is NOT a loop-fatal error.
	IsError bool
	// Result, when non-nil, terminates the loop and becomes the invocation's Result.
	// Only lifecycle tools (submit → done, escalate → needs-spec-clarification) set it.
	Result *core.Result
}

// Invocation is the per-task context handed to a ToolSource so it can bind tools to
// the live sandbox and broker for one run. Workspace tools capture Sandbox; lifecycle
// tools capture Broker (for the brokered git push on submit) and Brief.
type Invocation struct {
	// Sandbox is the live, provisioned sandbox the worktree lives in; workspace tools
	// Exec against it.
	Sandbox sandbox.Sandbox
	// Broker is the agent side of the broker for this invocation: lifecycle tools push
	// the candidate branch and publish events through it. It holds no credentials.
	Broker BrokerClient
	// Completer is the model-call seam for tools that run their own nested model loop —
	// today only `explore`, whose helper soul drives a read-only sub-loop (specs/components/
	// agent.md "Explore"). It is the same brokered connection the main loop relays through,
	// so a tool's model calls stay provider-unaware and keyless exactly like the loop's. May
	// be nil for a loop built without model-driven tools (most tests).
	Completer Completer
	// Brief is the task envelope — the agent's entire knowledge of the world.
	Brief core.Brief
}

// ToolSource builds the tool set for one invocation. It is the seam plan T1.14
// (workspace tools) and T1.15 (lifecycle tools) fill; the loop calls it once per run
// with the live Invocation. Returning an error fails the invocation.
//
// It also returns a cleanup func (may be nil) the loop defers until the invocation
// returns — the hook for per-invocation tool resources that outlive a single tool call,
// such as the warm LSP session manager (Phase 6, T6.1), which shuts its language servers
// down cleanly rather than waiting for the sandbox teardown to kill them.
type ToolSource func(inv Invocation) (tools []Tool, cleanup func(), err error)

// BrokerClient is the subset of the broker the lifecycle tools use: push the candidate
// branch (only the task branch is accepted by the runner) and publish progress events.
// *broker.Client satisfies it; abstracting it keeps the loop and tools testable without
// a real socket.
type BrokerClient interface {
	GitPush(ctx context.Context, req broker.GitPushRequest) (broker.GitPushResult, error)
	PublishEvent(ctx context.Context, ev broker.PublishRequest) error
}

// Completer is the model-call seam: the loop relays a canonical Request through the
// broker and gets back a canonical Response. *broker.Client satisfies it; the agent is
// provider-unaware (no key, no provider name) — the runner attaches both (see
// specs/models.md).
type Completer interface {
	Complete(ctx context.Context, req model.Request) (model.Response, error)
}

// ExplorerCompleterSource mints a Completer for one explore call's nested sub-loop. Each call
// gets a fresh per-call stream id so the runner meters that call against policy.explore_budget
// and resets per call ("an explore call behaves the same wherever in an invocation it is made").
// The returned Completer tags every completion to the explorer sub-context, so the runner routes
// it to the pinned explorer model — the agent names the tool, never the model (specs/models.md
// "Helper souls", T12.2). *broker.Client provides one (via ExploreCompleter); tests fake it. It
// is a seam so the explore tool need not depend on the concrete broker and stays unit-testable.
type ExplorerCompleterSource interface {
	ExploreCompleter(stream string) Completer
}

// brokerConn is the full broker connection an invocation needs: model completion for
// the loop plus git push / events for the lifecycle tools. The loop builds one per
// invocation from the broker endpoint and shares it with the tools, so a single
// connection backs every brokered call. *broker.Client satisfies it.
type brokerConn interface {
	Completer
	BrokerClient
}

// Compile-time proof the real broker client backs an invocation's brokered calls; if
// the broker API drifts this fails loud here rather than at the wiring in cmd/software-factory.
var _ brokerConn = (*broker.Client)(nil)

// funcTool adapts a definition + closure into a Tool. The concrete workspace (T1.14)
// and lifecycle (T1.15) tools are built as funcTools over closures that capture the
// sandbox / broker / brief, keeping each tool a few lines instead of a named type.
type funcTool struct {
	def model.ToolDef
	fn  func(ctx context.Context, args json.RawMessage) (Outcome, error)
}

func (t funcTool) Def() model.ToolDef { return t.def }
func (t funcTool) Invoke(ctx context.Context, args json.RawMessage) (Outcome, error) {
	return t.fn(ctx, args)
}
