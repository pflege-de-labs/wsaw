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
make build-cloudblob  # ./dist/wsaw_cloudblob, with the S3, GCS and Azure drivers
make release          # both variants, all four platforms, with checksums and sizes
```

Two binaries, because the three cloud SDKs weigh more than the rest of wsaw: take the default one unless you intend to keep evidence in object storage, in which case see [the artifact bucket](#the-artifact-bucket).

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
| `wsaw store migrate` | Bring the store's schema up to date; `--dry-run` reports what would move |
| `wsaw store prune` | Apply retention and reclaim the artifacts it orphans; `--dry-run` lists them |
| `wsaw store sweep` | Delete artifacts nothing references any more; `--dry-run` lists them |
| `wsaw store rebuild-index` | Rebuild the index from the documents in the bucket; `--verify` checks it and exits non-zero on drift |
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

This is not the store's retry (`store.maxAttempts`), which retries a store
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

`/api/v1/ready` separates "the process is alive" from "wsaw can actually scan and store the result": Chrome is usable, configuration is loaded, the store answers and the artifact bucket is still there. It lists stale targets with it. The dashboard flags a series as stale when it has never been scanned, when its last scan failed, or when it is simply old.

Poll that endpoint rather than the `wsaw_ready` gauge for readiness. The gauge covers what the process knows about itself — Chrome usable, configuration loaded — and deliberately not the store, because asking a store and a bucket whether they answer costs a round trip each and a metrics scrape is not the place to spend it. A wsaw whose store or bucket has gone away reports 503 on `/api/v1/ready` while `wsaw_ready` stays 1; the reason it gives says which half is unreachable, and the detail — which bucket, which directory — goes to the log rather than into a response body that is unauthenticated when no API token is configured.

## Deployment

- **systemd**: [`deploy/wsaw.service`](deploy/wsaw.service), hardened for a process that renders hostile pages. `RestrictNamespaces` is deliberately off: the Chrome sandbox depends on unprivileged user namespaces, and disabling them would push operators to turn off the sandbox instead — trading a real boundary for a nominal one.
- **launchd**: [`deploy/de.pflege.wsaw.plist`](deploy/de.pflege.wsaw.plist).
- **Container**: multi-arch, Chromium bundled and pinned, runs as a non-root user, sandbox enabled.

### Where results are stored

A result is stored in two halves. The **index** entry names the scan — target, consent mode, scan ID, start time, and the handful of counts a listing shows — and points at the scan's JSON document, which is kept in the **artifact bucket** beside the screenshots and response bodies from the same scan. That keeps the published schema the single source of truth for what a result *is*, and keeps a multi-megabyte payload out of every backup and every replication stream.

The evidence always goes in the bucket. The one choice is where the index goes: into a SQL database reached through `database/sql`, or into that same bucket. Four stores ship, all pure Go, so the binary still cross-compiles to four platforms without CGo.

**SQLite** is the default and needs no server — one binary, one file:

```yaml
store:
  driver: sqlite
  path: /var/lib/wsaw/wsaw.db      # empty uses the platform state directory
```

It is the fastest of the four to read, and the file opens in any SQL tool.

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

**`blob`** puts the index in the bucket too, so there is no database at all — one binary, one bucket. It takes no `path` and no `dsn`, and the artifact location *is* the store, so one of `artifactURL` and `artifactDir` is required and there is no default:

```yaml
store:
  driver: blob
  artifactURL: "${env:WSAW_ARTIFACT_URL}"   # s3://wsaw-evidence?region=eu-central-1
```

What it buys and what it costs are both worth reading before choosing it: [the store with no database](#the-store-with-no-database-the-index-in-the-bucket), below.

**Choosing between the four.** SQLite if you want one file, the fastest reads, and the ability to open your history with any SQL tool. A server database if you would rather back up and replicate results the way you do everything else. `blob` if you would rather have no local state and no database process anywhere, and can pay per-request latency for it.

The DSN belongs in a secret reference — it carries a password, and wsaw redacts it everywhere a webhook token is redacted. Anything driver-specific (TLS mode, connect timeout) goes in the DSN itself rather than being re-invented as wsaw settings.

The schema is created and migrated by wsaw on startup, forward-only, and a store written by a newer wsaw is refused rather than misread. `blob` has no schema; it records its index layout as an object in the bucket, and refuses a layout written by a newer wsaw on exactly the same principle.

A setting that belongs to a driver you are not using — a `dsn` left behind after a switch to `blob`, a `path` on `postgres`, a connection-pool size on either — is a configuration error naming the line, never a setting quietly ignored. Somebody who left one there has one idea about where their history is kept and wsaw has another, and only one of them can be right.

A store reached over a network — a server database, or a bucket — retries a transient failure rather than turning it into a lost result:

```yaml
store:
  maxAttempts: 3       # 1 disables retrying
  retryBackoff: 200ms  # doubles per attempt
