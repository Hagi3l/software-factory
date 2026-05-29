package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// statusInProgress is the beads status an issue holds while a runner owns it. The
// orchestrator acts on a Result only while its issue is in this state, which is what
// keeps Result processing idempotent under at-least-once redelivery.
const statusInProgress = "in_progress"

// consumeResults is the event-driven half of the loop: it pulls Result envelopes off
// the orchestrator's durable consumer and processes each. Ack/Nak encode the outcome:
//   - a fully-processed Result (accepted, routed, or dead-lettered) is Acked.
//   - a transient failure (a gate that could not reach a verdict, a beads write that
//     failed, a merge that could not complete) is Naked so the Result is redelivered
//     and retried — the issue stays in_progress, so nothing is lost.
//   - a Result that cannot be correlated to an issue is poison and is Termed: a
//     redelivery cannot fix it, and terminating avoids an infinite redeliver loop.
//
// Canceling ctx stops the iterator, whose closed-iterator error is a clean shutdown.
func (o *Orchestrator) consumeResults(ctx context.Context, cons jetstream.Consumer) error {
	iter, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("orchestrator: open results iterator: %w", err)
	}
	go func() {
		<-ctx.Done()
		iter.Stop()
	}()
	for {
		msg, err := iter.Next()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil //nolint:nilerr // ctx cancel stopped the iterator; clean shutdown, not a failure
			}
			return fmt.Errorf("orchestrator: pull results: %w", err)
		}
		o.handleMessage(ctx, msg)
	}
}

// handleMessage decodes one Result and dispatches it to the reconcile logic, applying
// the Ack/Nak/Term decision.
func (o *Orchestrator) handleMessage(ctx context.Context, msg jetstream.Msg) {
	var res core.Result
	if err := json.Unmarshal(msg.Data(), &res); err != nil {
		o.log.Error("orchestrator: undecodable result, terminating", "err", err)
		_ = msg.TermWithReason("undecodable result")
		return
	}
	if res.IssueID == "" {
		o.log.Error("orchestrator: result has no issue id, terminating")
		_ = msg.TermWithReason("result without issue id")
		return
	}

	transient, err := o.handleResult(ctx, res)
	switch {
	case err != nil && transient:
		o.log.Error("orchestrator: transient failure handling result, redelivering", "issue", res.IssueID, "err", err)
		_ = msg.Nak()
	case err != nil:
		// A non-transient processing error: terminating the message is the safe choice,
		// since redelivery would just repeat it. The issue stays in_progress and the
		// reconcile sweep will eventually release it for a fresh attempt.
		o.log.Error("orchestrator: permanent failure handling result, terminating", "issue", res.IssueID, "err", err)
		_ = msg.TermWithReason("unprocessable result")
	default:
		_ = msg.Ack()
	}
}

