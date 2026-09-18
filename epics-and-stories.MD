# wsaw — Epics & Stories

**wsaw** (website asset watcher) is a daemon for Linux and macOS, written in Go. It loads a configured list of URLs in headless Chrome, records every subsequent network fetch the page triggers (documents, scripts, stylesheets, images, fonts, XHR/fetch, beacons, media, websockets), optionally accepts or rejects the site's cookie banner beforehand, and reports the resulting asset inventory and its changes over time.

Primary use cases:

1. **Privacy / consent compliance** — prove which third parties are contacted before consent, after "reject all", and after "accept all".
2. **Third-party supply-chain watch** — detect newly appearing hosts, scripts, or changed script content on production sites.
3. **Asset inventory & drift** — track what a site actually loads, versus what is expected.

Chrome is interchangibly used for Chrome / Chromium based browser in this document

Roles used:

- **Operator** — runs the daemon (SRE/platform).
- **Compliance Officer** — needs evidence about third parties and consent.
- **Site Owner** — owns the scanned website, wants to know what changed.
- **Developer** — integrates wsaw with other systems or extends it.

Companion documents: `non-functional-requirements.MD`, `architecture-tenets.MD`.

**Implementation status.** A story heading with no note is built and covered by tests. A heading marked *not implemented* has no code behind it; one marked *partly implemented* names the acceptance criteria that are still open. The notes were written by auditing the tree at commit `8bc0ce3` (8 September 2026) and go stale the moment the gap is closed — close the gap, drop the note.

---

## Table of Contents

