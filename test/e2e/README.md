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
| `e2e-fixture-browse` | open the fixture in a visible Chrome, to see the page and the banner yourself |
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

### Looking at it yourself

```
make e2e-fixture-browse
```

This opens the fixture in a real Chrome window — the quickest way to answer
"what does the page actually look like?" when a consent assertion fails. Close
the window, or press Ctrl-C, to finish.

It uses Chrome from wsaw's own discovery, so it finds the same browser a scan
would, and a throwaway profile under `dist/e2e/`. The profile is not a detail:
with a shared one, an already-running Chrome takes the URL and **silently
ignores** the resolver rules, so the fixture's hostnames would not resolve and
nothing on screen would explain why. The profile is fresh each run so the
banner appears each time — pass `KEEP_PROFILE=1` to keep a decision made in
the previous one, and `CHROME_PATH=...` to use a particular browser.

Chrome is launched in its own process group and killed as a group, because
Chrome is a tree — killing only the parent would leave renderers behind, which
is the leak wsaw refuses to tolerate for its own browsers.

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

## Compose stack

The fixture above is one origin among four. `test/e2e/compose.yaml` brings up
the whole system (Story 7.2): wsaw, the fixture's two origins, and a server
database, addressing each other by name on one Compose network — which is
what makes it work at all, since a browser inside a container cannot resolve
the host's `localhost` (Story 1.8, AC10). The browser runs **inside** the
wsaw container rather than as a sibling: mounting the runtime socket so wsaw
could start one alongside itself would give a container that renders hostile
pages the ability to start privileged containers, which is a worse trade
than losing sibling-container isolation.

The database is not defined in `compose.yaml` — it is picked with an
override file, so the same base file serves either database rather than two
copies of it drifting apart:

```sh
make e2e-compose-up   DB=postgres   # or DB=mysql
make e2e-compose-scan DB=postgres   # scans all three consent modes
make e2e-compose-logs DB=postgres   # follow every service
make e2e-compose-down DB=postgres   # stop the stack and remove its volumes
```

which is the same as running Compose directly:

```sh
docker compose -f test/e2e/compose.yaml -f test/e2e/compose.postgres.yaml up -d --wait
```

(`podman compose` works the same way — everything here has been run against
both.)

`e2e-compose-up` passes `--wait`, so it blocks until every service —
including wsaw itself — reports healthy rather than merely started, and
fails loudly if one does not. A race that usually passes is worse than one
that always fails.

`/dev/shm`'s container default, 64 MiB, is not enough for Chrome on a real
page; `compose.yaml` sizes it explicitly rather than leaving it as a
troubleshooting note. Every credential anywhere in the stack — the database
password, the API token wsaw needs because its listener is reachable beyond
loopback inside the Compose network — is an obvious fixture
(`wsaw-e2e-fixture-only...`), set nowhere else and named so nobody mistakes
it for a real one.

`e2e-compose-down` always passes `-v`: a stale database left over from an
earlier run must never be why a later one passes or fails for reasons that
are not in the code.

Three ports are published to the host, for a test driving the stack from
outside it (Stories 7.4 and 7.5): wsaw's API on `18712` — not its documented
default, `8712`, which a wsaw already running on this machine for real could
be using, and a request that lands on the wrong process fails with a
confusing 401 rather than a connection error — the fixture site on `18081`,
so a test can switch its variant with a real PUT (the fixture ships no curl,
and busybox `wget` cannot send one), and the database on its normal port, so
results can be read back with SQL from the host directly.

`e2e-compose-scan` triggers a scan through wsaw's own HTTP API
(`allowAdHocScan`), because inside the stack that API is the only thing that
can reach it — there is no shared file store to run `wsaw scan` against from
outside. Read the results back with SQL against the database directly
(`docker compose ... exec db psql ...` / `... exec db mysql ...`), not
through wsaw's own API — asking the writer whether it wrote correctly proves
less. That is what Stories 7.4 and 7.5 automate.

## Podman pod

The rootless runtime is preferred throughout wsaw (Story 1.8 already picks
Podman where a runtime is being detected), so the deployment story is not
Docker-only either: `make e2e-pod-up DB=postgres|mysql` brings up the same
four services as a genuine Podman pod (Story 7.3) — not a translation of the
Compose file, a pod.