// handleResult runs the reconcile decision for one Result. It returns (transient,
// err): a non-nil err with transient=true means "retry later" (Nak); transient=false
// means "do not redeliver". A nil err means the Result was fully processed.
//
// It is idempotent: it acts only while the issue is in_progress, so a duplicate or
// stale redelivery for an issue already accepted, rejected, or dead-lettered is a
// no-op (see specs/components/orchestrator.md).
func (o *Orchestrator) handleResult(ctx context.Context, res core.Result) (transient bool, err error) {
	issue, err := o.bd.Get(ctx, res.IssueID)
	if err != nil {
		return true, fmt.Errorf("get issue %s: %w", res.IssueID, err)
	}
	if issue.Status != statusInProgress {
		o.log.Info("orchestrator: result for issue not in progress, ignoring as stale/duplicate",
			"issue", issue.ID, "status", issue.Status, "result_status", res.Status)
		return false, nil
	}

	stage, ok := o.stageForRole(issue.Role)
	if !ok {
		// An in_progress issue whose role has no agent stage should never have been
		// dispatched; it cannot be advanced, so dead-letter it for human triage.
		return o.deadLetter(ctx, issue, "issue role has no agent stage")
	}

	// Validate emergent breadth before trusting it: a proposal naming an unknown role is
	// an illegal proposal, which the failure taxonomy classes as an Escalation —
	// dead-letter rather than write a malformed graph (see specs/workflow.md).
	for _, p := range res.Proposes {
		if !o.roleIsAgentStage(p.Issue.Role) {
			return o.deadLetter(ctx, issue, fmt.Sprintf("illegal proposal: unknown role %q", p.Issue.Role))
		}
	}

	switch res.Status {
	case core.StatusNeedsSpecClarification:
		// The agent escalated spec ambiguity; only a human (via spec refinement) can
		// resolve it. Dead-letter — never retry, since a retry would hit the same ambiguity.
		return o.deadLetter(ctx, issue, "agent escalated: needs-spec-clarification")

	case core.StatusFailed:
		// The agent could not produce a usable candidate. Route via on_failure or, if the
		// retry cap is spent, dead-letter.
		return o.route(ctx, issue, stage, "agent reported failure")

	case core.StatusDone:
		if stage.Kind == config.StageKindPlan {
			// A plan stage is an agent stage but is not sandbox-gated: the planner writes no
			// candidate, only proposes the children that make up the decomposition. Accept it
			// structurally (no branch, no gate) — see acceptPlan and specs/workflow.md.
			return o.acceptPlan(ctx, issue, stage, res)
		}
		if res.Branch.Ref == "" {
			// "done" with no candidate branch is not gradable; treat it as a failure to
			// produce a candidate and route it.
			return o.route(ctx, issue, stage, "done result carried no candidate branch")
		}
		report, gerr := o.runGate(ctx, issue, stage, res)
		if gerr != nil {
			// The gate could not reach a verdict (infrastructure) — transient, retry.
			return true, fmt.Errorf("gate issue %s: %w", issue.ID, gerr)
		}
		if report.Passed {
			return o.accept(ctx, issue, stage, res, report)
		}
		o.log.Info("orchestrator: gate rejected candidate", "issue", issue.ID, "checks_run", len(report.Checks))
		return o.route(ctx, issue, stage, "gate rejected candidate")

	default:
		return o.deadLetter(ctx, issue, fmt.Sprintf("unknown result status %q", res.Status))
	}
}

// runGate verifies the candidate in a fresh sandbox distinct from the producer's. The
// verification profile is the producing role's sandbox profile (same toolchain), but a
// brand-new sandbox — producer != verifier holds by construction (see
// specs/verification.md). The candidate is graded against the *stage's* declared
// postconditions, which the gate resolves to checks through its registry, so check
// selection is per-stage and config-driven (T2.1) rather than one hardcoded set. BaseRef
// is the ref the candidate branched from — the same value buildBrief seeds the producer's
// worktree at: the issue's threaded Base when it carries one (a stage produced from a
// predecessor's verified candidate, e.g. implement off the author-tests candidate that
// holds the failing acceptance tests), else the pipeline base (o.base, main). A red→green
// proof checks that ref out to confirm the acceptance tests fail without the change — so
// for implement the red half runs against the author-tests candidate, where the tests are
// present but the implementation is absent (T2.3/T2.5, see specs/verification.md).
func (o *Orchestrator) runGate(ctx context.Context, issue core.Issue, stage config.Stage, res core.Result) (gate.Report, error) {
	profile := ""
	if soul, ok := o.soulForRole(issue.Role); ok {
		profile = soul.Sandbox
	}
	baseRef := o.base
	if issue.Base != "" {
		baseRef = issue.Base
	}
	return o.gate.Run(ctx, gate.Candidate{
		Repo:           o.opts.Repo,
		Ref:            res.Branch.Ref,
		BaseRef:        baseRef,
		Postconditions: stage.Postcondition,
		Profile:        profile,
		Limits:         o.opts.Limits,
	})
}

