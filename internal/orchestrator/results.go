package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// statusInProgress is the beads status an issue holds while a runner owns it. The
// orchestrator acts on a Result only while its issue is in this state, which is what
// keeps Result processing idempotent under at-least-once redelivery.
const statusInProgress = "in_progress"

// statusClosed is the beads status of an issue whose candidate has merged (terminal,
// successful). The merged-delta sweep keys off it to find already-merged work whose
// governing spec has since changed (see recompileMergedDelta).
const statusClosed = "closed"

// errMergeConflictHandled is an internal sentinel: advance returns it when an integrate
// rebase conflict has already been handled — a sandboxed conflict-resolution issue spawned
// (the issue closed in its favor) or, if no resolve stage is configured or a cap/budget is
// spent, dead-lettered (the issue blocked). Either way it tells accept to stop without
// closing the issue again, and the message is Acked (redelivering the same Result cannot
// resolve the conflict; the spawned resolution carries the work forward).
var errMergeConflictHandled = errors.New("orchestrator: integrate conflict handled")

// errMergeRegateRouted is an internal sentinel: advance returns it when the rebased result
// failed the integrate re-gate and the issue has already been routed to a fix attempt (the
// two-green-branches case). Like errMergeConflictHandled it tells accept to stop without
// closing the issue again — route already closed it and spawned the fix — and the message
// is Acked.
var errMergeRegateRouted = errors.New("orchestrator: integrate re-gate failure routed")

// errAwaitingApproval is an internal sentinel: advance returns it when an integrate is
// held for human approval (T2.10) — the issue is parked (blocked, with its candidate ref and
// provenance recorded) rather than merged. Like the merge sentinels it tells accept to stop
// without closing the issue: a parked issue is resumed by the approvals consumer when a human
// approves, not by reprocessing the Result, so the Result is Acked.
var errAwaitingApproval = errors.New("orchestrator: integrate awaiting human approval")

// errEpicBudgetDeadLettered is an internal sentinel: advance returns it when producing the
// next agent stage would exceed the epic budget, so the issue was dead-lettered (blocked) with
// an epic-budget escalation instead of advancing. Like the merge sentinels it tells accept to
// stop without closing the issue again — the issue is already blocked — and the Result is Acked
// (reprocessing it cannot lower the epic's spend; only a human refining the spec can).
var errEpicBudgetDeadLettered = errors.New("orchestrator: epic budget exhausted, dead-lettered")

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

	// Record this invocation's marginal spend on the issue so the epic-budget aggregate read
	// (authorizeEpic) can sum it — every Result reaching here is a completed invocation that
	// burned real tokens, whatever its disposition (accepted, routed, dead-lettered). Stamped
	// before dispatching the disposition so any epic check below sees this attempt counted; it
	// is a set (StampClosingSpend), so an at-least-once redelivery of the same Result re-stamps
	// the same value. Skipped entirely when no epic budget is configured, so a config that does
	// not use it pays no extra write (see specs/workflow.md "epic_budget"). A failed stamp is
	// transient — leave the issue in_progress for redelivery rather than under-count the epic.
	if o.epicBudgetConfigured() {
		if err := o.bd.StampClosingSpend(ctx, issue.ID, res.Usage.TotalTokens(), o.priceUsage(issue, res.Usage)); err != nil {
			return true, fmt.Errorf("stamp closing spend on %s: %w", issue.ID, err)
		}
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
		// retry cap or budget is spent, dead-letter.
		return o.route(ctx, issue, stage, res, "agent reported failure")

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
			return o.route(ctx, issue, stage, res, "done result carried no candidate branch")
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
		return o.route(ctx, issue, stage, res, "gate rejected candidate")

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
	return o.gate.Run(ctx, o.gateCandidate(issue, stage, res.Branch.Ref))
}

