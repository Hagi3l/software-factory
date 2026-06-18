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
// It is idempotent: it acts only while the issue is in the in-flight projection, so a
// duplicate or stale redelivery for an issue already accepted, rejected, or dead-lettered
// is a no-op — the terminal transition removed it from the projection (see
// specs/components/orchestrator.md "Live state vs. durable state").
func (o *Orchestrator) handleResult(ctx context.Context, res core.Result) (transient bool, err error) {
	// Gate on the in-flight projection, NOT a beads status read. beads is not read-your-writes
	// consistent under load, so a *valid* result can arrive before a status read would show
	// in_progress (the claim write still propagating) — discarding it as "stale" was the observed
	// bug. The projection is the single writer's own live record, so it never lags its own writes:
	// a result is live iff its issue is in the projection. A duplicate or stale redelivery is
	// ignored because the first result's terminal transition already removed the issue, and result
	// handling is serial so that removal lands before the next is processed — so a duplicate can
	// never re-Apply (specs/components/orchestrator.md "Live state vs. durable state").
	if !o.inflight.has(res.IssueID) {
		o.log.Info("orchestrator: result for issue not in flight, ignoring as stale/duplicate",
			"issue", res.IssueID, "result_status", res.Status)
		return false, nil
	}
	issue, err := o.bd.Get(ctx, res.IssueID)
	if err != nil {
		return true, fmt.Errorf("get issue %s: %w", res.IssueID, err)
	}

	// Record this invocation's priced spend for the cost view (specs/control-room.md T4.10).
	// The orchestrator is the only component that holds the per-model price table, so it
	// prices here, once per result, keyed by the model that ran. Recorded for every
	// disposition (the spend was real); a zero cost (unpriced model) records nothing. Unlike
	// the beads closing-spend stamp below — which is idempotent under redelivery because
	// budget enforcement must be exact — this is a monotonic observability counter, so an
	// at-least-once redelivery may modestly over-count it; that is acceptable for a metric.
	if soul, ok := o.selectSoul(issue); ok {
		o.tel.RecordCost(ctx, soul.Model, o.priceUsage(issue, res.Usage))
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

	// Record the most-recent invocation's transcript hash on the issue so the decision trail is
	// reachable from the issue itself — for in-flight and dead-lettered work, not only from a
	// merge trailer (which exists only for merged work). This is what lets the Resolve wizard
	// (T4.15) pre-load "the agent transcript that raised the escalation" and the replay view
	// reconstruct a non-merged invocation. Stamped for EVERY disposition (the run happened
	// whatever the outcome) and idempotent under redelivery (a set). It is observability, not a
	// correctness gate, so a failed stamp is logged and the disposition proceeds rather than
	// looping the Result on a non-critical write — and an empty hash (no transcript harvested)
	// is a no-op (see core.Issue.Transcript, specs/observability.md).
	if hash := transcriptHash(res); hash != "" {
		if err := o.bd.StampTranscript(ctx, issue.ID, hash); err != nil {
			o.log.Warn("orchestrator: stamp transcript failed (non-fatal)", "issue", issue.ID, "err", err)
		}
	}

	// Record this invocation's transformation-log hash on the issue (T6.3) so the verification
	// view can weigh which of its writes were precise (semantic) and which fell back to the text
	// floor — for a dead-lettered candidate as much as a merged one. Stamped for EVERY
	// disposition, idempotent (a set), non-fatal observability like the transcript stamp; an
	// empty hash (the invocation ran no semantic write tools) is a no-op (see
	// core.Issue.TransformLog).
	if hash := transformLogHash(res); hash != "" {
		if err := o.bd.StampTransformLog(ctx, issue.ID, hash); err != nil {
			o.log.Warn("orchestrator: stamp transform log failed (non-fatal)", "issue", issue.ID, "err", err)
		}
	}

	stage, ok := o.stageForRole(issue.Role)
	if !ok {
		// An in_progress issue whose role has no agent stage should never have been
		// dispatched; it cannot be advanced, so dead-letter it for human triage.
		return o.deadLetter(ctx, issue, "issue role has no agent stage")
	}

	// Stamp this issue's own producing soul as its stage runs, keyed off the stage's reserved
	// proof: the author-tests stage's soul lands as TestsSoul, the implement stage's as
	// ImplementSoul. Recording it here — for every disposition, before the switch — is what
	// makes producer ≠ verifier *demonstrable* after the fact (the choice is otherwise transient),
	// and threads it forward from this issue (advance/route below propagate issue.TestsSoul /
	// issue.ImplementSoul). It mutates the in-memory issue too, so the just-stamped soul rides
	// into the produced child / fix issue. Non-fatal like the transcript stamp: it is audit
	// metadata, not a correctness gate, so a failed write is logged and the disposition proceeds.
	o.stampProducingSoul(ctx, &issue, stage)

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
		// Stamp the assembled gate-verdict record's hash onto the issue for every disposition
		// (accept or route below), so the verification view can render this gate run's trust
		// argument — including for a *rejected* candidate, which has no merge trailer to carry it
		// (T4.22). Non-fatal like the transcript/soul stamps: an empty hash (no store / failed
		// harvest) is a no-op, and a failed write is logged rather than looping the Result.
		if hash := report.Verdict.Hash; hash != "" {
			if err := o.bd.StampGateVerdict(ctx, issue.ID, hash); err != nil {
				o.log.Warn("orchestrator: stamp gate verdict failed (non-fatal)", "issue", issue.ID, "err", err)
			}
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
	// Resolve the logical profile to the concrete artifact the verifier boots, so the gate
	// grades on the *same* (ideally digest-pinned) toolchain image the producer used —
	// producer != verifier, same toolchain (see specs/components/sandbox.md). Empty when
	// no infra is wired (some tests); the backend then falls back to the profile name.
	image := ""
	if o.opts.Config != nil && o.opts.Config.Infra != nil {
		image = o.opts.Config.Infra.Sandbox.ResolveImage(profile)
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
		Image:          image,
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

	if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
		return o.bd.Close(ctx, issue.ID)
	}); err != nil {
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
	// Belt-and-suspenders idempotency for the non-atomic Apply→Close window (T3.12). The
	// in-flight projection already stops a duplicate Result from re-entering here within a
	// process; this guards the one remaining gap: Apply succeeds, then the Close transition
	// fails (transient) or the orchestrator crashes — the plan stays in_progress, so a redelivery
	// or a lease-sweep redispatch runs acceptPlan again. Without this it would Apply a SECOND
	// decomposition (the corruption observed in the demo run: 4 children for a 2-child feature).
	// The plan's children carry it in their DependsOn (a parent link added below), so their
	// existence is the durable proof the decomposition already ran: if they exist, skip Apply and
	// just finish closing the plan. This must precede the no-proposals/role checks, since the
	// redelivered Result may be a fresh planner attempt with different (or no) proposals — they
	// are moot once the decomposition exists.
	hasChildren, err := o.planHasChildren(ctx, issue.ID)
	if err != nil {
		return true, fmt.Errorf("check plan %s children: %w", issue.ID, err)
	}
	if hasChildren {
		o.log.Info("orchestrator: plan already decomposed (children exist); closing without re-applying", "issue", issue.ID)
		if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
			return o.bd.Close(ctx, issue.ID)
		}); err != nil {
			return true, fmt.Errorf("close already-decomposed plan issue %s: %w", issue.ID, err)
		}
		return false, nil
	}
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
		// Add a parent link: each child is blocked-by the plan issue, so it does not dispatch
		// until the decomposition is accepted (the plan closes — a closed blocker no longer
		// blocks, exactly like the planner's own inter-sibling DependsOn edges). This serves two
		// ends: the child becomes ready only once its decomposition is committed, and the edge is
		// the durable parent link planHasChildren reads to make a re-run of acceptPlan idempotent
		// (T3.12). Copied, not appended in place, so a shared backing array can't alias siblings.
		p.DependsOn = append(append([]string{}, p.DependsOn...), issue.ID)
		proposes[i] = p
	}
	if _, err := o.bd.Apply(ctx, proposes); err != nil {
		return true, fmt.Errorf("apply planner proposals for issue %s: %w", issue.ID, err)
	}
	if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
		return o.bd.Close(ctx, issue.ID)
	}); err != nil {
		return true, fmt.Errorf("close plan issue %s: %w", issue.ID, err)
	}
	o.log.Info("orchestrator: accepted decomposition", "issue", issue.ID, "children", len(res.Proposes))
	return false, nil
}

