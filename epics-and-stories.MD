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

---

## Table of Contents

- [Epic 1 — Capture Engine](#epic-1--capture-engine)
- [Epic 2 — Consent & Cookie Banner Handling](#epic-2--consent--cookie-banner-handling)
- [Epic 3 — Targets, Configuration & Daemon Operation](#epic-3--targets-configuration--daemon-operation)
- [Epic 4 — Storage, Baselines & Change Detection](#epic-4--storage-baselines--change-detection)
- [Epic 5 — Reporting, Export & Alerting](#epic-5--reporting-export--alerting)
- [Epic 6 — Operations, Packaging & Distribution](#epic-6--operations-packaging--distribution)
- [Technical Decisions](#technical-decisions-proposed-for-review)
- [Open Questions](#open-questions-for-the-product-owner)

---

# Epic 1 — Capture Engine

Load a URL in headless Chrome and produce a complete, deterministic record of every network fetch it causes.

**Goal:** a single `Scan(target) → ScanResult` operation that is reliable, isolated, bounded in time, and produces stable output suitable for diffing.

**Scope:** browser lifecycle, CDP session handling, network event capture, page lifecycle/idle detection, resource classification, first-party/third-party attribution, initiator chains, per-scan browser profile isolation.

**Out of scope:** consent handling (Epic 2), scheduling (Epic 3), persistence (Epic 4).

### Story 1.1 — Launch and control headless Chrome

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

---

# Epic 3 — Targets, Configuration & Daemon Operation

Run as a long-lived service that scans a list of targets on a schedule.

**Goal:** operators define targets and schedules declaratively, start the daemon, and it keeps producing results without further interaction.

**Scope:** target list definition, config loading and validation, hot reload, scheduler, worker pool and concurrency limits, browser pool, per-target overrides (viewport, user agent, device emulation, headers, basic auth, geolocation/timezone, proxy), one-shot CLI mode, graceful shutdown.

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

### Story 3.3 — Concurrency and browser pooling

*As an Operator, I want bounded resource use so that the daemon is a good citizen on shared hosts.*

- AC1: `--concurrency` bounds parallel scans; default derived from `NumCPU` and available memory.
- AC2: A browser pool reuses Chrome processes across scans while preserving per-scan context isolation (Story 1.5).
- AC3: Pool members are recycled after a configurable number of scans or on memory threshold breach.
- AC4: Backpressure: when the pool is saturated, scans queue rather than spawning unbounded browsers.

### Story 3.4 — Config hot reload

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

---

# Epic 4 — Storage, Baselines & Change Detection

Persist scan results and turn a stream of scans into meaningful change events.

**Goal:** answer "what is new, gone, or changed since last time?" for every target and consent mode.

**Scope:** result store, retention, baseline/approved-state model, normalization rules (URL canonicalization, cache-buster and session-ID stripping), diff engine, script content hashing (SRI-style digests), allow/deny host lists, change event model, flap suppression.

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

### Story 4.4 — Allow and deny lists

*As a Compliance Officer, I want expected third parties declared so that alerts focus on the unexpected.*

- AC1: Per-target and global allow lists (host or domain patterns) suppress change events for known-good parties.
- AC2: Deny lists raise a finding whenever a matching host appears at all, regardless of baseline.
- AC3: A shipped, optional starter deny list of common tracking domains, clearly marked as advisory and updatable by the user.

### Story 4.5 — Change events with severity

*As an Operator, I want changes classified so that I can route them appropriately.*

- AC1: Each change produces an event with type, target, consent mode, severity, and the before/after values.
- AC2: Severity rules are configurable; sensible defaults (new third-party host in `reject` mode = high; changed script digest on a third-party script = high; new first-party image = low).
- AC3: Flap suppression: a change reverting within a configurable window is collapsed into a single event.

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

### Story 5.4 — HTTP API

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

### Story 5.8 — Dashboard and target overview

*As an Operator, I want a single screen showing the state of every target so that I can see at a glance whether wsaw is healthy and whether anything changed.*

- AC1: A target list showing, per target: name/URL, labels, configured consent modes, last scan time per mode, last scan outcome, consent outcome, and open findings by severity.
- AC2: Targets whose last successful scan is older than their schedule allows are visibly flagged — a stalled watcher must be obvious in the UI, not just in metrics (Tenet 8).
- AC3: Daemon health is surfaced: Chrome usable, config loaded, queue depth, browser restarts, notification failures.
- AC4: Filter and sort by label, severity, and staleness.
- AC5: Empty and error states are explicit ("never scanned", "scan failed", "consent unverified") and never render as a clean zero.

### Story 5.9 — Result and diff browser

*As a Compliance Officer, I want to drill into a single scan so that I can see exactly which third parties were contacted and in which consent state.*

- AC1: A scan result view showing scan metadata (target, consent mode, consent outcome and reason, termination reason, Chrome version, applied overrides), request counts by resource type, and the full asset table.
- AC2: The asset table is filterable by host, resource type, first/third-party, consent phase (`pre-interaction` / `post-interaction`), and status, and is sortable.
- AC3: A host-centric view aggregating requests per registrable domain, with first/third-party classification and request counts.
- AC4: Initiator chains are inspectable, so a user can see which script caused a given request.
- AC5: A diff view against baseline or previous scan, grouped into added hosts, removed hosts, added/removed assets, changed script digests, and changed cookies, with severity shown per change.
- AC6: A side-by-side comparison of the same target across consent modes, making "fired despite reject" the visually obvious finding.
- AC7: Every view offers download of the underlying data (JSON, CSV, HAR where available) so the UI never becomes the only way to reach a result.
- AC8: Result history per target and consent mode is browsable over time.

### Story 5.10 — Baseline review and approval workflow

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
- AC5: Captured screenshots and stored bodies are served with a non-renderable content disposition and never from the UI's own origin without sandboxing.
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

---

# Epic 6 — Operations, Packaging & Distribution

Make wsaw installable, operable, and debuggable on the target platforms.

**Goal:** a single static Go binary plus a documented Chrome dependency, deployable as a systemd unit, a launchd agent, or a container image.

**Scope:** build and release pipeline, cross-compilation for linux/darwin × amd64/arm64, systemd and launchd unit files, container image with Chrome bundled, Chrome discovery and version check at startup, self-observability (structured logs, self-metrics), sandbox and privilege model, CI (lint, vet, test, govulncheck, license check, SBOM).

**Out of scope:** package-manager distribution (brew/apt) beyond a stretch goal.

### Story 6.1 — Single-binary distribution

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

### Story 6.3 — Container image

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

### Story 6.8 — Soak testing

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

---

## Technical Decisions (proposed, for review)

1. **CDP library:** drive Chrome via `chromedp`/`cdproto` rather than WebDriver — network-domain fidelity and initiator chains are the core product, and WebDriver does not expose them well. Playwright-Go is rejected as it requires a Node-based driver process.
2. **Chrome is an external runtime dependency** on bare-metal installs, and bundled in the container image. wsaw does not download browsers at runtime.
3. **Storage:** an embedded, pure-Go store (e.g. `bbolt` or SQLite via a CGo-free driver) to keep the single-binary and cross-compilation promise. No external DB in v1.
4. **Config:** YAML file plus flags/env overrides. A target list is inherently file-shaped, so config-file-less operation is supported only for one-shot CLI runs.
5. **Consent rules ship as data, not code** (embedded YAML rule pack), so rules can be updated by users and by releases without recompilation.
6. **v1 is single-node.** Distributed scheduling, sharding, and a web UI are explicitly deferred.

---

## Open Questions (for the Product Owner)

- Expected scale: how many targets, at what interval, in how many consent modes? (Drives pooling, storage, and the performance figures in the NFRs.)
- Is stored evidence (screenshots, response bodies, HAR) required for compliance sign-off, or are hashes and metadata sufficient?
- Is authenticated scanning (login flows behind a session) in scope for v1, or public pages only?
- Which notification channels are actually required day one — is a generic webhook enough?
- Do results need to be retained for a fixed legal period, and must they be tamper-evident?
- Are scans expected from specific geographies or via specific egress IPs (consent banners and third parties vary by region)?
- What is the global default for `robots.txt` handling — `ignore` (own properties) or `respect` (third-party sites)?
