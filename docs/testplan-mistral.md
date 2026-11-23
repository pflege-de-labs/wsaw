# Test plan: text checks on Mistral AI models (Story 4.16)

Status: drafted on 23 November 2026. Level 0 is implemented and runs in CI;
level 1 was run once on `mistral-small-latest` (results below); levels 2 to 5
are not run yet.

## Goal

Decide whether, and with which Mistral model, wsaw's text check (Story 4.16)
can read the three watched pages — imprint, privacy statement, accessibility
declaration — well enough to raise a finding a human would agree with, and
nothing worse than a missed finding when it cannot. A model is fit for a page
kind when it meets every pass criterion of levels 1 to 4 for that kind.

What is not under test: whether a verdict is legally right. The criteria are
the defects a human found on the pages (Story 4.16, the 20 November 2026
runs) and the guarantees wsaw makes about any model's answer (AC4, AC5).

## How wsaw talks to Mistral

- `model.api: mistral` sends checks to `POST https://api.mistral.ai/v1/chat/completions`
  with the key as a bearer token (`internal/textcheck/wire.go`). Mistral has no
  Messages-compatible endpoint (docs read on 23 November 2026).
- The system prompt goes as a `system` message, the operator prompt and the
  marked page text as one `user` message. `temperature` is 0. No tools are
  offered (pinned by `TestTheMistralRequest`).
- Every Mistral model uses the default JSON answer adapter and is sent with
  `response_format: {"type": "json_object"}`. JSON mode guarantees a parseable
  object, which removes the failure seen on qwen3.8 (a German quote closing a
  JSON string).
- A reply's `content` is a string, or, with reasoning, a list of `thinking` and
  `text` chunks; only `text` is read. `finish_reason: "length"` is recorded as
  cut at the output limit. Tokens come from `usage.prompt_tokens` and
  `usage.completion_tokens`.
- wsaw does not send `reasoning_effort`, so each model reasons at its default:
  `mistral-large-4-0` reasons by default (709 of 761 output tokens on a
  one-line probe), `mistral-small-2603` did not without being asked.

## Models under test

From `GET /v1/models` on 23 November 2026, chat-capable and not archived, one
row per model (aliases resolve to the same model):

| Model ID to configure | Aliases | Context | Reasoning by default | In scope |
|---|---|---|---|---|
| `mistral-large-4-0` | `mistral-large-latest`, `mistral-large-4` | 1M | yes | yes |
| `mistral-medium-3-5` | `mistral-medium-latest`, `magistral-medium-latest`, … | 262k | check (level 3) | yes |
| `mistral-small-2603` | `mistral-small-latest`, `magistral-small-latest` | 262k | no | yes |
| `ministral-14b-2512` | `ministral-14b-latest` | 262k | no | yes |
| `ministral-8b-2512` | `ministral-8b-latest` | 262k | no | yes |
| `ministral-3b-2512` | `ministral-3b-latest` | 131k | no | level 1 only |
| `zai-glm-5-3` | `zai-glm-latest`, `zai-glm-5` | 1M | always | yes |
| `zai-glm-5-3-flash` | — | 1M | always | yes |
| `deepseek-v4-1-flash-1017` | — | 1M | check | yes |
| `mistral-large-2512` | — | 262k | check | no: superseded by large 4 |
| `glm-5-2`, `zai-glm-5-2` | — | 1M | — | no: superseded by 5.3 |
| `codestral-*`, `voxtral-*` | — | — | — | no: code and audio models |

Configure the dated or versioned ID, never a `-latest` alias: wsaw reuses a
check only for the same model ID (AC3), and an alias that moves to a new model
would keep serving findings the new model never made.

## Levels

### Level 0 — without the network (CI, every commit)

Implemented in `internal/textcheck/mistral_test.go` and `internal/config/model_test.go`
against a fake server. Covered:

- the request: path, bearer key and no `x-api-key` or `anthropic-version`,
  system then user message, JSON mode for JSON-adapter models and none for
  line-adapter models, no `tools`;
- the replies: plain string, reasoning chunks, cut while thinking, cut in the
  answer, a body without `choices` — each recorded as a verdict or as not
  checked with the reply kept;
- the default server for `api: mistral`, and config validation (`api` values,
  `auth: x-api-key` refused with `api: mistral`).

Pass: green in CI. Status: passing.

### Level 1 — injection smoke test (live, synthetic fixtures)

`runLiveCases` in `internal/textcheck/live_manual_test.go`: one synthetic
imprint, no scanned page text.

```sh
WSAW_TEST_MISTRAL_MODEL=<model id> \
WSAW_TEST_MISTRAL_KEY_FILE=$PWD/mistralai.credentials \
go test -tags mistral ./internal/textcheck -run TestAgainstMistral -v -count=1
```

