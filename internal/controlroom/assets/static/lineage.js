// lineage.js — the board's epic-lineage thread (specs/control-room.md "Epics on the board",
// T7.8). It draws a bespoke client-side SVG layer over the live kanban: a curved connector from
// each card to the card that produced it, so a feature reads as a tree threading left-to-right
// through the pipeline (root → its children → each child's next stage → qa). This is the one
// deliberate client-side-graph exception (control-room.md "Graph viz") — connectors between the
// moving, browser-laid-out cards are inherently a client job; it pulls in no graph library and
// the DAG view stays server-side SVG.
//
// It needs no new data: the server stamps each card with data-parent (its producer, derived in
// query.parentOf from the candidate base / decomposition root) and data-epic (the shared epic
// id), and publishes the epic's color once as the --epic CSS custom property on the card
// (board.go cardStyle). This script reads that one color source — never re-implementing the Go
// FNV hash — so the tint, the badge dot, and these strokes always agree.
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

  // edges collects every drawable producer link present on the board: a card with a data-parent
  // whose parent card is also on the board (the thread is a clean producer tree). It also builds
  // the parent/children adjacency the hover-highlight walks.
  function collectEdges(c) {
    const edges = [];
    const parentOf = {};
    const childrenOf = {};
    c.querySelectorAll('a[data-parent]').forEach((el) => {
      const child = el.id.replace(/^card-/, '');
      const parent = el.getAttribute('data-parent');
      if (!parent || !cardEl(parent)) return; // producer not on the board (e.g. integrate has no card)
      parentOf[child] = parent;
      (childrenOf[parent] = childrenOf[parent] || []).push(child);
      const color = getComputedStyle(el).getPropertyValue('--epic').trim() || 'currentColor';
      edges.push({ child, parent, color });
    });
    return { edges, parentOf, childrenOf };
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

  let active = null; // adjacency for the current draw, for the hover handler

  function draw() {
    const c = container();
    if (!c) {
      active = null;
      return;
    }
    const old = c.querySelector(':scope > svg[data-lineage]');
    if (old) old.remove();

    const { edges, parentOf, childrenOf } = collectEdges(c);
    active = { parentOf, childrenOf };
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

    for (const e of edges) {
      const p = contentRect(cardEl(e.parent), scroll, base);
      const ch = contentRect(cardEl(e.child), scroll, base);
      const x1 = p.right;
      const y1 = p.midY;
      const x2 = ch.left;
      const y2 = ch.midY;
      const dx = Math.max(36, (x2 - x1) / 2);
      const path = document.createElementNS(SVG_NS, 'path');
      path.setAttribute(
        'd',
        'M ' + x1 + ' ' + y1 + ' C ' + (x1 + dx) + ' ' + y1 + ', ' + (x2 - dx) + ' ' + y2 + ', ' + x2 + ' ' + y2
      );
      path.setAttribute('fill', 'none');
      path.setAttribute('stroke', e.color);
      path.setAttribute('stroke-width', '1.5');
      path.setAttribute('stroke-linecap', 'round');
      path.setAttribute('stroke-opacity', FAINT);
      if (!reduceMotion) path.style.transition = 'stroke-opacity 150ms, stroke-width 150ms';
      path.dataset.child = e.child;
      path.dataset.parent = e.parent;
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
    svg.querySelectorAll('path').forEach((path) => {
      if (!nodes) {
        path.setAttribute('stroke-opacity', FAINT);
        path.setAttribute('stroke-width', '1.5');
        return;
      }
      const on = nodes.has(path.dataset.child) && nodes.has(path.dataset.parent);
      path.setAttribute('stroke-opacity', on ? BRIGHT : DIM);
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
