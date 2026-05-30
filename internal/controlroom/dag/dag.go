// Package dag is the control room's issue-dependency-graph model and its server-side
// renderer (specs/control-room.md, "DAG"). It owns the graph types (Node/Edge/Graph), a
// deterministic pure-Go layered layout, and an SVG emitter — no graphviz/d2 binary and no
// client-side graph library, so the rendered graph keeps the harness's self-contained-binary
// property (a deployed control room is one Go binary). It imports only internal/core and the
// standard library; the query layer returns a dag.Graph directly, so these types live here
// and nowhere else (no parallel type, no adapter).
//
// An edge runs blocker→dependent: Edge{From: blocker, To: dependent}. That direction is the
// one the layout flows top→bottom, so a blocker sits above the work it blocks — the natural
// reading of "what must merge first".
package dag

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

// Node is one issue in the dependency graph: its id, its title, and its beads lifecycle
// status (a plain string, mirroring core.Issue.Status), the last of which tints the rendered
// node so the graph reads at a glance like the board.
type Node struct {
	ID     string
	Title  string
	Status string
}

// Edge is a dependency: From (the blocker) must complete before To (the dependent). It is
// the blocked-by relation beads emits, oriented blocker→dependent for a top→bottom layout.
type Edge struct {
	From string // the blocker
	To   string // the dependent (blocked by From)
}

// Graph is the whole dependency graph: the issue nodes and the edges between them.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// Placed is a node with its computed position and box size in the laid-out diagram.
type Placed struct {
	Node
	X, Y, W, H int
}

// PlacedEdge is an edge with its endpoint pixel coordinates, ready to draw as a line.
type PlacedEdge struct {
	From, To       string
	X1, Y1, X2, Y2 int
}

// Diagram is the result of laying out a Graph: the canvas size and every node/edge placed
// in pixel space. It is what RenderSVG draws from.
type Diagram struct {
	Width, Height int
	Nodes         []Placed
	Edges         []PlacedEdge
}

// Layout sizing constants. Boxes are a fixed size laid out on a grid; the values are tuned
// so two lines of text (id + truncated title) fit and the graph stays legible.
const (
	nodeW   = 180
	nodeH   = 48
	gapX    = 40
	gapY    = 60
	marginX = 20
	marginY = 20
)

// Layout assigns every node a layer (its longest path from a root) and lays the graph out
// top→bottom: y grows with the layer, x with the node's index within its layer. It is
// deterministic — nodes within a layer are ordered by id — and cycle-defensive: layer
// assignment processes nodes in dependency order and a node already on the current path is
// not revisited, so a cyclic graph yields a finite layout rather than an infinite loop or a
// panic. Edges whose endpoints are not both in the node set are dropped (a dangling edge has
// no box to connect).
func Layout(g Graph) Diagram {
	// Index nodes by id and keep a deterministic id order.
	index := make(map[string]Node, len(g.Nodes))
	ids := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if _, dup := index[n.ID]; dup {
			continue
		}
		index[n.ID] = n
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)

	// Keep only edges whose endpoints both exist; build the blocker adjacency (From→To).
	type edge struct{ from, to string }
	var edges []edge
	children := make(map[string][]string) // blocker -> dependents
	indeg := make(map[string]int)         // dependents blocked count, for root detection
	for _, e := range g.Edges {
		if _, ok := index[e.From]; !ok {
			continue
		}
		if _, ok := index[e.To]; !ok {
			continue
		}
		edges = append(edges, edge{from: e.From, to: e.To})
		children[e.From] = append(children[e.From], e.To)
		indeg[e.To]++
	}

	// layer[id] = longest path (in edges) from any root to id. Computed with a memoized
	// longest-path walk over blocker→dependent edges; a visited-on-path guard makes it
	// cycle-defensive (a back-edge into the current path contributes no depth).
	layer := make(map[string]int, len(ids))
	onPath := make(map[string]bool, len(ids))

	var depth func(id string) int
	depth = func(id string) int {
		if v, done := layer[id]; done {
			return v
		}
		if onPath[id] {
			// Cycle: do not recurse back through the current path.
			return 0
		}
		onPath[id] = true
		best := 0
		// Walk to this node's blockers (incoming edges) for longest-path-from-root.
		for _, e := range edges {
			if e.to == id {
				if d := depth(e.from) + 1; d > best {
					best = d
				}
			}
		}
		onPath[id] = false
		layer[id] = best
		return best
	}
	for _, id := range ids {
		depth(id)
	}

	// Group ids by layer, ordered by id within each layer for a stable render.
	byLayer := make(map[int][]string)
	maxLayer := 0
	for _, id := range ids {
		l := layer[id]
		byLayer[l] = append(byLayer[l], id)
		if l > maxLayer {
			maxLayer = l
		}
	}
	for l := range byLayer {
		sort.Strings(byLayer[l])
	}

	// Place each node: x by index within its layer, y by layer.
	pos := make(map[string][2]int, len(ids)) // id -> [centerX, centerY]
	var placed []Placed
	maxCols := 0
	for l := 0; l <= maxLayer; l++ {
		row := byLayer[l]
		if len(row) > maxCols {
			maxCols = len(row)
		}
		for i, id := range row {
			x := marginX + i*(nodeW+gapX)
			y := marginY + l*(nodeH+gapY)
			pos[id] = [2]int{x + nodeW/2, y + nodeH/2}
			placed = append(placed, Placed{Node: index[id], X: x, Y: y, W: nodeW, H: nodeH})
		}
	}

	// Edges connect box centers; the marker-end arrowhead is drawn by RenderSVG.
	var pedges []PlacedEdge
	for _, e := range edges {
		a, b := pos[e.from], pos[e.to]
		pedges = append(pedges, PlacedEdge{From: e.from, To: e.to, X1: a[0], Y1: a[1], X2: b[0], Y2: b[1]})
	}

	width := marginX*2 + nodeW
	if maxCols > 0 {
		width = marginX*2 + maxCols*nodeW + (maxCols-1)*gapX
	}
	height := marginY*2 + nodeH
	if maxLayer >= 0 && len(placed) > 0 {
		height = marginY*2 + (maxLayer+1)*nodeH + maxLayer*gapY
	}

	return Diagram{Width: width, Height: height, Nodes: placed, Edges: pedges}
}

