// wizard.js — chat ergonomics for the Create/Resolve wizard (specs/control-room.md "The
// wizard"). Alpine evaluates x-data="wizardChat()" against globals, so this exposes a global
// factory returning the component, mirroring dag.js/ticker.js. It powers two behaviors on the
// shared conversation (views/wizard.templ `wizardConversation`):
//
//   - Sticky auto-scroll. The transcript is a fixed-height scroll viewport; new turns arrive
//     either as an htmx innerHTML swap (the `turn` re-fetch) or as token-by-token text mutations
//     of the live `delta` target. A MutationObserver on the viewport pins it to the latest turn
//     so the human watches the reply stream in — but only while they are already at the bottom.
//     Once they scroll up to read history, `stick` goes false and the view stops yanking them
//     down; scrolling back to the bottom re-arms it.
//   - Enter to send. Enter submits the composer (via requestSubmit, so the `required` textarea is
//     still validated and htmx handles the hx-post); Shift+Enter falls through to a newline.
//   - Insert prepared requirement. When requirements_planner.prefill is configured, the composer
//     shows a button carrying the prepared text in data-prefill; insertPrefill drops it into the
//     textarea for the human to review and send.
function wizardChat() {
  return {
    stick: true,
    // watch(el) wires the scroll viewport (the #wizard-transcript element, passed as $el from its
    // x-init). `this` is the Alpine component, so stick is shared with the scroll listener.
    watch(el) {
      const atBottom = () => el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      const toBottom = () => { if (this.stick) el.scrollTop = el.scrollHeight; };
      el.addEventListener('scroll', () => { this.stick = atBottom(); });
      new MutationObserver(toBottom).observe(el, { childList: true, subtree: true, characterData: true });
      this.$nextTick(() => { el.scrollTop = el.scrollHeight; });
    },
    // send(e) handles the composer's keydown.enter: plain Enter submits, Shift+Enter newlines.
    send(e) {
      if (e.shiftKey) return;
      e.preventDefault();
      e.target.form.requestSubmit();
    },
    // insertPrefill(e) drops the prepared requirement (the button's data-prefill, from
    // requirements_planner.prefill) into the composer and focuses it. Insert only — the human
    // still reviews and presses Enter, so the send stays a deliberate act.
    insertPrefill(e) {
      const btn = e.currentTarget;
      const ta = btn.form.querySelector('textarea');
      ta.value = btn.dataset.prefill;
      ta.focus();
    },
  };
}

// --- Idle-gated panel backstop (specs/control-room.md "Rendering" — the interactive-panel
// exception). The ledger/draft/resolve panels refetch on their precise SSE nudge
// (sse:ledger/sse:draft) and on reconnect (htmx:sseOpen). Those cover every real change, but a
// nudge missed with no reconnect to recover it — a dropped frame, or an event that fires before
// htmx bound the listener — would strand the panel showing state the server has moved past, with
// no clock to converge it. That is acute when the wizard is embedded in an iframe that reloads (a
// slide deck). So a slow timer fires a `refresh` event the panels also listen for — but ONLY while
// the panel is IDLE, so a periodic refetch never discards a half-filled answer or snaps shut a spec
// diff the human is mid-read. "Idle" = nothing focused inside it, the ledger form not dirty
// (untouched since its last render), and no <details> open. The real nudge + sseOpen refetch stay
// unconditional (they fire only at a planner-turn boundary or on reconnect). This is the runtime
// half of the spec's idle-gated backstop; supersedes the T4.33 no-backstop rule.
(function wizardPanelBackstop() {
  var PANEL_IDS = ['wizard-ledger', 'wizard-draft', 'resolve-panel'];
  var REFRESH_MS = 12000;

  // Mark the ledger dirty on any input so an in-progress answer suppresses the timed refetch; a
  // real swap (afterSwap) re-renders it clean. Delegated from document so it survives the panel's
  // own innerHTML swaps (the listeners are never torn down with the swapped content).
  function markLedgerDirty(e) {
    var led = e.target.closest && e.target.closest('#wizard-ledger');
    if (led) led.dataset.dirty = '1';
  }
  document.addEventListener('input', markLedgerDirty);
  document.addEventListener('change', markLedgerDirty);
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target && e.target.id === 'wizard-ledger') delete e.target.dataset.dirty;
  });

  // idle reports whether the timed backstop may refetch this panel without clobbering the human:
  // nothing focused inside it, the ledger form not dirty, and no spec-diff <details> expanded.
  function idle(el) {
    return el &&
      !el.contains(document.activeElement) &&
      !el.dataset.dirty &&
      !el.querySelector('details[open]');
  }

  setInterval(function () {
    if (typeof htmx === 'undefined') return;
    for (var i = 0; i < PANEL_IDS.length; i++) {
      var el = document.getElementById(PANEL_IDS[i]);
      if (idle(el)) htmx.trigger(el, 'refresh');
    }
  }, REFRESH_MS);
})();

// wizardElapsed(elapsed) drives the activity line's live "mm:ss" clock. `elapsed` is the whole
// seconds the turn had already run when the server rendered this line; the component anchors a
// base time off the *client* clock at mount (now − elapsed) and ticks up from it every second, so
// the count is immune to client/server clock skew and survives the periodic transcript re-fetch
// (each re-render re-anchors from a fresh server elapsed). Cleared on teardown so a finalized turn
// leaves no interval running. Mirrors the board's client-ticked timers (ticker.js).
function wizardElapsed(elapsed) {
  const base = Date.now() / 1000 - elapsed;
  return {
    label: '',
    init() {
      const tick = () => {
        const secs = Math.max(0, Math.floor(Date.now() / 1000 - base));
        const m = Math.floor(secs / 60);
        const s = secs % 60;
        this.label = m + ':' + String(s).padStart(2, '0');
      };
      tick();
      this._timer = setInterval(tick, 1000);
    },
    destroy() {
      clearInterval(this._timer);
    },
  };
}
