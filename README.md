# wsaw — website asset watcher

wsaw loads a list of URLs in headless Chrome, records **every** network fetch each page performs, optionally accepts or rejects the cookie banner first, and reports what changed since last time.

It answers three questions that are hard to answer any other way:

1. **Which third parties does this site contact, and does it still contact them after the user clicks "reject all"?**
2. **Did a third-party script change under a stable URL?** (Same file name, different content — the shape of a supply-chain compromise.)
3. **What does this site actually load, and how has that drifted?**

## Status

All of the functionality described in `epics-and-stories.MD` is implemented and tested, including browser-driven integration tests against local fixture sites. See [Limitations](#limitations) for what is deliberately not built.

## How it works

```
targets (YAML) ──▶ scheduler ──▶ browser pool ──▶ capture ──▶ store ──▶ diff ──▶ notify
                                       │            │                     │
                                   consent      normalize             web UI / API
```

Each target is scanned once per configured consent mode. A scan in `reject` mode and a scan in `accept` mode are separate results and are **never** compared with each other — the difference between them is the finding, not drift.

## Requirements

- Go 1.26+ to build (the CDP library sets that floor).
- A browser. wsaw prefers to run it in a container (see below); failing that, Chrome or Chromium on the host, whose version it checks at startup.
- Linux or macOS, amd64 or arm64. No Windows.

## Where the browser runs

wsaw renders pages it does not control. Left on the host, the only thing between a compromised renderer and your machine is the Chrome sandbox — a boundary the browser enforces on itself. So when a container runtime is available, wsaw runs the browser in a container by default, adding a boundary the operating system enforces.

**Podman is preferred over Docker**: daemonless and rootless by default, so wsaw needs no privileged socket and an escape lands as an unprivileged user.

```sh
podman pull docker.io/chromedp/headless-shell@sha256:2d349b544a1ea6b5b5fd7c0fe99215ff662339c57407ee2e8c0a11af93516b04
wsaw scan --url https://example.com/          # uses the container automatically
wsaw scan --url https://example.com/ --browser-runtime local
```

- The image is **pinned by digest** and never pulled during a scan. A moving tag would change capture behaviour between scans, and the diff would report it as the site's change.
- Every result records `browserRuntime`, `browserImage` and `browserSandbox`, because a result is only comparable with another if you can see what rendered it.
- One container per scan, removed on every exit path, and orphans from an unclean shutdown are reaped at startup.
- **A containerised browser cannot reach this machine's `localhost`** — it has its own network namespace. Scanning a local service fails with that explanation; use `--browser-runtime local` for it.
- Inside the container, Chrome's own sandbox is off. Nesting it would require privileges that weaken the container boundary that replaced it, so the container is the boundary and the result says so.

## Install

```sh
make build            # ./dist/wsaw
make release          # all four platforms, with checksums
```

Or run the container:

```sh
docker run --rm -v "$PWD/wsaw.yaml:/etc/wsaw/wsaw.yaml:ro" wsaw:latest
```

## Quick start

Scan one URL and print a report:

```sh
wsaw scan --url https://example.com/ --consent-modes reject
```

Watch a list of targets continuously, with the web interface on localhost:

```sh
cp wsaw.example.yaml wsaw.yaml     # annotated; edit the targets
wsaw config --config wsaw.yaml --check
wsaw run --config wsaw.yaml
```

Then open <http://127.0.0.1:8712>.

## Using it as a CI gate

`wsaw scan` exits with a contract:

| Code | Meaning |
|---|---|
| `0` | No findings at or above the threshold |
| `1` | Findings at or above `--fail-on` (default `high`) |
| `2` | Operational failure — bad config, no usable browser, a scan that could not run |

```sh
wsaw scan --config wsaw.yaml --fail-on high --format json
```

The ordering matters: an operational failure outranks findings. wsaw will never exit `0` because a scan failed to run — reporting "clean" when nothing was actually observed is the worst thing a gate can do.

## Commands

| Command | Purpose |
|---|---|
| `wsaw run` | Daemon: scan on a schedule, serve the API and web interface |
| `wsaw scan` | Scan once and exit with the CI contract |
| `wsaw debug <url>` | One scan, verbose, for authoring consent rules |
| `wsaw rules list` / `rules test <url>` | Inspect consent rules, or test them against a live page |
| `wsaw config` | Print the *resolved* configuration, or `--check` to validate |
| `wsaw version` | Build information |

`SIGHUP` reloads the target list. An invalid new configuration is rejected and the running one stays active — a watcher must not stop watching because of a bad edit.

It reloads the **target list** and nothing else, and that is a promise it keeps
out loud. Every other section became a browser pool, a normalizer, an HTTP
server, a store handle or a scheduler when the process started, and a running
daemon cannot swap those out from under in-flight scans. So a `SIGHUP` whose
file also moved one of those settings is **refused**, naming what moved:

```
reload rejected, keeping the running configuration
  error="detection.degradedFailureRatio, normalize.bodyIdentity cannot change
  without a restart; the running configuration is unchanged. Restart wsaw to
  apply them"
```

The refusal is the point. Adopting the half a reload can apply and logging
"configuration reloaded" would leave you believing the rest had taken effect
too — the same shape of defect as a broken scan that reads as a clean site.
Reverting the offending line and signalling again reloads normally; nothing is
sticky.

What a reload does apply: `targets`, `defaults`, per-target overrides,
`detection.severity`, `detection.allowHosts`, `detection.denyHosts`, and the
schedule shape (`scheduler.interval`, `cron`, `jitter`, `minInterval`).
Everything else needs a restart. A setting added to wsaw later is
non-reloadable until someone deliberately says otherwise, so the failure mode
for new configuration is a loud refusal rather than a silent no-op.

## Consent handling

wsaw prefers documented interfaces over guessing, and always records which mechanism it used so a reviewer can weigh the evidence:

1. **IAB TCF v2.2 API** (`__tcfapi`), plus `__gpp` where present.
2. **A vendor's own documented entry point** — Usercentrics, OneTrust, Cookiebot, Didomi and others.
3. **Selector rules** from the shipped rule pack.
4. **Heuristic label matching**, last, and flagged as heuristic in every result it produces.

Rules are **data, not code** (`internal/consent/rules/*.yaml`). CMP markup changes constantly, so handling a new banner must never require a new binary:

```yaml
# my-rules.yaml — referenced from consent.ruleFiles
version: 1
rules:
  - name: my-site-fix
    hosts: ["*.example.com"]
    priority: 1000          # outranks the shipped rules
    detect: "!!document.querySelector('#my-banner')"
    reject: [{click: "#my-reject-button"}]
    verify: "!document.querySelector('#my-banner')"
```

```sh
wsaw rules test --rules my-rules.yaml https://www.example.com/
```

A rule without a `verify` expression can never report `applied`, only `unverified`. That is deliberate.

## Reading a result

Two fields decide whether anything else on the page can be believed:

- **`termination`** — `idle` means the page went quiet on its own. `timeout`, `request-cap` or `byte-cap` mean the list may be incomplete. `error` or `skipped` mean it is not a result at all.
- **`consent.outcome`** — `applied` (verified), `unverified` (acted, unconfirmed), `not-needed` (no banner, which is common and legitimate), or `failed`.

Every request carries a **`phase`**: `pre-interaction` or `post-interaction`. Third-party hosts in the pre-interaction phase of a `reject`-mode scan are the headline compliance finding.

A missing script digest always carries a `bodyUnavailable` reason. wsaw never reports a script as unchanged because it could not read it.

### Screenshots

Set `screenshots: true` on a target and each scan captures the page twice —
before the consent interaction and after it — and the scan's page shows the
pair:

```yaml
targets:
  - name: marketing-site
    url: https://www.example.com/
    screenshots: true
```

The pair is the point. "Before" is the banner as the site presented it, which
is what a regulator asks about; "after" is what wsaw's interaction actually
did, which is the claim every `reject`-mode finding rests on. Read together
they either corroborate the recorded consent outcome or contradict it. Each
image shows its size and the SHA-256 the scan recorded, and links the original
file, so a screenshot in a compliance pack can be checked rather than trusted.

A screenshot is rendered as an image; a stored response body never is. The
difference is whose bytes they are: Chrome produced the PNG under wsaw's
control, so the page influenced its pixels only, whereas a body *is* the
page's bytes. Screenshots are served as `image/png` with `nosniff` and a
policy that allows an image and nothing else, and only when the file really is
a PNG — the kind and the magic bytes both have to agree.

A screenshot that retention has since removed is reported as missing rather
than shown as a broken image: expired evidence must not look like evidence
that never existed. Screenshots can contain personal data, so they are off by
default and retention applies to them like everything else.

### Response bodies

Every script gets a SHA-256 digest by default. Set `storeBodies: true` on a target to keep the bodies themselves:

```yaml
targets:
  - name: marketing-site
    url: https://www.example.com/
    storeBodies: true
```

Bodies are kept outside the result document, as content-addressed files, so a
result stays small enough to read on every page of history. Every **export**
resolves them, though, because a result that names a body nothing can reach is
not evidence of anything:

- the **HAR** carries each body in `response.content.text`, base64-encoded
  when the bytes are not text — which is what lets DevTools and every HAR
  viewer show it;
- the **JSON** result carries it in `body`, alongside the `bodyRef` it was
  stored under. Add `?bodies=false` when polling the API for metadata only;
- **`GET /api/v1/artifacts/{ref}`** serves one directly, and the result page
  links it.

A stored body is always served as an opaque attachment, never as something a
browser will render: those bytes came from a scanned site, and rendering them
on wsaw's own origin would hand a hostile page a same-origin context. A body
the size cap truncated says so — `bodyStoredSize` next to `decodedSize`, and a
note in the HAR — because a short body and a truncated one are different
facts. Bodies can contain personal data, so `storeBodies` is off by default
and retention applies to them as it does to everything else.

#### Scripts that rewrite themselves

A digest answers "did these bytes change", which is the right question for
almost every script and the wrong one for a few. A Google Tag Manager
container folds experiment flags into every response: two fetches of the same
*published* container can differ by a handful of tokens out of a hundred
thousand, and hashing them reports `script-changed` on nearly every scan —
which buries the one publish that mattered.

The container states its own version, so compare that instead:

```yaml
normalize:
  bodyIdentity:
    - urlPattern: 'googletagmanager\.com/gtm\.js'
      extract: '"version":"(\d+)"'
      label: GTM container version
```

`extract` needs exactly one capturing group, and the group is the identity;
a rule that cannot produce one is rejected at load rather than failing
silently on every scan. The change then reads *GTM container version changed
from 231 to 232* instead of printing two hashes.

The digest is still recorded next to it, so a reader can always see what was
hashed. If a rule matches the URL but the body does not carry the identity,
the script counts as **not comparable** rather than unchanged — the same
treatment as a missing digest, because a false "unchanged" is the worse answer
for a supply-chain check.

The result schema is published at [`docs/result.schema.json`](docs/result.schema.json) and the HTTP API at [`docs/openapi.yaml`](docs/openapi.yaml).

## Web interface and API

Both are served by the same process and the same port; the web interface is a client of the public API and has no privileged path into the store. Assets are embedded in the binary, so there is nothing to deploy alongside it and no Node toolchain to build it.

It binds to loopback by default. A non-loopback listener **requires** a token — configuration validation refuses to start without one, because scan results can contain personal data.

The dashboard refreshes itself, so it can be left on a screen and still be
worth looking at:

```yaml
api:
  refreshInterval: 30s   # the default a fresh browser gets; 0 disables it
```

That is only the default. The interval is a property of the person looking,
not of the deployment, so there is a control on the page itself, the choice is
remembered per browser, and `?refresh=30` in the URL sets it for a wall
display that should need no further setup. Every page says **when it was
rendered and what it will do about that** — a page that reloads silently
invites you to trust whatever is on it, and one that says "as of 14:32,
refreshing every 30s" cannot mislead you about how old it is. With JavaScript
available it also counts the age up and marks the page stale if a refresh
stops happening; with JavaScript off a `<noscript>` meta refresh does the
reloading instead.

Turning refreshing off wins over the automatic reload a running scan
triggers: you asked for the page to hold still, and the running scan is
visible on it anyway. The interval has a 5-second floor, because every refresh
re-renders the whole dashboard and reads every series' latest result.

The interval applies to the pages where something can change — the dashboard
and a target's history. **A scan detail page never auto-refreshes**, whatever
interval is in force and whether or not it is given in the URL: one finished
scan is an immutable record, so a reload re-renders identical content and
costs you your scroll position, the filter you just typed and the screenshot
you were looking at. It still says when it was rendered, because that is the
honest anchor for a tab left open an hour, and it says why it holds still
rather than reading as though refreshing were broken. Reloading it yourself
works as it always did.

Scans in flight are shown as they happen: a `pending` row on the target page, a
list on the dashboard, and `GET /api/v1/running` for anything else. A running
scan exists in no stored result — the store only learns of a scan when it ends —
so without this an idle daemon and a busy one look identical. Pages showing a
running scan reload themselves; pages with nothing running stay still. Starting
a second scan of a target and consent mode that is already scanning is refused
(`409`) rather than queued, because two concurrent scans of one series would
produce two results for the same moment and double the load on the scanned site.

## When a scan fails

A scan that produced no usable observation is retried:

```yaml
defaults:
  retryAttempts: 2       # counts the first attempt; 1 disables retrying
  retryBackoff: 60s      # doubles per attempt, jittered
  retryMaxBackoff: 10m
  retryTruncated: false  # also retry a timeout or a cap
```

The distinction that decides what gets retried is between a failure and a
finding. A browser that crashed, a container that would not start, a
navigation that failed at the network level — those are **missing
observations**, and not missing them is the whole job. A page that loads
nothing, refuses consent, or answers 500 is a **result**: wsaw saw it, and
scanning again would report the same thing at the site's expense. A scan
skipped by robots policy is a decision, not a failure, and is never retried.

Four consequences worth knowing:

- **Every attempt stays in the history**, with its own scan ID, and each
  result records which attempt it was and why the previous one failed. A retry
  is not a way to make a bad scan disappear.
- **A failed scan is never the comparison baseline.** Without that, a
  successful retry would be diffed against the failure it replaced and report
  the whole site as new — a finding manufactured out of wsaw's own recovery.
- **Notification is on the final outcome.** A failure a retry fixed pages
  nobody; `wsaw_scan_retries_total` and `wsaw_scan_retries_exhausted_total`
  still count it, because a site that only works on the third attempt is a
  finding of its own.
- **A pending retry does not hold a worker**, so one flapping target cannot
  stall the schedule. Retries skip the minimum interval — that floor bounds
  how often wsaw asks for a *new* observation, and a retry is the same one —
  but they stay inside the per-origin concurrency limit and are jittered, so a
  blip that fails fifty targets does not become a load test of fifty sites.

`wsaw scan` retries too, and its exit code reports the final outcome: a CI
gate that fails on one dropped connection teaches people to re-run it until it
passes, which is the opposite of a gate.

This is not the store's retry (`store.maxAttempts`), which retries a database
operation *inside* a scan. They are configured separately and neither implies
the other.

### When a scan half-fails

A scan can finish on schedule, terminate `idle`, and still be missing a tenth
of its requests. Chrome reports `net::ERR_INSUFFICIENT_RESOURCES` when it
cannot get a socket or the shared memory to open one — typically because a
page released its images in one burst and the browser container was sized for
less. Those requests never reach the network, so they say nothing about the
site, and every asset they would have loaded is absent too. One failed loader
silences everything below it.

Left alone this is the worst kind of noise, because it looks exactly like a
finding: assets vanish, then come back next scan. wsaw counts it instead:

```yaml
detection:
  degradedFailureRatio: 0.05   # 0 uses the default; above 1 disables the check
```

Above that share of lost requests, the scan is reported as `scan-degraded`
naming the actual error, and **no `asset-removed` or `host-removed` change is
raised** for that comparison. Additions still are, and so is everything about
consent. The asymmetry is deliberate: a request that is present can only mean
the site made it, while a request that is missing may mean wsaw failed to see
it. A false positive on an addition costs somebody a look; a false negative
hides a tracker that fired without consent.

A degraded *baseline* is reported too, for the mirror-image reason — an asset
that looks new may only have been missed last time — but its additions are
still raised rather than suppressed.

Failures that are not wsaw's fault do not count towards the ratio.
`net::ERR_ABORTED` is what a beacon looks like when the page is torn down
around it, and such a request usually carries a status because the server did
answer; `net::ERR_BLOCKED_BY_*` records a decision. Both are observations.

The fix for the underlying fault is to stop starving the browser:

```yaml
browser:
  container:
    shmSize: 1g          # the runtime default of 64m is not enough for a real page
    fileDescriptors: 8192
```

Both are defaults now, and both are limits rather than allocations, so they
cost nothing until they are needed.

## Sharing one result

Sending somebody the API token gives them every result, every target and
every write action, and cannot be taken back. A share link gives them one
scan, read-only, until it expires:

```yaml
api:
  share:
    enabled: true
    key: "${env:WSAW_SHARE_KEY}"   # at least 32 characters, and not the API token
    validity: 168h                  # the default a link gets
    maxValidity: 720h               # the most any request may ask for
    baseUrl: https://wsaw.example.com
```

Mint one from the result page, from the API, or from the shell when the
interface is not exposed:

```
wsaw share --target marketing-site --mode reject --scan latest --validity 48h
```

The link opens a page with that scan and nothing else — no navigation, no
audit log, no configuration path, no write actions — and it says at the top
that it is a shared, read-only view and when it stops working. The JSON, HAR
and CSV downloads come with it, because evidence a reader cannot take away is
evidence they cannot check, and so do that scan's screenshots.

**A link cannot be revoked before it expires.** Nothing is stored, so there is
nothing to delete: verification is arithmetic on the token itself. That is why
validity is short by default and why the scope is a single result. To withdraw
access in a hurry, rotate `api.share.key` — every outstanding link stops
working at once.

What a link cannot do, by construction: open another scan, reach the target
list or the audit log, write anything at all, or fetch any stored artifact
other than the ones its own result names. The token is signed HS256 with a
pinned algorithm — the signature is checked before any claim is read — and it
is kept out of wsaw's logs, results and error messages.

## Notifications

A **webhook** notifier posts one JSON event per change, optionally templated, which is the right shape for something that routes or deduplicates events.

A **teams** notifier posts one Adaptive Card per scan to a Power Automate Workflow webhook. It is separate code rather than a template for two reasons. A scan with forty changes would be forty chat messages and get rate-limited, so a channel wants the scan, not the change. And a malformed Adaptive Card *fails silently*: the Workflow returns success and posts nothing. An alert path that reports itself healthy while delivering silence is the failure this tool exists least to tolerate, so the card is built and size-checked in Go, not written by hand in YAML.

```yaml
notify:
  - name: teams
    kind: teams
    url: "${env:WSAW_TEAMS_WORKFLOW_URL}"   # a Workflow URL is a secret: its
    minSeverity: medium                      # authorisation is in the query
    baseUrl: https://wsaw.example.com        # so the card can link back
```

The card leads with the target, the consent mode, the highest severity present and the consent outcome, then lists the changes; severity is stated as text as well as colour. A scan that failed, was skipped, or was cut short is reported as untrustworthy rather than as a clean scan with few findings. A clean scan with nothing to report posts nothing at all — a channel that reports every scan gets muted, and then it reports nothing.

The retired `MessageCard` format is available as `format: messagecard` for a tenant still running an Office 365 connector webhook. Microsoft retired those on 30 April 2026; wsaw warns at startup when it is used.

## Logs

Log format follows where the output is going: readable and coloured on a
terminal, JSON when piped, redirected, containerised, or run under a service
manager. Nothing to configure for either case, and the resolved format is
stated in the startup line.

```
09:45:36.039 INFO  scan started    scan_id=scan-0083985a target=demo consent_mode=reject url=https://example.com/
09:45:38.490 INFO  scan finished   scan_id=scan-0083985a target=demo consent_mode=reject termination=idle requests=4
```

Force either with `--log-format pretty|json|text`, or `logging.format` in the
configuration. Colour is dropped for `NO_COLOR`, `TERM=dumb`, and any
non-terminal, and the level is always present as text so nothing depends on
it. Redaction is identical in every format.

## Monitoring wsaw itself

The metric that matters is `wsaw_last_successful_scan_timestamp_seconds`. It is only refreshed by a scan that produced a trustworthy result, so a stalled watcher is alertable:

```
# Alert when a target has not produced a good scan in 48h.
time() - max by (target, consent_mode) (wsaw_last_successful_scan_timestamp_seconds) > 172800
```

`/api/v1/ready` separates "the process is alive" from "Chrome is usable and configuration is loaded", and lists stale targets. The dashboard flags a series as stale when it has never been scanned, when its last scan failed, or when it is simply old.

## Deployment

- **systemd**: [`deploy/wsaw.service`](deploy/wsaw.service), hardened for a process that renders hostile pages. `RestrictNamespaces` is deliberately off: the Chrome sandbox depends on unprivileged user namespaces, and disabling them would push operators to turn off the sandbox instead — trading a real boundary for a nominal one.
- **launchd**: [`deploy/de.pflege.wsaw.plist`](deploy/de.pflege.wsaw.plist).
- **Container**: multi-arch, Chromium bundled and pinned, runs as a non-root user, sandbox enabled.

### Where results are stored

Results live in a SQL database reached through `database/sql`. Each result is stored as its JSON document plus the columns needed to index it, which keeps the published schema the single source of truth and makes the store queryable with ordinary SQL. Three drivers ship, all pure Go, so the binary still cross-compiles to four platforms without CGo.

**SQLite** is the default and needs no server — one binary, one file:

```yaml
store:
  driver: sqlite
  path: /var/lib/wsaw/wsaw.db      # empty uses the platform state directory
```

**PostgreSQL:**

```yaml
store:
  driver: postgres
  dsn: "${env:WSAW_STORE_DSN}"     # postgres://wsaw@db:5432/wsaw?sslmode=verify-full
  artifactDir: /var/lib/wsaw/artifacts
  maxOpenConns: 8
```

**MySQL** (8.0.19 or newer):

```yaml
store:
  driver: mysql
  dsn: "${env:WSAW_STORE_DSN}"     # wsaw@tcp(db:3306)/wsaw?tls=true
  artifactDir: /var/lib/wsaw/artifacts
```

The DSN belongs in a secret reference — it carries a password, and wsaw redacts it everywhere a webhook token is redacted. Anything driver-specific (TLS mode, connect timeout) goes in the DSN itself rather than being re-invented as wsaw settings. Screenshots and stored bodies stay on disk whichever driver is used: they do not belong in a row.

The schema is created and migrated by wsaw on startup, forward-only, and a store written by a newer wsaw is refused rather than misread.

A server database is reached over a network, so a transient failure — a restart, a failover, a deadlock — is retried:

```yaml
store:
  maxAttempts: 3       # 1 disables retrying
  retryBackoff: 200ms  # doubles per attempt
```

A permanent failure, such as a constraint violation, is never retried: that would only make it slower and hide the cause. Every retry is logged and counted as `wsaw_store_retries_total`, because a database that flaps while each scan quietly succeeds on the second attempt is worth knowing about before it becomes an outage.

Two things a server database does **not** do. It does not make wsaw multi-node: two instances sharing one database would still disagree about baselines and would duplicate every scheduled scan. And it makes the store a network dependency, so readiness fails when the database is unreachable — a wsaw that cannot record what it observed is not ready, however healthy its browser is.

State lives in the platform's directory by default (`$XDG_STATE_HOME/wsaw` on Linux, `~/Library/Application Support/wsaw` on macOS) and is created `0700`.

## Being a good citizen

wsaw scans other people's infrastructure, so politeness is enforced in code rather than left to configuration discipline: per-origin concurrency separate from worker concurrency, a minimum interval applied as a hard floor after the schedule, deterministic startup jitter, and catch-up off by default so a restart is not a scan storm.

A restart also continues each target's schedule instead of starting it over. The scheduler reads each target's last scan from the store when it starts, so a daily target scanned an hour before a restart is next due in twenty-three hours — not immediately. Otherwise the minimum interval would have nothing to measure from, and a daemon that restarts on every deploy would scan everything on every deploy.

`robots.txt` handling is **per target** (`ignore` or `respect`). Scanning your own properties argues for one, scanning someone else's argues for the other, and there is no honest universal default.

## Privacy

Captured data can itself be personal data, so:

- Cookie values are hashed, never stored. Header values are never serialized — only their names.
- Response bodies and screenshots are off by default.
- Credentials come from `${env:NAME}` or `${file:/path}` references, are redacted by type rather than by discipline, and are scrubbed from log output centrally.
- Artifacts and the database are written `0600`.

## Development

```sh
make check        # fmt, vet, lint, test -race
make test-fast    # skip browser tests
make soak         # long-run stability test (Story 6.8)
make cover        # coverage profile, one total
make cover-report # the same profile as a browsable HTML page
```

`make cover-report` writes `coverage-report.html`: every package ranked by statement coverage and
again by how many statements are untested, then a card per package with its files and the functions
no test ever reaches. The two rankings disagree on purpose — one answers "how well tested is this
package", the other "where should the next test go".

The fast suite runs **without Chrome installed** — browser tests skip themselves and say why. Integration tests use local fixture servers, including a synthetic consent banner and a synthetic third-party host; they never touch a live third-party website, so CI does not depend on someone else's site staying unchanged.

Contributors and coding agents: read [`AGENTS.md`](AGENTS.md), [`architecture-tenets.MD`](architecture-tenets.MD) and [`non-functional-requirements.MD`](non-functional-requirements.MD) first. The tenets are binding, not advisory.

## Limitations

Deliberate non-goals for this version:

- **No distributed operation.** One process, one host, local storage.
- **No multi-user accounts or roles.** One shared token, plus a read-only mode.
- **No target editing through the web interface.** Configuration stays a versionable file; the UI writes baselines and says where it put them.
- **No authenticated crawling flows** beyond HTTP basic auth and injected headers.
- **HAR exports carry no headers**, because wsaw does not retain them. Inventing plausible ones would be worse than an honest gap.
- **wsaw makes no legal judgement.** It reports facts about network activity and consent state; whether those facts constitute a violation is a human decision.

## Licence

MIT — see [LICENSE](LICENSE). Copyright (c) 2026 web care lbj GmbH.

Dependency licences are gated in CI, not merely reported: the build fails on
anything outside MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC and MPL-2.0,
and `make licenses` runs the same check locally. The tree is MIT and BSD apart
from one MPL-2.0 module, `github.com/go-sql-driver/mysql`, which wsaw links
unmodified — see NFR §9 for why that is accepted and what would change the
answer. A per-release SBOM records the exact versions.
