package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// --- relay fakes -------------------------------------------------------------

// recordingAdapter streams the configured deltas, returns the configured response (or
// error), and counts calls so usage-tally accumulation across calls is observable.
// errs is a per-call error script consumed first (nil entry = that call succeeds),
// so retry tests express "fail twice, then succeed"; the plain err field keeps the
// original always-errors shape.
type recordingAdapter struct {
	deltas    []string
	reasoning []string
	resp      model.Response
	err       error
	errs      []error
	calls     int
}

func (a *recordingAdapter) Complete(_ context.Context, _ model.Request, onEvent model.StreamHandler) (model.Response, error) {
	a.calls++
	if len(a.errs) > 0 {
		e := a.errs[0]
		a.errs = a.errs[1:]
		if e != nil {
			return model.Response{}, e
		}
	} else if a.err != nil {
		return model.Response{}, a.err
	}
	for _, d := range a.deltas {
		if onEvent != nil {
			onEvent(model.StreamEvent{TextDelta: d})
		}
	}
	for _, d := range a.reasoning {
		if onEvent != nil {
			onEvent(model.StreamEvent{ReasoningDelta: d})
		}
	}
	return a.resp, nil
}

// recordingPublisher captures every published (subject, data) so token/event fan-out
// can be asserted.
type recordingPublisher struct {
	mu   sync.Mutex
	subj []string
	data [][]byte
	err  error
}

func (p *recordingPublisher) Publish(subject string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subj = append(p.subj, subject)
	p.data = append(p.data, append([]byte(nil), data...))
	return p.err
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subj)
}

// bundleSandbox records the Exec command and returns a canned ExecResult, standing in
// for the in-sandbox `git bundle` without a real container.
type bundleSandbox struct {
	gotCmd  sandbox.Command
	result  sandbox.ExecResult
	execErr error
}

func (s *bundleSandbox) ID() string { return "sb-relay" }
func (s *bundleSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.gotCmd = cmd
	return s.result, s.execErr
}
func (s *bundleSandbox) Teardown(context.Context) error { return nil }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// parentCall wraps a plain model request as a parent-sub-context completion — the shape the relay
// receives for the invocation's own soul stream (empty SubContext, no explorer stream).
func parentCall(req model.Request) broker.CompletionParams {
	return broker.CompletionParams{Request: req}
}

func testRelay(adapter model.Adapter, pub Publisher, sb sandbox.Sandbox) *relay {
	return newRelay(adapter, pub, sb, relayConfig{
		eventSubject:  "harness.agent.inv-1.events",
		issueID:       "iss-1",
		role:          "implementor",
		repo:          "/repo",
		allowedBranch: "candidate/iss-1",
		log:           discardLogger(),
	})
}

// decodeEvent unwraps one published agent-event wire payload: the issue/role-stamped
// envelope (core.AgentEventEnvelope) the relay publishes, returning the envelope and the
// opaque inner event bytes. Every published event funnels through the same envelope, so
// the tests assert the stamping here and decode the inner event from the returned payload.
func decodeEvent(t *testing.T, data []byte) (core.AgentEventEnvelope, []byte) {
	t.Helper()
	var env core.AgentEventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal event envelope: %v", err)
	}
	if env.IssueID != "iss-1" || env.Role != "implementor" {
		t.Errorf("envelope binding = {issue:%q role:%q}, want {iss-1 implementor}", env.IssueID, env.Role)
	}
	return env, env.Payload
}

// --- Complete ----------------------------------------------------------------

func TestRelayCompleteStreamsDeltasAndTalliesUsage(t *testing.T) {
	adapter := &recordingAdapter{
		deltas: []string{"hel", "lo"},
		resp:   model.Response{Text: "hello", Stop: model.StopEndTurn, Usage: model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2}},
	}
	pub := &recordingPublisher{}
	r := testRelay(adapter, pub, &bundleSandbox{})

	resp, err := r.Complete(context.Background(), parentCall(model.Request{}))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("resp.Text = %q, want hello", resp.Text)
	}

	// Each non-empty text delta is published to the invocation's event subject.
	if pub.count() != 2 {
		t.Fatalf("published events = %d, want 2", pub.count())
	}
	if pub.subj[0] != "harness.agent.inv-1.events" {
		t.Errorf("event subject = %q, want harness.agent.inv-1.events", pub.subj[0])
	}
	_, payload := decodeEvent(t, pub.data[0])
	var ev tokenEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Type != "token" || ev.Delta != "hel" {
		t.Errorf("event = %+v, want {token hel}", ev)
	}

	// A second completion accumulates onto the running usage tally (the budget input).
	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	u := r.Usage()
	if u.InputTokens != 20 || u.OutputTokens != 10 || u.CacheReadTokens != 4 {
		t.Errorf("tallied usage = %+v, want input=20 output=10 cacheRead=4", u)
	}
}

