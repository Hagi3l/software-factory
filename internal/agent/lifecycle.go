package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
)

// LifecycleTools are the tools that control the invocation and produce its Result (see
// specs/components/agent.md): submit (candidate ready → done), submit_plan (a
// decomposition is ready → done, with no candidate), escalate (spec ambiguity →
// needs-spec-clarification), request_subtask (propose a child issue — emergent breadth),
// and trace_test (record a test↔spec traceability entry). submit, submit_plan, and
// escalate are terminal: they return the Result that ends the loop. request_subtask and
// trace_test are not terminal — they accumulate (a Proposal, a TraceEntry) and let the
// agent keep working; the accumulated proposals and trace entries are folded into the
// terminal Result that follows.
//
// The tools are universal — every soul gets all of them — but a soul's persona decides
// which it uses: only the planner calls submit_plan and only the test author calls
// trace_test, the same way only the implementor produces a real candidate via submit.
//
// They share one lifecycle value so the terminal tools can attach what was gathered
// across the run. They are bound per invocation to the broker (submit pushes the
// candidate through it) and the Brief (the issue id fixes the task-branch name and is the
// default dependency for proposed children).
//
// transforms is the shared transformation ledger the semantic write tools (T6.3) record
// into; the terminal tools fold it into the Result's evidence so the mechanism (semantic
// vs text floor) of every rename/code_action travels with the candidate. It may be nil
// (no semantic write tools wired), in which case the Result simply carries no Transforms.
func LifecycleTools(brief core.Brief, brk BrokerClient, transforms *TransformLedger) []Tool {
	lc := &lifecycle{brief: brief, brk: brk, transforms: transforms}
	return []Tool{lc.submitTool(), lc.submitPlanTool(), lc.escalateTool(), lc.requestSubtaskTool(), lc.traceTestTool()}
}

type lifecycle struct {
	brief      core.Brief
	brk        BrokerClient
	transforms *TransformLedger

	mu        sync.Mutex
	proposals []core.Proposal
	trace     []core.TraceEntry
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

func (lc *lifecycle) addTrace(e core.TraceEntry) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.trace = append(lc.trace, e)
}

func (lc *lifecycle) takeTrace() []core.TraceEntry {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if len(lc.trace) == 0 {
		return nil
	}
	return append([]core.TraceEntry(nil), lc.trace...)
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
				Status:     core.StatusDone,
				Branch:     core.Branch{Ref: branch, Commits: []string{res.Commit}},
				Proposes:   lc.takeProposals(),
				Trace:      lc.takeTrace(),
				Transforms: lc.transforms.take(),
			}
			return Outcome{Content: "submitted: " + res.Commit, Result: &result}, nil
		},
	}
}

