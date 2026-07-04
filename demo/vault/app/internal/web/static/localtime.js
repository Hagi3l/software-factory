// Rewrite UTC <time datetime> instants into the viewer's local timezone. The server
// stores and emits UTC only (RFC 3339 in the datetime attribute, a labeled-UTC text
// fallback for no-JS); the browser is the only place the user's zone is known, so
// formatting happens here. Runs on initial load and again after every htmx swap.
(function () {
  var brief = new Intl.DateTimeFormat(undefined, {
    month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
  });
  var full = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium', timeStyle: 'long',
  });

  function localize(root) {
    var scope = root && root.querySelectorAll ? root : document;
    var nodes = Array.prototype.slice.call(scope.querySelectorAll('time[datetime]'));
    if (scope !== document && scope.matches && scope.matches('time[datetime]')) {
      nodes.push(scope);
    }
    nodes.forEach(function (el) {
      var d = new Date(el.getAttribute('datetime'));
      if (isNaN(d)) return;
      el.textContent = brief.format(d);
      el.title = full.format(d); // hover shows the unabbreviated local time + zone
    });
  }

  document.addEventListener('DOMContentLoaded', function () { localize(document); });
  document.addEventListener('htmx:afterSwap', function (e) { localize(e.target); });
})();
