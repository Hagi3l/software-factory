// dag.js — the DAG view's hover-to-highlight interaction (specs/control-room.md, T4.6).
//
// Alpine evaluates x-data="dagHover()" against globals, so this exposes a global factory
// returning the component. On hover over a node it dims the whole graph (a class on the
// wrapper root) and re-activates the hovered node, its directly-connected neighbor nodes
// (both directions of the blocked-by edges), and the incident edges — so a glance shows what
// a given issue blocks and is blocked by. Clearing removes the dim/active classes. Clicking a
// node is the plain <a href="/issue/{id}"> drill-through rendered by dag.RenderSVG; this
// script only adds the visual emphasis, never navigation.
function dagHover() {
  return {
    hover(e) {
      const node = e.target.closest('[data-node]');
      if (!node) return;
      const id = node.getAttribute('data-node');
      const root = this.$root;
      root.classList.add('dag-dimmed');

      // Neighbors discovered from the edges incident to this node.
      const neighbors = new Set([id]);
      const edges = root.querySelectorAll('.dag-edge');
      edges.forEach((edge) => {
        const from = edge.getAttribute('data-from');
        const to = edge.getAttribute('data-to');
        if (from === id || to === id) {
          edge.classList.add('dag-active');
          neighbors.add(from);
          neighbors.add(to);
        }
      });

      neighbors.forEach((nid) => {
        const el = root.querySelector('[data-node="' + CSS.escape(nid) + '"]');
        if (el) el.classList.add('dag-active');
      });
    },
    clear() {
      const root = this.$root;
      root.classList.remove('dag-dimmed');
      root.querySelectorAll('.dag-active').forEach((el) => el.classList.remove('dag-active'));
    },
  };
}
