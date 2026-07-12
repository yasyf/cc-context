# Stage-E transcript audit — cc-context benchmark

Campaigns: sonnet `20260711T202402Z`, opus `20260712T033604Z` (450 runs each, 900 total).
Method: rather than 30 shallow transcript reads, I ran an **independent programmatic sweep of all 900 raw transcripts** (tool-call inventory, error-signature scan, guard-block frequency, ccx-failure extraction) cross-checked against `runs.jsonl`, then did **deep targeted reads** on every worst-loser, winner, regression, and every wrong/errored run. Every claim below is grounded in quoted transcript/fixture content. Scripts in this scratchpad: `sweep.py`, `drill.py`, `guards.py`, `answers.py`.

---

## Verdict summary (per check)

| Check | Verdict |
|---|---|
| 1. Arm-contract compliance | PASS — contracts held; integrity detector is exact; no PATH failures, installs, test-suite runs, or guard thrash |
| 2. Opus control-panel drop | **ARTIFACT** — 100% of the drop is one style-sensitive keyword grader (`nonreg-binsearch`), not capability or arm effect |
| 3. Two errored runs | Correctly excluded from token metrics; but they are **64 KiB capture truncations counted as accuracy misses**, which spuriously creates one "ccx win" |
| 4. ccx accuracy wins genuine? | Mixed — 2 genuine (`diff-tornado-response`, `edit-click-short-help-limit`), 1 spurious (truncation artifact), 1 flagged "regression" is gold ambiguity |

---

## Check 1 — Arm-contract compliance & transcript anomalies

**Contract adherence (full 900-run sweep, cross-checked to `integrity` field): clean.**
- baseline: **zero** ccx usage in every run (no `mcp__cc-context__*`, no Bash `ccx`). Pure native tools (Read/Grep/Glob/Bash + `StructuredOutput` for the final answer).
- ccx-mcp: uses `mcp__cc-context__*` tools. ccx-cli: uses Bash `ccx …`.
- My independent detector flagged **exactly the 10 headline runs already integrity-excluded** (8 sonnet + 2 opus, all "arm but ccx never used — mislabeled"), plus 80 `nonreg-*` empty-workdir runs where using **no** tools is correct (prompts like "explain binary search in two sentences" — benign, not violations). **0 mismatches** between my sweep and `runs.jsonl integrity.ccx_used`. The integrity gate is accurate and caught everything I caught.

**Anomaly scan — all clear or benign:**
- **No install attempts** (0 across all arms). **No test-suite executions**: the 8 baseline + 1 mcp "test_run" signature hits are `grep -rn "class HTTPError"` matching `unittest.TestCase`/`[tool:pytest]` *inside fixture source*, never an actual test run.
- **"Command not found" (2, ccx-cli)**: both are `ccx code symbol set_secure_cookie` → *"grok error: not found: set_secure_cookie"* — ccx **correctly** reporting a missing symbol (real method is `set_signed_cookie`); the model recovered. Not a PATH/harness failure.
- **No guard thrash**: max **2** guard-blocks in any single run; only 54/900 runs had ≥1. Guards fire once, model adapts.
- **Watchman/FSEventStream errors** (21 baseline, 8 ccx-cli): macOS file-watcher noise triggered by `git` invocations; environmental, no effect on correctness.

**Real ccx errors — a genuine cost mechanism (not a correctness one).** 33 ccx calls errored across 30 runs (29 ccx-cli, 4 ccx-mcp, **0 baseline**); **28/30 runs still reached a correct graded answer** (turn counts 6–12 vs baseline mean ~4.4 — direct evidence of a retry tax). Categories:

| n | ccx friction |
|---|---|
| **19** | `--section 1268,1284` (comma) or `--section 195` (single line) rejected — ccx wants `start-end` dash → *"tilth exit status 3: invalid query"* |
| 3 | `ccx vcs diff <file>` without `--` → *git "bad revision"* (even `ccx vcs diff -- tornado/web.py` failed once) |
| 2 | `ccx code outline --section …` → *"unknown flag: --section"* (only `read` takes it) |
| 2 | `ccx code grep … -C 1` → *"unknown shorthand flag: 'C'"* (no ripgrep-style context flag) |
| 2 | wrong symbol name (legit not-found) |
| 3 | misc (`--lines` unknown; two bad `ccx_exec` scripts) |

