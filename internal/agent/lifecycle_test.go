package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/core"
)

// recordingBroker captures GitPush calls and returns a configured result/error.
type recordingBroker struct {
	pushedBranch string
	pushCommit   string
	pushErr      error
}

func (b *recordingBroker) GitPush(_ context.Context, req broker.GitPushRequest) (broker.GitPushResult, error) {
	b.pushedBranch = req.Branch
	if b.pushErr != nil {
		return broker.GitPushResult{}, b.pushErr
	}
	return broker.GitPushResult{Commit: b.pushCommit}, nil
}
func (b *recordingBroker) PublishEvent(context.Context, broker.PublishRequest) error { return nil }

func lifecycleBrief() core.Brief {
	return core.Brief{Issue: core.Issue{ID: "iss-1", Role: "implement"}}
}

func lcToolByName(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Def().Name == name {
			return tl
		}
	}
	t.Fatalf("no lifecycle tool named %q", name)
	return nil
}

func TestLifecycleToolSet(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Def().Name] = true
		if !json.Valid(tl.Def().Params) {
			t.Errorf("tool %s has invalid JSON schema params", tl.Def().Name)
		}
	}
	for _, want := range []string{"submit", "submit_plan", "escalate", "request_subtask"} {
		if !got[want] {
			t.Errorf("missing lifecycle tool %q", want)
		}
	}
}

// submit pushes the canonical candidate branch and returns a terminal done Result with
// the pushed commit.
func TestSubmitPushesAndTerminates(t *testing.T) {
	brk := &recordingBroker{pushCommit: "abc123"}
	tools := LifecycleTools(lifecycleBrief(), brk)
	out := invoke(t, lcToolByName(t, tools, "submit"), `{"summary":"did the thing"}`)

	if out.Result == nil {
		t.Fatal("submit must return a terminal Result")
	}
	if brk.pushedBranch != "candidate/iss-1" {
		t.Errorf("pushed branch = %q, want candidate/iss-1", brk.pushedBranch)
	}
	r := out.Result
	if r.Status != core.StatusDone {
		t.Errorf("status = %q, want done", r.Status)
	}
	if r.Branch.Ref != "candidate/iss-1" || len(r.Branch.Commits) != 1 || r.Branch.Commits[0] != "abc123" {
		t.Errorf("branch = %+v, want candidate/iss-1 @ abc123", r.Branch)
	}
}

// A push failure is surfaced to the model (IsError, non-terminal), not fatal — the agent
// likely has not committed onto the candidate branch yet and can fix it and retry.
func TestSubmitPushFailureIsRecoverable(t *testing.T) {
	brk := &recordingBroker{pushErr: errors.New("no such ref")}
	tools := LifecycleTools(lifecycleBrief(), brk)
	out := invoke(t, lcToolByName(t, tools, "submit"), `{}`)

	if out.Result != nil {
		t.Errorf("push failure must not terminate the loop, got Result %+v", out.Result)
	}
	if !out.IsError || !strings.Contains(out.Content, "git push") {
		t.Errorf("submit push failure = %+v, want IsError mentioning git push", out)
	}
}

