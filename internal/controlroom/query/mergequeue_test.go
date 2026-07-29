package query

import (
	"context"
	"errors"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
)

// TestMergeQueueEnrichesAndFlags proves MergeQueue joins each merge-state event to its beads
// issue (title/role/spec), preserves the event (train) order, and derives the terminal/failed
// flags from the step — the read shape the merge-queue view (T4.25) renders.
func TestMergeQueueEnrichesAndFlags(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "factory-1", Title: "land me", Role: "integrate", Spec: "specs/a.md"},
		{ID: "factory-2", Title: "conflict", Role: "integrate", Spec: "specs/b.md"},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	events := []core.MergeStateEvent{
		{ID: "factory-2", State: core.MergeStateConflicted, Role: "integrate"},
		{ID: "factory-1", State: core.MergeStateLanded, Role: "integrate", Commit: "abc123"},
		{ID: "factory-3", State: core.MergeStateRebasing, Role: "integrate"}, // not in beads
	}
	rows, err := r.MergeQueue(context.Background(), events)
	if err != nil {
		t.Fatalf("MergeQueue: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	// Order preserved (the buffer holds the train's arrival order).
	if rows[0].ID != "factory-2" || rows[1].ID != "factory-1" || rows[2].ID != "factory-3" {
		t.Fatalf("order = %s,%s,%s; want factory-2,factory-1,factory-3", rows[0].ID, rows[1].ID, rows[2].ID)
	}

	// factory-2: conflicted → terminal + failed, title/spec enriched from beads.
	if !rows[0].Terminal || !rows[0].Failed {
		t.Fatalf("conflicted row flags: terminal=%v failed=%v, want both true", rows[0].Terminal, rows[0].Failed)
	}
	if rows[0].Title != "conflict" || rows[0].Spec != "specs/b.md" {
		t.Fatalf("conflicted row not enriched: %+v", rows[0])
	}

	// factory-1: landed → terminal, not failed, carries the commit.
	if !rows[1].Terminal || rows[1].Failed {
		t.Fatalf("landed row flags: terminal=%v failed=%v, want true/false", rows[1].Terminal, rows[1].Failed)
	}
	if rows[1].Commit != "abc123" || rows[1].Title != "land me" {
		t.Fatalf("landed row wrong: %+v", rows[1])
	}

	// factory-3: in-flight rebasing → not terminal; no beads issue, so title empty but the
	// row still renders (the merge step is the point, the title is enrichment).
	if rows[2].Terminal || rows[2].Failed {
		t.Fatalf("rebasing row should be non-terminal: %+v", rows[2])
	}
	if rows[2].Title != "" || rows[2].State != core.MergeStateRebasing || rows[2].Role != "integrate" {
		t.Fatalf("missing-issue row wrong: %+v", rows[2])
	}
}

// TestMergeQueueEmpty returns nil for an empty train without touching beads.
func TestMergeQueueEmpty(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("should not be called")}, &fakeArts{}, &fakeProv{})
	rows, err := r.MergeQueue(context.Background(), nil)
	if err != nil {
		t.Fatalf("MergeQueue(empty): %v", err)
	}
	if rows != nil {
		t.Fatalf("rows = %v, want nil for an empty train", rows)
	}
}

// TestMergeQueueListAllError surfaces a beads read failure (it cannot enrich without the issues).
func TestMergeQueueListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	_, err := r.MergeQueue(context.Background(), []core.MergeStateEvent{{ID: "factory-1", State: core.MergeStateQueued}})
	if err == nil {
		t.Fatal("MergeQueue: want error when ListAll fails")
	}
}
