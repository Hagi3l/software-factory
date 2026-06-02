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
// this is silently inert. The durable harness.dlq queue and the live status bar remain the source
// of truth — the notification is only the nudge to come look.
(function () {
  if (!("Notification" in window) || !("EventSource" in window)) {
    return;
  }

  // Ask once on load. A browser that defers the prompt without a user gesture, or a user who
  // dismisses/denies it, just leaves us inert — we never block the page on it.
  if (Notification.permission === "default") {
    try {
      Notification.requestPermission();
    } catch (e) {
      // Older browsers expose the callback form only; ignore — we degrade to no push.
    }
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
