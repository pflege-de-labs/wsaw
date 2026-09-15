// Watchboard enhancements: filter as you type, remember which groups are
// collapsed, and submit the "hosts only" toggle the moment it changes.
//
// All three already work without this file. The filter and the toggle are
// one form that POSTs to /filter and applies on the server from a cookie;
// the groups are <details> elements, which collapse in every browser on
// their own. This adds only what script is actually needed for — filtering
// without a round trip, persisting a collapse that <details> forgets on
// navigation, and skipping the Apply click for a preference that always
// needs a round trip anyway.
//
// Dependency-free and in the same style as filter.js and refresh.js: there is
// no bundler in this project and there is not going to be one (Tenet 14).

(function () {
  'use strict';

  var input = document.getElementById('watch-filter-input');
  var groups = Array.prototype.slice.call(document.querySelectorAll('details.watch-env'));

  if (!groups.length) {
    // Not the board.
    return;
  }

  var ENVS_COOKIE = 'wsaw_envs';
  var FILTER_COOKIE = 'wsaw_filter';
  var YEAR = 365 * 24 * 60 * 60;

  function setCookie(name, value) {
    document.cookie = name + '=' + encodeURIComponent(value) +
      ';path=/;max-age=' + YEAR + ';samesite=lax' +
      (window.location.protocol === 'https:' ? ';secure' : '');
  }

  // Collapse state, written on every toggle so it survives a refresh — which
  // on a board with a 30s interval is the normal case, not the exception.
  function rememberCollapsed() {
    var collapsed = groups
      .filter(function (g) { return !g.open; })
      .map(function (g) { return g.getAttribute('data-env') || ''; })
      .filter(function (v) { return v !== ''; });

    setCookie(ENVS_COOKIE, collapsed.join(','));
  }

  groups.forEach(function (g) {
    g.addEventListener('toggle', rememberCollapsed);
  });

  // Severity is computed on the server, so this preference can't apply
  // itself the way the text filter does — it submits the form the moment
  // it's toggled instead of waiting for Apply, which the checkbox still
  // reaches with script off.
  var hostsOnly = document.getElementById('watch-hostsonly-input');
  if (hostsOnly && hostsOnly.form) {
    hostsOnly.addEventListener('change', function () {
      hostsOnly.form.submit();
    });
  }

  if (!input) {
    return;
  }

  var shownEl = document.getElementById('watch-shown');
  var rows = Array.prototype.slice.call(document.querySelectorAll('.watch-row'));
  var total = rows.length;

  // The same haystack the server matches on: name, url, labels and the
  // target's worst severity, all of which are visible on the row, so a
  // reader can see why it matched (Story 5.24, AC1).
  function haystack(row) {
    var rail = row.querySelector('.watch-rail');
    var labels = row.getAttribute('data-labels') || '';
    var severity = row.getAttribute('data-severity') || '';

    return ((rail ? rail.textContent : '') + ' ' + labels + ' ' + severity).toLowerCase();
  }

  // A target matches when every whitespace-separated token in the typed
  // filter is a substring of its haystack, independently — not necessarily
  // contiguous. A single-word query is unaffected; this is what lets a
  // severity word combine with a name/label/url word, e.g. "critical prod"
  // (Story 5.24, AC2 — must stay identical to filterTargets/watchboard.go).
  function rowMatchesAllTokens(row, tokens) {
    var hay = haystack(row);

    for (var i = 0; i < tokens.length; i++) {
      if (hay.indexOf(tokens[i]) === -1) {
        return false;
      }
    }

    return true;
  }

  function apply() {
    var tokens = input.value.trim().toLowerCase().split(/\s+/).filter(function (t) { return t !== ''; });
    var shown = 0;

    rows.forEach(function (row) {
      var match = tokens.length === 0 || rowMatchesAllTokens(row, tokens);

      row.hidden = !match;

      if (match) {
        shown++;
      }
    });

    // A group with nothing left in it is hidden too: an empty disclosure
    // reads as a group whose targets were deleted.
    groups.forEach(function (g) {
      var visible = Array.prototype.slice
        .call(g.querySelectorAll('.watch-row'))
        .some(function (row) { return !row.hidden; });

      g.hidden = !visible;
    });

    if (shownEl) {
      shownEl.textContent = 'showing ' + shown + '/' + total;
    }

    setCookie(FILTER_COOKIE, input.value.trim());
  }

  // A short debounce so a fast typist does not re-filter on every keystroke,
  // matching filter.js (Story 5.22, AC3).
  var DEBOUNCE_MS = 120;
  var timer = null;

  input.addEventListener('input', function () {
    if (timer !== null) {
      window.clearTimeout(timer);
    }

    timer = window.setTimeout(function () {
      timer = null;
      apply();
    }, DEBOUNCE_MS);
  });

  // The Apply button becomes redundant once filtering is live, and a button
  // that does nothing new is worse than no button.
  var submit = input.form ? input.form.querySelector('button[type="submit"]:not([name])') : null;
  if (submit) {
    submit.hidden = true;
  }
})();
