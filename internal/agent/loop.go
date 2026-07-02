package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// DefaultMaxTurns bounds the ReAct loop when Budget.MaxTurns is unset. A turn cap is
// the loop's own termination guarantee independent of token accounting: a model that
// emits tiny responses could otherwise iterate far longer than a token budget intends.
const DefaultMaxTurns = 50

// Budget bounds one invocation so the loop always terminates — budgets ARE the halting
// guarantee, not merely cost control (see specs/workflow.md, CLAUDE.md). The loop
// enforces these per-invocation caps from tallied Usage; BudgetFromPolicy maps the
// operator's config.Policy onto them.
//
// A breach is not an error and is not, by itself, a dead-letter: the loop returns a
// `failed` Result so the runner harvests it cleanly (rather than redelivering the work
// to loop forever). Whether that failure is retried via on_failure or dead-lettered is
// the orchestrator's decision, made from the retry cap plus the cumulative per-issue
// budget — see the reconciliation loop in specs/components/orchestrator.md (plan T1.19).
// The per-invocation caps here are the secondary guarantee that a single invocation
// always halts; the cumulative budget + retry cap are the primary termination guarantee.
type Budget struct {
	// MaxTurns caps ReAct iterations. 0 → DefaultMaxTurns. Always positive after New.
	MaxTurns int
	// MaxTokens caps cumulative input+output tokens tallied across the invocation from
	// each Response.Usage. 0 → uncapped (MaxTurns still bounds the loop).
	MaxTokens int
	// MaxOutputTokens is the per-call output ceiling passed as Request.MaxTokens. 0 →
	// the adapter's default.
	MaxOutputTokens int
}

// Loop is the agent inner loop: a Go-native ReAct loop that drives one soul against one
// work item, then dies (see specs/components/agent.md). It implements runner.Invoker —
// the runner provisions the sandbox and stands up the broker, then hands both to the
// loop. The loop boots the soul (loads its persona as the system prompt), builds the
// initial context from the Brief, and iterates {canonical request → brokered model call
// → execute tool calls → append results → budget check} until a lifecycle tool submits
// or escalates, or the budget is exhausted.
//
// The loop is stateless across invocations and provider-unaware: it speaks the canonical
// model interface and relays every model call through the broker, so any tool-calling
// model can drive it and no key ever reaches the loop.
type Loop struct {
	tools  ToolSource
	budget Budget
	log    *slog.Logger

	// readPersona loads a soul's persona markdown into the system prompt. A seam over
	// os.ReadFile: in the co-located bootstrap the loop reads the persona from the host
	// path the Brief carries; once the agent runs as its own binary inside the sandbox
	// the persona content will travel in the Brief instead (Firecracker work, Phase 5).
	readPersona func(path string) ([]byte, error)
	// connect builds the broker connection for an invocation's endpoint. A seam so the
	// loop is unit-tested without a real socket; the default dials the runner's broker.
	connect func(ep sandbox.Endpoint) brokerConn
	// tracer opens a tool-call span around every workspace/lifecycle tool the model
	// invokes (specs/observability.md's `tool-call ×M`). These tools run unbrokered, so
	// unlike the broker's egress spans (git-push, package-fetch) the runner can't see
	// them — the loop, which runs co-located in the trusted runner today, spans them
	// directly off the invocation ctx. Defaults to a no-op so a Loop built without
	// telemetry (most tests) stays silent. Once the agent becomes its own in-sandbox
	// binary (Phase 5, zero-network) these spans must instead ride the broker.
	tracer trace.Tracer
}

// Option configures a Loop at construction. Added as a variadic tail on New so existing
// three-arg callers keep compiling.
type Option func(*Loop)

// WithTracer makes the loop emit a tool-call span per tool invocation. Pass the shared
// telemetry Provider's tracer (tel.Tracer()); omit it to leave tool calls untraced.
func WithTracer(t trace.Tracer) Option {
	return func(l *Loop) {
		if t != nil {
			l.tracer = t
		}
	}
}

