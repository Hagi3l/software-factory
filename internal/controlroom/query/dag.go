package query

import (
	"context"
	"fmt"
	"sort"

	"github.com/Loxstomper/harness/internal/controlroom/dag"
)

// DAG builds the issue-dependency-graph projection for the control room's DAG view
// (specs/control-room.md, T4.6). It reads every issue (ListAll — all statuses, like the
// board, so completed work still appears in the graph) and turns the blocked-by edges beads
// emits inline (core.Issue.DependsOn) into dag.Edges oriented blocker→dependent. An edge
// whose endpoint is not in the issue set is dropped here as well as in dag.Layout — the
// orchestrator's prefix-blind dependency model can leave an edge pointing at an id outside
// this set, and the graph only draws what it can place. Nodes and edges are sorted for a
// stable render. The graph types live in package dag; this returns dag.Graph directly with
// no intermediate type.
func (r *Reader) DAG(ctx context.Context) (dag.Graph, error) {
	issues, err := r.issues.ListAll(ctx)
	if err != nil {
		return dag.Graph{}, fmt.Errorf("query: dag: %w", err)
	}

	nodes := make([]dag.Node, 0, len(issues))
	inSet := make(map[string]bool, len(issues))
	for _, i := range issues {
		nodes = append(nodes, dag.Node{ID: i.ID, Title: i.Title, Status: i.Status})
		inSet[i.ID] = true
	}

	var edges []dag.Edge
	for _, i := range issues {
		for _, dep := range i.DependsOn {
			// dep blocks i: edge runs blocker (dep) -> dependent (i). Drop dangling ends.
			if dep == "" || !inSet[dep] || !inSet[i.ID] {
				continue
			}
			edges = append(edges, dag.Edge{From: dep, To: i.ID})
		}
	}

	sort.Slice(nodes, func(a, b int) bool { return nodes[a].ID < nodes[b].ID })
	sort.Slice(edges, func(a, b int) bool {
		if edges[a].From != edges[b].From {
			return edges[a].From < edges[b].From
		}
		return edges[a].To < edges[b].To
	})

	return dag.Graph{Nodes: nodes, Edges: edges}, nil
}
