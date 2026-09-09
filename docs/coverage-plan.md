# Plan: reach 80% statement coverage

**Status:** proposal. Numbers measured 2026-09-09 on `main` (`f792284`), macOS arm64, Chrome and Podman both present.

NFR §5 asks for ≥ 80% coverage on core logic. The number the build currently prints is `59.1%`. That number is wrong, and most of the work in this plan is not "write more tests" — it is "stop mismeasuring, then fill two genuinely untested packages".

---

## 1. The headline: 59.1% is a measurement artifact

`make cover` runs:

```make
go test -coverprofile=coverage.out ./...
```

Without `-coverpkg`, Go credits each test binary **only for statements in its own package**. wsaw's browser stack is deliberately tested from the outside: `internal/scanner/integration_test.go` drives real Chrome against local fixture servers, and that run exercises `capture`, `browser` and `consent` heavily. None of it was being counted.

Re-measured with cross-package attribution:

```sh
go test -coverpkg=./... -coverprofile=coverage.out ./...
```

| Package | As reported today | Actually covered | Delta |
|---|---|---|---|
| `internal/capture` | 26.9% | **85.4%** | +58.5 |
| `internal/browser` | 6.2% | **68.1%** | +61.9 |
| `internal/consent` | 22.5% | **65.6%** | +43.1 |
| `internal/report` | 87.9% | 89.5% | +1.6 |
| `internal/store` | 75.6% | 77.0% | +1.4 |
| **total** | **59.1%** | **70.3%** | **+11.2** |

So the real starting point is **70.3%** (7663 / 10904 statements), and the gap to 80% is not 21 points — it is under 10.

### Denominator: drop the test scaffolding

`test/e2e/fixture` (302 stmts) and `test/e2e/browse` (124 stmts) are test *harnesses* — a fixture web server and a manual browse tool. Measuring them as product code is noise: `browse/main.go` is a debugging aid and will never have a test, and it drags the total down by ~1 point for no engineering signal.

Excluding them (`tools/coverreport` is already out via `//go:build ignore`):

> **Baseline: 7458 / 10478 = 71.2%. Target 80% = 8382. Need +924 covered statements.**

Every estimate below is against that denominator.

---

## 2. Where the remaining gap actually is

Deduplicated, cross-package attributed, product packages only:

| Package | Covered | Total | % | Uncovered |
|---|---|---|---|---|
| `cmd/wsaw` | 0 | 1111 | **0.0%** | **1111** |
| `internal/app` | 0 | 337 | **0.0%** | **337** |
| `internal/httpapi` | 1157 | 1446 | 80.0% | 289 |
| `internal/store` | 662 | 860 | 77.0% | 198 |
| `internal/consent` | 332 | 506 | 65.6% | 174 |
| `internal/capture` | 975 | 1142 | 85.4% | 167 |
| `internal/browser` | 339 | 498 | 68.1% | 159 |
| `internal/config` | 587 | 701 | 83.7% | 114 |
| `internal/scanner` | 261 | 371 | 70.4% | 110 |
| `internal/notify` | 449 | 515 | 87.2% | 66 |
| `internal/diff` | 369 | 420 | 87.9% | 51 |
| `internal/container` | 290 | 341 | 85.0% | 51 |
| `internal/report` | 385 | 430 | 89.5% | 45 |
| `internal/logging` | 191 | 229 | 83.4% | 38 |
| `internal/daemon` | 408 | 436 | 93.6% | 28 |
| `internal/metrics` | 334 | 360 | 92.8% | 26 |
| everything else | 903 | 975 | 92.6% | 72 |

**1448 of the 3020 uncovered statements — 48% — sit in the two packages with no test file at all.** `cmd/wsaw/*.go` and `internal/app/app.go` are the whole plan. Nothing else needs to move to clear 80%.

That is also where the risk is concentrated: `app.New` decides where the store lives, whether the browser runs in a container, and whether the Chrome sandbox is on; `cmd/wsaw/scan.go` owns the CI exit-code contract (`0` clean / `1` findings / `2` operational). Both are currently asserted by nothing.

---

## 3. Phases

### Phase 0 — fix the measurement (no new tests)

1. `Makefile`: give `cover` and `cover-report` cross-package attribution and a product-code package list.

   ```make
   # Packages whose statements count as product code. The e2e fixture server
   # and the manual browse tool are test scaffolding: measuring them says
   # nothing about wsaw and drags the number around when a fixture grows.
   COVER_PKGS = $(shell go list ./... | grep -v '/test/e2e/' | paste -sd, -)

   cover:
   	go test -coverpkg=$(COVER_PKGS) -coverprofile=coverage.out ./...
   	go tool cover -func=coverage.out | tail -1
   ```

   With `-coverpkg` spanning many packages, each test binary emits the full block set, so a merged profile contains one block many times over. `go tool cover -func` handles that; **any homegrown arithmetic over the profile must dedupe by block key first** or it reports garbage (this is how the per-package table above was built). `tools/coverreport/main.go` needs checking on that point.