func TestRelayCompletePublishesReasoningAndToolEvents(t *testing.T) {
	adapter := &recordingAdapter{
		deltas:    []string{"sure"},
		reasoning: []string{"let me ", "think"},
		resp: model.Response{
			Stop: model.StopToolUse,
			ToolCalls: []model.ToolCall{
				{ID: "1", Name: "write_file", Args: json.RawMessage(`{"path":"index.html","content":"<html>"}`)},
				{ID: "2", Name: "run_tests"},
			},
		},
	}
	pub := &recordingPublisher{}
	r := testRelay(adapter, pub, &bundleSandbox{})

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Published in stream order: the text delta, then the two reasoning deltas, then one
	// tool row per tool call (emitted from the assembled response after the turn).
	var got []tokenEvent
	for _, d := range pub.data {
		_, payload := decodeEvent(t, d)
		var ev tokenEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		got = append(got, ev)
	}
	want := []tokenEvent{
		{Type: "token", Delta: "sure"},
		{Type: "reasoning", Delta: "let me "},
		{Type: "reasoning", Delta: "think"},
		{Type: "tool", Delta: "write_file index.html"},
		{Type: "tool", Delta: "run_tests"},
	}
	if len(got) != len(want) {
		t.Fatalf("published events = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRelayCompletePropagatesErrorAndDoesNotTally(t *testing.T) {
	adapter := &recordingAdapter{err: errors.New("model API 503")}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err == nil {
		t.Fatal("Complete: want error, got nil")
	}
	if u := r.Usage(); u != (model.Usage{}) {
		t.Errorf("usage = %+v, want zero (a failed call tallies nothing)", u)
	}
	// A bare (unclassified) error is terminal by contract — one attempt, no retry loop.
	if adapter.calls != 1 {
		t.Errorf("calls = %d, want 1 (an unclassified error must not be retried)", adapter.calls)
	}
}

// --- transient-fault retry (T14.2) --------------------------------------------

// transientFault builds a model.Fault the way an adapter would for a rate limit /
// 5xx / stream reset, optionally carrying the failed attempt's billed usage.
func transientFault(msg string, u model.Usage) error {
	return &model.Fault{Err: errors.New(msg), Transient: true, Usage: u}
}

// A transient fault is absorbed at the relay: the completion is re-issued with
// doubling backoff until it succeeds, and the caller sees only the final response.
func TestRelayCompleteRetriesTransientFaultThenSucceeds(t *testing.T) {
	adapter := &recordingAdapter{
		errs: []error{transientFault("429", model.Usage{}), transientFault("stream reset", model.Usage{})},
		resp: model.Response{Usage: model.Usage{InputTokens: 10, OutputTokens: 5}, Stop: model.StopEndTurn},
	}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})
	var slept []time.Duration
	r.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }

	resp, err := r.Complete(context.Background(), parentCall(model.Request{}))
	if err != nil {
		t.Fatalf("Complete after transient faults: %v", err)
	}
	if resp.Stop != model.StopEndTurn {
		t.Errorf("stop = %q, want end_turn", resp.Stop)
	}
	if adapter.calls != 3 {
		t.Errorf("calls = %d, want 3 (two transient faults + the success)", adapter.calls)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; len(slept) != 2 || slept[0] != want[0] || slept[1] != want[1] {
		t.Errorf("backoff = %v, want %v (doubling per attempt)", slept, want)
	}
	if u := r.Usage(); u != (model.Usage{InputTokens: 10, OutputTokens: 5}) {
		t.Errorf("usage = %+v, want the successful attempt only (faults carried no usage)", u)
	}
}