// accept applies a verified Result: it writes any validated child-issue proposals
// (emergent breadth), advances the graph by the stage's declarative produces edges
// (depth), and closes the accepted issue. Ordering puts the idempotent-cheap close
// last; a crash between advancing and closing is re-processed on redelivery (re-gate,
// re-advance) — in the bootstrap's single serialized stream this window is narrow and
// the duplicate-produces risk is accepted, with a processed-marker hardening deferred
// to Phase 3.
func (o *Orchestrator) accept(ctx context.Context, issue core.Issue, stage config.Stage, res core.Result, report gate.Report) (bool, error) {
	if len(res.Proposes) > 0 {
		if _, err := o.bd.Apply(ctx, res.Proposes); err != nil {
			return true, fmt.Errorf("apply proposals for issue %s: %w", issue.ID, err)
		}
		o.log.Info("orchestrator: applied agent proposals", "issue", issue.ID, "count", len(res.Proposes))
	}

	for _, target := range stage.Produces {
		tstage, ok := o.opts.Config.Harness.DAG[target]
		if !ok {
			// validate guarantees produces targets exist; defense-in-depth.
			return false, fmt.Errorf("issue %s produces undefined stage %q", issue.ID, target)
		}
		if transient, err := o.advance(ctx, issue, target, tstage, res, report); err != nil {
			return transient, err
		}
	}

	if err := o.bd.Close(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("close accepted issue %s: %w", issue.ID, err)
	}
	o.log.Info("orchestrator: accepted", "issue", issue.ID, "produces", stage.Produces)
	return false, nil
}

// acceptPlan accepts a decomposition planner's result. A plan stage is an agent stage
// but is never verified in a sandbox: the planner produces no candidate branch, only the
// child issues it proposes (emergent breadth). Acceptance is therefore structural — the
// proposals were already checked for legal roles in handleResult; here we additionally
// require the planner produced at least one child and that each targets a role this stage
// declares it produces, then apply them and close the plan issue. There is no gate and no
// depth-advance: the proposals ARE the production (see specs/workflow.md "emergent
// breadth"). Restricting targets to the declared produces stops an untrusted planner from
// injecting work that skips stages (e.g. an implement issue with no author-tests).
func (o *Orchestrator) acceptPlan(ctx context.Context, issue core.Issue, stage config.Stage, res core.Result) (bool, error) {
	if len(res.Proposes) == 0 {
		// A planner that proposed nothing did not decompose the work; route it as a failure
		// so the retry cap eventually dead-letters rather than silently stalling the pipeline.
		return o.route(ctx, issue, stage, "planner produced no child issues")
	}
	allowed := map[string]bool{}
	for _, target := range stage.Produces {
		if ts, ok := o.opts.Config.Harness.DAG[target]; ok && ts.Role != "" {
			allowed[ts.Role] = true
		}
	}
	for _, p := range res.Proposes {
		if !allowed[p.Issue.Role] {
			return o.deadLetter(ctx, issue,
				fmt.Sprintf("illegal proposal: role %q is not a role stage %q produces", p.Issue.Role, issue.Role))
		}
	}
	if _, err := o.bd.Apply(ctx, res.Proposes); err != nil {
		return true, fmt.Errorf("apply planner proposals for issue %s: %w", issue.ID, err)
	}
	if err := o.bd.Close(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("close plan issue %s: %w", issue.ID, err)
	}
	o.log.Info("orchestrator: accepted decomposition", "issue", issue.ID, "children", len(res.Proposes))
	return false, nil
}

// advance applies one produces edge. A trusted-merge stage (integrate) is executed by
// the orchestrator inline — it fast-forwards the just-verified candidate onto main,
// which is the terminal state of the bootstrap pipeline. An agent stage is realized as
// a new ready issue carrying the produced role and the predecessor's just-verified
// candidate branch as its Base, so the downstream stage builds on the work already done
// rather than branching from main: this is what carries the author-tests candidate's
// failing tests into the implementor's worktree. The candidate branch persists in the
// repo after gating (it is never deleted; see specs/integration.md), so basing the new
// issue on it is sound.
func (o *Orchestrator) advance(ctx context.Context, issue core.Issue, target string, tstage config.Stage, res core.Result, report gate.Report) (bool, error) {
	if tstage.Kind == config.StageKindTrustedMerge {
		prov := o.provenanceFor(issue, res, report)
		commit, err := o.merger.Merge(ctx, o.opts.Repo, res.Branch.Ref, prov)
		if err != nil {
			return true, fmt.Errorf("merge candidate %s for issue %s: %w", res.Branch.Ref, issue.ID, err)
		}
		o.log.Info("orchestrator: merged to main", "issue", issue.ID, "ref", res.Branch.Ref, "commit", commit,
			"soul", prov.Soul, "model", prov.Model, "prompt_sha", prov.PromptSHA, "verified", prov.Verified)
		return false, nil
	}

	// Thread the test↔spec traceability map forward, like Base. The author-tests result
	// carries the freshly harvested map; later agent stages (e.g. implement) carry none of
	// their own, so they propagate the map already threaded onto this issue. Either way the
	// produced issue inherits it, so it survives to the integrate stage where it is cited
	// in the merge's provenance trailer (see specs/verification.md).
	traceMap := traceMapHash(res)
	if traceMap == "" {
		traceMap = issue.TraceMap
	}

	created, err := o.bd.Apply(ctx, []core.Proposal{{
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: tstage.Role, Base: res.Branch.Ref, TraceMap: traceMap},
	}})
	if err != nil {
		return true, fmt.Errorf("create produced %q issue from %s: %w", target, issue.ID, err)
	}
	for _, c := range created {
		o.log.Info("orchestrator: produced next-stage issue", "from", issue.ID, "stage", target, "new", c.ID, "base", res.Branch.Ref)
	}
	return false, nil
}

