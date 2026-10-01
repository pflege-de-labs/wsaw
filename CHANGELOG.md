# Changelog

All notable changes to wsaw are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and wsaw uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) (NFR §5).

The story numbers refer to `epics-and-stories.MD`, where each one's acceptance
criteria, and any that are still open, are written down.

## [Unreleased]

Upgrading: nothing to do by hand. `storeBodies` keeps working and means what
it did; setting it and `bodies` on the same level is now a configuration
error. The store migrates to schema version 10 on first start, adding the
`body_samples` table, and result documents move to `schemaVersion` 2.2, an
additive change against which 2.1 documents still validate.

### Added

- Every start now logs the store's schema version and whether this start
  migrated it: `store schema is current; no migration was needed` with
  `schema_version` when nothing ran, or `migrating the store schema`
  (`from_version`, `to_version`), one `applied store schema migration` per
  version with its `duration`, and a closing `store schema migrated` with
  `migrations_applied`. The `store opened` line carries `schema_version` too,
  so the one line operators already look for names the schema. A restart
  that quietly upgraded a store is now told apart from one that found it
  current, and a long migration no longer looks like a hung start.
- Query-string handling can be scoped with `normalize.queryRules`: a rule
  names a URL pattern, a party, or both, and keeps, drops or strips the query
  of the requests it matches. Shipped rules for Google, Microsoft, Meta and
  Pinterest tracking endpoints keep only the parameter that names the tag, so
  a beacon called with new per-visit parameters is one asset instead of an
  `asset-added` and an `asset-removed` on every scan, while a new measurement
  ID is still reported. On 30 days of one deployment's history this removed
  55% of asset additions and removals and no host change. Opt out with
  `normalize.useDefaultQueryRules: false` (Story 4.14).
- The example configuration compares the gtag loader and consentmanager's
  `cmp.php` by the version each declares (`bodyIdentity`), because both
  rewrite their bytes on nearly every fetch: 68 and 66 `script-changed`
  events in the same 30 days, with the declared version unchanged throughout.
- Bodies can now be stored for every request a scan sees, not only for
  scripts, and exported in the HAR: a `bodies` block under `defaults` or on a
  target sets `store: all` (or `hashed`), and `requestBodies: true` also keeps
  what the page sent — the payload of a beacon, a form, an XHR — as
  `request.postData` in the HAR and `requestBodyRef` in the result (Story
  1.11). A `ratio` keeps bodies for only that share of scans, honoured over
  `ratioWindow` (a week by default) per target and consent mode, so storage can
  stay on permanently at a fraction of the cost; any ratio above zero keeps at
  least one scan per window. The decisions are kept in the database, so a
  restart continues the window rather than starting it over, and a retry of a
  sampled scan stays sampled. `wsaw scan --bodies=all` keeps one run's bodies
  whatever the ratio says. Each result's new `bodyCapture` states whether it
  was sampled and why, and in a sampled scan every request carries its body or
  a reason it has none. `maxBodyBytes` and `maxScanBytes` cap one body and one
  scan. New metrics: `wsaw_bodies_stored_total`, `wsaw_body_bytes_stored_total`
  and `wsaw_bodies_unavailable_total`.
- `deploy/browser/Containerfile` builds the per-scan browser image on
  Chromium 154 (154.0.8037.57) from Debian testing, for amd64 and arm64.
  Alpine, which the published image is built from, still carries Chromium
  152. The image keeps the same contract and sandbox as the published one, so
  point `browser.container.image` at a local build to use it:
  `podman build -f deploy/browser/Containerfile -t wsaw-browser:154 deploy/browser`.
  Under Docker it needs the seccomp profile as the published image does.
  A daily workflow opens a pull request when Debian ships a newer Chromium
  for both architectures, because the pinned snapshot never stops building
  and so would otherwise never show that it is out of date (Story 1.8).

### Changed

- With `--browser-runtime auto` (the default), a missing browser image no
  longer stops startup. wsaw checks each container runtime for the image at
  startup, tries Docker when Podman lacks it, and when neither has it falls
  back to Chrome on the host, warning with the `podman pull` / `docker pull`
  command and that pages now render without the container's boundary; results
  record `browserRuntime: local`. The image is still never pulled. A runtime
  named explicitly with `--browser-runtime podman` or `docker` still fails
  startup when it lacks the image (Story 1.8, AC6).
- Comparisons now key both scans again from their stored raw URLs under the
  current normalization rules, in the scan's own diff, the web interface and
  API, and `wsaw mcp`. Changing a rule applies to history and no longer makes
  the next scan report every re-keyed asset as removed and added. The stored
  `normalizedUrl` and the result schema are unchanged (Story 4.14).
