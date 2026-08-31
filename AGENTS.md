# AGENTS.md — Rules of Engagement for Coding Agents

This file governs how automated coding agents work in the **wsaw** (website asset watcher) repository. It is binding. Where it conflicts with a general habit or a default behaviour, this file wins. Where it conflicts with an explicit instruction from a human in the current session, the human wins — but say that you are deviating and why.

Human contributors are welcome to follow it too; it is written for agents because agents need the parts humans infer.

---

## 1. Read before you write

Before the first line of code in a session, read:

| Document | What it settles |
|---|---|
| `epics-and-stories.MD` | What we are building, per story, with acceptance criteria |
| `non-functional-requirements.MD` | The quality bar — performance, reliability, security, privacy |
| `architecture-tenets.MD` | The standing design decisions that end arguments |

**The tenets are not advisory.** If a change violates a tenet, you do not have a design choice — you have a conflict. Stop, state the conflict, and let a human decide whether the design changes or the tenet does. Never silently work around a tenet.

If a requested change is not covered by any story, say so before implementing it. Scope creep dressed as helpfulness is the most common way an agent damages this repo.

---

## 2. Project shape

- **Language:** Go. Latest stable release. Standard library first (Tenet 18).
- **Platforms:** Linux and macOS, amd64 and arm64. No Windows.
- **Constraint:** pure Go, CGo-free, cross-compilable, single static binary. A dependency that breaks this is not an option, no matter how convenient.
- **External runtime dependency:** exactly one — a Chrome/Chromium binary. Do not add a second.
- **Domain:** wsaw drives headless Chrome against *untrusted, third-party websites* and reports what they load. Every page is hostile input.

---

## 3. Non-negotiables

Violating any of these is a defect regardless of whether tests pass.

1. **Never disable the Chrome sandbox** as a default, in code, in the Dockerfile, or in a test fixture. `--no-sandbox` is opt-in, warned about, and never how we make something work.
2. **Never interpolate page-controlled data** (URLs, headers, DOM text, script bodies, cookie values) into shell commands, file paths, SQL, or format strings without escaping. Page data never chooses a filesystem destination.
3. **Never log or serialize secrets** — basic auth, proxy credentials, webhook tokens, API tokens. Redaction is central; use it, do not reimplement it.
4. **Never let a failed observation look like a clean result.** "No CMP detected", "consent unverified", "body unavailable", "hit the request cap" are recorded outcomes with reasons. Returning an empty asset list because capture failed is the worst bug this product can have (Tenet 5).
5. **Never leak a process, temp directory, or file descriptor.** Every browser and every temp profile dir is cleaned up on every exit path, including panic and context cancellation.
6. **Never let one scan take down the daemon.** Blast radius is one scan. Recover panics at the scan boundary (Tenet 7).
7. **Never make a network call to a live third-party website in a test.** Tests use local fixture servers. Always.
8. **Never commit, push, or open a PR unless asked.** See §9.

---

## 4. Code conventions

### Go style

- `gofmt` (or `gofumpt`) formatted, always. Not negotiable, not a matter of taste.
- Package names: short, lowercase, no underscores, no `util`/`common`/`helpers` grab-bags. If you cannot name the package for what it does, the boundary is wrong.
- Exported identifiers are documented with a comment starting with the identifier name. Unexported ones are documented when non-obvious.
- Accept interfaces, return structs. Define the interface where it is *consumed*, not where it is implemented.
- **Interfaces only at the seams** (Tenet 12): capture engine, consent handler, target source, result store, differ, exporter, notifier. Do not add an interface for a single implementation on speculation.

### Errors

- Wrap with `%w` and enough context to locate the failure without a debugger: `fmt.Errorf("capture %s: %w", target.URL, err)`.
- Sentinel errors via `errors.Is`, typed errors via `errors.As`. No string matching on error text.
- Never `_ = err`. If an error is genuinely ignorable, write a comment saying why.
- `panic` is for programmer bugs only, never for control flow, and never crosses the scan boundary.

### Context

- Every function that does I/O, blocks, or drives the browser takes `ctx context.Context` as its first parameter.
- Never `context.Background()` below `main`/test setup. Never store a context in a struct.
- Every browser interaction has a deadline (Tenet 1). A CDP call without a timeout is a bug.

### Concurrency

- Goroutine lifetime is owned by whoever starts it, and it must be cancellable via context.
- No unbounded goroutine spawning. Worker pools with explicit limits.
- Run `go test -race` for anything touching concurrency; a race is a merge blocker.
- Do not introduce a new background loop, ticker, or watcher without wiring it into graceful shutdown.

### Logging

- `log/slog` only. No `fmt.Println`, no `log.Printf`, no third-party logging library.
- Structured key/value pairs, never string concatenation. `sloglint` enforces this.
- Every scan-scoped log line carries `scan_id`, `target`, and `consent_mode`.
- Log levels: `Error` = human intervention needed. `Warn` = degraded but handled. `Info` = lifecycle events an operator cares about. `Debug` = everything else. Per-request logging at `Info` is wrong.

### Linting

`.golangci.yaml` is the source of truth and is enabled with intent. Notable:

- `gosec` — security; do not blanket-suppress it. A `#nosec` needs a comment justifying it, and this repo scans hostile input, so the bar is high.
- `gocyclo` / `gocognit` — complexity. Hitting the limit means extract a function, not raise the limit.
- `noctx` — no HTTP requests without a context.
- `nilerr` — do not return `nil` after checking an error.
- `nlreturn`, `whitespace` — formatting; just comply.
- `godox` — **`TODO`/`FIXME` fail the lint.** Do not leave them. Either implement it, or raise it with the human, or write it into the stories document. An agent leaving `// TODO: handle this properly` in wsaw has not finished the task.
- `goconst` — repeated string literals become constants.

