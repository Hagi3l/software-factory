package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
)

// statusBlocked is the beads status a parked (or dead-lettered) issue holds. The approval
// handler acts on a Result only while its issue is blocked AND carries a parked candidate
// ref, which is what distinguishes an issue awaiting approval from one dead-lettered for
// good — and what keeps approval handling idempotent under at-least-once redelivery.
const statusBlocked = "blocked"

// approvalRequired reports whether a candidate must clear a human-approval gate before it
// integrates, plus a transient flag (a diff that could not be computed is an infrastructure
// fault to retry, not a verdict). Approval is required when the integrate stage explicitly
// declares the human-approved postcondition, when the profile is trusted-dev (every
// integrate), or — under autonomous — when the candidate's diff touches a TCB path. The
// fast paths avoid spending a git diff when the answer is already known (see
// specs/configuration.md, specs/bootstrap.md).
func (o *Orchestrator) approvalRequired(ctx context.Context, issue core.Issue, tstage config.Stage, candidateRef string) (required, transient bool, err error) {
	policy := o.opts.Config.Harness.Policy
	// An explicit human-approved postcondition on integrate, or the trusted-dev profile,
	// requires approval on every candidate regardless of its diff.
	if slices.Contains(tstage.Postcondition, core.PostconditionHumanApproved) || policy.Profile == config.ProfileTrustedDev {
		return true, false, nil
	}
	// Otherwise (autonomous) it is required only for a TCB-touching diff; with no TCB globs
	// configured nothing can be TCB-touching, so skip the diff entirely.
	if len(policy.TCBPaths) == 0 {
		return false, false, nil
	}
	files, derr := o.diffFiles(ctx, o.opts.Repo, o.base, candidateRef)
	if derr != nil {
		return false, true, fmt.Errorf("diff candidate %s for issue %s: %w", candidateRef, issue.ID, derr)
	}
	return policy.ApprovalRequired(files), false, nil
}

// parkAwaitingApproval holds an integrate candidate for human approval instead of merging it
// (T2.10). It captures the gate-verified provenance so the candidate can be merged on
// approval without being re-graded, records the candidate ref the approval must name, blocks
// the issue, and publishes an escalation so the control room surfaces it. It burns no retry
// — parking is not a failure — and returns errAwaitingApproval so accept stops without
// closing the issue; the approvals consumer resumes it when a human decides.
func (o *Orchestrator) parkAwaitingApproval(ctx context.Context, issue core.Issue, res core.Result, report gate.Report) (bool, error) {
	prov := o.provenanceFor(issue, res, report)
	provJSON, err := json.Marshal(prov)
	if err != nil {
		// Provenance that cannot be encoded is a degraded record, not a reason to drop the
		// park: store an empty provenance and proceed (the merge re-gate still rebuilds it on
		// a rebase; a fast-forward then carries a minimal trailer, self-describing like a
		// missing prompt sha).
		o.log.ErrorContext(ctx, "orchestrator: marshal parked provenance", "issue", issue.ID, "err", err)
		provJSON = nil
	}
	// Publish the escalation before blocking so a publish failure (transient) leaves the
	// issue in_progress to be retried rather than parked with no alert — mirroring deadLetter.
	alert := core.DLQAlert{IssueID: issue.ID, Role: issue.Role, Attempt: issue.Attempt,
		Reason: "awaiting human approval of integrate candidate; run: software-factory approve " + issue.ID}
	data, err := json.Marshal(alert)
	if err != nil {
		return false, fmt.Errorf("marshal approval escalation for %s: %w", issue.ID, err)
	}
	if _, err := o.js.Publish(ctx, o.dlq, data); err != nil {
		return true, fmt.Errorf("publish approval escalation for %s: %w", issue.ID, err)
	}
	if err := o.transition(ctx, issue, statusBlocked, func(ctx context.Context) error {
		return o.bd.AwaitApproval(ctx, issue.ID, res.Branch.Ref, string(provJSON))
	}); err != nil {
		return true, fmt.Errorf("park issue %s awaiting approval: %w", issue.ID, err)
	}
	o.log.WarnContext(ctx, "orchestrator: parked integrate candidate awaiting human approval",
		"issue", issue.ID, "candidate", res.Branch.Ref, "profile", o.opts.Config.Harness.Policy.Profile)
	return false, errAwaitingApproval
}