- A script that rewrites itself on every fetch and is now keyed as one asset
  reports `script-changed` where it used to report an added and a removed
  asset. Add a `bodyIdentity` rule where the script declares a version.
  Responses with no version to read can be listed under
  `normalize.volatileBodies`: their digest is not compared, their URL still
  is. Google Ads' `viewthroughconversion`, whose script is built for each
  visit, is shipped there; opt out with `normalize.useDefaultVolatileBodies:
  false` (Story 4.14, AC6).
- `storeBodies: true` now records each stored body's size (`bodyStoredSize`),
  so the history and storage pages count stored bodies at their real size
  instead of as 0 bytes (Story 1.11).
- A body that could not be fingerprinted because too many requests finished
  at once now says so on the request (`body queue full`), where before only a
  scan warning did (Story 1.11).

## [0.5.0] - 2026-09-30

Every scan now says how far it can be trusted. A confidence score from 0 to
100, with its band, sits on each watchboard tile and is explained signal by
signal on the scan page, so a scan that lost requests or stopped early no
longer looks exactly like a clean one.

Upgrading: nothing to do by hand. The store migrates to schema version 9 on
first start, and result documents move to `schemaVersion` 2.1, an additive
change against which 2.0 documents still validate. Scans stored before this
release read "confidence not computed" rather than a score.

### Added

- Every scan now carries a confidence score from 0 to 100 and a band
  (`high`, `medium`, `low`, or `none` for a scan that failed or was skipped),
  shown on the watchboard tile as a compact mark (`C72`; `C?` where no score
  was computed, `CXX` for a failed scan, the band in words on hover) and
  explained signal by signal on the scan page (Story 5.35). Points come off for a scan that stopped early, for
  requests wsaw could not observe, for a duration far from the series'
  usual, and for a consent state that could not be verified. Until now a scan
  that lost 4% of its requests, or finished in a fifth of its usual time,
  looked exactly like a clean one on the board.
  - The score, its reasons and the duration median it was compared against
    are stored in the result document, so a scan's score can be checked
    against the document alone after retention prunes the scans behind the
    median. The result schema is now 2.1, an additive change: 2.0 documents
    still validate and still read.
  - `GET /api/v1/targets` reports `confidenceScore` and `confidenceBand` on
    each `lastScan`, and the reasons as `confidence` on each series. The
    `scan finished` log line gains `confidence` and `confidence_band`.
  - Scans stored before this release have no score and read "confidence not
    computed", never 100. Their documents are not rewritten.
  - The store migrates to schema version 9 on first start, adding two
    columns to `results`. There is nothing to do.

### Fixed

- The web interface's auto-refresh now checks the address it reloads before
  following it, and reloads the current page if that address is not a plain
  path on wsaw itself. The server already produced nothing else — every
  redirect destination has been held to the same allowlist of well-formed
  local paths since #96 — but the script read the address back out of the
  page, and a page is not something to take on trust. There is nothing to
  change in a deployment.

## [0.4.0] - 2026-09-29

The browser wsaw runs by default is now its own published image, with
Chromium 152 and Chrome's own sandbox kept on inside the container, in place of
`chromedp/headless-shell`, which had stopped at Chromium 151 and ran without
it.

Upgrading: pull the new image first, since wsaw never pulls one itself. Under
Podman nothing else changes. **Under Docker, add the seccomp profile to
`browser.container.extraArgs` before upgrading, or the browser will not
start** — see Changed. A configuration that names its own image is
unaffected.

### Changed

- The default browser image is now `ghcr.io/pflege-de-labs/wsaw-browser`,
  pinned to the digest published with 0.3.0 (Chromium 152.0.7977.82, Story
  1.8, AC5). It replaces `chromedp/headless-shell`, which had not published a
  build since Chromium 151 and ran Chrome with `--no-sandbox`; the new image
  keeps Chrome's own sandbox on, so scans record `browserSandbox: true`.
  - Fetch it before upgrading, since wsaw never pulls it:
    `podman pull ghcr.io/pflege-de-labs/wsaw-browser@sha256:cdc2555a1bcd5d962343aefc6be766808c0ba592826aca7c3f6cb5f6a7eda32b`.
  - **Under Docker, the browser does not start without the seccomp
    profile.** Add `browser.container.extraArgs: ["--security-opt",
    "seccomp=/path/to/deploy/chromium-seccomp.json"]`. wsaw now warns at
    startup when it runs a sandboxed image under Docker with no seccomp
    option. Rootless Podman needs nothing.
  - A configuration that names an image in `browser.container.image` is
    unaffected.
  - Scans after the switch are rendered by a different Chromium, so expect
    differences in the first ones that come from the browser rather than
    from the site.

