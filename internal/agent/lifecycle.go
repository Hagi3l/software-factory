package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
)

// LifecycleTools are the tools that control the invocation and produce its Result (see
// specs/components/agent.md): submit (candidate ready → done), escalate (spec ambiguity
// → needs-spec-clarification), and request_subtask (propose a child issue — emergent
// breadth). submit and escalate are terminal: they return the Result that ends the loop.
// request_subtask is not terminal — it accumulates a Proposal and lets the agent keep
// working; the proposals are folded into whichever terminal Result follows.
//
// They share one lifecycle value so the terminal tools can attach the proposals gathered
// across the run. They are bound per invocation to the broker (submit pushes the
// candidate through it) and the Brief (the issue id fixes the task-branch name and is the
// default dependency for proposed children).
func LifecycleTools(brief core.Brief, brk BrokerClient) []Tool {
	lc := &lifecycle{brief: brief, brk: brk}
	return []Tool{lc.submitTool(), lc.escalateTool(), lc.requestSubtaskTool()}
}

type lifecycle struct {
	brief core.Brief
	brk   BrokerClient

	mu        sync.Mutex
	proposals []core.Proposal
}

func (lc *lifecycle) addProposal(p core.Proposal) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.proposals = append(lc.proposals, p)
}

func (lc *lifecycle) takeProposals() []core.Proposal {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if len(lc.proposals) == 0 {
		return nil
	}
	return append([]core.Proposal(nil), lc.proposals...)
}

// submitTool pushes the candidate branch through the broker and assembles the terminal
// `done` Result. The candidate is only "ready", not accepted — the independent gate
// decides (producer ≠ verifier); an agent never self-certifies (see
// specs/verification.md). The branch name is the canonical task branch; only it is
// pushable (the relay refuses any other), so the model does not choose it.
func (lc *lifecycle) submitTool() Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "submit",
			Description: "Submit the candidate as ready for verification. Commit your work onto the candidate " +
				"branch first; this pushes that branch and ends the task. The candidate is gated independently — " +
				"submitting is not acceptance.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"summary": {"type": "string", "description": "Short summary of what the candidate does and how it satisfies the criteria."}
				}
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Summary string `json:"summary"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}

			branch := core.CandidateBranch(lc.brief.Issue.ID)
			res, err := lc.brk.GitPush(ctx, broker.GitPushRequest{Branch: branch})
			if err != nil {
				// A push failure is the agent's to fix (it likely has not committed onto the
				// candidate branch yet), so surface it for another turn rather than failing
				// the whole invocation.
				return invalid(fmt.Sprintf("git push of %s failed: %v — commit your work onto %s, then submit again", branch, err, branch)), nil
			}

			result := core.Result{
				Status:   core.StatusDone,
				Branch:   core.Branch{Ref: branch, Commits: []string{res.Commit}},
				Proposes: lc.takeProposals(),
			}
			return Outcome{Content: "submitted: " + res.Commit, Result: &result}, nil
		},
	}
}

// escalateTool ends the invocation with needs-spec-clarification, routing to the human
// re-entry loop. The agent must escalate, never invent intent, on spec ambiguity or
// contradiction (see specs/specs-process.md). The required reason is recorded in the
// transcript (the artifact store, plan T1.18) — the canonical Result carries the status,
// and the reason travels with the transcript the human reads.
func (lc *lifecycle) escalateTool() Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "escalate",
			Description: "Escalate to a human because the specification is ambiguous, contradictory, or " +
				"insufficient to proceed without guessing. Ends the task without a candidate.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"reason": {"type": "string", "description": "What is ambiguous or contradictory, and what decision is needed."}
				},
				"required": ["reason"]
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Reason string `json:"reason"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Reason == "" {
				return invalid("reason is required to escalate"), nil
			}
			result := core.Result{
				Status:   core.StatusNeedsSpecClarification,
				Proposes: lc.takeProposals(),
			}
			return Outcome{Content: "escalated: " + a.Reason, Result: &result}, nil
		},
	}
}

// requestSubtaskTool proposes a child issue (emergent breadth). It is NOT terminal: the
// proposal is accumulated and the agent keeps working, so a run can propose several
// children and still submit its own candidate. Everything here is a proposal — the
// orchestrator validates DAG-legality (valid role, acyclic edges, within budget) before
// writing it; an illegal proposal is simply rejected (see specs/workflow.md).
func (lc *lifecycle) requestSubtaskTool() Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "request_subtask",
			Description: "Propose a new child work item to be scheduled separately. Use this to split out " +
				"work that belongs in its own issue rather than doing it inline. Does not end the task.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"title": {"type": "string", "description": "Short imperative title of the child work item."},
					"body": {"type": "string", "description": "What the child work item must accomplish."},
					"role": {"type": "string", "description": "DAG role/stage that should handle it, e.g. \"implement\"."},
					"depends_on": {"type": "array", "items": {"type": "string"}, "description": "IDs of existing issues this child is blocked by."}
				},
				"required": ["title", "role"]
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Title     string   `json:"title"`
				Body      string   `json:"body"`
				Role      string   `json:"role"`
				DependsOn []string `json:"depends_on"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Title == "" {
				return invalid("title is required"), nil
			}
			if a.Role == "" {
				return invalid("role is required"), nil
			}
			lc.addProposal(core.Proposal{
				Issue:     core.Issue{Title: a.Title, Body: a.Body, Role: a.Role},
				DependsOn: a.DependsOn,
			})
			return Outcome{Content: fmt.Sprintf("subtask proposed: %q (role %s)", a.Title, a.Role)}, nil
		},
	}
}
