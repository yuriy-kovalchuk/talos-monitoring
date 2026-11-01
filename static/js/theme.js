// theme.js light/dark theme. Loaded without defer in <head>, so the theme is set before first paint.
//
// The preference cycles system → light → dark. "system" follows the OS and
// tracks it live; the choice is per browser, so it lives in localStorage, which
// can be unavailable (private mode, blocked storage) and then lasts the page view.
(function () {
  'use strict';
  var root = document.documentElement;
  var media = window.matchMedia('(prefers-color-scheme: dark)');
  var order = ['system', 'light', 'dark'];
  var key = 'tm-theme';
  var pref = 'system';

  try {
    if (order.indexOf(localStorage.getItem(key)) >= 0) pref = localStorage.getItem(key);
  } catch (e) {
    // Storage unavailable: follow the OS.
  }

  function apply() {
    root.setAttribute('data-theme-pref', pref);
    root.setAttribute('data-theme', pref === 'system' ? (media.matches ? 'dark' : 'light') : pref);
    var btn = document.getElementById('theme-toggle');
    if (btn) btn.setAttribute('aria-label', 'Theme: ' + pref + '. Switch theme');
  }

  apply();
  media.addEventListener('change', apply);

  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    apply();
    btn.addEventListener('click', function () {
      pref = order[(order.indexOf(pref) + 1) % order.length];
      try {
        localStorage.setItem(key, pref);
      } catch (e) {
        // Storage refused: the choice lasts this page view.
      }
      apply();
    });
  });
})();
