package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/messaging"
)

// TestTransitionAnnouncesEachKind proves the single transition choke point publishes a typed
// issue-state event on factory.issue.<id>.state for every status it is driven to (the kinds the
// orchestrator transitions issues through: in_progress / closed / blocked / open), carrying the
// id/status/role/epic and a non-zero timestamp — the payload the T4.17 pump / T4.18 board read.
// The durable state_entered_at stamp that accompanies it is exercised at the beads layer
// (TestStateEnteredRoundTrip); here the contract under test is the fire-and-forget emit.
func TestTransitionAnnouncesEachKind(t *testing.T) {
	bd := newFakeBeads()
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	sub, err := nc.SubscribeSync(messaging.IssueStateWildcard)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for _, to := range []string{statusInProgress, statusClosed, statusBlocked, statusOpen} {
		issue := core.Issue{ID: "iss-1", Role: "implement", Status: "prev", EpicID: "epic-9"}
		called := false
		if err := o.transition(context.Background(), issue, to, func(context.Context) error {
			called = true
			return nil
		}); err != nil {
			t.Fatalf("transition(%s): %v", to, err)
		}
		if !called {
			t.Fatalf("transition(%s) did not run the write", to)
		}
		msg, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("transition(%s): no event published: %v", to, err)
		}
		if got := messaging.IssueIDFromStateSubject(msg.Subject); got != "iss-1" {
			t.Errorf("transition(%s): subject id = %q, want iss-1", to, got)
		}
		var ev core.IssueStateEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			t.Fatalf("transition(%s): decode event: %v", to, err)
		}
		if ev.ID != "iss-1" || ev.Status != to || ev.Role != "implement" || ev.Epic != "epic-9" {
			t.Errorf("transition(%s): event = %+v, want id=iss-1 status=%s role=implement epic=epic-9", to, ev, to)
		}
		if ev.TS.IsZero() {
			t.Errorf("transition(%s): event TS is zero", to)
		}
	}
}

// TestTransitionEpicFallback proves the event's Epic uses core.EpicOf — a root seed with no
// EpicID announces its own id as the epic, so a consumer can group by it uniformly.
func TestTransitionEpicFallback(t *testing.T) {
	bd := newFakeBeads()
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.IssueStateWildcard)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()

	root := core.Issue{ID: "root-1", Role: "implement", Status: statusInProgress} // no EpicID
	if err := o.transition(context.Background(), root, statusClosed, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("transition: %v", err)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no event: %v", err)
	}
	var ev core.IssueStateEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Epic != "root-1" {
		t.Errorf("root epic = %q, want its own id root-1 (EpicOf fallback)", ev.Epic)
	}
}

// TestTransitionWriteFailureNoAnnounce proves a failed status write is propagated verbatim and
// announces nothing — the live nudge follows a real, durable transition, never a phantom one.
func TestTransitionWriteFailureNoAnnounce(t *testing.T) {
	bd := newFakeBeads()
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.IssueStateWildcard)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()

	boom := errors.New("beads write failed")
	issue := core.Issue{ID: "iss-1", Role: "implement", Status: statusInProgress}
	if err := o.transition(context.Background(), issue, statusClosed, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("transition err = %v, want %v", err, boom)
	}
	if msg, err := sub.NextMsg(200 * time.Millisecond); err == nil {
		t.Errorf("announced despite a failed write: %q", string(msg.Data))
	}
}

// TestSettledIssueNoReannounce proves the redelivery idempotency the choke point is part of: a
// Result redelivered for an issue no longer in_progress (already accepted/closed) is ignored by
// handleResult before any status write — so it neither re-closes (no re-stamp) nor re-announces.
func TestSettledIssueNoReannounce(t *testing.T) {
	bd := newFakeBeads()
	// The issue is already closed — a stale/duplicate Result must be a no-op.
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: statusClosed})
	// The gate is never reached (handleResult returns at the status check), so a zero gate is fine.
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.IssueStateWildcard)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()

	transient, herr := o.handleResult(context.Background(), core.Result{
		IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"},
	})
	if herr != nil || transient {
		t.Fatalf("handleResult on settled issue = (%v, %v), want (false, nil)", transient, herr)
	}
	if msg, err := sub.NextMsg(200 * time.Millisecond); err == nil {
		t.Errorf("re-announced a settled issue: %q", string(msg.Data))
	}
	// And it never wrote status again (no re-close).
	if _, _, closed, _, _ := bd.snap(); len(closed) != 0 {
		t.Errorf("settled issue was re-closed: %v", closed)
	}
}