### Fixed

- The browser image update check no longer reports a rebuild of the same
  browser as a newer image (Story 6.11). Every release republishes
  `wsaw-browser` with its own version labels, so its digest moves even when
  Chromium does not, and a built-in default can only pin the previous
  release's image: every start would have warned. When the latest image
  declares its Chromium in `de.pflege.wsaw.chromium.version`, that is now
  compared with the running browser's version, and a match is logged as
  current. A newer Chromium is still a warning, which now names both
  versions. Images without the label are compared by digest as before.

## [0.3.0] - 2026-09-29

Every scan now runs in a browser context of its own, so a reused browser can
no longer hand one scan's cookies to the next, and the warm-cache mode that
depended on sharing them is gone. The release also brings the first published
browser image that keeps Chrome's own sandbox inside the container, a daemon
that says when a newer browser image is out, a read-only MCP server over the
stored results, and a scheduled SQLite vacuum.

Upgrading: remove `warmCache` from any target before starting 0.3.0 — a
configuration that still sets it is refused — and expect result documents at
`schemaVersion` 2.0, against which stored 1.x documents no longer validate
(see Removed). The first scans after the upgrade may differ from the ones
before in cookies that no longer carry over between scans; that change is the
browser's, not the site's.

### Added

- `wsaw run` checks once at startup whether a newer browser image is
  published (Story 6.11). It asks the configured image's registry which
  digest the `latest` tag names and, when that is not the pinned one, warns
  with the digest to pin and the pull command. It reads one manifest
  anonymously and pulls nothing; the browser changes only when
  `browser.container.image` does. A registry it cannot reach is logged as a
  failed check, never as a current image. On by default;
  `browser.container.checkForUpdates: false` stops wsaw contacting the
  registry.
- A per-scan browser image that keeps Chrome's own sandbox inside the
  container (Story 1.8, AC5). `deploy/browser` builds Alpine's
  `chromium-headless-shell` — the same Alpine digest and Chromium version as
  the wsaw image — running as an unprivileged user, and speaks the contract
  `chromedp/headless-shell` set: CDP on port 9222, extra arguments appended,
  `--version` on the last line. Rootless Podman's default seccomp profile
  allows the sandbox; under Docker pass `deploy/chromium-seccomp.json`
  through `browser.container.extraArgs`. Where the sandbox cannot be built,
  Chromium refuses to start. Published with each release as
  `ghcr.io/pflege-de-labs/wsaw-browser`, and built locally with
  `make docker-browser`. The default image is unchanged.
- `wsaw mcp`, a read-only Model Context Protocol server over stdio, so an
  LLM client can read the stored results (Story 5.34). Its tools list the
  watched series and their scans, read one scan and page through its
  requests, compare a scan with its baseline or the previous one, and read a
  stored body or screenshot. The server is given a read-only view of the
  store, opens it without applying migrations, and refuses a store that does
  not exist instead of creating one. Page content is marked as untrusted in
  every answer that carries it. Standard library only; no new dependency.
- A daily workflow, `.github/workflows/chromium.yaml`, reads Alpine's
  package index for both architectures and opens a pull request that bumps
  the pinned Chromium version once both have a newer build. Alpine drops the
  old build as soon as a new one lands, so before this the image build broke
  before anyone knew a Chromium release was out.
- The Chromium bump workflow updates the browser image together with the
  wsaw image, and CI refuses a change that lets their Alpine base or
  Chromium version drift apart.
- The daemon vacuums a SQLite database file, once a week by default, so the
  file shrinks when the data in it does (Story 4.13). SQLite never returns
  freed pages on its own, so a store that went through the 0.2.0 upgrade,
  which moved every result document into the bucket, kept its old size. The
  first scheduled vacuum runs 10–20 minutes after the daemon starts. It is
  skipped while less than `store.vacuumMinFreeRatio` (0.2) of the file is
  free, and refused when the disk cannot hold the copy it builds.
  `store.vacuum: false` turns it off, and `store.vacuumInterval` sets how
  often it runs.
- `wsaw store vacuum` runs one now. `--dry-run` shows what it would reclaim
  and how much free disk it needs, and `--force` ignores the threshold. It
  refuses a PostgreSQL or MySQL store by name.