// titleMax is the rune length a node title is truncated to before rendering (an ellipsis is
// appended when truncated), so a long title cannot blow out the fixed-width box.
const titleMax = 24

// RenderSVG lays out the graph and emits a standalone <svg> element: a <defs> arrowhead
// <marker>, then every edge first as <line class="dag-edge" data-from/data-to marker-end>,
// then every node as <a href="/issue/{id}"><g class="dag-node" data-node="{id}">…</g></a>
// (the anchor is the click-through into the issue-detail view). All dynamic text — ids and
// titles, which are semi-untrusted — is XML-escaped; titles are rune-truncated. An empty
// graph yields a minimal valid <svg> with no nodes and no panic.
func RenderSVG(g Graph) string {
	d := Layout(g)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" class="dag-svg" role="img" aria-label="issue dependency graph">`,
		d.Width, d.Height, d.Width, d.Height)

	// Arrowhead marker, referenced by every edge's marker-end.
	b.WriteString(`<defs><marker id="dag-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#64748b"></path></marker></defs>`)

	// Edges first so nodes paint over them.
	for _, e := range d.Edges {
		fmt.Fprintf(&b,
			`<line class="dag-edge" data-from="%s" data-to="%s" x1="%d" y1="%d" x2="%d" y2="%d" stroke="#64748b" stroke-width="1.5" marker-end="url(#dag-arrow)"></line>`,
			attr(e.From), attr(e.To), e.X1, e.Y1, e.X2, e.Y2)
	}

	// Nodes as drill-through anchors.
	for _, n := range d.Nodes {
		id := html.EscapeString(n.ID)
		fmt.Fprintf(&b, `<a href="/issue/%s"><g class="dag-node" data-node="%s">`, attr(n.ID), attr(n.ID))
		fmt.Fprintf(&b,
			`<rect x="%d" y="%d" width="%d" height="%d" rx="6" fill="%s" stroke="#1e293b" stroke-width="1"></rect>`,
			n.X, n.Y, n.W, n.H, statusFill(n.Status))
		fmt.Fprintf(&b,
			`<text x="%d" y="%d" font-family="monospace" font-size="11" fill="#0f172a">%s</text>`,
			n.X+8, n.Y+18, id)
		fmt.Fprintf(&b,
			`<text x="%d" y="%d" font-family="sans-serif" font-size="12" fill="#0f172a">%s</text>`,
			n.X+8, n.Y+36, html.EscapeString(truncate(n.Title, titleMax)))
		b.WriteString(`</g></a>`)
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// attr XML-escapes a value destined for a double-quoted attribute. ids/titles are
// semi-untrusted, so every dynamic value is escaped before it reaches the markup.
func attr(s string) string { return html.EscapeString(s) }

// statusFill maps a beads status to a node fill, consistent with the board's status palette
// (blocked stands out, completed work recedes). Inline hex keeps the SVG free of any
// Tailwind/toolchain dependency. An unknown status falls back to slate.
func statusFill(status string) string {
	switch status {
	case "blocked":
		return "#fda4af" // rose-300
	case "in_progress":
		return "#fcd34d" // amber-300
	case "in_review":
		return "#7dd3fc" // sky-300
	case "closed":
		return "#cbd5e1" // slate-300
	case "merged":
		return "#6ee7b7" // emerald-300
	case "open":
		return "#7dd3fc" // sky-300
	default:
		return "#cbd5e1" // slate-300
	}
}

// truncate shortens s to at most max runes, appending an ellipsis when it cuts. It counts
// runes (not bytes) so a multibyte title is not split mid-character.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