2. Establish the **CI** baseline, not the laptop one. This machine has Chrome and Podman; Linux CI installs Chromium and never pulls the pinned browser image, so `internal/container` and the container path in `app.resolveBrowser` skip there. The gate threshold must be whatever CI measures, or the gate is a coin flip.

3. Turn the coverage step into a real gate with a ratchet. There is no `cover-gate` target today — the CI step prints the number and passes regardless:

   ```yaml
   - name: Coverage gate
     run: make cover-gate            # fails below $(COVER_MIN)
   ```

   Set `COVER_MIN` to the measured CI baseline immediately (so it cannot regress), and raise it at the end of each phase below. A threshold set to the aspiration rather than the reality is a broken build, not a gate.

**Effect: reported coverage 59.1% → ~71% with zero behaviour change.** Do this first; every later estimate is unverifiable until it lands.

---

### Phase 1 — `cmd/wsaw`, the parts that need no browser  (+~520 stmts, ≈ +5.0 pt)

New: `cmd/wsaw/config_test.go`, `cmd/wsaw/main_test.go`, `cmd/wsaw/emit_test.go`, `cmd/wsaw/run_build_test.go`, `cmd/wsaw/share_test.go`.

`run(ctx, args) int` is already the testable seam — `main` is a five-line wrapper around it. Nothing here needs Chrome.

| Target | Stmts | What to assert |
|---|---|---|
| `config.go` — `register`, `load`, `loadFromDefaultPaths`, `addAdHocTargets`, `adHocName`, `applyOverrides`, `cmdConfig` | 154 | flag/file/override precedence; `--url` ad-hoc target naming and collisions; `--check` on a good and a broken config; unreadable and absent config paths |
| `main.go` — `run`, `usage` | 55 | every command word dispatches; unknown command and no args → exit 2 with usage on stderr; `version` output; `--help` → exit 0 |
| `scan.go` — `retryReason`, `sortOutcomes`, `lessOutcome`, `bodyLoader`, `emit`, `emitStdout`, `writeAtomic`, `sanitize` | ~130 | table-driven ordering; each `--format` (json/jsonl/csv/markdown) writes what `internal/report` produces; `writeAtomic` leaves no partial file when the write fails; `sanitize` on hostile target names — a page-controlled name must not choose a path (Rule 2) |
| `run.go` — `pluralSettings`, `resolveNotifier`, `buildDispatcher`, `buildShareSigner`, `buildServer`, `reload` | ~110 | a notifier per kind resolves; unknown kind and missing token error out; signer requires its secret; reload accepts a targets-only change and refuses one it cannot apply (regression cover for `2aa987f`) |
| `share.go` — `sharePathFor`, `trimTrailingSlash`, `loadShareTarget` | ~55 | link shape; unknown target / unknown mode / no stored scan each error distinctly |
| `debug.go` — `cmdRules`, `cmdRulesList`, `orDash` | ~50 | rule listing against the bundled rule set; bad `--rule` name |

Assert **exit codes and stream contents**, not internals: these tests are the executable form of the CI contract in the README.

---

### Phase 2 — `internal/app`  (+~260 of 337 stmts, ≈ +2.5 pt)

New: `internal/app/app_test.go`, `internal/app/browser_test.go`.

| Target | Stmts | What to assert |
|---|---|---|
| `New`, `openStore`, `openServerStore`, `adoptStore`, `logStoreRetry` | ~110 | sqlite under `t.TempDir()` opens and migrates; a bad DSN fails with a usable message; the retry path retries and gives up (inject the clock — no `time.Sleep`, per §5) |
| `resolveBrowser`, `resolveLocalBrowser`, `BrowserRuntimeName`, `BrowserSandboxed` | ~80 | `--browser-runtime local` never reports `browserSandbox: false` by accident; `auto` with no runtime falls back to local and *says so*; the container path skips with a clear reason when no runtime is present (Rule 1: no test may pass by disabling the sandbox) |
| `loadRules`, `buildScanner`, `Normalizer`, `TargetByName`, `Retention` | ~40 | wiring produces a scanner; a missing rules file errors; an unknown target name returns `false` |
| `LastScan`, `RunningScans`, `Close`, `closeAfterFailedStart`, `PruneLoop`, `defaultStateDir`, `DefaultConfigPaths`, `orAuto`, `orInfo` | ~30 | `Close` is idempotent and releases the store on the failed-start path (Rule 5); `PruneLoop` returns on context cancel |

