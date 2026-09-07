// Auto-refresh, when JavaScript is available (Story 5.16).
//
// The page already refreshes without this file: the layout emits a meta
// refresh inside <noscript>, which is what a browser with script disabled
// uses. This script is the enhancement, and it earns its place by doing two
// things a meta refresh cannot.
//
// It holds off while the tab is hidden. Refreshing a page nobody is looking
// at spends the daemon's time re-reading every target's latest result for an
// audience of nobody; a wall display that gets switched to another tab for an
// hour should not cost a hundred renders.
//
// And it keeps the age on screen honest. A meta refresh that never fires —
// a suspended tab, a daemon that went away, a lost connection — leaves a page
// that looks current and is not. Counting the age up from the render time
// means a stale page says so, which is the whole point of Story 5.16's AC6.
//
// It is deliberately small and dependency-free. There is no bundler in this
// project and there is not going to be one (Tenet 14).

(function () {
  'use strict';

  var el = document.getElementById('freshness');
  if (!el) {
    return;
  }

  var renderedAt = new Date(el.getAttribute('data-rendered-at'));
  var seconds = parseInt(el.getAttribute('data-refresh-seconds'), 10) || 0;
  var url = el.getAttribute('data-refresh-url') || window.location.pathname;

  var ageEl = document.getElementById('freshness-age');

  // A page is called stale once it is meaningfully older than it promised to
  // be. The grace is generous on purpose: a refresh that is a second late is
  // not a problem worth shouting about.
  function staleAfter() {
    return seconds > 0 ? seconds * 1000 * 2 + 5000 : 0;
  }

  function describe(ms) {
    var s = Math.round(ms / 1000);

    if (s < 60) {
      return s + 's old';
    }

    if (s < 3600) {
      return Math.floor(s / 60) + 'm old';
    }

    return Math.floor(s / 3600) + 'h old';
  }

  function tick() {
    var age = Date.now() - renderedAt.getTime();

    if (ageEl) {
      // Below ten seconds the age is noise; the "as of" time already says it.
      ageEl.textContent = age >= 10000 ? '· ' + describe(age) : '';
    }

    var limit = staleAfter();

    if (limit > 0 && age > limit) {
      el.classList.add('is-stale');

      if (ageEl) {
        ageEl.textContent = '· ' + describe(age) + ', not refreshing';
      }
    }
  }

  tick();
  window.setInterval(tick, 1000);

  if (seconds <= 0) {
    // Auto-refresh is off. The age still counts up, because knowing how old
    // the page is matters just as much when nothing is going to change it.
    return;
  }

  var timer = null;

  function refresh() {
    // location.replace rather than assign: a refresh should not fill the back
    // button with copies of the same page.
    window.location.replace(url);
  }

  function schedule() {
    if (timer !== null) {
      return;
    }

    timer = window.setTimeout(function () {
      timer = null;

      if (document.hidden) {
        // Nobody is looking. Wait for them to come back rather than
        // refreshing into an unwatched tab.
        return;
      }

      refresh();
    }, seconds * 1000);
  }

  function cancel() {
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
    }
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      cancel();

      return;
    }

    // Coming back to a page that is already older than its interval should
    // show current data at once rather than after another full interval.
    if (Date.now() - renderedAt.getTime() >= seconds * 1000) {
      refresh();

      return;
    }

    schedule();
  });

  if (!document.hidden) {
    schedule();
  }
})();
