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

  // Readiness helpers for Consentmanager's __cmp API.
  //
  // __cmp is installed as a stub before the CMP initialises, and a call made
  // against the stub is queued rather than applied. Expressing a choice too
  // early therefore returns without error while changing nothing — the exact
  // shape of failure wsaw must never report as success.
  //
  // Readiness is taken from the documented "settings" event, which fires when
  // the CMP has finished loading its settings and consent data becomes
  // readable. That event may already have fired before wsaw attaches, so a
  // direct consentStatus read is polled alongside it.
  //
  // See https://www.consentmanager.net/en/help/developer-reference/cmp-events/
  const cmpStatus = () => {
    try {
      const s = window.__cmp('consentStatus');
      if (s && typeof s === 'object' && 'consentExists' in s) return s;
    } catch (e) { /* the stub may throw or return nothing */ }
    return null;
  };

  window.__wsawCmpReady = async (timeoutMs) => {
    if (typeof window.__cmp !== 'function') return false;

    const deadline = Date.now() + (timeoutMs || 8000);

    let settled = cmpStatus() !== null;

    try {
      window.__cmp('addEventListener', ['settings', () => { settled = true; }, false], null);
    } catch (e) { /* polling below is the fallback */ }

    while (Date.now() < deadline) {
      if (settled || cmpStatus() !== null) {
        // Snapshot the state before wsaw touches anything. Verification
        // compares against this, because "a choice is on record" is only
        // evidence of *our* choice if none was on record beforehand.
        const s = cmpStatus();
        window.__wsawCmpPrior = s ? {exists: !!s.consentExists, data: s.consentData || ''} : {exists: false, data: ''};
        return true;
      }
      await new Promise(r => setTimeout(r, 100));
    }

    return false;
  };

  // __wsawCmpRecorded waits for evidence that *this* interaction was recorded.
  //
  // Waiting only for consentExists was wrong: a page carrying consent from an
  // earlier scan satisfies it immediately, so the rule reported "applied and
  // verified" while having changed nothing. Real evidence is a transition
  // from no-consent to consent, or a change in the consent data.
  window.__wsawCmpRecorded = async (timeoutMs) => {
    const deadline = Date.now() + (timeoutMs || 5000);
    const prior = window.__wsawCmpPrior || {exists: false, data: ''};

    while (Date.now() < deadline) {
      const s = cmpStatus();
      if (s && s.consentExists && (!prior.exists || (s.consentData || '') !== prior.data)) {
        return true;
      }
      await new Promise(r => setTimeout(r, 100));
    }

    return false;
  };

  // __wsawCmpChanged reports the same evidence synchronously, for a rule's
  // verify expression.
  window.__wsawCmpChanged = () => {
    const s = cmpStatus();
    if (!s || !s.consentExists) return false;
    const prior = window.__wsawCmpPrior;
    if (!prior) return false;
    return !prior.exists || (s.consentData || '') !== prior.data;
  };

  // Consent-container detection for the heuristic fallback.
  //
  // Visibility is decided from computed style and geometry, never from
  // offsetParent: that property is null for position:fixed elements, which is
  // exactly how most cookie banners are positioned. Using it would have made
  // the fallback blind to the common case.
  window.__wsawConsentContainer = () => {
    const words = /(cookie|consent|datenschutz|privacy|einwilligung|zustimmung|tracking)/i;

    for (const root of roots()) {
      let nodes;
      try {
        nodes = root.querySelectorAll('div,section,aside,dialog,form,footer');
      } catch (e) { continue; }

      for (const n of nodes) {
        if (!visible(n)) continue;

        const r = n.getBoundingClientRect();
        if (r.height < 30 || r.width < 150) continue;

        const text = n.textContent || '';
        // A whole-page wrapper matches the words too, so require the element
        // to be banner-shaped rather than the entire document.
        if (text.length > 3000) continue;
        if (!words.test(text)) continue;

        // It must actually offer a choice, otherwise a privacy-policy
        // paragraph would count as a banner.
        const buttons = n.querySelectorAll('button,a[href],[role="button"],input[type="button"],input[type="submit"]');
        if (buttons.length === 0) continue;

        return n;
      }
    }

    return null;
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
      // Consentmanager: setConsent(1) is its documented "accept all". It is
      // tried after the others because its __cmp global is also installed by
      // unrelated stubs, and only reached once the CMP reports readiness.
      else if (typeof window.__cmp === 'function' && await window.__wsawCmpReady()) {
        window.__cmp('setConsent', 1); acted = true;
      }
    } else {
      if (window.Didomi && window.Didomi.setUserDisagreeToAll) { window.Didomi.setUserDisagreeToAll(); acted = true; }
      else if (window.UC_UI && window.UC_UI.denyAllConsents) { await window.UC_UI.denyAllConsents(); acted = true; }
      else if (window.OneTrust && window.OneTrust.RejectAll) { window.OneTrust.RejectAll(); acted = true; }
      else if (typeof window.__cmp === 'function' && await window.__wsawCmpReady()) {
        window.__cmp('setConsent', 0); acted = true;
      }
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
