// app.js the dashboard's only script: the node switcher and connection state.
//
// The switcher lives here rather than as an inline onchange= because the
// Content-Security-Policy is script-src 'self': an inline handler is blocked,
// and the dropdown would silently stop navigating.
(function () {
  'use strict';
  var sel = document.getElementById('node-select');
  if (!sel) return;
  sel.addEventListener('change', function () {
    if (sel.value) window.location.href = sel.value;
  });
})();

// Connection state.
//
// htmx does not swap on a failed response, which is the right default: the
// region keeps its last good content instead of blanking. But for a
// monitoring UI that is dangerous on its own — the page goes on showing
// numbers that look live while nothing is updating. Surface it.
//
// Per region: the element a failed poll belongs to is marked, and only a
// success on that same element clears it. A single global flag instead let a
// healthy 30 s status poll erase the mark of a permanently failing node
// partial.
(function () {
  'use strict';
  var root = document.documentElement;
  var banner = document.getElementById('stale-banner');
  var failing = new WeakSet();
  var n = 0;

  function mark(elt, on) {
    if (!elt || failing.has(elt) === on) return;
    if (on) {
      failing.add(elt);
      elt.setAttribute('data-stale', '');
      if (++n === 1) {
        root.setAttribute('data-stale', '');
        // The banner is an aria-live region present from page load, so
        // inserting the text (not flipping visibility) is what gets announced.
        if (banner) banner.textContent = 'Connection lost — showing the last values received.';
      }
    } else {
      failing.delete(elt);
      elt.removeAttribute('data-stale');
      if (--n === 0) {
        root.removeAttribute('data-stale');
        if (banner) banner.textContent = '';
      }
    }
  }

  // The events bubble to the document, so one listener covers every region.
  // detail.target is the element the request belongs to; on a self-swap poll
  // that is the polled region itself.
  document.body.addEventListener('htmx:responseError', function (e) { mark(e.detail.target, true); });
  document.body.addEventListener('htmx:sendError', function (e) { mark(e.detail.target, true); });
  document.body.addEventListener('htmx:timeout', function (e) { mark(e.detail.target, true); });
  document.body.addEventListener('htmx:afterOnLoad', function (e) {
    if (e.detail && e.detail.xhr && e.detail.xhr.status >= 200 && e.detail.xhr.status < 300) {
      mark(e.detail.target, false);
    }
  });
})();