// submit_plan folds the accumulated proposals into a terminal done Result with NO
// candidate branch (a planner writes no code), and pushes nothing through the broker.
func TestSubmitPlanFoldsProposalsNoPush(t *testing.T) {
	brk := &recordingBroker{pushCommit: "should-not-be-used"}
	tools := LifecycleTools(lifecycleBrief(), brk)

	for _, args := range []string{
		`{"title":"add order type","role":"test-author","key":"order-type"}`,
		`{"title":"validate quantity","role":"test-author","depends_on":["order-type"]}`,
	} {
		if out := invoke(t, lcToolByName(t, tools, "request_subtask"), args); out.IsError {
			t.Fatalf("request_subtask %s = %+v", args, out)
		}
	}

	out := invoke(t, lcToolByName(t, tools, "submit_plan"), `{"summary":"split into two slices"}`)
	if out.Result == nil {
		t.Fatal("submit_plan must return a terminal Result")
	}
	if brk.pushedBranch != "" {
		t.Errorf("submit_plan pushed %q, want no push (a planner has no candidate)", brk.pushedBranch)
	}
	r := out.Result
	if r.Status != core.StatusDone {
		t.Errorf("status = %q, want done", r.Status)
	}
	if r.Branch.Ref != "" {
		t.Errorf("branch = %+v, want empty (no candidate)", r.Branch)
	}
	if len(r.Proposes) != 2 {
		t.Fatalf("Proposes = %+v, want 2 children", r.Proposes)
	}
	// The local key and the sibling reference ride through to the proposal so beads.Apply
	// can resolve the inter-sibling edge at write time.
	if r.Proposes[0].Key != "order-type" {
		t.Errorf("proposal[0].Key = %q, want order-type", r.Proposes[0].Key)
	}
	if len(r.Proposes[1].DependsOn) != 1 || r.Proposes[1].DependsOn[0] != "order-type" {
		t.Errorf("proposal[1].DependsOn = %v, want [order-type]", r.Proposes[1].DependsOn)
	}
}

// request_subtask threads selector tags onto the proposed child issue, so the
// decomposition planner sets at issue-creation which soul each child's role resolves to
// (see specs/configuration.md, core.Issue.Tags).
func TestRequestSubtaskThreadsTags(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	if out := invoke(t, lcToolByName(t, tools, "request_subtask"),
		`{"title":"add order type","role":"test-author","tags":{"lang":"go"}}`); out.IsError {
		t.Fatalf("request_subtask = %+v", out)
	}
	out := invoke(t, lcToolByName(t, tools, "submit_plan"), `{"summary":"one slice"}`)
	if out.Result == nil || len(out.Result.Proposes) != 1 {
		t.Fatalf("submit_plan = %+v, want one proposal", out.Result)
	}
	if tags := out.Result.Proposes[0].Issue.Tags; tags["lang"] != "go" || len(tags) != 1 {
		t.Errorf("proposal tags = %v, want {lang:go}", tags)
	}
}

// submit_plan with no proposals is a non-terminal error: a planner that decomposed
// nothing would end the pipeline with no work, so it is told to propose first.
func TestSubmitPlanRequiresProposals(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	out := invoke(t, lcToolByName(t, tools, "submit_plan"), `{}`)
	if out.Result != nil {
		t.Errorf("submit_plan with no proposals must not terminate, got %+v", out.Result)
	}
	if !out.IsError || !strings.Contains(out.Content, "request_subtask") {
		t.Errorf("submit_plan no-proposals = %+v, want IsError guiding to request_subtask", out)
	}
}

// escalate ends with needs-spec-clarification and requires a reason.
func TestEscalate(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	out := invoke(t, lcToolByName(t, tools, "escalate"), `{"reason":"spec contradicts itself"}`)
	if out.Result == nil || out.Result.Status != core.StatusNeedsSpecClarification {
		t.Fatalf("escalate = %+v, want terminal needs-spec-clarification", out)
	}

	out = invoke(t, lcToolByName(t, tools, "escalate"), `{}`)
	if out.Result != nil || !out.IsError || !strings.Contains(out.Content, "reason is required") {
		t.Errorf("escalate without reason = %+v, want IsError required", out)
	}
}