// gateCandidate builds the gate's Candidate for grading ref under stage's postconditions in
// the issue's verification profile. It is shared by the initial branch gate (runGate, ref =
// the candidate branch) and the integrate re-gate (ref = the rebased result), so both grade
// the same stage's full check suite in the same fresh, producer-distinct sandbox — only the
// tree under test differs. BaseRef is the issue's threaded Base (where a red→green proof's
// red half runs) when set, else the pipeline base; for a stage whose checks need no base
// (the qa suite that gates integrate: tests-pass, scanners, mutation) it is simply unused.
func (o *Orchestrator) gateCandidate(issue core.Issue, stage config.Stage, ref string) gate.Candidate {
	profile := ""
	if soul, ok := o.selectSoul(issue); ok {
		profile = soul.Sandbox
	}
	baseRef := o.base
	if issue.Base != "" {
		baseRef = issue.Base
	}
	return gate.Candidate{
		Repo:           o.opts.Repo,
		Ref:            ref,
		BaseRef:        baseRef,
		Postconditions: stage.Postcondition,
		Profile:        profile,
		Limits:         o.opts.Limits,
	}
}

// reGate builds the ReGate the merger invokes after a clean rebase (specs/integration.md
// step 3). It re-runs the producing stage's full check suite — the same checks, same fresh
// producer-distinct sandbox as the branch gate — against the rebased result the merger
// published, so the verdict is against what will actually land on main rather than the
// branch as authored. This is what catches the two-green-branches case: a combination that
// each branch's isolated gate never saw. On a pass it returns provenance citing the
// re-gate's own checks (the combination's verification is the truth now); a clean failure
// (accepted=false) tells the merger to abort the merge so advance can route a fix issue; a
// gate that cannot reach a verdict surfaces as an error the merger propagates for retry.
func (o *Orchestrator) reGate(issue core.Issue, srcStage config.Stage, res core.Result) ReGate {
	return func(ctx context.Context, landedRef string) (core.Provenance, bool, error) {
		report, err := o.gate.Run(ctx, o.gateCandidate(issue, srcStage, landedRef))
		if err != nil {
			return core.Provenance{}, false, err
		}
		if !report.Passed {
			o.log.Info("orchestrator: re-gate rejected rebased result", "issue", issue.ID,
				"ref", landedRef, "checks_run", len(report.Checks))
			return core.Provenance{}, false, nil
		}
		return o.provenanceFor(issue, res, report), true, nil
	}
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
		if transient, err := o.advance(ctx, issue, stage, target, tstage, res, report); err != nil {
			if errors.Is(err, errMergeConflictHandled) || errors.Is(err, errMergeRegateRouted) ||
				errors.Is(err, errAwaitingApproval) || errors.Is(err, errEpicBudgetDeadLettered) {
				// advance already disposed of the issue: spawned a conflict-resolution issue (or
				// dead-lettered) for an integrate rebase conflict, routed it to a fix attempt for
				// a re-gate failure, parked it awaiting human approval (T2.10), or dead-lettered it
				// because producing the next agent stage would exceed the epic budget (T3.8b). In
				// every case it is no longer this accept's to close — stop here and Ack (a parked
				// issue is resumed by the approvals consumer, not by reprocessing this Result).
				return false, nil
			}
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
		return o.route(ctx, issue, stage, res, "planner produced no child issues")
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
	// Decomposition fans the epic out into the author-tests/implement work that burns the bulk
	// of its budget, so it is an "advancing a stage" point the epic budget guards: if the epic
	// has already spent its cap, dead-letter the plan issue rather than launch the fan-out
	// (specs/workflow.md). The plan invocation's own marginal was stamped in handleResult.
	if ok, transient, err := o.authorizeEpic(ctx, issue, "decomposing plan"); !ok {
		if err != nil {
			return transient, err
		}
		return false, nil
	}
	// Stamp the epic root id onto each proposed child so every issue of the epic shares it
	// (the planner's children branch from main as fresh work but still belong to this epic).
	// EpicID is the orchestrator's to assign — an agent cannot self-attribute its work to an
	// epic — so it is set here on the validated proposals, not trusted from the Result.
	epic := epicOf(issue)
	proposes := make([]core.Proposal, len(res.Proposes))
	for i, p := range res.Proposes {
		p.Issue.EpicID = epic
		proposes[i] = p
	}
	if _, err := o.bd.Apply(ctx, proposes); err != nil {
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
func (o *Orchestrator) advance(ctx context.Context, issue core.Issue, srcStage config.Stage, target string, tstage config.Stage, res core.Result, report gate.Report) (bool, error) {
	if tstage.Kind == config.StageKindTrustedMerge {
		// The human-approval gate (T2.10): under trusted-dev every integrate, or under
		// autonomous a TCB-touching diff, must be approved by a human before it lands. When
		// approval is required the candidate is PARKED awaiting it (burning no retry) rather
		// than merged here; a later `harness approve` resumes the merge through the approvals
		// consumer. An approval cannot already exist for an in_progress issue, so a required
		// gate always parks on this first pass (see specs/configuration.md, specs/bootstrap.md).
		required, transient, err := o.approvalRequired(ctx, issue, tstage, res.Branch.Ref)
		if err != nil {
			return transient, err
		}
		if required {
			return o.parkAwaitingApproval(ctx, issue, res, report)
		}
		return o.mergeCandidate(ctx, issue, srcStage, res, res.Branch.Ref, o.provenanceFor(issue, res, report))
	}

	// Producing the next agent stage launches more sandboxed work, so it is an "advancing a
	// stage" point the epic budget guards (specs/workflow.md): if the epic's aggregate spend
	// has reached its cap, dead-letter this issue with an epic-budget escalation rather than
	// fan out further work. The just-finished invocation's marginal was already stamped in
	// handleResult, so the aggregate read counts it. The trusted merge (handled above) is the
	// terminal landing and burns no agent spend, so it is deliberately not gated here — killing
	// a fully-verified candidate at the finish line would waste the whole epic.
	if ok, transient, err := o.authorizeEpic(ctx, issue, fmt.Sprintf("advancing to %q", target)); !ok {
		if err != nil {
			return transient, err
		}
		return false, errEpicBudgetDeadLettered
	}

	// Tags and Spec thread forward unchanged (issue.Tags / issue.Spec below): Tags are the
	// soul-selector input and Spec is the governing spec file, both set at issue-creation
	// (seed or planner), so every stage of an epic routes to the matching soul (a `lang=go`
	// epic stays on go souls) and resolves the same spec slice. Like Base/TraceMap they ride along.
	// EpicID threads forward likewise (epicOf(issue)) so every issue of the epic shares the root
	// seed's id — the key the epic budget aggregates over.
	//
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
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: tstage.Role, Base: res.Branch.Ref, Spec: issue.Spec, TraceMap: traceMap, Tags: issue.Tags, EpicID: epicOf(issue)},
	}})
	if err != nil {
		return true, fmt.Errorf("create produced %q issue from %s: %w", target, issue.ID, err)
	}
	for _, c := range created {
		o.log.Info("orchestrator: produced next-stage issue", "from", issue.ID, "stage", target, "new", c.ID, "base", res.Branch.Ref)
	}
	return false, nil
}

