// Fills each tile's cycle bar (dashboard.html, .watch-cycle-fill).
//
// The bar's width is a data-percent attribute rather than an inline
// "width: N%" style, and this file is why: the page's Content-Security-Policy
// (server.go) sets style-src with no unsafe-inline, which drops inline style
// attributes silently — the div would keep its CSS default width instead of
// failing loudly, so every bar would read the same regardless of its
// percentage. A width set through the CSSOM, as here, is not inline style and
// is not subject to style-src at all.
//
// Without this file the bar stays at its CSS default (empty). That is a
// second reading lost, not information lost: the age and next-run text next
// to it already say the same thing in full (Tenet 14, dependency-free like
// refresh.js, filter.js and watch.js).

(function () {
  'use strict';

  var bars = document.querySelectorAll('.watch-cycle-fill[data-percent]');

  Array.prototype.forEach.call(bars, function (bar) {
    bar.style.width = bar.getAttribute('data-percent') + '%';
  });
})();