// A terminal fault — auth, malformed request, context overflow — is never retried.
func TestRelayCompleteTerminalFaultDoesNotRetry(t *testing.T) {
	adapter := &recordingAdapter{errs: []error{&model.Fault{Err: errors.New("401 unauthorized"), Transient: false}}}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})
	var slept int
	r.sleep = func(context.Context, time.Duration) error { slept++; return nil }

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err == nil {
		t.Fatal("Complete: want error, got nil")
	}
	if adapter.calls != 1 || slept != 0 {
		t.Errorf("calls = %d slept = %d, want 1 and 0 (terminal faults fail immediately)", adapter.calls, slept)
	}
}

// A persistent transient fault exhausts the attempt bound and surfaces — bounded
// attempts are the halting guarantee (the invocation ctx carries no deadline).
func TestRelayCompleteRetryIsBounded(t *testing.T) {
	adapter := &recordingAdapter{err: transientFault("provider outage", model.Usage{})}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})
	var slept int
	r.sleep = func(context.Context, time.Duration) error { slept++; return nil }

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err == nil {
		t.Fatal("Complete: want error after exhausting retries, got nil")
	}
	if adapter.calls != completionMaxAttempts {
		t.Errorf("calls = %d, want %d (the attempt bound)", adapter.calls, completionMaxAttempts)
	}
	if slept != completionMaxAttempts-1 {
		t.Errorf("slept %d times, want %d (no sleep after the final attempt)", slept, completionMaxAttempts-1)
	}
}

// A failed attempt's billed usage (a mid-stream fault lands after tokens were counted)
// still draws the invocation budget, so retries stay inside the termination guarantee.
func TestRelayCompleteFailedAttemptUsageDrawsBudget(t *testing.T) {
	adapter := &recordingAdapter{
		errs: []error{transientFault("mid-stream reset", model.Usage{InputTokens: 100, OutputTokens: 2})},
		resp: model.Response{Usage: model.Usage{InputTokens: 10, OutputTokens: 5}, Stop: model.StopEndTurn},
	}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})
	r.sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u := r.Usage(); u != (model.Usage{InputTokens: 110, OutputTokens: 7}) {
		t.Errorf("usage = %+v, want failed + successful attempts combined {110 7}", u)
	}
}

// The explore path retries too, and a failed explorer attempt's usage draws BOTH the
// combined ceiling and the per-call stream meter (the sub-budget stays honest).
func TestRelayExploreRetryDrawsSubBudget(t *testing.T) {
	parent := &recordingAdapter{}
	explore := &recordingAdapter{
		errs: []error{transientFault("overloaded", model.Usage{InputTokens: 60})},
		resp: model.Response{Usage: model.Usage{InputTokens: 30, OutputTokens: 5}, Stop: model.StopEndTurn},
	}
	r := testRelayWithExplore(parent, explore, core.ExploreBudget{Tokens: 1000})
	r.sleep = func(context.Context, time.Duration) error { return nil }

	if _, err := r.Complete(context.Background(), exploreCall("s1", model.Request{})); err != nil {
		t.Fatalf("explore Complete: %v", err)
	}
	if explore.calls != 2 {
		t.Errorf("explorer calls = %d, want 2 (one transient fault + the success)", explore.calls)
	}
	if got := r.streamTokens("s1"); got != 95 {
		t.Errorf("stream meter = %d, want 95 (60 failed + 35 successful)", got)
	}
	if u := r.Usage(); u != (model.Usage{InputTokens: 90, OutputTokens: 5}) {
		t.Errorf("combined usage = %+v, want {90 5}", u)
	}
}

// When the invocation's ctx is already gone, the backoff sleep returns immediately and
// the provider fault surfaces after the in-flight attempt — no retry loop outlives the
// invocation. Uses the real sleepCtx default to prove the ctx race, not a stub.
func TestRelayCompleteRetryStopsOnCancelledContext(t *testing.T) {
	adapter := &recordingAdapter{err: transientFault("429", model.Usage{})}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Complete(ctx, parentCall(model.Request{}))
	if err == nil {
		t.Fatal("Complete: want error, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want the provider fault (what the caller can act on), not the sleep interruption", err)
	}
	if adapter.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry once the ctx is gone)", adapter.calls)
	}
}