```

A permanent failure, such as a constraint violation, is never retried: that would only make it slower and hide the cause. Every retry is logged and counted as `wsaw_store_retries_total`, because a store that flaps while each scan quietly succeeds on the second attempt is worth knowing about before it becomes an outage.

**No store makes wsaw multi-node.** Moving the index off the local disk removes the file and its lock; it does not remove the constraint. Two instances sharing one database, or one bucket, still duplicate every scheduled scan and can still disagree about a baseline. What a remote store does change is that it becomes a network dependency, so readiness fails when it is unreachable — a wsaw that cannot record what it observed is not ready, however healthy its browser is.

State lives in the platform's directory by default (`$XDG_STATE_HOME/wsaw` on Linux, `~/Library/Application Support/wsaw` on macOS) and is created `0700`.

#### The artifact bucket

The bucket holds everything with bytes in it: each scan's JSON document, its screenshots, and any response bodies it stored. Every object is named `kind/sha256-of-its-bytes`, so identical bytes are one object however many scans captured them, and the index records the reference. Listing results, rendering the dashboard and answering the API's index endpoints are served from the index alone; a document or a screenshot is fetched only when somebody asks for one.

Two settings can name the bucket, and at most one of them may be set:

```yaml
store:
  artifactDir: /var/lib/wsaw/artifacts                   # a directory on local disk
```

```yaml
store:
  artifactURL: "s3://wsaw-evidence?region=eu-central-1"  # object storage
```

Setting neither is the default, and is what the single-binary deployment wants: artifacts land in an `artifacts` directory beside the database file, or beside the state directory when the store is a server database. Nothing to configure, no external service. The `blob` store is the one exception, because there is no database file to sit beside and the bucket is not merely where the evidence goes but the store itself: it has no default and one of the two settings is required.

`store.artifactDir` means exactly what it always meant — a directory on local disk — so an upgrade needs no configuration edit. It is now served by the same code path as a bucket, with the same key layout the directory implementation used, so an existing artifacts directory is read and written unchanged, with nothing moved. A URL written into `artifactDir`, or both settings set at once, is a configuration error naming the line: wsaw does not pick one and leave the other looking as though it were in force.

Credentials are not wsaw's business. Each provider's own chain resolves them, which is what lets a deployment use the identity it already has — an instance profile, a workload identity, `az login` — instead of copying keys into a file wsaw reads. If a URL of yours has to carry a credential anyway, write it as a secret reference (`artifactURL: "${env:WSAW_ARTIFACT_URL}"`); wsaw then keeps it out of its logs the way it keeps a DSN out of them. Either way, the printable form of the URL has its password and its whole query string removed before wsaw logs it, prints it in `wsaw config`, or names it in an error.

**A local directory** — the default. No credentials. wsaw creates the directory `0700` and each artifact `0600`, so evidence is readable only by the account wsaw runs as; that account needs write access to the parent directory.

```yaml
store:
  artifactDir: /var/lib/wsaw/artifacts
```

**S3.** The host is the bucket; the region belongs in the URL because the SDK will not guess it.

```yaml
store:
  artifactURL: "s3://wsaw-evidence?region=eu-central-1"
