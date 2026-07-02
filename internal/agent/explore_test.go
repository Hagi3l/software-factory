package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model"
)

// TestReadOnlyToolsMatchConfigNames is the anti-drift guard between the runtime read-only tool
// set (agent.ReadOnlyTools) and the list config validation uses to check an explorer soul's
// allowlist (config.ReadOnlyToolNames). config cannot import agent, so the two are separate
// lists; if a read tool is added/renamed on one side without the other, this fails — which is
// what keeps the explorer-soul validation honest about what the sub-loop can actually run.
func TestReadOnlyToolsMatchConfigNames(t *testing.T) {
	var runtime []string
	for _, tl := range ReadOnlyTools(fakeSandbox{}, NewSessions(nil, nil)) {
		runtime = append(runtime, tl.Def().Name)
	}
	want := append([]string(nil), config.ReadOnlyToolNames...)
	sort.Strings(runtime)
	sort.Strings(want)
	if strings.Join(runtime, ",") != strings.Join(want, ",") {
		t.Errorf("ReadOnlyTools names %v != config.ReadOnlyToolNames %v (they must not drift)", runtime, want)
	}
}

// explorerPersona is the fake system prompt the sub-loop should carry as Request.System.
const explorerPersona = "you are a fast read-only explorer"

// answerCall builds an `answer` tool call with the given coverage and one anchor.
func answerCall(id, summary, coverage string) model.ToolCall {
	args := fmt.Sprintf(`{"summary":%q,"anchors":[{"path":"foo.go","line":10,"why":"the frobnicate call"}],"coverage":%q,"leads":["check bar.go"]}`,
		summary, coverage)
	return toolCall(id, "answer", args)
}

// newExplore builds an explore tool over the fake sandbox + a no-LSP session manager, driven
// by the scripted conn. It is the parent-facing tool the main loop would advertise.
func newExplore(exp Explorer, conn *fakeConn) Tool {
	return ExploreTool(exp, fakeSandbox{}, NewSessions(nil, nil), conn, "the project map", nil)
}

func invokeExplore(t *testing.T, tool Tool, question string) Outcome {
	t.Helper()
	out, err := tool.Invoke(context.Background(), json.RawMessage(fmt.Sprintf(`{"question":%q}`, question)))
	if err != nil {
		t.Fatalf("explore Invoke returned a fatal error (explore must never fail the parent): %v", err)
	}
	return out
}

// A normal explore: the child reads, then calls answer; the parent gets the distilled result.
// The sub-loop carries the explorer persona as System and the question (not the parent's
// conversation) as the opening turn. The explore tool is non-terminal and not an error.
func TestExploreReturnsDistilledAnswer(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "read_file", `{"path":"foo.go"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{answerCall("c2", "the widget frobnicates in foo.go", CoverageComplete)}},
	}}
	tool := newExplore(Explorer{Persona: explorerPersona}, conn)

	out := invokeExplore(t, tool, "where is the widget frobnicated?")

	if out.Result != nil {
		t.Errorf("explore must be non-terminal (Result nil), got %+v", out.Result)
	}
	if out.IsError {
		t.Errorf("a successful explore must not report IsError")
	}
	for _, want := range []string{"coverage: complete", "the widget frobnicates in foo.go", "foo.go:10 — the frobnicate call", "check bar.go"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("rendered answer missing %q; got:\n%s", want, out.Content)
		}
	}

	reqs := conn.requests()
	if len(reqs) != 2 {
		t.Fatalf("sub-loop made %d model calls, want 2", len(reqs))
	}
	if reqs[0].System != explorerPersona {
		t.Errorf("sub-loop System = %q, want the explorer persona", reqs[0].System)
	}
	if !strings.Contains(reqs[0].Messages[0].Text, "where is the widget frobnicated?") {
		t.Errorf("sub-loop opening turn should carry the question; got:\n%s", reqs[0].Messages[0].Text)
	}
	if !strings.Contains(reqs[0].Messages[0].Text, "the project map") {
		t.Errorf("sub-loop opening turn should carry the project map (ambient specs)")
	}
}

// The explorer's toolset is exactly the read-only comprehension subset plus `answer` — no
// writers, no lifecycle tools, and NOT `explore` itself (no recursion). This is the structural
// enforcement of the five explore rules.
func TestExploreReadOnlyToolset(t *testing.T) {
	got := map[string]bool{}
	for _, tl := range ReadOnlyTools(fakeSandbox{}, NewSessions(nil, nil)) {
		got[tl.Def().Name] = true
	}
	want := []string{"read_file", "list_dir", "search", "find_symbol", "references", "definition", "implementation", "hover", "diagnostics"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("ReadOnlyTools missing read tool %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("ReadOnlyTools has %d tools, want %d (%v)", len(got), len(want), got)
	}
	// Never a writer, a self-check, a lifecycle terminal, or explore itself.
	for _, forbidden := range []string{"write_file", "edit_file", "run", "rename", "code_action", "run_tests", "run_gate", "submit", "submit_plan", "escalate", "request_subtask", "explore"} {
		if got[forbidden] {
			t.Errorf("ReadOnlyTools leaked forbidden tool %q", forbidden)
		}
	}
}

// A child that calls a tool outside its allowlist (a writer, or `explore` itself) gets an
// "unknown tool" result — proof the write/recursion exclusion is enforced at dispatch, not just
// advertised. It then recovers by calling answer.
func TestExploreRejectsForbiddenTools(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "write_file", `{"path":"x","content":"y"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c2", "explore", `{"question":"recurse"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{answerCall("c3", "answered after retries", CoverageComplete)}},
	}}
	tool := newExplore(Explorer{Persona: explorerPersona}, conn)

	out := invokeExplore(t, tool, "q")
	if !strings.Contains(out.Content, "answered after retries") {
		t.Fatalf("explorer should recover and answer; got:\n%s", out.Content)
	}

	// The tool-result turns fed back to the child must have flagged both forbidden calls.
	reqs := conn.requests()
	if len(reqs) != 3 {
		t.Fatalf("want 3 model calls (write rejected, explore rejected, answer), got %d", len(reqs))
	}
	assertToolResultError(t, reqs[1], "write_file")
	assertToolResultError(t, reqs[2], "explore")
}