// submitPlanTool ends a decomposition planner's invocation. Unlike submit it pushes no
// candidate branch: a planner writes no code — its entire contribution is the child
// issues it proposed with request_subtask (emergent breadth), which the orchestrator
// validates and writes. It folds the accumulated proposals into a terminal `done` Result
// with no branch; the orchestrator's plan stage accepts it structurally (no sandbox gate,
// since there is nothing to verify). Requiring at least one proposal here catches a
// planner that would otherwise end the pipeline with nothing to do (see
// specs/workflow.md "emergent breadth", specs/components/agent.md).
func (lc *lifecycle) submitPlanTool() Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "submit_plan",
			Description: "Submit your decomposition: the child work items you proposed with request_subtask " +
				"become the next stage's issues. Use this to finish a planning task. Produces no candidate " +
				"branch (a planner writes no code) and ends the task.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"summary": {"type": "string", "description": "Short summary of how you decomposed the work."}
				}
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Summary string `json:"summary"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			proposals := lc.takeProposals()
			if len(proposals) == 0 {
				return invalid("propose at least one child work item with request_subtask before submitting the plan"), nil
			}
			result := core.Result{
				Status:     core.StatusDone,
				Proposes:   proposals,
				Trace:      lc.takeTrace(),
				Transforms: lc.transforms.take(),
			}
			return Outcome{Content: fmt.Sprintf("plan submitted: %d child issue(s)", len(proposals)), Result: &result}, nil
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
				Status:     core.StatusNeedsSpecClarification,
				Proposes:   lc.takeProposals(),
				Transforms: lc.transforms.take(),
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
					"role": {"type": "string", "description": "The role that should handle it, e.g. \"test-author\"."},
					"spec": {"type": "string", "description": "Repository-relative path of the spec file that governs this child (e.g. \"specs/orders.md\"), so its bounded spec slice is resolved for it. Use the file paths named in your spec slice."},
					"key": {"type": "string", "description": "Optional local label for this child so a later child can name it in depends_on (siblings have no id yet)."},
					"tags": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Optional selector tags (e.g. {\"lang\": \"go\"}) that pick which soul fulfills the child's role when several do. Threaded forward across the child's stages."},
					"depends_on": {"type": "array", "items": {"type": "string"}, "description": "Blocked-by edges: existing issue ids, or the key of a sibling proposed in this same task."}
				},
				"required": ["title", "role"]
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Title     string            `json:"title"`
				Body      string            `json:"body"`
				Role      string            `json:"role"`
				Spec      string            `json:"spec"`
				Key       string            `json:"key"`
				Tags      map[string]string `json:"tags"`
				DependsOn []string          `json:"depends_on"`
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
				Issue:     core.Issue{Title: a.Title, Body: a.Body, Role: a.Role, Spec: a.Spec, Tags: a.Tags},
				Key:       a.Key,
				DependsOn: a.DependsOn,
			})
			return Outcome{Content: fmt.Sprintf("subtask proposed: %q (role %s)", a.Title, a.Role)}, nil
		},
	}
}

// traceTestTool records one row of the test↔spec traceability map: for an acceptance
// test it just wrote, the test author names the spec heading and sentence it claims to
// encode. It is NOT terminal — the author traces each test as it writes it, the way it
// proposes subtasks, and the accumulated map is folded into the terminal submit Result,
// harvested to the artifact store, and cited in the merge's provenance trailer. Specs are
// pure prose, so this is the author's own account of its interpretation — not a proof of
// faithfulness (the gate's red→green and mutation checks carry that), but the only window
// a human has into how the model read the prose, auditable after the fact (see
// specs/verification.md, specs/specs-process.md).
func (lc *lifecycle) traceTestTool() Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "trace_test",
			Description: "Record the spec heading and sentence an acceptance test you wrote claims to encode, " +
				"so your interpretation of the prose is auditable. Call once per test. Does not end the task.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"test": {"type": "string", "description": "The test this entry traces, e.g. \"TestRejectsNegativeQuantity\"."},
					"spec": {"type": "string", "description": "The spec file the heading lives in, e.g. \"verification.md\"."},
					"heading": {"type": "string", "description": "The spec heading the test claims to encode, e.g. \"Red→green proof\"."},
					"sentence": {"type": "string", "description": "The spec sentence the test claims to encode."}
				},
				"required": ["test", "heading", "sentence"]
			}`),
		},
		fn: func(_ context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Test     string `json:"test"`
				Spec     string `json:"spec"`
				Heading  string `json:"heading"`
				Sentence string `json:"sentence"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Test == "" {
				return invalid("test is required"), nil
			}
			if a.Heading == "" || a.Sentence == "" {
				return invalid("heading and sentence are required: name the spec heading and the sentence the test encodes"), nil
			}
			lc.addTrace(core.TraceEntry{Test: a.Test, Spec: a.Spec, Heading: a.Heading, Sentence: a.Sentence})
			return Outcome{Content: fmt.Sprintf("traced %s → %q: %q", a.Test, a.Heading, a.Sentence)}, nil
		},
	}
}