- Metrics: `wsaw_vacuum_runs_total{outcome}`,
  `wsaw_vacuum_bytes_reclaimed_total` and
  `wsaw_last_successful_vacuum_timestamp_seconds`. Each vacuum also leaves a
  receipt in the maintenance log, with kind `vacuum`.

### Changed

- A binary built with plain `go build` or `go install` from a checkout now
  names its code (Story 6.1, AC2). Without the Makefile's linker flags,
  `wsaw version` — and the version results record — used to say `dev`; it
  now falls back to the pseudo-version and commit the go command embeds,
  with `+dirty` for uncommitted changes. `go run` still reports `dev`.
- A `reject` scan whose consent outcome is `necessary-only` is now
  trustworthy (Story 2.10). The banner had no reject control, and wsaw
  verified that only strictly necessary categories were left active, which
  is the closest to a rejection the site allows. Such a scan no longer posts
  "Result not trustworthy" to Teams on every run. It posts only when it has
  changes at the notifier's threshold, and the card still states the outcome.
  The `trustworthy` field of a scan event changes meaning for this outcome
  only. A site gaining or losing its reject control is still reported, as a
  `consent-changed` finding.
- `browserSandbox` in a result, and the startup log, now report Chrome's
  sandbox as active in a container when the image declares it with the
  label `de.pflege.wsaw.browser.sandbox=enabled` and no sandbox-weakening
  switch (`--no-sandbox`, `--no-zygote-sandbox`, `--disable-namespace-sandbox`,
  `--disable-seccomp-filter-sandbox`) is in `browser.container.browserArgs`.
  Before, every containerised scan was recorded as unsandboxed. Results
  from the default image are unchanged. A label that cannot be read is
  logged and recorded as unsandboxed.
- `make test-store-minio` and the object-store end-to-end suite run the
  MinIO server from the PGSTY Silo image, `docker.io/pgsty/silo`, pinned to
  RELEASE.2026-09-16T00-00-00Z. MinIO's own image on quay.io no longer allows
  an anonymous pull, which failed every CI run.

### Removed

- **Breaking:** the `warmCache` target setting and the
  `environment.warmCache` result field (Story 1.5, AC3 dropped). Every scan
  now runs with the browser cache disabled. Each scan has its own browser
  context, so a cache could no longer carry over from an earlier scan. A
  warm scan also cannot be compared with the scans around it, so the
  option only added a way to get results that don't compare.
  - A configuration file that still sets `warmCache`, even to `false`, is
    refused at startup as an unknown field. Delete the line.
  - The result schema is now `schemaVersion` 2.0. A 2.0 document has no
    `warmCache`. The schema's `environment` object allows no other
    properties, so stored 1.x documents, which always carry the field, no
    longer validate against `docs/result.schema.json`. wsaw itself still
    reads and shows them: the field is ignored.

### Fixed

- Following a link to a result from outside the web interface — the
  "Open in wsaw" button on a Teams card, say — no longer shows the login
  form to a reviewer who is already signed in, and a reviewer who does have
  to sign in lands on that result rather than the dashboard. The browser
  withholds the `SameSite=Strict` session cookie from a navigation another
  site started, so such a request is now answered with a page that reloads
  the same path from wsaw's own origin, where the cookie is sent; the cookie
  itself stays `Strict`. The login form carries the requested path through
  sign-in, including a failed attempt, and accepts only a path on this
  origin. Redirect destinations containing a backslash, which a browser
  reads as a slash, are now refused as well.
- A scan on a reused browser no longer reports cookies another scan set
  (Story 1.5, AC1). Every scan now runs in a browser context of its own,
  with its own cookie jar, cache and storage, and that context is disposed
  of when the scan ends. Before this, scans on one browser shared a cookie
  jar that wsaw cleared when each scan started. The response to a closing
  page's unload beacon could arrive after that clear, so its cookie showed
  up in the next scan with no request in that scan to explain it. A
  pflege.de `reject` scan reported Bing's `MUID` and `MR` as new critical
  cookies this way, set by the `accept` scan that had just run on the same
  browser. With a context per scan there is nothing to clear, so the code
  that cleared cookies, cache and storage between scans is gone.
- At startup wsaw checks whether Chrome can create browser contexts; a
  managed install can forbid them with its `IncognitoModeAvailability`
  policy. If Chrome refuses, or the check cannot run, wsaw uses one browser
  per scan and overrides `browser.maxScansPerBrowser` to 1 if it was set to
  anything else, including 0. It logs a warning that states the configured
  and effective values, with an `instruction` field that says how to fix
  it. Set `browser.silenceContextFallbackWarning: true` to log it at info
  level instead. That setting does not apply to a `browser.remoteUrl`
  browser: every scan shares that browser, so one browser per scan
  isolates nothing there. A reload compares the file with the value wsaw
  actually runs with, so an unchanged file is not refused after the
  override.
