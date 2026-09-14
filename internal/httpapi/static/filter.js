// Instant, client-side filtering of the target list (Story 5.22).
//
// The dashboard already renders every target's current state in one response
// (Story 5.8, AC1); the data a filter needs is on the page before this file
// even runs. So filtering never asks the daemon for anything — it hides and
// shows rows already in the DOM, which is instant by construction and adds
// no load to a page that might already be watching hundreds of targets.
//
// It is deliberately small and dependency-free, matching refresh.js: there is
// no bundler in this project and there is not going to be one (Tenet 14).

(function () {
  'use strict';

  var panel = document.getElementById('target-filters');
  if (!panel) {
    // Not the dashboard, or JavaScript-disabled fallback already covers it:
    // the panel starts `hidden` in markup and nothing here runs to reveal it
    // (Story 5.22, AC8).
    return;
  }

  var sections = Array.prototype.slice.call(document.querySelectorAll('section.target'));
  var countEl = document.getElementById('filter-count');
  var labelInput = document.getElementById('filter-label');
  var resetButton = document.getElementById('filter-reset');
  var checkboxes = Array.prototype.slice.call(panel.querySelectorAll('input[type="checkbox"]'));

  // Query-string keys are short and stable so a shared or bookmarked link
  // stays readable (Story 5.22, AC7).
  var PARAM = { severity: 'sev', outcome: 'outcome', mode: 'mode', label: 'label' };

  function groupValues(name) {
    var set = {};

    checkboxes
      .filter(function (cb) { return cb.name === name; })
      .forEach(function (cb) {
        if (cb.checked) {
          set[cb.value] = true;
        }
      });

    return set;
  }

  function isEmpty(set) {
    for (var k in set) {
      if (Object.prototype.hasOwnProperty.call(set, k)) {
        return false;
      }
    }

    return true;
  }

  function currentFilters() {
    return {
      severity: groupValues('severity'),
      outcome: groupValues('outcome'),
      mode: groupValues('mode'),
      label: labelInput.value.trim().toLowerCase(),
    };
  }

  function anyActive(f) {
    return !isEmpty(f.severity) || !isEmpty(f.outcome) || !isEmpty(f.mode) || f.label !== '';
  }

  // A row matches when it satisfies every active dimension (AND) and, within
  // a dimension with more than one value checked, any one of them (OR) — the
  // usual faceted-filter convention (Story 5.22, AC1).
  function rowMatches(row, f) {
    if (!isEmpty(f.severity) && !f.severity[row.getAttribute('data-severity')]) {
      return false;
    }

    if (!isEmpty(f.outcome) && !f.outcome[row.getAttribute('data-outcome')]) {
      return false;
    }

    if (!isEmpty(f.mode)) {
      var modes = (row.getAttribute('data-modes') || '').split(' ').filter(Boolean);
      var matchesMode = modes.some(function (m) { return f.mode[m]; });

      if (!matchesMode) {
        return false;
      }
    }

    if (f.label !== '') {
      var labels = (row.getAttribute('data-labels') || '').toLowerCase();

      if (labels.indexOf(f.label) === -1) {
        return false;
      }
    }

    return true;
  }

  function apply() {
    var f = currentFilters();
    var visibleTargets = 0;

    sections.forEach(function (section) {
      var rows = Array.prototype.slice.call(section.querySelectorAll('tbody tr[data-severity]'));
      var visibleRows = 0;

      rows.forEach(function (row) {
        var match = rowMatches(row, f);
        row.hidden = !match;

        if (match) {
          visibleRows += 1;
        }
      });

      // A target with rows for more than one mode stays visible with only
      // the matching rows shown; it disappears only once none of its rows
      // match (Story 5.22, AC4).
      section.hidden = visibleRows === 0;

      if (visibleRows > 0) {
        visibleTargets += 1;
      }
    });

    if (countEl) {
      countEl.textContent = 'showing ' + visibleTargets + ' of ' + sections.length + ' targets';
    }

    if (resetButton) {
      resetButton.disabled = !anyActive(f);
    }

    updateURL(f);
  }

  function updateURL(f) {
    var params = new URLSearchParams(window.location.search);

    setListParam(params, PARAM.severity, Object.keys(f.severity));
    setListParam(params, PARAM.outcome, Object.keys(f.outcome));
    setListParam(params, PARAM.mode, Object.keys(f.mode));

    if (f.label !== '') {
      params.set(PARAM.label, labelInput.value.trim());
    } else {
      params.delete(PARAM.label);
    }

    var query = params.toString();
    var url = window.location.pathname + (query ? '?' + query : '');

    // replaceState, not pushState: a filter narrows and widens the same view
    // rather than navigating to a new one, so it must not fill the back
    // button with one entry per keystroke or checkbox click. The URL still
    // carries the filter, so it survives a reload, a bookmark, or a link
    // handed to a colleague (Story 5.22, AC7).
    window.history.replaceState(null, '', url);
  }

  function setListParam(params, key, values) {
    if (values.length) {
      params.set(key, values.join(','));
    } else {
      params.delete(key);
    }
  }

  function fromURL() {
    var params = new URLSearchParams(window.location.search);

    checkFromParam(params, PARAM.severity, 'severity');
    checkFromParam(params, PARAM.outcome, 'outcome');
    checkFromParam(params, PARAM.mode, 'mode');

    var label = params.get(PARAM.label);
    if (label) {
      labelInput.value = label;
    }
  }

  function checkFromParam(params, key, name) {
    var raw = params.get(key);
    if (!raw) {
      return;
    }

    var wanted = {};
    raw.split(',').forEach(function (v) {
      if (v) {
        wanted[v] = true;
      }
    });

    checkboxes
      .filter(function (cb) { return cb.name === name; })
      .forEach(function (cb) {
        cb.checked = !!wanted[cb.value];
      });
  }

  function reset() {
    checkboxes.forEach(function (cb) { cb.checked = false; });
    labelInput.value = '';
    apply();
  }

  // No debounce for checkboxes/selects: a click is a single, deliberate
  // event already. Free text gets a short one so a fast typist does not
  // trigger a full-page re-filter on every keystroke (Story 5.22, AC3).
  var LABEL_DEBOUNCE_MS = 120;
  var labelTimer = null;

  checkboxes.forEach(function (cb) {
    cb.addEventListener('change', apply);
  });

  labelInput.addEventListener('input', function () {
    if (labelTimer !== null) {
      window.clearTimeout(labelTimer);
    }

    labelTimer = window.setTimeout(function () {
      labelTimer = null;
      apply();
    }, LABEL_DEBOUNCE_MS);
  });

  if (resetButton) {
    resetButton.addEventListener('click', reset);
  }

  fromURL();
  apply();

  // Reveal the panel only once it is wired up and already reflects the URL,
  // so nothing usable ever flashes before it works.
  panel.hidden = false;
})();
