# End-to-end fixture

The website wsaw's end-to-end tests scan (Story 7.1). It is not a mock: the
page runs a real [Klaro](https://github.com/kiprotect/klaro) consent banner,
pinned by version and digest, so a scan exercises the Klaro rule wsaw actually
ships. A banner written to match that rule would only prove the rule matches
itself.

## What it serves

One binary, two roles, because the stack needs both and they differ only in
what they serve.

**`-role=site`** — the first party:

| Path | |
|---|---|
| `/` | the scanned page |
| `/assets/klaro.js` | Klaro, pinned (see `fixture/klaro.pinned`) |
| `/assets/klaro-config.js` | one consent-gated service, `analytics` |
| `/assets/app.js` | a first-party script whose body differs per variant |
| `/assets/site.css` | a first-party stylesheet |
| `/favicon.ico` | served, so the baseline holds no failed request |

**`-role=third-party`** — the second origin, reached by its own hostname so
that first-party and third-party attribution is exercised rather than assumed:

| Path | |
|---|---|
| `/pixel.gif` | loaded **unconditionally** by the page |
| `/analytics.js` | loaded **only after consent**, released by Klaro |
| `/collect` | what `analytics.js` beacons to, at a fixed URL |
| `/extra.gif` | reached only in the changed variant |

The unconditional pixel is the point of the whole fixture: it is what
"contacted before any consent decision" looks like, and it appears in every
consent mode.

## Variants

The fixture has two states, switched over HTTP so a test can produce a known
change between scans:

```
curl -X PUT --data changed http://site:8080/__fixture/variant
curl -X PUT --data base    http://site:8080/__fixture/variant
curl            http://site:8080/__fixture/variant
```

`changed` adds one third-party host and changes one first-party script's body
— so a scan reports a new host, a new asset and a changed script digest, and
nothing else. The switch is refused with `412` if the extra origin was not
configured, because a test that believes it added a host and did not would
read a passing scan as proof of something it never checked.

## Determinism

Everything served here is static: no clock, no counters, no cache busters, no
generated identifiers. Two scans of an unchanged fixture must produce an empty
diff — that is what makes a non-empty diff mean something (Tenet 6), and it is
verified in `fixture_test.go`.

## Running it

```
make e2e-fixture-up        # build the image, start both origins, wait for health
make e2e-fixture-scan      # scan all three consent modes
make e2e-fixture-down      # remove the containers
```

| Target | |
|---|---|
| `e2e-fixture-image` | build the image; fails on a Klaro digest mismatch |
| `e2e-fixture-up` | start both roles on loopback and wait until each answers `/__fixture/healthz` |
| `e2e-fixture-scan` | scan the running fixture with `--fail-on info`, so a clean run exits 0 |
| `e2e-fixture-variant VARIANT=changed` | switch state, then scan again to see the change |
| `e2e-fixture-logs` | follow both origins' request logs |
| `e2e-fixture-down` | remove the containers |
| `e2e-fixture-config` | write only the configuration, to `dist/e2e/wsaw.yaml` |

`up` waits for health rather than sleeping, and prints both origins' logs if
either never becomes healthy — a race that usually passes is worse than one
that always fails. It is safe to run twice; it removes what is already there
first. Ports and hostnames are overridable: `make e2e-fixture-up
E2E_SITE_PORT=9091`.

Klaro is fetched during the image build, by the version and SHA-256 in
`fixture/klaro.pinned`, and the build fails on a digest mismatch. Nothing is
fetched at scan time.

### Why the generated config looks like that

`e2e-fixture-config` writes `dist/e2e/wsaw.yaml` with two things a hand-run
scan cannot do without:

- **`--host-resolver-rules`**, because the fixture is published on this
  machine's loopback and the browser has to be told its hostnames live there.
  The hostnames matter: scanning `127.0.0.1:8081` and `127.0.0.1:8082` would
  make both origins the same host, and the first/third-party classifier would
  have nothing to distinguish.
- **`runtime: local`**, because a containerised browser has its own network
  namespace and cannot reach this host's loopback at all. wsaw refuses such a
  target with an explanation rather than scanning its own container
  (Story 1.8, AC10).

Neither applies in the Compose stack or the Podman pod (Stories 7.2 and 7.3):
there the services resolve each other by name and the browser runs where it
can reach them.

### Or by hand

Each role needs to know the URLs the *browser* will use, not its own listen
address:

```
podman run --rm -p 127.0.0.1:8082:8080 localhost/wsaw-fixture:dev \
  -role=third-party -self-base=http://tracker.example:8082

podman run --rm -p 127.0.0.1:8081:8080 localhost/wsaw-fixture:dev \
  -role=site \
  -third-party-base=http://tracker.example:8082 \
  -extra-third-party-base=http://extra-tracker.example:8082
```

## Licence

Klaro is BSD-3-Clause, © KIProtect GmbH. It is fetched at build time rather
than vendored, and its LICENSE is copied into the image next to it.