// assertToolResultError checks that the last message of req is a tool-result turn whose content
// reports an unknown tool of the given name.
func assertToolResultError(t *testing.T, req model.Request, toolName string) {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != model.RoleTool || len(last.ToolResults) == 0 {
		t.Fatalf("expected a tool-result turn before the %q rejection, got role %q", toolName, last.Role)
	}
	tr := last.ToolResults[0]
	if !tr.IsError || !strings.Contains(tr.Content, "unknown tool") || !strings.Contains(tr.Content, toolName) {
		t.Errorf("expected unknown-tool error for %q, got %+v", toolName, tr)
	}
}

// A child that never answers is capped by its turn budget and returns partial-budget — never a
// failed parent. The fixed sub-budget is the fan-out backstop.
func TestExploreTurnBudgetExhausted(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "read_file", `{"path":"a"}`)}},
	}} // repeats the last response forever — the child keeps reading, never answers
	tool := newExplore(Explorer{Persona: explorerPersona, Budget: Budget{MaxTurns: 3}}, conn)

	out := invokeExplore(t, tool, "q")
	if out.Result != nil || out.IsError {
		t.Errorf("budget exhaustion must degrade, not fail the parent: %+v", out)
	}
	if !strings.Contains(out.Content, "coverage: "+CoveragePartialBudget) {
		t.Errorf("want partial-budget coverage, got:\n%s", out.Content)
	}
	if n := len(conn.requests()); n != 3 {
		t.Errorf("turn budget of 3 should cap at 3 model calls, got %d", n)
	}
}

// The token dimension of the sub-budget also caps the loop and degrades to partial-budget.
func TestExploreTokenBudgetExhausted(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, Usage: model.Usage{InputTokens: 80, OutputTokens: 40},
			ToolCalls: []model.ToolCall{toolCall("c1", "read_file", `{"path":"a"}`)}},
	}}
	tool := newExplore(Explorer{Persona: explorerPersona, Budget: Budget{MaxTurns: 10, MaxTokens: 100}}, conn)

	out := invokeExplore(t, tool, "q")
	if !strings.Contains(out.Content, "coverage: "+CoveragePartialBudget) {
		t.Errorf("want partial-budget on token exhaustion, got:\n%s", out.Content)
	}
	if n := len(conn.requests()); n != 1 {
		t.Errorf("120 tokens over a 100 cap should stop after 1 call, got %d", n)
	}
}

// A model error inside the sub-loop degrades to partial-uncertain and is swallowed — the parent
// invocation is never killed by an explore failure (explore is additive, never load-bearing).
func TestExploreModelErrorDegrades(t *testing.T) {
	conn := &fakeConn{completeErr: errors.New("broker down")}
	tool := newExplore(Explorer{Persona: explorerPersona}, conn)

	out := invokeExplore(t, tool, "q") // invokeExplore fatals if Invoke returns a Go error
	if !strings.Contains(out.Content, "coverage: "+CoveragePartialUncertain) {
		t.Errorf("want partial-uncertain on model error, got:\n%s", out.Content)
	}
}

// An invalid `answer` (missing summary, bad coverage, or a `complete` with no anchors) comes
// back as an IsError result the child corrects on the next turn, exactly like any tool's arg
// validation — the loop does not terminate on a malformed answer.
func TestExploreInvalidAnswerRetries(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "answer", `{"summary":"","coverage":"complete"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c2", "answer", `{"summary":"grounded","anchors":[{"path":"foo.go","line":1,"why":"here"}],"coverage":"bogus"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c3", "answer", `{"summary":"claims complete but no anchors","coverage":"complete"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{answerCall("c4", "finally grounded", CoverageComplete)}},
	}}
	tool := newExplore(Explorer{Persona: explorerPersona, Budget: Budget{MaxTurns: 10}}, conn)

	out := invokeExplore(t, tool, "q")
	if !strings.Contains(out.Content, "finally grounded") {
		t.Fatalf("child should recover after invalid answers; got:\n%s", out.Content)
	}
	if n := len(conn.requests()); n != 4 {
		t.Errorf("want 4 model calls (3 rejected answers + 1 accepted), got %d", n)
	}
}

// The `explore` tool advertises a `question` param and rejects an empty question with a
// recoverable IsError (not a fatal loop error).
func TestExploreToolDefAndEmptyQuestion(t *testing.T) {
	tool := newExplore(Explorer{Persona: explorerPersona}, &fakeConn{})
	def := tool.Def()
	if def.Name != "explore" {
		t.Errorf("tool name = %q, want explore", def.Name)
	}
	if !strings.Contains(string(def.Params), "question") {
		t.Errorf("explore params should declare a question field: %s", def.Params)
	}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"question":"   "}`))
	if err != nil {
		t.Fatalf("empty question must be recoverable, not fatal: %v", err)
	}
	if !out.IsError {
		t.Errorf("empty question should return IsError")
	}
}