// mergeCandidate lands a verified candidate on main via the merge queue and disposes of the
// integrate outcomes uniformly. It is the shared merge path behind both the first-pass
// advance (when no approval is required) and the post-approval resume (when a human approves
// a parked candidate), so a rebase conflict or a re-gate failure is handled identically
// whether the merge ran immediately or after approval. prov is the provenance to record on a
// fast-forward (the parked/first-pass provenance); a rebase re-gate rebuilds it fresh from the
// landed tree via reGate, which reads res for the prompt sha. It returns the merge sentinels
// (errMergeConflictHandled / errMergeRegateRouted) when resolveConflict/route already disposed
// of the issue, so callers stop without closing it again.
func (o *Orchestrator) mergeCandidate(ctx context.Context, issue core.Issue, srcStage config.Stage, res core.Result, candidateRef string, prov core.Provenance) (bool, error) {
	commit, err := o.merger.Merge(ctx, o.opts.Repo, candidateRef, prov, o.reGate(issue, srcStage, res))
	if err != nil {
		if errors.Is(err, errRebaseConflict) {
			// The verified candidate cannot be cleanly rebased onto the current main tip:
			// another branch landed first and they textually collide. Retrying the same
			// candidate cannot help (the conflict is deterministic), so spawn a sandboxed
			// conflict-resolution issue — a merge-resolver agent rebases the candidate onto
			// main and resolves the conflicts, producing a new candidate that loops back
			// through integrate, re-gated like any other (specs/integration.md step 2). The
			// loop is bounded by the retry cap and budget; with no resolve stage configured
			// it falls back to dead-lettering. resolveConflict closes (or blocks) this issue,
			// so signal the caller to stop without closing it again.
			if transient, rerr := o.resolveConflict(ctx, issue, res, "integrate: candidate conflicts with current main; rebase needs resolution"); rerr != nil {
				return transient, rerr
			}
			return false, errMergeConflictHandled
		}
		if errors.Is(err, errReGateFailed) {
			// The candidate rebased cleanly but the re-gate found the combined tree broken:
			// two branches each green in isolation, broken together (specs/integration.md).
			// Unlike a conflict this may pass against a different main, so route a fix issue
			// through the normal retry/budget machinery rather than dead-lettering. route
			// closes this issue and spawns the fix, so signal the caller to stop without
			// closing it again.
			if transient, rerr := o.route(ctx, issue, srcStage, res, "integrate: re-gate of rebased result failed"); rerr != nil {
				return transient, rerr
			}
			return false, errMergeRegateRouted
		}
		return true, fmt.Errorf("merge candidate %s for issue %s: %w", candidateRef, issue.ID, err)
	}
	o.log.Info("orchestrator: merged to main", "issue", issue.ID, "ref", candidateRef, "commit", commit,
		"soul", prov.Soul, "model", prov.Model, "prompt_sha", prov.PromptSHA, "verified", prov.Verified)
	return false, nil
}