- Downloads are now blocked in the scan's own browser context. Chrome
  applies a download policy with no context named to its default context
  only.
- A `SIGHUP` reload no longer loses a scan retry (Story 3.8). Before, a
  retry waiting to run was dropped, and a scan that failed while the reload
  landed scheduled its retry on the job the reload had just replaced, which
  never ran again. The failed attempt had been held back from notification
  because a retry was coming, so the failure was never published and the
  target went unobserved until its next scheduled run. The new job now takes
  over the pending retry and applies the reloaded retry policy. A target the
  reload removed has its outcome published as final.

## [0.2.1] - 2026-09-24

A certificate the server could not load is caught when the configuration is
checked, not first as a failed reload in the daemon's log.

### Changed

- `wsaw config`, including `--check`, and a SIGHUP reload now read the TLS
  certificate and key and refuse a configuration whose pair could not be
  served: unreadable, not PEM, or mismatched (Story 5.33, AC11). A refused
  reload leaves the certificate already being served in place.
- An expired certificate is reported but not refused, as at startup: the
  reload logs an error and goes ahead, and `wsaw config` prints the expiry on
  stderr and passes.
- Run `wsaw config --check` as the daemon's user, since the key is usually
  readable only by that user. No other command reads the files.

## [0.2.0] - 2026-09-24

A renewed TLS certificate is served without a restart, and the metrics are no
longer rounded before a scraper reads them.

### Added

- A renewed TLS certificate is served without a restart (Story 5.33). wsaw
  checks `api.tlsCert` and `api.tlsKey` every `api.tlsReloadInterval`
  (new; default 1m, at least 10s) and loads the pair when the files' contents
  change. It compares contents rather than modification times, so the
  symlink swap of a Kubernetes secret mount is picked up too. SIGHUP loads the
  pair at once, including when the rest of the reloaded configuration is
  refused. Open connections keep the certificate they negotiated.
- A renewal that cannot be loaded — half-written, unparseable, mismatched with
  its key, or already expired — leaves the served certificate in place. It is
  logged as a warning, and as an error once less than a fifth of the served
  certificate's lifetime is left.
- `wsaw_tls_certificate_expiry_timestamp_seconds`, the Unix time the served
  certificate expires, and `wsaw_tls_certificate_reloads_total{outcome}`. The
  gauge is left out when the interface is served over plain HTTP.

### Changed

- A missing, unparseable or mismatched TLS certificate or key now stops
  `wsaw run` before it listens, with the setting and the path in the error.
  Before, it surfaced only once the listener tried to start serving TLS.
- `api.tlsCert` and `api.tlsKey` may change on SIGHUP while TLS stays on.
  Turning TLS on or off still needs a restart, and a reload that tries is
  refused, naming both settings.
