// "Scan now" without leaving the page (Story 5.26).
//
// The button is a real form and stays one: with this file blocked, disabled
// or broken, the press submits normally and the server's 303 brings the
// reader back to the page the form says it came from. This script only
// removes the navigation, which on the watchboard costs the reader their
// filter, their collapsed groups and their place in a long list every time
// they start a scan.
//
// Small and dependency-free, like every other script here: there is no
// bundler in this project and there is not going to be one (Tenet 14).

(function () {
  'use strict';

  // Anything missing here means the plain form submission is the better
  // press: no listener is attached, so the browser does what it always did.
  if (!window.fetch || !window.FormData || !window.URLSearchParams) {
    return;
  }

  var ok = document.getElementById('flash-ok');
  var bad = document.getElementById('flash-error');

  // How soon the page reloads after a scan starts, in seconds. It matches the
  // server's own refreshWhileRunningInterval (refresh.go), which a page that
  // never reloaded cannot pick up for itself.
  var afterStartSeconds = 10;

  function say(target, message) {
    if (ok) {
      ok.textContent = target === ok ? message : '';
    }

    if (bad) {
      bad.textContent = target === bad ? message : '';
    }
  }

  // The same markup the server renders for a running series, added to the
  // tile that was pressed so the board says at once what it would otherwise
  // only say after the next refresh.
  function markPending(form) {
    var tile = form.closest ? form.closest('.watch-tile') : null;
    if (!tile || tile.querySelector('.term-pending')) {
      return null;
    }

    var foot = tile.querySelector('.watch-tile-foot');
    if (!foot) {
      return null;
    }

    var mark = document.createElement('span');

    mark.className = 'term term-pending';
    mark.title = 'a scan of this series is running now';
    mark.textContent = 'pending';
    foot.insertBefore(mark, foot.firstChild);

    return mark;
  }

  function unmark(mark) {
    if (mark && mark.parentNode) {
      mark.parentNode.removeChild(mark);
    }
  }

  // The server's own sentence, whichever shape it arrives in. A body that is
  // not the JSON we asked for is reported as the status, rather than as
  // nothing at all.
  function messageFrom(response, payload) {
    if (payload && typeof payload === 'object') {
      if (typeof payload.error === 'string' && payload.error !== '') {
        return payload.error;
      }

      if (typeof payload.message === 'string' && payload.message !== '') {
        return payload.message;
      }
    }

    return 'The scan could not be started (' + response.status + ').';
  }

  function read(response) {
    return response.json().catch(function () {
      return null;
    });
  }

  document.addEventListener('submit', function (event) {
    var form = event.target;

    if (!form || !form.classList || !form.classList.contains('scan-form')) {
      return;
    }

    event.preventDefault();

    var button = form.querySelector('button[type="submit"], button:not([type])');
    var mark = markPending(form);

    if (button) {
      button.disabled = true;
    }

    function failed(message) {
      unmark(mark);

      if (button) {
        button.disabled = false;
      }

      say(bad, message);
    }

    fetch(form.action, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded'
      },
      body: new URLSearchParams(new FormData(form)).toString()
    }).then(function (response) {
      return read(response).then(function (payload) {
        if (!response.ok) {
          failed(messageFrom(response, payload));

          return;
        }

        say(ok, messageFrom(response, payload));

        // The button stays disabled: the scan it starts is now running, and
        // the server would refuse a second press anyway (runningFor, ui.go).
        // The refresh below brings back a page whose buttons are whatever the
        // server says they should be.
        if (button) {
          button.title = 'a scan of this series is already running';
        }

        if (window.wsaw && window.wsaw.refreshIn) {
          window.wsaw.refreshIn(afterStartSeconds);
        }
      });
    }).catch(function () {
      // Offline, blocked, or a connection that went away mid-request. The
      // press may or may not have reached the daemon, so it is reported as
      // unknown rather than as either outcome (Tenet 5).
      failed('The scan could not be started: wsaw did not answer. Reload the page to see whether it started.');
    });
  });
})();
