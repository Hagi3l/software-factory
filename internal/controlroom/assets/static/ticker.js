// ticker.js — the board's per-card live timers (specs/control-room.md "The board, in
// motion", T4.18).
//
// The server stamps each card's timer line with two epoch anchors — data-state-since (when
// the issue entered its current beads status, core.Issue.StateEnteredAt) and data-created
// (when beads created it) — and never re-renders to tick. Alpine evaluates
// x-data="cardTicker()" on that line; this factory reads the anchors and rewrites the two
// x-ref spans (state = time in the current state, total = time since creation) every second,
// entirely client-side. So the clock is live with no server round-trip; the current-state
// timer resets only when a real transition refetches the card with a fresh anchor.
//
// A missing/zero anchor renders as an em dash — the orchestrator had not stamped
// state_entered_at before T4.16, or the read missed it. Alpine calls init() when a card is
// added (including after an htmx swap, via its mutation observer) and destroy() when it is
// removed, so the interval is cleared on every refresh rather than leaking.
function cardTicker() {
  return {
    timer: null,
    init() {
      this.tick();
      this.timer = setInterval(() => this.tick(), 1000);
    },
    destroy() {
      if (this.timer) clearInterval(this.timer);
    },
    tick() {
      const now = Date.now() / 1000;
      this.render(this.$refs.state, this.$el.dataset.stateSince, now);
      this.render(this.$refs.total, this.$el.dataset.created, now);
    },
    render(el, anchor, now) {
      if (!el) return;
      const since = parseInt(anchor, 10);
      if (!since || since <= 0) {
        el.textContent = '—'; // em dash: unknown anchor
        return;
      }
      el.textContent = fmtDuration(Math.max(0, Math.floor(now - since)));
    },
  };
}

// fmtDuration renders whole seconds as a compact, glanceable h/m/s string (e.g. "45s",
// "3m12s", "2h05m", "1d04h"), matching the tabular density the board wants — the larger unit
// drops the trailing seconds once it would be noise.
function fmtDuration(s) {
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm' + String(s % 60).padStart(2, '0') + 's';
  const h = Math.floor(m / 60);
  if (h < 24) return h + 'h' + String(m % 60).padStart(2, '0') + 'm';
  const d = Math.floor(h / 24);
  return d + 'd' + String(h % 24).padStart(2, '0') + 'h';
}