// testRelayWithExplore builds a relay pinned to a second (explorer) adapter + sub-budget, the
// two-models-in-one-sandbox shape the runner assembles when the trusted dispatch attached an
// explorer soul (T12.2).
func testRelayWithExplore(parent, explore model.Adapter, budget core.ExploreBudget) *relay {
	return newRelay(parent, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject:   "harness.agent.inv-1.events",
		issueID:        "iss-1",
		role:           "implementor",
		repo:           "/repo",
		allowedBranch:  "candidate/iss-1",
		log:            discardLogger(),
		model:          "frontier",
		exploreAdapter: explore,
		exploreModel:   "cheap",
		exploreBudget:  budget,
	})
}

// exploreCall is an explorer-tagged completion on the given per-call stream — what the sandbox's
// explore sub-loop sends.
func exploreCall(stream string, req model.Request) broker.CompletionParams {
	return broker.CompletionParams{SubContext: broker.SubContextExplorer, Stream: stream, Request: req}
}

// TestRelayExploreRoutesToPinnedAdapterAndDrawsCeiling: an explorer-tagged call runs on the
// pinned explorer adapter (never the parent's frontier model — that is the tier-escape guard),
// and its tokens still feed Usage() so the explorer's spend draws the parent-task ceiling.
func TestRelayExploreRoutesToPinnedAdapterAndDrawsCeiling(t *testing.T) {
	parent := &recordingAdapter{resp: model.Response{Usage: model.Usage{InputTokens: 100, OutputTokens: 50}, Stop: model.StopEndTurn}}
	explore := &recordingAdapter{resp: model.Response{Usage: model.Usage{InputTokens: 7, OutputTokens: 3}, Stop: model.StopEndTurn}}
	r := testRelayWithExplore(parent, explore, core.ExploreBudget{Tokens: 1000})

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err != nil {
		t.Fatalf("parent Complete: %v", err)
	}
	if _, err := r.Complete(context.Background(), exploreCall("explore-1", model.Request{})); err != nil {
		t.Fatalf("explore Complete: %v", err)
	}
	if parent.calls != 1 {
		t.Errorf("parent adapter calls = %d, want 1 (an explorer call must not touch the parent model)", parent.calls)
	}
	if explore.calls != 1 {
		t.Errorf("explorer adapter calls = %d, want 1", explore.calls)
	}
	u := r.Usage()
	if u.InputTokens != 107 || u.OutputTokens != 53 {
		t.Errorf("combined usage = %+v, want input=107 output=53 (parent+explorer draw the one ceiling)", u)
	}
}

// TestRelayExploreSubBudgetRefusesPerStream: the runner refuses further calls on a stream once it
// reaches policy.explore_budget (typed CodeSubBudgetExhausted), but a fresh stream (a new explore
// call) gets the full budget again — the fixed cap resets per call.
func TestRelayExploreSubBudgetRefusesPerStream(t *testing.T) {
	explore := &recordingAdapter{resp: model.Response{Usage: model.Usage{InputTokens: 60}, Stop: model.StopEndTurn}}
	r := testRelayWithExplore(&recordingAdapter{}, explore, core.ExploreBudget{Tokens: 50})

	if _, err := r.Complete(context.Background(), exploreCall("s1", model.Request{})); err != nil {
		t.Fatalf("first explore Complete: %v", err)
	}
	_, err := r.Complete(context.Background(), exploreCall("s1", model.Request{}))
	var be *broker.Error
	if !errors.As(err, &be) || be.Code != broker.CodeSubBudgetExhausted {
		t.Fatalf("second same-stream call err = %v, want *broker.Error{CodeSubBudgetExhausted}", err)
	}
	if explore.calls != 1 {
		t.Errorf("explorer adapter calls = %d, want 1 (the refused call must not reach the model)", explore.calls)
	}
	if _, err := r.Complete(context.Background(), exploreCall("s2", model.Request{})); err != nil {
		t.Fatalf("fresh-stream explore Complete: %v (the per-call budget must reset)", err)
	}
	if explore.calls != 2 {
		t.Errorf("explorer adapter calls = %d, want 2 (a fresh stream must run)", explore.calls)
	}
}