// request_subtask accumulates a proposal (non-terminal) that a later submit folds into
// its Result.
func TestRequestSubtaskAccumulatesIntoSubmit(t *testing.T) {
	brk := &recordingBroker{pushCommit: "deadbeef"}
	tools := LifecycleTools(lifecycleBrief(), brk)

	out := invoke(t, lcToolByName(t, tools, "request_subtask"),
		`{"title":"add metrics","body":"emit counters","role":"implement","depends_on":["iss-1"]}`)
	if out.Result != nil {
		t.Errorf("request_subtask must not terminate, got %+v", out.Result)
	}
	if out.IsError || !strings.Contains(out.Content, "add metrics") {
		t.Errorf("request_subtask = %+v", out)
	}

	out = invoke(t, lcToolByName(t, tools, "submit"), `{}`)
	if out.Result == nil || len(out.Result.Proposes) != 1 {
		t.Fatalf("submit Result should carry 1 proposal, got %+v", out.Result)
	}
	p := out.Result.Proposes[0]
	if p.Issue.Title != "add metrics" || p.Issue.Role != "implement" || len(p.DependsOn) != 1 || p.DependsOn[0] != "iss-1" {
		t.Errorf("proposal = %+v, want add metrics/implement/depends iss-1", p)
	}
}

// request_subtask requires title and role.
func TestRequestSubtaskValidation(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	rs := lcToolByName(t, tools, "request_subtask")

	if out := invoke(t, rs, `{"role":"implement"}`); !out.IsError || !strings.Contains(out.Content, "title is required") {
		t.Errorf("missing title = %+v", out)
	}
	if out := invoke(t, rs, `{"title":"x"}`); !out.IsError || !strings.Contains(out.Content, "role is required") {
		t.Errorf("missing role = %+v", out)
	}
}

// trace_test is non-terminal and accumulates one TraceEntry per call into the terminal
// submit Result, in emission order — the test↔spec traceability map the author produces so
// its reading of the pure-prose spec is auditable (see specs/verification.md).
func TestTraceTestAccumulatesIntoSubmit(t *testing.T) {
	brk := &recordingBroker{pushCommit: "cafe"}
	tools := LifecycleTools(lifecycleBrief(), brk)

	out := invoke(t, lcToolByName(t, tools, "trace_test"),
		`{"test":"TestRejectsNegative","spec":"orders.md","heading":"Quantities","sentence":"reject negative quantities with a 400"}`)
	if out.Result != nil {
		t.Errorf("trace_test must not terminate, got %+v", out.Result)
	}
	if out.IsError || !strings.Contains(out.Content, "TestRejectsNegative") {
		t.Errorf("trace_test = %+v", out)
	}
	// A second entry, to prove accumulation and order.
	invoke(t, lcToolByName(t, tools, "trace_test"),
		`{"test":"TestHappyPath","heading":"Quantities","sentence":"accept positive quantities"}`)

	out = invoke(t, lcToolByName(t, tools, "submit"), `{}`)
	if out.Result == nil || len(out.Result.Trace) != 2 {
		t.Fatalf("submit Result should carry 2 trace entries, got %+v", out.Result)
	}
	e := out.Result.Trace[0]
	if e.Test != "TestRejectsNegative" || e.Spec != "orders.md" || e.Heading != "Quantities" ||
		e.Sentence != "reject negative quantities with a 400" {
		t.Errorf("trace[0] = %+v, want the first traced test verbatim", e)
	}
	if out.Result.Trace[1].Test != "TestHappyPath" {
		t.Errorf("trace[1] = %+v, want TestHappyPath (emission order preserved)", out.Result.Trace[1])
	}
}

// trace_test requires the test name and both the heading and the sentence: an entry that
// names no spec sentence records no interpretation and is worthless for audit.
func TestTraceTestValidation(t *testing.T) {
	tools := LifecycleTools(lifecycleBrief(), &recordingBroker{})
	tr := lcToolByName(t, tools, "trace_test")

	if out := invoke(t, tr, `{"heading":"H","sentence":"S"}`); !out.IsError || !strings.Contains(out.Content, "test is required") {
		t.Errorf("missing test = %+v", out)
	}
	if out := invoke(t, tr, `{"test":"T","sentence":"S"}`); !out.IsError || !strings.Contains(out.Content, "heading and sentence are required") {
		t.Errorf("missing heading = %+v", out)
	}
	if out := invoke(t, tr, `{"test":"T","heading":"H"}`); !out.IsError || !strings.Contains(out.Content, "heading and sentence are required") {
		t.Errorf("missing sentence = %+v", out)
	}
}
