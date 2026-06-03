package dag

import (
	"strings"
	"testing"
)

// nodeIDsByLayer groups laid-out node ids by their y coordinate (one y per layer), so a
// test can assert a node sits below its blocker without hard-coding pixel math.
func layerOf(d Diagram, id string) (int, bool) {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n.Y, true
		}
	}
	return 0, false
}

func TestLayoutChainStacksByLayer(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}
	d := Layout(g)
	ya, _ := layerOf(d, "a")
	yb, _ := layerOf(d, "b")
	yc, _ := layerOf(d, "c")
	if ya >= yb || yb >= yc {
		t.Errorf("chain not stacked top->bottom: a=%d b=%d c=%d", ya, yb, yc)
	}
}

func TestLayoutDiamond(t *testing.T) {
	// a blocks b and c; both block d. d must end up below b and c.
	g := Graph{
		Nodes: []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "a", To: "c"}, {From: "b", To: "d"}, {From: "c", To: "d"}},
	}
	d := Layout(g)
	ya, _ := layerOf(d, "a")
	yb, _ := layerOf(d, "b")
	yd, _ := layerOf(d, "d")
	if ya >= yb || yb >= yd {
		t.Errorf("diamond layering wrong: a=%d b=%d d=%d", ya, yb, yd)
	}
	if len(d.Edges) != 4 {
		t.Errorf("placed edges = %d, want 4", len(d.Edges))
	}
}

func TestLayoutDisconnected(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "x"}, {ID: "y"}}}
	d := Layout(g)
	if len(d.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(d.Nodes))
	}
	if len(d.Edges) != 0 {
		t.Errorf("edges = %d, want 0", len(d.Edges))
	}
	// Both roots sit on the same (top) layer.
	yx, _ := layerOf(d, "x")
	yy, _ := layerOf(d, "y")
	if yx != yy {
		t.Errorf("disconnected roots on different layers: x=%d y=%d", yx, yy)
	}
}

func TestLayoutDropsDanglingEdges(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "a"}},
		Edges: []Edge{{From: "a", To: "ghost"}, {From: "ghost", To: "a"}},
	}
	d := Layout(g)
	if len(d.Edges) != 0 {
		t.Errorf("dangling edges not dropped: %+v", d.Edges)
	}
	if len(d.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(d.Nodes))
	}
}

func TestLayoutCycleTerminates(t *testing.T) {
	// A 2-cycle must not hang or panic.
	g := Graph{
		Nodes: []Node{{ID: "a"}, {ID: "b"}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	d := Layout(g) // must return
	if len(d.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2", len(d.Nodes))
	}
}

func TestLayoutDeterministic(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "c"}, {ID: "a"}, {ID: "b"}},
		Edges: []Edge{{From: "a", To: "b"}, {From: "a", To: "c"}},
	}
	first := Layout(g)
	for i := 0; i < 5; i++ {
		again := Layout(g)
		if len(again.Nodes) != len(first.Nodes) {
			t.Fatalf("node count varied across runs")
		}
		for j := range first.Nodes {
			if again.Nodes[j] != first.Nodes[j] {
				t.Fatalf("layout not deterministic at node %d: %+v vs %+v", j, again.Nodes[j], first.Nodes[j])
			}
		}
	}
}

func TestRenderSVGContents(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "h-1", Title: "first", Status: "open"}, {ID: "h-2", Title: "second", Status: "blocked"}},
		Edges: []Edge{{From: "h-1", To: "h-2"}},
	}
	svg := RenderSVG(g)
	for _, want := range []string{
		"<svg",
		"h-1", "h-2",
		`data-node="h-1"`,
		`data-from="h-1"`,
		`data-to="h-2"`,
		`href="/issue/h-1"`,
		`marker-end="url(#dag-arrow)"`,
		`id="dag-arrow"`,
		"</svg>",
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg missing %q\n%s", want, svg)
		}
	}
}

func TestRenderSVGEscapesText(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: "h-1", Title: `<script>&"`, Status: "open"}}}
	svg := RenderSVG(g)
	if strings.Contains(svg, "<script>") {
		t.Errorf("unescaped title leaked into svg:\n%s", svg)
	}
	if !strings.Contains(svg, "&lt;script&gt;") {
		t.Errorf("title not XML-escaped:\n%s", svg)
	}
}

func TestRenderSVGEmptyGraphNoPanic(t *testing.T) {
	svg := RenderSVG(Graph{})
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Errorf("empty graph did not render a minimal valid svg: %q", svg)
	}
}

// TestRenderSVGWithDefaultMatchesRenderSVG: RenderSVG must be exactly RenderSVGWith with the
// zero options, so the issue-DAG output is unaffected by the role-flow extension.
func TestRenderSVGWithDefaultMatchesRenderSVG(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "h-1", Title: "first", Status: "open"}, {ID: "h-2", Title: "second", Status: "blocked"}},
		Edges: []Edge{{From: "h-1", To: "h-2"}},
	}
	if RenderSVG(g) != RenderSVGWith(g, RenderOptions{}) {
		t.Error("RenderSVG diverged from RenderSVGWith(zero options)")
	}
	// A plain (kind-less) graph must not emit the failure marker or dashed styling.
	svg := RenderSVG(g)
	if strings.Contains(svg, "dag-arrow-fail") || strings.Contains(svg, "stroke-dasharray") {
		t.Errorf("kind-less graph leaked failure-edge styling:\n%s", svg)
	}
}

// TestRenderSVGWithRoleFlow: the config role-flow options — a stage-kind fill, an
// anchor-suppressing NodeHref, and an on_failure edge — render distinctly: no /issue/ anchor,
// the dashed amber failure edge with its own marker, the custom fill, and the custom label.
func TestRenderSVGWithRoleFlow(t *testing.T) {
	g := Graph{
		Nodes: []Node{{ID: "plan", Title: "planner", Status: "plan"}, {ID: "implement", Title: "implementor", Status: "agent"}},
		Edges: []Edge{
			{From: "plan", To: "implement", Kind: EdgeProduces},
			{From: "implement", To: "plan", Kind: EdgeOnFailure},
		},
	}
	svg := RenderSVGWith(g, RenderOptions{
		NodeHref: func(string) string { return "" },
		NodeFill: func(status string) string {
			if status == "plan" {
				return "#abcdef"
			}
			return "#123456"
		},
		Label: "role flow",
	})
	if strings.Contains(svg, "/issue/") || strings.Contains(svg, "<a ") {
		t.Errorf("role-flow nodes should have no anchor:\n%s", svg)
	}
	if !strings.Contains(svg, `data-node="plan"`) {
		t.Errorf("missing stage node:\n%s", svg)
	}
	if !strings.Contains(svg, "#abcdef") {
		t.Errorf("custom NodeFill not applied:\n%s", svg)
	}
	if !strings.Contains(svg, `aria-label="role flow"`) {
		t.Errorf("custom label not applied:\n%s", svg)
	}
	if !strings.Contains(svg, `data-kind="on_failure"`) || !strings.Contains(svg, "dag-arrow-fail") || !strings.Contains(svg, "stroke-dasharray") {
		t.Errorf("on_failure edge not styled distinctly:\n%s", svg)
	}
}