// consumeApprovals is the event-driven approvals reader, mirroring consumeResults: it pulls
// human approve/reject decisions off the orchestrator's durable consumer and processes each.
// Canceling ctx stops the iterator (a clean shutdown).
func (o *Orchestrator) consumeApprovals(ctx context.Context, cons jetstream.Consumer) error {
	iter, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("orchestrator: open approvals iterator: %w", err)
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
			return fmt.Errorf("orchestrator: pull approvals: %w", err)
		}
		o.handleApprovalMessage(ctx, msg)
	}
}

// handleApprovalMessage decodes one approval and applies the Ack/Nak/Term decision, the same
// taxonomy consumeResults uses: a processed decision is Acked, a transient failure Naked for
// redelivery, an undecodable message Termed (a redelivery cannot fix it).
func (o *Orchestrator) handleApprovalMessage(ctx context.Context, msg jetstream.Msg) {
	var req core.ApprovalRequest
	if err := json.Unmarshal(msg.Data(), &req); err != nil {
		o.log.ErrorContext(ctx, "orchestrator: undecodable approval, terminating", "err", err)
		_ = msg.TermWithReason("undecodable approval")
		return
	}
	if req.IssueID == "" {
		o.log.ErrorContext(ctx, "orchestrator: approval has no issue id, terminating")
		_ = msg.TermWithReason("approval without issue id")
		return
	}
	transient, err := o.handleApproval(ctx, req)
	switch {
	case err != nil && transient:
		o.log.ErrorContext(ctx, "orchestrator: transient failure handling approval, redelivering", "issue", req.IssueID, "err", err)
		_ = msg.Nak()
	case err != nil:
		o.log.ErrorContext(ctx, "orchestrator: permanent failure handling approval, terminating", "issue", req.IssueID, "err", err)
		_ = msg.TermWithReason("unprocessable approval")
	default:
		_ = msg.Ack()
	}
}

// handleApproval applies one human decision to a parked issue. It is idempotent: it acts
// only on an issue that is blocked AND carries a parked candidate ref (so a decision for a
// dead-lettered issue, or one already resumed/closed, is a no-op), and the decision must
// name the parked candidate (so a stale approval — the candidate changed — is ignored rather
// than applied to different code; see specs/configuration.md "human-approval").
func (o *Orchestrator) handleApproval(ctx context.Context, req core.ApprovalRequest) (transient bool, err error) {
	issue, err := o.bd.Get(ctx, req.IssueID)
	if err != nil {
		return true, fmt.Errorf("get issue %s: %w", req.IssueID, err)
	}
	if issue.Status != statusBlocked || issue.CandidateRef == "" {
		o.log.InfoContext(ctx, "orchestrator: approval for issue not awaiting approval, ignoring",
			"issue", issue.ID, "status", issue.Status, "approved", req.Approved)
		return false, nil
	}
	if req.CandidateSHA != "" && req.CandidateSHA != issue.CandidateRef {
		o.log.WarnContext(ctx, "orchestrator: approval candidate mismatch (stale), ignoring",
			"issue", issue.ID, "approved_sha", req.CandidateSHA, "parked_sha", issue.CandidateRef)
		return false, nil
	}
	stage, ok := o.stageForRole(issue.Role)
	if !ok {
		// A parked issue whose role lost its agent stage cannot be resumed; dead-letter it.
		return o.deadLetter(ctx, issue, "approval: issue role has no agent stage")
	}
	if !req.Approved {
		return o.rejectParked(ctx, issue, stage, req)
	}
	return o.resumeApproved(ctx, issue, stage, req)
}

