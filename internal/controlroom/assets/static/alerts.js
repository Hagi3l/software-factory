// alerts.js — browser push for dead-letter arrivals (T4.19).
//
// The dead-letter queue is the human's only action surface (specs/control-room.md), so a new
// escalation is the one factory event worth a push to an operator who isn't looking — everything
// else in the control room is pull. This opens its own EventSource on /events purely for that
// push: when the DLQ pump broadcasts a "dlq-arrival" event, we fire a browser Notification. The
// status bar's own SSE connection handles the live count refresh; keeping the notification on a
// separate, tiny listener avoids depending on htmx-ext-sse internals for a browser-native capability.
//
// Best-effort throughout: if Notifications or EventSource are unsupported, or permission is denied,
// this is silently inert. The durable factory.dlq queue and the live status bar remain the source
// of truth — the notification is only the nudge to come look.
(function () {
  if (!("Notification" in window) || !("EventSource" in window)) {
    return;
  }

  // Ask once, on the first user gesture. Browsers (Firefox, and Chrome since 2020) reject
  // requestPermission() outside a "short running user-generated event handler" — calling it on
  // load logs a console warning and never prompts, so permission stays "default" and push never
  // works. Defer the ask to the first pointer/key interaction (a one-shot capturing listener,
  // self-removing) so the prompt is gesture-backed. A user who dismisses/denies, or a browser
  // that still defers, just leaves us inert — we never block the page on it.
  if (Notification.permission === "default") {
    var askOnce = function () {
      window.removeEventListener("pointerdown", askOnce, true);
      window.removeEventListener("keydown", askOnce, true);
      try {
        Notification.requestPermission();
      } catch (e) {
        // Older browsers expose the callback form only; ignore — we degrade to no push.
      }
    };
    window.addEventListener("pointerdown", askOnce, true);
    window.addEventListener("keydown", askOnce, true);
  }

  var es = new EventSource("/events");
  es.addEventListener("dlq-arrival", function (ev) {
    if (Notification.permission !== "granted") {
      return;
    }
    var issue = "";
    var reason = "";
    try {
      var d = JSON.parse(ev.data);
      issue = d.issue_id || "";
      reason = d.reason || "";
    } catch (e) {
      // Malformed payload — still notify generically rather than swallow the escalation.
    }
    var title = issue ? "Escalation: " + issue : "New escalation";
    var body = reason || "An issue needs a human in the dead-letter queue.";
    try {
      // tag dedups repeated notifications for the same issue into one.
      new Notification(title, { body: body, tag: issue || "harness-dlq" });
    } catch (e) {
      // Notification construction can throw on some platforms; ignore.
    }
  });
})();