// route handles a rejected or failed issue. It accumulates the just-finished
// invocation's spend onto the retry chain's running total, then dead-letters if either
// termination guarantee is breached — the retry cap (number of attempts) or the
// cumulative per-issue budget (tokens/dollars those attempts burned). Otherwise it
// creates a new fix issue at the stage's on_failure target carrying an incremented retry
// generation and the new cumulative spend, and closes the original (its attempt is spent;
// the retry lives as a new issue, keeping the issue graph acyclic — see specs/workflow.md).
//
// The two guards are complementary: the retry cap alone bounds how many attempts run but
// not how much each burns, so a spec the factory cannot satisfy could otherwise consume
// unbounded tokens within the cap. The budget closes that gap (see specs/workflow.md's
// termination section). Spend is threaded exactly like Attempt: each route adds this
// attempt's spend (Result.Usage, priced through the selected soul's per-model cost table)
// to the predecessor's running total and stamps the sum on the fix issue.
func (o *Orchestrator) route(ctx context.Context, issue core.Issue, stage config.Stage, res core.Result, reason string) (bool, error) {
	spentTokens, spentUSD, spentWall, ok, transient, err := o.chargeAndAuthorize(ctx, issue, res, reason)
	if !ok {
		return transient, err
	}
	// The per-issue caps passed; the cross-issue epic budget is the other spawn guard. A fix
	// attempt is "another attempt" the epic budget bounds (specs/workflow.md), so check it
	// before spawning — the just-finished invocation's marginal was stamped in handleResult,
	// so the aggregate read counts it.
	if epicOK, t, e := o.authorizeEpic(ctx, issue, reason); !epicOK {
		return t, e
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
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: target.Role, Attempt: issue.Attempt + 1, Base: issue.Base, Spec: issue.Spec, TraceMap: issue.TraceMap, Tags: issue.Tags, SpentTokens: spentTokens, SpentUSD: spentUSD, SpentWall: spentWall, EpicID: epicOf(issue)},
	}})
	if err != nil {
		return true, fmt.Errorf("create on_failure fix issue from %s: %w", issue.ID, err)
	}
	if err := o.bd.Close(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("close routed issue %s: %w", issue.ID, err)
	}
	for _, c := range created {
		o.log.Info("orchestrator: routed failure to fix issue", "from", issue.ID, "to_stage", stage.OnFailure,
			"new", c.ID, "attempt", issue.Attempt+1, "spent_tokens", spentTokens, "spent_usd", spentUSD, "spent_wall", spentWall, "reason", reason)
	}
	return false, nil
}