// TestRelayExploreDisabledFailsClosed: with no explorer adapter pinned, an explorer-tagged call
// is refused rather than silently answered on the parent's frontier model — the agent must never
// reach a stronger tier by tagging a call.
func TestRelayExploreDisabledFailsClosed(t *testing.T) {
	parent := &recordingAdapter{resp: model.Response{Stop: model.StopEndTurn}}
	r := testRelayWithExplore(parent, nil, core.ExploreBudget{})

	if _, err := r.Complete(context.Background(), exploreCall("s1", model.Request{})); err == nil {
		t.Fatal("explorer call with no pinned explorer adapter: want error, got nil (must fail closed)")
	}
	if parent.calls != 0 {
		t.Errorf("parent adapter calls = %d, want 0 (a disabled explorer must never route to the parent model)", parent.calls)
	}
}

func TestRelayCapturesPromptAndTranscript(t *testing.T) {
	adapter := &recordingAdapter{resp: model.Response{Text: "ok", Reasoning: "thinking it through", Stop: model.StopEndTurn}}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})

	// No model call yet: there is nothing to harvest.
	if _, ok := r.Prompt(); ok {
		t.Error("Prompt available before any completion")
	}
	if _, ok := r.Transcript(); ok {
		t.Error("Transcript available before any completion")
	}

	first := model.Request{System: "persona", Messages: []model.Message{{Role: model.RoleUser, Text: "first"}}}
	if _, err := r.Complete(context.Background(), parentCall(first)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// A second turn must not overwrite the captured prompt (the first request).
	if _, err := r.Complete(context.Background(), parentCall(model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "second"}}})); err != nil {
		t.Fatalf("second Complete: %v", err)
	}

	promptData, ok := r.Prompt()
	if !ok {
		t.Fatal("Prompt not captured after a completion")
	}
	var gotPrompt model.Request
	if err := json.Unmarshal(promptData, &gotPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if gotPrompt.System != "persona" || len(gotPrompt.Messages) != 1 || gotPrompt.Messages[0].Text != "first" {
		t.Errorf("captured prompt = %+v, want the first request", gotPrompt)
	}

	transcriptData, ok := r.Transcript()
	if !ok {
		t.Fatal("Transcript not captured after completions")
	}
	var turns []model.TranscriptTurn
	if err := json.Unmarshal(transcriptData, &turns); err != nil {
		t.Fatalf("unmarshal transcript: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("transcript turns = %d, want 2", len(turns))
	}
	if turns[0].Request.Messages[0].Text != "first" || turns[1].Request.Messages[0].Text != "second" {
		t.Errorf("transcript did not record both turns in order: %+v", turns)
	}
	// The model's reasoning stream is part of the recorded turn (T14.3): the audited
	// transcript carries the decision trail, not just the final text and tool calls.
	if turns[0].Response.Reasoning != "thinking it through" {
		t.Errorf("transcript turn reasoning = %q, want the emitted thinking recorded", turns[0].Response.Reasoning)
	}
}

// TestRelayExploreCapturedSeparatelyFromParentTranscript proves the T12.4 invariant: an
// explorer completion lands in the SEPARATE explore transcript (harvested on its own hash),
// never in the parent transcript, and never overwriting the parent's captured prompt. The
// pinned explorer model becomes available only once the sub-loop actually ran.
func TestRelayExploreCapturedSeparatelyFromParentTranscript(t *testing.T) {
	parent := &recordingAdapter{resp: model.Response{Text: "parent", Stop: model.StopEndTurn}}
	explore := &recordingAdapter{resp: model.Response{Text: "explored", Stop: model.StopEndTurn}}
	r := testRelayWithExplore(parent, explore, core.ExploreBudget{Tokens: 1000})

	// Before any explore call there is nothing to harvest and no pinned model to record.
	if _, ok := r.ExploreTranscript(); ok {
		t.Error("ExploreTranscript available before any explorer completion")
	}
	if m, ok := r.ExploreModel(); ok {
		t.Errorf("ExploreModel = %q available before any explorer completion; want ok=false", m)
	}

	parentReq := model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "parent-prompt"}}}
	if _, err := r.Complete(context.Background(), parentCall(parentReq)); err != nil {
		t.Fatalf("parent Complete: %v", err)
	}
	exploreReq := model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "explore-question"}}}
	if _, err := r.Complete(context.Background(), exploreCall("explore-1", exploreReq)); err != nil {
		t.Fatalf("explore Complete: %v", err)
	}

	// The parent transcript holds ONLY the parent turn — the explorer exchange must not leak in.
	parentData, ok := r.Transcript()
	if !ok {
		t.Fatal("parent Transcript not captured")
	}
	var parentTurns []model.TranscriptTurn
	if err := json.Unmarshal(parentData, &parentTurns); err != nil {
		t.Fatalf("unmarshal parent transcript: %v", err)
	}
	if len(parentTurns) != 1 || parentTurns[0].Request.Messages[0].Text != "parent-prompt" {
		t.Fatalf("parent transcript = %+v, want exactly the one parent turn", parentTurns)
	}

	// The captured prompt is the parent's, not the explorer's question.
	promptData, ok := r.Prompt()
	if !ok {
		t.Fatal("Prompt not captured")
	}
	var gotPrompt model.Request
	if err := json.Unmarshal(promptData, &gotPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if gotPrompt.Messages[0].Text != "parent-prompt" {
		t.Errorf("captured prompt = %+v, want the parent request (explorer must not overwrite it)", gotPrompt)
	}

	// The explore transcript holds ONLY the explorer turn.
	exploreData, ok := r.ExploreTranscript()
	if !ok {
		t.Fatal("ExploreTranscript not captured after an explorer completion")
	}
	var exploreTurns []model.TranscriptTurn
	if err := json.Unmarshal(exploreData, &exploreTurns); err != nil {
		t.Fatalf("unmarshal explore transcript: %v", err)
	}
	if len(exploreTurns) != 1 || exploreTurns[0].Request.Messages[0].Text != "explore-question" {
		t.Fatalf("explore transcript = %+v, want exactly the one explorer turn", exploreTurns)
	}

	// The pinned explorer model is now recordable — the "explore happened" provenance signal.
	m, ok := r.ExploreModel()
	if !ok || m != "cheap" {
		t.Errorf("ExploreModel = (%q, %v), want (cheap, true)", m, ok)
	}
}

