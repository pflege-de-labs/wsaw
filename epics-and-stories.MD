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
- [Epic 7 — Containerised End-to-End Testing](#epic-7--containerised-end-to-end-testing)
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

### Story 1.8 — Run Chrome in a container

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

- AC1: The dashboard refreshes itself on an interval, and the interval can be changed from a control on the dashboard, including turning it off.
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

### Story 6.10 — Run wsaw in Kubernetes

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

## Technical Decisions (proposed, for review)

1. **CDP library:** drive Chrome via `chromedp`/`cdproto` rather than WebDriver — network-domain fidelity and initiator chains are the core product, and WebDriver does not expose them well. Playwright-Go is rejected as it requires a Node-based driver process.
2. **Chrome is an external runtime dependency** on bare-metal installs, and bundled in the container image. wsaw does not download browsers at runtime.
3. **Storage:** SQLite through `database/sql`, with a CGo-free driver, so the single-binary and cross-compilation promise holds while the driver stays the seam for a server database later (Story 4.6). No external DB is required in v1. Postgres and MySQL become options — never requirements — once Story 4.7 turns the SQLite-specific SQL into a dialect.
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
