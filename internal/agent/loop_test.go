package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/runner"
	"github.com/Loxstomper/software-factory/internal/sandbox"
	"github.com/Loxstomper/software-factory/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The Loop must satisfy the runner's Invoker seam (signature incl. the broker endpoint).
var _ runner.Invoker = (*Loop)(nil)

// --- fakes -------------------------------------------------------------------

// fakeConn is the brokered connection: it scripts model responses and records the
// requests the loop sent, plus any git push / event calls the tools made.
type fakeConn struct {
	mu          sync.Mutex
	responses   []model.Response // returned in order; the last repeats once exhausted
	completeErr error
	gotReqs     []model.Request
	calls       int
}

func (c *fakeConn) Complete(_ context.Context, req model.Request) (model.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gotReqs = append(c.gotReqs, req)
	if c.completeErr != nil {
		return model.Response{}, c.completeErr
	}
	i := c.calls
	c.calls++
	if i >= len(c.responses) {
		i = len(c.responses) - 1
	}
	return c.responses[i], nil
}

func (c *fakeConn) GitPush(context.Context, broker.GitPushRequest) (broker.GitPushResult, error) {
	return broker.GitPushResult{}, nil
}
func (c *fakeConn) PublishEvent(context.Context, broker.PublishRequest) error { return nil }

func (c *fakeConn) requests() []model.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.Request(nil), c.gotReqs...)
}

// fakeTool records the args it was invoked with and returns a configured Outcome.
type fakeTool struct {
	name    string
	outcome Outcome
	err     error
	mu      sync.Mutex
	gotArgs []json.RawMessage
}

func (t *fakeTool) Def() model.ToolDef {
	return model.ToolDef{Name: t.name, Description: "fake " + t.name, Params: json.RawMessage(`{"type":"object"}`)}
}

func (t *fakeTool) Invoke(_ context.Context, args json.RawMessage) (Outcome, error) {
	t.mu.Lock()
	t.gotArgs = append(t.gotArgs, args)
	t.mu.Unlock()
	if t.err != nil {
		return Outcome{}, t.err
	}
	return t.outcome, nil
}

func (t *fakeTool) args() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]json.RawMessage(nil), t.gotArgs...)
}

type fakeSandbox struct{}

func (fakeSandbox) ID() string { return "sb-test" }
func (fakeSandbox) Exec(context.Context, sandbox.Command) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (fakeSandbox) Teardown(context.Context) error { return nil }

// newLoop wires a Loop with the fake broker conn and tools, bypassing the persona file
// read and the real socket dial.
func newLoop(t *testing.T, budget Budget, conn *fakeConn, tools ...Tool) *Loop {
	t.Helper()
	l := New(func(Invocation) ([]Tool, func(), error) { return tools, nil, nil }, budget, nil)
	l.readPersona = func(string) ([]byte, error) { return []byte("you are a helpful implementor"), nil }
	l.connect = func(sandbox.Endpoint) brokerConn { return conn }
	return l
}

func testBrief() core.Brief {
	return core.Brief{
		Issue:    core.Issue{ID: "iss-1", Title: "add widget", Body: "build the widget", Role: "implement"},
		Spec:     "the widget must frobnicate",
		Base:     "main",
		Criteria: []string{"tests pass", "no lint errors"},
		Soul:     core.Soul{Name: "implementor-go", Model: "claude", Persona: "/souls/impl.md"},
	}
}

func toolCall(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
}

func run(t *testing.T, l *Loop, brief core.Brief) (core.Result, error) {
	t.Helper()
	return l.Invoke(context.Background(), fakeSandbox{}, brief, sandbox.Endpoint{Network: "unix", Address: "/x"})
}

// --- tests -------------------------------------------------------------------

