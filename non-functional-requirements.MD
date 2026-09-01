# wsaw — Non-Functional Requirements

---

## 1. Performance & Efficiency

- A scan of a typical content website (≤ 300 requests) completes in < 20s wall-clock at default settings, excluding deliberate dwell time.
- Memory: the wsaw process itself stays < 250 MB RSS with 500 configured targets; total footprint including the browser pool is bounded and configurable — the pool size, not the target count, determines peak memory.
- Startup to operational readiness < 2s, excluding the one-time Chrome presence/version check.
- Scan work never blocks the scheduler, the API, or the config-reload path — capture runs in a bounded worker pool.
- Response bodies are streamed and hashed, never buffered whole in memory beyond a configurable cap.
- Storage growth is predictable and documented per scan (metadata-only baseline figure), and bounded by retention policy.

## 2. Reliability & Availability

- A failure in a single scan (page crash, browser hang, TLS error, hostile page) must never terminate the daemon — the error is recorded against that target and the remaining targets keep processing.
- A crashed or unresponsive Chrome process is detected within a bounded interval, killed, and replaced; browser restarts are counted as a self-metric and recorded to the target.
- No process, temp profile directory, or file-descriptor leak during long-running operation (continuous run > 30 days). Verified by a soak test in CI (separated from regular CI runs) or on a schedule.
- Result persistence is atomic: an unclean shutdown or a full disk never leaves a corrupt store or a truncated result file.
- Notification and export failures trigger retry with exponential backoff and jitter; brief outages cause no event loss within a documented buffer limit. Dropped events are logged and counted, never silently discarded.
- Scan results are reproducible: two consecutive scans of an unchanged site in the same consent mode produce an empty diff. A false-positive rate above a small documented threshold on stable fixture sites is a defect.

## 3. Compatibility & Portability

- Runs on Linux (amd64, arm64) and macOS (amd64, arm64/Apple Silicon). Windows is explicitly out of scope.
- Requires an external Chrome or Chromium binary on the host (or bundled into the container image). The minimum supported major version is documented and checked at startup, failing immediately with an actionable message.
- Tolerant of Chrome minor/major upgrades: CDP usage sticks to stable domains; a new Chrome version must not silently change result semantics. Chrome version is recorded in every result.
- No assumptions about the scanned site's stack, CMS, CDN, or CMP vendor. Unknown CMPs degrade to a recorded "not detected" outcome, never to a crash.
- Output formats are ecosystem-standard: HAR 1.2, JSON with a published schema, Prometheus/OTLP metrics.

## 4. Security & Privacy

- **Untrusted input by definition.** Every scanned page is hostile until proven otherwise. Page-controlled data (URLs, headers, DOM text, script content) is never interpolated into shell commands, file paths, SQL, or log formats without escaping.
- The Chrome sandbox stays enabled. `--no-sandbox` is opt-in, warned about at startup, and never the documented default — including in the container image.
- The daemon runs as an unprivileged user; no root or admin rights in normal operation. The shipped systemd unit is hardened (dedicated user, `ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`).
- Downloads are suppressed and non-HTTP(S) scheme navigation is blocked, so a scan cannot write attacker-chosen files to disk.
- Credentials (basic auth, proxy, webhook tokens, API tokens) are never written to logs, results, API responses, or error messages; redaction is enforced centrally and covered by tests.
- Captured data may itself be personal data: URLs and cookies can contain identifiers. Retention is bounded, and stored artifacts are written with restrictive file permissions (0600/0700).
- The HTTP API binds to localhost by default; remote exposure requires explicitly enabling it, and supports bearer-token auth and TLS.
- Outbound scanning is constrained: SSRF-style abuse is mitigated by an optional deny list for private/link-local address ranges, off only by deliberate configuration.
- CI runs `govulncheck` and dependency scanning; no known critical CVEs in a release. Chrome version pinning in the container image is tracked and updated.

## 5. Maintainability & Testability

- ≥ 80% unit-test coverage on core logic: capture normalization, diff engine, URL normalization, consent rule matching, severity rules.
- Integration tests run against local fixture sites served by the test harness (including a synthetic cookie banner and a synthetic third-party host) — no reliance on live third-party websites in CI.
- `golangci-lint` and `go vet` are merge blockers.
- Modular structure with interfaces at the seams: capture engine, consent handler, target source, result store, differ, exporter, notifier are separately swappable and independently testable. The browser must be mockable so that non-browser logic is testable without Chrome.
- Consent rules live in data files with their own test fixtures, so a rule fix requires no code change and no release of the binary.
- Conventional Commits and SemVer for releases; a documented compatibility policy for the result schema and for metric names/labels.

## 6. Observability (of wsaw itself)

- Structured JSON logs via `log/slog`, level configurable, every scan-scoped line correlated by scan ID, target.
- Self-metrics (Prometheus and/or OTLP): scans started/succeeded/failed by reason, scan duration histogram, requests captured per scan, browser restarts, consent outcomes by result, queue depth, notification failures, and last-successful-scan timestamp per target.
- The last-successful-scan-per-target metric must make a silently stalled watcher alertable — a watcher that stops watching is the worst failure mode of this product.
- Health and readiness endpoints distinguish "process alive" from "Chrome usable and config loaded".

## 7. Installability & Operability

- Single static Go binary per platform, plus the documented Chrome dependency. No runtime downloads, no Node.js, no CGo requirement — which is why the SQLite driver must be a CGo-free one.
- Multi-arch container image with Chrome bundled and pinned.
- Ships a systemd unit and a launchd plist; documented XDG-compliant paths on Linux and `~/Library/Application Support` on macOS.
- Configurable via a YAML config file, with flags and environment variables overriding it. One-shot CLI mode works with no config file at all.
- Config errors are caught at load time with line-referencing messages; the daemon refuses to start on invalid config, and refuses to *apply* invalid config on reload while keeping the previous one active.
- Upgrades are drop-in: a new binary reads the previous config and store without a migration step, or performs the migration automatically and reversibly.

## 8. Ethics & Fair Use

- wsaw must be a polite crawler: per-origin rate limiting and scheduling jitter are on by default, and the default cadence must not amount to load-testing someone else's site.
- user-agent can be configured, but defaults to the shipped chrome user-agent
- `robots.txt` handling can be configured per site

## 9. License & Compliance

- Source code under the MIT license (see `LICENSE`).
- All dependencies license-compatible with MIT; automated license checking in CI.
- Chrome/Chromium redistribution terms respected in the container image; Chromium is preferred over Chrome where redistribution matters.
- A SBOM is generated in CI

