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
- **Chrome's own sandbox depends on the image.** The default, `chromedp/headless-shell`, forces `--no-sandbox`, so the container is the only boundary. The image in [`deploy/browser`](deploy/browser/Dockerfile) keeps the sandbox, so a hostile page has two boundaries to cross. Every result's `browserSandbox` says which it was.

### Keeping Chrome's sandbox inside the container

`deploy/browser` builds Alpine's `chromium-headless-shell`, the same Chromium the wsaw image bundles, with its namespace sandbox on and running as an unprivileged user. It is published with each release as `ghcr.io/pflege-de-labs/wsaw-browser`, or built locally with `make docker-browser`. Pin it by digest like any browser image:

```yaml
browser:
  container:
    image: "ghcr.io/pflege-de-labs/wsaw-browser@sha256:..."
    # Docker only: its default seccomp profile refuses the sandbox. Rootless
    # Podman's default allows it and needs nothing here.
    extraArgs: ["--security-opt", "seccomp=/path/to/deploy/chromium-seccomp.json"]
```

- Where the sandbox cannot be built, Chromium **refuses to start** rather than running without it, so a scan fails with a reason instead of silently losing a boundary.
- The image declares the sandbox with the label `de.pflege.wsaw.browser.sandbox=enabled`, which is how wsaw knows to record `browserSandbox: true`. A `--no-sandbox` (or another sandbox-weakening switch) in `browser.container.browserArgs` makes the scan unsandboxed for the record, whatever the label says.
- Switching images changes the browser that renders a target, so expect differences in the first scans after the switch that come from the browser rather than from the site.

## Install

```sh
make build            # ./dist/wsaw
make build-cloudblob  # ./dist/wsaw_cloudblob, with the S3, GCS and Azure drivers
make release          # both variants, all four platforms, with checksums and sizes
```