// TestToolResultAgingOnTheWire proves the model receives the AGED view of the history
// while the loop's own messages stay pristine (specs/components/agent.md "Tool-result
// aging"). The run makes 17 big read_file rounds then submits; at 16 accumulated rounds
// the boundary quantizes to 8, so turn 17's request must carry stubs for rounds 1-8 and
// full content for the keep window — and turn 18's request must render the aged region
// byte-identically (batch cadence: no per-turn re-editing, the cache-stability property).
// Earlier captured requests (which alias the loop's backing array in fakeConn) must keep
// their full content — the proof agedView copies rather than mutates.
func TestToolResultAgingOnTheWire(t *testing.T) {
	big := strings.Repeat("y", 2*elideMinBytes)
	var responses []model.Response
	for r := 1; r <= 17; r++ {
		responses = append(responses, model.Response{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{
			toolCall(fmt.Sprintf("c%d", r), "read_file", `{"path":"foo.go"}`),
		}})
	}
	responses = append(responses, model.Response{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{
		toolCall("s1", "submit", `{}`),
	}})
	conn := &fakeConn{responses: responses}
	read := &fakeTool{name: "read_file", outcome: Outcome{Content: big}}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &core.Result{Status: core.StatusDone}}}
	l := newLoop(t, Budget{}, conn, read, submit)

	if _, err := run(t, l, testBrief()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	reqs := conn.requests()
	if len(reqs) != 18 {
		t.Fatalf("model calls = %d, want 18", len(reqs))
	}

	// Round r's tool message sits at index 2r (brief at 0, round r = assistant 2r-1, tool 2r).
	toolContent := func(req model.Request, r int) string {
		t.Helper()
		m := req.Messages[2*r]
		if m.Role != model.RoleTool || len(m.ToolResults) != 1 {
			t.Fatalf("round %d: message at %d is not the tool answer: %+v", r, 2*r, m)
		}
		return m.ToolResults[0].Content
	}

	// Turn 16 (15 rounds): below the threshold, everything verbatim.
	if got := toolContent(reqs[15], 1); got != big {
		t.Errorf("turn 16 round-1 content aged too early: %q", got[:64])
	}
	// Turn 17 (16 rounds): boundary 8 — the first batch stubbed, the keep window intact,
	// the Brief and assistant messages untouched.
	if got := toolContent(reqs[16], 1); !strings.Contains(got, "elided") || !strings.Contains(got, "read_file") {
		t.Errorf("turn 17 round-1 content = %q, want the elision stub naming the call", got)
	}
	if got := toolContent(reqs[16], 9); got != big {
		t.Error("turn 17 round-9 content aged; the keep window must stay verbatim")
	}
	if got := toolContent(reqs[16], 16); got != big {
		t.Error("turn 17 round-16 (first appearance) not full — the tail must always be pristine")
	}
	if reqs[16].Messages[0].Text != reqs[0].Messages[0].Text {
		t.Error("the Brief changed under aging")
	}
	// Turn 18 (17 rounds, boundary still 8): the aged region is byte-identical to turn 17.
	if a, b := toolContent(reqs[16], 1), toolContent(reqs[17], 1); a != b {
		t.Errorf("aged region re-edited between turns:\n%q\n%q", a, b)
	}
	// The earlier captured request still holds full content: agedView copied, never mutated.
	if got := toolContent(reqs[15], 1); got != big {
		t.Error("an earlier request's content changed after aging — the pristine history was mutated")
	}
}