`New` and `Trigger` need a browser for their last few statements — leave those to Phase 3 rather than mocking Chrome.

**After Phases 0–2: ~78.6%.** Short of target, but both zero-coverage packages are gone.

---

### Phase 3 — the CLI end to end, against fixtures  (+~350 stmts, ≈ +3.3 pt)

New: `cmd/wsaw/cli_integration_test.go`, guarded exactly like `internal/scanner/integration_test.go` (skip on `WSAW_SKIP_BROWSER_TESTS`, skip with a stated reason when no usable Chrome). Fixture servers only — never a live site (Rule 7).

| Target | Stmts | What to assert |
|---|---|---|
| `cmdScan`, `runOnce`, `scanWithRetries` | 208 | the exit-code contract: clean fixture → 0; fixture that trips `--fail-on high` → 1; unusable browser or bad config → 2. **Operational failure outranks findings** — assert a failed scan never exits 0 |
| `cmdDebug` | 101 | one scan, markdown to stdout, no store required |
| `cmdShare` | 84 | a stored scan produces a link that `internal/share` verifies |
| `cmdRulesTest` | 83 | a fixture banner matches the expected rule and the verdict is reported |
| `cmdRun`, `supervise` | 164 | the daemon starts, serves the API, reloads on SIGHUP, and exits cleanly on SIGTERM leaving no browser or temp dir behind (Rule 5) — `supervise` alone is 113 statements and the least-tested code in the repo |

`supervise` and the retry ladder are where a leak or a hung shutdown would live, so this phase is worth doing even after the number is green.

**After Phases 0–3: ~82%.** Target met with ~1.5 pt of headroom.

---

### Phase 4 — buffer, in cost order (only if Phases 1–3 underrun)

| Work | Stmts | Cost |
|---|---|---|
| Run the existing `store` suite against MySQL and Postgres in CI (`make test-store-mysql` and `make test-store-postgres` already exist; add service containers) | ~56 | **No new tests.** `mysql.go` 26/55 and `postgres.go` 15/42 are only exercised when the DSN env vars are set |
| `httpapi`: `handleGetBaseline`, `handleDeleteBaseline`, `handleSchedule`, `Serve`, `ArtifactLink`, `uiError` | ~150 | httptest, no browser |
| `browser/pool.go`: `Discard`, `discard`, `closeBrowser`, `restarted` recycle paths | ~80 | needs a fake launcher or a real Chrome; the recycle-on-crash path is untested today |
| `consent`: `applyTCF`, `readGPP`, `waitFor` | ~85 | needs a fixture page exposing `__tcfapi`/`__gpp` — `test/e2e/fixture` already serves a pinned Klaro banner, so extend it there |
| `capture`: `scroll`, `dwell`, `dismissDialog`, `captureFromView` | ~60 | fixture page with a long body and a `beforeunload`/`alert` dialog |

---

## 4. Summary

| Phase | Work | Stmts | Coverage after |
|---|---|---|---|
| 0 | `-coverpkg`, drop test scaffolding from the denominator, gate + ratchet | 0 | **71.2%** (from a reported 59.1%) |
| 1 | `cmd/wsaw` without a browser | +520 | ~76.2% |
| 2 | `internal/app` | +260 | ~78.6% |
| 3 | CLI end to end against fixtures | +350 | **~82%** |
| 4 | Buffer: store dialects in CI, httpapi handlers, pool recycle, TCF, capture interaction | +430 available | headroom |

Phase 0 is a few lines of `Makefile` and CI. Phases 1–2 are ordinary table-driven tests with no browser, and are where most of the real risk reduction is. Phase 3 is the one that needs Chrome, and it buys both the last three points and the only coverage the daemon supervisor has ever had.

## 5. Rules this plan will not break

- No new dependency (§6). `httptest` and `t.TempDir()` are enough.
- No live network in any test (Rule 7). Fixture servers only.
- No `--no-sandbox`, and no sandbox-disabling fixture to make a browser test pass (Rule 1). A test that can only pass with the sandbox off does not get written.
- No `time.Sleep` to paper over a race (§5). Inject the clock — `internal/daemon` already does.
- No test written purely to move the number: `main()` (`os.Exit`) and `test/e2e/browse` stay uncovered on purpose, which is why the denominator excludes the latter rather than pretending otherwise.