Two binaries, because the three cloud SDKs weigh more than the rest of wsaw: take the default one unless you intend to keep evidence in object storage, in which case see [the artifact bucket](#the-artifact-bucket).

Tagged releases publish the same artifacts on the repository's GitHub Releases page, with `SHA256SUMS` and a CycloneDX SBOM per variant. [CHANGELOG.md](CHANGELOG.md) says what each release changed and what it still lacks.

Or run the container. Every release from 0.1.1 on has images for linux/amd64 and linux/arm64 on the GitHub Container Registry, in both variants — `ghcr.io/pflege-de-labs/wsaw` and `ghcr.io/pflege-de-labs/wsaw-cloudblob` — tagged with the full version (`0.1.0`), with major.minor (`0.1`), and, for the newest release, `latest`. A tag can move; for a deployment, pin the digest instead, which the package page and each publish run's summary show:

```sh
docker run --rm \
  --security-opt seccomp=deploy/chromium-seccomp.json \
  -v "$PWD/wsaw.yaml:/etc/wsaw/wsaw.yaml:ro" ghcr.io/pflege-de-labs/wsaw:0.1
```

`make docker` builds the same image from a checkout.

The seccomp profile is what lets Chromium keep its own sandbox in the container, and under Docker it is not optional: Docker's default profile refuses the `clone`, `unshare` and `setns` calls the sandbox is built from, so without it the browser cannot start. [`deploy/chromium-seccomp.json`](deploy/chromium-seccomp.json) is Docker's default with those three and `chroot` allowed and nothing else changed — `make seccomp-profile` derives it from a pinned upstream, and CI checks both that the committed file is what that produces and that the sandbox starts under it. Podman's default profile already allows user namespaces, so under Podman the flag is harmless but not needed. Do not reach for `--privileged`, `--cap-add SYS_ADMIN` or `browser.noSandbox` instead: each one makes the browser start by giving up more isolation than the profile does.

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
| `wsaw store vacuum` | Shrink the SQLite file to the data it holds; `--dry-run` shows what it would reclaim, `--force` ignores the threshold |
| `wsaw store rebuild-index` | Rebuild the index from the documents in the bucket; `--verify` checks it and exits non-zero on drift |
| `wsaw artifacts compress` | Compress the artifacts already in the bucket, in place |
| `wsaw prune` | Apply retention once; `--dry-run` shows what it would delete |
| `wsaw share` | Mint an expiring link to one scan result |
| `wsaw ui` | Open the web interface in a browser, already signed in |
| `wsaw mcp` | Serve the stored results to an LLM client over MCP (stdio), read-only |
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
`detection.severity`, `detection.allowHosts`, `detection.denyHosts`, the
schedule shape (`scheduler.interval`, `cron`, `jitter`, `minInterval`), and the
bucket sweep's schedule (`store.sweep`, `store.sweepInterval`), whose next run
is recomputed from the last recorded sweep, the database vacuum's schedule
(`store.vacuum`, `store.vacuumInterval`, `store.vacuumMinFreeRatio`), recomputed
the same way, and where the interface's
certificate is read from (`api.tlsCert`, `api.tlsKey`) — though not whether TLS
is on at all. Everything else needs a restart. A setting added to wsaw later is
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

A rule without a `verify` expression can never report `applied`, only `unverified` — unless the page itself left evidence. A site that keeps its consent state in Web Storage records the choice there and nowhere else, and a storage write plus a banner that is gone is the same pair of facts a consent cookie plus a dismissed dialog provides.

A rule written for one site binds to what survives that site's next deploy: visible text, `role` and `aria-*`, an author-written `id` or `data-` attribute. Never to class names a bundler generated — those change on every build, and a rule that quietly stops matching is worse than no rule. Where a host-scoped rule no longer matches the host it was written for, wsaw records it as stale in the result and as a warning on the scan.

`verify` proves the CMP recorded the choice; it does not prove the banner closed — a vendor API commonly records consent without ever running the banner's own dismiss handler. When `verify` passes but the banner is still on screen, wsaw retries the rule's click steps with a fuller pointer/mouse event sequence and, failing that, the generic label-matching fallback, before giving up and reporting `banner-visible` rather than `applied`. A rule can name its own banner-gone check with `dismissed`; left unset, wsaw falls back to the same "does anything banner-shaped remain" heuristic the label-matching fallback uses.

## Reading a result

Two fields decide whether anything else on the page can be believed:

- **`termination`** — `idle` means the page went quiet on its own. `timeout`, `request-cap` or `byte-cap` mean the list may be incomplete. `error` or `skipped` mean it is not a result at all.
- **`consent.outcome`** — `applied` (verified), `unverified` (acted, unconfirmed), `not-needed` (no banner, which is common and legitimate), `banner-visible` (the CMP recorded the choice, but the banner stayed on screen even after a fallback click), or `failed`.
- **`consent.cmpKind`** — `vendor` (a CMP product was identified), `bespoke` (a consent UI is present and no vendor matched it: the site's own banner, as far as wsaw can tell) or `none` (no consent UI was found). Only `none` licenses reading a result as a page that never asks for consent. A banner wsaw could not drive is recorded as `bespoke` with a `diagnostic` naming the element, its text and the labels of the controls it offered — which is what writing the missing rule needs.

Not every banner is rendered with the page. An application-rendered one is mounted after hydration or on an idle callback, so wsaw waits for one to appear before concluding there is none; `consent.bannerWait` sets that wait, and `consentBannerWait` overrides it per target.

Every request carries a **`phase`**: `pre-interaction` or `post-interaction`. Third-party hosts in the pre-interaction phase of a `reject`-mode scan are the headline compliance finding.

**"Zero third parties" is a narrower claim than it looks.** Analytics reverse-proxied onto the site's own domain — a collector on `hog.example.com`, a server-side tag container on `t.example.com` — is first-party by registrable domain and never appears in a third-party count. The report names the first-party hosts contacted before the consent interaction for exactly that reason; read them before reading a clean third-party count as "nothing happened".

### Cookies are half the picture

Consent state and analytics identifiers live in `localStorage` on a large class of sites: one that writes `localStorage["cookie-accepted"]` sets no cookie at all. Each scan records Web Storage per origin — key names, value digests and lengths, never values — and the diff reports keys appearing and disappearing the way it reports cookies. A third-party key written in `reject` mode is ranked with a third-party cookie, not below it.

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

The storage dashboard (`/storage`) and its JSON form (`/api/v1/storage`) require a token on every listener, loopback included. They describe the installation rather than the sites it watches — the database driver, where the evidence bucket is, which targets are kept and how much history each holds — and loopback is reachable by every local user and every page a local browser renders. Without `api.token` both answer `403` and name the setting; everything else on a loopback listener keeps working as before.

That token also protects the loopback case, which means normally typing it into a login form. `wsaw ui` skips that: it reads the token from the same config file the daemon runs with — which an operator running the command can already read — and opens the browser at a one-time sign-in link instead of the plain address:

```
wsaw ui                # opens the browser, already signed in
wsaw ui --print        # prints the link instead, e.g. to open over SSH
```

The link is minted by the running daemon on request, is good for one redemption, expires in seconds, and never carries the standing token anywhere a browser keeps history — only the one-time link does, and it is worthless to anyone the moment it is used or the moment it expires, whichever comes first.

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

The board marks a tile for the one question a board can answer at a glance:
**did a third party appear in a scan where the visitor had agreed to
nothing.** A third-party host the site did not contact before is critical in
`reject` and `none` mode, and unremarkable in `accept` mode, where the visitor
agreed to be tracked. Request counts, added or removed assets, a script whose
bytes changed, and a third party the site stopped contacting all rank `info`
on the board — a live site re-deploys its bundles most days, and a board where
every tile is marked is a board nobody reads. Cookies, storage, consent
regressions, denied hosts and degraded scans keep their usual severity, and so
does everything outside the board: `GET /api/v1/targets`, the notifier and the
CI exit code all keep reporting the diff engine's own ranking for the same
scan, so a tile reading `info` and an API reporting `high` are the same
comparison seen through two questions.

Scans in flight are shown as they happen: a `pending` row on the target page, a
list on the dashboard, and `GET /api/v1/running` for anything else. A running
scan exists in no stored result — the store only learns of a scan when it ends —
so without this an idle daemon and a busy one look identical. Pages showing a
running scan reload themselves; pages with nothing running stay still. Starting
a second scan of a target and consent mode that is already scanning is refused
(`409`) rather than queued, because two concurrent scans of one series would
produce two results for the same moment and double the load on the scanned site.

### HTTPS

Give the interface a certificate and a key, both PEM, and it is served over HTTPS instead of plain HTTP:

```yaml
api:
  tlsCert: /etc/wsaw/tls/cert.pem
  tlsKey: /etc/wsaw/tls/key.pem
  tlsReloadInterval: 1m   # the default; at least 10s
```

The pair is loaded before anything listens, so a missing file, a PEM that does not parse, or a key that does not belong to the certificate stops `wsaw run` with the setting and the path, rather than surfacing as a failed handshake later. HTTPS also turns on `Strict-Transport-Security` and marks the session cookie `Secure`.

**A renewed certificate is served without a restart.** wsaw reads both files again every `tlsReloadInterval` and loads the pair when their contents have changed; connections already open keep the certificate they negotiated, and the next handshake gets the new one. SIGHUP loads the pair at once, whether or not the files look changed, and it does so even when the rest of the reloaded configuration is refused. How the usual renewers are picked up:

- **certbot** points the symlinks under `live/` at new files; point `tlsCert` at `fullchain.pem` and `tlsKey` at `privkey.pem`, and either wait for the next check or add `--deploy-hook "pkill -HUP -x wsaw"`.
- **cert-manager** updates the mounted secret by swapping its `..data` symlink. The files' own modification times do not change, which is why wsaw compares contents: the swap is picked up at the next check.
- **A plain `cp`** of a new certificate and key is picked up at the next check, or at once with `kill -HUP`.

A reload that fails leaves the certificate being served where it is. A renewal writes the certificate and the key as two separate writes, so between them the files briefly disagree, and a listener that stopped serving at that moment would turn every renewal into an outage. The same goes for a replacement that does not parse, does not match its key, or has already expired: wsaw logs a warning naming the file and the reason, and tries again when either file changes or on SIGHUP. It logs an error once less than a fifth of the served certificate's lifetime is left — about 18 days of a 90-day certificate, about 5 hours of a 24-hour one — because that is when somebody has to act.

`wsaw config --check` reads the certificate and the key, and fails, naming the setting, if they could not be served: a file it cannot read, a PEM that does not parse, or a key that does not match. A SIGHUP reload runs the same check and refuses the configuration on any of them, so "configuration reloaded" is never logged over a certificate that is not being served. An expired certificate is reported, not refused. `wsaw run` starts with one and logs an error, so the reload logs the same error and goes ahead, and `--check` prints it on stderr and still passes. Run the check as the user the daemon runs as, since the key is usually readable only by that user. No other command reads the files.

Moving the pair to new paths is a reload like any other. Turning HTTPS on or off is not: it changes what the listener is, so it needs a restart, and a reload that tries is refused naming `api.tlsCert` and `api.tlsKey`.

### Scanning a URL that is not a target

Sometimes the question is about one site, once, and it is not worth a line in
the configuration file. With `api.adHocUrls.enabled`, the interface grows a
**Scan a URL** page: type an address, pick a consent mode, get a result.

```yaml
api:
  enabled: true
  adHocUrls:
    enabled: true          # off by default
    # consentModes: [reject, accept]   # default: defaults.consentModes
    # maxPerHour: 20                   # whole deployment, rolling hour
    # allowPrivateHosts: false         # see below
```

The same thing over the API, for a client that wants the result rather than a
page:

```sh
curl -X POST http://127.0.0.1:8712/api/v1/scan-url \
  -H 'Authorization: Bearer '"$WSAW_API_TOKEN" \
  -d '{"url": "https://example.com/", "consentMode": "reject"}'
```

The result is stored like any other, under a name derived from the address —
the same name `wsaw scan --url` derives — so scanning the same address twice
gives you a diff, and an address scanned from the command line and from the
form share one history. The watchboard still lists the configured targets:
typing a URL creates history, not a watched target.

**This is a switch worth understanding before you flip it.** On, whoever can
reach the interface decides what this machine fetches, which is the shape of a
server-side request forgery. So:

- The address is admitted before a browser sees it: http or https only, no
  embedded credentials, and a host that resolves **entirely** to public
  internet addresses. Loopback (by literal or by the name `localhost`), the
  private ranges, link-local — the cloud metadata service with it —
  unique-local, carrier-grade NAT and the other special-purpose ranges are all
  refused, and a name whose answers include one of them is refused rather than
  left to pick which address the browser reaches. `allowPrivateHosts: true`
  turns that off, for a deployment that means "scan our own staging" and knows
  who can reach the page.
- The credentials in `defaults.basicAuthUser`, `defaults.basicAuthPassword`
  and `defaults.extraHeaders` are **not** sent to a typed address. They exist
  to reach your own sites; sending them to an address somebody typed would
  hand them to whoever typed it.
- Everything else is bounded the way a scheduled scan is: the deployment's own
  budget of scans per hour, the `minInterval` floor between two scans of the
  same address, the robots policy, and the refusal to run two scans of one
  series at once.
- It needs the web interface and is refused in read-only mode, and a
  configuration that says otherwise is refused at load rather than ignored.

A scan started this way is labelled `typed-url` in the running list, so an
operator can tell an address somebody typed from a target somebody configured.

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

### When a scan never goes quiet

A scan ends when the network has been quiet for `idleQuiet`, and some trackers
make sure it never is. A time-on-site beacon exists to keep reporting while
nobody does anything: Taboola's fires every 10 seconds for as long as the tab
is open, which against a 10s quiet window restarts the wait a fraction before
it elapses. The page went quiet at 14s, the scan ends on its hard timeout at
90s, and the result is marked truncated over eight repetitions of one beacon.
Raising the timeout cannot help — the heartbeat has no end — and the outcome
turns into a coin toss, `idle` on one run and `timeout` on the next.

So idle detection does not wait for them:

```yaml
capture:
  useDefaultBeacons: true   # the shipped list; the default
  beacons:
    - host: telemetry.example.net
    - host: analytics.example.com   # a host that also serves scripts
      urlPattern: '/collect'        # is matched by path, never wholesale
```

A rule matches on a host, on a regular expression over the URL, or on both
together — and both must then match. Host patterns work like allow and deny
lists: an exact host, a bare domain covering its subdomains, or a leading
`*.`. Rules add up rather than replace, so a target's own session ping goes on
the target:

```yaml
targets:
  - name: app
    url: https://app.example.com/
    beacons:
      - host: app.example.com
        urlPattern: '/api/session/ping'
```

The shipped list covers the periodic beacons known to hold scans open —
Taboola, Outbrain, Clarity, GA4, Matomo, New Relic, Datadog, Sentry,
FullStory, Hotjar, Chartbeat and a few more — and every entry names an
endpoint that exists to receive telemetry. Where a vendor serves its script
from the same host, the rule is scoped by path: excluding a *script* from idle
detection would end the scan before the assets that script loads were ever
requested. `useDefaultBeacons: false` declines the list entirely.

Nothing is filtered out of the result. A beacon is recorded like any other
request — URL, timing, status, party, initiator — and counts against
`maxRequests` and `maxBytes`; it carries `beacon: true` so a reader can see
which requests the scan chose not to wait for. Only the moment the scan stops
changes.

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

## Asking an LLM about the results

`wsaw mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio. An MCP client — Claude Code, Claude Desktop, an IDE
agent — starts it as a subprocess, and its model can then read the store: which
targets are watched, the scans of each, one scan's consent outcome and
requests, what changed against the baseline or the previous scan, and a stored
body or screenshot.

```sh
claude mcp add wsaw -- wsaw mcp --config /etc/wsaw/wsaw.yaml
```

Or, for a client configured with JSON:

```json
{
  "mcpServers": {
    "wsaw": { "command": "wsaw", "args": ["mcp", "--config", "/etc/wsaw/wsaw.yaml"] }
  }
}
```

| Tool | Answers |
|---|---|
| `list_series` | Every stored target and consent mode, and whether it has an approved baseline |
| `list_scans` | The scans of one series, newest first, as summaries |
| `get_scan` | One scan without its request list, with the request count and third-party domains before and after consent |
| `list_requests` | One scan's requests, paged, filtered by party, phase or registrable domain |
| `diff_scans` | One scan compared with the baseline, the previous scan, or another scan, with a severity per change |
| `get_artifact` | A stored body as text (capped, and saying so), or a screenshot as an image |

A scan is named by its ID, by `latest`, or by `baseline` — the copy the
baseline keeps, which retention does not prune.

**It is read-only by construction.** The server is given a view of the store
that has no write methods, and opens the store without applying migrations:
a store whose schema is behind is refused with the command that upgrades it.
A database or artifact directory that does not exist is refused, not created.
It runs beside a daemon on the same store, and it never starts a scan.

Two things to know before handing it to a model:

- **Everything a page wrote is untrusted.** Bodies, URLs, cookie values and
  headers come from the scanned site, which can put text in them that is
  written to steer a model. Every tool that returns page content says so, and
  the server's instructions tell the model not to follow it. Keep your client's
  confirmation prompts on for tools from other servers in the same session.
- **An empty answer is never a quiet failure.** A failed scan is marked
  `"ok": false` with a warning that its request list is incomplete, a paged
  list states its total and whether there is more, a body cut at the cap says
  how much of how much it returned, and a scan or artifact that retention
  pruned is an error naming it.

`diff_scans` uses the default severity rules, as the web interface does, not
a target's configured allow and deny lists. Only stdio is offered; there is no
network listener to secure.

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

With HTTPS on, `wsaw_tls_certificate_expiry_timestamp_seconds` is the Unix time the certificate being served expires, and `wsaw_tls_certificate_reloads_total{outcome="success"|"failure"}` counts attempts to load a renewed one. A check that finds nothing changed is not an attempt, and neither is the load at startup. The gauge is left out on a plain-HTTP listener rather than reading 0:

```
# Less than a day of the served certificate is left. Pick the margin from
# the certificate's lifetime: a day is late for a 90-day one, early for a 24-hour one.
wsaw_tls_certificate_expiry_timestamp_seconds - time() < 86400

# A renewal is on disk but cannot be loaded.
increase(wsaw_tls_certificate_reloads_total{outcome="failure"}[1h]) > 0
```

## Deployment

- **systemd**: [`deploy/wsaw.service`](deploy/wsaw.service), hardened for a process that renders hostile pages. `RestrictNamespaces` is deliberately off: the Chrome sandbox depends on unprivileged user namespaces, and disabling them would push operators to turn off the sandbox instead — trading a real boundary for a nominal one.
- **launchd**: [`deploy/de.pflege.wsaw.plist`](deploy/de.pflege.wsaw.plist).
- **Container**: multi-arch, Chromium bundled and pinned, runs as a non-root user, sandbox enabled — under Docker, with [`deploy/chromium-seccomp.json`](deploy/chromium-seccomp.json) (see [Install](#install)).

### Where results are stored

A result is stored in two halves. The **index** entry names the scan — target, consent mode, scan ID, start time, and the handful of counts a listing shows — and points at the scan's JSON document, which is kept in the **artifact bucket** beside the screenshots and response bodies from the same scan. That keeps the published schema the single source of truth for what a result *is*, and keeps a multi-megabyte payload out of every backup and every replication stream.

The evidence always goes in the bucket. The one choice is which SQL database keeps the index, reached through `database/sql`. Three stores ship, all pure Go, so the binary still cross-compiles to four platforms without CGo.

**SQLite** is the default and needs no server — one binary, one file:

```yaml
store:
  driver: sqlite
  path: /var/lib/wsaw/wsaw.db      # empty uses the platform state directory
```

It is the fastest of the three to read, and the file opens in any SQL tool.

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

**Choosing between the three.** SQLite if you want one file, the fastest reads, and the ability to open your history with any SQL tool. A server database if you would rather back up and replicate results the way you do everything else.

The DSN belongs in a secret reference — it carries a password, and wsaw redacts it everywhere a webhook token is redacted. Anything driver-specific (TLS mode, connect timeout) goes in the DSN itself rather than being re-invented as wsaw settings.

Evidence is stored gzipped where that makes it smaller: most stored bodies — they are scripts, stylesheets and JSON — and every result document, which is the largest and most repetitive thing wsaw writes. No screenshot is, since a PNG is already compressed and wsaw keeps it as Chrome produced it. A compressed artifact is an ordinary gzip object named `kind/sha256hex.gz`, so a local directory stays readable with `zcat` and without wsaw, and the reference in a result is unchanged: it is the digest of the evidence, not of the object holding it. Reading is transparent, both forms are always readable, and nothing is migrated — artifacts written before this stay where they are.

Against object storage the two spellings cost one request when the first guess is wrong, so wsaw guesses from what it writes: with compression on it looks for the packed object first for a body, a probe and a document, and for the plain one first for a screenshot.

```yaml
store:
  compressArtifacts: true   # the default; false stores artifacts as captured
```

`wsaw_artifact_bytes_total` and `wsaw_artifact_stored_bytes_total` report what it saved on your data, which depends on the sites you scan.

Artifacts stored before this existed are read where they lie and are never rewritten behind your back. To apply the saving to a bucket you already have, ask for it:

```sh
wsaw artifacts compress --dry-run   # what it would do, and what it would save
wsaw artifacts compress             # do it, printing a summary that adds up
wsaw artifacts compress --verbose   # and name every artifact as it goes
```

It writes the compressed object, reads it back, and checks it against the digest in the artifact's own key before removing the original — so a rewrite is verified rather than assumed, and every artifact stays readable in one form or the other throughout, which makes the command safe to run while wsaw is scanning. Interrupting it leaves a half-compressed, wholly readable bucket, and running it again picks up where it stopped. An object that is not a wsaw artifact, or one whose contents no longer hash to its own key, is reported and left exactly as it is: a corrupt artifact is a finding, not something to repack.

Against object storage it is not free: every artifact is a GET, a PUT and a DELETE, and the whole history moves twice. `--dry-run` prices it first.

The schema is created and migrated by wsaw on startup, forward-only, and a store written by a newer wsaw is refused rather than misread.

A setting that belongs to a driver you are not using — a `path` on `postgres`, a connection-pool size on sqlite — is a configuration error naming the line, never a setting quietly ignored. Somebody who left one there has one idea about where their history is kept and wsaw has another, and only one of them can be right.

A store reached over a network — a server database, or a bucket — retries a transient failure rather than turning it into a lost result:

```yaml
store:
  maxAttempts: 3       # 1 disables retrying
  retryBackoff: 200ms  # doubles per attempt
```

A permanent failure, such as a constraint violation, is never retried: that would only make it slower and hide the cause. Every retry is logged and counted as `wsaw_store_retries_total`, because a store that flaps while each scan quietly succeeds on the second attempt is worth knowing about before it becomes an outage.

**No store makes wsaw multi-node.** Moving the index off the local disk removes the file and its lock; it does not remove the constraint. Two instances sharing one database, or one bucket, still duplicate every scheduled scan and can still disagree about a baseline. What a remote store does change is that it becomes a network dependency, so readiness fails when it is unreachable — a wsaw that cannot record what it observed is not ready, however healthy its browser is.

### How long results are kept

Retention has two forms. The blunt one draws a line and drops everything past it:

```yaml
store:
  maxAge: 2160h      # 90 days
  maxPerSeries: 200  # newest 200 scans of each target and consent mode
```

That is enough for one target scanned nightly and wrong for anything busier: four scans a day fill 200 in seven weeks, so the age limit never fires and the scan that would show when a tracker first appeared is the one that goes.

The thinning form keeps a history that gets sparser with age, the way `restic forget` does:

```yaml
store:
  keep:
    last: 10        # the newest 10 scans, whenever they ran
    within: 72h     # everything from the last three days
    hourly: 24
    daily: 14
    weekly: 8
    monthly: 12
    yearly: 3
    timezone: Europe/Berlin  # where a day begins; empty uses the host zone
```

`keep` **replaces** `maxAge` and `maxPerSeries`. Configuring both is a startup error rather than a precedence to remember — a count limit left over from an older file would cut a policy asked to keep five years back to a few hundred scans, and it would do it silently.

Three things are worth knowing about how it decides:

- **Rules only keep.** They combine by union, so adding `yearly: 5` can never shrink what is stored. Each of `hourly` through `yearly` keeps one scan in each of the newest N periods that hold a scan at all; empty periods are not counted, so `monthly: 12` means twelve months that were scanned, not the last twelve months on the calendar.
- **A period keeps its most usable scan, not simply its newest.** A scan that ran to idle beats one cut short by the clock or a cap, which in turn beats one that errored or was skipped. A day whose 23:50 scan hit the hard timeout keeps the day's clean 06:00 scan instead — otherwise the record left a year later reads like a quiet day rather than like a failed observation. A period holding nothing but failures still keeps one: that wsaw tried, and what came of it, is evidence too.
- **A policy of counts alone never empties a series.** `within` and `maxAge` are explicit age limits and may: captured data can itself be personal data, so an operator who says "keep 30 days" is taken at their word.

Retention runs hourly in the daemon, and every prune is logged and counted as `wsaw_results_pruned_total`. A policy change applies to everything already stored the next time it runs, so there is a dry run:

```console
$ wsaw prune --dry-run --all
site  reject  3 kept, 41 to delete
  2026-03-20T09:12:04Z  scan-a3f  idle     keep (last)
  2026-03-19T09:11:58Z  scan-91c  timeout  delete
  …
would delete 41 result(s), keep 3; nothing was deleted
```

Without `--dry-run` the same command applies the policy once, outside the daemon. Baselines are never pruned — an approved baseline holds its own copy of the result, so history can expire without changing what "expected" means. The artifacts a pruned result alone referenced — its document, its screenshots, its stored bodies — are reclaimed from the bucket with it; see [retention](#retention-reclaims-what-it-stops-referencing).

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

Setting neither is the default, and is what the single-binary deployment wants: artifacts land in an `artifacts` directory beside the database file, or beside the state directory when the store is a server database. Nothing to configure, no external service.

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
- **A lifecycle rule on the bucket will delete evidence behind wsaw's back.** If you set one — a transition to a cold tier or an expiry after 30 days — it applies to objects wsaw's index still references, and there is no way for wsaw to object. Screenshots start coming back as evidence that is no longer stored, and a baseline can lose the scan it approved. Let wsaw's own retention (`store.maxAge`, `store.maxPerSeries`) decide what goes, and leave expiry rules off the bucket it owns. A cold-storage transition is the same trap in slower form: a restore is not a GET, and wsaw will not wait for one. It has a second, quieter effect: a *result document* removed that way leaves a result whose evidence wsaw can no longer enumerate, and while such a result exists no screenshot and no stored body is ever reclaimed again. `wsaw store prune` prints how many are in that state, and [retention](#retention-reclaims-what-it-stops-referencing) below says what to do about it.
- **The bucket, or a prefix that is wsaw's alone, must be wsaw's alone.** A retention sweep walks it. It deletes only keys of the shape wsaw writes evidence under — `body/<sha256>`, `screenshot-…/<sha256>`, `result/<sha256>`, `probe/<sha256>` — and counts anything else as "not written by wsaw" and leaves it, the bookkeeping objects under `_wsaw/` included. But two wsaw deployments sharing one bucket write the same shapes, and each one's sweep would then collect the other's evidence. Give each deployment its own bucket. Note that a path in the URL is *not* a prefix: `s3://bucket/wsaw` is refused at startup, because every provider takes the bucket from the host and silently drops the path.

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

`prune` is what the daemon does on its own every hour. `sweep` exists for what a prune cannot see — an object left behind by a scan that was interrupted between writing the bucket and recording its index entry, and a key an earlier prune's delete was refused — and the daemon runs it on its own too, once a day by default:

```yaml
store:
  sweep: true          # the default; false turns the scheduled sweep off
  sweepInterval: 24h   # the default; at least 1h
```

Leaving both out means a daily sweep, not no sweep: garbage nobody collects is a cost that grows without bound. To turn it off, write `sweep: false`; an interval next to it is refused rather than left looking effective, and so is an interval under an hour — nothing younger than a day is ever collected, so sweeping more often buys nothing and costs another listing.

**What one sweep costs.** A sweep lists every key wsaw owns: against object storage that is one LIST request per thousand keys, plus one DELETE per object it actually collects. A bucket of a hundred thousand objects is about a hundred requests a day, well under a cent a month on S3, GCS or Azure. It reads no object bodies. Against a local artifact directory it costs a directory walk.

**When it runs.** The next sweep is due one interval after the last *recorded* one — the maintenance log every prune and sweep writes to, whichever process ran it — not one interval after the daemon started, so a daemon restarted every night still sweeps. A store that has never been swept, or whose last sweep is overdue, is swept 10 to 20 minutes after startup: soon, but not in the same second as every other instance restarted by the same deploy, and not on every restart of one that is crash-looping. A sweep that fails, or is refused, is tried again one interval later, never in a loop. The daemon's prune and sweep never run at the same time, and a sweep that is still running when the next falls due is not started twice. On shutdown a sweep stops at the next key; what it had already deleted is recorded, and nothing it had not reached is touched.

**What it will not do.** The scheduled sweep is the same sweep `wsaw store sweep` runs, and it never passes `--allow-empty-index`. A daemon whose store holds no results logs a warning naming that command on every scheduled run, records the refusal, and deletes nothing — deciding that an empty history is real is for an operator, not a timer.

`wsaw store sweep` run by hand while the daemon is running is safe for the same reason a sweep is safe beside live scans: the grace period below protects anything a running scan could still be about to reference. There is no lock between the two processes, and none is needed.

Every scheduled sweep is logged with its figures, counted as `wsaw_sweep_runs_total{outcome="success"|"error"}`, and its deletions join `wsaw_artifacts_deleted_total`, `wsaw_artifact_bytes_freed_total` and `wsaw_artifact_deletions_failed_total`. `wsaw_last_successful_sweep_timestamp_seconds` and `wsaw_last_successful_prune_timestamp_seconds` hold the Unix time of the last run of each that succeeded in this process, 0 until one has:

```
# Alert when a daemon that has been up for two days has not swept its bucket
# successfully in that time. A gauge still at 0 counts as "not in two days".
(time() - wsaw_last_successful_sweep_timestamp_seconds > 172800) and (wsaw_uptime_seconds > 172800)
```

The gauge starts at 0 in a fresh process — hence the uptime term — so the history across restarts is in the maintenance log, which the web interface's storage page (`/storage`, and `/api/v1/storage`) reads back.

Both are safe to run while wsaw is scanning. An unreferenced object is left alone if it was written in the last 24 hours, and also if a scan running now has *taken* it: artifacts are content-addressed, so a scan that captures an unchanged asset writes nothing at all — the key is already there, dated by whichever scan first stored those bytes — and wsaw records the take so that neither a prune nor a sweep can collect an object the scan in progress is about to reference. Objects in the bucket that wsaw did not write are counted separately and never deleted.

A result document is never collected, even when no index entry points at it, and is counted and reported instead. A document decodes to the scan it records, so it is something [an index rebuild](#rebuilding-an-index-from-the-bucket) can recover from rather than garbage; an interrupted write, or an index that is behind the bucket, is what a run of them looks like. A sweep also stands down entirely while a rebuild is running — the rebuild leaves a marker object in the bucket, and until it is cleared nothing is collected, because half a rebuilt index makes the other half's evidence look unreferenced.

A sweep refuses to walk the bucket at all if the store's index holds nothing — no results, no baselines, no references, no takes. An index that knows nothing cannot tell garbage from a year of evidence, and that is what a database restored without its bucket, or a fresh store pointed at an existing one, looks like from inside a sweep. `--allow-empty-index` says the empty history is real and sweeps anyway, and it is the one thing that lifts the protection above: with it, orphaned result documents are collected too. A listing that is merely lagging looks exactly like an index that is genuinely absent from out here, so use the flag only when you know which of the two you have — never as a way to make a refused sweep run. There is no undo: `wsaw store rebuild-index` can put a result index back from the documents, and with `--allow-empty-index` the documents are exactly what the sweep will have deleted.

Neither command runs at all if the bucket is unreachable. A deletion against a bucket that has gone away — an unmounted volume, an expired credential — answers "already gone" for every key, which would be reported as a successful reclaim and would clear the very records that say those objects still need collecting.

A bucket that refuses a delete does not fail the prune. The key is counted as `wsaw_artifact_deletions_failed_total`, its reference is kept as the record that it still needs collecting, and the next sweep meets it again.

One case holds artifact collection back on purpose. If a stored result's document has gone missing from the bucket or no longer decodes, wsaw cannot know which screenshots and bodies that result named — so it will not declare any screenshot or body unreferenced while such a result exists, and says so in the prune's output ("unknown: N results do not say which artifacts they reference"). Result documents are still collected, because the index records where its own document went regardless. Absence of a reference is not evidence of an unreferenced artifact.

That state is sticky, and worth knowing about: it lasts as long as the affected results do, so a bucket lifecycle rule that removed one result document stops screenshot and body reclamation for the whole store until they are gone. Deleting them — tightening `store.maxAge` so they expire, or removing them from the store — is the remedy available today.

#### The SQLite file shrinks only when it is vacuumed

SQLite never gives freed space back to the filesystem on its own. The pages a deleted row leaves go on a freelist inside the file, later writes reuse them, and the file stays at the largest size it ever reached. So a prune does not make `wsaw.db` smaller, and neither does [the upgrade that moves every result document into the bucket](#upgrading-a-store-that-predates-the-bucket): after it, a store can be a file of hundreds of megabytes that is almost entirely free pages. `wsaw store vacuum --dry-run` shows how many there are and what a vacuum would give back.

A vacuum rewrites the database without its free pages and then truncates the WAL. The daemon does it on its own, once a week:

```yaml
store:
  vacuum: true             # the default; false turns the scheduled vacuum off
  vacuumInterval: 168h     # the default; at least 1h
  vacuumMinFreeRatio: 0.2  # the default; the share of the file that must be free, in (0, 1]
```

**When it runs.** Like the sweep, the next vacuum is due one interval after the last one recorded in the maintenance log, and a store that has never been vacuumed — every store that went through the upgrade above — is vacuumed 10 to 20 minutes after the daemon starts. A vacuum runs after that hour's prune and sweep, never at the same time as either. If less than `vacuumMinFreeRatio` of the file is free, it is skipped and recorded as skipped: most weeks, that is what a healthy schedule does. All three settings are applied by SIGHUP.

**What it costs.** A vacuum holds the database's write lock while it rewrites the file: a fraction of a second for tens of megabytes, longer for gigabytes. Scans that finish during it wait for it and are then stored as usual. It also needs free disk: the rewrite builds a copy of the live data in SQLite's temporary directory (`SQLITE_TMPDIR`, then `TMPDIR`, then `/var/tmp`) and writes it once more into the WAL beside the database, so it needs about the live data free in each place, or twice that if they are on one filesystem. It checks before it starts and refuses — logged, recorded and counted as `refused` — rather than run out of disk part way. An interrupted vacuum leaves the file exactly as it was.

`wsaw store vacuum` runs one now, and `--force` rewrites the file whatever its free share. It is safe beside a running daemon, but it has to take the same write lock: if the daemon is writing for longer than the busy timeout, the command gives up and says so. Stop the daemon first, or leave it to the schedule.

Only SQLite is vacuumed. PostgreSQL and MySQL reclaim space themselves; `wsaw store vacuum` refuses them by name, and the daemon logs once at startup that no vacuum applies.

Every vacuum is logged with its figures and counted as `wsaw_vacuum_runs_total{outcome="vacuumed"|"skipped"|"refused"|"error"}`; `wsaw_vacuum_bytes_reclaimed_total` adds up what the file and its WAL shrank by, and `wsaw_last_successful_vacuum_timestamp_seconds` is the Unix time of the last vacuum that rewrote the file or found no need to, 0 until one has in this process.

#### Rebuilding an index from the bucket

The bucket holds the record. The index — a pointer per scan plus the handful of counts a listing shows — is derived from it, and `wsaw store rebuild-index` re-derives it: every object under the `result/` prefix is read, decoded, and turned into the same index entry and the same summary the scan that stored it wrote, by the same code.

```
wsaw store rebuild-index --dry-run    # what it would add, and what it would cost
wsaw store rebuild-index              # do it
wsaw store rebuild-index --verify     # change nothing, report every disagreement, exit non-zero on any
```

It works for all three stores. Which one you are running is the only thing that differs.

**When to run it.** A database dropped, restored from a backup older than its bucket, or restored without one. A result you can see in the bucket and not in the interface — the state an interrupted scan leaves, where the document landed and the index entry never did. A summary column that a wsaw upgrade computes differently from the one that wrote it.

**What it restores.** Every scan the bucket still holds a document for and the index has no record of having removed: the listing, the ordering, the summaries, the artifact references, and the reverse index retention decides deletions from. Documents are content-addressed and are read back byte-identically, so a rebuilt result is the result — not a reconstruction of one.

**What it does not put back is history retention deleted.** Pruning removes a result and keeps the artifacts something else still names, so a scan a baseline was approved from leaves its document in the bucket for ever after the result itself has expired. A rebuild that treated "there is a document" as "there should be an entry" would undo a deletion made to satisfy a retention policy — quietly, and reported as work done. It does not: the index records the removal in the artifact references a pruned result leaves behind, and those documents are counted and named in a category of their own, `pruned:`, rather than folded into what the run would add. `--verify` does not call them drift either, so a correctly pruned store stays green. The documents are still in the bucket; nothing was lost, and nothing came back. Where the index itself is genuinely gone — a dropped database, a deleted `_wsaw/` prefix — nothing records what was pruned and a rebuild recovers the whole bucket, which is the recovery the command is for.

**What it cannot restore — read this before you rely on it.** Baselines and their approvals, the audit log, and change-event history are decisions and observations *about* scans, not properties of them, and no stored document contains one. A rebuild preserves them where they still exist and reports exactly what it found where they do not, prominently and at the top of its output, rather than handing you a store that looks intact and quietly reports every target as never having been approved. If your index is gone, they are gone with it: restore them from a backup of the index, or approve them again. Share links need no restoring; they are signed with `api.share.key` and are not stored anywhere.

**It adds and never removes.** There is no swap and no "clear, then rebuild": the run merges into whatever index is there, so an abort at any point leaves a store you can read, and running it again finishes the job without repeating a single write. An entry it cannot find a document for is reported and **left where it is** — a bucket that lost an object and a listing that has not caught up look identical from out here, and only one of them is a reason to delete a record of a scan. Deciding that is a person's job, and `--verify` is how they see it.

**A missing screenshot is recorded, not tidied away.** A rebuilt result whose screenshots or stored bodies are gone from the bucket keeps its references and reads as evidence that is no longer stored. Dropping the reference would turn a scan whose evidence was deleted into a scan that captured none, which is the one inference this tool must never make. The run says how many it found.

**A damaged object is named and skipped.** An object under `result/` that does not hash to the key it is stored under, or that will not decode, is reported by key, counted, and passed over. One unreadable object out of a hundred thousand does not stop the other ninety-nine thousand from being indexed, and it does not go missing from the summary either. A rebuild carries on and exits zero; `--verify` exits non-zero, because a truncated or tampered document is evidence this store can no longer produce and a scheduled check must not be green over it.

**A document a newer wsaw wrote is refused rather than re-derived.** A result document carries the schema version of the build that wrote it. An older binary decoding one drops every field it does not know, and the summary it would derive is short by exactly those fields — so a rebuild by the old binary would rewrite the history downwards and print it as a repair. It refuses instead: the documents are counted, named with their schema version, left entirely alone, and `--verify` exits non-zero over them. Upgrade wsaw and run it again. This is the same rule a store applies to a newer database schema and the `blob` store to a newer index layout.

**It also tells you what is in the bucket.** A rebuild is the one operation that sees every key, so it reports what wsaw wrote, what no result references, and what wsaw did not write, in objects and bytes — the cheapest place to learn whether `wsaw store sweep` has anything to do.

**`--verify` is the scheduled one.** It writes nothing and compares the two in both directions: index entries naming documents that are gone, documents with no index entry, and summaries that no longer match their document. It exits non-zero on any of them, and on any object it could not read or could not understand, so it belongs in a cron entry or a CI job rather than in the recollection of whoever handled the last incident.

Two things it deliberately does *not* exit non-zero over, because a cron job that is permanently red is a cron job nobody reads. Evidence a lifecycle rule expired — a screenshot or a stored body the result still names and the bucket no longer holds — is the recorded outcome the rest of this section describes, and is reported by count and by key without changing the exit code. And a scan retention removed is not a disagreement: see "what it does not put back" above.

`--verify` and `--dry-run` also do not migrate the store's schema on the way in. If the schema is behind this build they refuse and name `wsaw store migrate`, rather than performing the one-way upgrade in the middle of a command that promised to change nothing.

**Running it while wsaw is running is safe, and it does not lock anything out.** A rebuild only ever creates records a scan would have created itself, at keys derived from the scan, so a result stored while it runs is either seen by the walk and written identically or not seen and already written by the scan — never lost, and a later run picks up whatever the first did not see. What it does take out is `wsaw store sweep`: the rebuild leaves a marker object in the bucket, and while that marker is there every store's sweep collects nothing and says so, because half a rebuilt index makes the other half's evidence look like garbage. The marker is cleared when the run ends, including when it is interrupted. A process that is killed outright cannot clear it, so a marker is also ignored once it is more than a day old — a rebuild that started yesterday is not running — and the sweep logs loudly when it steps over one. Until then the sweep names the key, and deleting that object clears it immediately.

**What it costs.** A rebuild reads every stored document, so its cost is proportional to the whole history rather than to what is wrong with the index:

- one GET per stored document, and the bytes of the entire history transferred;
- one small index read per scan already recorded;
- one existence check per screenshot and stored body the results name (nothing extra when body and screenshot storage are off, which is the default);
- for each scan it adds, one row, plus the artifact references it names;
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

**The database file does not shrink on its own.** Moving the documents out leaves their pages free inside a SQLite file, which stays as large as it was. The daemon's first scheduled vacuum, 10 to 20 minutes after it starts, gives that space back; `wsaw store vacuum` does it now. See [the SQLite file shrinks only when it is vacuumed](#the-sqlite-file-shrinks-only-when-it-is-vacuumed).

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
make cover        # coverage profile, one total
make cover-report # the same profile as a browsable HTML page
```

The soak reports what the artifact bucket cost the run — requests and bytes,
in total and per scan — beside its memory and goroutine figures.

### Testing the store

The store's test suite is the specification of what a store does, so it is run
against every combination that ships rather than against the default one. Two
environment variables select the combination, and they are independent: one
says where the **index** is kept, the other where the **evidence** goes.

```sh
make test-store-memory    # evidence in a bucket that has no files
make test-store-postgres  # index in PostgreSQL, in a container
make test-store-mysql     # index in MySQL, in a container
make test-store-minio     # evidence in MinIO — a real S3-compatible service
make test-store-all       # the fast suite and then all of the above
```

```sh
WSAW_TEST_STORE_DRIVER=sqlite|postgres|mysql        # where the index is
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

`make cover-report` writes `coverage-report.html`: every package ranked by statement coverage and
again by how many statements are untested, then a card per package with its files and the functions
no test ever reaches. The two rankings disagree on purpose — one answers "how well tested is this
package", the other "where should the next test go".

The fast suite runs **without Chrome installed** — browser tests skip themselves and say why. Integration tests use local fixture servers, including a synthetic consent banner and a synthetic third-party host; they never touch a live third-party website, so CI does not depend on someone else's site staying unchanged.

Everything above proves wsaw in one process. What a deployment breaks — a browser that cannot reach the host's loopback from inside a container, a database connection that drops, a consent banner served over a real origin rather than a fixture string — only shows up between processes, so it has its own containerised end-to-end stack: [`test/e2e/README.md`](test/e2e/README.md).

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