// A submit tool terminates the loop with its Result, and the first request carries the
// persona as System, the Brief context as the opening user turn, and the tool defs.
func TestSubmitTerminatesAndFirstRequestShape(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "submit", `{}`)}},
	}}
	want := core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &want}}
	l := newLoop(t, Budget{}, conn, submit)

	got, err := run(t, l, testBrief())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Status != core.StatusDone || got.Branch.Ref != "candidate/iss-1" {
		t.Errorf("result = %+v, want done candidate/iss-1", got)
	}
	if len(submit.args()) != 1 {
		t.Fatalf("submit invoked %d times, want 1", len(submit.args()))
	}

	reqs := conn.requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(reqs))
	}
	r0 := reqs[0]
	if r0.System != "you are a helpful implementor" {
		t.Errorf("System = %q, want persona", r0.System)
	}
	if len(r0.Tools) != 1 || r0.Tools[0].Name != "submit" {
		t.Errorf("Tools = %+v, want [submit]", r0.Tools)
	}
	if len(r0.Messages) != 1 || r0.Messages[0].Role != model.RoleUser {
		t.Fatalf("Messages = %+v, want one user turn", r0.Messages)
	}
	ctx := r0.Messages[0].Text
	for _, want := range []string{"iss-1", "add widget", "build the widget", "main", "frobnicate", "tests pass"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q:\n%s", want, ctx)
		}
	}
}

// TestBuildContextSurfacesIntegrationBranch checks the opening user turn names the branch the
// merge queue will land the candidate on — the rebase target the merge-resolver soul reads (T7.3).
// It defaults to "main" when unset (per-item / a pre-epic brief) and shows the epic branch when
// the orchestrator set IntegrationBase.
func TestBuildContextSurfacesIntegrationBranch(t *testing.T) {
	def := buildContext(testBrief()) // IntegrationBase unset → defaults to main
	if !strings.Contains(def, "# Integration branch") || !strings.Contains(def, "main") {
		t.Errorf("default context missing the integration-branch section:\n%s", def)
	}

	b := testBrief()
	b.IntegrationBase = "epic/feat-1"
	got := buildContext(b)
	if !strings.Contains(got, "# Integration branch") || !strings.Contains(got, "epic/feat-1") {
		t.Errorf("context did not surface the epic integration branch:\n%s", got)
	}
}

// A non-terminal tool's result is appended as a RoleTool message and the loop continues;
// the second request carries the assistant tool-call turn and the tool result.
func TestToolResultAppendedAndLoopContinues(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, Text: "reading", ToolCalls: []model.ToolCall{toolCall("c1", "read_file", `{"path":"a.go"}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c2", "submit", `{}`)}},
	}}
	read := &fakeTool{name: "read_file", outcome: Outcome{Content: "package main"}}
	done := core.Result{Status: core.StatusDone}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &done}}
	l := newLoop(t, Budget{}, conn, read, submit)

	if _, err := run(t, l, testBrief()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(read.args()[0]) != `{"path":"a.go"}` {
		t.Errorf("read_file args = %s, want path a.go", read.args()[0])
	}

	reqs := conn.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	msgs := reqs[1].Messages // user, assistant(tool_call), tool(result)
	if len(msgs) != 3 {
		t.Fatalf("second request messages = %d, want 3:\n%+v", len(msgs), msgs)
	}
	if msgs[1].Role != model.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[1].Text != "reading" {
		t.Errorf("assistant turn = %+v", msgs[1])
	}
	if msgs[2].Role != model.RoleTool || len(msgs[2].ToolResults) != 1 {
		t.Fatalf("tool turn = %+v", msgs[2])
	}
	tr := msgs[2].ToolResults[0]
	if tr.ToolCallID != "c1" || tr.Content != "package main" || tr.IsError {
		t.Errorf("tool result = %+v, want c1/package main/ok", tr)
	}
}

// An unknown tool name yields an IsError tool result (not a crash) and the loop continues.
func TestUnknownToolReportsError(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "bogus", `{}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c2", "submit", `{}`)}},
	}}
	done := core.Result{Status: core.StatusDone}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &done}}
	l := newLoop(t, Budget{}, conn, submit)

	if _, err := run(t, l, testBrief()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	reqs := conn.requests()
	tr := reqs[1].Messages[2].ToolResults[0]
	if !tr.IsError || !strings.Contains(tr.Content, "unknown tool") || tr.ToolCallID != "c1" {
		t.Errorf("unknown-tool result = %+v, want IsError unknown tool c1", tr)
	}
}