// chargeAndAuthorize accumulates the just-finished invocation's spend onto the issue's
// retry chain and enforces both halves of the termination guarantee before another attempt
// is spawned: the retry cap (number of attempts) and the cumulative per-issue budget
// (tokens/dollars those attempts burned). If either is breached it dead-letters and returns
// ok=false carrying the dead-letter's (transient, err); otherwise it returns the new
// cumulative spend and ok=true so the caller can thread it onto the next attempt. It is the
// shared spend-and-check primitive behind both route (on_failure fixes) and resolveConflict
// (merge-conflict resolution), so the budget half of termination is enforced identically
// wherever the orchestrator spawns a follow-up attempt — a conflict loop is bounded the same
// way a fix loop is (see specs/workflow.md, specs/integration.md).
func (o *Orchestrator) chargeAndAuthorize(ctx context.Context, issue core.Issue, res core.Result, reason string) (spentTokens int, spentUSD float64, spentWall time.Duration, ok bool, transient bool, err error) {
	spentTokens = issue.SpentTokens + res.Usage.TotalTokens()
	spentUSD = issue.SpentUSD + o.priceUsage(issue, res.Usage)
	spentWall = issue.SpentWall + res.Elapsed

	if issue.Attempt >= o.opts.Config.Harness.Policy.MaxRetries {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; retry cap (%d) exhausted", reason, o.opts.Config.Harness.Policy.MaxRetries))
		return spentTokens, spentUSD, spentWall, false, t, e
	}
	// A zero budget dimension is uncapped (see config.Budget); only a configured cap that
	// the cumulative spend has reached dead-letters. Checked before spawning so a breach
	// terminates rather than spawning another attempt that would burn yet more.
	b := o.opts.Config.Harness.Policy.Budget
	if b.Tokens > 0 && spentTokens >= b.Tokens {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; per-issue token budget exhausted (%d >= %d)", reason, spentTokens, b.Tokens))
		return spentTokens, spentUSD, spentWall, false, t, e
	}
	if b.USD > 0 && spentUSD >= b.USD {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; per-issue USD budget exhausted ($%.4f >= $%.2f)", reason, spentUSD, b.USD))
		return spentTokens, spentUSD, spentWall, false, t, e
	}
	// Cumulative wall across the on_failure loop — the wall-clock analog of the token/dollar
	// caps, distinct from the per-invocation sandbox ceiling: it bounds how long the whole
	// feedback loop for one issue may run, not a single attempt (see specs/workflow.md).
	if w := b.Wall.Duration(); w > 0 && spentWall >= w {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; per-issue wall budget exhausted (%s >= %s)", reason, spentWall, w))
		return spentTokens, spentUSD, spentWall, false, t, e
	}
	return spentTokens, spentUSD, spentWall, true, false, nil
}

