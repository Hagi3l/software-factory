package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// WorkGraphSnapshot is the orchestrator's in-memory work-graph projection, read as the control
// room's live read model (specs/observability.md "The live read model"). *orchestrator.Orchestrator
// satisfies it via its Snapshot method. It is declared here as a structural interface so the query
// layer takes no dependency on the orchestrator package (which would invert the layering) and stays
// testable with a fake.
type WorkGraphSnapshot interface {
	// Snapshot returns every issue the single writer knows, with its live status stamped on. It is
	// an in-memory read (no beads/Dolt traffic), consistent the instant a status is written.
	Snapshot(ctx context.Context) ([]core.Issue, error)
}

// projectionReader is a projection-backed IssueReader. It serves the control room's LIVE work-state
// reads — board, DAG, dead-letter, status bar, epic roll-up — from the orchestrator's in-memory
// work-graph projection instead of polling beads, so those views never lag the single writer (no
// card shows `open` while its agent runs) and place no `bd list` load on the store — the
// `signal: killed` read overload the demo run hit under polling (specs/observability.md "The live
// read model", demo-run-issues.md #4/#8). beads stays the durable truth the forensic pages still
// render from; this only replaces the *live* issue read, and only when co-located. Under a
// standalone `harness serve` (no attached orchestrator) the control room keeps the beads-backed
// reader instead — the same way the live SSE feed degrades there.
//
// A single Snapshot backs every read; List filters it by status; Get scans it and falls back to the
// durable beads reader for an id the projection does not hold (a forensic deep-link to history older
// than the projection — though after cold-start hydration the projection holds the whole graph, so
// this is a safety net, not the common path).
type projectionReader struct {
	graph    WorkGraphSnapshot
	fallback IssueReader // beads, consulted only on a Get miss; nil tolerated
}

// NewProjectionIssueReader builds a projection-backed IssueReader over the orchestrator's work-graph
// snapshot, with a beads reader as the Get fallback for ids the projection does not hold. It is
// wired only in the co-located run (harness run --serve-addr); standalone serve keeps the
// beads-backed reader (see query.NewReader).
func NewProjectionIssueReader(graph WorkGraphSnapshot, fallback IssueReader) IssueReader {
	return &projectionReader{graph: graph, fallback: fallback}
}

// ListAll returns the whole work-graph snapshot — the board's and DAG's read.
func (p *projectionReader) ListAll(ctx context.Context) ([]core.Issue, error) {
	return p.graph.Snapshot(ctx)
}

// List returns the snapshot issues in the given beads status. status may be a comma-separated set
// (mirroring beads.Client.List), so a caller asking for "blocked" gets exactly the dead-letter
// queue. Filtering an in-memory slice is cheap and keeps the membership consistent with ListAll.
func (p *projectionReader) List(ctx context.Context, status string) ([]core.Issue, error) {
	all, err := p.graph.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, s := range strings.Split(status, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wanted[s] = true
		}
	}
	out := make([]core.Issue, 0, len(all))
	for _, is := range all {
		if wanted[is.Status] {
			out = append(out, is)
		}
	}
	return out, nil
}

// Get returns one issue from the snapshot, falling back to the durable beads reader for an id the
// projection does not hold. The live views never call Get (they read ListAll/List); it exists to
// satisfy IssueReader and to keep a forensic deep-link resolvable even for pre-hydration history.
func (p *projectionReader) Get(ctx context.Context, id string) (core.Issue, error) {
	all, err := p.graph.Snapshot(ctx)
	if err == nil {
		for _, is := range all {
			if is.ID == id {
				return is, nil
			}
		}
	}
	if p.fallback != nil {
		return p.fallback.Get(ctx, id)
	}
	if err != nil {
		return core.Issue{}, err
	}
	return core.Issue{}, fmt.Errorf("query: issue %s not in work-graph projection", id)
}