// An escalate tool terminates with needs-spec-clarification.
func TestEscalateTerminates(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "escalate", `{}`)}},
	}}
	esc := core.Result{Status: core.StatusNeedsSpecClarification}
	tool := &fakeTool{name: "escalate", outcome: Outcome{Result: &esc}}
	l := newLoop(t, Budget{}, conn, tool)

	got, err := run(t, l, testBrief())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Status != core.StatusNeedsSpecClarification {
		t.Errorf("status = %q, want needs-spec-clarification", got.Status)
	}
}

// A model that never calls a tool is nudged and bounded by the turn cap, returning failed.
func TestTurnBudgetExhausted(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopEndTurn, Text: "I am thinking"},
	}}
	l := newLoop(t, Budget{MaxTurns: 3}, conn)

	got, err := run(t, l, testBrief())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Status != core.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	reqs := conn.requests()
	if len(reqs) != 3 {
		t.Fatalf("model calls = %d, want exactly MaxTurns=3", len(reqs))
	}
	// The second request must carry the nudge appended after the first tool-less turn.
	if len(reqs[1].Messages) < 3 || !strings.Contains(reqs[1].Messages[2].Text, "did not call a tool") {
		t.Errorf("expected a nudge user turn in the second request, got %+v", reqs[1].Messages)
	}
}

// A per-soul MaxTurns (core.Soul.MaxTurns, yaml max_tool_turns) caps the loop below its own
// budget for that invocation — the operator's per-invocation turn knob for a flail-prone soul.
func TestSoulMaxTurnsOverridesBudget(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopEndTurn, Text: "still thinking"},
	}}
	// The loop budget is generous (50); the soul tightens it to 4, so the loop must stop at 4,
	// not 50. This is the fast-fail backstop for a soul that fills turns without submitting.
	l := newLoop(t, Budget{MaxTurns: 50}, conn)
	brief := testBrief()
	brief.Soul.MaxTurns = 4

	got, err := run(t, l, brief)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Status != core.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if reqs := conn.requests(); len(reqs) != 4 {
		t.Fatalf("model calls = %d, want exactly the soul's MaxTurns=4 (not the loop budget 50)", len(reqs))
	}
}

// The cumulative input+output token cap stops the loop with failed.
func TestTokenBudgetExhausted(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, Usage: model.Usage{InputTokens: 40, OutputTokens: 20},
			ToolCalls: []model.ToolCall{toolCall("c1", "noop", `{}`)}},
	}}
	noop := &fakeTool{name: "noop", outcome: Outcome{Content: "ok"}}
	l := newLoop(t, Budget{MaxTurns: 100, MaxTokens: 100}, conn, noop)

	got, err := run(t, l, testBrief())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.Status != core.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	// 60 tokens/turn, cap 100 → breach after the 2nd turn's tally.
	if n := len(conn.requests()); n != 2 {
		t.Errorf("model calls = %d, want 2 before token breach", n)
	}
}

// MaxOutputTokens is passed through as Request.MaxTokens.
func TestMaxOutputTokensPassedThrough(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "submit", `{}`)}},
	}}
	done := core.Result{Status: core.StatusDone}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &done}}
	l := newLoop(t, Budget{MaxOutputTokens: 4096}, conn, submit)

	if _, err := run(t, l, testBrief()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := conn.requests()[0].MaxTokens; got != 4096 {
		t.Errorf("Request.MaxTokens = %d, want 4096", got)
	}
}

// A broker completion error is fatal to the invocation (so the runner redelivers).
func TestModelCallErrorIsFatal(t *testing.T) {
	conn := &fakeConn{completeErr: errors.New("broker down")}
	l := newLoop(t, Budget{}, conn)

	if _, err := run(t, l, testBrief()); err == nil {
		t.Fatal("Invoke: want error from failed model call, got nil")
	}
}

