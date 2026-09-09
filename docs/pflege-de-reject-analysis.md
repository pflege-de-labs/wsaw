# pflege.de, reject mode: what changed and what rules follow from it

Window: 2026-09-02 11:50 — 2026-09-09 15:25. 84 reject-mode scans of
`https://www.pflege.de/`, one per ~75 min. Source: `results` rows for
`(pflege.de, reject)` plus the `body/` artifacts they reference.
Cross-checked against the other four targets in `wsaw.yaml`.

Everything below is derived from stored results only; no new scans were run.

## 1. Host inventory over the window

| present | host | party |
|--------:|------|-------|
| 84/84 | www.pflege.de, mailing-assets.pflege.de | first |
| 84/84 | js.sentry-cdn.com, widget.trustpilot.com, www.googletagmanager.com | third |
| 81/84 | cdn.pflege.de | first |
| 80/84 | cdn.consentmanager.net, c.delivery.consentmanager.net | third |
| **7/84** | **bzr.openai.com, bzrcdn.openai.com** | third |
| **3/84** | **www.google.com, www.google.de, googleads.g.doubleclick.net, www.googleadservices.com** | third |
| **2/84** | **ad.doubleclick.net** | third |

The two bold rows are the whole story. The rest is presence flapping
(section 4).

## 2. Finding A — OpenAI conversion pixel, added 2026-09-08, fires before consent

`GTM-PKKTX6B` gained an OpenAI pixel between the scans at 2026-09-08 13:13
and 14:28. The container body proves it: `oaiq` occurrences in
`gtm.js?id=GTM-PKKTX6B` go 0 → 4 at that boundary.

Two identical custom-HTML tags (`tag_id` 1027 and 1032) inject:

```js
!function(a,b,d,e){if(!a.oaiq){...}}(window,document,"script",
  "https://bzrcdn.openai.com/sdk/oaiq.min.js");
oaiq("init",{pixelId:"NCuKoZ3wbS2WqQtLVS421U",debug:!0});
```

A third tag (`tag_id` 1033) was added between 2026-09-09 12:41 and 13:57:

```js
oaiq("measure","lead_created",{ type:"customer_action" });
```

Observed behaviour in reject mode:

- `bzrcdn.openai.com/sdk/oaiq.min.js` (82 799 B, sha
  `c25abbd7f5a09dda…`, stable across all 7 observations)
- `bzrcdn.openai.com/pixel-config/v1/NCuKoZ3wbS2WqQtLVS421U.json`
- `POST bzr.openai.com/v1/sdk/events?pid=NCuKoZ3wbS2WqQtLVS421U&st=oaiq-web&sv=0.1.41&t=…&ec=2` → 202
- first-party cookie `__obref`, 36 bytes, expiry +1 year
- third-party cookies `__cf_bm` and `_cfuvid` on `.bzr.openai.com` and
  `.bzrcdn.openai.com`

Phase is `pre-interaction` in 6 of 7 scans — the pixel loads and posts its
first event **before the reject click happens at all**. It is not consent-gated.

Portfolio-wide, reject mode, scans after 2026-09-08 12:00:

| target | fired | `__obref` set |
|--------|------:|--------------:|
| pflege.de | 7/10 | 7/10 |
| service.pflege.de | 7/10 | 7/10 |
| curabox.de | 7/10 | 7/10 |
| PGR (meine.pflege.de) | 7/11 | 7/11 |
| Wizco (curabox.de/pflege/beantragen) | 0/11 | 0/11 |

The 3–4 misses per target are the scans where consent handling failed or the
scan ended early — not scans where the pixel declined to fire. Treat the true
rate as ~100 % of successful reject scans on four of five properties.

Also worth flagging to whoever owns the container: `debug:!0` is enabled in
production, and the init tag is duplicated.

## 3. Finding B — Google Ads fires under reject, but only when the CMP fails to boot