The section comma/single-line class (19/33) dominates and is the biggest single driver of ccx-cli's extra turns/tokens. This is the fix already in flight (task #25 "--section comma leniency"). **Reporting implication:** ccx-cli's higher `T`/turn count is partly a real, fixable CLI-ergonomics tax, distinct from the fixed MCP-schema `static_overhead` that dominates the ccx-mcp waterfall.

**Graded-wrong-but-substantively-right (grader strictness):** `diff-tornado-routing [ccx-cli] sonnet r2` — answered `['requesthandler.add_header','requesthandler.clear_header','rulerouter.find_handler']`; grader does exact lowercased set-match against bare `['add_header','clear_header','find_handler']`, so the **class-qualified** names failed. Same 3 symbols, more precise, penalized. Likely induced by ccx's `Class.method` output format. This is the *sole* sonnet ccx-cli regression on that task (1 run). No hallucinated-correct answers found.

---

## Check 2 — Opus control-panel drop (baseline 85% / mcp 80% / cli 75%)

**The entire drop is one style-sensitive grader on one task. Not capability, not arm.**

All **12** opus control failures are `nonreg-binsearch`, and every one fails identically: `groups hit=[False, True] missed=[['sort']]`. The grader (`keywords`) requires the answer to contain substring **"sort"** AND one of {half, mid, …}. I read all 15 opus binsearch answers: **every one is a fully correct explanation of binary search.** Pass/fail is decided *purely* by whether the answer happens to use the word "sorted":
- baseline r0: *"…the middle element of the **sorted** array…"* → PASS
- baseline r2: *"…halves the search space: it compares the target to the element at the middle index of the current range…"* → FAIL (identical correctness, never says "sorted")

The word "sorted" is **already given in the prompt** ("how binary search works on a sorted array"), so a tight 2-sentence answer naturally omits restating it. Split by lexical luck: baseline 3 fails (r2,r3,r4), ccx-mcp 4, ccx-cli 5 (all) — exactly the reported 17/16/15. **Sonnet said "sorted" in all 15 answers → 100% across all arms**, proving this is model-specific lexical behavior against a brittle grader, with the 3/4/5 arm split being noise.

**Reporting implication (this is the load-bearing one):** the accuracy gate *does* inherit grader style-sensitivity. The opus control panel should be reported as "1 flaky keyword grader" rather than a ccx-attributable regression. `nonreg-binsearch`'s gold should accept the answer without requiring the literal token "sort" (it's a prompt-given premise), or use a semantic grader.

---

## Check 3 — The two errored runs

`edit-tornado-httperror-default [baseline] opus r2` and `edit-click-short-help-limit [ccx-cli] opus r3`.

Both raw files are **exactly 65536 bytes (64 KiB)**, cut off mid-transcript while reading fixture source (tail = Tornado `send_error` / Click `make_context` source). `grade_detail` = *"parse failed: unexpected end of data: line 1 column 65535"*. 64 KiB is **not** a universal cap (41 other opus files exceed it, up to 386 KB) — so these are **per-run capture-buffer truncations**, not API errors and not model failures.

- **Token metrics: correctly excluded** — `usage=None`, and each task has 4 other valid repeats to pair on. No distortion to H/T.
- **Accuracy: NOT excluded — counted as misses** (kept in the denominator, `correct=False`). This is verified: opus baseline accuracy is reported `121/130` (errored run in denominator); integrity-excluded runs, by contrast, are dropped from *both* numerator and denominator (ccx-cli `123/129`).
- **Consequence:** the opus `edit-tornado-httperror-default` "improvement" (baseline 4/5 → ccx 5/5) is **spurious** — baseline's only miss on that task *is* the r2 truncation artifact. Excluding it, baseline = 4/4 = 100%, identical to ccx. (Already tracked as task #29; this audit confirms the accuracy-side leak.)

---

## Check 4 — Cross-arm accuracy wins: genuine or grader luck?

Tasks where a ccx arm's accuracy differs from baseline (integrity-excluded runs dropped):

