package main

// The fixture's content, kept in one file so that what a scan sees is
// reviewable in one place.
//
// Every string here is static. A timestamp, a counter, or a cache buster
// would make two scans of an unchanged fixture differ, and a fixture that
// cannot produce an empty diff cannot prove that a non-empty one means
// something (Story 7.1, AC4).

// indexHTML is the scanned page.
//
// The three third-party interactions are deliberate and distinct:
//
//   - pixel.gif loads unconditionally, so it is contacted before any consent
//     decision, in every consent mode. It is the headline finding wsaw
//     exists to report.
//   - analytics.js is a Klaro-managed service: type="text/plain" with
//     data-type is how Klaro holds a script back until a decision, so it is
//     absent under reject and present under accept.
//   - extra.gif appears only in the changed variant, as a new third-party
//     host for the diff to find.
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>wsaw end-to-end fixture</title>
<link rel="stylesheet" href="/assets/site.css">
<script src="/assets/klaro-config.js"></script>
<script defer data-config="klaroConfig" src="/assets/klaro.js"></script>
</head>
<body>
<main>
  <h1>wsaw end-to-end fixture</h1>
  <p>Variant: <span id="variant">{{.Variant}}</span></p>

  <p>
    <!-- Unconditional: contacted whatever the consent decision is. -->
    <img src="{{.ThirdPartyBase}}/pixel.gif" alt="" width="1" height="1">
  </p>

  <!-- Consent-gated by Klaro: released only when "analytics" is allowed. -->
  <script type="text/plain" data-type="application/javascript" data-name="analytics"
          src="{{.ThirdPartyBase}}/analytics.js"></script>

  {{if .ExtraBase}}
  <p>
    <!-- Only in the changed variant: a second third-party host. -->
    <img src="{{.ExtraBase}}/extra.gif" alt="" width="1" height="1">
  </p>
  {{end}}
</main>
<script src="/assets/app.js"></script>
</body>
</html>
`

// siteCSS is a first-party stylesheet, present so the asset inventory has
// something first-party in it besides scripts.
const siteCSS = `body { font-family: system-ui, sans-serif; margin: 2rem; }
h1 { font-size: 1.4rem; }
#variant { font-family: ui-monospace, monospace; }
`

// klaroConfigJS configures Klaro.
//
// acceptAll and a visible decline button are what give the notice the two
// buttons wsaw's shipped rule clicks (.cm-btn-success / .cm-btn-accept-all
// and .cm-btn-decline / .cn-decline). Without hideDeclineAll: false there is
// nothing to reject with, and the reject mode of the whole product would be
// untested against a real CMP.
//
// storageMethod stays the default cookie, because a consent decision that
// survives in a cookie is exactly what per-scan isolation has to prevent
// leaking between scans (Tenet 2).
const klaroConfigJS = `var klaroConfig = {
  version: 1,
  elementID: 'klaro',
  storageMethod: 'cookie',
  cookieName: 'klaro',
  cookieExpiresAfterDays: 30,
  privacyPolicy: '/privacy',
  default: false,
  mustConsent: false,
  acceptAll: true,
  hideDeclineAll: false,
  hideLearnMore: false,
  noticeAsModal: false,
  lang: 'en',
  translations: {
    en: {
      consentNotice: {
        description: 'This fixture would like to load {purposes}.',
      },
      purposes: {
        analytics: 'analytics',
      },
    },
  },
  services: [
    {
      name: 'analytics',
      title: 'Fixture Analytics',
      purposes: ['analytics'],
      required: false,
      optOut: false,
      onlyOnce: true,
    },
  ],
};
`

// analyticsJS is the consent-gated third-party script. It makes one further
// request, to a fixed URL, so the capture has an initiator chain to record
// without introducing anything that varies between scans.
//
// The URL is absolute, with the third party's own base substituted in at
// startup. A relative one would resolve against the *page's* origin, so the
// request wsaw recorded would be first-party — which is the opposite of what
// this script exists to demonstrate.
const analyticsJS = `(function () {
  window.__fixtureAnalytics = 'loaded';

  var img = new Image();
  img.src = '{{selfBase}}/collect?e=pageview';
})();
`

// appScriptBase and appScriptChanged are the same first-party script in two
// versions. The only difference a scan can see is the body, and therefore its
// digest — which is what makes "a third-party script changed silently"
// testable end to end (AC5).
const appScriptBase = `(function () {
  window.__fixtureApp = { variant: 'base' };
})();
`

const appScriptChanged = `(function () {
  window.__fixtureApp = { variant: 'changed', extra: true };
})();
`