- The embedded SQLite driver, `modernc.org/sqlite`, is updated from 1.58.0 to
  1.59.0, and with it `modernc.org/libc` from 1.75.6 to 1.75.7 (#81). It is
  still pure Go, and no module was added.

### Fixed

- Metric values above a million are written exactly. They were formatted
  with six significant digits, which put `wsaw_last_successful_prune_timestamp_seconds`
  and `wsaw_last_successful_sweep_timestamp_seconds` up to about 1.4 hours
  off. It also rounded the artifact byte totals and histogram `_sum` values.
  The text on `/metrics` changes from, for example, `1.79e+09` to
  `1790000123`. Metric names, types and labels, and the histograms' `le`
  bucket labels, are unchanged.

### Known limitations

- Twelve metrics named as counters are declared `# TYPE … gauge` on
  `/metrics`: `wsaw_browser_restarts_total`, `wsaw_notifications_sent_total`,
  `wsaw_notifications_failed_total`, `wsaw_store_retries_total`,
  `wsaw_results_pruned_total`, `wsaw_scan_retries_total`,
  `wsaw_scan_retries_exhausted_total`, `wsaw_artifacts_deleted_total`,
  `wsaw_artifact_bytes_freed_total`, `wsaw_artifact_deletions_failed_total`,
  `wsaw_artifact_bytes_total` and `wsaw_artifact_stored_bytes_total`. Their
  values only ever rise, and they reset only when the process restarts, so
  `rate()` and `increase()` over them are correct. But tooling that reads the
  declared type — `promtool check metrics`, an OpenMetrics parser, a
  dashboard that picks its query from the type — treats them as gauges.
  Declaring them `counter` is a change to the metrics interface
  (AGENTS.md §8), so it waits for its own release and will be listed there
  under Changed. The other `_total` metrics are already declared counters.

## [0.1.1] - 2026-09-23

The first release with a container image. The binaries behave exactly as
0.1.0's do.

### Added

- Container images on the GitHub Container Registry, for linux/amd64 and
  linux/arm64 in both variants: `ghcr.io/pflege-de-labs/wsaw` and
  `ghcr.io/pflege-de-labs/wsaw-cloudblob` (Story 6.3). They are pushed when a
  release is published — not when its tag is — so an image never goes out
  ahead of the release it belongs to. Each push is pulled back by digest and
  checked: both architectures must report the release's version, and Chromium's
  sandbox must start under `deploy/chromium-seccomp.json`. 0.1.0 has no image: its
  `Dockerfile` predates the pins below, and an image built from it would carry
  whichever Chromium Alpine held on the day it was built.

### Changed

- The `Dockerfile` cross-compiles the Go binary on the build machine instead of
  compiling it under emulation for each target architecture, as `make release`
  already does for the binaries.
- The image pins what it is built from: both base images by digest, and
  Chromium by exact package version (152.0.7977.82-r0), which the image
  records in its `de.pflege.wsaw.chromium.version` label (Story 6.3, AC1).
  Before, `apk add chromium` installed whatever Alpine's branch held on the
  day of the build. Alpine keeps only the newest Chromium in a stable branch,
  so the build fails when a new one replaces it; the `Dockerfile` says how to
  bump the pin.

## [0.1.0] - 2026-09-23

The first release. wsaw loads a list of URLs in headless Chrome, records every
network fetch each page performs, optionally accepts or rejects the cookie
banner first, and reports what changed since the last scan.

### Added

#### Capture (Epic 1)

- Chrome discovery, a minimum-version check at startup, and a bounded browser
  pool; every browser and temporary profile is removed on every exit path,
  including cancellation and panic (Stories 1.1, 1.5, 3.3).
- Every network fetch a page performs is recorded — documents, scripts,
  stylesheets, images, fonts, XHR/fetch, beacons, media and websockets — with
  initiators, timings and content fingerprints of scripts (Stories 1.2, 1.6).
- Deterministic page-idle detection that does not let a tracker's heartbeat
  hold a scan open, and a scan budget reserved for the consent interaction
  (Stories 1.3, 1.10).
- First-party / third-party attribution through the public suffix list
  (Story 1.4).
- Per-target browser context tuning — viewport and device emulation, user
  agent, `Accept-Language`, timezone, geolocation, extra headers, basic auth and
  an outbound proxy — echoed into the result (Story 1.7).
- The browser runs in a container by default when Podman or Docker is present,
  pinned by digest, with the runtime, image and sandbox state recorded in every
  result (Story 1.8).
- `data:` URI payloads are treated as bodies rather than URLs (Story 1.9).
- A failed main-frame load is reported as an operational error rather than as
  an empty page.

#### Consent handling (Epic 2)

- A consent mode per target — `none`, `reject` or `accept` — each scanned and
  stored as its own result, never compared with the others (Story 2.1).
- CMP detection, driving TCF and vendor APIs first and falling back to
  rule-driven clicks, with the interaction verified rather than assumed
  (Stories 2.2–2.5, 2.7).
- Traffic split into pre-consent and post-consent phases (Story 2.6).
- A necessary-only outcome for CMPs with no reject control, and handling of a
  site's own banner where there is no CMP product (Stories 2.8, 2.9).
- The rule pack ships as embedded YAML data — Usercentrics, OneTrust,
  Cookiebot, Didomi, Sourcepoint, Consentmanager, CCM19, Complianz, Borlabs,
  Klaro and Osano, plus heuristics — and operators can add their own rule files
  without a release.

#### Targets, configuration and the daemon (Epic 3)

- A declarative YAML target list with line-referencing validation, overridable
  by flags and environment variables; one-shot mode needs no config file
  (Story 3.1).
- `wsaw run`: scheduled scanning on an interval or cron, on the wall clock, with
  jitter and a continued schedule across restarts (Story 3.2).
- `SIGHUP` reloads the target list, and refuses a reload that changes a setting
  only a restart can apply (Story 3.4).
- Graceful shutdown, secret references with central redaction, a per-site
  politeness and `robots.txt` policy, and retries of a failed scan
  (Stories 3.5–3.8).

#### Storage, baselines and change detection (Epic 4)

- Results, baselines and audit entries in a `database/sql` store: SQLite by
  default, with PostgreSQL and MySQL drivers behind a dialect seam
  (Stories 4.1, 4.6, 4.7). CI runs the store suite against all three.
- URL normalization, baselines and diffs, allow and deny lists, and change
  events ranked by severity (Stories 4.2–4.5).
- Artifacts stored compressed, and `wsaw artifacts compress` for those already
  on disk (Stories 4.8, 4.9).
- Thinning retention in the style of restic, which never keeps a broken scan in
  place of a good one, and receipts for what a prune or sweep removed
  (Stories 4.10, 4.11).
- The daemon sweeps the artifact bucket on its own, every 24 hours by default,
  collecting what an interrupted write or a refused delete left behind
  (Story 4.12). The schedule is kept across restarts, never overrides the
  refusal to sweep an empty store, never overlaps a prune, and is reported by
  `wsaw_last_successful_sweep_timestamp_seconds` and `wsaw_sweep_runs_total`.
  Set `store.sweepInterval` (one hour at least) to change it, or
  `store.sweep: false` to turn it off. Against object storage one sweep costs a
  listing request per thousand keys.

#### Reporting, export, alerting and the web interface (Epic 5)

- The JSON result document with a published schema,
  [`docs/result.schema.json`](docs/result.schema.json), plus HAR 1.2, JSONL, CSV
  and Markdown renderings derived from it (Stories 5.1–5.3).
- An HTTP API ([`docs/openapi.yaml`](docs/openapi.yaml)), bound to localhost by
  default, with bearer-token auth and TLS (Story 5.4).
- `wsaw scan` as a CI gate with a documented exit-code contract (Story 5.5).
- Webhook and Microsoft Teams notifications with retry and backoff
  (Stories 5.6, 5.14).
- A bundled web interface: a watchboard of targets with severity filtering and
  a scan-cycle bar, scan and diff pages with screenshots and change counts,
  baseline review and approval, running scans, ad-hoc scans of a typed URL,
  expiring share links, a history page that states what a year of scans costs,
  and a storage dashboard (Stories 5.7–5.32; see the stories for the ones
  superseded along the way).
- The storage dashboard and `/api/v1/storage` describe the installation — its
  database driver, its bucket and the shape of its history — so they are
  served only when `api.token` is set, on a loopback listener too, and answer
  `403` naming the setting otherwise. The bucket location they show is
  redacted (Story 5.32).
- `wsaw ui` opens the web interface already signed in (Story 5.21).

#### Operations and packaging (Epic 6)

- Cross-compiled, CGo-free binaries for linux and darwin on amd64 and arm64,
  with version, commit and build date embedded (Story 6.1).
- A hardened systemd unit and a launchd plist (Story 6.2), and a multi-arch
  container image definition with Chromium bundled (Story 6.3). Chromium keeps
  its own sandbox in the image: under Docker, run it with
  `--security-opt seccomp=deploy/chromium-seccomp.json`, Docker's default
  profile plus the four syscalls the sandbox needs. `make seccomp-profile`
  derives it from a pinned upstream, and CI checks on Docker that the sandbox
  starts under it and does not start without it.
- Structured `log/slog` logs with secret scrubbing, readable console output when
  a human is watching, self-metrics with a last-successful-scan timestamp per
  target, health and readiness endpoints, and a diagnostics mode
  (Stories 6.4, 6.5, 6.7, 6.9).
- Hardening against hostile pages, and a scheduled soak test (Stories 6.6, 6.8).

#### Containerised end-to-end testing (Epic 7)

- A self-hosted Klaro fixture site with a second origin (Story 7.1), and a
  Docker Compose stack of wsaw, a server database and the fixture
  (Story 7.2). See Known limitations for Stories 7.3–7.6.

#### Result documents in object storage (Epic 8)

- Evidence — result documents, screenshots and stored bodies — reached through
  a `gocloud.dev/blob` bucket. The default bucket is the local artifacts
  directory; S3, GCS and Azure are configuration (Stories 8.1, 8.2, 8.6).
- Listing and summaries answered from the index without reading a document, a
  forward migration of stores that kept documents in rows, retention that
  deletes what it stops referencing, and artifacts served from the bucket
  (Stories 8.3–8.5, 8.7).
- Two released variants: the default build, which links no cloud SDK, and the
  `cloudblob` build with the S3, GCS and Azure drivers. `make verify-variants`
  checks the difference on every release, and the measured size cost is in the
  release notes (Story 8.8).
- The store suite runs against a directory, an in-memory bucket and a real
  MinIO, and CI runs it on SQLite, PostgreSQL and MySQL (Story 8.9), and `wsaw store rebuild-index` rebuilds a lost index from
  the documents in the bucket (Story 8.10).

### Release artifacts

- `wsaw_<os>_<arch>` (default build) and `wsaw_cloudblob_<os>_<arch>` for
  linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64.
- A CycloneDX SBOM per variant, `wsaw.cdx.json` and `wsaw-cloudblob.cdx.json`.
- `SHA256SUMS`, covering every binary, both SBOMs, `SIZES` and
  `RELEASE-NOTES.md`.
- No container image is published for 0.1.0. The `Dockerfile` builds one
  (`make docker`), but nothing pushes it to a registry yet.

### Compatibility

The JSON result document is wsaw's interface (Tenet 16 in
[`architecture-tenets.MD`](architecture-tenets.MD)); this release publishes it
as `schemaVersion` 1.0, described by
[`docs/result.schema.json`](docs/result.schema.json). Changes to it are
additive by default. Removing or repurposing a field is a breaking change, and
takes a human decision and a version bump. Metric names and labels are a
public interface under the same rule ([`AGENTS.md` §8](AGENTS.md)). NFR §5 asks
for this policy to be documented; those two places are where it is written
today.