// route handles a rejected or failed issue. If the retry cap is spent it dead-letters;
// otherwise it creates a new fix issue at the stage's on_failure target carrying an
// incremented retry generation, and closes the original (its attempt is spent; the
// retry lives as a new issue, keeping the issue graph acyclic — see
// specs/workflow.md). The retry cap, enforced against the persistent Attempt counter,
// is the primary termination guarantee.
func (o *Orchestrator) route(ctx context.Context, issue core.Issue, stage config.Stage, reason string) (bool, error) {
	if issue.Attempt >= o.opts.Config.Harness.Policy.MaxRetries {
		return o.deadLetter(ctx, issue, fmt.Sprintf("%s; retry cap (%d) exhausted", reason, o.opts.Config.Harness.Policy.MaxRetries))
	}
	if stage.OnFailure == "" {
		// Every gate that can fail must have a defined route, or there is nowhere to send
		// the work; absent one, dead-letter rather than silently drop it.
		return o.deadLetter(ctx, issue, reason+"; stage has no on_failure route")
	}
	target, ok := o.opts.Config.Harness.DAG[stage.OnFailure]
	if !ok {
		return false, fmt.Errorf("issue %s on_failure target %q undefined", issue.ID, stage.OnFailure)
	}

	created, err := o.bd.Apply(ctx, []core.Proposal{{
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: target.Role, Attempt: issue.Attempt + 1, Base: issue.Base, TraceMap: issue.TraceMap},
	}})
	if err != nil {
		return true, fmt.Errorf("create on_failure fix issue from %s: %w", issue.ID, err)
	}
	if err := o.bd.Close(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("close routed issue %s: %w", issue.ID, err)
	}
	for _, c := range created {
		o.log.Info("orchestrator: routed failure to fix issue", "from", issue.ID, "to_stage", stage.OnFailure,
			"new", c.ID, "attempt", issue.Attempt+1, "reason", reason)
	}
	return false, nil
}

// deadLetter terminates an issue into the dead-letter queue for human triage: it
// publishes an alert to the DLQ subject and marks the issue blocked. A pathological
// issue can never wedge the pipeline — it always terminates here (see
// specs/workflow.md). Publishing precedes the block so a publish failure (transient)
// leaves the issue in_progress to be retried rather than silently blocked with no alert.
func (o *Orchestrator) deadLetter(ctx context.Context, issue core.Issue, reason string) (bool, error) {
	alert := dlqAlert{IssueID: issue.ID, Role: issue.Role, Attempt: issue.Attempt, Reason: reason}
	data, err := json.Marshal(alert)
	if err != nil {
		return false, fmt.Errorf("marshal dlq alert for %s: %w", issue.ID, err)
	}
	if _, err := o.js.Publish(ctx, o.dlq, data); err != nil {
		return true, fmt.Errorf("publish dlq alert for %s: %w", issue.ID, err)
	}
	if err := o.bd.Block(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("block dead-lettered issue %s: %w", issue.ID, err)
	}
	o.log.Warn("orchestrator: dead-lettered", "issue", issue.ID, "role", issue.Role, "attempt", issue.Attempt, "reason", reason)
	return false, nil
}

// dlqAlert is the payload published to the dead-letter subject. It carries enough for a
// human to find the issue and understand why it terminated; the full transcript/evidence
// lives in the artifact store keyed by the issue (see specs/observability.md).
type dlqAlert struct {
	IssueID string `json:"issue_id"`
	Role    string `json:"role"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
}
