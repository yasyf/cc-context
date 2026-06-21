# cc-context benchmark

The cache-aware **cost-per-correct-answer** harness for `ccx`. It measures whether
cc-context lowers the real API bill per correct answer without trading away accuracy. No
cc-context doc may quote a savings percentage until this benchmark produces one.

The harness drives real `claude -p` headless runs and reads the cache-aware cost Claude
Code already computes (`total_cost_usd` over input + output + cache-create + cache-read),
pairs it with a deterministic task grade, and reports the paired baseline-vs-ccx delta.

## The metric

Two arms run the same tasks on the same model in the same repo. Only ccx differs:

- **baseline** — native tools (Read/Bash/Grep/Glob), ambient MCP stripped, plus a matched
  "navigate efficiently" control prompt.
- **ccx** — the same, plus the cc-context facade MCP (`ccx_*` tools), `ccx` on PATH, the
  ccx ladder appended to the system prompt, and — when the guard pack loads — the
  capt-hook PreToolUse guards.

Both arms get length-matched frugality guidance, so the delta isolates ccx's tools and
guards rather than the generic "be token-frugal" advice the ladder also contains.

For each arm: `accuracy = correct / N` and `cost_per_correct = Σ cost / Σ correct`. The
headline is **Δ accuracy (pp)** and **Δ cost-per-correct (%)**, per model, with bootstrap
CIs over tasks. A cost win bought with an accuracy loss is the rtk trap and is flagged as
such — never reported as a bare "tokens saved".

## Quick start

```bash
cd bench
./run.sh                       # build corpus, self-test graders, cross-check cost, run pilot
./run.sh run --models sonnet   # full corpus on one model
python -m ccxbench selftest    # graders only, no API cost
```

Outputs land in `results/<session>/`: `runs.jsonl` (one record per run) and `RESULTS.md`.

## Commands

| Command | What it does |
|---|---|
| `build-corpus` | Generate the fixture repo + `tasks/*.json` |
| `selftest` | Every grader passes on gold and fails on a wrong answer (no API cost) |
| `crosscheck FILE` | Recompute cost from a saved envelope and compare to `total_cost_usd` |
| `run [filters]` | Real runs → `results/<session>/{runs.jsonl,RESULTS.md}` |
| `pilot` | A small `run` (one task per category, 1 repeat) to validate end to end |
| `report SESSION` | Rebuild `RESULTS.md` from a session's `runs.jsonl` |

`run`/`pilot` filters: `--tasks id,id`, `--categories nav,callees`, `--sample N`
(per category), `--limit N`, `--models a,b`, `--repeats N`.

## The corpus

`tasks/*.json` (≥40 tasks) across seven categories. Navigation, callees, callers, and
intent-search tasks are derived from a generated fixture's ground-truth manifest, so
their gold answers cannot drift from the repo. diff-review applies an uncommitted edit
whose changed symbols are the gold; targeted-edit is graded by running a check in the
post-run workdir; non-regression tasks (`ccx_helps=false`) confirm ccx adds no harm where
it cannot help. The fixture includes a 25 KB generated Go file so big-file reads exercise
ccx's core lever and trip the large-Read guard. Every grader is deterministic — grading
is never done by the model under test.

## How confounds are handled

- **Only ccx differs.** Same model, prompt, repo, base tools, permission mode, and
  disallowed tools. Ambient MCP is stripped with `--strict-mcp-config`.
- **Auth.** Runs use the default Claude config dir; OAuth is keychain-bound, so a relocated
  config dir would lose auth.
- **Ambient plugins** load identically in both arms (they cancel in the paired delta). Each
  run records its init surface (`mcp_servers`, `plugins`, tools), and the report asserts
  the arms were symmetric rather than assuming it.
- **Cache contamination.** Each run is a fresh fixture checkout; arms interleave so neither
  rides the other's warm cache. The measured bill includes cold cache-creation.
- **Answer key.** The fixture manifest (gold answers) is written outside the repo tree and
  never copied into a run; a run that reads `manifest.json` is flagged invalid by the
  integrity check, not scored.
- **Mislabeling.** Each run is integrity-checked: the ccx arm must actually use ccx (or a
  guard must fire); the baseline must not. The headline delta is paired over only the tasks
  present in both arms. Mislabeled runs are counted in the report.
- **Cost truth.** `total_cost_usd` is the headline, independently recomputed from `usage` ×
  per-model prices and asserted equal within tolerance.

## Configuration

`config.toml` sets models, repeats, the `$` budget cap (a soft ceiling checked before each
run; spend accrues after each run, including cost salvaged from unparseable runs),
permission mode, prices, and paths. Prices were verified 2026-06-21 against the published
table; the cost cross-check guards against drift.

## Note on guards

The capt-hook guard pack is loaded only if it imports cleanly. If it does not, the ccx arm
runs with the facade MCP + ladder alone and `RESULTS.md` says so. The guards are ccx's
enforcement layer; without them a weaker model may ignore the ladder and use native tools,
which the integrity check will flag.
