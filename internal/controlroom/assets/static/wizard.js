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