// TestRelayExploreEventsCarrySubContext proves explorer live events are labeled with the
// explorer sub-context (so the control room nests them), while the parent's own events stay
// unlabeled — the wire half of the observability nesting.
func TestRelayExploreEventsCarrySubContext(t *testing.T) {
	parent := &recordingAdapter{deltas: []string{"p"}, resp: model.Response{Stop: model.StopEndTurn}}
	explore := &recordingAdapter{
		deltas:    []string{"e"},
		reasoning: []string{"think"},
		resp:      model.Response{Stop: model.StopEndTurn, ToolCalls: []model.ToolCall{{Name: "read_file"}}},
	}
	pub := &recordingPublisher{}
	r := newRelay(parent, pub, &bundleSandbox{}, relayConfig{
		eventSubject:   "harness.agent.inv-1.events",
		issueID:        "iss-1",
		role:           "implementor",
		repo:           "/repo",
		allowedBranch:  "candidate/iss-1",
		log:            discardLogger(),
		model:          "frontier",
		exploreAdapter: explore,
		exploreModel:   "cheap",
		exploreBudget:  core.ExploreBudget{Tokens: 1000},
	})

	if _, err := r.Complete(context.Background(), parentCall(model.Request{})); err != nil {
		t.Fatalf("parent Complete: %v", err)
	}
	if _, err := r.Complete(context.Background(), exploreCall("explore-1", model.Request{})); err != nil {
		t.Fatalf("explore Complete: %v", err)
	}

	type ev struct {
		Type       string `json:"type"`
		SubContext string `json:"subContext"`
	}
	var parentEvents, explorerEvents int
	pub.mu.Lock()
	data := append([][]byte(nil), pub.data...)
	pub.mu.Unlock()
	for _, d := range data {
		_, payload := decodeEvent(t, d)
		var e ev
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatalf("unmarshal inner event: %v", err)
		}
		switch e.SubContext {
		case "":
			parentEvents++
		case string(broker.SubContextExplorer):
			explorerEvents++
		default:
			t.Errorf("unexpected sub-context %q on event %+v", e.SubContext, e)
		}
	}
	// Parent published one token event; explorer published token + reasoning + tool = 3, all
	// tagged explorer.
	if parentEvents != 1 {
		t.Errorf("parent-tagged events = %d, want 1", parentEvents)
	}
	if explorerEvents != 3 {
		t.Errorf("explorer-tagged events = %d, want 3 (token+reasoning+tool)", explorerEvents)
	}
}

