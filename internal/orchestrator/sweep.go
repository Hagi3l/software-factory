package orchestrator

import (
	"context"
	"time"
)

// sweepLeases recovers work stranded by a dead runner. An issue claimed but never
// harvested stays in_progress with a lease; once that lease expires the runner is
// presumed dead, so the orchestrator releases the issue back to ready and a later
// schedule pass redispatches it. The lease — durable in beads — not orchestrator
// memory is what makes this survive an orchestrator restart (see
// specs/components/orchestrator.md). Releasing is idempotent, and a late Result for a
// released issue is ignored because it is no longer in_progress.
func (o *Orchestrator) sweepLeases(ctx context.Context) {
	stranded, err := o.bd.ListStranded(ctx, time.Now().UTC())
	if err != nil {
		o.log.Error("orchestrator: list stranded issues", "err", err)
		return
	}
	for _, id := range stranded {
		if err := o.bd.Release(ctx, id); err != nil {
			o.log.Error("orchestrator: release stranded issue", "issue", id, "err", err)
			continue
		}
		o.log.Info("orchestrator: released stranded issue back to ready", "issue", id)
	}
}
