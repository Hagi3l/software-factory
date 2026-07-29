package live

import (
	"sync"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
)

// TestMergeQueueLatestWinsKeepsPosition proves a candidate advancing through steps updates in
// place (latest-wins on state) without reordering the train — the shape the merge-queue view
// reads.
func TestMergeQueueLatestWinsKeepsPosition(t *testing.T) {
	q := NewMergeQueue(10)
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateQueued})
	q.Record(core.MergeStateEvent{ID: "factory-2", State: core.MergeStateQueued})
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateRebasing})
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateLanded, Commit: "deadbeef"})

	snap := q.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (one row per candidate)", len(snap))
	}
	// factory-1 keeps its original front position despite three updates.
	if snap[0].ID != "factory-1" || snap[1].ID != "factory-2" {
		t.Fatalf("order = %s,%s; want factory-1,factory-2 (position preserved)", snap[0].ID, snap[1].ID)
	}
	if snap[0].State != core.MergeStateLanded || snap[0].Commit != "deadbeef" {
		t.Fatalf("factory-1 latest = %+v, want landed/deadbeef", snap[0])
	}
}

// TestMergeQueueEvictsOldest proves the bounded ring drops the earliest-queued candidate when a
// new one pushes the buffer over capacity — the earliest (typically already-landed) rows age out.
func TestMergeQueueEvictsOldest(t *testing.T) {
	q := NewMergeQueue(2)
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateLanded})
	q.Record(core.MergeStateEvent{ID: "factory-2", State: core.MergeStateRebasing})
	q.Record(core.MergeStateEvent{ID: "factory-3", State: core.MergeStateQueued})

	snap := q.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (max)", len(snap))
	}
	if snap[0].ID != "factory-2" || snap[1].ID != "factory-3" {
		t.Fatalf("order = %s,%s; want factory-2,factory-3 (factory-1 evicted)", snap[0].ID, snap[1].ID)
	}
	// An update to an evicted candidate re-adds it as a fresh row (it is no longer tracked).
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateLanded})
	if got := q.Snapshot(); got[len(got)-1].ID != "factory-1" {
		t.Fatalf("re-added candidate should append at the back, got %+v", got)
	}
}

// TestMergeQueueDropsIDless ignores an event with no id (it cannot be a queue row).
func TestMergeQueueDropsIDless(t *testing.T) {
	q := NewMergeQueue(10)
	q.Record(core.MergeStateEvent{State: core.MergeStateQueued})
	if snap := q.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot len = %d, want 0 (id-less dropped)", len(snap))
	}
}

// TestMergeQueueSnapshotIsCopy proves Snapshot returns a copy — mutating it cannot corrupt the
// buffer.
func TestMergeQueueSnapshotIsCopy(t *testing.T) {
	q := NewMergeQueue(10)
	q.Record(core.MergeStateEvent{ID: "factory-1", State: core.MergeStateQueued})
	snap := q.Snapshot()
	snap[0].State = "tampered"
	if again := q.Snapshot(); again[0].State != core.MergeStateQueued {
		t.Fatalf("buffer mutated through snapshot: %+v", again[0])
	}
}

// TestMergeQueueConcurrentRecord exercises the mutex under the race detector.
func TestMergeQueueConcurrentRecord(t *testing.T) {
	q := NewMergeQueue(50)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		id := "factory-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			q.Record(core.MergeStateEvent{ID: id, State: core.MergeStateQueued})
			q.Record(core.MergeStateEvent{ID: id, State: core.MergeStateLanded})
		}()
	}
	wg.Wait()
	if snap := q.Snapshot(); len(snap) != 20 {
		t.Fatalf("snapshot len = %d, want 20", len(snap))
	}
}
