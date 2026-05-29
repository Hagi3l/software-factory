package orchestrator

import (
	"context"

	"github.com/Loxstomper/harness/internal/spec"
)

// recompileSpecDelta is the orchestrator's response to a spec edit — "the factory recompiles
// the delta" (see specs/specs-process.md). A spec file is a human's only lever, and it may be
// refined while work is in flight. The agents cannot see that change: each issue's spec slice
// is materialized once, at dispatch, so keeping in-flight work aligned with an edited spec
// must be structural, not a prompt instruction. Each in_progress issue pinned the content
// hash of the slice it was briefed against (T3.6, scheduleReady/PinSpecHash); this sweep
// re-resolves that slice from the integration repo, re-hashes it, and compares it to the pin.
// A mismatch means the spec version underneath the work changed, so the issue is reissued —
// returned to the ready pool to be re-dispatched against the new slice — and the in-flight
// attempt's eventual Result is ignored because the issue is no longer in_progress, exactly as
// for a released stranded lease.
//
// Re-resolving every in_progress issue every tick subsumes "diff which issues referenced the
// edited file": an issue whose slice does not include the edited file re-hashes to the same
// value and is left untouched, so no explicit edit-event channel or membership test is needed.
// The in_progress set is small (bounded by concurrent dispatch) and resolution is a handful
// of local file reads, so this is cheap in the bootstrap; a future optimization could gate it
// on a cheap spec-tree change signal (T3.7b).
//
// Scope: only in_progress issues are swept. A not-yet-dispatched issue carries no pin and
// will resolve the current slice when it dispatches; a terminal (closed/blocked) issue is
// past re-dispatch — re-deriving already-merged work for a spec diff (spawning new issues for
// the delta) is a separate, less-defined concern deferred to T3.7b. Re-resolution is
// best-effort, mirroring buildBrief's discipline: an issue with no spec or no pin is skipped,
// and a slice that fails to resolve (the file is mid-edit or was deleted) is logged and left
// alone rather than disrupting live work on an ambiguous signal.
func (o *Orchestrator) recompileSpecDelta(ctx context.Context) {
	inflight, err := o.bd.InProgress(ctx)
	if err != nil {
		o.log.Error("orchestrator: list in_progress for spec-drift sweep", "err", err)
		return
	}
	for _, issue := range inflight {
		// No spec reference (nothing to resolve) or no pin yet (dispatched before the slice
		// could be pinned — degraded, not drifted): there is no version to diff against.
		if issue.Spec == "" || issue.SpecHash == "" {
			continue
		}
		slice, err := spec.Resolve(o.opts.Repo, issue.Spec, o.opts.Config.Harness.SpecDepth)
		if err != nil {
			o.log.Error("orchestrator: resolve spec slice for drift check; leaving in-flight work untouched",
				"issue", issue.ID, "spec", issue.Spec, "err", err)
			continue
		}
		current := spec.Hash(slice)
		if current == issue.SpecHash {
			continue
		}
		if err := o.bd.Reissue(ctx, issue.ID); err != nil {
			o.log.Error("orchestrator: reissue spec-drifted issue", "issue", issue.ID, "err", err)
			continue
		}
		o.log.Info("orchestrator: spec drift detected; reissued stale in-flight work",
			"issue", issue.ID, "spec", issue.Spec, "pinned", issue.SpecHash, "current", current)
	}
}
