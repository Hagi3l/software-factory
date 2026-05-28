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
	for _, want := range []string{"submit", "escalate", "request_subtask"} {
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