Do not edit `.golangci.yaml` to make your code pass. Fix the code.

---

## 5. Testing

- **Fast suite runs without Chrome installed.** Most of wsaw — normalization, diffing, severity, scheduling, retention, export — has nothing to do with a browser and must be testable without one (Tenet 13). If your new logic needs a real browser to be tested, the boundary is in the wrong place.
- **Integration tests use local fixture servers**: `httptest` servers serving a synthetic page, a synthetic consent banner, and a synthetic "third-party" origin. Never a live external site (Tenet 13, NFR §5).
- Browser-requiring tests are behind a build tag or a skip on missing Chrome, and say clearly why they skipped.
- Table-driven tests for pure logic. Use `t.Parallel()` where safe — `tparallel` and `thelper` are enabled, so mark helpers with `t.Helper()`.
- ≥ 80% coverage on core logic: capture normalization, diff engine, URL normalization, consent rule matching, severity rules (NFR §5).
- **Every bug fix starts with a failing test** that reproduces it.
- Determinism is a tested property, not an aspiration: assert that two scans of an unchanged fixture produce an empty diff (Tenet 6).
- No `time.Sleep` in tests to paper over a race. Synchronize properly or inject a clock.

---

## 6. Dependencies

Every dependency is a supply-chain liability in a tool whose entire purpose is detecting supply-chain changes. Act accordingly.

- **Do not add a dependency without asking.** Present the alternative (standard library, small vendored helper) and the cost.
- A new dependency must be: actively maintained, permissively licensed, CGo-free, and cross-compilable to all four target platforms.
- The justified set is roughly: CDP driver, public suffix list, YAML parser, embedded store, cron parser, metrics exporter. Additions beyond that need a reason.
- Never add a dependency for ergonomics alone (assertion libraries, `lo`-style helper packages, logging wrappers).
- `govulncheck` and license checks are merge blockers. Do not silence them.

---

## 7. Consent rules are data

CMP markup changes constantly. Handling a new banner must **never** require a Go release (Tenet 10).

- New CMP support goes into the YAML rule pack with fixtures, not into Go code.
- Only extend the rule *engine* when a rule genuinely cannot be expressed. Then extend it minimally — the engine must not become a scripting language.
- Prefer the API over the click (Tenet 11): TCF/`__gpp`/vendor SDK first, selectors second, heuristic label matching last and always flagged as heuristic in the result.
- Every rule change ships with a fixture page that exercises it.

---

## 8. Changing the result schema

The JSON result schema is the product's real interface (Tenet 16).

- Additive changes only, by default. Removing or repurposing a field is a breaking change requiring a human decision and a version bump.
- Update the published JSON Schema file and the compatibility notes in the same change.
- Raw capture is immutable and stored verbatim; derived data (normalized keys, classifications, diffs) is recomputable (Tenet 4). Never discard raw information to make output tidier.
- The same rule applies to metric names and labels — they are a public interface.

---

## 9. Git, commits, and boundaries

- **Do not commit unless the human asks.** Do not push. Do not open a PR. Do not create branches speculatively. Do not `git add -A` sweeping up files you did not touch.
- Never commit directly to the default branch.
- Conventional Commits: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`, `perf:`, `build:`, `ci:`. Reference the story where one applies (`feat(capture): record initiator chains (Story 1.2)`).
- Commit messages are written in normal prose. Explain *why*, not *what* — the diff shows what.
- Never rewrite published history, never force-push, never `git checkout --`/`git restore` over uncommitted work you did not create.
- Never modify or delete a file outside the repository.
- Secrets, `.env` files, real target lists, and captured scan output never get committed.

---

## 10. Working style

- **Finish the story you were given, not the one next to it.** Unrelated refactors, renames, and "while I was in there" cleanups are separate changes, and generally require asking first.
- **Do not fabricate progress.** If tests fail, say so and show the output. If you skipped something, say what and why. A green summary over a red test suite is a serious failure of this role.
- **Verify, do not assume.** Run the build. Run the tests. Run the linter. Do not report a change as working because it looks correct.
- Prefer the smallest change that fully satisfies the acceptance criteria.
- If a story's acceptance criteria are ambiguous, ask before guessing — but only about the ambiguous part. Do everything unambiguous first.
- Match the surrounding code's style, naming, and comment density. Do not import idioms from other ecosystems.
- Comment *why*, not *what*. Deleted code goes away; it does not get commented out.
- Commit each story and change separately.

---

## 11. Definition of done

A change is done when all of the following are true:

- [ ] It satisfies the acceptance criteria of its story, all of them.
- [ ] It violates no architectural tenet, or the conflict was raised and resolved by a human.
- [ ] `gofmt` clean, `go vet` clean, `golangci-lint run` clean — with no suppressions added and no config loosened.
- [ ] `go test ./...` passes, including `-race` for concurrent code.
- [ ] New logic has tests; a bug fix has a regression test.
- [ ] No `TODO`/`FIXME` left behind (`godox` will fail anyway).
- [ ] No new dependency, unless explicitly approved.
- [ ] Docs updated when behaviour changed: README, config reference, JSON Schema, and the story document if scope shifted.
- [ ] Every resource acquired is released on every path, cancellation and panic included.
- [ ] The report to the human states plainly what was done, what was verified, and what was left out.

---

## 12. When in doubt

Ask. Specifically, ask when:

- A change would violate a tenet or an NFR.
- A dependency would be added.
- The result schema, a metric name, or a CLI flag would change or be removed.
- The work is not covered by an existing story.
- A default would become less safe or less private.
- Something must be deleted, force-pushed, or otherwise made hard to reverse.

Asking a short question costs one round-trip. Guessing wrong in this repository costs a false compliance result, and a false compliance result is worse than no tool at all.
