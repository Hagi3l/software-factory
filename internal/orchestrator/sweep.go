package orchestrator

import (
	"context"
	"time"
)

// sweepLeases recovers work stranded by a dead runner. An issue claimed but never harvested stays
// in_progress with a lease; once that lease expires the runner is presumed dead, so the
// orchestrator releases the issue back to ready and a later schedule pass redispatches it.
//
// The stranded set comes from the in-flight projection, not a beads query (T3.13): the projection
// already holds every in_progress issue and the lease it was claimed under, and it is the single
// writer's read-your-writes-consistent record, so scanning it in memory both avoids the full-table
// read that fed the write-visibility lag and answers "stranded?" without trusting a lagging beads
// status. Crash-safety is unchanged: the projection is rebuilt from beads' durable lease_until on
// restart (rebuildInflight/reset), so a runner that died before the restart is still swept on its
// original deadline. The cached issue snapshot carries the role/epic the issue-state event needs,
// so no per-issue beads Get is required either. Releasing is idempotent (the transition drops the
// issue from the projection), and a late Result for a released issue is ignored because it is no
// longer in flight. See specs/components/orchestrator.md "Live state vs. durable state".
func (o *Orchestrator) sweepLeases(ctx context.Context) {
	for _, issue := range o.inflight.expired(time.Now().UTC()) {
		// Release (in_progress → open) is a reset transition, funneled through the choke point so
		// the recovery stamps state_entered_at, announces the open event, and drops the issue from
		// the projection.
		if err := o.transition(ctx, issue, statusOpen, func(ctx context.Context) error {
			return o.bd.Release(ctx, issue.ID)
		}); err != nil {
			o.log.ErrorContext(ctx, "orchestrator: release stranded issue", "issue", issue.ID, "err", err)
			continue
		}
		o.log.InfoContext(ctx, "orchestrator: released stranded issue back to ready", "issue", issue.ID)
	}
}