```sh
make e2e-pod-up   DB=postgres   # or DB=mysql
make e2e-pod-scan DB=postgres   # scans all three consent modes
make e2e-pod-logs                # follow every container
make e2e-pod-down                # stop the pod and remove it
```

The difference that matters: a pod's containers share one network
namespace, so they reach each other on `localhost` rather than by service
name, and — unlike Compose, where every container has its own address —
every one of them needs its **own port** there or two services collide. That
is a real failure mode inside a pod, and it is the reason
`test/e2e/pod/wsaw.postgres.yaml` is its own file rather than
`compose/wsaw.postgres.yaml` with the hostnames swapped: site, tracker, the
database and wsaw's own API each get an unobvious port
(`18081`/`18082`/`5432` or `3306`/`18712`), chosen the same way the fixture's
own `8081`/`8082` are — so a pod left running cannot collide with the
Compose stack, with `e2e-fixture-up`, or with a wsaw already running on the
machine for real (wsaw's documented default port, `8712`, is exactly what a
real deployment would be using).

It runs rootless — the same as every other `podman` target in this file.
Nothing about being in a pod needs anything extra granted to Chrome's
sandbox beyond what a standalone container already needs; where a host's
rootless configuration does need more (user namespaces, a seccomp
adjustment), that requirement belongs to running Chrome in *any*
unprivileged container, and is documented once, in the root `Dockerfile`,
rather than repeated here.

`e2e-pod-up` builds the wsaw image itself (there is no Compose to do it),
creates the pod, starts the fixture's two origins and the database, waits
for the database to answer over **TCP** — not the Unix socket a bare
`mysqladmin ping -h localhost` would silently prefer, which answers before
the real server is listening on the port wsaw actually connects to — then
starts wsaw and waits for its own health endpoint, the same way
`e2e-compose-up --wait` does.

There is no Kubernetes manifest in this repository yet to compare this pod
against (Story 6.10 is not implemented): once one exists, this is the
container list, images and ports it should be checked against, and the
documentation should say where they differ and why (Story 7.3, AC5).

## The end-to-end test

Everything above stands the stack up; `test/e2e/stack_test.go` (Story 7.4) is
what actually proves it. It is a normal Go test, behind the `e2e` build tag
so `go test ./...` never runs it, but it does not start the stack itself —
it needs `test/e2e/compose.yaml` already up with a database picked, which is
what the wrapper below is for:

```sh
make e2e-postgres-test
```

This brings the Compose stack up with Postgres, runs the suite, and always
tears the stack down afterwards — printing every service's log first if the
suite failed, because a red end-to-end test that says only "assertion
failed" costs more time than it saves (AC8). Running `go test -tags e2e`
directly, without the make target, skips every test with an explanation
rather than failing to dial a stack nobody started.

What it actually checks, against the running stack rather than a hand-built
result:

- The schema exists because wsaw's own migrations created it, not a
  migration tool run separately (AC5).
- In `none` the banner is present and untouched; in `reject` the blocked
  third party never appears, in either phase — Klaro's own state, not just
  wsaw's report of it; in `accept` it appears, post-interaction (AC2).
- The fixture's one unconditional third party is reported as contacted
  before any consent decision, in every mode (AC3).
- Two scans of the unchanged fixture diff as nothing; switching the variant
  and scanning again produces exactly the two changes the switch makes, at
  the severities the real diff engine assigns them — not a hand-built
  expectation (AC4).
- A baseline approval and its audit entry, read back with SQL, describe the
  same event (AC7).
- The database is stopped, a scan is triggered while it is down, and started
  again: the attempt is not silently retried into existence once the
  database returns, wsaw's own log says persisting it failed, and the next
  scan after recovery is stored normally (AC6).
- Restarting wsaw against the same, already-populated database changes
  nothing already stored and does not duplicate the schema version row
  (AC5).

It drives the stack the same way the Makefile does — through
`docker compose` / `podman compose`, never by guessing a container's name,
which differs between the two (Story 7.3 found exactly that difference
once already) — using the same invocation `make e2e-postgres-test` was
called with, passed through as `WSAW_E2E_COMPOSE_CMD`.

