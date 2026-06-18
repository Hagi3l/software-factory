package orchestrator

import (
	"sort"
	"sync"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// inflightProjection is the orchestrator's volatile, in-memory **work-graph projection**: the
// single writer's authoritative live view of the current state of EVERY issue it knows — not just
// the ones in flight. It is a read-your-writes-consistent cache over beads that the single writer
// maintains at its one transition choke point (see specs/components/orchestrator.md "Live state vs.
// durable state — the work-graph projection").
//
// Why it exists: beads is the durable source of truth but NOT a strongly read-your-writes
// consistent read surface under the reconcile loop's concurrent traffic. With beads in server
// mode a fresh bd.ready() issued moments after a status write may not yet observe that write, and a
// heavy poll over the whole graph can saturate or time out. Three symptoms share this one root
// cause: (1) re-dispatching a just-claimed issue every tick until its in_progress write is visible
// (the dispatch storm — observed in a demo/vault run: one plan issue claimed ~23 times in 45s, its
// first valid Result discarded as stale, its decomposition applied twice); (2) re-dispatching a
// just-SETTLED issue (e.g. a plan closed at decomposition) before the close is visible — a wasted
// second invocation; (3) the control room reading a card as `open` while its agent runs, or its
// `bd` reads being killed under load. The orchestrator is the single writer, so it already knows
// the live status of every issue at the instant it writes it; this projection records exactly that
// for the whole graph, and its readers consult it instead of a lagging beads read.
//
// Generalizes the original in-flight (in_progress-only) cache: retaining SETTLED issues too is what
// lets the scheduler skip just-closed/just-blocked candidates (case 2) and lets the control room
// read closed/blocked state from a consistent surface (case 3). The in-flight-specific accessors
// (has, issues, expired, size) preserve their old meaning by filtering to in_progress; the new
// readers (statusOf, snapshot) expose the whole graph.
//
// It is DERIVED, never authoritative: it holds nothing that is not recoverable from beads, is
// rebuilt from the full graph at startup (rebuildInflight) before the first dispatch, and is
// mutated ONLY at transition(), so it cannot silently drift from beads — every status write is
// already funneled through that one choke point. The mutex guards concurrent access from the tick
// goroutine (scheduleReady and the sweeps) and the serial Result consumer (handleResult), which
// both drive transitions.
type inflightProjection struct {
	mu sync.Mutex
	m  map[string]projectedEntry
}

// projectedEntry is one issue's cached live state: the issue snapshot as the single writer last saw
// it, its current beads status, and (while in_progress) the lease it was claimed under. The lease
// is load-bearing only for in_progress entries: the in-memory lease sweep (T3.13, sweepLeases)
// reads it instead of a beads query, and on restart reset() seeds it from the durable lease_until so
// recovery uses the original deadline. A settled (open/closed/blocked) entry carries the zero lease.
type projectedEntry struct {
	issue  core.Issue
	status string
	lease  time.Time
}

func newInflightProjection() *inflightProjection {
	return &inflightProjection{m: map[string]projectedEntry{}}
}

// add records an issue as in-flight (claimed, in_progress) under the given lease. Called from
// transition on a transition TO in_progress. It overwrites any prior projected state for the id,
// so a re-claim (after a release/reissue) cleanly reflects the new lease and in_progress status.
func (p *inflightProjection) add(issue core.Issue, lease time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[issue.ID] = projectedEntry{issue: issue, status: statusInProgress, lease: lease}
}

// settle records an issue's transition AWAY from in_progress (to open/closed/blocked). Unlike the
// old in-flight cache — which DELETED the issue here — the work-graph projection RETAINS it with its
// new settled status (and no lease), so the scheduler can skip a just-settled candidate a lagging
// bd.ready() still lists, and the control room can read closed/blocked state from a consistent
// surface. Called from transition on every non-in_progress target.
func (p *inflightProjection) settle(issue core.Issue, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[issue.ID] = projectedEntry{issue: issue, status: status}
}

// has reports whether the orchestrator considers the issue in-flight (projected status
// in_progress). It is the authority for "already dispatched" (scheduleReady) and "is this a live
// result" (handleResult), replacing a lagging beads status read. A settled issue retained in the
// projection reports false here (it is no longer in flight) — see statusOf for its actual status.
func (p *inflightProjection) has(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[id]
	return ok && e.status == statusInProgress
}

// statusOf returns the projected status of any known issue and whether the issue is known at all.
// It is the whole-graph read the scheduler uses to skip a candidate already settled (closed/blocked)
// in the single writer's own view even when a lagging bd.ready() still lists it — generalizing the
// has() (in_progress-only) skip to the just-settled case. A not-known issue (never seen, or evicted)
// returns ("", false).
func (p *inflightProjection) statusOf(id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[id]
	if !ok {
		return "", false
	}
	return e.status, true
}

// updateIssue replaces the cached issue snapshot for a known id, preserving its status and lease. It
// is called by scheduleReady after it pins the spec hash — which happens *after* the claim
// transition that first recorded the (un-pinned) issue — so the projection's snapshot carries the
// SpecHash the in-memory spec-drift sweep (recompileSpecDelta) diffs against (T3.13). A no-op for an
// id not known, so a result that settled the issue between claim and pin cannot resurrect it.
func (p *inflightProjection) updateIssue(issue core.Issue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.m[issue.ID]; ok {
		e.issue = issue
		p.m[issue.ID] = e
	}
}

// issues returns a snapshot of every in_progress issue as the single writer last recorded it (at
// claim/pin time), with the projected status stamped on. It is the in-memory replacement for the
// beads InProgress() query the spec-drift sweep and the epic-completion drain test iterated (T3.13):
// the projection already holds the in_progress set and each issue's pinned spec hash, so they need
// no beads read. Settled issues retained in the projection are excluded — this is the in-flight set.
func (p *inflightProjection) issues() []core.Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.Issue, 0, len(p.m))
	for _, e := range p.m {
		if e.status != statusInProgress {
			continue
		}
		is := e.issue
		is.Status = e.status
		out = append(out, is)
	}
	return out
}

