package consent

// helperScript defines the primitives rules rely on. It is injected before
// any rule runs, in every frame.
//
// It exists because CMPs hide their controls in places a plain
// document.querySelector cannot reach: open shadow roots and nested
// same-origin iframes. Rules should stay declarative, so the traversal lives
// here rather than being re-expressed in every rule's selector.
const helperScript = `
(() => {
  if (window.__wsawHelpers) return;
  window.__wsawHelpers = true;

  // Collect the document plus every reachable open shadow root, so a
  // selector can match inside a web component.
  const roots = () => {
    const out = [document];
    const walk = (node) => {
      const all = node.querySelectorAll('*');
      for (const el of all) {
        if (el.shadowRoot) {
          out.push(el.shadowRoot);
          walk(el.shadowRoot);
        }
      }
    };
    try { walk(document); } catch (e) { /* detached nodes are not interesting */ }
    return out;
  };

  window.__wsawQuery = (selector) => {
    for (const root of roots()) {
      try {
        const el = root.querySelector(selector);
        if (el) return el;
      } catch (e) { /* an invalid selector is a rule bug, not a page problem */ }
    }
    return null;
  };

  const visible = (el) => {
    if (!el) return false;
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden' || style.pointerEvents === 'none') return false;
    if (parseFloat(style.opacity || '1') < 0.1) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };

  window.__wsawClick = (selector) => {
    const el = window.__wsawQuery(selector);
    if (!el) return { clicked: false, reason: 'no element matched ' + selector };
    if (!visible(el)) {
      // Click it anyway: some CMPs keep the real control off-screen and rely
      // on a handler. Record that it was not visible so the caller can tell.
      try { el.click(); } catch (e) { return { clicked: false, reason: String(e) }; }
      return { clicked: true, visible: false };
    }
    try {
      el.scrollIntoView({ block: 'center', inline: 'center' });
      el.click();
    } catch (e) {
      return { clicked: false, reason: String(e) };
    }
    return { clicked: true, visible: true };
  };

  // Label matching for the heuristic fallback. Deliberately conservative:
  // only clickable elements, only short labels, longest match first so that
  // "reject all" is preferred over a stray "all".
  window.__wsawClickLabel = (labels) => {
    const clickable = [];
    for (const root of roots()) {
      try {
        clickable.push(...root.querySelectorAll('button,a[href],[role="button"],input[type="button"],input[type="submit"]'));
      } catch (e) { /* ignore */ }
    }

    const norm = (s) => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();

    for (const label of labels) {
      for (const el of clickable) {
        const text = norm(el.innerText || el.textContent || el.value || el.getAttribute('aria-label'));
        if (!text || text.length > 60) continue;
        if (text === label) {
          if (!visible(el)) continue;
          try {
            el.scrollIntoView({ block: 'center' });
            el.click();
            return { clicked: true, matched: label, exact: true };
          } catch (e) { /* try the next candidate */ }
        }
      }
    }

    // Only after exact matching fails, allow a contains match.
    for (const label of labels) {
      for (const el of clickable) {
        const text = norm(el.innerText || el.textContent || el.value || el.getAttribute('aria-label'));
        if (!text || text.length > 60) continue;
        if (text.includes(label)) {
          if (!visible(el)) continue;
          try {
            el.scrollIntoView({ block: 'center' });
            el.click();
            return { clicked: true, matched: label, exact: false };
          } catch (e) { /* try the next candidate */ }
        }
      }
    }

    return { clicked: false, reason: 'no element matched any known label' };
  };
})()
`

// tcfProbeScript reports what consent APIs the page exposes. Detection is
// separate from interaction so that "no CMP detected" is a first-class,
// recorded outcome rather than an interaction failure.
const tcfProbeScript = `
(() => {
  const out = { tcf: false, gpp: false, usp: false, cmpId: null, cmpVersion: null, tcfLocator: false };
  try { out.tcf = typeof window.__tcfapi === 'function'; } catch (e) {}
  try { out.gpp = typeof window.__gpp === 'function'; } catch (e) {}
  try { out.usp = typeof window.__uspapi === 'function'; } catch (e) {}
  try {
    out.tcfLocator = !!window.frames['__tcfapiLocator'];
  } catch (e) {}
  return out;
})()
`