- [Epic 1 — Capture Engine](#epic-1--capture-engine)
- [Epic 2 — Consent & Cookie Banner Handling](#epic-2--consent--cookie-banner-handling)
- [Epic 3 — Targets, Configuration & Daemon Operation](#epic-3--targets-configuration--daemon-operation)
- [Epic 4 — Storage, Baselines & Change Detection](#epic-4--storage-baselines--change-detection)
- [Epic 5 — Reporting, Export & Alerting](#epic-5--reporting-export--alerting)
- [Epic 6 — Operations, Packaging & Distribution](#epic-6--operations-packaging--distribution)
- [Epic 7 — Containerised End-to-End Testing](#epic-7--containerised-end-to-end-testing)
- [Epic 8 — Result Documents in Object Storage](#epic-8--result-documents-in-object-storage)
- [Technical Decisions](#technical-decisions-proposed-for-review)
- [Open Questions](#open-questions-for-the-product-owner)

---

# Epic 1 — Capture Engine

Load a URL in headless Chrome and produce a complete, deterministic record of every network fetch it causes.

**Goal:** a single `Scan(target) → ScanResult` operation that is reliable, isolated, bounded in time, and produces stable output suitable for diffing.

**Scope:** browser lifecycle, CDP session handling, network event capture, page lifecycle/idle detection, resource classification, first-party/third-party attribution, initiator chains, per-scan browser profile isolation.

**Out of scope:** consent handling (Epic 2), scheduling (Epic 3), persistence (Epic 4).

### Story 1.1 — Launch and control headless Chrome (partly implemented: no test asserts that Chrome processes return to baseline, AC5)

*As an Operator, I want wsaw to drive a headless Chrome instance so that pages are rendered exactly as a real browser renders them.*

- AC1: wsaw connects to Chrome via the Chrome DevTools Protocol (CDP), either by launching a local binary or by attaching to a remote CDP endpoint (`--chrome-url`).
- AC2: Chrome binary is auto-discovered on `PATH` and at well-known locations for Linux and macOS; `--chrome-path` overrides.
- AC3: Missing/incompatible Chrome fails at startup with an actionable error naming the minimum supported major version.
- AC4: A crashed or hung browser is detected and replaced without terminating the daemon.
- AC5: No Chrome processes are leaked after shutdown (verified by a test that scans N targets and asserts process count returns to baseline).

### Story 1.2 — Record all network fetches

*As a Site Owner, I want a complete list of every request the page makes so that I know what my site really loads.*

- AC1: Every request is captured, including main document, subresources, JS-initiated `fetch`/XHR, `sendBeacon`, dynamically injected `<script>`/`<img>`, preloads, service-worker and worker requests, EventSource, and WebSocket upgrades.
- AC2: Per request record: absolute URL, method, resource type, initiator (type + script URL + line where available), HTTP status, MIME type, transfer size, decoded body size, timing (start, TTFB, end), redirect chain, protocol, whether it was served from cache, and the remote IP where reported by CDP.
- AC3: Redirects are recorded as their own hops, not collapsed.
- AC4: Failed and blocked requests are recorded with their error reason (`net::ERR_*`, blocked-by-client, blocked-by-CSP, mixed content).
- AC5: Data URIs and `blob:` URLs are recorded but flagged as non-network.

### Story 1.3 — Deterministic page-idle detection

*As an Operator, I want scans to end at a well-defined point so that results are comparable between runs.*

- AC1: A scan ends on the first of: network-idle (0 in-flight requests for a configurable quiet period, default 2s), a configurable hard timeout (default 45s), or a configurable maximum request count.
- AC2: The termination reason is recorded in the result (`idle`, `timeout`, `request-cap`, `error`).
- AC3: Optional post-load dwell time and optional scroll-to-bottom step (to trigger lazy-loaded assets), both configurable per target.
- AC4: Repeated scans of a static fixture site produce byte-identical normalized results.

### Story 1.4 — First-party / third-party attribution

*As a Compliance Officer, I want each asset classified as first- or third-party so that I can focus on external data flows.*

- AC1: Classification uses the public suffix list (eTLD+1) against the target's origin, not naive string matching.
- AC2: Configurable additional first-party domains per target (CDNs the site owner controls).
- AC3: Each record carries the registrable domain, the full host, and the classification.
- AC4: Cookies set during the scan are recorded with name, domain, path, expiry, `SameSite`, `Secure`, `HttpOnly`, and first/third-party classification.

### Story 1.5 — Clean per-scan isolation

*As a Compliance Officer, I want every scan to start from a clean browser state so that a previous scan's consent cannot leak into the next one.*

- AC1: Each scan runs in a fresh, ephemeral user-data-dir or an incognito browser context; storage, cookies, cache, and service workers do not persist between scans.
- AC2: Isolation is verifiable: scanning a consent-setting site twice in `none` mode yields the banner both times.
- AC3: Optional opt-in warm-cache mode for performance testing, off by default and flagged in the result.

### Story 1.6 — Capture script content fingerprints

*As a Site Owner, I want a hash of every loaded script so that I notice when a third-party script silently changes.*

- AC1: For configurable resource types (default: `script`), the response body is hashed (SHA-256) and the digest stored.
- AC2: Bodies are hashed in a streaming fashion; raw bodies are not persisted unless `--store-bodies` is set, and then only up to a configurable size cap.
- AC3: Body capture failures (e.g. evicted from the CDP body cache) are recorded as such and never silently omitted.

### Story 1.7 — Per-target browser context tuning

*As a Site Owner, I want to scan as a specific device/locale so that results reflect what my users see.*

- AC1: Configurable per target: viewport size and device scale factor, mobile emulation, user agent, `Accept-Language`, timezone, geolocation, extra HTTP headers, HTTP basic auth, and an outbound proxy.
- AC2: The user agent defaults to the one shipped by the launched Chrome build; overrides are explicit and recorded.
- AC3: All applied overrides are echoed into the scan result for reproducibility.

### Story 1.8 — Run Chrome in a container (partly implemented: Chrome's own sandbox is disabled inside the container and no seccomp profile is shipped, AC5)

*As an Operator, I want wsaw to run headless Chrome inside a container when a container runtime is available, so that hostile pages are rendered behind a boundary the operating system enforces rather than one the browser enforces on itself.*

Podman is preferred over Docker because it is daemonless and rootless by default, so wsaw needs no privileged socket and a container escape lands as an unprivileged user rather than as root.

This is distinct from Story 6.3, which packages *wsaw itself* with Chromium bundled. Here wsaw runs on the host and the browser runs in a container.

- AC1: A container runtime is auto-detected at startup, preferring `podman` and falling back to `docker`. When one is found, running Chrome in a container is the **default**; when neither is, wsaw uses a local Chrome binary exactly as it does today, saying which it chose and why.
- AC2: The choice is overridable in both directions and by name (`--browser-runtime auto|podman|docker|local`), so an operator can pin a runtime or opt out entirely. `auto` is the documented default rather than hidden behaviour.
- AC3: **Isolation is not weakened.** Each browser still serves one scan by default, so a container is per browser and disposed with it. A scan must never inherit another scan's cookie jar, which is the failure this project has already been bitten by.
- AC4: No container outlives the process that started it. Containers are labelled as wsaw's, removed on normal exit, on cancellation, and on panic, and any orphan left by an earlier crash is reaped at startup. A leaked container is worse than a leaked process: it survives the restart that would have cleaned up after wsaw.
- AC5: The Chrome sandbox stays enabled inside the container. Where the runtime needs a seccomp profile or specific capabilities for that, the profile is shipped and documented; `--no-sandbox` is not the way the container is made to work.
- AC6: The image is pinned by digest, configurable, and never pulled implicitly during a scan. A missing image fails at startup with the command to fetch it, so a scan never blocks on a network fetch of a browser (Tenet 14).
- AC7: The runtime, image reference and resolved Chrome version are recorded in every result, so a result says what rendered it. A scan run in a container is comparable with one run outside only if the reader can see which it was.
- AC8: Container startup is bounded and measured. A runtime that hangs fails the scan with a recorded reason rather than the daemon, and the added startup cost per scan is documented so an operator can weigh it against the isolation gained.
- AC9: The container is given only what it needs: no host filesystem mounts beyond what a scan requires, memory and process limits, and an explicit note in the documentation of what a compromised renderer would and would not reach.
- AC10: Reaching a host-local target still works, or fails with an explanation. A containerised browser cannot resolve the host's `localhost`, which affects local fixtures and any operator scanning a service on the machine itself.
- AC11: The browser-driven test suite runs against both the local and the containerised path, and skips with a clear reason where no runtime is present, so the container path is covered rather than assumed.

### Story 1.9 — Treat a `data:` URI's payload as a body, not as a URL

*As a Compliance Officer opening a scan, I want an embedded `data:image/...;base64,...` asset shown the same bounded way every other asset is, so that one inlined icon does not turn the page I am reading into the thing I am trying to read.*

Story 1.2, AC5 already says `data:` and `blob:` URLs are "recorded but flagged as non-network," and that flag is where the story stops today: `isNonNetwork` (`internal/capture/recorder.go`) marks the request `NonNetwork` and the hashing path explicitly skips it (`!rec.req.NonNetwork`, Story 1.6), so a `data:` request is the one kind of asset that never gets a `BodySHA256` or a `BodyRef`. Its entire payload — for a base64-encoded image, routinely tens or hundreds of kilobytes of text — sits in the `URL` field instead, and every consumer of that field treats it as a URL: the scan detail page renders it verbatim in `<code>{{.URL}}</code>` (`internal/httpapi/templates/result.html`), a Markdown report inlines it in a table cell (Story 5.3), and a JSON or CSV export repeats it in full (Story 5.1, Story 5.9 AC7). None of those surfaces did anything wrong; they were built on the assumption every other request record satisfies, that a URL is short. A `data:` URI breaks that assumption while still type-checking as a string, which is exactly the kind of gap that is invisible until a page full of inlined icons is the one somebody has to scroll past to find the finding they opened the scan for.

The fix is not a display hack on one page. It is recognising that a `data:` URI's payload is a response body that happens to be spelled inside the request line instead of arriving over the wire — the same thing Story 1.6 already hashes and, optionally, stores for every fetched resource — so it should be captured that way once, at the source, and every consumer already knows how to show a body: as a digest and a bounded download link, never inlined (the `body-link` pattern `internal/httpapi/templates/result.html` already applies to every other stored body, per Story 5.11 AC5).

- AC1: At capture time, a `data:` request's decoded payload is hashed into `BodySHA256` exactly as Story 1.6 hashes a fetched body, regardless of resource type — a `data:` URI carries its payload in the request itself, so there is no separate fetch to gate the hash on the configured `--hash-types` list. The digest is always computed; it costs nothing extra to store.
- AC2: When body storage is enabled (`--store-bodies`, Story 1.6 AC2), the decoded bytes are stored as a content-addressed artifact and `BodyRef` is set, identically to a fetched body. When it is not, `BodySHA256` alone is still recorded — evidence is never lost outright, only the raw bytes are, on the same opt-in Story 1.6 already established.
- AC3: The stored `URL` field is replaced with a bounded form: the `data:` scheme and MIME type kept verbatim (so `image/svg+xml` is still visibly different from `image/png`), the payload replaced by its length in bytes and a note that it was truncated. This happens once, in the result document itself — not as a per-view rendering trick — so a raw JSON export, a CSV, a Markdown report and the UI all agree on what the request's URL says, and none of them independently re-discovers that this one field can be enormous (Tenet 16: one machine output, every human view derived from it).
- AC4: Truncation is never silent. The truncated form names itself as truncated and states the original length, the same way a corrupted or unreadable record is reported rather than dropped (Tenet 5) — a reader comparing two scans must be able to tell a shrunk field from a field that was always short.
- AC5: The scan detail page's asset table renders a `data:` request exactly like any other asset with a stored body: the truncated URL in the URL column, the digest, and — where `BodyRef` is set — the existing body-link download, opaque and non-renderable per Story 5.11 AC5. No template change is needed beyond what already renders `BodyRef`; the fix is that a `data:` request now has one.
- AC6: Diffing is unaffected. `NormalizedURL` and the diff engine (Story 4.2, Story 4.3) key off `BodySHA256` where one is present for comparing `data:` payloads across scans, not off the truncated `URL` text, so two scans embedding the same icon by different accidental formatting still compare as unchanged, and a changed embedded image is still a finding.
- AC7: HAR export is unaffected: Story 1.2 AC5 and the existing HAR writer already exclude `NonNetwork` requests (`internal/report/har.go`), and this story does not add them back in truncated or full form.
- AC8: Regression coverage captures a fixture page embedding a `data:image/png;base64,...` asset above a size threshold and asserts: the stored result's `URL` is truncated with the correct original length, `BodySHA256` matches the decoded payload, `BodyRef` is set when body storage is on and absent when it is off, and the scan detail page renders the truncated form with a working body-link rather than the raw payload.

---

# Epic 2 — Consent & Cookie Banner Handling

Interact with the site's consent management platform (CMP) so the same URL can be scanned in different consent states.

**Goal:** three reproducible consent modes per target — `none` (no interaction), `reject`, `accept` — so the difference between them becomes the compliance finding.

**Scope:** CMP detection, IAB TCF v2.2 API driving, vendor-specific adapters (Usercentrics, OneTrust, Cookiebot, Didomi, Sourcepoint, Consentmanager), generic selector-based rules, user-supplied rules, a site's own bespoke banner where no CMP product is involved, verification that the interaction actually worked.

**Out of scope:** legal interpretation of results, TCF consent string decoding beyond what is needed to verify the click.

### Story 2.1 — Configurable consent mode per target

*As a Compliance Officer, I want to scan the same URL in different consent states so that I can compare them.*

- AC1: Consent mode is `none`, `reject`, or `accept`, set globally and overridable per target.
- AC2: A target may declare multiple modes; each mode produces its own independent scan result.
- AC3: The consent mode and its outcome are part of the result identity — results are never compared across modes.

### Story 2.2 — Detect the consent management platform

*As an Operator, I want wsaw to recognize the CMP in use so that it can interact with it correctly.*

- AC1: Detection covers the IAB TCF v2.2 `__tcfapi`/`__tcfapiLocator` surface and the Google Additional Consent / `__gpp` API where present.
- AC2: Vendor detection for at least: Usercentrics, OneTrust, Cookiebot, Didomi, Sourcepoint, Consentmanager, Complianz, Borlabs.
- AC3: The detected CMP (vendor, version where readable, detection method) is recorded in the result.
- AC4: "No CMP detected" is an explicit, first-class outcome, not an error.

### Story 2.3 — Drive TCF-compliant CMPs via API

*As a Compliance Officer, I want consent granted or denied through the official CMP API so that the interaction is robust and not dependent on button labels.*

- AC1: Where a TCF v2.2 API is present, wsaw uses it to express full consent or full rejection for all purposes and vendors.
- AC2: The resulting TC string (and GPP string where present) is recorded verbatim in the result.
- AC3: Where the CMP exposes a documented programmatic accept/reject entry point (e.g. Usercentrics, OneTrust), the vendor adapter uses it in preference to clicking.

### Story 2.4 — Click-based banner interaction with rules

*As an Operator, I want selector-based fallback rules so that non-TCF banners can still be handled.*

- AC1: A rule set maps host patterns and/or DOM signatures to CSS/XPath selectors plus an action for `accept` and `reject`.
- AC2: Rules search the main frame and all same-origin and cross-origin iframes, plus open shadow roots.
- AC3: A built-in rule set ships with the binary and can be extended or overridden by user rule files without recompiling.
- AC4: Rules support waiting for the selector to appear (bounded), scrolling it into view, clicking, and an optional follow-up step (e.g. "open settings → deny all → save").
- AC5: A generic heuristic fallback (multilingual label matching, DE/EN at minimum) is attempted last and clearly flagged as heuristic in the result.

### Story 2.5 — Verify the consent interaction actually happened

*As a Compliance Officer, I want to know whether the banner was really handled so that I do not draw conclusions from a failed click.*

- AC1: After interaction, wsaw verifies the outcome (banner element gone, TCF `eventStatus` reached `useractioncomplete`, consent cookie present).
- AC2: The result carries a consent outcome: `applied`, `not-needed`, `failed`, or `unverified`, with a reason string.
- AC3: A failed or unverified consent interaction can be configured to fail the scan, or to yield a flagged result (default: flagged, not failed).
- AC4: Screenshots before and after the interaction can be enabled per target for evidence.

### Story 2.6 — Distinguish pre-consent from post-consent traffic

*As a Compliance Officer, I want to see which requests fired before any consent action so that I can identify unlawful pre-consent tracking.*

- AC1: Every request record carries a consent phase marker: `pre-interaction` or `post-interaction`.
- AC2: The report summarizes third-party hosts contacted in the `pre-interaction` phase separately.
- AC3: In `reject` mode, third-party hosts contacted after rejection are highlighted as the primary finding.

### Story 2.7 — Fall back to the click when the API does not close the banner

*As a Compliance Officer, I want wsaw to click the banner's own control when the vendor API leaves the banner standing so that the evidence I show a regulator matches what a visitor would have seen.*

Tenet 11 ranks the API above the click, and that ranking is right for expressing the choice: `submitCustomConsent(false, false, false)` denies every category without depending on a button label. It is not right for concluding the interaction is over. A vendor API writes the CMP's consent state; the banner's disappearance is usually wired to the button's own handler, and calling the API around the button leaves that handler unrun. The result is a page that has rejected everything and still shows the dialog.

`https://www.hartmann.info/` is the case that surfaced this. Its Cookiebot skin offers category checkboxes, "Allow selected cookies" and "Allow all cookies" — and no decline control at all, so the `cookiebot` rule's `click` step for `#CybotCookiebotDialogBodyButtonDecline` matches nothing and, being `optional`, is skipped without comment. The `eval` step succeeds, `window.Cookiebot.hasResponse` turns true, `verify` passes, and the scan records `applied — rule "cookiebot" applied and verified`. The before and after screenshots are the same image byte for byte, and the report says so — the one place the truth appears is a caption the consent outcome never reads.

The rejection itself was real: no non-essential third party was contacted afterwards. That is what makes the gap subtle rather than loud, and why it is worth a story. wsaw claims a state it has only half reached, which is the failure Tenet 5 exists to prevent, and it does so on the two words a reviewer actually reads.

The fix is to stop letting one expression stand for two questions. "Did the CMP record my choice?" and "is the banner gone?" are separate facts with separate evidence, and only the second one licenses the tool to stop interacting. When the choice is recorded and the banner persists, the click steps that were skipped as optional become the next thing to try, not the thing to skip — the ranking in Tenet 11 is about which mechanism to *try first*, never about which one is allowed to finish the job.

- AC1: A rule can express two verifications: that the CMP recorded the choice (`verify`, as today) and that the banner is no longer displayed (a new expression, defaulting to the shared visible-container check that `window.__wsawConsentContainer` already provides for the heuristic rules). A rule that names neither can still only ever be `unverified` (Story 2.5).
- AC2: When the choice is recorded but the banner is still displayed, the engine escalates: the rule's remaining steps run even where they are marked `optional`, and where a rule offers no selector the heuristic label match for the current mode is attempted (Story 2.4, AC5). Escalation is bounded to one pass with the existing step timeout — a banner that survives it is reported, not retried in a loop (Tenet 6).
- AC3: The escalation click reaches controls that `HTMLElement.click()` alone does not. The element is scrolled into view and receives the pointer and mouse event sequence a real click produces before `click()` is dispatched, because a CMP whose handler is bound to `pointerdown` is exactly the case this story exists to serve. The synthesized events are recorded as synthesized; wsaw does not claim a trusted event.
- AC4: The recorded mechanism names what actually closed the banner, not what was tried first. An interaction that began at the vendor API and finished with a click is distinguishable in the result from one the API completed alone, and a click that only ran because the API left the banner up is distinguishable from a rule that was selector-based from the start (Tenet 11).
- AC5: A banner that is still displayed after the escalation yields an honest outcome. The recorded choice is not thrown away — the categories really were denied and the post-interaction traffic really is evidence — but the result states that the banner persisted, and the outcome is not a bare `applied`.
- AC6: The escalation does not move the pre/post-consent phase boundary. It is marked once, before the first step (Story 2.6, AC1); traffic a fallback click triggers is post-consent traffic, and no request already recorded is relabelled.
- AC7: The screenshot pair and the consent outcome agree. Where before and after are the same image byte for byte and the outcome claims the banner was handled, that contradiction is surfaced in the result rather than only in the report's caption — the signal already exists in the UI layer and belongs where every consumer of the document can see it (Tenet 16).
- AC8: The `cookiebot` rule closes the banner on `https://www.hartmann.info/` in `reject` mode, or reports that it could not. Regression coverage uses a stored fixture of that banner — a Cookiebot skin with no decline control — so the case survives the site being redesigned.
- AC9: Rules that the API already closes cleanly do not gain a click. The escalation is driven by the banner-gone check, never by the mode or the vendor, so a CMP whose API does dismiss its own dialog performs exactly the interactions it performs today.

### Story 2.8 — Detect a CMP with no reject control, and settle for necessary-only consent

*As a Compliance Officer, I want wsaw to recognize when a consent banner offers no way to reject at all, and record that as its own finding, so that a missing reject control is documented as a fact about the site rather than silently smoothed over as a successful rejection.*

Story 2.7 is about a banner that stays open after a real reject was recorded. This story is about the case one step earlier: there is no reject to record, because the banner itself only offers "Accept all" plus, at best, a settings panel that lets individual categories be toggled — never a single "reject all" or "decline" control. That shape is common enough to be a pattern rather than an outlier, and it is exactly the case `reject` mode (Story 2.1) is silent about today: with no selector to match and no fallback, the rule's `Reject` steps find nothing, and the scan ends `failed` or `unverified` having tried nothing more useful. A heuristic label match (Story 2.4, AC5) is no safer — the nearest big, clickable control on such a banner is "Accept all," and a heuristic that grabbed it would record a `reject`-mode scan whose traffic is actually full-consent traffic. That is the exact failure Tenet 5 exists to prevent, on the two words a reviewer reads, and it is worse than the honest `failed` wsaw produces today.

The reachable state on a banner like this is not "rejected" — it does not offer that — but "only strictly necessary categories active," reached by opening the settings panel and leaving every optional category deselected (or confirming it is already so) before saving. That is a real, defensible fallback, and it is also not a reject test. Conflating the two would make a site that structurally cannot reject look, in the data, identical to a site that was asked to reject and complied — which erases the very finding a Compliance Officer needs from this case.

- AC1: A rule can declare a necessary-only fallback sequence, distinct from `Accept` and `Reject`: the steps that open a settings panel, ensure every optional category is deselected, and save. It is data (Tenet 10), not a special case in the engine, exactly like every other step sequence in a rule.
- AC2: In `reject` mode, the fallback runs only where the rule defines no `Reject` steps, or where every defined `Reject` step is optional and none matched — never as a preferred path ahead of a real reject sequence (Tenet 11's ranking: real reject beats a fallback, and a fallback beats nothing). The generic heuristic label match (Story 2.4, AC5) is restricted to deny-shaped labels for this purpose; it is never allowed to fall through to "Accept all" for lack of a better match.
- AC3: The recorded consent outcome for this path is its own value — `necessary-only` — joining `applied`, `not-needed`, `failed`, `unverified`, and `banner-visible` (Story 2.5, AC2, Story 2.7, AC5). It is never reported as `applied`: a reader comparing `reject`-mode results across sites must be able to tell "this site truly has no non-essential traffic when rejected" from "this site has no reject control, and wsaw reached the closest state it could."
- AC4: The result names the mechanism that reached that state (category deselection plus save, or a vendor API scoped to purposes rather than an all-or-nothing switch) and, where the CMP's own labels distinguish them, which categories were left active. Absence of a reject control is recorded as a fact about the CMP, not inferred from the shape of the traffic that follows (Tenet 5).
- AC5: Verification follows Story 2.5 unchanged: the fallback state is verified the same way a reject is (choice recorded, banner dismissed), and a fallback that cannot be verified is `unverified`, never silently promoted to `necessary-only`.
- AC6: **The outcome is surfaced everywhere a `reject`-mode result is read, not only in the raw JSON.** The Markdown/plain-text report (Story 5.3), the result and diff browser (Story 5.9), and the side-by-side consent-mode comparison (Story 5.9, AC6) all state plainly that this banner has no reject control and that wsaw limited consent to strictly necessary categories instead — so a `reject` column that means "verified no optional traffic" and one that means "no reject control existed" are never presented as equivalent.
- AC7: A `necessary-only` outcome is itself a change-event finding about the site (Story 4.5), with its own configurable severity, independent of whatever third-party hosts the scan went on to observe — a CMP with no reject control is worth flagging on its own, the same way a failed or unverified consent interaction already is.
- AC8: Regression coverage uses a stored fixture of a banner that offers only "Accept all" and a settings panel with no way to save a fully-deselected state except by deselecting it manually, so the fallback is tested against a banner shape rather than assumed from a vendor's documentation.
- AC9: A rule with a working `Reject` sequence is unaffected. The fallback exists only for the banners that have none, and adding it never changes the recorded outcome for a CMP that already rejects cleanly.

### Story 2.9 — Handle a site's own consent banner, where there is no CMP product at all

*As a Compliance Officer, I want a site that wrote its own cookie banner to be scanned as carefully as one that bought a CMP, so that "no CMP detected" never becomes a quiet way of saying "wsaw did not try".*

Epic 2 is written around products: TCF, a vendor API, a vendor's markup, and a heuristic for the long tail. A large class of sites has none of those. They render a banner from their own application code, store the answer in Web Storage under a name they invented, and gate their own analytics call on it. There is no vendor fingerprint to detect, no API to prefer over the click, and no stable selector to write a rule against, because the class names are generated by the build.

`https://mein-pflegegrad-rechner.de/` is the case that surfaced this, and a `reject`-mode scan of it on 2026-09-18 produced a result that is wrong in every layer at once. The consent block reads `failed`, reason `rules matched but none completed: generic-label-match`, and carries no `cmp`, no `detection` and no `mechanism` — so the one artifact a reviewer reads records that something was tried and nothing about what was found. The banner itself is ordinary: a fixed container, the words "Wir verwenden Cookies", and two buttons labelled "Ablehnen" and "Akzeptieren", both of which are already in the heuristic's German label list. It is rendered by the page's own React code after hydration, and the choice is written to `localStorage["cookie-accepted"]`, never to a cookie. wsaw recorded `"cookies": null` for the scan — true, and completely misleading, because the site's consent state and its analytics identifier both live in a storage wsaw does not capture (no `localStorage` or `sessionStorage` capture exists anywhere in the codebase today).

The traffic tells the same story from the other end. The scan recorded 24 requests, **zero third-party domains**, and every single request marked `pre-interaction` — which is what the report will say, and which reads as an unusually clean site. Five of those requests go to `hog.mein-pflegegrad-rechner.de`, which is a PostHog ingest endpoint reverse-proxied onto the site's own domain, and the page's code initializes PostHog on `requestIdleCallback` regardless of the banner, switching only its persistence (`memory` before consent, `localStorage+cookie` after "Akzeptieren"). So the site does send analytics before any consent is given; it merely declines to persist an identifier while doing so. `internal/classify/classify.go` correctly calls that host first-party by registrable domain, and the finding disappears.

None of this is the heuristic being unlucky. It is the shape of a site that wsaw currently has no vocabulary for: a consent UI with no product behind it, consent state outside cookies, and a tracker that is first-party by DNS. This story gives that shape a name, an honest outcome, and evidence. It does not give it a vendor adapter, because there is no vendor.

The first-party-by-DNS tracker endpoint is the one part deliberately left out here: recognizing that `hog.example.com` is a PostHog collector, `analytics.example.com` a server-side GTM container, or `stats.example.com` a self-hosted Matomo, is an attribution and classification change (Story 1.4, Story 4.5), not a consent one, and it needs its own story. This story must not paper over it — see AC7.

- AC1: Detection tells three states apart, not two. A page with a vendor CMP, a page with a **bespoke banner** — a consent UI is present, no vendor fingerprint matches — and a page with no consent UI at all are three distinct recorded outcomes. `not-needed` because no CMP was present (`internal/model/result.go`) is only ever recorded for the third. A page that visibly asks a visitor for consent is never reported as a page that has no CMP.
- AC2: A bespoke banner is recorded with an identity a reader can act on. The result names the detection method, the banner's own visible heading or first line of text, its storage key where one is observed, and the fact that no vendor was identified — rather than today's empty `cmp` field or `ruleCMPName`'s bare `unknown (heuristic match)` (`internal/consent/handler.go`). "Which banner was this?" must be answerable from the stored document alone (Tenet 16).
- AC3: The banner is waited for before its absence is concluded. A banner rendered after hydration or on an idle callback is the normal case for an application-rendered banner, not an edge case: detection polls for a consent container up to a bounded deadline (Tenet 1) before any rule is declared not to apply, and the deadline is configurable per target for slow pages.
- AC4: A detection that matches nothing clickable is diagnosable without a rerun. Where `window.__wsawConsentContainer` (`internal/consent/script.go`) matches an element but no label in the mode's list matches inside it, the result records what was matched — the element's tag, its stable attributes, its first line of text, and the labels of the controls it does contain — so that "rules matched but none completed" stops being the end of the trail. Page text goes into the result as data, never interpolated anywhere (NFR §4), and is bounded in length.
- AC5: Consent evidence is not cookie-only. The scan captures Web Storage (`localStorage` and `sessionStorage`) per origin: key names always, values recorded under the same treatment cookie values already receive, since both may be personal data (NFR §4). Story 2.5 AC1's verification gains storage as a first-class form of evidence — `cookie-accepted: "false"` is exactly as good a proof that a rejection was recorded as a consent cookie is — and the diff engine sees storage keys appearing and disappearing the way it sees cookies (Story 4.3).
- AC6: A bespoke banner is handled by data, not by Go (Tenet 10). A rule for one of these sites is host-scoped and binds to what survives a redeploy: visible text, `role`, `aria-*`, element structure, an author-written `id` or `data-` attribute. It must not bind to build-generated class names or positional paths — for a Next.js or Vite site those change on every build, and a rule that silently stops matching is worse than no rule. A host-scoped rule that no longer matches its host is reported as a stale rule, not skipped in silence.
- AC7: Analytics that runs before the interaction is a finding even when it is first-party. The phase marker (Story 2.6 AC1) already records these requests as `pre-interaction`; the report and the result must state plainly that first-party requests were made before any consent choice, with their hosts, rather than summarizing only third-party hosts and leaving a reader to conclude that "0 third-party domains" means nothing happened. Recognizing that such a host is a *proxied third-party collector* is out of scope here and belongs to Story 1.4 / Story 4.5; this AC is satisfied by not hiding the traffic, not by classifying it.
- AC8: A `reject` on a bespoke banner is verified like any other (Story 2.5): the choice is recorded — now including a storage write — and the banner is gone (Story 2.7 AC1). Neither one alone promotes the outcome to `applied`, and a click on a control whose label was guessed stays flagged as heuristic (Story 2.4 AC5, Tenet 11). Where the site offers only "Akzeptieren" and no reject-shaped control, Story 2.8's `necessary-only` path applies unchanged.
- AC9: Fixtures cover the shape, not the site (Tenet 13, NFR §5): a fixture page that renders its banner from script after load, uses build-hashed class names, writes its answer to `localStorage` rather than a cookie, and fires a beacon to a same-origin path both before and after the choice. A stored copy of the `mein-pflegegrad-rechner.de` banner markup is kept as a regression fixture so the case survives the site being rebuilt. No test touches the live site.
- AC10: Regression coverage: the three-way detection of AC1, including that a hydration-delayed banner is not reported as "no CMP"; the recorded identity of AC2; the bounded wait of AC3 and that it does not extend a scan of a page with no banner; the diagnostic record of AC4 for a container that matches with no usable control; storage capture, storage-based verification, and storage keys in a diff; a host rule that stops matching being reported stale; that pre-interaction first-party requests appear in the report's own summary; and that every existing vendor rule's recorded outcome is byte-for-byte unchanged by this story.

---

# Epic 3 — Targets, Configuration & Daemon Operation

Run as a long-lived service that scans a list of targets on a schedule.

**Goal:** operators define targets and schedules declaratively, start the daemon, and it keeps producing results without further interaction.

**Scope:** target list definition, config loading and validation, hot reload, scheduler, retry of failed scans, worker pool and concurrency limits, browser pool, per-target overrides (viewport, user agent, device emulation, headers, basic auth, geolocation/timezone, proxy), one-shot CLI mode, graceful shutdown.

**Out of scope:** distributed/multi-node scheduling (explicit non-goal for v1).

### Story 3.1 — Declarative target list

*As an Operator, I want to define targets in a config file so that the watch list is versionable.*

- AC1: Targets are defined in YAML: URL, optional name/labels, consent modes, schedule, and any per-target overrides from Story 1.7.
- AC2: Global defaults exist for every per-target setting; targets override selectively.
- AC3: Targets can also be supplied as a plain newline-delimited URL file, and via CLI arguments for one-shot runs.
- AC4: Config is validated on load with precise, line-referencing error messages; the daemon refuses to start on invalid config.

### Story 3.2 — Run as a daemon on a schedule

*As an Operator, I want continuous unattended operation so that changes are detected without manual runs.*

- AC1: Schedules are expressed as an interval and/or a cron expression, globally and per target.
- AC2: Scans are jittered to avoid thundering herds against a single origin.
- AC3: A per-origin rate limit and a minimum interval between scans of the same target are enforced.
- AC4: `--once` runs the full target list a single time and exits with the CI exit-code contract (Story 5.5).
- AC5: Missed schedules during downtime are not backfilled by default; catch-up is opt-in.
- AC6: **A restart continues each target's schedule rather than starting it over.** The scheduler's own record of when a target last ran is in memory, so it is seeded from the store at startup: a daily target scanned an hour before a restart is due in twenty-three hours, not immediately. Without this, AC3's minimum interval has nothing to measure from and AC5 is vacuous — every target looks as though it had never been scanned, and a daemon that restarts on each deploy scans everything on each deploy. Only a genuinely overdue target runs at startup, and then jittered.

### Story 3.3 — Concurrency and browser pooling (partly implemented: no memory-threshold recycling, AC3)

*As an Operator, I want bounded resource use so that the daemon is a good citizen on shared hosts.*

- AC1: `--concurrency` bounds parallel scans; default derived from `NumCPU` and available memory.
- AC2: A browser pool reuses Chrome processes across scans while preserving per-scan context isolation (Story 1.5).
- AC3: Pool members are recycled after a configurable number of scans or on memory threshold breach.
- AC4: Backpressure: when the pool is saturated, scans queue rather than spawning unbounded browsers.

### Story 3.4 — Config hot reload (partly implemented: SIGHUP only, no file-watch, AC1)

*As an Operator, I want to change the target list without a restart so that operations stay uninterrupted.*

- AC1: `SIGHUP` and file-watch both trigger reload.
- AC2: Invalid new config is rejected and the previous config stays active, with a logged error.
- AC3: In-flight scans complete under their original config.

### Story 3.5 — Graceful shutdown

*As an Operator, I want clean termination so that no partial results or orphan processes remain.*

- AC1: `SIGTERM`/`SIGINT` stop scheduling new scans, allow in-flight scans a configurable grace period, then cancel them.
- AC2: All browser processes and temp profile directories are cleaned up, including after a cancelled scan.
- AC3: Partially written result files are never left in the output directory (atomic write + rename).

### Story 3.6 — Secrets handling

*As an Operator, I want credentials kept out of config files and logs so that the deployment is secure.*

- AC1: Basic-auth credentials, webhook URLs with tokens, and proxy credentials can be supplied via env vars or file references (`${env:NAME}`, `${file:/path}`).
- AC2: Secrets are redacted in logs, in API responses, and in the config-echo of scan results.

### Story 3.7 — Per-site crawl politeness policy

*As an Operator, I want politeness settings decided per site so that I can scan my own properties aggressively and third-party sites conservatively.*

- AC1: `robots.txt` handling is configurable per target (`ignore`, `respect`), with a documented global default.
- AC2: When set to `respect`, a disallowed target URL is skipped with a recorded, non-error outcome rather than a silent no-op.
- AC3: Fetched `robots.txt` results are cached per origin for a configurable TTL; a fetch failure is a recorded outcome with a documented fallback behavior.
- AC4: Per-origin rate limit and schedule jitter are configurable per target on top of the global defaults.

### Story 3.8 — Retry a failed scan

*As an Operator, I want a scan that failed for a transient reason retried promptly, so that a dropped connection or a crashed browser does not leave a hole in a target's history until the next scheduled run.*

Today a failed scan is recorded honestly and then waits for its interval — up to a day with the shipped default. The record is right; the gap is avoidable.

The distinction that makes this story tractable is between a failure and a finding. A page that loads nothing, refuses consent, or returns 500 is a *result*: wsaw observed it, and observing it again would only confirm it. A browser that died before navigating, a DNS lookup that failed, a container that would not start — those are *missing observations*, and the whole point of a watcher is not to miss.

- AC1: Retries are configurable: how many attempts, and the backoff between them, globally and per target. Zero disables retrying entirely, and the default is stated in the documentation rather than left to be discovered.
- AC2: Only a failed scan is retried, and only where another attempt could plausibly change the outcome — a browser or container that failed to start, a browser that crashed or hung, a navigation that failed at the network level. A scan skipped by robots policy is a decision, not a failure, and is never retried. A failed or unverified consent interaction produced a result and is not retried either: the scan saw what it saw.
- AC3: A truncated scan — timeout, request cap, byte cap — is retried only when configured to be, off by default. Retrying every slow site doubles the load wsaw puts on it and usually reproduces the same truncation.
- AC4: Backoff has jitter, and a retry obeys the same per-origin concurrency limit as any other scan (Tenet 17). A network blip that fails fifty targets at once must not turn their retries into a load test of fifty sites. The minimum interval, on the other hand, does **not** hold a retry back: that floor bounds how often wsaw asks a site for a *new* observation, a retry is the same observation, and with the shipped default of an hour a retry held by the floor would not be a retry at all. What bounds the extra load is that retries are counted — at most attempts-1 of them — and jittered.
- AC5: **Every attempt is recorded as its own result, with its own scan ID.** A failure that a later attempt replaced still happened and must remain visible (Tenet 5); a retry is not a way to make a bad scan disappear.
- AC6: **A superseded failure does not become the comparison baseline.** "The scan before this one" currently means the previous row whatever its outcome, so without this a successful retry would be diffed against the failure it replaced and the whole site would be reported as new. The retry is worthless if it manufactures a false finding.
- AC7: Notification is on the final outcome, not on each attempt. A failure that a retry fixed does not page anyone at three in the morning. A target that needed retries stays visible as a metric, because a site that only works on the third attempt is a finding of its own (Tenet 8).
- AC8: A pending retry does not hold a worker or delay other targets. It is requeued, not slept through, so one flapping target cannot stall the schedule.
- AC9: Attempts and backoff together cannot outlast the target's own interval, and a configuration where they could is refused at load with the reason. A retry schedule longer than the scan schedule means overlapping runs of the same target.
- AC10: The result records which attempt it was, how many were configured, and why the previous attempt failed, so the stored evidence explains itself without anyone having to find the matching log lines.
- AC11: One-shot mode retries too, and the exit-code contract reports the final outcome (Story 5.5). A CI gate that failed because of one dropped connection teaches people to re-run it until it passes, which is the opposite of a gate.
- AC12: This is not the store's retry (Story 4.7). That one retries a database operation inside a scan; this one retries the scan. They are configured separately, and neither implies the other — the documentation says so, because "wsaw retries" would otherwise mean two different things.

---

# Epic 4 — Storage, Baselines & Change Detection

Persist scan results and turn a stream of scans into meaningful change events.

**Goal:** answer "what is new, gone, or changed since last time?" for every target and consent mode.

**Scope:** result store, its SQL dialects and drivers, retention, baseline/approved-state model, normalization rules (URL canonicalization, cache-buster and session-ID stripping), diff engine, script content hashing (SRI-style digests), allow/deny host lists, change event model, flap suppression.

**Out of scope:** long-term analytics warehouse, UI.

### Story 4.1 — Persist scan results

*As a Site Owner, I want results kept over time so that I can look at history.*

- AC1: Results are persisted to an embedded local store (single-file, no external database required) plus optional JSONL files on disk.
- AC2: Each result is addressable by target, consent mode, and timestamp.
- AC3: Retention is configurable by age and by count, with a documented pruning behavior.
- AC4: The store survives an unclean shutdown without corruption.

### Story 4.2 — Normalize URLs for comparison

*As a Site Owner, I want cache-busting noise removed so that diffs show real changes.*

- AC1: Configurable normalization: strip listed query parameters, strip all query parameters, collapse numeric/hash path segments via regex rules, lowercase host, drop default ports and fragments.
- AC2: Both the raw URL and the normalized key are stored; diffing uses the normalized key.
- AC3: Normalization rules are testable in isolation and documented with examples.

### Story 4.3 — Baseline and diff

*As a Compliance Officer, I want to compare a scan against an approved baseline so that only deviations need review.*

- AC1: A baseline can be created from a specific scan result and stored per target and consent mode.
- AC2: Diff reports added hosts, removed hosts, added assets, removed assets, changed script digests, changed cookies, and changed status codes.
- AC3: Diffing against "previous scan" is supported as an alternative to a pinned baseline.
- AC4: Diff output is deterministically ordered.

### Story 4.4 — Allow and deny lists (partly implemented: no shipped starter deny list, AC3)

*As a Compliance Officer, I want expected third parties declared so that alerts focus on the unexpected.*

- AC1: Per-target and global allow lists (host or domain patterns) suppress change events for known-good parties.
- AC2: Deny lists raise a finding whenever a matching host appears at all, regardless of baseline.
- AC3: A shipped, optional starter deny list of common tracking domains, clearly marked as advisory and updatable by the user.

### Story 4.5 — Change events with severity

*As an Operator, I want changes classified so that I can route them appropriately.*

- AC1: Each change produces an event with type, target, consent mode, severity, and the before/after values.
- AC2: Severity rules are configurable; sensible defaults (new third-party host in `reject` mode = high; changed script digest on a third-party script = high; new first-party image = low).
- AC3: Flap suppression: a change reverting within a configurable window is collapsed into a single event.

### Story 4.6 — Store results through `database/sql`, on SQLite

*As a Developer, I want the store to speak `database/sql` with SQLite as the default, so that moving wsaw to a server database later is a change of driver rather than a rewrite.*

bbolt is a key-value store, so every query wsaw needs is hand-rolled: listing newest-first works only because keys are built to sort that way, and "the scan before this one" is a cursor walk. SQL expresses those directly, and `database/sql` makes the driver the seam — which is what Tenet 12 wanted from a "swappable result store" and what a Kubernetes deployment will need when the store has to move off a volume (Story 6.10).

SQLite on its own does not make a pod stateless: it is still one file. The point is that Postgres then becomes a driver and a dialect, not a second implementation of everything.

There is no migration. Existing stores are discarded and the first run creates a new one.

- AC1: All persistence goes through `database/sql`. No driver-specific type appears outside the store package, and nothing else in wsaw knows which database is in use.
- AC2: SQLite is the default, through a **CGo-free** driver. The single static binary cross-compiled to four platforms is not negotiable (Tenet 14), and the dependency's size is weighed against that on the record.
- AC3: A result is stored as its JSON document plus the few columns needed to index it — target, consent mode, scan ID, start time, termination. The document is **not** shredded into normalised tables: the JSON schema is the product's interface (Tenet 16), and a second schema would drift from it.
- AC4: The queries wsaw actually makes are expressed as SQL and indexed: newest-first per target and mode, one result by scan ID, the result preceding a given one, baselines by target and mode, the audit log, and retention pruning by age and by count.
- AC5: Schema versions are embedded in the binary and applied on open, forward-only. A binary refuses to run against a newer schema than it knows rather than corrupting it.
- AC6: SQLite is opened for correct concurrent use — WAL journalling, a busy timeout, and foreign keys on — and the single-writer constraint is respected rather than discovered under load.
- AC7: Writes stay atomic across related rows. Approving a baseline and recording its audit entry either both happen or neither does; that guarantee exists today and must not be lost in the move.
- AC8: A corrupt or unreadable row is skipped and reported, not fatal. One bad record must not make a target's whole history unreadable — the behaviour bbolt storage already has.
- AC9: Artifacts stay content-addressed files outside the database. Screenshots and stored bodies do not belong in a row, and keeping them out leaves the door open to object storage later.
- AC10: The store's tests run against the default driver, and the suite is written so a second driver can be run through the same tests without rewriting them.
- AC11: Retention and the store file's location remain configurable exactly as now, and a store that cannot be opened fails at startup with an actionable message rather than on the first scan.

### Story 4.7 — A dialect seam, and drivers for Postgres and MySQL

*As an Operator, I want wsaw to store results in the database my organisation already runs, so that backup, replication, and retention are handled the way everything else is.*

Story 4.6 made the driver the seam. It did not make the SQL portable: the schema is declared `strict`, the migration version lives in `pragma user_version`, `Prune` deletes by `rowid`, summaries are derived with `json_extract`, and placeholders are `?`. Each of those is a deliberate SQLite choice that a second database does not share, and this story is where they become a dialect rather than an assumption.

- AC1: A dialect interface covers exactly what differs: placeholder style, the schema DDL, how the schema version is stored and read, the upsert form, the JSON accessor, and the delete-by-identity form used by retention. Everything else stays one implementation — a second copy of the query set would drift.
- AC2: `postgres` and `mysql` join `sqlite` as configured store drivers, selected by configuration with SQLite remaining the default. A deployment that does not ask for a server database must not acquire one.
- AC3: **The single-binary promise survives.** Both drivers are pure Go (`jackc/pgx` and `go-sql-driver/mysql`), CGo stays off, and cross-compilation to linux/darwin × amd64/arm64 is verified as part of the story (Tenet 14).
- AC4: The migration mechanism is forward-only and identical across dialects: the same numbered migrations, applied in the same order, recorded wherever that dialect records them. A store written by a newer wsaw is refused with the same message it is refused with today.
- AC5: The store's existing test suite runs unchanged against every dialect. The tests are the specification of what a store does; a dialect that needs its own semantics is a dialect that has changed the behaviour.
- AC6: Connection settings a server database needs — TLS mode, connection limits, connect timeout, credentials as secret references — are configurable and redacted. A DSN carries a password, so it is a secret everywhere a webhook URL is.
- AC7: A lost connection is survivable. The daemon does not exit because the database restarted, it has a configurable retry for database operations.
- AC8: The documentation states what a server database does and does not buy. It removes the volume and the file lock. It does not make wsaw multi-node: two instances sharing one database would still disagree about baselines and duplicate every scheduled scan.
- AC9: The README contains examples to configure MySQL, Postgres and SQLite.

---

# Epic 5 — Reporting, Export, Alerting & Web Interface

Get results and change events out of the daemon and into humans and other systems.

**Goal:** every result is consumable machine-first (stable JSON), human-first (readable report), and interactively (a web interface served by the daemon), and material changes reach a notification channel.

**Scope:** JSON/JSONL result format with a versioned schema, HAR export, CSV summaries, HTTP API (query results, trigger scan, health), a bundled web interface for browsing targets, results, diffs and findings plus approving baselines, a CLI command to open that interface pre-authenticated, webhook notifications, Slack/Teams/e-mail via generic webhook, exit-code contract for CI usage, rule-based alert filtering.

**Out of scope:** multi-user accounts, roles and permissions; user management; a separately deployed frontend application; editing the target configuration through the UI (config stays a versionable file — see Epic 3).

### Story 5.1 — Machine-readable result format

*As a Developer, I want a stable JSON schema so that I can build on wsaw's output.*

- AC1: Results serialize to JSON with an explicit `schemaVersion`.
- AC2: The schema is documented and published as a JSON Schema file in the repo.
- AC3: Schema changes follow a documented compatibility policy; additive changes do not bump the major version.
- AC4: JSONL streaming output is supported for large runs.

### Story 5.2 — HAR export

*As a Developer, I want HAR files so that I can open results in familiar tooling.*

- AC1: Each scan can emit a HAR 1.2 file that opens correctly in Chrome DevTools and common HAR viewers.
- AC2: HAR export is optional and off by default (size).

### Story 5.3 — Human-readable reports

*As a Compliance Officer, I want a readable summary so that I can act without parsing JSON.*

- AC1: A Markdown (and plain-text) report per scan: target, consent mode and outcome, counts by resource type, third-party host table with request counts, pre-consent findings, and the diff versus baseline.
- AC2: A CSV export of the host/asset table for spreadsheet use.

### Story 5.4 — HTTP API (partly implemented: ad-hoc scans are not rate-limited, AC3)

*As a Developer, I want an API so that other systems can query and trigger wsaw.*

- AC1: Endpoints: list targets, list results for a target, fetch a single result, fetch a diff, trigger an ad-hoc scan, health/readiness.
- AC2: Bound to localhost by default; optional bearer-token auth; TLS support.
- AC3: The API is read-mostly and safe: ad-hoc scans are rate-limited and restricted to configured targets unless explicitly enabled otherwise.
- AC4: OpenAPI description shipped in the repo.

### Story 5.5 — CI / one-shot exit-code contract

*As a Developer, I want wsaw usable as a CI gate so that regressions are blocked before release.*

- AC1: `wsaw scan --once` exits `0` on no findings, `1` on findings above a configurable severity, `2` on operational error (browser failure, config error).
- AC2: `--fail-on` selects the severity threshold.
- AC3: Output format is selectable (`--format json|jsonl|markdown|csv`) and writes to stdout when no output directory is given.

### Story 5.6 — Notifications

*As an Operator, I want to be alerted so that I learn about changes without watching files.*

- AC1: Generic webhook notifier posting the change event as JSON, with configurable headers.
- AC2: Templated payloads so Slack/Teams/Mattermost work without dedicated code.
- AC3: Delivery retries with exponential backoff; failures are logged and surfaced as a self-metric, never lost silently.
- AC4: Notification filtering by severity, target label, and change type.

### Story 5.7 — Bundled web interface

*As a Compliance Officer, I want a web interface served by the daemon so that I can review findings without shell access or JSON tooling.*

- AC1: The daemon serves a web interface from the same process and the same port as the HTTP API (Story 5.4), under a distinct path prefix.
- AC2: All static assets (HTML, CSS, JS, fonts, icons) are embedded in the binary via `go:embed`. The single-binary promise holds — no asset directory to deploy, no CDN, no network fetch at runtime.
- AC3: The interface is served as HTML rendered by the daemon (`html/template`), enhanced progressively. No Node.js/npm build step is required to build wsaw from source.
- AC4: The UI is a strict consumer of the same API and the same result schema as any external client — it gets no privileged private endpoints and no separate data path (Tenet 16: machine output first, human output derived).
- AC5: The web interface can be disabled entirely by configuration; the API remains usable without it.
- AC6: Usable without JavaScript for the core read paths (target list, result view, diff view), degrading to plain server-rendered pages.

### Story 5.8 — Dashboard and target overview (partly implemented: AC4's filter by label and severity is covered by Story 5.23's substring filter and Story 5.24's severity matching; filter by staleness, and sort, are not implemented, AC4)

*As an Operator, I want a single screen showing the state of every target so that I can see at a glance whether wsaw is healthy and whether anything changed.*

- AC1: A target list showing, per target: name/URL, labels, configured consent modes, last scan time per mode, last scan outcome, consent outcome, and open findings by severity.
- AC2: Targets whose last successful scan is older than their schedule allows are visibly flagged — a stalled watcher must be obvious in the UI, not just in metrics (Tenet 8).
- AC3: Daemon health is surfaced: Chrome usable, config loaded, queue depth, browser restarts, notification failures.
- AC4: Filter and sort by label, severity, and staleness.
- AC5: Empty and error states are explicit ("never scanned", "scan failed", "consent unverified") and never render as a clean zero.

### Story 5.9 — Result and diff browser (partly implemented: the asset table is not sortable and has no status filter, AC2)

*As a Compliance Officer, I want to drill into a single scan so that I can see exactly which third parties were contacted and in which consent state.*

- AC1: A scan result view showing scan metadata (target, consent mode, consent outcome and reason, termination reason, Chrome version, applied overrides), request counts by resource type, and the full asset table.
- AC2: The asset table is filterable by host, resource type, first/third-party, consent phase (`pre-interaction` / `post-interaction`), and status, and is sortable.
- AC3: A host-centric view aggregating requests per registrable domain, with first/third-party classification and request counts.
- AC4: Initiator chains are inspectable, so a user can see which script caused a given request.
- AC5: A diff view against baseline or previous scan, grouped into added hosts, removed hosts, added/removed assets, changed script digests, and changed cookies, with severity shown per change.
- AC6: A side-by-side comparison of the same target across consent modes, making "fired despite reject" the visually obvious finding.
- AC7: Every view offers download of the underlying data (JSON, CSV, HAR where available) so the UI never becomes the only way to reach a result.
- AC8: Result history per target and consent mode is browsable over time.

### Story 5.10 — Baseline review and approval workflow (partly implemented: no allow-list write-back from a finding, AC2 and AC3)

*As a Compliance Officer, I want to approve a scan as the new baseline from the UI so that reviewing changes does not require the command line.*

- AC1: A diff view offers "approve as baseline" for the target and consent mode in question, promoting that scan to the stored baseline (Story 4.3).
- AC2: An individual host or asset can be added to the target's allow list directly from a finding, with the change written back to a documented, human-readable location.
- AC3: Any config-affecting write states plainly where it was persisted, so a file-based, version-controlled workflow is not silently bypassed.
- AC4: Write actions require the same authentication as the API and are refused when the daemon runs in read-only mode.
- AC5: Approvals and allow-list additions are recorded with timestamp and outcome in a log an operator can audit.
- AC6: Ad-hoc "scan now" for an existing target is available from the UI, subject to the same rate limits as the API (Story 5.4 AC3).

### Story 5.11 — Web interface security

*As an Operator, I want the web interface to be safe to expose so that adding a UI does not add an attack surface.*

- AC1: The interface binds to localhost by default, exactly like the API; remote exposure is explicit configuration.
- AC2: Authentication is shared with the API (bearer token, plus a session cookie for browser use); TLS is supported.
- AC3: **All scanned-site data rendered in the UI is treated as hostile.** URLs, header values, cookie values, DOM snippets and script content are contextually escaped; captured HTML/JS is never rendered as markup and never executed.
- AC4: A strict `Content-Security-Policy` with no `unsafe-inline` and no external origins; `X-Content-Type-Options`, `X-Frame-Options`/`frame-ancestors`, and `Referrer-Policy` are set.
- AC5: Stored response bodies are served with a non-renderable content disposition and never from the UI's own origin without sandboxing. **Narrowed by Story 5.17 for screenshots only:** a screenshot is a PNG that Chrome produced under wsaw's control — the page influenced its pixels, not its bytes — so it may be rendered as an image, served with an image content type, `nosniff`, and a policy that permits an image and forbids everything else. A response body *is* the page's bytes and keeps the opaque attachment. What is served as an image must also *be* one: the kind and the file's magic bytes both have to agree.
- AC6: State-changing requests are CSRF-protected.
- AC7: Secrets (basic auth, proxy credentials, webhook tokens) are redacted in every UI view, matching the API's redaction (Story 3.6).
- AC8: A read-only mode disables all write actions, for deployments where the UI is shared with reviewers.

### Story 5.12 — Show running scans in the web ui

*As an Operator, I want the web interface to show currently running scans to indiciate activity for a target and on the dashboard over all targets

- AC1: A line is shown on the target detail page, when the scan is initiated, outcome is stated as "pending".

### Story 5.13 — Add a log line when a scan is started

*As an Operator, I want the log to contain a line every time a scan is started so acitivity is matched in the log

- AC1: A log line is emitted, containing the scan id and target context.

### Story 5.14 — Microsoft Teams notifications

*As a Compliance Officer, I want findings to arrive in the Teams channel my team already watches, formatted so that the important part is legible without opening anything else.*

Story 5.6 can already post to Teams through a templated webhook, and for a while that was the honest answer. It is no longer sufficient, for three reasons.

Microsoft retired the Office 365 connector webhooks that the templated approach targeted; the final deadline was 30 April 2026, and new integrations go through a Power Automate **Workflow** webhook, which expects an Adaptive Card inside a message envelope rather than the old `MessageCard`.

An Adaptive Card is deeply nested JSON. Hand-authoring it in a Go text template is error-prone, and — this is the part that matters — a malformed card **fails silently**: the Workflow accepts the request, returns success, and posts nothing or an empty card. A broken alert path that reports itself healthy is the failure mode wsaw exists least to tolerate (Tenet 8), and it is not something an operator can be expected to catch by inspecting a template.

The generic notifier also emits one event per change, which is right for a webhook consumer and wrong for a chat channel: a scan with forty changes would post forty messages and be rate-limited.

- AC1: A `teams` notifier posts to a Power Automate Workflow webhook URL, sending an Adaptive Card wrapped in the message envelope Workflows expect. The card format and version used are documented, so a Teams administrator can see what will be posted.
- AC2: The webhook URL is a secret reference like any other and is redacted wherever it appears. Workflow URLs carry their authorisation in the query string, so treating one as ordinary configuration would leak it into logs and results.
- AC3: All 2xx responses count as success. Workflows answer `202 Accepted` rather than `200`, and treating that as a failure would produce endless retries of messages that were in fact delivered.
- AC4: One scan produces **one** card, summarising its changes, rather than one card per change. Where there are more changes than the card can hold, the card says how many were omitted and the count it reports is the true total.
- AC5: The card leads with what a reviewer needs first: the target, the consent mode, the highest severity present, and the consent outcome — then the changes. Severity is conveyed as text as well as colour, so a colour-blind reader or a plain-text notification digest loses nothing.
- AC6: The card links back to the result in the web interface where a base URL is configured, and states plainly that it is a summary. A chat message must never be the only place a finding exists.
- AC7: A scan whose result is not trustworthy — failed, skipped, or truncated — is reported as such rather than as a clean scan with few findings, consistent with how every other output treats it.
- AC8: Payload size is bounded to what Teams accepts, and the notifier verifies before sending that what it built is valid JSON. A card rejected for size or shape is a delivery failure that is logged and counted (Story 5.6, AC3), never a silent no-op.
- AC9: The legacy `MessageCard` format is available as an explicit opt-in for tenants still running a connector webhook, documented as deprecated, and not the default.
- AC10: Delivery is tested against a local fixture server asserting the exact posted payload, so the card structure is covered without a Teams tenant. A live tenant is never required to run the test suite.

### Story 5.15 — Link to the last scan from the dashboard

*As an Operator, I want to open a target's last scan straight from the dashboard so that seeing something odd and looking at it are one click apart, not two.*

- AC1: The last-scan cell on the dashboard links to that scan's result page. An icon may carry the link instead of, or in addition to, the cell text.
- AC2: The link points at the **specific scan** the row describes, not at "the latest". A row that says "3h ago" and opens a scan from four minutes ago has misrepresented what the reader clicked.
- AC3: A series that has never been scanned renders no link. The explicit "never scanned" state stays as it is — a link to nothing is worse than no link (Tenet 5).
- AC4: A failed or skipped scan is still linked. That is the row a reader most needs to open, and a UI that only links the healthy ones hides its failures.
- AC5: The link's accessible name says where it goes — the target, mode and scan — rather than "click here". Where an icon is used it carries a text alternative, and the icon is never the only indication that the row is reachable (Story 5.11's reasoning applied to the UI's own affordances).
- AC6: A row whose series has a scan in flight still links to the last **finished** scan; the pending row itself links nowhere, because there is no result yet, and says so (Story 5.12).
- AC7: It is a server-rendered anchor, so it works with JavaScript disabled and can be opened in a new tab, bookmarked, and copied like any other link (Story 5.7, AC6).

### Story 5.16 — Refresh the dashboard on an interval, set from the dashboard

*As an Operator, I want the dashboard to refresh itself on an interval I can change from the page, so that it can be left on a screen and still be worth looking at.*

- AC1: The dashboard refreshes itself on an interval, and the interval can be changed from a control on the dashboard, including turning it off. **Narrowed by Story 5.18 for the scan detail page:** the interval applies to the pages where something can change; one finished scan is an immutable record, so that page holds still and says why.
- AC2: The control works without JavaScript, like the rest of the core read paths (Story 5.7, AC6) — a form that submits, not a script that rewrites the page.
- AC3: The choice is remembered per browser and survives a reload, without being written into wsaw's configuration. A viewer's display preference is not deployment configuration (Tenet 15), and one person's wall display must not change what anybody else sees.
- AC4: The configured default is settable for the deployment, so a wall display is right the first time it is opened rather than after someone adjusts it.
- AC5: **The page says when it was rendered and when it will refresh next.** A page that reloads silently invites the reader to trust whatever is on it; a page that says "as of 14:32, next in 20s" cannot mislead them about how old it is. This is the acceptance criterion that makes the rest of the story safe.
- AC6: A stale page is obvious when refreshing has stopped working — a tab that lost its connection, a daemon that went away, a refresh turned off and forgotten. Whatever the reason, the reader must be able to tell that what they are looking at is old (Tenet 8 applied to the interface itself).
- AC7: Turning refreshing off wins over the automatic reload that a running scan triggers. The reader asked for the page to hold still; the running scan is visible on it, and a manual reload is one keystroke away.
- AC8: The interval has a floor, and the floor is documented. Every refresh re-renders the whole dashboard, which reads every series' latest result — a one-second interval on a list of two hundred targets is a denial of service against one's own daemon.
- AC9: A refresh does not repeat an action or re-show a stale message. The dashboard's write actions are already POST-then-redirect, so a reload must not re-submit one, and the flash message from an earlier action must not reappear on every subsequent refresh as though it had just happened.
- AC10: A refresh may be skipped while the tab is not visible, where the browser can say so. Refreshing a page nobody is looking at spends the daemon's time to no purpose. It is an enhancement, not a requirement: without it the page still refreshes correctly.
- AC11: The interval can also be given in the URL, so a wall display can be pointed at one address and need no further setup, and a shared link carries what the sender was looking at.

### Story 5.17 — Show captured screenshots on the scan detail page

*As a Compliance Officer, I want to see the screenshots a scan captured on the scan's own page, so that the consent banner and what wsaw did about it are visible rather than merely described.*

A scan with `screenshots: true` captures the page before the consent interaction and again after it. Both are stored, both are referenced in the result — and neither is shown anywhere. They are the most directly persuasive evidence wsaw produces: a reader can be told that the banner was rejected, or they can see the banner and then see it gone.

The pair matters more than either image. "Before" is the banner as the site presented it, which is what a regulator asks about; "after" is what wsaw's interaction actually did, which is the claim every `reject`-mode finding rests on. Side by side they either corroborate the recorded consent outcome or contradict it, and a contradiction is exactly what somebody needs to see.

- AC1: The scan detail page shows the screenshots the scan captured, before and after the consent interaction, presented as a pair rather than as two unrelated files.
- AC2: A scan that captured none says so, and says how to enable them, instead of rendering an empty frame or nothing at all (Tenet 5).
- AC3: A screenshot whose stored file is gone — pruned, or on a volume that went away — is reported as missing rather than shown as a broken image. Evidence that has expired must not look like evidence that never existed.
- AC4: Each screenshot shows its SHA-256 and its size, and offers the original file for download. A screenshot in a compliance pack is worth only as much as the reader's ability to check it against the digest the scan recorded.
- AC5: **A screenshot may be rendered; a stored response body still may not.** Story 5.11's AC5 currently forbids both, and this story narrows it: a screenshot is a PNG that Chrome produced under wsaw's control, so the page influenced its pixels and not its bytes, whereas a response body *is* the page's bytes. Screenshots are therefore served with an image content type and nothing else — `nosniff`, no script, a policy that permits an image and forbids everything else — and bodies keep their opaque attachment.
- AC6: The page stays cheap to open. Screenshots are large, so they are scaled for display, loaded lazily, and never inlined into the HTML; a result page must not cost megabytes before a reader has asked to look at one.
- AC7: The images are reachable without JavaScript, like the rest of the core read paths (Story 5.7, AC6).
- AC8: Screenshots can contain personal data, so nothing here makes them easier to reach than the results they belong to: same authentication, same read-only rules, same retention. They remain off by default (Tenet 19).

### Story 5.18 — Do not auto-refresh the scan detail page

*As a Compliance Officer reading one scan, I want its page to hold still, because nothing on it can change and being reloaded costs me my place.*

Story 5.16 gave the interface an auto-refresh interval, and it applies to every page. On the dashboard and on a target's history that is the point: a scan finishes, a new one appears, a series goes stale. A scan detail page is the opposite — it is one finished scan, an immutable record. Refreshing it re-renders identical content, and the pages worth reading closely are exactly the ones with the longest asset table and the screenshots, where a reload throws away scroll position, a filter that was just typed, and an image the reader was looking at.

- AC1: The scan detail page does not auto-refresh, whatever interval is in force for the interface.
- AC2: It says why, rather than quietly behaving differently from the page the reader came from. Somebody who set thirty seconds on the dashboard and then opened a scan should be told that this page holds still because the scan is finished, not left wondering whether refreshing is broken.
- AC3: The page still says when it was rendered. That remains the honest anchor for a reader who has had the tab open for an hour — but there is no "next refresh in" claim, and no going-stale warning, because a finished scan does not go stale.
- AC4: An interval given in the URL does not re-enable it here either. `?refresh=30` exists so a wall display needs no setup, and a wall display of one finished scan still has nothing to refresh; the parameter keeps working for the pages where something can change.
- AC5: The interval control stays usable on this page — a reader may set the interface's interval from anywhere — but must not suggest it affects the page they are on.
- AC6: Pages where something can change keep refreshing exactly as they do now: the dashboard, and a target's history, where a running scan finishes and a new result appears.
- AC7: A manual reload still works and still shows the same record. Not refreshing is about not doing it unasked, not about making the page harder to refresh.

### Story 5.19 — Share one scan result by link, without handing over the token

*As a Compliance Officer, I want to send a colleague or an auditor a link to one scan result, so that they can read the evidence without being given credentials to the whole interface — and without the link becoming a permanent public URL.*

Today there are two options and both are wrong. Hand over the API token, which unlocks every result, every target and every write action, and cannot be taken back from someone who has read it. Or export the JSON and email it, which detaches the evidence from the tool that explains it and from the digests that make it checkable.

What is wanted is narrower than either: this result, for a while, read-only. A signed token in the URL gives exactly that and needs no share registry — the link carries its own authority, and wsaw verifies it without looking anything up.

The trade-off has to be stated rather than discovered. **A stateless link cannot be revoked before it expires.** That is the price of not keeping a share table, and it is why validity is short by default, why scope is a single result, and why rotating the signing key — which invalidates every outstanding link at once — is the documented way to withdraw access in a hurry.

- AC1: An authenticated operator can produce a share link for one stored result, from the result page and from the command line. The command-line path matters because the interface is not always exposed.
- AC2: The link carries a signed token — a JWT — naming exactly one target, consent mode and scan ID, and an expiry. It is not a credential for anything else: presenting it for another result, or for the target list, the audit log or the schedule, is refused.
- AC3: **Read-only, without exception.** A share token cannot approve a baseline, trigger a scan, change configuration, or reach any write path, whatever wsaw's read-only setting happens to be for authenticated users.
- AC4: Validity is configurable, with a documented default and a documented maximum that a caller cannot exceed. A link that outlives the interest in the scan is a link nobody remembers sharing.
- AC5: The token is signed with a key that is wsaw's own, kept as a secret reference like any other, and **not** the API token. Sharing must not be derivable from admin access, and withdrawing sharing must not mean rotating the credential the operator uses.
- AC6: **The algorithm is pinned.** The verifier accepts one algorithm and rejects everything else, including `none`, rather than trusting the algorithm the token names — that is the oldest way to forge a JWT. The signature is checked before any claim is read.
- AC7: An expired link says it has expired, and an unsigned or altered one says it is not valid. Those are different facts for the person holding the link, and the difference costs nothing to disclose.
- AC8: The shared page shows the result — assets, hosts, consent outcome, changes, screenshots and the downloads — and nothing else. No navigation into the rest of the interface, no configuration path, no audit log, no "scan now", no "approve as baseline".
- AC9: The page states that it is a shared view and when it expires, so the reader knows the link is not permanent and the sender can see what they sent.
- AC10: A share token reaches the artifacts of **its own result** and no others. Screenshots and stored bodies are content-addressed rather than scoped by scan, so the check is that the reference is one the shared result actually names — otherwise a link to a harmless scan would be a key to every stored artifact.
- AC11: The token does not leak through wsaw's own surfaces: it is scrubbed from logs, kept out of results and error messages, and the existing `Referrer-Policy: no-referrer` keeps it out of other sites' logs. A URL is a poor place for a secret, which is the other reason validity is short.
- AC12: Sharing is off until configured. It publishes data that can be personal to whoever holds a link (NFR §4, Tenet 19), so it is opt-in, and the documentation says plainly what a link grants, for how long, and that revocation before expiry means rotating the key.

### Story 5.20 — Identify the baseline in a target's history, and link to it

*As a Compliance Officer reading a target's history, I want to see which scan is the approved baseline and open it in one click, so that "what changed" has a visible other half.*

Every change wsaw reports is a statement about a comparison, and on the history page the thing being compared against is the one item that cannot be opened. The panel above the table names the baseline's scan ID and then leaves the reader to find that ID by eye in a hundred rows of near-identical hex; the ID is `<code>`, not a link; the row that holds it looks like every other row; and that row still offers "Approve as baseline", an action that would record an approval for the state that is already approved.

None of this is a missing feature so much as a missing sentence. The page already has the baseline in hand — it renders its scan ID, its approval time and who approved it. What it does not do is connect that to the row it belongs to, or to the result it names.

- AC1: The baseline's scan ID in the panel above the history links to that scan's result page. Seeing which scan is the baseline and reading it are one click apart, not a copy-paste and a search (Story 5.15's reasoning, applied to the other end of the comparison).
- AC2: The history row that is the baseline is marked as such. The marking is legible on its own — a label or an icon with a text alternative — and never colour alone, so a reader finds it by scanning the table rather than by matching hex digits.
- AC3: The marked row still links to its own result, exactly like every other row. Marking a row must not cost it the affordance every other row has.
- AC4: The baseline row does not offer "Approve as baseline"; it says that it is the baseline. Re-approving the current baseline writes an audit record for a change that did not happen, which makes the audit log harder to read for no gain (Story 5.10).
- AC5: **A baseline that is not in the list is reported as absent from it, not left silently unmarked.** The history shows a bounded number of recent scans and retention prunes beyond that, so an approved baseline can be older than the window. The panel still links to it, and says that the row is not in the table below — an unmarked table must never be readable as "no scan here is the baseline" (Tenet 5).
- AC6: **The link never dangles.** A baseline keeps its own copy of the approved result precisely so that retention cannot invalidate it, which means the baseline can outlive the result row it was approved from. Where the underlying result is gone, the panel says that the approved copy is still held and offers no link into a 404 — or serves the baseline's own stored copy and says that is what it is showing.
- AC7: A series with no approved baseline keeps saying so, and keeps saying what scans are compared against instead. That panel is already right; this story does not change it.
- AC8: Everything here is a server-rendered anchor and works with JavaScript disabled (Story 5.7, AC6), under the same authentication and read-only rules as the rest of the interface. A read-only deployment sees the marking and the link: identifying the baseline is a read, and the approve control is the only part of this that is not.
- AC9: None of it appears on a shared view. A share link is one result with no navigation into the interface (Story 5.19, AC8), and a link to the baseline would be a link out of the shared scope.

Not in this story, and worth its own: a scan detail page saying "this scan is the current baseline", and the Changes section naming — and linking — the scan it was compared against rather than calling it "the baseline".

### Story 5.21 — Open the web interface from the CLI, already signed in

*As an Operator, I want one command that opens my browser straight into wsaw's dashboard, already authenticated, so that reaching the interface costs a command rather than finding the URL, finding the token in the config, and pasting both into the right places.*

Story 5.11 AC2 put a bearer token in front of the interface, correctly — but the audience that pays for it is the one that needs it least. An Operator running `wsaw ui` already has shell access to the config file the token lives in (Story 3.6); making them also copy that token into a login form is friction with no security benefit, and it teaches people to leave a browser tab logged in indefinitely to avoid doing it twice.

The shape borrows from Story 5.19 rather than reinventing it: a link that carries its own authority, verified without a lookup table, scoped to exactly one thing and nothing more. Where a share link's one thing is "read this result," this one's is "start one browser session" — and because that is a local, same-operator action rather than something handed to a colleague, it can be single-use and short-lived instead of merely time-boxed, and it never needs to be typed by a human at all.

- AC1: `wsaw ui` opens the operator's default browser at the dashboard, already authenticated, when a token is configured (Story 5.11 AC2). When no token is configured, it just opens the URL — matching the daemon's own auth-optional behaviour (`withAuth`: `!Token.IsSet()` skips the check).
- AC2: The command reads the same config the daemon runs with (`--config`, defaulting like every other subcommand), resolves the interface's address and the token from the same secret reference (Story 3.6), and does not need a new way to read the token back out of a running daemon over HTTP. If nothing answers at the resolved address, the command says so — it does not open a browser tab onto a connection error and call that done.
- AC3: The standing API token is never put in a URL. The CLI instead asks the daemon for a **one-time login token**: a new endpoint, reachable only from loopback regardless of the interface's configured `Listen`, that mints a random, single-use token authorising nothing but "start one session," expiring in a short, documented window (default a few seconds). A second redemption, or one past expiry, fails explicitly — the same honesty Story 5.19 AC7 already requires of an expired share link, not a generic 401.
- AC4: The one-time token is its own secret, distinct from both the API token and the sharing key (Story 5.19 AC5); rotating or reusing either does not touch it. It cannot be presented as a `Bearer` token to any API endpoint and grants no scope beyond what the session cookie it sets already carries.
- AC5: The token appears in exactly one HTTP request — the redemption. That request's response sets the session cookie (the same `wsaw_session` cookie `withAuth` already issues) and redirects to the plain dashboard URL with no token in it, so nothing token-bearing ever sits in browser history, autocomplete, or a `Referer` header on the next click (`Referrer-Policy: no-referrer`, Story 5.11, covers the hop after that).
- AC6: Where there is no usable way to open a browser — a headless host, no `xdg-open`/`open`, no `$DISPLAY` — the command prints the fully-formed one-time login URL instead of failing outright, so it stays useful over SSH. The printed URL carries the same short expiry as the happy path; the fallback is not a wider hole than opening the browser directly would have been.
- AC7: `--print` prints the login URL without attempting to open a browser at all, for a script or an SSH session that hands the link to a human elsewhere.
- AC8: If the web interface is disabled (Story 5.7 AC5), the command fails immediately with that reason rather than opening a browser onto a 404.
- AC9: The one-time-login endpoint needs no CSRF token — there is no prior session to protect against (Story 5.11 AC6) — but is rate-limited per source address, so its use is bounded by more than the token's own entropy and lifetime.
- AC10: A read-only deployment (Story 5.10 AC4, Story 5.11 AC8) opens read-only, exactly as a manual login would. The command grants no capability the operator's configured token did not already have.
- AC11: Regression coverage: a token is rejected on a second redemption, rejected after its window expires, never appears in the browser's final URL, and is rejected outright if presented as a `Bearer` token against any API endpoint.

### Story 5.22 — Instant, client-side filtering of the target list (superseded by Story 5.23: the watchboard redesign replaced this checkbox panel with a single substring filter over name, URL and labels — AC1 and AC11's severity/outcome/mode facets and their match counts no longer exist in the UI)

*As an Operator watching a long target list, I want to narrow it by severity, consent outcome, mode and label as I type or click, so that finding the handful of rows I care about does not cost a page round-trip for every change of mind.*

Story 5.8 AC4 asks for filter and sort on the dashboard and is still open — today the only way to find one target among many is the browser's own text search, which knows nothing about severity, outcome, mode, or a label's structure. Server-side filtering would work but is the wrong shape for this page: the dashboard already renders every target's current state in one response (Story 5.8 AC1), so the data a filter needs is already on the page, and round-tripping to the daemon on every keystroke or checkbox would make the common case — "does anything need my attention right now" — slower than just scanning the list by eye. The values worth filtering on are exactly the ones the compact target list already renders per row (`internal/httpapi/templates/dashboard.html`): a target's worst severity and its labels, and per consent-mode series the consent outcome and the mode itself (`$mode`, `.ConsentOutcome`).

- AC1: Filter controls sit above the target list: a multi-select or checkbox group for severity (matching the values Story 4.5 defines), one for consent outcome (Story 2.5 AC2, Story 2.8 AC3), one for consent mode (Story 2.1 AC1), and a text input for labels that matches against the `key=value` pairs Story 3.1 AC1 defines. Each is independent and combines with the others by AND; multiple selections within one filter combine by OR — the same convention as any faceted filter, stated explicitly in the UI so it isn't guessed at.
- AC2: Filtering runs entirely client-side, against the DOM already rendered by the server for this page load. No request is made to apply, change, or clear a filter — the daemon is not asked to re-render or re-query for something the browser already has.
- AC3: A filter takes effect the instant its control changes — a checkbox toggle, a select change, a keystroke in the label field, with no debounce delay for checkboxes/selects and a short debounce (documented, on the order of 100–150ms) for free-text label input to avoid filtering on every keystroke of a fast typist.
- AC4: A target section is hidden in full when none of its consent-mode rows match the active filters; where a target has multiple modes and only some rows match, the target section stays visible with only the matching rows shown, so a reader is never left looking at an empty target header.
- AC5: A visible, always-current count reads "showing N of M targets" (or the row-level equivalent), so filtering to zero results is distinguishable from a slow-loading page — an empty filtered view states plainly that nothing matched, per the empty-state honesty Story 5.8 AC5 already requires.
- AC6: A single "reset all" control clears every active filter in one action and is disabled (or hidden) when no filter is active, so it is never a dead click.
- AC7: The active filter state lives in the URL (query string), not only in DOM/JS state: applying a filter updates the URL without a navigation (`history.pushState`/`replaceState`), so the current view survives a page reload, a bookmark, and a link shared with a colleague. "Reset all" clears the query string to match.
- AC8: This is additive to, not a replacement for, the no-JavaScript baseline Story 5.7 AC6 requires: with JavaScript disabled, the filter controls are either absent or inert, and the full target list renders exactly as it does today — the dashboard's core read path does not regress to depend on script.
- AC9: Filtering never touches write actions already on this page (Story 5.10 AC6's "scan now", Story 5.16's refresh control) — a filtered-out row's in-flight scan still completes and still updates the row if a refresh brings it back into view.
- AC10: Regression coverage: each filter dimension in isolation, two filters combined (AND), multiple values within one filter (OR), the zero-match empty state, "reset all" restoring the unfiltered list, the URL round-trip (filter → reload → same filtered view), and the no-JS fallback rendering the unfiltered list with no broken controls.
- AC11: Each severity, outcome and mode option shows the number of targets it would match, in parentheses after the option — the faceted-search convention: the count previews what selecting that value alone in its own dimension would show, combined with whatever is already active in the *other* dimensions, so it answers "if I also checked this" rather than repeating the page's unfiltered total. It recomputes on every filter change, never only once at load, since a severity count taken while an outcome filter is active must reflect that outcome filter or it answers a question the reader did not ask. An option matching nothing shows `(0)` rather than no count at all — absence stated, not left for the reader to infer (Tenet 5's reasoning applied to a filter control). The label field has no discrete values and so gets no count.

### Story 5.23 — The target list becomes a watchboard (AC7 and AC9 superseded by Story 5.25: the tile's own age link and separate request-count marking were replaced by a merged title/figures link and a cycle-position bar, though the underlying marking rules are unchanged)

*As an Operator scanning the target list every day, I want the two signals that decide whether I have to act — how old the last scan is, and whether anything changed against baseline — to be what I see from across the room, so that "does anything need me right now" is a glance instead of a read of eight tables.*

The list this replaces answered "what is configured" well and "is anything wrong right now" badly: age and pre-consent counts were two numeric columns among ten. Nothing about what the list already rendered is removed; what changed is what size, position and colour claim about it. This story folds together a design handoff applied and then iterated on directly in this repository (`design_handoff_watchboard`, `design_handoff_target_tile`) rather than written up first — flagged per AGENTS.md §1, and written up here after the fact so the story document catches up with what shipped.

- AC1: One tile per target-and-consent-mode series, in place of the table-per-target list. A fixed-width **target tile** (the rail) heads each row, followed by one **scan tile** per configured consent mode (`internal/httpapi/templates/dashboard.html`, `internal/httpapi/watchboard.go`).
- AC2: Targets are grouped by the `env` label, each group a collapsible `<details>` — collapse is native disclosure behaviour, not a scripted toggle, so it works with JavaScript off. A target with no `env` label is grouped under a named "no env label" group, listed last, rather than hidden or folded into another group (Tenet 5). `watch.js` remembers which groups are collapsed, per browser, in the `wsaw_envs` cookie (Tenet 15) — collapse state survives the page's own auto-refresh.
- AC3: The target tile shows the target's name (linking to `/compare/{name}`, unchanged), its worst severity across all its scan tiles as an always-visible badge (never gated on the severity being "notable" — info and low read as a quiet grey rather than disappearing), and its URL with the scheme stripped. A long name or URL truncates with an ellipsis instead of wrapping and pushing the row height around; the full URL is still reachable via the host line's `title` attribute. The tile's left border widens from 4px to 12px on hover (120ms ease) as its only interactive affordance — the name link inside it is unaffected (`design_handoff_target_tile`, option 4a).
- AC4: The header is one row: the target count, a stale-series count, a running-scan count (or "activity not tracked" where the daemon does not track it), a compact "as of HH:MM · interval" freshness readout, the filter control, and "showing N/M". Each count is muted until it is non-zero — a board that reports "0 stale" in a colour every render teaches the reader to stop reading it — and the same applies to a not-ready indicator, shown only when the daemon is not ready rather than restating "ready" on every render.
- AC5: A prior attempt at a header-level "N pre-consent hosts" total was removed: it summed each series' pre-consent count across every displayed mode row, so a target contacting the same tracker under two different consent modes was counted twice — a number that did not correspond to distinct hosts, distinct events, or anything else real. The tile-level "N pre" figure (per series, not summed) is unaffected.
- AC6: Filtering replaces Story 5.22's severity/outcome/mode checkbox panel with one substring match over a target's name, URL and labels — case-insensitive, matching what the row already shows so a reader can see why something matched. It is applied server-side from a `wsaw_filter` cookie on every render (so the remembered preference means the same thing with JavaScript off) and then live, client-side, on a 120ms debounce once script runs; the Apply/Clear buttons the no-JS path needs are hidden once live filtering takes over. Story 5.8 AC4's filter-by-severity-and-staleness and Story 5.22's per-option match counts are not carried forward — a narrower control than either superseded story specified, on explicit request in the session that made the change (AGENTS.md §1).
- AC7: A scan tile is the one thing on the board allowed to draw the eye by itself, and only for a change that has actually happened: it is marked (border colour, `var(--crit)`) when the last scan's diff against the series' approved baseline contains at least one critical-severity change — not merely a pre-consent host, which by itself no longer colours the tile. The tile's request count is separately marked (`var(--warn)`, bold) when the displayed scan deviates from the baseline at any severity, gated strictly on a baseline actually being set — Severity's own fallback to comparing against the previous scan, used when no baseline exists, is a different claim and does not trigger it.
- AC8: A "hosts only" toggle, off by default and remembered per browser (`wsaw_hostsonly` cookie), narrows every marking in AC7 (and the target tile's own badge, AC3) to a host appearing, disappearing, or being denied — ignoring an asset, script, cookie, status flip, or consent regression. The toggle is a checkbox in the same filter form; since severity is computed server-side, JavaScript submits the form the instant the checkbox changes rather than waiting for Apply, and the checkbox still reaches Apply with script off. The JSON API (`GET /api/v1/targets`) always reports full, unnarrowed severity regardless of this cookie — it is a display preference of the HTML dashboard, not a property of the result (Tenet 16).
- AC9: The whole scan tile opens the scan it describes — not just the age — while still holding its own "history" link and a "Scan now" button, which an `<a>` cannot contain as nested interactive content. The existing age link's hit area is stretched over the tile by CSS (`position:absolute; inset:0` against the tile's own positioned box) rather than wrapping the tile in an anchor; "history" and "Scan now" are raised above it so they stay independently clickable. The age link itself carries no underline.
- AC10: "history" and "Scan now" are secondary to the tile's own information: they stay at `opacity:0`, right-aligned, until the tile is hovered — or, for a keyboard reader who cannot hover, until one of them has focus (`:focus-within`, not only `:hover`, so both stay reachable by Tab and by assistive technology throughout). They are positioned out of normal flow so a tile with no pending/stale badge reserves no empty space for them, and when shown they overlay the metrics line rather than pushing a reserved row below it — a small opaque rectangle, matching whichever background the tile itself is currently showing (plain, critical-tinted, or running-tinted), so the metrics text underneath does not show through.
- AC11: Every no-JavaScript guarantee the list already had is preserved: the target list, the filter, the "hosts only" toggle, group collapse, and the scan tile's links and "Scan now" form all work as plain server-rendered forms, anchors and `<details>` with script disabled (Story 5.7 AC6). No inline `style` or `script` attribute is introduced anywhere in this story's markup — the CSP forbidding `unsafe-inline` still holds.
- AC12: Regression coverage: grouping by label and its order, the unlabelled group, collapse remembered per browser and scoped to one group, server-side and client-side filtering, a critical change marking a tile and a pre-consent host alone not doing so, a request count marked only when it deviates from an actual baseline (not the previous-scan fallback) and only when a baseline is set, the "hosts only" toggle narrowing the same markings and the JSON API ignoring it, the target tile's badge present even with nothing to compare, and the host line's full-URL title.

### Story 5.24 — Filter the target list by severity, from the same text box

*As an Operator scanning the watchboard, I want to type a severity into the filter box already on the page and have it narrow the list to targets at that severity, so that finding the handful of targets that actually need me does not require a second control to learn.*

Story 5.8 AC4 asks for filter by severity on the dashboard and it is still the one facet of that AC nothing today covers: Story 5.22's checkbox panel would have covered it but was superseded, and Story 5.23's watchboard replaced it with one substring match over name, URL and labels (`filterTargets`/`targetHaystack`, `internal/httpapi/watchboard.go`, mirrored client-side by `haystack()`/`apply()` in `internal/httpapi/static/watch.js`) — deliberately narrower, on explicit request in that session (AGENTS.md §1), and severity was the facet named as dropped. The watchboard already renders the value this story needs: `targetRow.Severity` (`internal/httpapi/ui.go`) is the worst `diff.Severity` across a target's series, always populated (never empty — it defaults to `SeverityInfo`), and it is already the text and colour of the always-visible rail badge (`watch-sev sev-{{$t.Severity}}`, `internal/httpapi/templates/dashboard.html`). Nothing about severity needs to be computed for this story, and — on explicit direction — nothing new needs to be added to the page to filter by it either: the one filter box the board already has is where it goes.

The mechanism is to add the target's severity word to the same haystack the filter already searches, not to add a second control next to it. That means a reader who wants "everything critical" types `critical`; a reader who wants "critical, in prod" types `critical prod`. The second case is new: today's match is a single contiguous substring test (`strings.Contains`), so a two-word query only ever matched if that exact phrase appeared verbatim somewhere in the haystack, which name/URL/label text rarely does. Folding severity into the same field without changing that would make combining it with anything else undiscoverable, so this story also changes the match itself, from one substring to a per-word AND — a superset of today's behaviour for the single-word queries every existing filter test already exercises.

- AC1: `targetHaystack` (`internal/httpapi/watchboard.go`) appends the target's worst-severity word — `targetRow.Severity`, the same value the rail badge already shows — to the string it builds from name, URL and labels. `haystack()` (`internal/httpapi/static/watch.js`) does the same client-side, reading the value from a new `data-severity` attribute on `.watch-row` (the row does not carry one today — only the per-mode scan tile does, at line 89 of `dashboard.html` — so this adds it at the row level the filter actually reads).
- AC2: `filterTargets` and `apply()` change from one contiguous substring match to a per-word AND: the typed filter is split on whitespace into tokens, and a target matches when every token is a substring of its haystack, independently — not necessarily contiguous, and not necessarily in the words' typed order. A single-word query behaves exactly as it does today; this only changes what a multi-word query can find, which is what lets `critical prod` mean "critical severity, and prod" without a second control.
- AC3: The five words matched are exactly `diff.Severity`'s values (info, low, medium, high, critical) — the same words already printed on the rail badge — so what a reader sees on a tile is exactly what they can type to find more like it. No new vocabulary, syntax, or prefix (no `severity:` or similar) is introduced; a severity word is just one more token the haystack happens to contain.
- AC4: No new control, cookie, or template helper is introduced. The existing `wsaw_filter` cookie, `watchFilter`/`filterTargets` (server) and `apply()` (client) carry the whole feature unchanged in shape. The `filterSeverities` template function left over from the superseded Story 5.22 (`internal/httpapi/ui.go`) stays unused — it enumerates the values for a checkbox panel this story deliberately does not add; removing it is separate housekeeping, not part of this story.
- AC5: The filter input's placeholder (`placeholder="filter target, url or env"`, `internal/httpapi/templates/dashboard.html`) is updated to mention severity, so the capability is discoverable without reading documentation — typing a severity word works today for name/URL/label substrings that happen to match one, and this story is what makes it a documented, intentional behaviour rather than an accident of the haystack.
- AC6: "showing N/M" and the empty state (`{{if not .Targets}}`) behave exactly as they do today for any filter text — a severity word, alone or combined with other tokens, that matches nothing is the same honest empty state a name filter matching nothing already is.
- AC7: The JSON API (`GET /api/v1/targets`) is unaffected: this remains a display preference of the HTML dashboard (Tenet 16), unchanged from today.
- AC8: Every no-JavaScript guarantee the filter already has (Story 5.23 AC11) is preserved: the per-word AND matching runs identically in the server-side fallback, so a reader with script disabled gets the same result a scripted reader would for the same typed query.
- AC9: Regression coverage: a severity word alone narrowing to that severity, a severity word combined with a name/label/URL token (AND across tokens, not necessarily contiguous or in order), a query that matches nothing, a single-word query behaving identically to today's contiguous-substring match, a multi-word query where none of the words is a severity still finding what it would have found under the per-word AND (i.e. token order does not matter), and client-side and server-side matching agreeing on the same input.

### Story 5.25 — The scan tile states its own cadence

*As an Operator scanning the watchboard, I want a tile's headline to be the figures that differ between renders — not the age, and not the consent mode repeated on every row — with the age readable against the cadence it is supposed to keep, so that "3h old" tells me something instead of needing me to remember whether this series runs hourly or daily.*

The tile's headline was the age of the last scan, at 1.35rem, and the consent mode. Neither is what a reader compares: the mode is the same three words on every row in a group — eight targets across three modes wrote "none", "accept" and "reject" twenty-four times — and an age alone says nothing about whether it is normal or a problem, since `3h old` is fine for a six-hourly series and overdue for a thirty-minute one, and the list did not say which. This folds together a design handoff applied and then iterated on directly in this repository (`pr-6a`, design option 6a on `design_handoff_target_tile`'s `Targets.dc.html`, following Story 5.23's own `design_handoff_watchboard`/`design_handoff_target_tile`) rather than written up first — flagged per AGENTS.md §1, and written up here after the fact. Applying the patch surfaced three defects fixed before this story shipped, kept here only as context for the regression tests they produced: a copy-paste artifact in the applied template rendered as literal page text; the cycle bar's percentage was computed against the daemon's raw schedule interval (`NextRun` minus `LastRun`) rather than against the same two numbers written above the bar, so a retried or failed scan could show a bar that disagreed with its own labels; and the bar's fill, an inline `style="width"` attribute, was silently dropped by the page's `unsafe-inline`-forbidding CSP (Story 5.23 AC11), so every tile's bar rendered full regardless of its actual percentage.

- AC1: A scan tile's headline and hit area is the figures that differ between renders — `N req · N 3p · N pre` — replacing the age as both the largest text on the tile and the element the tile's stretched `::after` hit area attaches to (`.watch-scan`, `internal/httpapi/templates/dashboard.html`, `internal/httpapi/static/wsaw.css`). The tile stays fully clickable, and "history" / "Scan now" still sit above it (Story 5.23 AC9, AC10, unchanged).
- AC2: The consent mode moves off the tile entirely and becomes a column header, stated once per group instead of once per tile (`envGroup.ModeColumns`, `internal/httpapi/watchboard.go`; `.watch-modes`/`.watch-mode-col`, `internal/httpapi/templates/dashboard.html`). A folded "none / reject" row (Story 5.23's `modeRow` fold) spans both of the columns it covers and keeps its own inline mode label (`.watch-tile-mode`), since the header above it no longer names it alone.
- AC3: The age keeps a place on the tile but not the tile's largest type: `AgeLabel` and `NextLabel` (`internal/httpapi/watchboard_cycle.go`) sit left- and right-aligned on a smaller time line — the age of the shown scan, and the time to the next one the daemon has scheduled. A run in flight replaces both with "scanning now" / "in flight"; a `NextRun` already in the past reads "next scan overdue" in the warning colour; a series the daemon's schedule says nothing about reads "not scheduled" rather than guessing at a cadence.
- AC4: A bar under the time line (`.watch-cycle`/`.watch-cycle-fill`) is a second, visual reading of the same two numbers already written above it — never the only carrier of the fact, per Story 5.11's reasoning about colour applied here to a fill level. Its total is the shown scan's age plus the time to the next run (`NextRun` minus the same start `AgeLabel` measures from, `modeRow.interval`) — not the daemon's raw schedule interval — so the bar's fill always agrees with the two labels beside it even when a failed or retried scan has moved the schedule's own `LastRun` out of step with the result actually shown. It reads full for a scan in flight or a `NextRun` already due, and is drawn only for a series with a schedule entry at all (`modeRow.Scheduled`).
- AC5: The baseline-deviation and pre-consent markings Story 5.23 AC7 already established move onto the merged headline anchor as `is-deviant` and `is-pre` classes (`.watch-scan.is-deviant`, `.watch-scan.is-pre`, both styled with `var(--warn)`) in place of the separate `watch-req`/`watch-pre-n` spans the previous tile shape used. The rules themselves are unchanged: a request count is marked only when the shown scan deviates from an actual approved baseline (not the no-baseline previous-scan fallback), and the pre-consent figure is always printed, with only a non-zero count colouring it.
- AC6: The cycle bar's fill is set from a `data-percent` attribute by a same-origin script (`internal/httpapi/static/cycle.js`) writing to the element's CSSOM, never from an inline `style` attribute — the page's CSP forbids `unsafe-inline` (Story 5.23 AC11) and would otherwise drop the width silently rather than failing loudly, which is exactly the defect this story's own patch shipped with and this AC exists to prevent from recurring. Without JavaScript the bar renders empty (its CSS default), not full: a lost second reading, not a false one, since the age and next-run text beside it already carries the same information in full.
- AC7: No API or data-model change. `TargetView` and `SeriesView` (`internal/httpapi/api.go`) are untouched; `modeRow`'s new `NextRun`, `LastRun` and `Folded` fields (`internal/httpapi/ui.go`) and every method in `watchboard_cycle.go` are display-only, populated by `attachSchedule`, which runs before `shapeWatchboard` so the schedule reaches the board's own groups (which hold copies of each row) and not only the Schedule table at the page's foot.
- AC8: Regression coverage: `AgeLabel` for a scanned, a never-scanned and a running series; `NextLabel` and `Overdue` for a scheduled, an unscheduled and a past-due `NextRun`; `CyclePercent` at the start, middle, running and overdue states, with a zero-length interval returning 0 rather than dividing by it, and confirmed to track age against next-run rather than the schedule's raw `NextRun` minus `LastRun` even when the two disagree; `attachSchedule` filling the row a schedule entry names and leaving others zero, and running before `shapeWatchboard`; `ModeColumns` for a group with a folded row and for a group with one mode; the pre-consent and baseline-deviation markings against the new merged anchor (Story 5.23 AC12's own two cases, re-pointed at the new markup); and that the cycle bar carries no inline `style` attribute anywhere the template renders it (Story 5.23 AC11's own guarantee, asserted directly against this element).

### Story 5.26 — "Scan now" leaves the reader on the watchboard

*As an Operator working through the watchboard, I want pressing "Scan now" on a tile to start the scan and leave me where I was — same board, same filter, same scroll position — so that kicking off three targets in a row does not mean three trips back from three different target pages.*

Today the button is a form posting to `POST /rescan/{target}/{mode}` (`handleUIRescan`, `internal/httpapi/ui.go`), and the handler's `dest` is always `/targets/{target}/{mode}` — the target's own history page — whatever page the button was pressed on. That is the right destination when the button is pressed on the history page itself, and the wrong one on the board: the reader is navigated off a page they were reading, their typed filter and scroll position are gone, and returning costs a back-navigation that re-renders the whole board. On a board where the common gesture is "start these four and watch them", the navigation is the cost of the feature. Nothing about who may scan changes here: `AllowScan` (`AllowAdHocScan && !ReadOnly`), the CSRF field, and the server's own "already running" guard are untouched, and "Scan now" remains a real `<form>` with script off (Story 5.23 AC11).

- AC1: The rescan form carries the page it was submitted from, as a hidden `from` field rendered by the template that draws the button (`internal/httpapi/templates/dashboard.html` posts the board's own path; the series page keeps posting its own). `handleUIRescan` redirects to that value instead of to a hard-coded target page, so the no-JavaScript path's 303 lands the reader back on the board they pressed the button on rather than on the target's history page.
- AC2: The `from` value is not trusted: it is reduced by the existing `safeLocal` sanitiser and then accepted only if it is one of the interface's own known UI paths (the board, `/`, or a `/targets/{name}/{mode}` history page); anything else falls back to today's `/targets/{target}/{mode}` destination rather than redirecting somewhere a request chose. A missing `from` — an old bookmark, a replayed form — behaves exactly as today. No open redirect is reachable through this field (Tenet 9).
- AC3: Both flash messages the handler can produce follow the reader to the same page: "Scan started. It appears as pending below and this page refreshes until it finishes." on the success path, and the existing "A scan of this target and consent mode is already running." on the guarded path. On the board the "below" wording is wrong for the destination, so the success message becomes one sentence that is true on both pages ("Scan started for {target} ({mode}). It appears as pending until it finishes."), naming the target because a board shows many and the reader needs to know which one they just started.
- AC4: With JavaScript available, pressing the button does not navigate at all: a small same-origin script (`internal/httpapi/static/scan.js`, loaded from `layout.html` alongside the board's other scripts) intercepts the form's `submit`, posts the same body — CSRF field included — with `fetch` and `credentials: 'same-origin'` (permitted by the page's `connect-src 'self'`, Story 5.23 AC11), and cancels the default submission. Typed filter text, the "hosts only" state, collapsed groups and scroll position all survive because nothing reloads.
- AC5: The script's result is shown in the page's own flash slot (`.flash`, `internal/httpapi/templates/layout.html`), not in a new alert element invented for scripted presses, so a press that reloads (AC1) and a press that does not read the same. The slot today is rendered only when the server has a message; it becomes a container that is always present and empty, marked `aria-live="polite"`, so a message written into it after load is announced rather than silently appearing. Focus stays on the button that was pressed — a scripted action that moves focus to the top of the document is worse than the navigation it replaces.
- AC6: The pressed tile shows the scan as pending immediately, client-side, without waiting for the next auto-refresh: the same `pending` term the server renders for a running series (`.Series.Running`, `internal/httpapi/templates/dashboard.html`) is added to that tile, and the button is disabled while the request is in flight so a second press cannot queue a second scan from the client. The server's own duplicate guard (`runningFor`, `handleUIRescan`) remains the authority and is unchanged — the disabled button is a courtesy, not the protection.
- AC7: Because the page no longer reloads after a scan starts, it will not pick up the server's "a scan is running, refresh every 10s" fallback (`refreshWhileRunningInterval`, `internal/httpapi/refresh.go`) on its own. The script therefore asks `refresh.js` to re-arm at that interval via a single exported hook rather than each script owning its own timer; a viewer who has explicitly chosen "off" keeps "off" — a button press is not consent to start reloading a wall display.
- AC8: Any failure keeps the reader informed and the board honest rather than silently swallowing the press (Tenet 5): a non-2xx response or a rejected `fetch` (offline, extension, blocked request) re-enables the button, drops the optimistic pending mark, and shows the server's own error text where it can be read — and a `fetch` that is unavailable or throws before it reaches the server falls through to the plain form submission (AC1), so the worst case is the navigation we have today, never a press that appears to have worked and did not.
- AC9: No API, data-model or permission change. `POST /api/v1/scan/{target}/{mode}` (`internal/httpapi/api.go`) is untouched and stays synchronous; `/rescan` stays the browser-only, CSRF-protected, detached-goroutine path with its existing `adHocScanBudget`; read-only mode and `AllowAdHocScan=false` still render no button at all and still reject the POST.
- AC10: Regression coverage: a POST with `from` set to the board redirecting to the board and one without it redirecting to the target page as today; an off-origin, a scheme-relative and an unknown-path `from` all falling back to the target page; the "already running" guard returning to the same page the press came from; the success flash naming the target and mode; the form rendering the hidden field on the board and not rendering the button at all under read-only or disabled ad-hoc scanning; that `scan.js` introduces no inline `style` or `script` attribute and no third-party request (Story 5.23 AC11); and that a script-off submission of the same form still starts exactly one scan.

---

# Epic 6 — Operations, Packaging & Distribution

Make wsaw installable, operable, and debuggable on the target platforms.

**Goal:** a single static Go binary plus a documented Chrome dependency, deployable as a systemd unit, a launchd agent, or a container image.

**Scope:** build and release pipeline, cross-compilation for linux/darwin × amd64/arm64, systemd and launchd unit files, container image with Chrome bundled, Chrome discovery and version check at startup, self-observability (structured logs, self-metrics), sandbox and privilege model, CI (lint, vet, test, govulncheck, license check, SBOM).

**Out of scope:** package-manager distribution (brew/apt) beyond a stretch goal.

### Story 6.1 — Single-binary distribution (partly implemented: release artifacts are not signed and notarization is undocumented, AC3 and AC4)

*As an Operator, I want one binary per platform so that installation is trivial.*

- AC1: Release artifacts for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`.
- AC2: Reproducible builds with version, commit, and build date embedded and printed by `wsaw version`.
- AC3: Checksums and signed release artifacts; an SBOM is generated in CI and published per release.
- AC4: macOS binaries are notarization-ready (documented, even if signing is deferred).

### Story 6.2 — Service integration

*As an Operator, I want native service management so that wsaw starts with the host.*

- AC1: A hardened systemd unit is shipped (non-root user, `ProtectSystem`, `PrivateTmp`, restart policy).
- AC2: A launchd plist is shipped for macOS.
- AC3: Documented directory layout for config, state, and output on both platforms (XDG on Linux, `~/Library/Application Support` on macOS).

### Story 6.3 — Container image (partly implemented: deploy/chromium-seccomp.json is referenced by the Dockerfile but not shipped, AC2)

*As an Operator, I want a container image with Chrome included so that I avoid host dependencies.*

- AC1: Multi-arch image (amd64, arm64) with a pinned Chrome/Chromium version.
- AC2: Runs as a non-root user; documented required kernel capabilities or seccomp profile for the Chrome sandbox.
- AC3: The Chrome sandbox stays enabled; `--no-sandbox` is never the documented default.

### Story 6.4 — Structured logging

*As an Operator, I want machine-parseable logs so that they fit my aggregation stack.*

- AC1: `log/slog` with JSON and text handlers, level configurable.
- AC2: Every log line for a scan carries target, consent mode, and a scan ID for correlation.
- AC3: URLs and headers are redacted according to the secrets policy (Story 3.6).

### Story 6.5 — Self-observability

*As an Operator, I want metrics about wsaw itself so that I can alert on the watcher failing.*

- AC1: Metrics: scans started/succeeded/failed by reason, scan duration histogram, requests captured per scan, browser restarts, consent outcome counts, queue depth, last successful scan timestamp per target, notification failures.
- AC2: Exposed via a Prometheus endpoint and/or OTLP export, both optional.
- AC3: Browser restarts are attributable to the target that triggered them.
- AC4: A readiness endpoint that reports "Chrome usable" and "config loaded".

### Story 6.6 — Robustness against hostile pages

*As an Operator, I want a scan of a broken or hostile page to be contained so that the daemon stays healthy.*

- AC1: Per-scan caps on requests, total transferred bytes, and wall-clock time; exceeding a cap ends the scan with a recorded reason, not a crash.
- AC2: Infinite redirect loops, endless polling, and dialog spam (`alert`/`confirm`/`beforeunload`) are handled without hanging.
- AC3: Downloads are suppressed; navigation to non-HTTP schemes is blocked.
- AC4: A panic in a single scan is recovered and isolated; the daemon keeps running and records the failure.

### Story 6.7 — Diagnostics mode

*As a Developer, I want to debug a single target quickly so that rule authoring is practical.*

- AC1: `wsaw debug <url>` runs one scan with verbose CDP logging, optional headful mode, screenshots, and a dump of the DOM at consent-interaction time.
- AC2: A `wsaw rules test` command validates a consent rule against a live URL and reports which selectors matched.

### Story 6.8 — Soak testing (partly implemented: no file-descriptor or child-process assertions, AC2)

*As an Operator, I want long-run stability proven so that I can trust a 30-day deployment.*

- AC1: A soak test exercises continuous scanning over an extended run against local fixture sites.
- AC2: It asserts no growth in RSS, goroutines, file descriptors, child processes, or temp directories beyond documented bounds.
- AC3: It runs on a schedule, separate from the regular CI pipeline, and reports results independently.

### Story 6.9 — Readable console logs when a human is watching

*As an Operator, I want log output to be easy to read when I run wsaw in a terminal, without giving up machine-readable logs where something is collecting them.*

Structured JSON (Story 6.4) is right for an aggregator and hostile to a person running `wsaw scan` by hand. Rather than making that a flag people must remember, wsaw should pick the sensible default from where its output is going, and let either choice be forced.

- AC1: A `pretty` log format renders one line per record: timestamp, level, message, then the remaining attributes. It is aligned and easy to scan, and colour is used to distinguish levels and to make warnings and errors stand out.
- AC2: The default format is chosen by detecting whether the log output is an interactive terminal: `pretty` when it is, `json` when it is not. A piped, redirected, containerised, or service-managed run therefore keeps machine-readable output with no configuration.
- AC3: The choice can be forced in both directions, by configuration and by flag (`--log-format pretty|json|auto`), and `auto` is the documented default rather than a hidden behaviour.
- AC4: Colour is suppressed when the terminal does not support it, when `NO_COLOR` is set, when `TERM=dumb`, and whenever the format is `json`. Colour is never the only carrier of meaning — the level is always present as text.
- AC5: Pretty output loses no information: every attribute of the record is rendered, and the scan-scoped fields (`scan_id`, `target`, `consent_mode`) stay visible so a line can still be tied to a scan.
- AC6: Secret redaction and scrubbing apply identically in both formats. A format intended for humans must not become a way for a credential to reach a screen or a scrollback buffer.
- AC7: Multi-line values, such as an error containing a newline, are rendered so that one record stays visually one record and cannot be mistaken for several.
- AC8: The chosen format is decided once at startup and recorded in the daemon's own startup line, so an operator can see which mode is active.

### Story 6.10 — Run wsaw in Kubernetes (not implemented)

*As an Operator, I want to run wsaw on the cluster my other services already run on, without discovering its single-node assumptions the hard way.*

wsaw is deliberately single-node (Tenet 18): its store is one embedded file holding an exclusive lock, and a baseline is only meaningful against one history. Kubernetes invites the opposite — replicas, rolling updates, rescheduling — so the manifests have to make the constraint explicit rather than let a cluster discover it by corrupting a store or deadlocking on a lock.

Kubernetes also removes the mechanism Story 1.8 relies on. There is no container runtime socket to launch a sibling browser through, and mounting one would hand a pod that renders hostile pages the ability to start privileged containers. In-cluster the browser therefore runs inside the pod, and the pod is the boundary.

- AC1: A worked example is shipped and documented: namespace, ConfigMap, Secret, PersistentVolumeClaim, Deployment, Service, and the RBAC needed (which is none — wsaw does not talk to the API server, and the manifests must not grant it a token it has no use for).
- AC2: **Exactly one replica, enforced rather than advised.** The Deployment sets `replicas: 1` and `strategy: Recreate`, because the default rolling update would briefly run two pods and the second would fail on the store's file lock. The documentation states plainly that scaling out is not supported and why.
- AC3: The store lives on a `ReadWriteOnce` PersistentVolumeClaim, with guidance on sizing it from the retention settings, since a volume that fills is a watcher that stops watching.
- AC4: The browser runs in the pod. Runtime detection resolves to `local` in-cluster because no container runtime is present, and that is documented as the intended outcome rather than left as an accident of `PATH`. Chrome's own sandbox is off, the pod is the boundary, and the result records it exactly as the containerised path does.
- AC5: `/dev/shm` is sized for the browser. A container's default 64 MiB makes Chrome crash on real pages, so the pod mounts a memory-backed volume, or the browser is told not to use shared memory — and the manifest says which and why.
- AC6: Concurrency and pool size are derived from the container's own CPU and memory limits, not from the node's. `runtime.NumCPU` reports the host's processors, so an unconfigured pod with a one-core limit on a large node would size itself for the node and be throttled or killed. Where the limits cannot be read, the manifests set the values explicitly.
- AC7: `terminationGracePeriodSeconds` exceeds wsaw's own shutdown grace with margin. Otherwise the kubelet sends SIGKILL while a scan is still finishing, which loses the result and leaks the browser it was driving.
- AC8: Liveness uses the health endpoint and readiness uses the readiness endpoint, with thresholds that survive a long scan. A probe must never restart a pod that is merely busy, and readiness must not flap because one scan failed — the distinction between "alive" and "able to scan" already exists and the probes must use the right one for each.
- AC9: Configuration comes from a ConfigMap and credentials from a Secret, using the `${env:…}` and `${file:…}` references wsaw already supports. The volume is mounted so that a ConfigMap update reaches the container, and the documentation says how to trigger the reload and that a `subPath` mount will not update.
- AC10: The API is reachable through a Service, which means binding beyond loopback, which wsaw's own validation requires a token for. The manifests therefore ship a Secret-backed token by default rather than leaving an unauthenticated endpoint exposed inside the cluster.
- AC11: **Egress is restricted, and internal addresses are refused.** A pod that fetches operator-supplied URLs is a request-forgery engine pointed at the cluster: it can reach the metadata endpoint, in-cluster services, and the node's own network. A NetworkPolicy is shipped, and the private and link-local deny list is on by default in the example configuration rather than being an option someone might not find.
- AC12: The pod runs unprivileged: non-root, no added capabilities, a read-only root filesystem with writable volumes only where the browser needs them, and a seccomp profile. Where the browser needs something the default profile forbids, the manifest grants that specific thing and says why.
- AC13: Metrics are exposed for scraping, and the alert that matters — a target whose last successful scan has aged out — ships as a rule rather than being left for the operator to work out.
- AC14: A `CronJob` shape is documented for `wsaw scan --once` as a gate, with `concurrencyPolicy: Forbid`, since two jobs sharing one volume would contend for the same lock.
- AC15: The manifests are validated in CI against a schema, so a broken example cannot ship. Applying them to a real cluster is not required to run the test suite.

---

# Epic 7 — Containerised End-to-End Testing

Prove that wsaw works as a deployed system — its own container, a server database in another, and a real consent banner in a third — rather than only as a set of packages that pass their unit tests.

**Goal:** one command brings up a stack of wsaw, a database, and a fixture website running [Klaro](https://github.com/kiprotect/klaro), runs a scan against it, and asserts on what landed in the database. The same stack runs under Docker Compose and as a Podman pod, and runs in CI without a live third-party website.

**Why this is worth its own epic.** Everything wsaw's fast test suite proves, it proves in one process. The things that break a deployment are the things that only exist between processes: a browser in a container cannot reach the host's loopback (Story 1.8, AC10), a database connection is a network that can drop, a consent banner behaves differently when it is served over a real origin than when it is a fixture string in a Go test. Those are exactly the failures an operator meets first and the current suite cannot see at all.

**Klaro is the fixture on purpose.** It is open source, self-hostable, has no tenant to register and no rate limit to respect, and wsaw already ships a rule for it (`internal/consent/rules/vendors.yaml`). So the fixture exercises the real rule pack against the real CMP rather than a synthetic banner written to match the rule — which would only ever prove that the rule matches itself. It is also a genuine CMP: it blocks scripts before consent and releases them after, so "fired despite reject" is a thing the fixture can actually do.

**Scope:** a Klaro fixture image, a Compose stack, a Podman pod, the end-to-end assertions, and the CI wiring.

**Out of scope:** the store's dialect seam and its Postgres and MySQL drivers. That is a storage decision and lives in Epic 4 (Story 4.7); this epic consumes it. Also out of scope: replacing the fast suite. These tests are slow, need a container runtime, and must never become the thing that has to pass before a developer can commit. Also out of scope: making wsaw multi-node. A server database removes the store's file lock; it does not make two wsaw instances agree about a baseline (Tenet 18 stands until a story retires it).

**Prerequisite worth stating plainly:** wsaw's store is SQLite today, and the SQL it emits is SQLite's — `strict` tables, `pragma user_version`, `rowid`, `json_extract`, `?` placeholders. There is no Postgres or MySQL support to test yet. That work is Story 4.7, where it belongs — it is a storage decision, not a testing one — and Stories 7.4 and 7.5 cannot start before it lands.

### Story 7.1 — A Klaro fixture website

*As a Developer, I want a self-hosted site with a real consent banner so that end-to-end tests exercise the shipped consent rules without depending on anyone else's website.*

- AC1: A container image serves a small static site with Klaro installed and configured, pinned to a specific Klaro version. An unpinned CMP would make the test suite fail on someone else's release schedule.
- AC2: The page loads assets from a **second** origin in the stack, so first-party and third-party attribution is exercised rather than assumed. A single-origin fixture cannot tell a working classifier from a broken one.
- AC3: Klaro is configured so that at least one third-party asset is blocked before consent and loaded after it, and at least one loads unconditionally. The unconditional one is the point: it is what "contacted before any consent decision" looks like, and the test asserts wsaw reports it.
- AC4: The fixture is deterministic. No live third parties, no clock-dependent content, no randomised URLs — two scans of the unchanged fixture must produce an empty diff, which is what makes a non-empty diff meaningful (Tenet 6).
- AC5: The fixture can be changed by the test between scans in a controlled way — one extra third-party host, one changed script body — so the diff engine and the severity rules are exercised on real captures rather than on hand-built results.
- AC6: The image is built from source in the repository, and its build is part of the stack rather than a step someone has to remember.

### Story 7.2 — A Docker Compose stack

*As a Developer, I want `docker compose up` to bring up the whole system so that reproducing a deployment problem takes a minute rather than an afternoon.*

- AC1: A Compose file defines four services: wsaw, the database, the Klaro fixture site, and the third-party origin the fixture loads from.
- AC2: Services address each other by name on a Compose network. This is what makes the stack work at all: a browser inside the wsaw container cannot reach the host's loopback, and Story 1.8 AC10 makes wsaw refuse such a target with an explanation rather than silently scan its own container.
- AC3: The browser runs **inside** the wsaw container, not as a sibling. Mounting the runtime socket would give a container that renders hostile pages the ability to start privileged containers, which is a worse trade than losing sibling-container isolation.
- AC4: wsaw waits for the database to be *ready*, not merely started, through a health check. A race that usually passes is worse than one that always fails.
- AC5: The database service is parameterised so the same file serves Postgres and MySQL, through a profile or an override file. Two nearly-identical Compose files would drift.
- AC6: `/dev/shm` is sized for the browser. The default 64 MiB makes Chrome crash on real pages, and the fix belongs in the shipped file rather than in a troubleshooting section.
- AC7: The stack runs unprivileged and holds no real credentials. The passwords in it are obviously fixtures, and the file says so, so that nobody copies it into production and inherits them.
- AC8: Bringing the stack down removes its volumes, so a stale database from an earlier run cannot make a test pass or fail for reasons that are not in the code.

### Story 7.3 — A Podman pod

*As an Operator, I want the same stack as a Podman pod so that the rootless runtime we prefer is a first-class path and not an afterthought.*

Story 1.8 already prefers Podman where a runtime is being detected. The deployment story should not then be Docker-only.

- AC1: The stack is expressible as a Podman pod, shipped either as a Kubernetes YAML for `podman kube play` or as a script that creates the pod and its containers — whichever the repository can also validate in CI.
- AC2: Containers in the pod share a network namespace and therefore reach each other on `localhost`. This is a genuine difference from the Compose stack, not a translation of it, and the fixture URLs differ accordingly — a port collision between services is a real failure mode here and the ports are assigned to avoid it.
- AC3: It runs rootless. Where rootless Podman needs something extra for Chrome — user namespaces, a seccomp adjustment — it is in the shipped definition with the reason, not left for the operator to rediscover.
- AC4: The same assertions run against the pod as against Compose. Two stacks that are tested differently are two systems.
- AC5: If `podman kube play` is used, the manifest is close enough to Story 6.10's Kubernetes manifests to be worth comparing, and the documentation says where they differ and why.

### Story 7.4 — End-to-end test against Postgres

*As a Compliance Officer, I want proof that a wsaw deployment backed by Postgres records what it observed, so that the evidence chain holds outside a developer's laptop.*

- AC1: The test brings up the stack with Postgres, scans the Klaro fixture in all three consent modes, and asserts on the rows the database actually holds — read back through SQL, not through wsaw's own API. Asking the writer whether it wrote correctly proves less.
- AC2: The consent assertions are the substance: in `none` the banner is present and untouched; in `reject` the banner is dismissed and the blocked third party stays absent; in `accept` it appears. Klaro's own state is checked too, so a scan that clicked nothing and a scan that clicked correctly cannot both pass.
- AC3: The third party the fixture loads unconditionally is reported as contacted before any consent decision, in every mode. This is the product's headline finding, and it is asserted end-to-end rather than in a unit test on a hand-built result.
- AC4: Two scans of the unchanged fixture produce an empty diff; a scan after the fixture changes produces exactly the expected changes with the expected severities. Diff noise is a defect of equal weight to a missed asset (Tenet 6).
- AC5: The schema is created by wsaw's own migrations against an empty database, and running the daemon twice against the same database is a no-op the second time. A migration that only works on a fresh schema is a migration that fails in production.
- AC6: The database is restarted mid-run and the daemon survives it: the scan in flight is recorded as failed, and the next scan reconnects and succeeds (Story 4.7, AC7).
- AC7: Baseline approval and its audit entry are asserted to be atomic under Postgres, not merely assumed to have carried over from SQLite.
- AC8: The test cleans up after itself — containers, volumes, networks — whether it passes or fails, and reports the container logs of every service on failure. A red end-to-end test that says only "assertion failed" costs more time than it saves.

### Story 7.5 — End-to-end test against MySQL

*As an Operator running MySQL rather than Postgres, I want the same guarantees, so that the choice of database is a deployment decision and not a support tier.*

- AC1: The same assertions as Story 7.4 run against MySQL. They are one test body parameterised by dialect, not two test files that will drift.
- AC2: The dialect differences MySQL forces are exercised rather than hidden: its upsert form, its JSON accessor, its identifier and reserved-word rules, and the absence of `rowid` in the retention delete.
- AC3: Character set and collation are pinned to `utf8mb4` and asserted. Captured URLs and page-controlled strings contain everything Unicode has; a database that silently truncates at a four-byte character would corrupt evidence quietly, which is the worst way to lose it.
- AC4: `sql_mode` is strict, so a value that does not fit is an error rather than a silent truncation. A store that quietly shortens a URL has changed the finding.
- AC5: Timestamps survive a round trip at the precision wsaw relies on for ordering. MySQL's default `DATETIME` drops fractional seconds, and "the scan before this one" is decided by that ordering.
- AC6: Where MySQL cannot do something the same way, the difference is recorded in the dialect documentation rather than worked around silently in the test.

### Story 7.6 — Run the stack in CI, and keep it out of the fast suite

*As a Developer, I want the end-to-end tests to run automatically without slowing down the loop I work in.*

- AC1: The end-to-end tests are behind a build tag or an explicit opt-in, so `go test ./...` on a machine with no container runtime still passes and still means something.
- AC2: They run in CI on a schedule and before a release, not on every push. A twenty-minute gate on every commit is a gate people learn to route around.
- AC3: Their failure is a release blocker. A test that can fail without consequence is documentation.
- AC4: Images are pinned by digest and cached between runs, so the tests do not fail because an upstream base image moved, and do not take ten minutes pulling.
- AC5: On failure, CI keeps the artefacts that make the failure diagnosable: every service's logs, the stored result documents, and the diff that was produced.
- AC6: The whole stack comes up and the assertions complete within a documented time budget, and exceeding it fails the run. An end-to-end suite with no budget grows until it is skipped.

---

# Epic 8 — Result Documents in Object Storage

Move the scan result's JSON document out of the database and into artifact storage, reached through `gocloud.dev/blob`, so the database holds an index and a reference rather than the payload.

Story 4.6 put the result document in a column and kept screenshots and stored bodies as content-addressed files beside it, on the record that artifacts do not belong in a row and that keeping them out "leaves the door open to object storage later" (Story 4.6, AC9). That door is what this epic walks through, and it takes the document with it. A scan of a busy site is a multi-megabyte JSON document; a row that carries it makes every backup, every replication stream, and every query for a summary pay for evidence nobody asked for. The document is the product's interface (Tenet 16), not a queryable structure — nothing in wsaw filters on its contents, so nothing is lost by storing it where large immutable blobs belong.

The second half is that "where artifacts live" stops being "a directory". One seam — a bucket — serves the local directory that is the default today, and an S3, GCS or Azure bucket for a deployment that already has one. That is the argument Story 4.7 made for the database, applied to evidence: an organisation's existing operational machinery should be usable, without wsaw growing a second implementation of everything.

**Goal:** every scan's JSON document is written to the artifact bucket and referenced by a row; the default deployment still writes to the `artifacts` directory on local disk and needs no configuration; an operator who wants object storage changes one URL.

**Scope:** a bucket abstraction over `gocloud.dev/blob`, the document-as-artifact write and read path, the index columns that make listing possible without fetching documents, a forward-only migration of existing stores, retention that deletes what it stops referencing, bucket configuration and credentials, serving artifacts from a bucket over the HTTP API and share links, the dependency budget this costs, for a deployment that would rather run no database at all a store that keeps its index in the bucket too (Story 8.10), and a rebuild of the index from the bucket for when it is lost (Story 8.11).

**Out of scope:** for the SQL-backed stores, moving baselines, audit entries or change events out of the database — they are small, they are queried, and they stay rows; Story 8.10 is the deliberate exception, where there is no database to keep them in. Multi-node operation (Story 4.7, AC8 still holds). Encryption of artifacts at rest beyond what the bucket itself provides.

### Story 8.1 — Reach artifacts through a bucket, not a directory (not implemented)

*As a Developer, I want artifact access to go through one bucket abstraction so that where evidence lives becomes a URL rather than a set of filesystem calls spread through the store.*

Today `PutArtifact`, `GetArtifact` and `StatArtifact` call `os.MkdirAll`, `os.CreateTemp`, `os.Rename`, `os.ReadFile` and `os.Stat` directly, and defend against traversal with `filepath.Rel`. Those are the right calls for a directory and the wrong shape for anything else. `gocloud.dev/blob` is the seam that makes the directory one implementation instead of the only one — chosen over hand-rolled per-provider clients because the provider list, the retry behaviour, and the credential chains are exactly the part nobody wants to maintain three times.

- AC1: All artifact reads, writes, existence checks and deletions go through a single bucket type backed by `gocloud.dev/blob`. No filesystem call for artifacts survives outside it, and no provider-specific type appears outside the store package — the rule Story 4.6, AC1 set for the database driver, applied to storage.
- AC2: The default is a local file bucket rooted at the artifact directory, with the **same key layout as today** (`kind/sha256hex`). An existing installation's artifact directory is readable by the new code without moving a single file, and an operator who configures nothing sees no change.
- AC3: Artifacts stay content-addressed. Writing the same screenshot twice still costs one copy, and writing a key that already exists is a no-op rather than a rewrite — captured evidence is immutable (Tenet 4).
- AC4: A write is atomic from a reader's point of view. A crash, a cancelled context, or a failed upload never leaves a key a reader would treat as complete evidence; where the provider has no rename, the write completes in one operation or not at all.
- AC5: Keys are validated before use. References originate from stored results, which derive from page-controlled data, so a crafted reference must not escape the bucket's prefix or address anything outside it — the traversal defence `artifactPath` provides today has to survive the move (Tenet 9).
- AC6: Every operation takes a `context.Context` and is bounded by the store's configured timeout. A bucket is a network dependency for every provider but the local one, and an unbounded read is how a daemon stalls (Tenet 8).
- AC7: Reads and writes stream. A multi-megabyte document must not have to exist in memory in full on both sides of the call; the `[]byte` shape of `GetArtifact` is replaced or complemented by a reader, and the HTTP API serves from it (Story 8.7).
- AC8: Transient bucket failures are retried on the same policy as the database — `maxAttempts` and `retryBackoff` — and permanent ones (missing key, access denied) are not. Retries are logged and counted, as store retries already are.
- AC9: A missing artifact is still `ErrNotFound`, so every caller that distinguishes "pruned" from "broken" today keeps that distinction (Story 5.17, AC3).

### Story 8.2 — Store the result document as an artifact, reference it in the database (not implemented)

*As an Operator, I want scan documents kept in the bucket and referenced from the database so that my database holds an index rather than gigabytes of evidence.*

- AC1: `PutResult` writes the JSON document to the bucket under its own artifact kind (`result/`), content-addressed like every other artifact, and inserts a row carrying the reference plus the columns that index it.
- AC2: The `document` column is gone. A row carries the artifact reference, the document's byte size, and its digest — not the payload, and not a copy of it "for now". Two sources of truth for one document is the failure this story exists to avoid.
- AC3: The read paths — `GetResult`, `LatestResult`, `PreviousResult` — resolve the reference and return the same `*model.Result` they return today. Nothing outside the store learns that the document moved.
- AC4: The write order is bucket first, row second. An interrupted write leaves an unreferenced artifact, which is garbage the sweep collects (Story 8.5); the reverse order leaves a row pointing at nothing, which reads as a corrupt result.
- AC5: A row whose artifact is missing or undecodable is reported and skipped, not fatal. One lost document must not make a target's whole history unreadable (Story 4.6, AC8).
- AC6: The document's digest is verified on read, and a mismatch is reported as corruption rather than returned as evidence. Once the payload leaves the database's consistency guarantees, its integrity is checked where it is used.
- AC7: The transactional guarantees that exist today survive. Approving a baseline and writing its audit entry remain one transaction (Story 4.6, AC7), and a bucket write is never inside a database transaction, because it cannot roll back.
- AC8: The JSONL, HAR and Markdown outputs written to the output directory are unchanged. They are derived views of the same document (Tenet 16) and are not what this epic moves.

### Story 8.3 — List and summarise without fetching a single document (not implemented)

*As a Site Owner, I want the results list to stay fast so that browsing history does not become one object-storage request per row.*

`ListResults` reads every document and summarises it in Go — acceptable when the document is a column on a row already read, ruinous when each one is a separate network fetch. Listing fifty scans would become fifty round trips before the page renders. The summary is small, fixed, and already computed at write time.

- AC1: The fields a summary needs are materialised as columns on write: duration, termination, error, consent outcome and CMP, request count, third-party domain count, pre-consent domain count — alongside the existing target, consent mode, scan ID and start time.
- AC2: `ListResults` is answered entirely from the database. Listing N results performs zero bucket reads, asserted by a test that fails if the count is not zero.
- AC3: `HasResult` stays a row lookup and never touches the bucket, as it avoids reading the document today.
- AC4: The summary columns are produced by the same code that renders a summary now, so the two cannot disagree. Adding or changing a summary field later is a migration that recomputes, not a second definition.
- AC5: Recomputing every summary from the stored documents is available as an explicit operation (Story 8.11), so a bad derivation is repairable without rescanning. Derived data is disposable; the document is not (Tenet 4).

### Story 8.4 — Migrate stores that keep documents in rows (not implemented)

*As an Operator, I want my existing history preserved across the upgrade so that a change of storage layout is not a change of dataset.*

Story 4.6 could discard existing stores because bbolt held little that mattered yet. That is no longer true: a store holds months of evidence, baselines pinned to specific scans, and share links pointing at results. Discarding it would destroy exactly the compliance history the product exists to keep.

- AC1: A schema migration moves every document in the database to the bucket, writes back the reference, populates the summary columns from the document, and drops the column — forward-only, through the same numbered-migration mechanism every dialect already uses (Story 4.7, AC4).
- AC2: The migration is resumable and idempotent. Interrupting it and starting again finishes the job without duplicating artifacts (content addressing makes a repeated write free) and without skipping rows.
- AC3: It runs in bounded batches with progress logged, and does not hold one transaction open across the whole table. A store with a hundred thousand results must migrate without exhausting memory or blocking the database for minutes.
- AC4: A row that will not migrate — undecodable JSON, a bucket write that keeps failing — is reported with its scan ID and left in place rather than silently dropped, and the migration states how many such rows there are.
- AC5: Migrating needs a writable bucket, and an unreachable or read-only one fails at startup with an actionable message naming the URL, before any row is touched.
- AC6: A dry run reports what would move, how many objects and bytes, and to where, so an operator can size the bucket and the maintenance window before committing.
- AC7: The migration is verified for every dialect — SQLite, Postgres, MySQL — by a test that writes results in the old layout, migrates, and reads every result back byte-identically.
- AC8: The upgrade is documented in the README, with the one-way nature stated plainly: an older wsaw cannot read a migrated store, and the supported rollback is a database backup taken before the upgrade.

### Story 8.5 — Retention deletes what it stops referencing (not implemented)

*As an Operator, I want pruning to reclaim storage so that a bucket that now holds every document does not grow without bound.*

Retention deletes rows today and leaves artifacts behind — a tolerable leak while artifacts were opt-in screenshots and bodies, and not tolerable once every document is out there. The bucket becomes the largest thing wsaw owns, and nothing currently removes anything from it.

- AC1: Pruning a result deletes the artifacts that result alone referenced — its document, its screenshots, its stored bodies — and keeps those another result still references. Content addressing means two scans can share one artifact, so deletion is decided by reference, never by the age of a key.
- AC2: The reference check is a query. Which artifacts a result names is derivable from what the database holds, without reading documents back out of the bucket to find out.
- AC3: A sweep collects unreferenced artifacts left by interrupted writes (Story 8.2, AC4) and by failed deletions. It is safe to run while scans are running: an artifact written moments ago and not yet referenced is protected by a grace period rather than collected as garbage.
- AC4: A deletion failure does not fail the prune. A bucket that refuses a delete leaves the key for the next sweep, logged and counted, rather than aborting retention.
- AC5: Pruning reports what it removed — rows, artifacts, bytes — in its existing statistics, and the counts are exposed as metrics.
- AC6: A dry run lists what would be deleted. Deleting evidence is irreversible, and an operator changing retention should see the consequence before it happens.
- AC7: Retention configuration is unchanged (`maxAge`, `maxPerSeries`). This story changes what pruning removes, not how it is asked for.

### Story 8.6 — Configure the bucket, and document it (not implemented)

*As an Operator, I want the bucket configured by URL, with examples I can copy, so that pointing wsaw at my object storage is a one-line change.*

- AC1: One setting names the bucket as a URL (`store.artifactURL`), and with nothing configured the default is the local artifacts directory beside the store, exactly as today. The single-binary deployment that needs no external service stays the default deployment (Tenet 14).
- AC2: The existing `store.artifactDir` keeps working and is interpreted as a local file bucket. An upgrade does not require a configuration edit; setting both fails validation rather than silently picking one.
- AC3: Credentials come from the provider's own environment — the AWS, Google and Azure credential chains — and any credential that does appear in configuration is a secret reference, redacted everywhere a DSN or a webhook token is redacted (Story 4.7, AC6).
- AC4: The URL is validated at load with a line-referencing error for an unknown or malformed scheme, and the bucket is opened and probed at startup: a bucket wsaw cannot write to fails then, naming the URL, rather than on the first scan (Tenet 15).
- AC5: Bucket reachability is part of readiness, on the same argument as the database: a wsaw that cannot record what it observed is not ready, however healthy its browser is.
- AC6: The README documents the storage model — index in the database, documents and evidence in the bucket — and carries working configuration examples for the local directory (the default), S3, GCS, Azure Blob Storage, and an S3-compatible endpoint such as MinIO, each with the credentials it expects and the permissions the bucket needs to grant.
- AC7: The README states the operational consequences honestly: object storage is remote, so a write can fail for reasons a local disk would not; requests cost money and latency; and a lifecycle rule configured on the bucket will delete evidence behind wsaw's back if it is set to.
- AC8: `wsaw.example.yaml` carries the same examples, commented, so the shipped configuration and the README cannot drift apart.

### Story 8.7 — Serve artifacts that live in a bucket (not implemented)

*As a Compliance Officer, I want screenshots and downloads to work in the web interface whether the evidence sits on disk or in a bucket, so that where it is stored is not my problem.*

- AC1: The API and share-link routes serve artifacts from the bucket, streaming rather than buffering the whole object, with the headers each kind gets today unchanged — a response body stays an opaque attachment, a screenshot stays an image and nothing else (Story 5.17, AC5).
- AC2: A share token still reaches only the artifacts its own result names (Story 5.19, AC10). Content addressing plus a shared bucket makes that check the only thing between a link to a harmless scan and every stored artifact, so it is asserted directly against the bucket-backed path.
- AC3: Where a provider supports signed URLs and the operator opts in, wsaw may redirect instead of proxying. The signed URL's lifetime is short, configurable, and never longer than the share link that produced it. It is off by default: a redirect moves access control from wsaw to the bucket, which is a decision, not an optimisation.
- AC4: Content length and conditional requests still work, so the interface can show sizes and a browser can cache a screenshot instead of re-fetching megabytes (Story 5.17, AC4 and AC6).
- AC5: A missing artifact renders as evidence that is no longer stored, not as an error page. Retention removing evidence is expected, and absence is recorded rather than inferred (Tenet 5).
- AC6: Bucket latency stays confined to the request that caused it: an artifact fetch is bounded by a timeout, and its failure does not degrade the rest of the interface.

### Story 8.8 — Keep the single-binary promise, on the record (not implemented)

*As a Developer, I want the cost of this dependency stated and bounded, so that a convenience library does not quietly end cross-compilation or double the binary.*

`gocloud.dev/blob` with the S3, GCS and Azure drivers pulls in three cloud SDKs. That is a large dependency tree for a tool whose selling point is detecting supply-chain changes (Tenet 18), and it is justified only because the alternative — three hand-written clients with three credential chains — is worse. What is not negotiable is CGo and cross-compilation (Tenet 14).

- AC1: The build stays pure Go and CGo-free, and cross-compilation to linux/darwin × amd64/arm64 is verified in CI as part of this epic.
- AC2: The binary size change from adding the blob drivers is measured and recorded in the release notes. A number, not an assurance.
- AC3: Which providers are compiled in is a deliberate, documented decision. If the cloud drivers go behind a build tag to keep a lean default build, both builds are produced, tested and named in the release; if they do not, the reason is written down.
- AC4: The default build with nothing configured opens no cloud SDK, resolves no credential chain, and makes no network call at startup. A local deployment must not acquire cloud behaviour merely by being linked against it.
- AC5: The SBOM published per release (Story 6.1, AC3) covers the new dependency tree, and the added modules are reviewed once, explicitly, rather than arriving as transitive noise.

### Story 8.9 — Test both paths, and one real object store (not implemented)

*As a Developer, I want the bucket path covered by the same tests as the directory path, so that a provider difference surfaces in CI rather than in production.*

- AC1: The store's test suite runs unchanged against the file bucket and against an in-memory bucket, on the argument Story 4.7, AC5 made for dialects: the tests are the specification of what a store does.
- AC2: An S3-compatible service (MinIO) joins the containerised end-to-end stack (Epic 7), and the suite runs a full scan → store → prune → read-back cycle against it, skipping with a clear reason where no container runtime is present.
- AC3: The failure modes are tested, not assumed: a bucket that rejects a write mid-scan, a key deleted behind wsaw's back, a truncated object, and a digest mismatch. Each produces the recorded, non-fatal behaviour Story 8.2 requires.
- AC4: The migration is tested from a store populated in the old layout, for every dialect, including an interrupted-and-resumed run (Story 8.4, AC2).
- AC5: A test asserts that listing results performs no bucket operations (Story 8.3, AC2). That regression is invisible until it arrives as a production latency complaint.
- AC6: The soak test reports bucket operations and bytes transferred alongside its existing figures, so the storage cost of a long run is a measured number rather than a surprise on an invoice.

### Story 8.10 — A store with no database: the index in the bucket (not implemented)

*As an Operator, I want a store that keeps its index in the bucket alongside the evidence, so that a wsaw deployment can be one binary and one bucket, with no database to run, back up, or fail over.*

Stories 8.1 to 8.9 leave a database holding the index. For a deployment already running Postgres that is the right answer; for one running in Kubernetes against object storage (Story 6.10) it is an entire piece of infrastructure kept alive to hold a few thousand small rows. This story adds `blob` as a third kind of store beside SQLite and the server databases: the index becomes objects in the same bucket as the documents.

The reason this is a separate implementation rather than a fourth SQL dialect is that a bucket is not a database and must not be pretended into one. There are no transactions, no cross-key atomicity, and — portably, across the providers `gocloud.dev/blob` reaches — no compare-and-swap on which a read-modify-write could be built. Providers differ further in what they promise about listing: a key written a moment ago may not appear in the next prefix listing, and a listing may be served from a stale view. **A design that mutates an index object is therefore wrong twice over: it loses a concurrent write, and it reads a version that a later listing contradicts.** The design that survives is append-only — every write goes to a key nobody else writes and nothing ever rewrites, and every read is a fold over what is currently visible.

That has a price, and it is stated rather than hidden: this store answers the handful of queries wsaw actually makes, by key layout, and nothing else. It is not queryable with SQL, ad-hoc questions have no answer, and a listing costs requests. It buys the removal of the database, not a better database.

- AC1: `blob` joins `sqlite`, `postgres` and `mysql` as a configured store, selected by configuration, with SQLite remaining the default. A deployment that does not ask for it does not get it (Story 4.7, AC2).
- AC2: It implements the same store interface the rest of wsaw uses. Nothing outside the store package learns that there is no database — no caller acquires a second code path, and the scanner, API, scheduler and reports are untouched.
- AC3: **Every index write is to a unique, immutable key.** No index object is ever overwritten, appended to in place, or deleted as part of an ordinary write. Two writers racing produce two keys, never a lost update or a torn object.
- AC4: **The key layout answers the queries, so a read is a bounded prefix listing rather than a scan.** Newest-first per target and consent mode, one result by scan ID, the result preceding a given one, baselines per target and mode, and the audit log are each served by a prefix whose listing is already in the required order — ordering encoded in the key (an inverted timestamp with the scan ID as tie-breaker), not recovered by sorting everything the bucket holds. This is the same query list Story 4.6, AC4 indexed in SQL.
- AC5: **Ordering never depends on a clock being correct.** Timestamps order keys; they do not decide correctness. A clock that jumps, or two hosts that disagree, may put an entry in an unexpected position, and the resolution rule stays deterministic anyway because ties and inversions are broken by the scan ID. A wrong clock must not be able to make one scan overwrite or hide another.
- AC6: **Reads tolerate an index that is incomplete or lagging.** An entry that is not yet visible is treated as a result that does not exist yet, never as a result that was deleted; a document that is visible without its index entry is not silently lost (Story 8.5's sweep must not collect it, and a repair pass can re-derive its entry). Absence of evidence is recorded, never inferred (Tenet 5).
- AC7: **Reads are deterministic given what is visible.** Two reads that see the same set of keys produce the same answer, including the same "latest" and the same "previous". Where a read is not repeatable because the visible set changed, the change is a new entry appearing — not a different answer to the same question over the same data.
- AC8: **Mutable state is expressed as an append-only log with a deterministic fold, not as an object that is edited.** Approving a baseline appends an approval entry; changing it appends another; withdrawing it appends a revocation. The current baseline is the fold over the visible entries under that prefix, resolved by the AC5 rule. A baseline is a compliance decision, so last-writer-wins on a mutated object — where the loser leaves no trace — is not an acceptable failure mode.
- AC9: **The cross-row guarantee is preserved by shape, since it cannot be preserved by a transaction.** Story 4.6, AC7 requires that approving a baseline and recording its audit entry either both happen or neither does. Here they are written as one object, so one write carries both, and a partial write leaves neither. Where any operation genuinely cannot be reduced to a single object write, it is idempotent and self-healing on the next read, and that is documented per operation.
- AC10: **Writes are idempotent and retry-safe.** Re-running an interrupted write produces the same key with the same bytes, so a retry after an ambiguous failure cannot create a duplicate entry or a second version of the same fact.
- AC11: **Listing cost is bounded and measured.** Reads are paginated, a target's history does not require listing the whole bucket, and the number of requests per read path is documented and asserted by a test — this is the store where a careless read path becomes a line on an invoice. Where a prefix grows without bound, periodic compaction writes a checkpoint object summarising the entries before it, under a new immutable key; readers use the newest visible checkpoint plus the entries after it, and entries are only removed once their checkpoint has been durably visible for a documented grace period.
- AC12: Retention works as Story 8.5 defines it, with the reference check answered from the index objects rather than from a database query, and pruning removes an entry by appending a tombstone or by deleting a key that no reader can still need — never by rewriting a live index object.
- AC13: **It stays single-node, and says so.** Two wsaw instances against one bucket do not corrupt the index — that is what AC3 buys — but they still duplicate scheduled scans and can still disagree about a baseline between one appending an approval and the other seeing it. This story removes the database, not the constraint in Story 4.7, AC8.
- AC14: Startup validates the bucket the way Story 8.6, AC4 validates it, and additionally refuses to run against an index written by a newer layout version, exactly as a database store refuses a newer schema (Story 4.6, AC5). The layout version is an object in the bucket, written once.
- AC15: The store's existing test suite runs unchanged against it (Story 8.9, AC1), plus tests for what only this store can get wrong: a delayed listing that omits a just-written key, a listing served from a stale view, two concurrent writers of the same logical fact, an interrupted compaction, and a repair pass over an index entry that never appeared. A fake bucket that injects listing delay is part of the suite — provider behaviour that only shows up under eventual consistency cannot be left to a live-service test to discover.
- AC16: There is no migration between the SQL stores and this one in either direction, and that is stated in the README rather than discovered. Both keep their documents in the same bucket layout (Story 8.1, AC2), so what is not portable is the index — and rebuilding an index from the documents is the supported way across (Story 8.11).
- AC17: The README documents what this store is for and what it costs: no database to operate, no ad-hoc queries, per-request cost and latency on every read path, and a history whose listing gets slower as it grows unless compaction is left on. An operator should be able to choose between it and SQLite from that paragraph.

### Story 8.11 — Rebuild the index from what the bucket holds (not implemented)

*As an Operator, I want to rebuild the whole index from the artifacts in the bucket, so that a lost, corrupted, or out-of-date index is a recovery procedure rather than a lost history.*

Once the documents live in the bucket, the index stops being the record and becomes a derived view of it: an ordered list of pointers plus the summaries computed from the documents it points at. Derived data is disposable, raw capture is not (Tenet 4) — but only if the derivation can actually be re-run. That is what this story is. Without it, a dropped database, a half-finished migration, an index entry that never appeared under eventual consistency (Story 8.10, AC6), or a restore of the bucket from backup without its matching database each leave evidence that exists and cannot be found.

It is also the honest answer to moving between store kinds. Story 8.10, AC16 refuses to write a migration from SQL to the bucket index and back; rebuilding is why it can. Point the new store at the same bucket, rebuild, and the documents produce the index — no format converter, no second migration path to keep correct.

What a rebuild cannot do is invent facts that were never in a document, and this story is as concerned with saying that clearly as with doing the work.

- AC1: A command rebuilds the index for the configured store from the bucket alone — `wsaw store rebuild-index`, alongside the existing subcommands — and works for every store kind: SQLite, Postgres, MySQL, and the bucket-index store of Story 8.10.
- AC2: The source of truth is the stored documents. Every object under the result prefix is read, decoded, and yields its index entry and its summary fields, derived by the same code that derives them on write (Story 8.3, AC4), so a rebuilt index and a written one cannot disagree.
- AC3: It is resumable, idempotent, and bounded. Objects are listed and processed in batches with progress logged, memory does not grow with the number of results, an interrupted run continues rather than starting over, and running it twice changes nothing the second time.
- AC4: **It is non-destructive by default.** The existing index is not deleted before the new one is complete; the rebuild either builds beside it and swaps at the end, or merges into it, and an abort at any point leaves a readable store. Discarding history to rebuild it is the one failure this command must not have.
- AC5: A dry run reports what would be indexed — objects found, results decoded, entries that would be added, entries that would be removed — without writing anything, so an operator can see the outcome before choosing it.
- AC6: **What is not in a document is not recoverable, and the command says so rather than quietly producing an empty one.** Baselines and their approvals, the audit log, share tokens, and change-event history are decisions and observations, not properties of a scan; a rebuild preserves them where they still exist, and where they do not it reports exactly what was lost instead of presenting a store that merely looks intact.
- AC7: A corrupt, undecodable, or digest-mismatched object is reported by key, counted, and skipped — never fatal, and never silently omitted from the summary the run prints (Story 8.2, AC5 and AC6).
- AC8: A rebuilt result whose screenshots or stored bodies are missing from the bucket keeps its references and is recorded as evidence that is no longer stored, not repaired by dropping the reference. Absence is recorded, never inferred (Tenet 5).
- AC9: The run reports artifacts it found that no result references — the orphans Story 8.5's sweep exists to collect — as counts and bytes. A rebuild is the one operation that sees the whole bucket, so it is the cheapest place to learn what is in it.
- AC10: **A verify mode compares index against bucket and changes nothing.** It reports drift in both directions — entries pointing at objects that are gone, objects with no entry, summaries that no longer match their document — and exits non-zero when it finds any, so it can run on a schedule or in CI rather than being remembered after an incident.
- AC11: Its cost is bounded and stated. A rebuild reads every result object, so the request count and bytes are proportional to history; the run prints them, concurrency is configurable, and the README says what a rebuild of a large bucket will cost before someone runs one against object storage.
- AC12: It is safe against a store that is being written. Either it takes exclusive access and refuses with a clear message while a daemon is running, or it runs concurrently and results written during the rebuild are picked up or arrive afterwards without being lost — whichever it does is documented and tested, not left to chance.
- AC13: Tested by deleting the index and rebuilding it, for every store kind, asserting every result reads back byte-identically and every summary matches what the original write produced; plus an interrupted-and-resumed run, a corrupt object, a missing artifact, and a drift case that verify must catch.
- AC14: The README documents it as the recovery procedure — when to run it, what it restores, what it cannot restore (AC6), and that it is the supported path between store kinds — next to the storage configuration examples it belongs with.

---

## Technical Decisions (proposed, for review)

1. **CDP library:** drive Chrome via `chromedp`/`cdproto` rather than WebDriver — network-domain fidelity and initiator chains are the core product, and WebDriver does not expose them well. Playwright-Go is rejected as it requires a Node-based driver process.
2. **Chrome is an external runtime dependency** on bare-metal installs, and bundled in the container image. wsaw does not download browsers at runtime.
3. **Storage:** SQLite through `database/sql`, with a CGo-free driver, so the single-binary and cross-compilation promise holds while the driver stays the seam for a server database later (Story 4.6). No external DB is required in v1. Postgres and MySQL become options — never requirements — once Story 4.7 turns the SQLite-specific SQL into a dialect.
4. **Config:** YAML file plus flags/env overrides. A target list is inherently file-shaped, so config-file-less operation is supported only for one-shot CLI runs.
5. **Consent rules ship as data, not code** (embedded YAML rule pack), so rules can be updated by users and by releases without recompilation.
6. **v1 is single-node.** Distributed scheduling, sharding, and a web UI are explicitly deferred.
7. **Evidence storage:** scan documents, screenshots and stored bodies live in a bucket reached through `gocloud.dev/blob`, with the database holding an index and a reference (Epic 8). The default bucket is the local artifacts directory, so the no-external-service deployment is unchanged; S3, GCS and Azure are configuration. The library is preferred over per-provider clients because the credential chains and retry behaviour are the expensive part, and the cost it adds to the binary is measured rather than assumed (Story 8.8). A deployment that would rather run no database at all can put the index in the bucket too (Story 8.10), which is an append-only design rather than a fourth SQL dialect, because a bucket offers no transaction and no portable compare-and-swap to build one on.

---

## Open Questions (for the Product Owner)

- Expected scale: how many targets, at what interval, in how many consent modes? (Drives pooling, storage, and the performance figures in the NFRs.)
- Is stored evidence (screenshots, response bodies, HAR) required for compliance sign-off, or are hashes and metadata sufficient?
- Is authenticated scanning (login flows behind a session) in scope for v1, or public pages only?
- Which notification channels are actually required day one — is a generic webhook enough?
- Do results need to be retained for a fixed legal period, and must they be tamper-evident?
- Are scans expected from specific geographies or via specific egress IPs (consent banners and third parties vary by region)?
- What is the global default for `robots.txt` handling — `ignore` (own properties) or `respect` (third-party sites)?