As a 0.x release, 0.1.0 makes no further promise beyond that rule. Semantic
Versioning allows a 0.x minor release to break compatibility, and anything that
does will say so here.

### Known limitations

Open acceptance criteria are listed on each story heading in
[`epics-and-stories.MD`](epics-and-stories.MD). These are the ones an operator
is most likely to meet:

- **Release artifacts are not signed, and macOS notarization is not
  documented** (Story 6.1, AC3 and AC4). Verify downloads with `SHA256SUMS`.
- **No container image is published.** Build one with `make docker`.
- **The per-scan browser container runs Chrome with `--no-sandbox`**
  (Story 1.8, AC5). Its `chromedp/headless-shell` image forces the flag, so
  there the container is the only boundary. The wsaw image itself keeps
  Chromium's sandbox (Story 6.3).
- **The end-to-end suite does not gate this release** (Story 7.6, AC2 and AC3).
  The Klaro fixture (Story 7.1) and the Compose stack (Story 7.2) are on main.
  The Podman pod, the assertions against PostgreSQL and MySQL, and their CI job
  (Stories 7.3–7.6) were merged into stacked feature branches rather than into
  main, and need porting to the Epic 8 store before they can run. Until then
  the release workflow runs no end-to-end job; running the Compose targets
  alone would bring the stack up without asserting anything.