// expired returns the in_progress issues whose lease has elapsed as of now (or that carry no lease
// at all — an anomalous claim, swept so it cannot wedge in_progress forever). It is the in-memory
// replacement for the beads stranded query the lease sweep ran (T3.13): the projection already holds
// every in_progress issue and its lease, so the sweep needs no beads read. The condition mirrors the
// old beads logic exactly — stranded iff the lease is not strictly after now (the zero time, an
// absent lease, is before any now and so is reported). Settled entries carry no lease and are never
// returned: only in_progress work is sweepable.
func (p *inflightProjection) expired(now time.Time) []core.Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []core.Issue
	for _, e := range p.m {
		if e.status != statusInProgress {
			continue
		}
		if !e.lease.After(now) {
			out = append(out, e.issue)
		}
	}
	return out
}

// snapshot returns every known issue with its live projected status and lease stamped on, ordered by
// id for a stable read. It is the whole-graph read the control room consumes as its live read model
// (snapshot-then-stream): the snapshot at connect is this set, and subsequent issue-state events
// applied as deltas keep it gap-free (specs/observability.md "The live read model"). Because it is
// the single writer's own view it never disagrees with reality and places no `bd list` load on the
// store.
func (p *inflightProjection) snapshot() []core.Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.Issue, 0, len(p.m))
	for _, e := range p.m {
		is := e.issue
		is.Status = e.status
		is.Lease = e.lease
		out = append(out, is)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// reset replaces the projection's contents with the given issues — the WHOLE work graph at startup,
// not just the in_progress set — each under its own durable status and lease (issue.Status, and
// issue.Lease decoded from beads' lease_until for in_progress work). Used by rebuildInflight to
// hydrate the projection from beads at startup, so a restarted orchestrator resumes with an accurate
// live view of every issue AND the in-memory lease sweep recovers stranded work on the original
// deadline rather than a fresh leaseTTL — keeping crash-safety unchanged (an issue whose lease
// already passed is swept on the first tick; see specs/components/orchestrator.md "Crash safety").
func (p *inflightProjection) reset(issues []core.Issue) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = make(map[string]projectedEntry, len(issues))
	for _, is := range issues {
		p.m[is.ID] = projectedEntry{issue: is, status: is.Status, lease: is.Lease}
	}
}

// size reports the number of in-flight (in_progress) issues — the load-bearing count for the
// dispatch/sweep paths and tests. The projection also retains settled issues; snapshot reports the
// whole graph.
func (p *inflightProjection) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.m {
		if e.status == statusInProgress {
			n++
		}
	}
	return n
}
