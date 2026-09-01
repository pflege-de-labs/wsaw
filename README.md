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

- Go 1.24+ to build.
- Chrome or Chromium on the host (or use the container image, which bundles Chromium). wsaw checks the version at startup and refuses to run against a browser too old to mean what the schema says.
- Linux or macOS, amd64 or arm64. No Windows.

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

The result schema is published at [`docs/result.schema.json`](docs/result.schema.json) and the HTTP API at [`docs/openapi.yaml`](docs/openapi.yaml).

## Web interface and API

Both are served by the same process and the same port; the web interface is a client of the public API and has no privileged path into the store. Assets are embedded in the binary, so there is nothing to deploy alongside it and no Node toolchain to build it.

It binds to loopback by default. A non-loopback listener **requires** a token — configuration validation refuses to start without one, because scan results can contain personal data.

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

State lives in the platform's directory by default (`$XDG_STATE_HOME/wsaw` on Linux, `~/Library/Application Support/wsaw` on macOS) and is created `0700`.

## Being a good citizen

wsaw scans other people's infrastructure, so politeness is enforced in code rather than left to configuration discipline: per-origin concurrency separate from worker concurrency, a minimum interval applied as a hard floor after the schedule, deterministic startup jitter, and catch-up off by default so a restart is not a scan storm.

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
```

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