// --- GitPush -----------------------------------------------------------------

func TestRelayGitPushRefusesNonTaskBranch(t *testing.T) {
	sb := &bundleSandbox{}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)
	pushed := false
	r.pushBundle = func(context.Context, string, string, []byte) (string, error) {
		pushed = true
		return "", nil
	}

	_, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "main"})
	if err == nil {
		t.Fatal("GitPush onto main: want error, got nil")
	}
	if sb.gotCmd.Path != "" {
		t.Error("a refused branch must not exec inside the sandbox")
	}
	if pushed {
		t.Error("a refused branch must not reach pushBundle")
	}
}

func TestRelayGitPushExtractsBundleAndPushes(t *testing.T) {
	sb := &bundleSandbox{result: sandbox.ExecResult{ExitCode: 0, Stdout: []byte("BUNDLEBYTES")}}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)

	var gotRepo, gotBranch string
	var gotBundle []byte
	r.pushBundle = func(_ context.Context, repo, branch string, bundle []byte) (string, error) {
		gotRepo, gotBranch, gotBundle = repo, branch, bundle
		return "deadbeef", nil
	}

	res, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"})
	if err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if res.Commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", res.Commit)
	}

	// The branch is extracted as a git bundle on stdout from inside the sandbox.
	want := []string{"bundle", "create", "-", "candidate/iss-1"}
	if sb.gotCmd.Path != "git" || strings.Join(sb.gotCmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("exec = %s %v, want git %v", sb.gotCmd.Path, sb.gotCmd.Args, want)
	}
	if gotRepo != "/repo" || gotBranch != "candidate/iss-1" || string(gotBundle) != "BUNDLEBYTES" {
		t.Errorf("pushBundle got repo=%q branch=%q bundle=%q", gotRepo, gotBranch, string(gotBundle))
	}
}

func TestRelayGitPushFailsOnNonZeroBundleExit(t *testing.T) {
	sb := &bundleSandbox{result: sandbox.ExecResult{ExitCode: 128, Stderr: []byte("not a valid object name")}}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)
	r.pushBundle = func(context.Context, string, string, []byte) (string, error) {
		t.Fatal("pushBundle must not run when bundle extraction fails")
		return "", nil
	}

	if _, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"}); err == nil {
		t.Fatal("GitPush with failing bundle: want error, got nil")
	}
}

// --- PublishEvent ------------------------------------------------------------

func TestRelayPublishEvent(t *testing.T) {
	pub := &recordingPublisher{}
	r := testRelay(&recordingAdapter{}, pub, &bundleSandbox{})

	err := r.PublishEvent(context.Background(), broker.PublishRequest{Type: "progress", Payload: json.RawMessage(`{"msg":"hi"}`)})
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	if pub.count() != 1 || pub.subj[0] != "harness.agent.inv-1.events" {
		t.Fatalf("published %d events on %v, want 1 on the agent event subject", pub.count(), pub.subj)
	}
	_, payload := decodeEvent(t, pub.data[0])
	var got broker.PublishRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got.Type != "progress" {
		t.Errorf("event type = %q, want progress", got.Type)
	}
}

// --- FetchPackage ------------------------------------------------------------

func TestRelayFetchPackageProxiesAndLogs(t *testing.T) {
	var gotPath string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("v1.0.0\nv1.1.0\n"))
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "harness.agent.inv-1.events", issueID: "iss-1", role: "implementor",
		repo: "/repo", allowedBranch: "candidate/iss-1", log: discardLogger(),
		packageProxy: proxy.URL,
	})

	res, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/github.com/pkg/errors/@v/list"})
	if err != nil {
		t.Fatalf("FetchPackage: %v", err)
	}
	if gotPath != "/github.com/pkg/errors/@v/list" {
		t.Errorf("proxy got path %q, want the request path joined onto the proxy base", gotPath)
	}
	if res.Status != 200 || string(res.Body) != "v1.0.0\nv1.1.0\n" {
		t.Errorf("result = %+v, want status 200 and the proxied body", res)
	}
	if !strings.HasPrefix(res.ContentType, "text/plain") {
		t.Errorf("content-type = %q, want the upstream text/plain echoed", res.ContentType)
	}
}

