package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// DefaultExploreMaxTurns bounds the explore sub-loop when its Budget.MaxTurns is unset.
// It is deliberately much tighter than the main loop's DefaultMaxTurns (50): explore is a
// focused localization query, not a full task, and a runaway sub-loop must not burn the
// parent's frontier tokens. Matches the configuration.md `policy.explore_budget` example.
const DefaultExploreMaxTurns = 12

// Coverage classifies how far an explore answer got, so the parent knows how much to trust
// a partial (specs/components/agent.md "Explore — distilled comprehension"). It is the
// explorer's honesty knob: a wrong-but-confident anchor is worse than an honest partial.
const (
	CoverageComplete         = "complete"          // confident it found what the question asked
	CoveragePartialBudget    = "partial-budget"    // ran out of budget; more may exist (see Leads)
	CoveragePartialUncertain = "partial-uncertain" // could not resolve confidently
)

// ExploreAnchor is a single grounded pointer the explorer returns: a file:line the parent
// can re-read at full fidelity before acting, plus a one-line reason it matters. Anchors are
// pointers, NOT snippets — the whole point of explore is to keep the raw reading out of the
// parent's window, so the parent precise-reads the anchor itself (specs/components/agent.md
// "distill for navigation, precise-read for action").
type ExploreAnchor struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Why  string `json:"why"`
}

// ExploreAnswer is the distilled residue an explore call returns to the parent instead of the
// raw search→read→refine reading. It is the free-form-question-in / structured-answer-out
// contract from specs/components/agent.md: a tight prose Summary, grounded Anchors (always
// required for a confident answer), a Coverage honesty flag, and Leads the parent can re-ask
// narrower on rather than re-explore blind.
type ExploreAnswer struct {
	Summary  string          `json:"summary"`
	Anchors  []ExploreAnchor `json:"anchors"`
	Coverage string          `json:"coverage"`
	Leads    []string        `json:"leads,omitempty"`
}

// ReadOnlyTools is the comprehension (read-only) tool subset: the text-floor read tools
// (read_file/list_dir/search) plus the LSP-backed semantic read tools (find_symbol,
// references, definition, implementation, hover, diagnostics). It is the single definition of
// "what a read-only agent may touch", shared by the explore sub-loop here and the wizard's
// read-only codebase exploration (internal/controlroom/wizard) — one source of truth so the
// two can never drift into leaking a writer.
//
// It fails closed by construction: it *selects* the read tools by name out of the full
// WorkspaceTools set rather than listing writers to exclude, so a renamed or newly added
// write tool simply does not appear here. Passing a nil sandbox is valid ONLY for reading each
// tool's Def() (static data); Invoke needs a live sandbox.
func ReadOnlyTools(sb sandbox.Sandbox, sessions *Sessions) []Tool {
	read := keepTools(WorkspaceTools(sb, sessions), "read_file", "list_dir", "search")
	tools := make([]Tool, 0, len(read)+6)
	tools = append(tools, read...)
	tools = append(tools, SemanticReadTools(sessions)...)
	return tools
}

// keepTools returns the tools whose advertised name is in keep, preserving order. Selecting
// the read subset by name (rather than excluding writers) is what makes ReadOnlyTools fail
// closed — an unknown name is simply dropped.
func keepTools(tools []Tool, keep ...string) []Tool {
	want := make(map[string]bool, len(keep))
	for _, k := range keep {
		want[k] = true
	}
	out := make([]Tool, 0, len(keep))
	for _, t := range tools {
		if want[t.Def().Name] {
			out = append(out, t)
		}
	}
	return out
}

// Explorer configures the explore tool's nested helper soul: the cheap, read-only explorer
// invoked as a tool (specs/components/agent.md "Reuses a Soul, off the DAG"). Persona is the
// explorer's system prompt content (resolved by the caller, as personas will travel in the
// Brief once agents run in-sandbox); Budget is the FIXED per-call sub-budget
// (`policy.explore_budget`, T12.3) that bounds the sub-loop under the parent-task ceiling.
type Explorer struct {
	Persona string
	Budget  Budget
}