Three scans (#15 2026-09-04 16:05, #28 2026-09-05 10:57, #73 2026-09-08 11:52)
loaded five `gtag/js?id=…` tags and then sent conversion traffic to Google,
in reject mode, `post-interaction`:

```
t+7.5s  GET  googletagmanager.com/gtag/js?id={AW-967278100,AW-825411646,AW-971332686,DC-5240955,DC-5388750}&cx=c&gtm=4e6921
t+8.3s  POST www.google.com/ccm/collect?…&tid=AW-…&en=page_view&dt=<page title>&dl=<page url>&auid=…&npa=1&gcd=13l3l3l2l1l1
t+8.3s  GET  www.googleadservices.com/pagead/conversion/825411646/?…
t+8.4s  POST googleads.g.doubleclick.net/pagead/viewthroughconversion/825411646/?… → 302
t+8.6s  GET  www.google.com/pagead/1p-conversion/825411646/?… → 302 → www.google.de/pagead/1p-conversion/…
t+—     POST ad.doubleclick.net/ccm/s/collect?auid=…   (scans #28, #73)
```

`npa=1` is set, so personalisation is suppressed, and no Google cookie is
written — these are consent-mode "cookieless pings". They still carry page
URL, page title, referrer and a per-visit `auid` to Google after a rejection.

**The discriminator is the CMP, not the container.** Comparing the
consentmanager request bodies between the three leaking scans and the 69
non-leaking `applied` scans gives a clean split, 3/3 vs 0/69, on five
independent signals:

| signal | 69 healthy scans | 3 leaking scans |
|---|---|---|
| `consent.php?id=` | `4770` | `0` |
| `consent.php?p=` | `5` | `3` |
| `consent.php?cvc=` | `_s1052_s23_s2612_s905_s977_c266…` | `__U_` |
| `consent.php?cpc=` | `_51_` | `__` |
| TCF string `c=` | `CQq…AAfMCBENCvFg` | `CQq…AAfAAAENAsFg` |
| `info/?id/did/cfdid` | `4770/1/1`, `4770/1/15659` | `0/0/0` |
| `cdn.consentmanager.net` recall shield + logo | present | absent |
| cookies | `__cmpconsentx4770`, `__cmpcpcx4770`, `__cmpcvcx4770` | none of them; `_gcl_au` instead |

`id=0`, `did=0`, `cfdid=0` and an empty vendor list mean the CMP script ran
but never loaded its account configuration. wsaw still drove the vendor API,
still got a TCF string back, and still recorded
`outcome: applied — rule "consentmanager" applied and verified`. But an
unconfigured CMP has no Google Consent Mode bridge to push
`gtag('consent','update',{ad_storage:'denied', …})`, so GTM kept its own
default state and released the ad tags.

Same pattern, same counts, on `service.pflege.de` (1 accept-mode occurrence)
and `pflege.de` accept mode (1) — it is a CMP-availability failure, not
something specific to reject mode.

Corroboration: `_gcl_au` (Google Ads first-party linker, 90-day, **not**
`Secure`) is written in exactly those 3 scans and in none of the other 81.

Checked against the whole store — 852 scans, 281 `none` / 287 `reject` /
284 `accept`, across all five targets:

| mode | scans | ads fired | `consent.php?id=0` | `_gcl_au` set |
|------|------:|----------:|-------------------:|--------------:|
| none | 281 | 0 | 0 | 0 |
| reject | 287 | 3 | 3 | 3 |
| accept | 284 | 258 | 2 | 258 |

In the 568 `none` and `reject` scans the two events co-occur three times and
never separately: no scan sent ad traffic without `id=0`, and no scan with
`id=0` failed to send it. `_gcl_au` tracks ads-fired exactly in all 852,
accept mode included — which is what makes it usable as the alerting signal
even where the CMP endpoint is not visible.

So: **`consent.php?id=0` predicts a Google Ads leak under reject with no
false positives and no false negatives in the whole store.** In accept mode
it predicts nothing, because there the ad tags are supposed to fire.

Two consequences:

1. For the site: a visitor whose consentmanager config request fails gets ad
   tags regardless of what they click. Worth a `waitForConsentBridge`-style
   guard, or blocking GTM until the CMP reports a configured state.
2. For wsaw: "applied and verified" is too generous. Verification confirmed a
   TCF string exists; it did not confirm the CMP was in a state where its
   consent signal could reach anything. See section 6.

## 4. Noise taxonomy

954 add/remove events across the 83 consecutive diffs, from 336 distinct
comparison keys. Where they come from:

| churn events | source | cause |
|-------------:|--------|-------|
| 278 | `c.delivery.consentmanager.net/delivery/info` | `?o=<epoch ms>` cache buster — 141 distinct keys, ~2 new per scan |
| 162 | `…/delivery/cmp.php` | same `o=`, plus `__cmpcc`, `__cmpfcc`, `dlt`, `odw` |
| 63 | `widget.trustpilot.com/stats/TrustboxImpression` | lazy widget, 0/1/2 of the two trustboxes captured |
| 38 | `…/delivery/consent.php` | `c=` TCF string, `cvc`/`cpc` vendor lists — per-visit by design |
| 63 | `widget.trustpilot.com/trustbox-data/…`, `…/trustboxes/…/main.js` | same lazy widget |
| 48 | `cdn.consentmanager.net/delivery/{recall/recall_shield2.svg,img/logo…gif}` | present/absent with CMP outcome |
| 30 | `www.googletagmanager.com/gtag/js` | only the 3 leak scans |
| 30 | `www.google.com/ccm/collect` | `tfd`, `tft`, `tag_exp`, `rcb`, `auid`, `rnd` |

Two distinct problems, needing two distinct fixes:

- **Volatile query parameters** — a normalization gap. Fixable with rules.
- **Presence flapping** — a capture-window problem. The Trustpilot trustboxes
  load lazily; median scan duration is 4.76 s for scans that captured both and
  4.91 s for scans that captured one or none, so duration is not the variable —
  the widgets simply are or are not done when `idleQuiet: 2s` expires. No
  normalization rule can fix that; only a longer settle budget or a
  `deferredHosts`-style wait can.

The same window also exposes a capture-coverage gap: the ad tags fire at
t+7.5 s, but 69 of 84 scans ended at t≈4.7 s. wsaw only saw the leak in scans
whose consent interaction happened to be slow. Whatever the cause of a leak,
a 4.7 s window under-observes a page whose tags fire at 7.5 s.

## 5. Rules deduced

### 5a. Normalization (measured effect: 954 → 580 churn events, −39 %; 336 → 104 distinct keys)

```yaml
normalize:
  dropQueryParams:
    - session_id
    # consentmanager cache busters and per-call state
    - o             # epoch-ms buster on /delivery/{info,cmp.php} — 440 of 954 churn events
    - __cmpcc
    - __cmpfcc
    - dlt
    - odw
    # Google Ads / consent-mode per-request noise
    - rcb
    - tfd
    - tft
    - tag_exp
    - auid
    - fst
    - rnd
    - cerd
    - crd
    - pscrd
    - fsk
    - cid
    - apvc
    - gcd
    # Trustpilot widget styling
    - styleHeight
```

Deliberately **not** dropped, because they are identity or a real signal:

- `id`, `tid`, `tids` — GTM container and Google Ads account IDs. Dropping
  them collapses five distinct ad accounts into one key.
- `did`, `cfdid`, `sv`, `dv`, `lv` — consentmanager config/version numbers.
  These are exactly the fields that expose Finding B; a change here is real.
- `gtm` — GTM container fingerprint. Noisy, but a change in it is a real
  container publish. Keep it and let `flapWindow` handle the oscillation.
- `c`, `cvc`, `cpc` — the consent record itself. Per-visit, so it churns, but
  dropping it discards the primary evidence for both findings. Better handled
  by making `/delivery/consent.php` a watched endpoint than by normalizing it
  away.

Dropping the last group too would only take churn from 580 to 508 (−47 %
total) — not worth the blindness.

### 5b. Detection rules

```yaml
detection:
  denyHosts:
    - bzr.openai.com
    - bzrcdn.openai.com
    - googleads.g.doubleclick.net
    - ad.doubleclick.net
    - googleadservices.com
    - google-analytics.com
    - analytics.google.com
  severity:
    firstPartyAssetAdded: info
    firstPartyCookieAdded: high   # see the gap in section 6
```

`denyHosts` gives `SeverityCritical` on every appearance rather than only on
the first, which is what these hosts warrant: they are a finding whenever they
show up in `reject`, and — because the observation window under-samples them —
"new" is the wrong test.

`www.google.com` and `www.google.de` are deliberately **not** on the deny
list: they are also plain page assets elsewhere. Match those on path
(`/ccm/collect`, `/pagead/1p-conversion/`) instead, which today means an
assertion in the CI gate rather than a `denyHosts` entry.

### 5c. Capture budget

```yaml
defaults:
  idleQuiet: 5s      # was 2s; ad tags fire at t+7.5s, scans ended at t+4.7s
  hardTimeout: 60s   # was 45s; keeps headroom for the longer settle
```

This is the single highest-value change in the file. With `idleQuiet: 2s` the
Google Ads leak was visible in 3 of 84 scans; the evidence says the trigger
condition — CMP bootstrap failure — is what is rare, but a scan that ends at
4.7 s cannot distinguish "did not happen" from "not yet". It should also
stabilise the Trustpilot flapping that accounts for ~126 of the 954 churn
events.

## 6. wsaw rule gaps this window exposed

1. **No `firstPartyCookieRejectMode` severity.** `internal/diff/rules.go`
   has `ThirdPartyCookieRejectMode: critical` but only
   `FirstPartyCookieAdded: low`. Both tracking cookies found here — `_gcl_au`
   (Google Ads) and `__obref` (OpenAI) — are first-party, so both land at
   `low`. A first-party cookie written by a third-party tag under reject is
   the same finding as a third-party one; it just uses the first-party
   loophole. Suggest a `FirstPartyCookieRejectMode` rule defaulting to `high`,
   or classifying by setter rather than by domain.

2. **Consent verification accepts an unconfigured CMP.** The consentmanager
   rule reported `applied and verified` while `consent.php?id=0` said the CMP
   had no configuration. Verification should assert that the CMP is
   configured, not merely that it answers. For consentmanager specifically,
   `id != 0` in `/delivery/consent.php` (or the presence of
   `__cmpconsentx<accountId>`) is a cheap, exact post-condition — and when it
   fails, the right outcome is `flag`, not `applied`.

3. **`flapWindow` cannot express lazy-load flapping.** The Trustpilot
   trustboxes appear 59–73 times out of 84 with no reverse-within-window
   pattern to collapse; they are simply sometimes unfinished. A
   `settleHosts`/`awaitHosts` concept — "do not call the page idle until these
   hosts have gone quiet" — would fix the class, and would also have caught
   Finding B on the first day.

4. **The empty host.** 82 of 84 scans contain requests with `host: ""`
   (`data:`/`blob:`), grouped as first-party. Harmless, but they inflate host
   counts in the UI.

## 7. Timeline

| when | what |
|------|------|
| 2026-09-02 11:50 | window opens. 5 third-party hosts steady state |
| 2026-09-03 12:04 | `cdn.consentmanager.net/delivery/js/cmp_en.min.js` 488 004 → 489 232 B (the only vendor-script content change in the window) |
| 2026-09-04 16:05 | **first CMP bootstrap failure → Google Ads under reject** |
| 2026-09-05 10:57 | second occurrence |
| 2026-09-07 09:47 | GTM container restructured: `ogt_` tag templates 1117 → 0 refs, ad tags now delivered on demand via `gtag/js`. No behaviour change observed |
| 2026-09-08 11:52 | third CMP bootstrap failure → Google Ads under reject |
| 2026-09-08 14:28 | **OpenAI pixel added to GTM-PKKTX6B**, fires pre-consent |
| 2026-09-08 15:43 | consentmanager customdata token `xt_94` → `xt_97` (config version bump); content later changed 54 109 → 54 184 B |
| 2026-09-09 13:57 | OpenAI `lead_created` conversion tag added (`tag_id` 1033) |
| 2026-09-09 15:25 | window closes |

Note on `gtm.js` content: 26 distinct sha256 over 84 observations, and they
oscillate — `0c3141c7` appears at scans 57–64, 67, 69, 70 with `18e5cc60` and
`21d52746` interleaved. Google serves per-request container variants, so
`thirdPartyScriptChanged` on `gtm.js` is ~38 alerts of which 3 are real
publishes. A semantic extract (the set of vendor endpoints and tag IDs the
container references) is a far better comparison key for this one asset than
its hash. Grepping the stored bodies for vendor markers is how both findings
above were dated, and it is worth automating.