// planHasChildren reports whether a plan issue's decomposition has already been applied, by
// looking for any issue that lists the plan in its DependsOn — the parent link acceptPlan stamps
// on every child it creates. It is the durable idempotency signal for the Apply→Close window
// (T3.12): children created by a prior acceptPlan persist even if that attempt failed before
// closing the plan, so their presence proves the decomposition ran and must not run again. It
// reads ListAll because there is no by-parent beads query and a plan's children may be in any
// status; this is acceptable because acceptPlan runs once per successful plan (a rare event), not
// on the hot dispatch path. A re-derivation plan (recompileMergedDelta) is fresh and links no
// children to itself, so it is correctly seen as un-decomposed and the original epic's issues —
// which depend on a *different* plan — never false-positive it.
func (o *Orchestrator) planHasChildren(ctx context.Context, planID string) (bool, error) {
	all, err := o.bd.ListAll(ctx)
	if err != nil {
		return false, fmt.Errorf("list all issues: %w", err)
	}
	for _, is := range all {
		for _, dep := range is.DependsOn {
			if dep == planID {
				return true, nil
			}
		}
	}
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

	// The producing souls thread forward like TraceMap (issue.TestsSoul / issue.ImplementSoul
	// below): stampProducingSoul recorded this issue's own soul onto the in-memory issue earlier
	// in handleResult, so the produced child inherits the author-tests / implement identities and
	// the producer≠verifier split survives to integrate and the verification view (T4.22).
	created, err := o.bd.Apply(ctx, []core.Proposal{{
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: tstage.Role, Base: res.Branch.Ref, Spec: issue.Spec, TraceMap: traceMap, Tags: issue.Tags, EpicID: epicOf(issue), TestsSoul: issue.TestsSoul, ImplementSoul: issue.ImplementSoul},
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
	// Announce the merge-queue lifecycle (T4.24). queued on entry (the candidate has reached the
	// serialized integrate step); the merger announces its internal rebasing/re-gating steps via
	// the progress closure (which just publishes — the merger stays NATS-unaware); and the
	// orchestrator announces the terminal step from the Merge return below (landed / conflicted /
	// regate-failed), so every step a candidate passes through is visible in the merge-queue view.
	o.announceMergeState(issue, core.MergeStateQueued, "")
	progress := func(state string) { o.announceMergeState(issue, state, "") }
	// The branch the candidate integrates onto: refs/heads/main in per-item mode, or the issue's
	// epic branch in epic mode, where children land atomically and main advances only at the
	// epic's terminal merge (T7.3). The merger creates the epic branch off main on first use.
	target := o.integrationTargetRef(issue)
	commit, err := o.merger.Merge(ctx, o.opts.Repo, candidateRef, target, prov, o.reGate(issue, srcStage, res), progress)
	if err != nil {
		if errors.Is(err, errRebaseConflict) {
			o.announceMergeState(issue, core.MergeStateConflicted, "")
			// The verified candidate cannot be cleanly rebased onto the current integration
			// target: another branch landed first and they textually collide. Retrying the same
			// candidate cannot help (the conflict is deterministic), so spawn a sandboxed
			// conflict-resolution issue — a merge-resolver agent rebases the candidate onto the
			// integration branch and resolves the conflicts, producing a new candidate that loops
			// back through integrate, re-gated like any other (specs/integration.md step 2). The
			// loop is bounded by the retry cap and budget; with no resolve stage configured
			// it falls back to dead-lettering. resolveConflict closes (or blocks) this issue,
			// so signal the caller to stop without closing it again.
			if transient, rerr := o.resolveConflict(ctx, issue, res, "integrate: candidate conflicts with the current integration branch; rebase needs resolution"); rerr != nil {
				return transient, rerr
			}
			return false, errMergeConflictHandled
		}
		if errors.Is(err, errReGateFailed) {
			o.announceMergeState(issue, core.MergeStateRegateFailed, "")
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
	o.announceMergeState(issue, core.MergeStateLanded, commit)
	// Stamp the durable integration marker before the bead is closed (accept closes it once this
	// returns). `closed` alone cannot distinguish an integration from a superseded retry or the
	// closed epic root, so the epic roll-up counts THIS marker — written the instant the candidate
	// lands, for both per-item and epic mode (specs/integration.md "Integrated vs. closed", T8.3).
	// Best-effort: a stamp failure must not undo a landed merge (the commit is already on the
	// integration branch), so it is logged, not fatal — a cold-start rebuild re-reads the marker
	// and the roll-up is a progress hint, not a correctness gate.
	if err := o.bd.StampIntegrated(ctx, issue.ID); err != nil {
		o.log.Warn("orchestrator: stamp integrated marker failed (merge already landed)", "issue", issue.ID, "err", err)
	}
	// Name the real integration target: in epic mode a child lands on epic/<epic_id>, not main —
	// main advances only at the epic's terminal merge (T8.6, specs/integration.md).
	o.log.Info("orchestrator: merged candidate", "issue", issue.ID, "target", o.integrationBranchName(issue),
		"ref", candidateRef, "commit", commit,
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
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: target.Role, Attempt: issue.Attempt + 1, Base: issue.Base, Spec: issue.Spec, TraceMap: issue.TraceMap, Tags: issue.Tags, SpentTokens: spentTokens, SpentUSD: spentUSD, SpentWall: spentWall, EpicID: epicOf(issue), TestsSoul: issue.TestsSoul, ImplementSoul: issue.ImplementSoul},
	}})
	if err != nil {
		return true, fmt.Errorf("create on_failure fix issue from %s: %w", issue.ID, err)
	}
	if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
		return o.bd.Close(ctx, issue.ID)
	}); err != nil {
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
		Issue: core.Issue{Title: issue.Title, Body: issue.Body, Role: rstage.Role, Attempt: issue.Attempt + 1, Base: res.Branch.Ref, Spec: issue.Spec, TraceMap: issue.TraceMap, Tags: issue.Tags, SpentTokens: spentTokens, SpentUSD: spentUSD, SpentWall: spentWall, EpicID: epicOf(issue), TestsSoul: issue.TestsSoul, ImplementSoul: issue.ImplementSoul},
	}})
	if err != nil {
		return true, fmt.Errorf("create conflict-resolution issue from %s: %w", issue.ID, err)
	}
	if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
		return o.bd.Close(ctx, issue.ID)
	}); err != nil {
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

// stampProducingSoul records the issue's own producing soul onto it, keyed off which
// reserved proof the issue's stage carries: the author-tests stage (tests-red) stamps
// TestsSoul, the implement stage (red→green) stamps ImplementSoul. It is the orchestrator
// turning a transient dispatch-time choice (selectSoul) into a durable, after-the-fact
// record of producer ≠ verifier (specs/verification.md "The separation is recorded"). It
// mutates the in-memory issue so the freshly stamped soul threads forward onto the produced
// child / fix issue in this same handleResult pass. A stage carrying neither proof (plan,
// qa, resolve) records no soul — only the two producing stages have a recordable identity.
// Non-fatal: a failed write is logged and the in-memory issue is left unstamped (audit
// metadata, not a correctness gate); a re-stamp under redelivery is idempotent (a set).
func (o *Orchestrator) stampProducingSoul(ctx context.Context, issue *core.Issue, stage config.Stage) {
	soul, ok := o.selectSoul(*issue)
	if !ok {
		return
	}
	switch {
	case stageProves(stage, core.PostconditionTestsRed):
		if issue.TestsSoul == soul.Name {
			return
		}
		if err := o.bd.StampSouls(ctx, issue.ID, soul.Name, ""); err != nil {
			o.log.Warn("orchestrator: stamp tests-soul failed (non-fatal)", "issue", issue.ID, "err", err)
			return
		}
		issue.TestsSoul = soul.Name
	case stageProves(stage, core.PostconditionRedGreen):
		if issue.ImplementSoul == soul.Name {
			return
		}
		if err := o.bd.StampSouls(ctx, issue.ID, "", soul.Name); err != nil {
			o.log.Warn("orchestrator: stamp implement-soul failed (non-fatal)", "issue", issue.ID, "err", err)
			return
		}
		issue.ImplementSoul = soul.Name
	}
}

// stageProves reports whether a stage declares the given reserved proof as a postcondition.
// It is how the orchestrator identifies the author-tests stage (PostconditionTestsRed) and
// the implement stage (PostconditionRedGreen) without a hardcoded role name — the same
// principled signal config validation and the diversity warning key off (see
// specs/verification.md, internal/config/warnings.go).
func stageProves(stage config.Stage, proof string) bool {
	for _, pc := range stage.Postcondition {
		if pc == proof {
			return true
		}
	}
	return false
}

// epicOf returns the id of an issue's epic: its threaded EpicID when set, else its own id.
// A root seed carries no EpicID — it IS its own epic — so it falls back to its id, exactly as
// epicOf is the orchestrator's local spelling of the epic-grouping rule, delegating to the
// single source core.EpicOf so the aggregate epic budget here and the control room's budget
// view (T4.10) group identically. It lets the epic-budget aggregate include the root
// alongside its descendants (which all carry the root's id), with no write to stamp the
// root's own id onto itself.
func epicOf(i core.Issue) string { return core.EpicOf(i) }

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
	alert := core.DLQAlert{IssueID: issue.ID, Role: issue.Role, Attempt: issue.Attempt, Reason: reason}
	data, err := json.Marshal(alert)
	if err != nil {
		return false, fmt.Errorf("marshal dlq alert for %s: %w", issue.ID, err)
	}
	if _, err := o.js.Publish(ctx, o.dlq, data); err != nil {
		return true, fmt.Errorf("publish dlq alert for %s: %w", issue.ID, err)
	}
	if err := o.transition(ctx, issue, statusBlocked, func(ctx context.Context) error {
		return o.bd.Block(ctx, issue.ID, reason)
	}); err != nil {
		return true, fmt.Errorf("block dead-lettered issue %s: %w", issue.ID, err)
	}
	o.log.Warn("orchestrator: dead-lettered", "issue", issue.ID, "role", issue.Role, "attempt", issue.Attempt, "reason", reason)
	return false, nil
}

// The dead-letter payload is core.DLQAlert — a single source shared by this write side and
// the control-room DLQ pump that tails harness.dlq to fire the browser escalation alert (T4.19).
