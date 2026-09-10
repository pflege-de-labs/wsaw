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

---

# Epic 2 — Consent & Cookie Banner Handling

Interact with the site's consent management platform (CMP) so the same URL can be scanned in different consent states.

**Goal:** three reproducible consent modes per target — `none` (no interaction), `reject`, `accept` — so the difference between them becomes the compliance finding.

**Scope:** CMP detection, IAB TCF v2.2 API driving, vendor-specific adapters (Usercentrics, OneTrust, Cookiebot, Didomi, Sourcepoint, Consentmanager), generic selector-based rules, user-supplied rules, verification that the interaction actually worked.

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

**Scope:** JSON/JSONL result format with a versioned schema, HAR export, CSV summaries, HTTP API (query results, trigger scan, health), a bundled web interface for browsing targets, results, diffs and findings plus approving baselines, webhook notifications, Slack/Teams/e-mail via generic webhook, exit-code contract for CI usage, rule-based alert filtering.

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

### Story 5.8 — Dashboard and target overview (partly implemented: no open-findings column and no filter or sort, AC1 and AC4)

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

### Story 7.2 — A Docker Compose stack (not implemented)

*As a Developer, I want `docker compose up` to bring up the whole system so that reproducing a deployment problem takes a minute rather than an afternoon.*

- AC1: A Compose file defines four services: wsaw, the database, the Klaro fixture site, and the third-party origin the fixture loads from.
- AC2: Services address each other by name on a Compose network. This is what makes the stack work at all: a browser inside the wsaw container cannot reach the host's loopback, and Story 1.8 AC10 makes wsaw refuse such a target with an explanation rather than silently scan its own container.
- AC3: The browser runs **inside** the wsaw container, not as a sibling. Mounting the runtime socket would give a container that renders hostile pages the ability to start privileged containers, which is a worse trade than losing sibling-container isolation.
- AC4: wsaw waits for the database to be *ready*, not merely started, through a health check. A race that usually passes is worse than one that always fails.
- AC5: The database service is parameterised so the same file serves Postgres and MySQL, through a profile or an override file. Two nearly-identical Compose files would drift.
- AC6: `/dev/shm` is sized for the browser. The default 64 MiB makes Chrome crash on real pages, and the fix belongs in the shipped file rather than in a troubleshooting section.
- AC7: The stack runs unprivileged and holds no real credentials. The passwords in it are obviously fixtures, and the file says so, so that nobody copies it into production and inherits them.
- AC8: Bringing the stack down removes its volumes, so a stale database from an earlier run cannot make a test pass or fail for reasons that are not in the code.

### Story 7.3 — A Podman pod (not implemented)

*As an Operator, I want the same stack as a Podman pod so that the rootless runtime we prefer is a first-class path and not an afterthought.*

Story 1.8 already prefers Podman where a runtime is being detected. The deployment story should not then be Docker-only.

- AC1: The stack is expressible as a Podman pod, shipped either as a Kubernetes YAML for `podman kube play` or as a script that creates the pod and its containers — whichever the repository can also validate in CI.
- AC2: Containers in the pod share a network namespace and therefore reach each other on `localhost`. This is a genuine difference from the Compose stack, not a translation of it, and the fixture URLs differ accordingly — a port collision between services is a real failure mode here and the ports are assigned to avoid it.
- AC3: It runs rootless. Where rootless Podman needs something extra for Chrome — user namespaces, a seccomp adjustment — it is in the shipped definition with the reason, not left for the operator to rediscover.
- AC4: The same assertions run against the pod as against Compose. Two stacks that are tested differently are two systems.
- AC5: If `podman kube play` is used, the manifest is close enough to Story 6.10's Kubernetes manifests to be worth comparing, and the documentation says where they differ and why.

### Story 7.4 — End-to-end test against Postgres (not implemented)

*As a Compliance Officer, I want proof that a wsaw deployment backed by Postgres records what it observed, so that the evidence chain holds outside a developer's laptop.*

- AC1: The test brings up the stack with Postgres, scans the Klaro fixture in all three consent modes, and asserts on the rows the database actually holds — read back through SQL, not through wsaw's own API. Asking the writer whether it wrote correctly proves less.
- AC2: The consent assertions are the substance: in `none` the banner is present and untouched; in `reject` the banner is dismissed and the blocked third party stays absent; in `accept` it appears. Klaro's own state is checked too, so a scan that clicked nothing and a scan that clicked correctly cannot both pass.
- AC3: The third party the fixture loads unconditionally is reported as contacted before any consent decision, in every mode. This is the product's headline finding, and it is asserted end-to-end rather than in a unit test on a hand-built result.
- AC4: Two scans of the unchanged fixture produce an empty diff; a scan after the fixture changes produces exactly the expected changes with the expected severities. Diff noise is a defect of equal weight to a missed asset (Tenet 6).
- AC5: The schema is created by wsaw's own migrations against an empty database, and running the daemon twice against the same database is a no-op the second time. A migration that only works on a fresh schema is a migration that fails in production.
- AC6: The database is restarted mid-run and the daemon survives it: the scan in flight is recorded as failed, and the next scan reconnects and succeeds (Story 4.7, AC7).
- AC7: Baseline approval and its audit entry are asserted to be atomic under Postgres, not merely assumed to have carried over from SQLite.
- AC8: The test cleans up after itself — containers, volumes, networks — whether it passes or fails, and reports the container logs of every service on failure. A red end-to-end test that says only "assertion failed" costs more time than it saves.

### Story 7.5 — End-to-end test against MySQL (not implemented)

*As an Operator running MySQL rather than Postgres, I want the same guarantees, so that the choice of database is a deployment decision and not a support tier.*

- AC1: The same assertions as Story 7.4 run against MySQL. They are one test body parameterised by dialect, not two test files that will drift.
- AC2: The dialect differences MySQL forces are exercised rather than hidden: its upsert form, its JSON accessor, its identifier and reserved-word rules, and the absence of `rowid` in the retention delete.
- AC3: Character set and collation are pinned to `utf8mb4` and asserted. Captured URLs and page-controlled strings contain everything Unicode has; a database that silently truncates at a four-byte character would corrupt evidence quietly, which is the worst way to lose it.
- AC4: `sql_mode` is strict, so a value that does not fit is an error rather than a silent truncation. A store that quietly shortens a URL has changed the finding.
- AC5: Timestamps survive a round trip at the precision wsaw relies on for ordering. MySQL's default `DATETIME` drops fractional seconds, and "the scan before this one" is decided by that ordering.
- AC6: Where MySQL cannot do something the same way, the difference is recorded in the dialect documentation rather than worked around silently in the test.

### Story 7.6 — Run the stack in CI, and keep it out of the fast suite (not implemented)

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