- The unreferenced result document that an interrupted write leaves behind is
  counted by `wsaw store sweep` but never collected. It is kept as input for
  `wsaw store rebuild-index` (Stories 8.2, AC4 and 8.5, AC3).
- The storage dashboard's growth and prune charts carry figures that its JSON
  form does not report (Story 5.32, AC9 and AC12).
- Ad-hoc API scans of configured targets are not rate-limited; only scans of a
  typed URL have a budget (Story 5.4, AC3).
- Reload is on `SIGHUP` only, with no file watch (Story 3.4, AC1). Browsers are
  recycled after a scan count or a crash, not at a memory threshold
  (Story 3.3, AC3). No starter deny list ships (Story 4.4, AC3).
- There are no Kubernetes manifests (Story 6.10). Remaining web-interface
  gaps are in Stories 5.8–5.10 and 5.31.
- Test coverage gaps: no test checks that Chrome processes return to baseline
  (Story 1.1, AC5), the soak test does not count file descriptors or child
  processes (Story 6.8, AC2), and two of Story 4.11's receipt tests are
  missing (AC9).

### Stores created before 0.1.0

A SQLite or PostgreSQL store opened by a build of main between the receipt
log's arrival (#64) and this release has a `maintenance_runs.trigger` column.
Schema migration 8 renames it to `triggered_by` when 0.1.0 first opens the
store, and the receipts already recorded survive the rename. There is nothing
to do by hand. No MySQL store could have got that far, since the migration that
added the column never applied there.

[Unreleased]: https://github.com/pflege-de-labs/wsaw/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.5.0
[0.4.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.4.0
[0.3.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.3.0
[0.2.1]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.2.1
[0.2.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.2.0
[0.1.1]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.1.1
[0.1.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.1.0