### The same test against MySQL

```sh
make e2e-mysql-test
```

`TestMySQLStack` in the same file runs every assertion above against the
MySQL profile — one test body parameterized by dialect, not a second file
that would drift (Story 7.5, AC1) — plus what MySQL specifically forces that
Postgres does not:

- **Charset and collation.** MySQL's default collation is case-insensitive
  and accent-insensitive, which would silently merge two differently-cased
  target names into one series; the schema wsaw's own migrations created on
  the live server is confirmed pinned to `utf8mb4`/`utf8mb4_bin` (AC3).
- **Strict `sql_mode`.** Without `STRICT_ALL_TABLES`, MySQL truncates a value
  that overflows its column and reports success — which would shorten a
  captured URL and change the finding rather than fail loudly. This is
  confirmed against the live server with a value that overflows a real
  column, in a transaction that is always rolled back so it can never affect
  what another subtest counts (AC4).
- **Timestamp precision.** MySQL's default `DATETIME` drops fractional
  seconds, and "the scan before this one" is decided by that ordering. wsaw
  sidesteps `DATETIME` entirely — `started_at` is a bigint count of
  nanoseconds, the same as every other dialect — and this confirms that
  choice actually preserves sub-second precision on a live server (AC5).
- **The upsert form.** MySQL's `insert ... as new on duplicate key update`
  is not the portable form the other two dialects use. Approving a second
  baseline for the same target and mode must overwrite the first rather
  than error or duplicate the row, proven through a real baseline approval
  rather than a hand-built query (AC2).

Two things AC2 also names are deliberately not re-tested here, rather than
worked around silently (AC6):

- **The JSON accessor.** There is not one. Story 4.6 chose to derive
  summaries by parsing the stored document in Go rather than in SQL, so
  nothing in the store depends on `json_extract`, `->>`, or
  `JSON_EXTRACT()` — the dialect seam is smaller because of it, and there is
  nothing dialect-specific left to exercise.
- **The retention delete's absence of `rowid`.** Pruning has no HTTP
  endpoint to trigger it on demand from outside the process, so this suite
  cannot reach it without reopening the store package directly — which is
  exactly what `internal/store`'s own dialect tests already do, against a
  real MySQL container, via `make test-store-mysql`. Re-deriving that here
  would test the same SQL a second way rather than a new one.

## In CI

Both `make e2e-postgres-test` and `make e2e-mysql-test` run in
`.github/workflows/ci.yaml`'s `e2e` job (Story 7.6) — on the same schedule
as the soak test, on a release tag, and on demand, never on every push
(AC2): the whole stack takes minutes to build and run, which no pull request
should wait on. A failure there is a release blocker in the sense that
matters here — this repository has no separate automated release pipeline
to gate directly, so it is a required status check, the same as everything
else here that blocks a merge, and the job runs again, deliberately, on the
release tag itself (AC3).

The database image each profile pulls is pinned by digest in
`test/e2e/compose.postgres.yaml` / `compose.mysql.yaml`, not just a tag that
can move upstream between runs (AC4) — a floating tag would make this suite
fail because of a change nobody here made, which defeats the point of an
end-to-end gate. The pinned image is cached between CI runs keyed on that
same digest, so a passing pin never re-pulls; only a deliberately bumped one
does.

On failure, the job uploads every service's log, the stored result
documents and the audit trail — read back the database the same way the
test itself does — and every diff the run actually produced, which is
never stored anywhere to read back on its own (AC5). The job has a time
budget; exceeding it fails the run rather than hanging (AC6).

What is not wired into CI: the Podman pod (Story 7.3) has no Go test suite
of its own, only the manual verification recorded in that story — Story
7.3's AC4 ("the same assertions run against the pod as against Compose")
is proven by hand, not automated. Extending `runStack` with a third,
pod-shaped way to reach the stack, rather than a parallel copy of the
Compose-only assertions, is the next piece of this epic to pick up, not
something worked around silently here.

## Licence

Klaro is BSD-3-Clause, © KIProtect GmbH. It is fetched at build time rather
than vendored, and its LICENSE is copied into the image next to it.
