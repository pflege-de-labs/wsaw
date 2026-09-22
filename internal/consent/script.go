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

  // __wsawSimulateClick is __wsawClick's more thorough sibling, reserved for
  // the escalation path a rule reaches when its own click already ran but the
  // banner is still on screen (Story 2.7, AC3).
  //
  // Element.click() only ever synthesizes a "click" event. Real user input
  // fires a whole sequence first — pointerover, pointerdown, mousedown,
  // pointerup, mouseup — and a control bound to one of those, rather than to
  // "click" itself, never reacts to a plain .click(). That is a real and
  // fairly common way for a banner's own dismiss handler to stay silent while
  // the click step reports success.
  //
  // Every event dispatched here is exactly as untrusted as .click() already
  // is — isTrusted is false either way — so this widens which handlers can
  // fire without pretending to be a real user gesture.
  window.__wsawSimulateClick = (selector) => {
    const el = window.__wsawQuery(selector);
    if (!el) return { clicked: false, reason: 'no element matched ' + selector };

    try { el.scrollIntoView({ block: 'center', inline: 'center' }); } catch (e) { /* best effort */ }

    const r = el.getBoundingClientRect();
    const point = { clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 };
    const shared = { bubbles: true, cancelable: true, composed: true, view: window, ...point };

    try {
      el.dispatchEvent(new PointerEvent('pointerover', { ...shared, pointerId: 1, isPrimary: true }));
      el.dispatchEvent(new PointerEvent('pointerdown', { ...shared, pointerId: 1, isPrimary: true, button: 0 }));
      el.dispatchEvent(new MouseEvent('mousedown', { ...shared, button: 0 }));
      el.dispatchEvent(new PointerEvent('pointerup', { ...shared, pointerId: 1, isPrimary: true, button: 0 }));
      el.dispatchEvent(new MouseEvent('mouseup', { ...shared, button: 0 }));
      // .click() still runs last: it is what fires "click" handlers and runs
      // a native control's default action (link navigation, form submit),
      // neither of which the pointer/mouse sequence above triggers on its own.
      el.click();
    } catch (e) {
      return { clicked: false, reason: String(e) };
    }

    return { clicked: true, synthesized: true };
  };

  // __wsawLocate finds a click target's on-screen centre without clicking
  // it, so the caller can dispatch a genuine CDP pointer event there instead
  // of a synthetic one. A CDP-dispatched click is trusted the way
  // el.click() and dispatchEvent() are not — Event.isTrusted is true only
  // for input that goes through Chrome's real input pipeline — and at least
  // one CMP (CCM19) checks isTrusted and silently ignores an untrusted
  // click, so a synthetic click can appear to succeed while nothing is
  // actually recorded.
  window.__wsawLocate = (selector) => {
    const el = window.__wsawQuery(selector);
    if (!el) return { found: false, reason: 'no element matched ' + selector };
    if (visible(el)) {
      try { el.scrollIntoView({ block: 'center', inline: 'center' }); } catch (e) { /* best effort */ }
    }
    const r = el.getBoundingClientRect();
    return { found: true, visible: visible(el), x: r.left + r.width / 2, y: r.top + r.height / 2 };
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
  //
  // Candidates are scored rather than taken in document order. The first
  // element that merely mentions cookies is usually the page's own footer —
  // it carries a "Datenschutz" link, it is visible, and it is banner-shaped
  // enough to pass a keyword-and-a-link test. Returning it makes the
  // heuristic click nothing and, worse, makes "is the banner gone?" answer
  // "no" forever, because the footer never goes anywhere (Story 2.9).
  const CONSENT_WORDS = /(cookie|consent|datenschutz|privacy|einwilligung|zustimmung|tracking)/i;

  const ownText = (el) => {
    let out = '';
    for (const n of el.childNodes) {
      if (n.nodeType === 3) out += n.nodeValue;
    }
    return out;
  };

  const scoreCandidate = (n) => {
    if (!visible(n)) return null;

    const r = n.getBoundingClientRect();
    if (r.height < 30 || r.width < 150) return null;

    // A panel parked off-canvas via transform or a negative offset (a common
    // closed state for animated cookie panels) still has display, opacity and
    // a nonzero rect, so require it to actually intersect the viewport.
    const vw = window.innerWidth || document.documentElement.clientWidth;
    const vh = window.innerHeight || document.documentElement.clientHeight;
    if (r.right <= 0 || r.bottom <= 0 || r.left >= vw || r.top >= vh) return null;

    const text = n.textContent || '';
    // A whole-page wrapper matches the words too, so require the element to
    // be banner-shaped rather than the entire document.
    if (text.length > 3000) return null;
    if (!CONSENT_WORDS.test(text)) return null;

    // It must actually offer a choice, otherwise a privacy-policy paragraph
    // would count as a banner.
    const buttons = n.querySelectorAll('button,[role="button"],input[type="button"],input[type="submit"]');
    const links = n.querySelectorAll('a[href]');
    if (buttons.length === 0 && links.length === 0) return null;

    let score = 0;

    // A real control beats a link: a footer offers a privacy-policy link, a
    // banner offers something to press. Links still count, because a CMP
    // skin that renders its controls as anchors is common enough that
    // excluding them would miss real banners — Cookiebot's category skin is
    // built that way.
    if (buttons.length > 0) score += 3;
    else score += 1;

    const role = (n.getAttribute('role') || '').toLowerCase();
    if (role === 'dialog' || role === 'alertdialog' || n.hasAttribute('aria-modal')) score += 3;

    // A banner is laid over the page; page furniture scrolls with it.
    const position = getComputedStyle(n).position;
    if (position === 'fixed' || position === 'sticky') score += 2;

    if (CONSENT_WORDS.test(ownText(n) || '')) score += 1;
    if (text.length <= 600) score += 1;

    // Page furniture that happens to mention cookies is not a banner, and a
    // dense list of links is what furniture looks like.
    if (n.closest('footer,header,nav')) score -= 3;
    if (links.length >= 8) score -= 2;

    return score;
  };

  // A candidate needs more than a keyword and something clickable. The
  // threshold is set so that a footer carrying a privacy link cannot reach
  // it, while an ordinary banner — a fixed container with a button — clears
  // it comfortably.
  const CONSENT_SCORE_MIN = 4;

  const consentCandidates = () => {
    const out = [];

    for (const root of roots()) {
      let nodes;
      try {
        nodes = root.querySelectorAll('div,section,aside,dialog,form,footer');
      } catch (e) { continue; }

      for (const n of nodes) {
        const score = scoreCandidate(n);
        if (score === null || score < CONSENT_SCORE_MIN) continue;
        out.push({ el: n, score: score });
      }
    }

    // Highest score wins; ties go to the smaller element, which is the one
    // closer to the controls rather than a wrapper around them.
    out.sort((a, b) => b.score - a.score ||
      (a.el.textContent || '').length - (b.el.textContent || '').length);

    return out;
  };

  window.__wsawConsentContainer = () => {
    const found = consentCandidates();
    return found.length > 0 ? found[0].el : null;
  };

  // __wsawConsentSummary describes the banner without acting on it, so a
  // detection that could not be driven still leaves a reader something to act
  // on: which element, what it said, and which controls it offered (Story
  // 2.9, AC2 and AC4). Everything here is page-controlled text; it is
  // truncated, and the caller treats it as data.
  window.__wsawConsentSummary = () => {
    const el = window.__wsawConsentContainer();
    if (!el) return { found: false };

    const clip = (s, max) => {
      const t = (s || '').replace(/\s+/g, ' ').trim();
      return t.length > max ? t.slice(0, max) + '…' : t;
    };

    let name = el.tagName.toLowerCase();
    if (el.id) name += '#' + el.id;
    for (const attr of ['data-testid', 'data-cy', 'aria-label', 'role']) {
      const v = el.getAttribute(attr);
      if (v) { name += '[' + attr + '="' + clip(v, 40) + '"]'; break; }
    }

    const heading = el.querySelector('h1,h2,h3,h4,strong,p');

    const controls = [];
    const seen = new Set();
    for (const c of el.querySelectorAll('button,a[href],[role="button"],input[type="button"],input[type="submit"]')) {
      const label = clip(c.innerText || c.textContent || c.value || c.getAttribute('aria-label'), 60);
      if (!label || seen.has(label)) continue;
      seen.add(label);
      controls.push(label);
      if (controls.length >= 12) break;
    }

    return {
      found: true,
      element: clip(name, 120),
      heading: clip(heading ? (heading.innerText || heading.textContent) : '', 160),
      text: clip(el.innerText || el.textContent, 400),
      controls: controls,
    };
  };

  // __wsawStorageSnapshot lists Web Storage keys and value lengths for the
  // frame it runs in. It exists so the consent engine can see that a choice
  // was written somewhere at all: a site that keeps consent in localStorage
  // leaves no cookie behind, and "no cookie" would otherwise read as "nothing
  // was recorded" (Story 2.9, AC5).
  window.__wsawStorageSnapshot = () => {
    const read = (area, label) => {
      const out = {};
      try {
        for (let i = 0; i < area.length && i < 200; i++) {
          const k = area.key(i);
          if (k === null) continue;
          const v = area.getItem(k);
          out[label + ':' + k] = v === null ? 0 : v.length;
        }
      } catch (e) { /* storage can be blocked; an empty view is the answer */ }
      return out;
    };

    return Object.assign({}, read(window.localStorage, 'local'), read(window.sessionStorage, 'session'));
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