// New builds a Loop over a ToolSource (the workspace + lifecycle tools, plan T1.14/T1.15)
// and a Budget. A nil logger discards. MaxTurns defaults to DefaultMaxTurns so the loop
// always terminates.
func New(tools ToolSource, budget Budget, log *slog.Logger, opts ...Option) *Loop {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if budget.MaxTurns <= 0 {
		budget.MaxTurns = DefaultMaxTurns
	}
	l := &Loop{
		tools:       tools,
		budget:      budget,
		log:         log,
		readPersona: os.ReadFile,
		connect: func(ep sandbox.Endpoint) brokerConn {
			return broker.NewClient(ep.Network, ep.Address)
		},
		tracer: noop.NewTracerProvider().Tracer(telemetry.ScopeName),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Invoke runs the full inner loop for one Brief and returns its Result. It satisfies
// runner.Invoker. A returned error is fatal to the invocation (provisioning the loop's
// own collaborators failed, the broker call could not complete, or a tool errored
// catastrophically) and tells the runner to redeliver the work; a budget breach or a
// model that gives up returns a `failed` Result, not an error.
func (l *Loop) Invoke(ctx context.Context, sb sandbox.Sandbox, brief core.Brief, brokerEndpoint sandbox.Endpoint) (core.Result, error) {
	persona, err := l.bootSoul(brief.Soul)
	if err != nil {
		return core.Result{}, err
	}

	conn := l.connect(brokerEndpoint)
	tools, cleanup, err := l.tools(Invocation{Sandbox: sb, Broker: conn, Completer: conn, Brief: brief})
	if err != nil {
		return core.Result{}, fmt.Errorf("agent: build tools: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	byName := make(map[string]Tool, len(tools))
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		d := t.Def()
		byName[d.Name] = t
		defs = append(defs, d)
	}

	messages := []model.Message{{Role: model.RoleUser, Text: buildContext(brief)}}
	var total model.Usage

	// The per-soul turn cap (if set) overrides the loop's default for this invocation only —
	// the Loop is shared across every soul, so this stays a local rather than mutating
	// l.budget. A soul prone to flailing can fail fast at a tighter cap; 0 keeps the default.
	maxTurns := l.budget.MaxTurns
	if brief.Soul.MaxTurns > 0 {
		maxTurns = brief.Soul.MaxTurns
	}

	for turn := 1; turn <= maxTurns; turn++ {
		req := model.Request{
			System:    persona,
			Messages:  messages,
			Tools:     defs,
			MaxTokens: l.budget.MaxOutputTokens,
		}
		resp, err := conn.Complete(ctx, req)
		if err != nil {
			return core.Result{}, fmt.Errorf("agent: model call (turn %d): %w", turn, err)
		}

		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		total.CacheReadTokens += resp.Usage.CacheReadTokens
		total.CacheCreationTokens += resp.Usage.CacheCreationTokens

		messages = append(messages, model.Message{
			Role:      model.RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if l.budget.MaxTokens > 0 && total.InputTokens+total.OutputTokens >= l.budget.MaxTokens {
			l.log.WarnContext(ctx, "agent: token budget exhausted, stopping",
				"issue", brief.Issue.ID, "turn", turn,
				"input_tokens", total.InputTokens, "output_tokens", total.OutputTokens, "cap", l.budget.MaxTokens)
			return core.Result{Status: core.StatusFailed}, nil
		}

		// No tool calls means the model stopped without submitting or escalating. It
		// cannot self-submit (there is no candidate), so nudge it back to a lifecycle
		// tool; the turn cap bounds a model that keeps refusing.
		if len(resp.ToolCalls) == 0 {
			l.log.DebugContext(ctx, "agent: model returned no tool calls, nudging", "issue", brief.Issue.ID, "turn", turn, "stop", resp.Stop)
			messages = append(messages, model.Message{Role: model.RoleUser, Text: nudge})
			continue
		}

		results := make([]model.ToolResult, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tool, ok := byName[tc.Name]
			if !ok {
				l.log.WarnContext(ctx, "agent: model called unknown tool", "issue", brief.Issue.ID, "tool", tc.Name)
				results = append(results, model.ToolResult{
					ToolCallID: tc.ID,
					Content:    fmt.Sprintf("unknown tool %q; available tools: %s", tc.Name, strings.Join(toolNames(defs), ", ")),
					IsError:    true,
				})
				continue
			}
			out, err := l.invokeTool(ctx, brief, turn, tool, tc)
			if err != nil {
				return core.Result{}, fmt.Errorf("agent: tool %q (turn %d): %w", tc.Name, turn, err)
			}
			if out.Result != nil {
				l.log.InfoContext(ctx, "agent: invocation terminated by tool", "issue", brief.Issue.ID, "tool", tc.Name, "status", out.Result.Status, "turns", turn)
				return *out.Result, nil
			}
			results = append(results, model.ToolResult{ToolCallID: tc.ID, Content: out.Content, IsError: out.IsError})
		}
		messages = append(messages, model.Message{Role: model.RoleTool, ToolResults: results})
	}

	l.log.WarnContext(ctx, "agent: turn budget exhausted, stopping", "issue", brief.Issue.ID, "max_turns", maxTurns)
	return core.Result{Status: core.StatusFailed}, nil
}

// bootSoul resolves the soul's persona into the system prompt. The persona is the soul's
// only behavioral input; an empty path is a config gap (harness validate guarantees
// persona files exist), so the loop treats a missing path as a hard error rather than
// running a personaless agent.
func (l *Loop) bootSoul(soul core.Soul) (string, error) {
	if soul.Persona == "" {
		return "", fmt.Errorf("agent: soul %q has no persona", soul.Name)
	}
	data, err := l.readPersona(soul.Persona)
	if err != nil {
		return "", fmt.Errorf("agent: read persona %s for soul %q: %w", soul.Persona, soul.Name, err)
	}
	return string(data), nil
}

// invokeTool runs one tool call inside a tool-call span parented to the invocation ctx,
// so every workspace/lifecycle tool the model drives shows up in the invocation trace
// (specs/observability.md's `tool-call ×M`) — not just the broker's egress tools. The
// span's duration captures the tool's real wall-clock (a `run` that shells out a slow
// `go build`, a `read_file`), which is the only place that timing is observable while
// the tools run unbrokered. A tool that reports IsError (a failed compile, a bad path)
// is normal loop flow, so it is tagged via an attribute rather than marked span-failed;
// only a loop-fatal Go error records on the span.
func (l *Loop) invokeTool(ctx context.Context, brief core.Brief, turn int, tool Tool, tc model.ToolCall) (Outcome, error) {
	ctx, span := l.tracer.Start(ctx, telemetry.SpanToolCall, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentRunner),
		attribute.String(telemetry.AttrToolName, tc.Name),
		attribute.String(telemetry.AttrIssueID, brief.Issue.ID),
		attribute.String(telemetry.AttrIssueRole, brief.Issue.Role),
		attribute.Int(telemetry.AttrToolTurn, turn),
	))
	defer span.End()

	out, err := tool.Invoke(ctx, tc.Args)
	if err != nil {
		span.RecordError(err)
		return out, err
	}
	if out.IsError {
		span.SetAttributes(attribute.Bool(telemetry.AttrToolError, true))
	}
	return out, nil
}

// nudge steers a model that stopped without calling a lifecycle tool back to one. The
// loop cannot infer intent (it has no candidate to submit and no detected ambiguity to
// escalate), so it must ask rather than guess — escalate, never invent intent (see
// specs/components/agent.md).
const nudge = "You did not call a tool. Continue the task using the available tools. " +
	"When the candidate branch is ready, call submit. If the specification is ambiguous " +
	"or contradictory, call escalate rather than guessing."

// buildContext renders the Brief into the opening user turn. Because the agent is
// stateless and sandboxed, the Brief is its entire knowledge of the world (see
// specs/components/agent.md): the work item, the bounded spec slice, the base ref, and
// the postconditions this node must satisfy.
func buildContext(brief core.Brief) string {
	var b strings.Builder
	b.WriteString("# Work item\n")
	fmt.Fprintf(&b, "%s: %s\n", brief.Issue.ID, brief.Issue.Title)
	if brief.Issue.Body != "" {
		b.WriteString("\n")
		b.WriteString(brief.Issue.Body)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\n# Base\nBranch from: %s\nCommit your work onto branch %q before calling submit; only that branch can be pushed.\n",
		brief.Base, core.CandidateBranch(brief.Issue.ID))

	// The integration branch the trusted merge queue will land this candidate onto — `main`,
	// or the epic branch in epic mode (specs/integration.md). It is informational for most
	// roles (they branch from Base and never touch it), but load-bearing for the merge-resolver,
	// whose whole job is rebasing a conflicting candidate onto exactly this branch; surfacing it
	// here is what lets that soul target the epic branch without a per-mode persona.
	fmt.Fprintf(&b, "\n# Integration branch\nThe trusted merge queue integrates your verified candidate onto %q. You never push it.\n",
		brief.IntegrationBaseOrMain())

	if brief.Spec != "" {
		b.WriteString("\n# Specification (resolved slice)\n")
		b.WriteString(brief.Spec)
		b.WriteString("\n")
	}

	if len(brief.Criteria) > 0 {
		b.WriteString("\n# Acceptance criteria\n")
		for _, c := range brief.Criteria {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}

	b.WriteString("\nComplete this work item using the available tools. " +
		"Cite the specification for non-obvious decisions. When the candidate branch is ready, " +
		"call submit. If the specification is ambiguous or contradictory, call escalate rather than guessing.\n")
	return b.String()
}

func toolNames(defs []model.ToolDef) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}