// tcfApplyScript drives a TCF v2.2 CMP through its own API.
//
// The API is preferred over clicking because it does not depend on button
// labels, translations, or markup, and because it reports back what the CMP
// actually recorded — which is the evidence a compliance reviewer needs.
//
// %s is replaced with "true" to consent or "false" to reject.
const tcfApplyScript = `
(async () => {
  const grant = %s;

  const call = (command, version, param) => new Promise((resolve) => {
    let settled = false;
    const done = (data, success) => {
      if (settled) return;
      settled = true;
      resolve({ data: data, success: success !== false });
    };
    try {
      window.__tcfapi(command, version, done, param);
    } catch (e) {
      done(null, false);
    }
    setTimeout(() => done(null, false), 5000);
  });

  // Wait for the CMP to finish loading; acting earlier is ignored silently,
  // which would look like success while changing nothing.
  const waitReady = async () => {
    for (let i = 0; i < 50; i++) {
      const r = await call('getTCData', 2);
      const status = r.data && r.data.cmpStatus;
      if (status === 'loaded' || status === 'error') return r.data;
      await new Promise(res => setTimeout(res, 100));
    }
    return null;
  };

  const before = await waitReady();
  if (!before) return { ok: false, reason: 'TCF CMP did not report cmpStatus loaded' };

  // There is no standard command to set consent, so the vendor UI is driven
  // where a documented entry point exists, and the caller falls back to
  // selectors otherwise.
  let acted = false;

  try {
    if (grant) {
      if (window.Didomi && window.Didomi.setUserAgreeToAll) { window.Didomi.setUserAgreeToAll(); acted = true; }
      else if (window.UC_UI && window.UC_UI.acceptAllConsents) { await window.UC_UI.acceptAllConsents(); acted = true; }
      else if (window.OneTrust && window.OneTrust.AllowAll) { window.OneTrust.AllowAll(); acted = true; }
    } else {
      if (window.Didomi && window.Didomi.setUserDisagreeToAll) { window.Didomi.setUserDisagreeToAll(); acted = true; }
      else if (window.UC_UI && window.UC_UI.denyAllConsents) { await window.UC_UI.denyAllConsents(); acted = true; }
      else if (window.OneTrust && window.OneTrust.RejectAll) { window.OneTrust.RejectAll(); acted = true; }
    }
  } catch (e) {
    return { ok: false, reason: 'vendor API threw: ' + String(e) };
  }

  if (!acted) return { ok: false, reason: 'no documented vendor entry point available' };

  // Read back what the CMP recorded, and wait for it to report that a user
  // action completed.
  for (let i = 0; i < 50; i++) {
    const r = await call('getTCData', 2);
    const d = r.data;
    if (d && (d.eventStatus === 'useractioncomplete' || d.gdprApplies === false)) {
      return {
        ok: true,
        tcString: d.tcString || '',
        eventStatus: d.eventStatus || '',
        cmpId: d.cmpId || null,
        cmpVersion: d.cmpVersion || null,
        purposeConsents: d.purpose && d.purpose.consents ? Object.keys(d.purpose.consents).filter(k => d.purpose.consents[k]).length : 0
      };
    }
    await new Promise(res => setTimeout(res, 100));
  }

  const last = await call('getTCData', 2);

  return {
    ok: false,
    reason: 'CMP never reported useractioncomplete',
    tcString: (last.data && last.data.tcString) || '',
    eventStatus: (last.data && last.data.eventStatus) || ''
  };
})()
`

// gppReadScript reads the Global Privacy Platform string, recorded verbatim
// as evidence when present.
const gppReadScript = `
(async () => {
  if (typeof window.__gpp !== 'function') return { ok: false };
  return await new Promise((resolve) => {
    let settled = false;
    const done = (v) => { if (!settled) { settled = true; resolve(v); } };
    try {
      window.__gpp('ping', (data) => {
        if (!data) { done({ ok: false }); return; }
        done({ ok: true, gppString: data.gppString || '', sectionList: data.applicableSections || [] });
      });
    } catch (e) { done({ ok: false }); }
    setTimeout(() => done({ ok: false }), 3000);
  });
})()
`
