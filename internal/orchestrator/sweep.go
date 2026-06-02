package orchestrator

import (
	"context"
	"time"

	"github.com/Loxstomper/harness/internal/core"
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
		// Read the issue (best-effort) so the issue-state event carries its role/epic; on a read
		// failure fall back to a minimal issue (id only) — the release must still proceed, and the
		// event then nudges the board with just the id (EpicOf falls back to the id, role empty).
		issue, gerr := o.bd.Get(ctx, id)
		if gerr != nil {
			issue = core.Issue{ID: id}
		}
		// Release (in_progress → open) is a reset transition, funneled through the choke point so
		// the recovery stamps state_entered_at and announces the open event.
		if err := o.transition(ctx, issue, statusOpen, func(ctx context.Context) error {
			return o.bd.Release(ctx, id)
		}); err != nil {
			o.log.Error("orchestrator: release stranded issue", "issue", id, "err", err)
			continue
		}
		o.log.Info("orchestrator: released stranded issue back to ready", "issue", id)
	}
}