```

Credentials come from the AWS chain: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (plus `AWS_SESSION_TOKEN` for temporary credentials), or `~/.aws/config` with `AWS_PROFILE`, or the role attached to the machine — an instance profile on EC2, IRSA or Pod Identity on EKS. The bucket policy needs `s3:PutObject`, `s3:GetObject` and `s3:DeleteObject` on `arn:aws:s3:::wsaw-evidence/*`, and `s3:ListBucket` on the bucket itself. Deletion is needed for retention and `s3:ListBucket` for the sweep; a wsaw that may only write will start, and then fail its first prune.

**Google Cloud Storage.**

```yaml
store:
  artifactURL: "gs://wsaw-evidence"
```

Credentials come from Google's application default credentials: `GOOGLE_APPLICATION_CREDENTIALS` pointing at a service-account key, `gcloud auth application-default login` for a workstation, or the attached service account — Workload Identity on GKE, the default service account on GCE. The service account needs `roles/storage.objectAdmin` on the bucket, which covers creating, reading, listing and deleting objects without granting anything over the bucket's own configuration.

**Azure Blob Storage.** The host is the container, not the storage account; the account is named by the environment.

```yaml
store:
  artifactURL: "azblob://wsaw-evidence"
```

Set `AZURE_STORAGE_ACCOUNT`, and then one of: `AZURE_STORAGE_KEY` for a shared key, `AZURE_STORAGE_SAS_TOKEN` for a SAS token, `AZURE_STORAGE_CONNECTION_STRING`, or nothing at all — with none of them set the default Azure credential chain is used, which is what a managed identity or an `az login` session arrives through. The identity needs **Storage Blob Data Contributor** on the container. A SAS token is a credential in an environment variable, and it expires: when it does, wsaw fails its startup probe rather than losing evidence quietly.

**An S3-compatible endpoint — MinIO, Ceph, and the rest.** Same driver, with the endpoint named and path-style addressing turned on, since these services rarely do virtual-host buckets:

```yaml
store:
  artifactURL: "s3://wsaw-evidence?endpoint=https://minio.example.internal:9000&use_path_style=true&region=us-east-1"
```

Credentials are still the AWS ones: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` hold MinIO's access key and secret key. `region` is required by the SDK even where the service ignores it. The bucket needs the same four permissions as S3; MinIO's built-in `readwrite` policy scoped to the one bucket is enough. For a service on plain HTTP inside a private network, add `&disable_https=true` — and know that the evidence then crosses that network unencrypted.

**`s3://`, `gs://` and `azblob://` resolve only in a build made with `-tags cloudblob`.** The three cloud SDKs cost more than the rest of wsaw put together — the released `linux/amd64` binary is about 26 MB by default and about 56 MB with them, and `dist/SIZES` carries the figure for every platform of every release — so which providers are compiled in is a deliberate decision rather than a default (Story 8.8). The default binary speaks `file://` and plain directory paths and nothing else: it opens no cloud SDK and resolves no credential chain, so a local deployment does not acquire cloud behaviour by being linked against it. Pointed at a scheme it cannot open, either build fails at startup, names the URL, says which build it is, and lists what that build does support. Both builds are released; the tagged one is named as such.

wsaw writes to the bucket while it is starting, and removes what it wrote. A bucket that is unreachable, that does not exist, or that refuses writes — a read-only policy, a credential with read scope, an expired SAS token — fails the start with a message naming the bucket, rather than turning tonight's first scan into evidence nobody can store. Reachability then stays part of readiness: `/api/v1/ready` reports not-ready when the bucket has stopped answering, on the same argument the store is checked on, because a wsaw that cannot record what it observed is not ready however healthy its browser is.

**Serving what the bucket holds.** Where the evidence sits is not the reader's problem: the API route (`/api/v1/artifacts/…`) and the share-link route serve a screenshot or a stored body from the bucket exactly as they served it from a directory, with the same headers — a stored body stays an opaque attachment that no browser will render, a screenshot stays an image and nothing else. Objects are streamed rather than read into memory, so a forty-megabyte result document costs the daemon a buffer rather than a copy. What bounds such a fetch is progress rather than a stopwatch: the bucket gets a deadline to open the object and a deadline for each further piece of it, so a bucket that has stopped answering is abandoned while a large download over a slow link is served to the end rather than cut off after its headers have gone out. Each response carries its length and a strong `ETag`, which is free to be exactly right because an artifact's key *is* the SHA-256 of its bytes: a browser that already holds a screenshot revalidates it and gets a 304 instead of the megabytes. Evidence retention has removed is reported as evidence that is no longer stored — on the result page as such, on the route as a 404 — never as a broken image or a stack trace. A share link still reaches only the artifacts its own result names, which matters more with a bucket than with a directory: every scan's evidence shares one keyspace, and a key is nothing but a digest.

**Redirecting readers to the bucket, if you want that.** wsaw can answer an artifact request with a redirect to a signed URL instead of copying the bytes through itself. It is off by default and stays off unless you turn it on, because it moves access control from wsaw — which checks the API token, or that a share link actually covers the file being asked for — to a URL anybody holding it can replay until it expires:

```yaml
store:
  artifactURL: "s3://wsaw-evidence?region=eu-central-1"
  artifactSignedURLs: true      # off by default
  artifactSignedURLTTL: 5m      # default 5m, maximum 1h
```

What a redirect also gives up is wsaw's own response headers — but not what they are for: every artifact is written to the bucket as `application/octet-stream`, so a provider hands it over as an opaque download, from the bucket's origin rather than from wsaw's. The lifetime is short, and a redirect issued to someone holding a share link is additionally cut to whatever is left of that link — a link expiring in thirty seconds cannot be turned into five minutes of access to the evidence it names. A signed URL cannot be withdrawn before it expires, which is why the ceiling is an hour rather than a suggestion. Only a provider that can sign gets used this way: turning it on against a local artifact directory is a configuration error naming the line, and a bucket that refuses to sign at runtime is logged once and then served by wsaw itself, rather than failing requests.

**What object storage costs you.** Four things worth knowing before moving evidence off local disk:

- **It is remote, so a write can fail for reasons a disk would not.** A dropped connection, a throttled request, an expired credential, a bucket policy someone changed this morning. wsaw retries what its error code says is transient (`store.maxAttempts`, `store.retryBackoff`), records a failure it cannot recover as a failure rather than as an empty result, and keeps the scan's blast radius to that one scan.
- **Requests cost money and latency.** Every screenshot, every stored body and every result document is one PUT; opening a result in the interface is a GET; a retention sweep lists every key wsaw owns. None of it is expensive by object-storage standards, but it is not free either, and `wsaw store migrate --dry-run` will tell you how many objects and bytes an upgrade is about to write before you find out from an invoice.
- **A lifecycle rule on the bucket will delete evidence behind wsaw's back.** If you set one — a transition to a cold tier or an expiry after 30 days — it applies to objects wsaw's index still references, and there is no way for wsaw to object. Screenshots start coming back as evidence that is no longer stored, and a baseline can lose the scan it approved. Let wsaw's own retention (`store.maxAge`, `store.maxPerSeries`) decide what goes, and leave expiry rules off the bucket it owns. A cold-storage transition is the same trap in slower form: a restore is not a GET, and wsaw will not wait for one. It has a second, quieter effect: a *result document* removed that way leaves a result whose evidence wsaw can no longer enumerate, and while such a result exists no screenshot and no stored body is ever reclaimed again. `wsaw store prune` prints how many are in that state, and [retention](#retention-reclaims-what-it-stops-referencing) below says what to do about it. With the `blob` store the warning is sharper still, because the index is in the bucket as well: index objects are small and, once a history settles, old — precisely what an expire-by-age rule is written to catch — so exclude the `_wsaw/` prefix from every lifecycle policy.
- **The bucket, or a prefix that is wsaw's alone, must be wsaw's alone.** A retention sweep walks it. It deletes only keys of the shape wsaw writes evidence under — `body/<sha256>`, `screenshot-…/<sha256>`, `result/<sha256>`, `probe/<sha256>` — and counts anything else as "not written by wsaw" and leaves it, index objects under `_wsaw/` included. But two wsaw deployments sharing one bucket write the same shapes, and each one's sweep would then collect the other's evidence. Give each deployment its own bucket. Note that a path in the URL is *not* a prefix: `s3://bucket/wsaw` is refused at startup, because every provider takes the bucket from the host and silently drops the path.

#### The store with no database: the index in the bucket

`store.driver: blob` keeps the index in the same bucket as the evidence — one small object per scan, per baseline decision and per audit entry, all under the `_wsaw/` prefix, and nothing anywhere else. There is no database to run, patch, back up or fail over. It exists for the deployment where a database is an entire piece of infrastructure kept alive to hold a few thousand small records, which is what running wsaw in Kubernetes against object storage usually amounts to.

```yaml
store:
  driver: blob
  artifactURL: "s3://wsaw-evidence?region=eu-central-1"
  maxAge: 2160h
  maxPerSeries: 200
```

Nothing in that index is ever overwritten. Every write goes to a key nobody else writes, and every read is a fold over the keys the bucket currently shows. That is what makes it safe on a service that offers no transaction and no portable compare-and-swap — and it is where most of what follows comes from.

**What it costs.**

- **No ad-hoc queries.** It answers exactly the questions wsaw asks — a target's history newest-first, one scan by ID, the scan before a given one, the baseline for a target and mode, the audit log, and retention — because the key layout was built for those and for nothing else. There is no SQL, no reporting tool, and no way to ask a new question without a new key. If you expect to query your own scan history, choose SQLite or Postgres.
- **Every read is requests and latency, on every path.** Listing a page of results is one listing plus one small read per row; opening a result is two reads; the targets page pays that for every series it shows, plus one further listing each to learn whether a baseline exists. None of that is expensive by object-storage standards, but a page that renders instantly against SQLite takes a couple of hundred milliseconds here, and every read is a line on the bill. The per-path request counts are asserted by a test rather than estimated, so a read path that quietly became more expensive fails the build.
- **Retention walks each target's history.** The hourly prune described [below](#retention-reclaims-what-it-stops-referencing) costs a listing per target and consent mode against this store, every hour, plus the deletes for whatever actually expired. There is no interval to turn down; what you can change is how much there is to walk, and `wsaw store prune --dry-run` prices a policy before you set it.
- **A long history is compacted for you, and there is nothing to configure.** Left alone, a series' listing would grow by one key per scan for ever. So once a target's history passes a thousand loose entries, wsaw folds the older ones into a single checkpoint object and leaves the newest two hundred loose; the keys a checkpoint covers are deleted only once the bucket's own clock says that checkpoint has been sitting there for a day. Reading a whole thousand-scan history then costs one listing and about two hundred reads instead of a thousand. It is checked on the first scan a process stores for a target and every thousandth after that, the thresholds are constants rather than settings, and a pass that fails never fails the scan that triggered it. What it costs is that roughly one scan in a thousand takes noticeably longer to store; it logs `compacted a target's history in the bucket index` when it happens.
- **It is still single-node, and the disagreement lasts longer here.** That no store makes wsaw multi-node is said above and is not changed by this one; what differs is what happens on either side of that line. Two instances against one bucket cannot corrupt the index — nothing is ever overwritten, so two writers produce two keys rather than a lost update, which is more than a shared database gives you. But the gap between one instance recording an approval and the other's listing showing it is as long as the provider takes to converge, rather than as long as a transaction, so the window in which they disagree about a baseline is wider and has no upper bound wsaw can state. This store removes the database, not the constraint.
- **A just-written result may not appear in a listing immediately — not even to the process that wrote it.** Object storage does not promise that it will, and wsaw does not paper over it by remembering its own writes: every read is a fold over what the bucket currently shows, so every reader gets the same answer at the same moment. A scan inside that window is reported as one that is not there yet and never as one that was deleted, and it is readable by scan ID throughout — it is the listing that lags, not the store. The consequence worth knowing: a scan written by `wsaw scan` while the daemon is running may not be what the daemon's next comparison for that target is made against; it will compare against the scan before it instead, and nothing detects that, because from outside the bucket the newer scan simply is not there yet. What *is* detected is an index that is visibly incomplete rather than merely behind — a key a listing showed and a read then could not find — and that fails the read and says so rather than quietly returning a history one scan short.
- **Two operators approving different baselines in the same moment both succeed.** Each approval is its own object, the fold picks the later one deterministically, and the other stays in the audit log rather than vanishing. A baseline is a compliance decision, so last-writer-wins on a mutated object — where the loser leaves no trace — is not an acceptable failure mode. Withdrawing one is not symmetrical with approving it, and deliberately so: an approval that loses that race leaves the previous state standing and silences nothing, while a withdrawal that loses would leave findings silenced against a baseline you were told was gone. So a withdrawal reads the log back after writing, and reports `the withdrawal … did not take effect` rather than reporting success — retry it, and the second attempt takes.
- **An interrupted approval can leave the audit log one entry behind, and it catches up on the next decision rather than on the next read.** The approval itself is never at risk: the decision and the audit entry it explains are one object under one key, so there is no interruption that records one without the other. What can be missing is the copy filed under `audit/` that the log view is read through, and it is re-created by the next approval or withdrawal of that same target. If a target takes no further baseline decision, the gap stays until [`wsaw store rebuild-index --verify`](#rebuilding-an-index-from-the-bucket) finds it, which is one of the things that command is for. Healing it on every read of the log would mean listing every target's decisions on every render, to repair something that never endangers the record.
- **The `_wsaw/` prefix is the one thing you still have to back up.** A scan document decodes to the scan it records, so the result index can be rebuilt from the evidence. Baselines, their approvals and the audit log cannot be rebuilt from anything, because they are decisions rather than properties of a scan. Turn on bucket versioning, or back that prefix up. (Share links need no backup — they are signed by `api.share.key`, not stored.)
- **A local directory is the wrong home for it at scale.** The local file bucket is a file per object, and this store writes several small objects per scan, so a few million results become several million files: you exhaust inodes long before you fill the disk, and a 450-byte index object rounded up to a filesystem block wastes most of the space it occupies. On local disk, SQLite is the right store. The bucket index is for object storage — or for a small deployment that values one binary above everything else.

**Moving between store kinds.** There is no migration between the SQL stores and this one, in either direction, and there will not be one. All four keep their documents in the same bucket layout, so what does not port is the index — and an index is derived. The way across is to point the new store at the same bucket and run [`wsaw store rebuild-index`](#rebuilding-an-index-from-the-bucket), which is also the recovery procedure for an index that was lost rather than moved. `wsaw store migrate` against `blob` says so rather than pretending to convert anything: there is no schema here to migrate.

One thing survives a switch away from `blob` and is worth knowing about rather than discovering: the `_wsaw/` objects stay in the bucket, and a SQLite or Postgres deployment pointed at it afterwards will never remove them. A sweep deletes only keys shaped like an artifact, so it counts the whole index as something it does not own and leaves it — `foreign: N objects in the bucket were not written by wsaw and were left alone`. Read that line as "not an artifact" rather than "not ours"; the objects are wsaw's own, from the store that used to run here. That is the safe behaviour and the deliberate one, since a bucket that still holds a readable index is a bucket you can point `blob` back at. Deleting the prefix is therefore a decision for a person, not for a sweep — and it is the last copy of every baseline and every audit entry, which nothing else can rebuild.

#### Retention reclaims what it stops referencing

Retention is configured exactly as it always was — `store.maxAge` and `store.maxPerSeries` — and pruning now deletes the artifacts the results it removed were the last to reference: their documents, their screenshots, their stored bodies. The daemon does it hourly and reports `wsaw_results_pruned_total`, `wsaw_artifacts_deleted_total` and `wsaw_artifact_bytes_freed_total`, so whether a bucket is being kept in bounds is a number rather than an impression.

What is *not* deleted matters as much. Artifacts are content-addressed, so two scans that captured identical bytes share one object: deletion is decided by which results still reference a key, never by how old the key is. An artifact another result still names is kept, and so is one the baseline's own copy of an approved scan names — a baseline is never pruned, and neither is the evidence it points at.

Two commands, both with a dry run, because deleting evidence does not come back:

```
wsaw store prune --dry-run    # what the configured retention would remove, and the bytes
wsaw store prune              # apply it
wsaw store sweep --dry-run    # artifacts nothing references any more
wsaw store sweep              # collect them
```

A dry run prints the first 20 entries with an exact count; `--limit=N` prints N of them and `--limit=0` prints all of them, which is what to use before applying a retention change you have not seen the consequence of.

`prune` is what the daemon does on its own every hour. `sweep` is not: it walks every key in the bucket, which against object storage is a request per page, so it is asked for rather than scheduled. It exists for what a prune cannot see — an object left behind by a scan that was interrupted between writing the bucket and recording its index entry, and a key an earlier prune's delete was refused.

Both are safe to run while wsaw is scanning. An unreferenced object is left alone if it was written in the last 24 hours, and also if a scan running now has *taken* it: artifacts are content-addressed, so a scan that captures an unchanged asset writes nothing at all — the key is already there, dated by whichever scan first stored those bytes — and wsaw records the take so that neither a prune nor a sweep can collect an object the scan in progress is about to reference. Objects in the bucket that wsaw did not write are counted separately and never deleted.

A result document is never collected, even when no index entry points at it, and is counted and reported instead. A document decodes to the scan it records, so it is something [an index rebuild](#rebuilding-an-index-from-the-bucket) can recover from rather than garbage; an interrupted write, or an index that is behind the bucket, is what a run of them looks like. A sweep also stands down entirely while a rebuild is running — the rebuild leaves a marker object in the bucket, and until it is cleared nothing is collected, because half a rebuilt index makes the other half's evidence look unreferenced.

A sweep refuses to walk the bucket at all if the store's index holds nothing — no results, no baselines, no references, no takes. An index that knows nothing cannot tell garbage from a year of evidence, and that is what a database restored without its bucket, or a fresh store pointed at an existing one, looks like from inside a sweep. `--allow-empty-index` says the empty history is real and sweeps anyway, and it is the one thing that lifts the protection above: with it, orphaned result documents are collected too. A listing that is merely lagging looks exactly like an index that is genuinely absent from out here, so use the flag only when you know which of the two you have — never as a way to make a refused sweep run. There is no undo: `wsaw store rebuild-index` can put a result index back from the documents, and with `--allow-empty-index` the documents are exactly what the sweep will have deleted.

Neither command runs at all if the bucket is unreachable. A deletion against a bucket that has gone away — an unmounted volume, an expired credential — answers "already gone" for every key, which would be reported as a successful reclaim and would clear the very records that say those objects still need collecting.

A bucket that refuses a delete does not fail the prune. The key is counted as `wsaw_artifact_deletions_failed_total`, its reference is kept as the record that it still needs collecting, and the next sweep meets it again.

One case holds artifact collection back on purpose. If a stored result's document has gone missing from the bucket or no longer decodes, wsaw cannot know which screenshots and bodies that result named — so it will not declare any screenshot or body unreferenced while such a result exists, and says so in the prune's output ("unknown: N results do not say which artifacts they reference"). Result documents are still collected, because the index records where its own document went regardless. Absence of a reference is not evidence of an unreferenced artifact.

That state is sticky, and worth knowing about: it lasts as long as the affected results do, so a bucket lifecycle rule that removed one result document stops screenshot and body reclamation for the whole store until they are gone. Deleting them — tightening `store.maxAge` so they expire, or removing them from the store — is the remedy available today.

#### Rebuilding an index from the bucket

The bucket holds the record. The index — a pointer per scan plus the handful of counts a listing shows — is derived from it, and `wsaw store rebuild-index` re-derives it: every object under the `result/` prefix is read, decoded, and turned into the same index entry and the same summary the scan that stored it wrote, by the same code.

```
wsaw store rebuild-index --dry-run    # what it would add, and what it would cost
wsaw store rebuild-index              # do it
wsaw store rebuild-index --verify     # change nothing, report every disagreement, exit non-zero on any
```

It works for all four stores. Which one you are running is the only thing that differs.

**When to run it.** A database dropped, restored from a backup older than its bucket, or restored without one. A result you can see in the bucket and not in the interface — the state an interrupted scan leaves, where the document landed and the index entry never did. A summary column that a wsaw upgrade computes differently from the one that wrote it — for a SQL index; see the note below for what the `blob` store can and cannot do about that. And moving between store kinds, which is not a migration but this: point the new store at the same bucket and rebuild.

**Re-deriving a summary is a SQL-store capability.** Where the index is rows, a rebuild re-derives every summary column from the document and the change is applied to the whole history by running the command. Where the index is objects it cannot be: an entry's key is a function of the scan it records, the key is already written, and no index object in this store is ever rewritten — that rule is what makes concurrent writes safe and torn reads impossible, and a recovery command is the last place to make an exception to it. So a `blob` deployment's summaries are fixed at write time. A rebuild reports the disagreement (`stale: N carry a summary the document no longer produces`) and changes nothing, and `--verify` reports it too. The documents are intact and every read of a *result* is derived from them afresh, so what is affected is the listing's columns and nothing else; putting a new derivation into the listing means re-storing those scans.

**What it restores.** Every scan the bucket still holds a document for and the index has no record of having removed: the listing, the ordering, the summaries, the artifact references, and the reverse index retention decides deletions from. Documents are content-addressed and are read back byte-identically, so a rebuilt result is the result — not a reconstruction of one.

**What it does not put back is history retention deleted.** Pruning removes a result and keeps the artifacts something else still names, so a scan a baseline was approved from leaves its document in the bucket for ever after the result itself has expired. A rebuild that treated "there is a document" as "there should be an entry" would undo a deletion made to satisfy a retention policy — quietly, and reported as work done. It does not: the index records the removal (a tombstone in the series directory for `blob`, the artifact references a pruned result leaves behind for the SQL stores), and those documents are counted and named in a category of their own, `pruned:`, rather than folded into what the run would add. `--verify` does not call them drift either, so a correctly pruned store stays green. The documents are still in the bucket; nothing was lost, and nothing came back. Where the index itself is genuinely gone — a dropped database, a deleted `_wsaw/` prefix — nothing records what was pruned and a rebuild recovers the whole bucket, which is the recovery the command is for.

**What it cannot restore — read this before you rely on it.** Baselines and their approvals, the audit log, and change-event history are decisions and observations *about* scans, not properties of them, and no stored document contains one. A rebuild preserves them where they still exist and reports exactly what it found where they do not, prominently and at the top of its output, rather than handing you a store that looks intact and quietly reports every target as never having been approved. If your index is gone, they are gone with it: restore them from a backup of the index, or approve them again. This is why [the `blob` store's own section](#the-store-with-no-database-the-index-in-the-bucket) tells you to version or back up the `_wsaw/` prefix — it is the only copy. Share links need no restoring; they are signed with `api.share.key` and are not stored anywhere.

**It adds and never removes.** There is no swap and no "clear, then rebuild": the run merges into whatever index is there, so an abort at any point leaves a store you can read, and running it again finishes the job without repeating a single write. It does not run the `blob` store's compaction either, which is the one thing in this store that deletes index objects — a rebuild leaves a series exactly as compactable as it found it, and the next stored scan compacts it. An entry it cannot find a document for is reported and **left where it is** — a bucket that lost an object and a listing that has not caught up look identical from out here, and only one of them is a reason to delete a record of a scan. Deciding that is a person's job, and `--verify` is how they see it.

**A missing screenshot is recorded, not tidied away.** A rebuilt result whose screenshots or stored bodies are gone from the bucket keeps its references and reads as evidence that is no longer stored. Dropping the reference would turn a scan whose evidence was deleted into a scan that captured none, which is the one inference this tool must never make. The run says how many it found.

**A damaged object is named and skipped.** An object under `result/` that does not hash to the key it is stored under, or that will not decode, is reported by key, counted, and passed over. One unreadable object out of a hundred thousand does not stop the other ninety-nine thousand from being indexed, and it does not go missing from the summary either. A rebuild carries on and exits zero; `--verify` exits non-zero, because a truncated or tampered document is evidence this store can no longer produce and a scheduled check must not be green over it.

**A document a newer wsaw wrote is refused rather than re-derived.** A result document carries the schema version of the build that wrote it. An older binary decoding one drops every field it does not know, and the summary it would derive is short by exactly those fields — so a rebuild by the old binary would rewrite the history downwards and print it as a repair. It refuses instead: the documents are counted, named with their schema version, left entirely alone, and `--verify` exits non-zero over them. Upgrade wsaw and run it again. This is the same rule a store applies to a newer database schema and the `blob` store to a newer index layout.

**It also tells you what is in the bucket.** A rebuild is the one operation that sees every key, so it reports what wsaw wrote, what no result references, and what wsaw did not write, in objects and bytes — the cheapest place to learn whether `wsaw store sweep` has anything to do.

**`--verify` is the scheduled one.** It writes nothing and compares the two in both directions: index entries naming documents that are gone, documents with no index entry, summaries that no longer match their document, and — for the `blob` store — a baseline decision whose copy in the audit log never landed, which is the one gap [described above](#the-store-with-no-database-the-index-in-the-bucket) that nothing else looks for. It exits non-zero on any of them, and on any object it could not read or could not understand, so it belongs in a cron entry or a CI job rather than in the recollection of whoever handled the last incident.

Two things it deliberately does *not* exit non-zero over, because a cron job that is permanently red is a cron job nobody reads. Evidence a lifecycle rule expired — a screenshot or a stored body the result still names and the bucket no longer holds — is the recorded outcome the rest of this section describes, and is reported by count and by key without changing the exit code. And a scan retention removed is not a disagreement: see "what it does not put back" above.

`--verify` and `--dry-run` also do not migrate the store's schema on the way in. If the schema is behind this build they refuse and name `wsaw store migrate`, rather than performing the one-way upgrade in the middle of a command that promised to change nothing.

**Running it while wsaw is running is safe, and it does not lock anything out.** A rebuild only ever creates records a scan would have created itself, at keys derived from the scan, so a result stored while it runs is either seen by the walk and written identically or not seen and already written by the scan — never lost, and a later run picks up whatever the first did not see. What it does take out is `wsaw store sweep`: the rebuild leaves a marker object in the bucket, and while that marker is there every store's sweep collects nothing and says so, because half a rebuilt index makes the other half's evidence look like garbage. The marker is cleared when the run ends, including when it is interrupted. A process that is killed outright cannot clear it, so a marker is also ignored once it is more than a day old — a rebuild that started yesterday is not running — and the sweep logs loudly when it steps over one. Until then the sweep names the key, and deleting that object clears it immediately.

**What it costs.** A rebuild reads every stored document, so its cost is proportional to the whole history rather than to what is wrong with the index:

- one GET per stored document, and the bytes of the entire history transferred;
- one small index read per scan already recorded, and for the `blob` store one listing of each series' directory plus a read of each of its checkpoints — per series, not per scan, so that a long history is not charged for the same directory thousands of times;
- one existence check per screenshot and stored body the results name (nothing extra when body and screenshot storage are off, which is the default);
- for each scan it adds, one row for a SQL index, or three small objects plus one per artifact named for the `blob` index;
- listings: one request per 256 keys of the bucket and per 1,000 index keys, twice over — once for the documents and once for the closing survey of the bucket.

An interrupted run costs that again. It writes nothing twice — every record is keyed by the scan it describes, so the second run finds what the first wrote and skips it — but it re-lists and re-reads the whole bucket to find out, because the documents are the source of truth and a run that trusted the index about which of them it had already seen would be deriving from the thing it is repairing. Cheap in writes, full price in requests and egress.

For a history of 100,000 scans averaging 500 KB, that is roughly 100,000 GETs, a comparable number of small reads, a few hundred listings, and 50 GB transferred. The requests are cents; the 50 GB is the number to look at, and it is egress if the machine running the command is not in the same region as the bucket. `--dry-run` prints the same figures without writing anything, and every run prints exactly what it used. `--read-concurrency` (default 8) is how many documents are read at once — raise it against a fast link, lower it if your provider starts rate-limiting. Index writes stay serial whatever it is set to.

#### Upgrading a store that predates the bucket

Earlier versions of wsaw kept each result's JSON document in a column. The first start of this version moves them: every stored document is written to the artifact bucket, referenced from its own row, and summarised into the columns a listing reads, and only then is the column dropped. Nothing is discarded — a store holds months of evidence, baselines pinned to particular scans, and share links pointing at results, so discarding it would destroy the history the tool exists to keep.

Look before you leap:

```
wsaw store migrate --dry-run    # how many documents, how many bytes, and to where
wsaw store migrate              # do it, and report what was done
```

The dry run moves no document and touches no row. It reaches the database and the bucket — that is how it learns anything at all, and opening a local artifact bucket creates its directory — but it applies no schema change, not even the one it reports on. It exists so that the size of the move and the destination are known before a maintenance window is chosen; against object storage those numbers are also what the first month's bill is made of.

What to expect from the migration itself:

- It runs in bounded batches and logs its progress, so a store with a hundred thousand results advances visibly rather than appearing to hang. It never holds one transaction open across the whole table.
- It needs a writable bucket, and checks that first. An unreachable or read-only bucket fails the start with a message naming it, before a single row has been touched.
- It can be interrupted. Each row is moved and recorded in one statement, so starting again continues where the last run stopped; documents are content-addressed, so a document written twice is still one object.
- A row that will not move — a document that is not valid JSON, or a bucket that keeps refusing the write — is reported with its scan ID and left exactly as it is. The column is not dropped while any such row remains, so nothing is lost by fixing the cause and starting again. A document that moves but does not decode keeps its bytes in the bucket, and its row says that its summary could not be derived rather than showing zeros.

**The upgrade is one-way.** An older wsaw cannot read a migrated store: it would look for a column that is no longer there. There is no downgrade migration, and there will not be one — the supported rollback is a database backup taken before the upgrade, restored alongside the older binary. Take that backup. The artifacts the migration writes are harmless to an older wsaw and can be left where they are.

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
make check        # fmt, vet, lint, licences, test -race, and the cloudblob compile
make test-fast    # skip browser tests
make soak         # long-run stability test (Story 6.8)
```

The soak reports what the artifact bucket cost the run — requests and bytes,
in total and per scan — beside its memory and goroutine figures.
`WSAW_SOAK_STORE=blob make soak` measures the same thing for the store that
keeps its index in the bucket, whose per-scan request count is several times
the SQL store's because every read and write of the index is a request too.

### Testing the store

The store's test suite is the specification of what a store does, so it is run
against every combination that ships rather than against the default one. Two
environment variables select the combination, and they are independent: one
says where the **index** is kept, the other where the **evidence** goes.

```sh
make test-store-blob      # index in the bucket instead of in rows
make test-store-memory    # evidence in a bucket that has no files
make test-store-postgres  # index in PostgreSQL, in a container
make test-store-mysql     # index in MySQL, in a container
make test-store-minio     # evidence in MinIO — a real S3-compatible service
make test-store-all       # the fast suite and then all of the above
```

```sh
WSAW_TEST_STORE_DRIVER=sqlite|postgres|mysql|blob   # where the index is
WSAW_TEST_ARTIFACT_BUCKET=file|memory|<bucket URL>  # where the evidence is
```

The first two need nothing at all. The other three start what they need and
take it down again, so they want a container runtime — podman, or docker with
`STORE_TEST_RUNTIME=docker` — and fail rather than skip without one. What skips
with a reason is the test package itself, run on its own:
`go test -tags cloudblob,objectstore ./test/e2e/objectstore/` starts a MinIO if
it can and says why it did not if it cannot.

`test-store-minio` is worth its twenty seconds: it is the only run that meets an
object store nobody here wrote, and a bucket is not a directory. Listing order,
pagination, modification-time granularity, delete semantics and error codes all
differ, and the differences are asserted in `test/e2e/objectstore/`.

The fast suite runs **without Chrome installed** — browser tests skip themselves and say why. Integration tests use local fixture servers, including a synthetic consent banner and a synthetic third-party host; they never touch a live third-party website, so CI does not depend on someone else's site staying unchanged.

Contributors and coding agents: read [`AGENTS.md`](AGENTS.md), [`architecture-tenets.MD`](architecture-tenets.MD) and [`non-functional-requirements.MD`](non-functional-requirements.MD) first. The tenets are binding, not advisory.

## Limitations

Deliberate non-goals for this version:

- **No distributed operation.** One process, one host. Evidence may live in a bucket, and the index may live in a server database or in that same bucket, but two instances against one of them still duplicate every scheduled scan.
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
