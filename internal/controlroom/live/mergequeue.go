package live

import (
	"sync"

	"github.com/Loxstomper/software-factory/internal/core"
)

// MergeQueue is a bounded, concurrent-safe record of the serialized merge train's current
// shape: the latest merge-state step each integrate candidate has reached (specs/integration.md,
// specs/control-room.md "The merge-queue view"). It is the read model behind the merge-queue
// view (plan T4.25): between "a branch's gate passed" and "a commit landed on main" lies the
// rebase-and-re-gate interval where independently-green branches break, and only the typed
// merge-state events make that interval observable. beads/git hold no per-step state, so this
// in-memory buffer — fed by the merge-state pump — is where the live step lives.
//
// Each issue keeps exactly one entry (latest-wins on state) so a candidate advancing
// queued → rebasing → re-gating → landed updates in place rather than stacking rows; insertion
// order is preserved so the list reads in train (arrival) order, with the earliest-queued
// candidates — typically the terminal/landed ones — at the front, where they age out first as
// the bounded ring evicts the oldest.
//
// It is intentionally in-memory and best-effort, like the activity feed: the authoritative
// merge state is the git refs + beads, never reconstructed from these events, so a dropped
// event or a restart losing the live shape is harmless (the view's periodic backstop re-reads
// this buffer, and a fresh run repopulates it as candidates flow through).
type MergeQueue struct {
	mu    sync.Mutex
	max   int
	order []string                        // issue ids, oldest .. newest (train order)
	byID  map[string]core.MergeStateEvent // latest event per issue
}

// NewMergeQueue returns a MergeQueue retaining up to max distinct candidates (min 1).
func NewMergeQueue(max int) *MergeQueue {
	if max <= 0 {
		max = 1
	}
	return &MergeQueue{max: max, byID: make(map[string]core.MergeStateEvent)}
}

// Record upserts one merge-state transition: latest-wins on the candidate's state, keeping the
// candidate in its existing train position (so an advancing step does not reorder the list). A
// first sighting appends at the back and, if that pushes the buffer over max, evicts the oldest
// candidate. An event with no id is dropped — it cannot be a queue row — matching the
// best-effort nature of the buffer and the pump's own guard.
func (q *MergeQueue) Record(ev core.MergeStateEvent) {
	if ev.ID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, seen := q.byID[ev.ID]; !seen {
		q.order = append(q.order, ev.ID)
		if len(q.order) > q.max {
			evict := q.order[0]
			q.order = append([]string(nil), q.order[1:]...)
			delete(q.byID, evict)
		}
	}
	q.byID[ev.ID] = ev
}

// Snapshot returns a copy of the retained candidates in train (insertion) order — the
// merge-queue view's backing read. Each is the latest step seen for that candidate.
func (q *MergeQueue) Snapshot() []core.MergeStateEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]core.MergeStateEvent, 0, len(q.order))
	for _, id := range q.order {
		out = append(out, q.byID[id])
	}
	return out
}