func TestRelayFetchPackageForwardsUpstreamStatus(t *testing.T) {
	// 404/410 must be echoed, not swallowed: go reads them as "not found, try the next proxy".
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), packageProxy: proxy.URL,
	})

	res, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/v9.9.9.info"})
	if err != nil {
		t.Fatalf("FetchPackage: %v", err)
	}
	if res.Status != http.StatusGone {
		t.Errorf("status = %d, want 410 echoed from upstream", res.Status)
	}
}

func TestRelayFetchPackageNoProxyConfigured(t *testing.T) {
	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), // packageProxy empty
	})
	if _, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/list"}); err == nil {
		t.Fatal("FetchPackage with no proxy configured must error, got nil")
	}
}

func TestRelayFetchPackageRejectsMalformedPath(t *testing.T) {
	// The proxy must never be dialed for a malformed path; a hit here is a failure.
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("malformed path must be rejected before any egress")
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), packageProxy: proxy.URL,
	})

	for _, bad := range []string{"", "no-leading-slash", "/has/../traversal", "https://evil.example/x", "/has space"} {
		if _, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: bad}); err == nil {
			t.Errorf("path %q: want rejection, got nil error", bad)
		}
	}
}

// --- host-side bundle apply (real git, no docker) ----------------------------

// TestPushBundleToRepoIntegration drives the real host-side git path: it builds a
// candidate branch in one repo, bundles it exactly as the in-sandbox exec would, and
// asserts pushBundleToRepo lands that branch+commit in a separate source repo. This is
// what makes a candidate reachable to the gate/merge without a bind mount or copy-out.
func TestPushBundleToRepoIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()

	// Source repo the candidate is pushed into (mirrors r.opts.Repo).
	srcRepo := t.TempDir()
	mustGit(t, srcRepo, "init", "-q")
	mustGit(t, srcRepo, "config", "user.email", "t@example.com")
	mustGit(t, srcRepo, "config", "user.name", "t")
	writeFile(t, filepath.Join(srcRepo, "base.txt"), "base")
	mustGit(t, srcRepo, "add", ".")
	mustGit(t, srcRepo, "commit", "-qm", "base")

	// Candidate repo (mirrors the sandbox worktree): clone src, branch, commit, bundle.
	candRepo := t.TempDir()
	mustGit(t, "", "clone", "-q", srcRepo, candRepo)
	mustGit(t, candRepo, "config", "user.email", "t@example.com")
	mustGit(t, candRepo, "config", "user.name", "t")
	mustGit(t, candRepo, "checkout", "-q", "-b", "candidate/iss-1")
	writeFile(t, filepath.Join(candRepo, "feature.txt"), "feature")
	mustGit(t, candRepo, "add", ".")
	mustGit(t, candRepo, "commit", "-qm", "feature")
	wantSHA := strings.TrimSpace(mustGit(t, candRepo, "rev-parse", "candidate/iss-1"))
	bundle := []byte(mustGitRaw(t, candRepo, "bundle", "create", "-", "candidate/iss-1"))

	commit, err := pushBundleToRepo(ctx, srcRepo, "candidate/iss-1", bundle)
	if err != nil {
		t.Fatalf("pushBundleToRepo: %v", err)
	}
	if commit != wantSHA {
		t.Errorf("returned commit = %q, want %q", commit, wantSHA)
	}
	// The branch now exists in the source repo at the candidate head.
	gotSHA := strings.TrimSpace(mustGit(t, srcRepo, "rev-parse", "refs/heads/candidate/iss-1"))
	if gotSHA != wantSHA {
		t.Errorf("source repo branch head = %q, want %q", gotSHA, wantSHA)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return mustGitRaw(t, dir, args...)
}

func mustGitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
