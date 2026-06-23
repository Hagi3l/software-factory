// lineage.js — the board's epic overlays (specs/control-room.md "Epics on the board", T7.8 +
// T10.4). It draws a bespoke client-side SVG layer over the live kanban with two kinds of curved
// connector: a SOLID lineage thread from each card to the card that produced it (so a feature reads
// as a tree threading left-to-right through the pipeline: root → its children → each child's next
// stage → qa), and a DASHED "waits-for" edge between siblings a planner's inter-child blocked-by
// ordering imposes (so a staggered sibling start reads as a dependency being honored — made real by
// T10.2 — not the pipeline stalling). This is the one deliberate client-side-graph exception
// (control-room.md "Graph viz") — connectors between the moving, browser-laid-out cards are
// inherently a client job; it pulls in no graph library and the full blocker graph stays in the
// server-side DAG view, this overlay being its at-a-glance subset for the work in motion.
//
// Both connector kinds carry a small DIRECTION DOT at their downstream end (a shared SVG marker,
// marker-end): the produced child for a lineage thread, the waiting sibling for a waits-for edge.
// The dot makes an edge read as a direction (prerequisite -> dependent) rather than an ambiguous
// line. It takes the edge's own colour via the SVG `context-stroke` keyword (one marker, every
// colour), and because per-edge fading is applied as the path element's group `opacity` — not
// `stroke-opacity`, which a marker ignores — the dot fades and brightens in lockstep with its line.
//
// It needs no new data: the server stamps each card with data-parent (its producer, derived in
// query.parentOf from the candidate base / decomposition root), data-waits-for (its intra-epic
// sibling blockers, query.waitsForOf), and data-epic (the shared epic id), and publishes the epic's
// color once as the --epic CSS custom property on the card (board.go cardStyle). This script reads
// that one color source — never re-implementing the Go FNV hash — so the tint, the badge dot, and
// both kinds of stroke always agree.
//
// The SVG is absolutely positioned inside the [data-board-scroll] container in content-space
// coordinates, so it scrolls with the cards (no redraw on scroll). It is faint by default;
// hovering or focusing a card highlights the whole path through it (its ancestors *and*
// descendants) and dims the rest, so relationships are explorable without the board looking
// busy. It redraws on each live column swap (htmx:afterSwap) and on resize; card moves animate
// via the View Transitions API, but getBoundingClientRect already reports the settled positions
// at swap time, so the threads land on the final layout rather than chasing the tween. Under
// prefers-reduced-motion the highlight opacity changes instantly (no transition).
(function () {
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const FAINT = '0.18';
  const BRIGHT = '0.95';
  const DIM = '0.05';

  function container() {
    return document.querySelector('#board [data-board-scroll]');
  }

  // cardEl resolves a card's element from its issue id (the server prefixes the dom id "card-").
  function cardEl(id) {
    return document.getElementById('card-' + id);
  }

  // contentRect maps a card to the scroll container's content-space box (offset by the current
  // scroll so the SVG, which lives in that same scrolled content, lines up).
  function contentRect(el, scroll, base) {
    const r = el.getBoundingClientRect();
    return {
      left: r.left - base.left + scroll.x,
      right: r.right - base.left + scroll.x,
      midY: (r.top + r.bottom) / 2 - base.top + scroll.y,
    };
  }

  // edges collects every drawable link present on the board. Two kinds, both reading the one --epic
  // color source: solid PRODUCER edges (a card's data-parent, the clean producer tree the lineage
  // thread draws) and dashed WAITS-FOR edges (a card's data-waits-for, the planner's inter-sibling
  // blocked-by ordering — T10.4). Each edge endpoint must be a card on the board (a producer/blocker
  // off the board, e.g. integrate which has no card, is skipped). It also builds the producer
  // adjacency the lineage highlight walks and the waits-for adjacency the highlight adds the hovered
  // card's direct predecessors/successors from.
  function collectEdges(c) {
    const edges = [];
    const parentOf = {};
    const childrenOf = {};
    const waitsOf = {}; // id -> [ids it waits for]
    const waitedBy = {}; // id -> [ids waiting on it]
    c.querySelectorAll('a[id^="card-"]').forEach((el) => {
      const child = el.id.replace(/^card-/, '');
      const color = getComputedStyle(el).getPropertyValue('--epic').trim() || 'currentColor';
      const parent = el.getAttribute('data-parent');
      if (parent && cardEl(parent)) { // producer on the board (integrate has no card)
        parentOf[child] = parent;
        (childrenOf[parent] = childrenOf[parent] || []).push(child);
        edges.push({ from: parent, to: child, color, dashed: false });
      }
      (el.getAttribute('data-waits-for') || '')
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
        .forEach((awaited) => {
          if (!cardEl(awaited)) return; // sibling blocker not on the board
          (waitsOf[child] = waitsOf[child] || []).push(awaited);
          (waitedBy[awaited] = waitedBy[awaited] || []).push(child);
          // from = the awaited (prerequisite) card, to = the waiting card — the dependency points
          // toward the work that may start once its blocker lands.
          edges.push({ from: awaited, to: child, color, dashed: true });
        });
    });
    return { edges, parentOf, childrenOf, waitsOf, waitedBy };
  }

  // lineageNodes returns the connected path through a card: its ancestor chain plus its whole
  // descendant subtree (and itself). An edge is highlighted iff both its endpoints fall in this
  // set, which is exactly the path through the hovered card.
  function lineageNodes(id, parentOf, childrenOf) {
    const set = new Set([id]);
    let cur = id;
    while (parentOf[cur]) {
      cur = parentOf[cur];
      set.add(cur);
    }
    const stack = [id];
    while (stack.length) {
      const n = stack.pop();
      (childrenOf[n] || []).forEach((ch) => {
        if (!set.has(ch)) {
          set.add(ch);
          stack.push(ch);
        }
      });
    }
    return set;
  }

  // edgePath builds the SVG cubic between a prerequisite anchor (from) and its dependent (to). A
  // producer edge, and any waits-for edge whose prerequisite sits clearly to the left, flows
  // left-to-right (from.right → to.left) like the lineage thread. A waits-for edge between cards in
  // the same column (or with the prerequisite to the right) instead bows outward on the right edges,
  // so stacked siblings get a readable vertical C-curve rather than a degenerate flat line.
  function edgePath(from, to, dashed) {
    if (!dashed || from.right <= to.left + 8) {
      const x1 = from.right, y1 = from.midY, x2 = to.left, y2 = to.midY;
      const dx = Math.max(36, (x2 - x1) / 2);
      return 'M ' + x1 + ' ' + y1 + ' C ' + (x1 + dx) + ' ' + y1 + ', ' + (x2 - dx) + ' ' + y2 + ', ' + x2 + ' ' + y2;
    }
    const x1 = from.right, y1 = from.midY, x2 = to.right, y2 = to.midY;
    const bow = 24 + Math.abs(y2 - y1) * 0.12;
    const cx = Math.max(x1, x2) + bow;
    return 'M ' + x1 + ' ' + y1 + ' C ' + cx + ' ' + y1 + ', ' + cx + ' ' + y2 + ', ' + x2 + ' ' + y2;
  }

  let active = null; // adjacency for the current draw, for the hover handler

  function draw() {
    const c = container();
    if (!c) {
      active = null;
      return;
    }
    const old = c.querySelector(':scope > svg[data-lineage]');
    if (old) old.remove();

    const { edges, parentOf, childrenOf, waitsOf, waitedBy } = collectEdges(c);
    active = { parentOf, childrenOf, waitsOf, waitedBy };
    if (edges.length === 0) return;

    const base = c.getBoundingClientRect();
    const scroll = { x: c.scrollLeft, y: c.scrollTop };
    const w = c.scrollWidth;
    const h = c.scrollHeight;

    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('data-lineage', '');
    svg.setAttribute('width', w);
    svg.setAttribute('height', h);
    svg.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    svg.style.cssText =
      'position:absolute;top:0;left:0;pointer-events:none;overflow:visible;z-index:0';

    // One shared direction-dot marker for every edge. fill:context-stroke makes the dot take the
    // referencing path's stroke (the edge's --epic colour) so a single marker serves all colours;
    // the fill="currentColor" attribute is the fallback a browser without context-stroke uses (a
    // visible monochrome dot, not black). markerUnits=strokeWidth scales it with the hover
    // thickening, like the line. refX/refY centre it on the path end, so it anchors to the
    // downstream card. A circle is rotation-symmetric, so no orient is needed.
    const defs = document.createElementNS(SVG_NS, 'defs');
    const marker = document.createElementNS(SVG_NS, 'marker');
    marker.setAttribute('id', 'lineage-dot');
    marker.setAttribute('viewBox', '0 0 8 8');
    marker.setAttribute('refX', '4');
    marker.setAttribute('refY', '4');
    marker.setAttribute('markerWidth', '4');
    marker.setAttribute('markerHeight', '4');
    marker.setAttribute('markerUnits', 'strokeWidth');
    const dot = document.createElementNS(SVG_NS, 'circle');
    dot.setAttribute('cx', '4');
    dot.setAttribute('cy', '4');
    dot.setAttribute('r', '3');
    dot.setAttribute('fill', 'currentColor'); // fallback
    dot.style.fill = 'context-stroke'; // modern: the edge's own colour
    marker.appendChild(dot);
    defs.appendChild(marker);
    svg.appendChild(defs);

    for (const e of edges) {
      const from = contentRect(cardEl(e.from), scroll, base);
      const to = contentRect(cardEl(e.to), scroll, base);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute('d', edgePath(from, to, e.dashed));
      path.setAttribute('fill', 'none');
      path.setAttribute('stroke', e.color);
      path.setAttribute('stroke-width', '1.5');
      path.setAttribute('stroke-linecap', 'round');
      // Fade via the element's group `opacity`, not `stroke-opacity`: group opacity composites the
      // stroke AND the marker dot together, so the direction dot fades and brightens with its line
      // (a marker ignores stroke-opacity and would otherwise float at full opacity over a faint line).
      path.setAttribute('opacity', FAINT);
      // The downstream direction dot (prerequisite -> dependent). Shared marker; takes this edge's
      // colour via context-stroke. On both edge kinds — the producer thread and the waits-for edge.
      path.setAttribute('marker-end', 'url(#lineage-dot)');
      // A dashed stroke distinguishes a "waits-for" ordering edge from a solid producer edge, so a
      // staggered sibling start reads as a dependency honored, not a stall (T10.4).
      if (e.dashed) path.setAttribute('stroke-dasharray', '5 4');
      if (!reduceMotion) path.style.transition = 'opacity 150ms, stroke-width 150ms';
      path.dataset.from = e.from;
      path.dataset.to = e.to;
      svg.appendChild(path);
    }
    c.appendChild(svg);
  }

  function highlight(id) {
    const c = container();
    if (!c || !active) return;
    const svg = c.querySelector(':scope > svg[data-lineage]');
    if (!svg) return;
    const nodes = id ? lineageNodes(id, active.parentOf, active.childrenOf) : null;
    if (nodes) {
      // Extend the lit set with the hovered card's direct waits-for neighbors (the siblings it
      // waits on, and those waiting on it), so its ordering edges light alongside its lineage path
      // (T10.4, control-room.md: "the hover/focus path highlight includes a card's waits-for
      // predecessors and successors").
      (active.waitsOf[id] || []).forEach((n) => nodes.add(n));
      (active.waitedBy[id] || []).forEach((n) => nodes.add(n));
    }
    svg.querySelectorAll('path').forEach((path) => {
      if (!nodes) {
        path.setAttribute('opacity', FAINT);
        path.setAttribute('stroke-width', '1.5');
        return;
      }
      const on = nodes.has(path.dataset.from) && nodes.has(path.dataset.to);
      path.setAttribute('opacity', on ? BRIGHT : DIM);
      path.setAttribute('stroke-width', on ? '2.5' : '1.5');
    });
  }

  // Hover/focus a card to light its lineage; leaving restores the faint default. Delegated on the
  // document so the handlers survive the #board innerHTML swap that the live refetch performs.
  function cardIdFrom(target) {
    const a = target.closest && target.closest('#board a[id^="card-"]');
    return a ? a.id.replace(/^card-/, '') : null;
  }
  document.addEventListener('mouseover', (e) => {
    const id = cardIdFrom(e.target);
    if (id) highlight(id);
  });
  document.addEventListener('mouseout', (e) => {
    if (cardIdFrom(e.target)) highlight(null);
  });
  document.addEventListener('focusin', (e) => {
    const id = cardIdFrom(e.target);
    if (id) highlight(id);
  });
  document.addEventListener('focusout', (e) => {
    if (cardIdFrom(e.target)) highlight(null);
  });

  // Redraw after every live column swap. The cards are in their settled DOM positions at
  // afterSwap (the View Transitions tween is a pseudo-element overlay), so the threads land on
  // the final layout. requestAnimationFrame defers one frame so layout is flushed first.
  document.addEventListener('htmx:afterSwap', (e) => {
    if (e.target && e.target.id === 'board') requestAnimationFrame(draw);
  });

  let resizeTimer = null;
  window.addEventListener('resize', () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(draw, 100);
  });

  draw();
})();
