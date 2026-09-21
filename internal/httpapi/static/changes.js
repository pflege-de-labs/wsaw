// Instant filtering of a scan page's Changes section (Story 5.29).
//
// The section already works without this file: each chip is a link carrying
// ?changes=<kind>, the server applies it, and the rows it excludes come back
// marked `hidden`. What script adds is the round trip's removal — every row
// is already in the DOM, so switching facets is a class and an attribute, and
// a reader comparing hosts against requests does not reload a page holding a
// scan's full request table to do it.
//
// Dependency-free and in the same style as watch.js and refresh.js: there is
// no bundler in this project and there is not going to be one (Tenet 14).

(function () {
  'use strict';

  var summary = document.getElementById('change-summary');
  var table = document.getElementById('changes-table');

  if (!summary || !table) {
    // Not a scan page, or one whose scan has nothing to compare against.
    return;
  }

  var chips = Array.prototype.slice.call(summary.querySelectorAll('a.chip'));
  var rows = Array.prototype.slice.call(table.querySelectorAll('tbody tr[data-kind]'));

  function apply(kind) {
    rows.forEach(function (row) {
      row.hidden = kind !== '' && row.getAttribute('data-kind') !== kind;
    });

    chips.forEach(function (chip) {
      var on = (chip.getAttribute('data-kind') || '') === kind;

      chip.classList.toggle('is-on', on);

      if (on) {
        chip.setAttribute('aria-current', 'true');
      } else {
        chip.removeAttribute('aria-current');
      }
    });
  }

  chips.forEach(function (chip) {
    chip.addEventListener('click', function (event) {
      // Let a deliberate open-elsewhere through untouched: a middle click or
      // a modified click is a request for a second tab, and that tab needs
      // the link to still be a link.
      if (event.button !== 0 || event.metaKey || event.ctrlKey ||
          event.shiftKey || event.altKey) {
        return;
      }

      event.preventDefault();

      apply(chip.getAttribute('data-kind') || '');

      // The address bar keeps up, so a reload, a bookmark or a copied link
      // reproduces what is on screen — and replaceState rather than
      // pushState, because a filter is a way of looking at one page, not a
      // place the back button should have to walk out of chip by chip.
      if (window.history && window.history.replaceState) {
        window.history.replaceState(null, '', chip.href);
      }
    });
  });
}());