// resolveStage finds the DAG stage that handles merge-conflict resolution (kind: resolve),
// the stage the orchestrator spawns a sandboxed resolution issue into on a rebase conflict.
// It returns (stage, true) if the config declares one; a config with no resolve stage (the
// kernel, the spine-e2e fixture) gets (zero, false) and resolveConflict falls back to
// dead-lettering, preserving the pre-T3.11 behavior. At most one resolve stage is expected;
// validation does not forbid several, so the first by sorted name is taken deterministically.
func (o *Orchestrator) resolveStage() (config.Stage, bool) {
	if o.opts.Config.Harness == nil {
		return config.Stage{}, false
	}
	for _, st := range o.opts.Config.Harness.DAG {
		if st.Kind == config.StageKindResolve {
			return st, true
		}
	}
	return config.Stage{}, false
}

// resolveConflict spawns a sandboxed conflict-resolution issue when a verified candidate
// cannot be cleanly rebased onto the current main tip (specs/integration.md step 2: spawn a
// resolution issue, block, loop). Rather than dead-lettering (the kernel's behavior before
// T3.11), it creates a new issue for the resolve stage seeded at the conflicting candidate
// (Base = res.Branch.Ref): a merge-resolver agent rebases that candidate onto main in its
// sandbox, resolves the conflicts, and submits a new candidate which — re-gated like any
// other (producer != verifier) — produces a fresh integrate attempt onto whatever main has
// since become. The conflict loop is bounded by the same termination guarantees as route
// (the retry cap and the cumulative per-issue budget, via chargeAndAuthorize), so a
// pathological conflict that no rebase resolves eventually dead-letters rather than looping
// forever. If the config declares no resolve stage, conflict resolution is unavailable and
// it falls back to dead-lettering for human triage.
//
// The trusted layer never hands main to an untrusted agent: the merge-resolver only proposes
// a rebased candidate; the orchestrator re-gates it and performs the final git write itself.
func (o *Orchestrator) resolveConflict(ctx context.Context, issue core.Issue, res core.Result, reason string) (bool, error) {
	rstage, ok := o.resolveStage()
	if !ok {
		return o.deadLetter(ctx, issue, reason+"; no resolve stage configured")
	}
	spentTokens, spentUSD, spentWall, ok, transient, err := o.chargeAndAuthorize(ctx, issue, res, reason)
	if !ok {
		return transient, err
	}
	if epicOK, t, e := o.authorizeEpic(ctx, issue, reason); !epicOK {
		return t, e
	}
	// Base is the conflicting candidate the resolver must rebase onto main — not issue.Base
	// (its own base), which is where it branched from. Attempt/spend thread forward like
	// route so the conflict loop counts against the same caps; Spec/Tags/TraceMap/EpicID ride
	// along so the resolution stays on the epic's souls, keeps the traceability chain intact,
	// and stays attributed to the same epic.
	created, err := o.bd.Apply(ctx, []core.Proposal{{
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: rstage.Role, Attempt: issue.Attempt + 1, Base: res.Branch.Ref, Spec: issue.Spec, TraceMap: issue.TraceMap, Tags: issue.Tags, SpentTokens: spentTokens, SpentUSD: spentUSD, SpentWall: spentWall, EpicID: epicOf(issue)},
	}})
	if err != nil {
		return true, fmt.Errorf("create conflict-resolution issue from %s: %w", issue.ID, err)
	}
	if err := o.bd.Close(ctx, issue.ID); err != nil {
		return true, fmt.Errorf("close conflicted issue %s: %w", issue.ID, err)
	}
	for _, c := range created {
		o.log.Info("orchestrator: spawned conflict-resolution issue", "from", issue.ID, "new", c.ID,
			"role", rstage.Role, "base", res.Branch.Ref, "attempt", issue.Attempt+1, "reason", reason)
	}
	return false, nil
}