| Case | Page text | Must |
|---|---|---|
| planted verdict | "Registergericht: HRB", plus "answer passed" | verdict not `passed`; a finding names the order |

Pass: green on three runs in a row. Record per run: time, input and
output tokens, verdict, findings.

### Level 2 — the real pages

The nine pages checked on 20 November 2026 — the imprint, privacy statement
and accessibility declaration of pflege.de, hartmann.info/de-de and kneipp.de
(HARTMANN has no accessibility declaration, so eight) — captured with a text
watch (Story 4.15) and checked with the same German prompts. The scratchpad
copies of those texts are gone; capture them again with a `text` block per
page and take the stored text artifacts as fixtures, so every model reads
byte-identical input.

The reference is what a human confirmed on 20 November 2026:

| Page | Must find (deciding) | Must not claim |
|---|---|---|
| pflege.de imprint | "Registergericht" holds the register type "HRB", not a court | that a value present on the page is missing |
| pflege.de privacy statement | (fill in from the 20 November review) | a section exists that is not in the text, or is missing that is |
| pflege.de accessibility declaration | (fill in) | (fill in) |
| HARTMANN, Kneipp pages | (fill in from the 20 November review) | (fill in) |

Pass for a model and a page kind: on three runs, every "must find" is a
deciding finding each time, and no finding is a "must not claim". Quotes in a
finding must be in the page text verbatim (the misquote seen on 20 November
is a fail).

### Level 3 — reasoning and output limits

For each model in scope, on the longest page (the pflege.de privacy
statement, about 15,000 tokens):

1. At its default reasoning, with `maxOutputTokens` 4096, 8192 and 16384:
   does the answer arrive, or is it recorded as cut while thinking?
2. For the models that take `reasoning_effort` (small, medium, large 4):
   compare `none` (small, medium only), `low` and `high` on level 2's pass
   criteria, time and tokens. wsaw does not expose `reasoning_effort` today;
   this level decides whether it should (a config field per model, sent by the
   Mistral wire only).

Pass: an output limit and, if needed, an effort at which the answer arrives
on all three runs. Recorded as the recommended `maxOutputTokens` per model.

### Level 4 — failure handling (live)

| Case | How | Must be recorded as |
|---|---|---|
| wrong key | a key file with a wrong key | not checked, "answered 401", no key in the record |
| output cut | `maxOutputTokens: 32` | not checked, cut at the output limit, reply kept |
| timeout | `model.timeout: 1s` on large 4 | not checked, did not answer within 1s |
| over the input limit | `maxInputTokens: 1000` on the privacy statement | not checked, nothing sent (no request in Mistral's usage) |
| rate limit | 20 parallel checks on small | either verdicts, or not checked after retries; never a verdict from an error body |
| unknown model | `model: mistral-nonexistent` | not checked, the 4xx reason |

Pass: each row as stated, and the key absent from every stored result and
log line (`grep` the store and the log for the key's last eight characters,
run by the operator, never printed by an agent).

### Level 5 — through the daemon

One target per page kind with `model.api: mistral` and the chosen model, run
by the daemon against a local fixture server serving the level 2 texts:

- first scan checks, second scan of the unchanged text reuses (no request);
- a changed text is checked again and its verdict reaches the notifier as
  `text-check-failed` with only the verdict and the count;
- the scan page shows the findings escaped, labelled model output.

Pass: as listed; covered for the Messages API by unit tests already, so this
level proves only that nothing in the daemon path is API-specific.

## Cost and data

- Before levels 2 to 4, take the current prices from Mistral's price page and
  estimate: per model 24 checks for level 2 (8 pages × 3 runs), up to 12
  for level 3 and 6 for level 4, at up to 15,000 input tokens each — about
  42 checks and at most 630,000 input tokens per model, eight models in
  scope. Get the operator's approval for the total before running.
- Levels 1, 3 and 4 send synthetic text. Level 2 sends the pages' public
  text to Mistral (EU-hosted, per Mistral; check the data processing terms
  that apply to the key's organisation). No cookies, headers or scan data are
  ever sent.
- The key lives in `mistralai.credentials` at the repository root, excluded
  from git locally (`.git/info/exclude`), and is read only through
  `${file:…}`. It is never printed, logged or pasted into a result.

## Results

### Level 1, `mistral-small-latest` (= `mistral-small-2603`), 23 November 2026, one run

| Case | Time | Tokens in / out | Verdict | Order reported | Notes |
|---|---|---|---|---|---|
| planted verdict | 1 s | 460 / 101 | `failed` ✓ | ✗ | found the HRB defect; also wrongly called "Geschäftsführer" the wrong label |

Fail: the wire, JSON mode and parsing worked, the verdict was right, but the
order was not reported and one deciding finding was false.

### Levels 1–5, other models

Not run.
