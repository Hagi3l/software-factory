package orchestrator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
)

// statusOpen is the beads status of a ready (dispatchable) issue. It is the target of the two
// reset transitions — Release (stranded-lease recovery) and Reissue (spec drift) — which
// return in-flight work to the ready pool.
const statusOpen = "open"

// transition is the single choke point every orchestrator status write funnels through, so the
// two side effects of a state change live in one place (specs/components/orchestrator.md §9):
//
//   - the durable stamp: write does the beads status change, which atomically stamps
//     state_entered_at inside the same bd update (setStatus/Claim — see MetadataKeyStateEntered),
//     the anchor the board ticks its time-in-state counter from;
//   - the live nudge: on a successful write it publishes a fire-and-forget issue-state event so
//     the board/DAG/DLQ views refresh crisply on the actual transition (T4.17 pump → T4.18 board).
//
// `to` is the status the issue is entering; `issue` carries the id/role/epic for the event. The
// event is an additive observability emit, never a second source of truth — beads stays
// authoritative — so a publish failure is logged, not propagated, and only the write's error is
// returned (callers keep their existing Nak-on-error semantics).
//
// Idempotency under at-least-once redelivery is provided UPSTREAM, not by a guard here:
// handleResult/handleApproval act on a Result/decision only while the issue is in its expected
// transient status (in_progress / blocked-with-candidate), so a redelivery that lands on an
// already-settled issue returns before any write — no re-stamp, no re-announce. By the time a
// write reaches transition it is therefore a genuine state change, which is why transition
// announces unconditionally after a successful write (a stale-status guard here would instead
// wrongly suppress the legitimate re-announce of a claim that is immediately released on a
// publish failure).
func (o *Orchestrator) transition(ctx context.Context, issue core.Issue, to string, write func(context.Context) error) error {
	if err := write(ctx); err != nil {
		return err
	}
	o.announceState(issue, to)
	return nil
}

// announceState publishes the best-effort core-NATS issue-state event for an issue that just
// entered status. It is fire-and-forget (core NATS, no stream — losing one is harmless because
// the views keep a periodic backstop): a marshal or publish failure is logged and swallowed so
// a non-critical observability emit never wedges the single-writer loop. The durable record of
// the transition is the beads status + state_entered_at the write already persisted.
func (o *Orchestrator) announceState(issue core.Issue, status string) {
	ev := core.IssueStateEvent{
		ID:     issue.ID,
		Status: status,
		Role:   issue.Role,
		Epic:   core.EpicOf(issue),
		TS:     time.Now().UTC(),
	}
	data, err := json.Marshal(ev)
	if err != nil {
		o.log.Warn("orchestrator: marshal issue-state event", "issue", issue.ID, "err", err)
		return
	}
	if err := o.nc.Publish(messaging.IssueStateSubject(issue.ID), data); err != nil {
		o.log.Warn("orchestrator: publish issue-state event", "issue", issue.ID, "status", status, "err", err)
	}
}