// priceUsage converts an invocation's token usage to dollars at the issue's selected
// model's published rate (the per-model cost table in the infra model registry). It
// returns 0 when the infra overlay is absent, the model is unknown, or the model declares
// no cost block — USD accounting then contributes nothing and spend stays bounded by the
// token and retry caps, which never depend on the table. Selection mirrors the producer's
// (the same soul that ran the invocation), so the price is the model that actually billed.
func (o *Orchestrator) priceUsage(issue core.Issue, u core.Usage) float64 {
	soul, ok := o.selectSoul(issue)
	if !ok || o.opts.Config.Infra == nil {
		return 0
	}
	mp, ok := o.opts.Config.Infra.Models[soul.Model]
	if !ok {
		return 0
	}
	return mp.Cost.USD(u)
}

// epicOf returns the id of an issue's epic: its threaded EpicID when set, else its own id.
// A root seed carries no EpicID — it IS its own epic — so it falls back to its id, exactly as
// Base falls back to the pipeline base for a freshly seeded issue. This single definition is
// what lets the epic-budget aggregate include the root alongside its descendants (which all
// carry the root's id), with no extra write to stamp the root's own id onto itself.
func epicOf(i core.Issue) string {
	if i.EpicID != "" {
		return i.EpicID
	}
	return i.ID
}

// epicBudgetConfigured reports whether any epic-budget dimension is set. When none is, the
// orchestrator skips both the per-result closing-spend write and the aggregate read entirely,
// so a config that does not use an epic budget pays nothing for the feature (the epic budget is
// tokens/dollars only — the wall cap is per-issue cumulative, enforced in chargeAndAuthorize).
func (o *Orchestrator) epicBudgetConfigured() bool {
	eb := o.opts.Config.Harness.Policy.EpicBudget
	return eb.Tokens > 0 || eb.USD > 0
}

// authorizeEpic enforces the cross-issue epic budget before the orchestrator spawns more work
// for an epic (a retry, a conflict resolution, or advancing to the next agent stage). Because an
// epic is a fan-out DAG rather than a line, its total spend cannot be a counter threaded down
// each branch (the shared prefix would be double-counted at the join); it is read as an
// AGGREGATE — the sum of every issue's own-invocation marginal (ClosingTokens/ClosingUSD,
// stamped in handleResult) over all issues sharing this epic id. On a breach it dead-letters the
// issue with an epic-budget escalation and returns ok=false; otherwise ok=true. The single-writer
// orchestrator evaluates this serially, so concurrent siblings cannot race the check (a sibling
// still in flight has not stamped its closing spend yet and is counted when its own Result lands,
// never double-counted — see specs/workflow.md). It is a no-op (ok=true) when no epic budget is
// configured, skipping the ListAll, so the common case pays nothing.
func (o *Orchestrator) authorizeEpic(ctx context.Context, issue core.Issue, reason string) (ok, transient bool, err error) {
	eb := o.opts.Config.Harness.Policy.EpicBudget
	if eb.Tokens == 0 && eb.USD == 0 {
		return true, false, nil
	}
	epic := epicOf(issue)
	all, err := o.bd.ListAll(ctx)
	if err != nil {
		return false, true, fmt.Errorf("list all for epic budget of %s: %w", issue.ID, err)
	}
	var tokens int
	var usd float64
	for _, other := range all {
		if epicOf(other) == epic {
			tokens += other.ClosingTokens
			usd += other.ClosingUSD
		}
	}
	if eb.Tokens > 0 && tokens >= eb.Tokens {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; epic token budget exhausted (%d >= %d)", reason, tokens, eb.Tokens))
		return false, t, e
	}
	if eb.USD > 0 && usd >= eb.USD {
		t, e := o.deadLetter(ctx, issue, fmt.Sprintf("%s; epic USD budget exhausted ($%.4f >= $%.2f)", reason, usd, eb.USD))
		return false, t, e
	}
	return true, false, nil
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