// resumeApproved merges a parked candidate a human approved. It records the approval (who,
// which sha) for audit, replays the provenance captured at park (re-graded only if the merge
// must rebase, via the merge's own re-gate), and on a clean merge closes the parked issue
// (blocked → closed). A rebase conflict or re-gate failure routes through the same machinery
// as a first-pass integrate (resolveConflict / route), so approval does not bypass the
// two-green-branches guard.
func (o *Orchestrator) resumeApproved(ctx context.Context, issue core.Issue, stage config.Stage, req core.ApprovalRequest) (bool, error) {
	if err := o.bd.RecordApproval(ctx, issue.ID, issue.CandidateRef, req.Approver); err != nil {
		return true, fmt.Errorf("record approval on %s: %w", issue.ID, err)
	}
	var prov core.Provenance
	if issue.ParkedProvenance != "" {
		if err := json.Unmarshal([]byte(issue.ParkedProvenance), &prov); err != nil {
			o.log.ErrorContext(ctx, "orchestrator: decode parked provenance, merging with a degraded record", "issue", issue.ID, "err", err)
		}
	}
	prov.Issue = issue.ID
	// Reconstruct the minimal Result the merge's re-gate reads (the prompt sha for the
	// rebuilt provenance; the candidate ref for resolveConflict/route on a conflict).
	res := core.Result{IssueID: issue.ID, Branch: core.Branch{Ref: issue.CandidateRef}, Evidence: core.Evidence{PromptSHA: prov.PromptSHA}}
	transient, err := o.mergeCandidate(ctx, issue, stage, res, issue.CandidateRef, prov)
	if err != nil {
		if errors.Is(err, errMergeConflictHandled) || errors.Is(err, errMergeRegateRouted) {
			// resolveConflict / route already disposed of the issue; the decision is processed.
			return false, nil
		}
		return transient, err
	}
	// RecordApproval above is metadata-only (no status change), so it deliberately does NOT
	// announce — the issue is still blocked. The blocked→closed transition is this Close,
	// funneled through the choke point so the merge is what announces the closed event.
	if err := o.transition(ctx, issue, statusClosed, func(ctx context.Context) error {
		return o.bd.Close(ctx, issue.ID)
	}); err != nil {
		return true, fmt.Errorf("close integrated issue %s: %w", issue.ID, err)
	}
	o.log.InfoContext(ctx, "orchestrator: integrated approved candidate", "issue", issue.ID, "approver", req.Approver, "ref", issue.CandidateRef)
	return false, nil
}

// rejectParked handles a human rejecting a parked candidate. "Reject → fix / back to spec":
// it routes a fix attempt through the normal on_failure/retry machinery (which dead-letters
// for spec refinement if no route remains or the caps are spent). The just-finished
// invocation already charged its spend on the way in, so the reconstructed Result carries no
// usage — a reject adds no spend but does spend a retry, exactly like a gate rejection.
func (o *Orchestrator) rejectParked(ctx context.Context, issue core.Issue, stage config.Stage, req core.ApprovalRequest) (bool, error) {
	res := core.Result{IssueID: issue.ID, Branch: core.Branch{Ref: issue.CandidateRef}}
	o.log.WarnContext(ctx, "orchestrator: human rejected integrate candidate", "issue", issue.ID, "approver", req.Approver, "ref", issue.CandidateRef)
	return o.route(ctx, issue, stage, res, "human rejected integrate candidate", nil)
}

// gitChangedFiles lists the repo-relative paths a candidate ref changed relative to base,
// the input to the TCB-touching approval decision. It uses a three-dot diff (base...ref) so
// it reports only the candidate's own changes since the merge base, not files main moved
// underneath it. It is the default for Orchestrator.diffFiles; tests inject a fake.
func gitChangedFiles(ctx context.Context, repo, base, ref string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--name-only", base+"..."+ref) // #nosec G204 -- fixed git binary; base/ref are orchestrator-controlled refs, not untrusted input.
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...%s: %w", base, ref, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
