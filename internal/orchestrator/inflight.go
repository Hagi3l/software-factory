package orchestrator

import (
	"sync"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// inflightProjection is the orchestrator's volatile, in-memory record of the issues it
// currently considers in_progress — a read-your-writes-consistent cache over beads that the
// single writer maintains at its one transition choke point (see
// specs/components/orchestrator.md "Live state vs. durable state").
//
// Why it exists: beads is the durable source of truth but NOT a strongly read-your-writes
// consistent read surface under the reconcile loop's concurrent traffic. With beads in server
// mode a fresh bd.ready() issued moments after a Claim may not yet observe the in_progress
// write, so treating bd.ready() as the "already dispatched" authority re-dispatches in-flight
// work on every tick until the write becomes visible — a storm that multiplies agent spend and
// corrupts the graph with duplicate proposals (observed in a demo/vault run: one plan issue
// claimed ~23 times in 45s, its first valid Result discarded as stale, its decomposition applied
// twice). Polling faster than write-visibility makes a storm, not progress. The orchestrator is
// the single writer, so it already knows the live status of every issue at the instant it writes
// it; this projection records exactly that, and the two hot paths (dispatch, result gating)
// consult it instead of a lagging beads read.
//
// It is DERIVED, never authoritative: it holds nothing that is not recoverable from beads, is
// rebuilt from the in_progress set at startup (rebuildInflight) before the first dispatch, and is
// mutated ONLY at transition(), so it cannot silently drift from beads — every status write is
// already funneled through that one choke point. The mutex guards concurrent access from the tick
// goroutine (scheduleReady and the sweeps) and the serial Result consumer (handleResult), which
// both drive transitions.
type inflightProjection struct {
	mu sync.Mutex
	m  map[string]inflightEntry
}

// inflightEntry is one in-flight issue's cached live state: the issue snapshot as the single
// writer last saw it, plus the lease it was claimed under. The lease is load-bearing: the
// in-memory lease sweep (T3.13, sweepLeases) reads it instead of a beads query, and on restart
// rebuildInflight seeds it from the durable lease_until so recovery uses the original deadline.
type inflightEntry struct {
	issue core.Issue
	lease time.Time
}

func newInflightProjection() *inflightProjection {
	return &inflightProjection{m: map[string]inflightEntry{}}
}

// add records an issue as in-flight (claimed, in_progress) under the given lease. Called from
// transition on a transition TO in_progress.
func (p *inflightProjection) add(issue core.Issue, lease time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[issue.ID] = inflightEntry{issue: issue, lease: lease}
}

// remove drops an issue from the projection. Called from transition on any transition AWAY from
// in_progress (open/closed/blocked); a no-op for an id not present, so it is idempotent.
func (p *inflightProjection) remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, id)
}

// has reports whether the orchestrator considers the issue in-flight. It is the authority for
// "already dispatched" (scheduleReady) and "is this a live result" (handleResult), replacing a
// lagging beads status read.
func (p *inflightProjection) has(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.m[id]
	return ok
}

// updateIssue replaces the cached issue snapshot for an in-flight id, preserving its lease. It is
// called by scheduleReady after it pins the spec hash — which happens *after* the claim transition
// that first recorded the (un-pinned) issue — so the projection's snapshot carries the SpecHash
// the in-memory spec-drift sweep (recompileSpecDelta) diffs against (T3.13). A no-op for an id no
// longer in flight, so a result that settled the issue between claim and pin cannot resurrect it.
func (p *inflightProjection) updateIssue(issue core.Issue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.m[issue.ID]; ok {
		e.issue = issue
		p.m[issue.ID] = e
	}
}

// issues returns a snapshot of every in-flight issue as the single writer last recorded it (at
// claim/pin time). It is the in-memory replacement for the beads InProgress() query the spec-drift
// sweep iterated (T3.13): the projection already holds the in_progress set and each issue's pinned
// spec hash, so the sweep needs no beads read.
func (p *inflightProjection) issues() []core.Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.Issue, 0, len(p.m))
	for _, e := range p.m {
		out = append(out, e.issue)
	}
	return out
}

// expired returns the in-flight issues whose lease has elapsed as of now (or that carry no lease
// at all — an anomalous claim, swept so it cannot wedge in_progress forever). It is the in-memory
// replacement for the beads stranded query the lease sweep ran (T3.13): the projection already
// holds every in_progress issue and its lease, so the sweep needs no beads read. The condition
// mirrors the old beads logic exactly — stranded iff the lease is not strictly after now (the zero
// time, an absent lease, is before any now and so is reported).
func (p *inflightProjection) expired(now time.Time) []core.Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []core.Issue
	for _, e := range p.m {
		if !e.lease.After(now) {
			out = append(out, e.issue)
		}
	}
	return out
}

// reset replaces the projection's contents with the given in-flight issues, each under its own
// durable lease (issue.Lease, decoded from beads' lease_until). Used by rebuildInflight to seed
// the projection from beads' in_progress set at startup, so a restarted orchestrator resumes with
// an accurate live view AND the in-memory lease sweep recovers stranded work on the original
// deadline rather than a fresh leaseTTL — keeping crash-safety unchanged (an issue whose lease
// already passed is swept on the first tick; see specs/components/orchestrator.md "Crash safety").
func (p *inflightProjection) reset(issues []core.Issue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = make(map[string]inflightEntry, len(issues))
	for _, is := range issues {
		p.m[is.ID] = inflightEntry{issue: is, lease: is.Lease}
	}
}

// size reports the number of in-flight issues (for logging and tests).
func (p *inflightProjection) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}
