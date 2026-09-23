# Changelog

All notable changes to wsaw are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and wsaw uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) (NFR §5).

The story numbers refer to `epics-and-stories.MD`, where each one's acceptance
criteria, and any that are still open, are written down.

## [Unreleased]

### Added

- Container images on the GitHub Container Registry, for linux/amd64 and
  linux/arm64 in both variants: `ghcr.io/pflege-de-labs/wsaw` and
  `ghcr.io/pflege-de-labs/wsaw-cloudblob` (Story 6.3). They are pushed when a
  release is published — not when its tag is — so an image never goes out
  ahead of the release it belongs to. Each push is pulled back by digest and
  checked: both architectures must report the release's version, and Chromium's
  sandbox must start under `deploy/chromium-seccomp.json`. 0.1.0 predates this, and
  its images are published by running the workflow by hand for its tag.

### Changed

- The `Dockerfile` cross-compiles the Go binary on the build machine instead of
  compiling it under emulation for each target architecture, as `make release`
  already does for the binaries.

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

[Unreleased]: https://github.com/pflege-de-labs/wsaw/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/pflege-de-labs/wsaw/releases/tag/v0.1.0