// A tool that returns an error (catastrophic, not a tool-level failure) is fatal.
func TestToolErrorIsFatal(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "boom", `{}`)}},
	}}
	boom := &fakeTool{name: "boom", err: errors.New("sandbox dead")}
	l := newLoop(t, Budget{}, conn, boom)

	if _, err := run(t, l, testBrief()); err == nil {
		t.Fatal("Invoke: want error from failed tool, got nil")
	}
}

// A persona read failure is fatal, and an empty persona path is rejected up front.
func TestPersonaErrors(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{{Stop: model.StopEndTurn}}}
	l := newLoop(t, Budget{}, conn)
	l.readPersona = func(string) ([]byte, error) { return nil, errors.New("no file") }
	if _, err := run(t, l, testBrief()); err == nil {
		t.Fatal("want error when persona unreadable")
	}

	brief := testBrief()
	brief.Soul.Persona = ""
	if _, err := run(t, newLoop(t, Budget{}, conn), brief); err == nil {
		t.Fatal("want error when soul has no persona")
	}
}

// A ToolSource error is fatal.
func TestToolSourceErrorIsFatal(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{{Stop: model.StopEndTurn}}}
	l := New(func(Invocation) ([]Tool, func(), error) { return nil, nil, errors.New("bad tools") }, Budget{}, nil)
	l.readPersona = func(string) ([]byte, error) { return []byte("p"), nil }
	l.connect = func(sandbox.Endpoint) brokerConn { return conn }
	if _, err := run(t, l, testBrief()); err == nil {
		t.Fatal("want error when tool source fails")
	}
}

// Every workspace/lifecycle tool the model drives is wrapped in a tool-call span parented
// to the invocation, so the in-sandbox tools (which run unbrokered) show up in the trace
// alongside the broker's egress spans. A tool that returns IsError is tagged, not failed.
func TestToolCallSpansEmitted(t *testing.T) {
	conn := &fakeConn{responses: []model.Response{
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c1", "read_file", `{}`)}},
		{Stop: model.StopToolUse, ToolCalls: []model.ToolCall{toolCall("c2", "submit", `{}`)}},
	}}
	read := &fakeTool{name: "read_file", outcome: Outcome{Content: "build failed", IsError: true}}
	done := core.Result{Status: core.StatusDone}
	submit := &fakeTool{name: "submit", outcome: Outcome{Result: &done}}

	sr := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)).Tracer("test")
	src := func(Invocation) ([]Tool, func(), error) { return []Tool{read, submit}, nil, nil }
	l := New(src, Budget{}, nil, WithTracer(tracer))
	l.readPersona = func(string) ([]byte, error) { return []byte("persona"), nil }
	l.connect = func(sandbox.Endpoint) brokerConn { return conn }

	if _, err := run(t, l, testBrief()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	byTool := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		if s.Name() != telemetry.SpanToolCall {
			continue
		}
		for _, a := range s.Attributes() {
			if string(a.Key) == telemetry.AttrToolName {
				byTool[a.Value.AsString()] = s
			}
		}
	}
	if _, ok := byTool["read_file"]; !ok {
		t.Fatalf("no tool-call span for read_file; got %v", spanKeys(byTool))
	}
	if _, ok := byTool["submit"]; !ok {
		t.Fatalf("no tool-call span for submit; got %v", spanKeys(byTool))
	}

	// The errored read carries the IsError tag; the successful submit does not.
	if !hasBoolAttr(byTool["read_file"], telemetry.AttrToolError, true) {
		t.Errorf("read_file span missing %s=true", telemetry.AttrToolError)
	}
	if hasBoolAttr(byTool["submit"], telemetry.AttrToolError, true) {
		t.Errorf("submit span should not be tagged as a tool error")
	}
}

func spanKeys(m map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasBoolAttr(s sdktrace.ReadOnlySpan, key string, want bool) bool {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsBool() == want
		}
	}
	return false
}