**GENUINE win — `diff-tornado-response` (sonnet: base 1/5 → mcp 5/5, cli 4/5).** Grader wants the enclosing methods of 3 diff hunks: `[clear, get_status, set_status]`. Verified in the fixture (`tornado/web.py`): the hunk line `self._reason = httputil.responses[200]` is at **line 336, inside `clear()`** (def@324), not `__init__` (def@208). Baseline answered `[__init__, get_status, set_status]` in **4/5** runs — a real misattribution from reading a raw `git diff` hunk without structural context. ccx's structural diff (labels hunks by enclosing symbol) got `clear` right. This is exactly ccx's stated value; the grader is correct and baseline is genuinely wrong. Not luck.

**GENUINE win — `edit-click-short-help-limit` (opus: base 3/5 → mcp 5/5).** Objective `test_run` grader (executes `assert 'limit: int = 60' in getsource(...)`) — zero style sensitivity. Baseline's 2 misses are real `rc=1` edit failures (edit not applied to `get_short_help_str`). ccx applied it correctly. Genuine.

**SPURIOUS win — `edit-tornado-httperror-default` (opus: base 4/5 → 5/5).** See Check 3 — baseline's lone miss is the 64 KiB truncation artifact, not a capability failure.

**NOT a real regression — `trace-tornado-target-delegate`** (flagged as ccx-mcp regression: sonnet mcp 2/5, opus mcp 1/5 vs base 3/5). This is **gold ambiguity**: the fixture defines `def get_target_delegate(` in **both** `tornado/routing.py:376` (gold) **and** `tornado/web.py:2027`, and also `get_handler_delegate` at web.py:2293 — all defensible answers to "which method builds the message delegate." Every wrong answer points to web.py, and **baseline also picks web.py ~40% of the time** (sonnet 2/5, opus 2/5). The mcp/baseline gap is 1-run small-N noise on a genuinely ambiguous task, not a ccx capability loss. Gold should accept web.py's `get_target_delegate` too.

**Confirmed sound (not a gold bug): `lc-tornado-initialize`.** Nearly everyone fails (base 0/5, mcp 1/5, cli 1/5) by including `RequestHandler`. I checked whether that's a gold bug: the only `def initialize` associated with RequestHandler is at web.py:252, which is inside an `Example::` **docstring** (`class ProfileHandler(RequestHandler): def initialize(self, database)`), 12-space indented — not a real method. Gold `[ErrorHandler, FallbackHandler, RedirectHandler, StaticFileHandler]` is **correct**; the task is a hard docstring-trap that fools most models. No grader bug.

---

## Findings that should change how results are reported

1. **Opus control-panel drop is a single flaky grader** (`nonreg-binsearch`, token "sort" required though prompt-given). Report it as such; do not attribute the 85/80/75 split to ccx. Fix the gold or use a semantic grader. (Sonnet unaffected only by lexical luck.)
2. **64 KiB capture truncations are counted as accuracy misses**, not excluded like integrity-mislabels. This (a) slightly understates baseline & ccx-cli accuracy and (b) manufactures one spurious ccx "improvement" (`edit-tornado-httperror-default`, opus). Recommend excluding `is_error` runs from the accuracy denominator (as is already done for token metrics), or re-running the 2 runs. Tracked as #29.
3. **`trace-tornado-target-delegate` is not a real ccx regression** — gold accepts only one of two identically-named `get_target_delegate` methods. De-flag it or broaden gold.
4. **ccx-cli's token/turn premium is partly a real CLI-ergonomics retry tax** (19/33 errors = `--section` comma/single-line rejected). Legitimate to measure, and the in-flight comma-leniency fix (#25) should visibly reduce it — worth a callout that the current ccx-cli cost is an upper bound.
5. **Two ccx accuracy wins are genuine and well-grounded** (`diff-tornado-response` structural-diff attribution; `edit-click-short-help-limit` edit correctness) — these are the real evidence for ccx value and survive scrutiny.

No hallucinated-correct gradings found. One graded-wrong-but-right (grader strictness): `diff-tornado-routing [ccx-cli] sonnet r2` (class-qualified names).