// ExploreTool builds the canonical `explore` tool: the parent agent asks a broad, free-form
// comprehension question and gets back a compact distilled ExploreAnswer, instead of running
// the iterative search→read→refine itself and paying for it in its own (frontier) context
// (specs/components/agent.md "Explore — distilled comprehension").
//
// The nested sub-loop runs IN-PROCESS in the same sandbox: its read tools hit the parent's
// already-warm LSP sessions (no reseed, no cold server), its model calls go through the same
// completer, and its answer returns up as this tool's result — never leaving the sandbox.
// Five rules keep the nested loop from reintroducing what a sandbox contains, and this
// constructor enforces the structural ones:
//
//   - Read-only, no exceptions: the sub-loop's tool set is exactly ReadOnlyTools + `answer`.
//     No edit/write/run/submit/escalate/request_subtask is ever built, so single-writer,
//     producer≠verifier, and stateless-souls all hold at once.
//   - No recursion: the set structurally omits `explore` itself, so a sub-loop cannot fan out
//     — the backstop that keeps budgets = termination intact.
//
// The remaining rules are enforced at runtime: the fixed sub-budget bounds the loop (breach →
// a partial-budget answer, never a failed parent), and the model is pinned by the trusted
// dispatch (the completer the runner hands in — T12.2). explore is additive and never
// load-bearing: any failure (model error, low confidence) degrades to a partial answer that
// just routes the parent back to searching itself.
//
// projectMap is the ambient specs (the project index + conventions) the child gets as its map
// — deliberately NOT the parent's conversation: handing it the parent's context would defeat
// the point; handing it only the question would starve it of the project's shape.
func ExploreTool(exp Explorer, sb sandbox.Sandbox, sessions *Sessions, completer Completer, projectMap string, log *slog.Logger) Tool {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// Built once and reused across explore calls: the read tools are stateless closures over
	// the (warm) sandbox + sessions. The per-call `answer` tool is appended inside Invoke
	// because it captures that call's answer sink.
	readTools := ReadOnlyTools(sb, sessions)

	return funcTool{
		def: model.ToolDef{
			Name: "explore",
			Description: "Answer a BROAD, multi-step question about this codebase (\"where and how is " +
				"X handled?\", \"what's the shape of the Y layer and what touches Z?\") by delegating the " +
				"iterative search→read→refine to a fast read-only explorer, so you don't spend your own " +
				"context and turns navigating. Returns a distilled answer: a summary, file:line anchors you " +
				"should re-read at full fidelity before acting, a coverage flag, and leads. Use it to " +
				"localize before editing; use the single-shot read tools (find_symbol, read_file, search) " +
				"for a specific known lookup.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"question": {"type": "string", "description": "The broad, free-form question to investigate. State what you want to understand, not how to search for it."}
				},
				"required": ["question"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var in struct {
				Question string `json:"question"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad, nil
			}
			if strings.TrimSpace(in.Question) == "" {
				return invalid("question is required: state the broad question you want investigated"), nil
			}

			var sink exploreSink
			tools := make([]Tool, 0, len(readTools)+1)
			tools = append(tools, readTools...)
			tools = append(tools, answerTool(&sink))

			answer := runExplore(ctx, in.Question, projectMap, exp, tools, &sink, completer, log)
			log.InfoContext(ctx, "agent: explore call complete",
				"coverage", answer.Coverage, "anchors", len(answer.Anchors))
			// Non-terminal, never IsError: an explore is a pure accelerant. A degraded answer
			// (partial-uncertain / partial-budget) is honest signal the parent acts on, not a
			// tool failure — surfacing IsError would wrongly tell the model the tool broke.
			return Outcome{Content: renderExploreAnswer(answer)}, nil
		},
	}
}

// exploreSink captures the explorer's terminal answer from its `answer` tool. The sub-loop
// checks done after each dispatch and returns the captured answer — the two-rail termination
// (answer or budget) the main loop has, adapted to a value return instead of a core.Result.
type exploreSink struct {
	answer ExploreAnswer
	done   bool
}

// answerTool is the explorer's ONE lifecycle tool. It validates and records the structured
// answer into sink, then ends the sub-loop. It is the explorer's only way to terminate
// successfully — the read-only allowlist has no submit/escalate — so the loop always ends on
// `answer` or on its budget. Invalid args (missing summary/anchors, bad coverage) come back
// as an IsError result the model corrects, exactly like any other tool's arg validation.
func answerTool(sink *exploreSink) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "answer",
			Description: "Return your distilled answer and end the exploration. Every claim in `summary` " +
				"must be grounded in an anchor (a file:line you actually read). Do not speculate past what " +
				"you read — an honest partial-uncertain beats a confident wrong anchor.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"summary": {"type": "string", "description": "A tight prose answer to the question. No preamble; just what is true and how the pieces fit."},
					"anchors": {
						"type": "array",
						"description": "The file:line locations that back the summary. Required for a confident answer. Pointers, not snippets.",
						"items": {
							"type": "object",
							"properties": {
								"path": {"type": "string", "description": "Repo-relative file path."},
								"line": {"type": "integer", "description": "1-based line the claim anchors to."},
								"why": {"type": "string", "description": "One line on why this location matters."}
							},
							"required": ["path", "why"]
						}
					},
					"coverage": {"type": "string", "enum": ["complete", "partial-budget", "partial-uncertain"], "description": "How far you got: complete, partial-budget (ran out of budget), or partial-uncertain (could not resolve confidently)."},
					"leads": {"type": "array", "items": {"type": "string"}, "description": "Threads you saw but did not follow, so the caller can re-ask narrower."}
				},
				"required": ["summary", "coverage"]
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a ExploreAnswer
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if strings.TrimSpace(a.Summary) == "" {
				return invalid("summary is required"), nil
			}
			switch a.Coverage {
			case CoverageComplete, CoveragePartialBudget, CoveragePartialUncertain:
			default:
				return invalid("coverage must be one of: complete, partial-budget, partial-uncertain"), nil
			}
			// A confident answer must be grounded; a partial-uncertain may legitimately have no
			// anchor yet (that is what makes it uncertain). This is the anti-hallucination rail:
			// claim `complete`, and you must point at code that backs it.
			if a.Coverage == CoverageComplete && len(a.Anchors) == 0 {
				return invalid("a complete answer must include at least one anchor grounding the summary; " +
					"if you cannot anchor it, use coverage partial-uncertain"), nil
			}
			for i, an := range a.Anchors {
				if strings.TrimSpace(an.Path) == "" {
					return invalid(fmt.Sprintf("anchor %d is missing a path", i+1)), nil
				}
			}
			sink.answer = a
			sink.done = true
			return Outcome{Content: "answer recorded"}, nil
		},
	}
}

// runExplore is the nested read-only ReAct sub-loop: it drives the explorer soul over the
// question until it calls `answer` or exhausts its fixed sub-budget, then returns the distilled
// answer. It mirrors the main loop's request→complete→dispatch→append shape but terminates on a
// value (the answer) instead of a core.Result, and — crucially — NEVER surfaces an error to the
// parent: a model error or an exhausted budget degrades to a partial answer, because explore is
// additive and must never fail the parent task.
func runExplore(ctx context.Context, question, projectMap string, exp Explorer, tools []Tool, sink *exploreSink, completer Completer, log *slog.Logger) ExploreAnswer {
	byName := make(map[string]Tool, len(tools))
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		d := t.Def()
		byName[d.Name] = t
		defs = append(defs, d)
	}

	messages := []model.Message{{Role: model.RoleUser, Text: buildExploreContext(question, projectMap)}}
	var total model.Usage

	maxTurns := exp.Budget.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultExploreMaxTurns
	}

	for turn := 1; turn <= maxTurns; turn++ {
		req := model.Request{
			System:    exp.Persona,
			Messages:  messages,
			Tools:     defs,
			MaxTokens: exp.Budget.MaxOutputTokens,
		}
		resp, err := completer.Complete(ctx, req)
		if err != nil {
			log.WarnContext(ctx, "agent: explore model call failed, returning partial", "turn", turn, "err", err)
			return partialAnswer(CoveragePartialUncertain,
				"The explorer could not complete (model call failed); investigate directly with the read tools.")
		}

		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens

		messages = append(messages, model.Message{
			Role:      model.RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if exp.Budget.MaxTokens > 0 && total.InputTokens+total.OutputTokens >= exp.Budget.MaxTokens {
			log.WarnContext(ctx, "agent: explore token budget exhausted", "turn", turn,
				"input_tokens", total.InputTokens, "output_tokens", total.OutputTokens, "cap", exp.Budget.MaxTokens)
			return partialAnswer(CoveragePartialBudget,
				"The explorer ran out of its token budget before answering; narrow the question or read directly.")
		}

		if len(resp.ToolCalls) == 0 {
			messages = append(messages, model.Message{Role: model.RoleUser, Text: exploreNudge})
			continue
		}

		results := make([]model.ToolResult, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tool, ok := byName[tc.Name]
			if !ok {
				results = append(results, model.ToolResult{
					ToolCallID: tc.ID,
					Content:    fmt.Sprintf("unknown tool %q; available tools: %s", tc.Name, strings.Join(toolNames(defs), ", ")),
					IsError:    true,
				})
				continue
			}
			out, err := tool.Invoke(ctx, tc.Args)
			if err != nil {
				// A read tool erroring hard is not fatal to the parent — degrade to a partial and
				// let the parent read directly rather than propagating up and killing its task.
				log.WarnContext(ctx, "agent: explore tool errored, returning partial", "tool", tc.Name, "err", err)
				return partialAnswer(CoveragePartialUncertain,
					fmt.Sprintf("The explorer hit an error running %q; investigate directly with the read tools.", tc.Name))
			}
			// The `answer` tool records into sink and marks it done; that is the sub-loop's success
			// rail (the read-only allowlist has no other terminal). Check it right after dispatch so
			// a trailing read tool in the same turn cannot bury a completed answer.
			if sink.done {
				return sink.answer
			}
			results = append(results, model.ToolResult{ToolCallID: tc.ID, Content: out.Content, IsError: out.IsError})
		}
		messages = append(messages, model.Message{Role: model.RoleTool, ToolResults: results})
	}

	log.WarnContext(ctx, "agent: explore turn budget exhausted", "max_turns", maxTurns)
	return partialAnswer(CoveragePartialBudget,
		"The explorer ran out of its turn budget before answering; narrow the question or read directly.")
}

// partialAnswer builds a degraded answer for a budget/error path. It has no anchors — it is an
// infra-generated signal that the explorer did not finish, so the parent falls back to reading
// directly. The coverage flag is what the parent branches on.
func partialAnswer(coverage, summary string) ExploreAnswer {
	return ExploreAnswer{Summary: summary, Coverage: coverage}
}

// exploreNudge steers an explorer that stopped without calling a tool back to work. Unlike the
// main loop's nudge it points at `answer` (its only terminal) rather than submit/escalate.
const exploreNudge = "You did not call a tool. Keep using the read tools to investigate, or call " +
	"`answer` once you can ground a response in the code you have read."

// buildExploreContext renders the child's opening turn: the question plus the project map
// (ambient specs), and NOT the parent's conversation (specs/components/agent.md — that would
// defeat the point). It is the explorer's entire knowledge of the world beyond the worktree it
// can read.
func buildExploreContext(question, projectMap string) string {
	var b strings.Builder
	b.WriteString("# Question\n")
	b.WriteString(question)
	b.WriteString("\n")
	if strings.TrimSpace(projectMap) != "" {
		b.WriteString("\n# Project map\n")
		b.WriteString(projectMap)
		b.WriteString("\n")
	}
	b.WriteString("\nInvestigate the question above by reading this codebase with the read-only tools. " +
		"Follow the thread toward what the question asks; you are on a fixed budget, so spend it on the " +
		"load-bearing files, not exhaustive coverage. When you can ground an answer, call `answer` with a " +
		"tight summary, the file:line anchors that back it, an honest coverage flag, and any leads you did " +
		"not follow.\n")
	return b.String()
}

// renderExploreAnswer serializes the distilled answer into the compact text the parent gets as
// the tool result. It leads with the coverage flag so the parent weighs a partial correctly,
// and lists anchors as bare file:line pointers (never snippets) — re-inflating the context with
// pasted source is exactly what explore exists to avoid.
func renderExploreAnswer(a ExploreAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "coverage: %s\n\n", a.Coverage)
	b.WriteString(a.Summary)
	b.WriteString("\n")
	if len(a.Anchors) > 0 {
		b.WriteString("\nAnchors (re-read these at full fidelity before acting):\n")
		for _, an := range a.Anchors {
			loc := an.Path
			if an.Line > 0 {
				loc = fmt.Sprintf("%s:%d", an.Path, an.Line)
			}
			if strings.TrimSpace(an.Why) != "" {
				fmt.Fprintf(&b, "- %s — %s\n", loc, an.Why)
			} else {
				fmt.Fprintf(&b, "- %s\n", loc)
			}
		}
	}
	if len(a.Leads) > 0 {
		b.WriteString("\nLeads (not followed):\n")
		for _, l := range a.Leads {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	return b.String()
}
