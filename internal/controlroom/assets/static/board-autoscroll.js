// board-autoscroll.js — keep the board scrolled to the work frontier (specs/control-room.md
// "Follow the frontier", T4.30). The board is as wide as the whole pipeline, so on a real run
// it overflows the viewport; this follows the work left→right so the operator never chases it
// with the scrollbar.
//
// The frontier is decided server-side (query.frontierColumn) — the leftmost column with any
// incomplete card, else the rightmost when everything is closed — and the focal column is
// tagged data-board-focus. This script just scrolls that column into view: left-aligned
// (inline:'start') so the frontier *and the road ahead* of it show. It runs on first paint
// (instant) and on every live board swap (smooth). All its durable state — the toggle button
// and the document-level listeners — lives on the stable chrome, so it survives the #board
// innerHTML swap that the SSE refetch performs.
//
// On by default, remembered in localStorage. The header toggle turns it off and back on; any
// manual horizontal scroll also pauses it until the operator re-enables via the toggle, so the
// human always stays in control of their own viewport. prefers-reduced-motion is honored by
// dropping the smooth animation (the repositioning still happens, just instantly).
(function () {
  const STORAGE_KEY = 'harness.board.autoscroll';
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Default on; only an explicit "off" (a prior toggle/pause) disables it.
  let enabled = localStorage.getItem(STORAGE_KEY) !== 'off';

  function reflect() {
    const btn = document.getElementById('board-autoscroll-toggle');
    if (btn) btn.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  }

  function scrollToFrontier(smooth) {
    if (!enabled) return;
    const focus = document.querySelector('#board [data-board-focus]');
    if (!focus) return;
    focus.scrollIntoView({
      inline: 'start',
      block: 'nearest',
      behavior: smooth && !reduceMotion ? 'smooth' : 'auto',
    });
  }

  function setEnabled(on, persist) {
    enabled = on;
    if (persist) localStorage.setItem(STORAGE_KEY, on ? 'on' : 'off');
    reflect();
    if (on) scrollToFrontier(true);
  }

  // The header toggle is the only way back on once paused, so it binds to the stable chrome.
  document.addEventListener('click', (e) => {
    if (e.target.closest && e.target.closest('#board-autoscroll-toggle')) {
      setEnabled(!enabled, true);
    }
  });

  // A manual horizontal scroll yields the viewport to the operator until they re-enable via
  // the toggle. wheel/touchmove only come from real input — never from our own programmatic
  // scrollIntoView — so there are no false pauses; the deltaX guard ignores a vertical wheel
  // that is merely scrolling the page past the board.
  document.addEventListener('wheel', (e) => {
    if (!enabled || Math.abs(e.deltaX) <= Math.abs(e.deltaY)) return;
    if (e.target.closest && e.target.closest('#board')) setEnabled(false, true);
  }, { passive: true });
  document.addEventListener('touchmove', (e) => {
    if (!enabled) return;
    if (e.target.closest && e.target.closest('#board')) setEnabled(false, true);
  }, { passive: true });

  // Re-follow on every live board swap: the columns fragment re-renders with a fresh
  // data-board-focus as work advances. The listener is on the document (not #board) so it
  // outlives the innerHTML swap.
  document.addEventListener('htmx:afterSwap', (e) => {
    if (e.target && e.target.id === 'board') scrollToFrontier(true);
  });

  reflect();
  scrollToFrontier(false); // first paint: snap, no animation
})();
