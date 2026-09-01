# wsaw — Architectural Tenets

Draft for review. These are the standing decisions that settle arguments during implementation. Each tenet states the rule, the reasoning, and — where it matters — what we are deliberately giving up. A tenet is not a suggestion: if a design violates one, either the design changes or the tenet is revised in this file.

Tenets are ordered roughly by how often they will be invoked.

---

## 1. The browser is a hostile, disposable subprocess

Chrome is treated as untrusted, failure-prone, and replaceable. wsaw never assumes it will answer, never assumes it will exit, and never lets it live longer than its configured budget.

- Every browser interaction is under a `context.Context` with a deadline.
- Every scan gets a fresh, isolated browser context and a temp profile directory that is removed on every exit path, including panic and cancellation.
- The supervisor kills and replaces browsers; no code path waits indefinitely on CDP.

*Trade-off:* process churn costs performance versus a long-lived warm browser. Correctness and isolation win.

## 2. Isolation is a correctness requirement, not an optimization

Consent state, cookies, cache, and storage leaking between scans would silently invalidate the product's core claim ("this is what the site loads in this consent state"). Isolation is therefore non-negotiable and must be *tested*, not assumed.

*Trade-off:* no warm cache by default, so scans are slower and measure cold-load behavior. That is also the more honest measurement.

## 3. Capture and interpretation are separate layers

The capture engine records what the browser did. It does not decide what is good, bad, first-party, expected, or alarming. Classification, normalization, diffing, and severity are downstream, pure, and operate on stored records.

**Consequence:** any change to rules, allow lists, or severity can be re-applied to historical results without re-scanning. Interpretation bugs are fixable retroactively; capture bugs are not.

## 4. Raw capture is immutable; derived data is disposable

What Chrome reported is stored verbatim (subject to size caps). Everything else — normalized keys, classifications, diffs, reports — is derived and can be recomputed at any time.

**Consequence:** never lose information during capture in the name of tidiness. Store both the raw URL and its normalized key. Store failures as records, not as absences.

## 5. Absence of evidence is recorded, never inferred

"No CMP detected", "consent unverified", "body unavailable", "scan hit the request cap" are all first-class recorded outcomes. wsaw must never let a failed observation look like a clean result. A compliance tool that quietly reports "no trackers found" when it actually failed to look is worse than no tool.

## 6. Determinism over completeness

Given an unchanged site, two scans must produce an empty diff. Where completeness and determinism conflict — infinite polling, animation-triggered lazy loads, randomized cache busters — wsaw chooses the reproducible answer and records what it truncated.

**Consequence:** hard caps on time, request count, and bytes are features. Every termination carries an explicit recorded reason. Diff noise is treated as a bug of equal severity to a missed asset.

## 7. Fail the scan, never the daemon

Blast radius of any error is exactly one scan. Panics are recovered at the scan boundary. A hostile page, a corrupt response, a hung CMP, or a full disk degrades one result and is recorded — the process keeps running and keeps watching everything else.

## 8. A stalled watcher is the worst outcome

Silent inactivity is more dangerous than loud failure, because the operator concludes "no changes" when the truth is "no observation". Liveness must be externally observable: per-target last-successful-scan timestamps, health endpoints that check Chrome usability, and failure counters that increase rather than errors that get swallowed.

## 9. Every scanned page is untrusted input

URLs, headers, DOM content, and script bodies are attacker-controlled. They are never interpolated into shell commands, file paths, format strings, or queries without escaping. Nothing from a page is executed, deserialized into code, or used to choose a filesystem destination. The Chrome sandbox stays on.

## 10. Consent rules are data, shipped and updated as data

CMP vendors change their markup constantly. If handling a new banner requires a Go release, the product is broken by design. Rules live in versioned YAML — embedded as defaults, overridable by the user, testable against live URLs with a dedicated command.

**Consequence:** the rule engine's expressiveness is a deliberate design surface. It must handle iframes, shadow DOM, waiting, and multi-step flows without becoming a scripting language.

## 11. Prefer the API over the click

Where a CMP exposes a programmatic surface (TCF `__tcfapi`, `__gpp`, vendor SDKs), use it. Clicking labeled buttons is the fallback, and heuristic label matching is the last resort — always flagged as such in the result. Robustness ranks above cleverness, and the result must always disclose which mechanism was used.

## 12. Interfaces at the seams, concrete types within

Capture engine, consent handler, target source, result store, differ, exporter, and notifier are interfaces — because each has a plausible second implementation (remote CDP, SQLite vs. a server database, S3 export, a second notifier). Everything else is a plain struct. We do not introduce an interface for a single implementation on the theory that someone might want another one.

## 13. The browser must be mockable

Most of wsaw's logic — normalization, diffing, severity, scheduling, retention, export — has nothing to do with Chrome and must be testable without it. The CDP boundary is narrow and mockable by construction; the fast test suite runs with no browser installed.

**Consequence:** integration tests use local fixture servers (including a synthetic consent banner and a synthetic third-party origin), never live third-party websites. CI must not depend on someone else's site staying unchanged.

## 14. Single static binary, no runtime surprises

Pure Go, CGo-free, cross-compiled for linux/darwin × amd64/arm64. No Node.js driver, no runtime browser downloads, no external database. The one external dependency is Chrome, and that is checked and reported at startup.

*Trade-off:* rules out some convenient libraries (notably CGo SQLite, Playwright). Deployment simplicity is worth more.

## 15. Configuration is declarative, validated up front, and reloadable

The target list is a versionable YAML artifact. Config is fully validated at load with line-referencing errors; the daemon refuses to start on invalid config and refuses to *apply* invalid config on reload while keeping the running config intact. Flags and env vars override the file; one-shot CLI mode needs no file at all.

## 16. Machine output first, human output derived

The JSON result schema is the product's real interface — versioned, documented, and stability-governed. Markdown reports, CSV, and HAR are derived views. No information exists only in a human-readable rendering.

## 17. Be a polite guest on other people's infrastructure

Per-origin rate limits, scheduling jitter, bounded concurrency, and an identifiable user agent are defaults, not options. wsaw's traffic must never be indistinguishable from an attack, and its default cadence must not amount to load-testing a site.

## 18. Standard library first

`log/slog` for logging, `net/http` for the API, `database/sql` for persistence, `context` for cancellation, `errors` wrapping for error chains. Dependencies are added when they solve a hard problem (CDP, public suffix list, YAML, a CGo-free SQLite driver), not for ergonomics. Every dependency is a supply-chain liability in a tool whose selling point is detecting supply-chain changes.

## 19. Data minimization by default

wsaw records URLs and cookies, which can contain personal data. Bodies and screenshots are off by default, caps apply when they are on, retention is bounded, artifacts are written 0600, and secrets are redacted centrally. The default configuration must be the one you would be comfortable defending.

---

## Explicit Non-Goals (v1)

- No distributed or multi-node operation.
- No Windows support.
- No JavaScript execution or DOM analysis beyond what consent handling requires — wsaw watches the network, it is not a scanner or a linter.
- No legal judgment: wsaw reports facts about network activity and consent state. Whether that constitutes a violation is a human decision.
- No automatic remediation of findings.
