# Handoff: cc-context benchmark

A complete, pick-up-cold reference and plan for the cache-aware
cost-per-correct-answer benchmark (`bench/`). It covers what exists, how every piece
works, how to run the sonnet/opus campaign, how to extend it, and the road to a number
cc-context docs may cite. You should be able to take this over with no prior context.

## Table of contents

1. TL;DR and status
2. Why this exists (the rtk gate, the metric)
3. Repository map (every module)
4. Data contracts (Task, PrintResult, run record, config)
5. Arm design (every flag and why)
6. Cost model (prices, cross-check)
7. The corpus (categories, graders, fixture, OSS repos, inventory)
8. Integrity and confounds register
9. Operational runbook (build, selftest, run, report, resume, debug)
10. Part 1 — run sonnet + opus (~$15)
11. Part 2 — reading RESULTS.md
12. Caveats and known issues
13. Extension recipes (add a task / repo / grader / model)
14. Campaign to the citable number
15. Workflow plan
16. Verification / deliverable
17. Decisions log
18. Appendix: verified prices, the real PrintResult shape, glossary

---

## 1. TL;DR and status

The harness drives real `claude -p --output-format json` headless runs in two arms
(baseline = native tools, ccx = facade MCP + ladder + guards), reads the cache-aware
cost Claude Code already computes, grades each run deterministically, and reports the
paired baseline-vs-ccx delta in **cost-per-correct-answer** and **accuracy**. Spawning is
delegated to **spawnllm** (`run_sync(spec) -> RunResult`, envelope-aware transient-overload
retry) and parsing to **cc-transcript** (`parse_print_result(raw) -> PrintResult`); cost
cross-check uses cc-transcript's `cost_of`; guards point capt-hook directly at the canonical
`plugin/hooks` pack. The bench is a real uv project (`uv run python -m ccxbench …`).

**Part C complete** (de-hack: spawnllm + cc-transcript + single-source guards). The harness
no longer hand-rolls subprocess argv, JSON parsing, retry, a local price table, or a
duplicated guard pack. Verified:
- 64 tasks: 52 from a generated fixture + 12 over pinned real repos
  (`gorilla/mux@v1.8.1`, `pallets/click@8.1.7`).
- No API spend: grader selftest (selftest builds 72 tasks across 9 categories; all graders
  pass on gold, fail on wrong, incl. the Go targeted-edit); `cost_of(usage,'haiku').total`
  matches a real haiku bill exactly to the cent (`0.05759 == total_cost_usd`); 19 unit
  tests; the guard probe drives the real PreToolUse path (a synthetic >20 KB Read is `deny`'d
  naming `ccx`).
- ~$1 of spend: a real haiku smoke run (1 nav task, both arms) produced correct, cost-exact
  (rel_delta 0.0000), integrity-OK records with ccx 65.9% cheaper per correct; both
  ccx-detection paths fire (facade MCP `mcp__cc-context__*` and Bash `ccx`); the integrity
  check correctly flags runs where ccx was available but unused.

Next step: the full sonnet+opus R=3 run on the §10 selection (configured and ready; §10
drives it — it may be running).

## 2. Why this exists (the rtk gate, the metric)

Every cc-context savings number to date is self-reported and cache-blind: tilth's
"−40% cost/correct" had no prompt-cache accounting; semble's accuracy was self-judged.
The rtk "token-compression illusion" critique is the north star: measure **cost per
correct answer** against the **real API bill** (input + output + cache-create +
cache-read), *with* a task-success number — never raw terminal tokens "saved". Transcript
mining (this project's own evidence) put `Read` at 71.6% of all tool-output tokens, so
file reading is the lever ccx targets. **No cc-context doc may quote a percentage until
this benchmark produces one.**

The metric, per model:
- `accuracy = correct / N`
- `cost_per_correct = Σ cost_usd / Σ correct` (cache-aware)
- Headline = **Δ accuracy (pp)** and **Δ cost-per-correct (%)**, paired over tasks present
  in both arms, with bootstrap CIs. A cost win bought with an accuracy loss is the rtk trap
  and is flagged, never reported as savings.

## 3. Repository map

A real uv project, **outside** the Go module (nothing in the shipped `ccx` binary depends
on it). `pyproject.toml` has `requires-python = ">=3.13"` and
`dependencies = ["spawnllm>=0.4.0", "cc-transcript>=4.2.0", "capt-hook>=3.14.0"]`; a
`uv.lock` is committed and commands run via `uv run python -m ccxbench …`. Package
`bench/ccxbench/`:

| File | Responsibility |
|---|---|
| `config.py` | Load `config.toml` into typed `Config`/`Repo`. No local price table — prices live in cc-transcript's `cost.PRICING`. |
| `types.py` | Frozen dataclasses: `Task`, `Grader`, `Symbol`, `Usage`, `GradeResult`, `Integrity`; `ARMS`, `CATEGORIES`. |
| `cost.py` | `crosscheck(pr, model, tolerance)` calls `cc_transcript.cost.cost_of(usage, model) -> CostBreakdown` (`.total`) and compares vs `total_cost_usd`. No local price table; cc-transcript's `PRICING` is the single source of truth. |
| `integrity.py` | `assess(pr, arm)` over a `PrintResult` — did the arm behave as labeled? Iterates `pr.messages[].blocks` (typed `ToolUseBlock`/`ToolResultBlock`) to detect ccx use (facade or Bash), guard fires, heavy native calls, and **answer-key reads**. |
| `graders.py` | Deterministic graders: `file_line`, `file_match`, `set_match`, `keywords` (incl. any-of groups), `test_run`. `GradeContext` carries result text + workdir. |
| `grade.py` | `grade(task, pr, workdir)` reads `pr.structured_output`/`pr.result`/`pr.is_error` and dispatches by grader kind; an errored run is incorrect. `synthetic_result(structured_output, result_text="", is_error=False) -> PrintResult` builds a zero-cost `PrintResult` (via `parse_print_result`) for selftests/unit tests. |
| `fixtures.py` | Generate the deterministic multi-language fixture repo + ground-truth manifest (line numbers computed from content; call edges validated). Includes a 25 KB generated Go file. Manifest is written **outside** the repo tree. |
| `taskgen.py` | `generate(manifest)` → the 52 fixture tasks; `oss_tasks()` → the 12 real-repo tasks. |
| `repos.py` | Clone pinned OSS repos (idempotent). |
| `arms.py` | `build_run_spec(cfg, task, arm, model, workdir) -> RunSpec` (prompt, model, schema, cwd, env, timeout, `provider_configs={"claude": ClaudeConfig(...)}`). `guard_command`/`guards_available` invoke `uvx --from 'capt-hook>=3.14.0' capt-hook --hooks <plugin_hooks> …`; the probe drives the real PreToolUse path. Holds `LADDER` and `BASELINE_CONTROL`. |
| `runner.py` | `spawnllm.run_sync` → `cc_transcript.parse_print_result` → integrity → cost cross-check → grade → JSONL record; budget ceiling; session `meta.json`. spawnllm owns transient-overload retry (no salvage). |
| `report.py` | Aggregate JSONL → `RESULTS.md`: per-arm tables, paired headline + bootstrap CIs, non-regression slice, integrity/confounds section. |
| `__main__.py` | CLI: `build-corpus`, `list-tasks`, `selftest`, `crosscheck`, `run`, `pilot`, `report`. |
| `ladder.txt` | The ccx ladder appended to the ccx arm's system prompt. |
| `baseline_control.txt` | Length-matched "navigate efficiently" prompt for the baseline arm. |

Other: `config.toml`, `run.sh`, `pyproject.toml`, `uv.lock`, `README.md`,
`tests/test_harness.py`, `tasks/*.json` (materialized corpus). Git-ignored: `.work/`,
`.fixtures/`, `results/`.

## 4. Data contracts

### Task (`tasks/*.json`, `types.Task`)

```json
{
  "id": "click-nav-command",
  "category": "navigation",
  "repo": "click",                       // "fixture" or an OSS repo name
  "prompt": "…",
  "schema": { "...": "JSON Schema for --json-schema" },
  "grader": { "kind": "file_line", "spec": { "line_tolerance": 2 } },
  "gold": { "file": "src/click/core.py", "line": 1160, "verify_decl": "class Command(" },
  "ccx_helps": true,                     // false for non_regression
  "setup": { "edits": [ { "file": "...", "find": "...", "replace": "..." } ] }
}
```

### `PrintResult` (from `cc_transcript.parse_print_result(raw: bytes)`)

The harness never parses the `claude -p` JSON itself; cc-transcript (`>=4.2.0`, resolves to
5.0.0) turns the raw bytes into a typed `PrintResult`. Fields:
- `total_cost_usd`, `model_usage` (`Mapping[str, ModelUsage]`), `usage` (`Usage`),
  `structured_output` (the `--json-schema` object — graded), `num_turns`, `is_error`,
  `result` (`str | None`), `session_id`, `fast_mode_state`, `stop_reason`,
  `permission_denials`, `init` (`InitInfo | None`), `messages` (`tuple[PrintMessage]`).
- `Usage`: `input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`, `cache_creation` (`CacheCreation | None` with
  `ephemeral_5m_input_tokens`/`ephemeral_1h_input_tokens`), `service_tier`, `inference_geo`.
- `PrintMessage.blocks` are typed blocks: `ToolUseBlock` (`.name`/`.input`/`.id`),
  `ToolResultBlock` (`.content: str`/`.is_error`), `TextBlock`, `ThinkingBlock`. `integrity`
  iterates these for the ccx/heavy-call scan and the guard-fire scan.
- `InitInfo`: `.mcp_servers`/`.plugins` are `McpServer`/`Plugin` objects with `.name` — the
  real surface (captured per run to prove arm symmetry).

### Run record (`results/<session>/runs.jsonl`, one per run)

`task_id, category, arm, model, model_ids, repeat, ccx_helps, is_error, correct,
grade_detail, total_cost_usd, cost_recomputed_usd, cost_rel_delta, cost_ok, cost_note,
num_turns, usage{input,output,cache_read,cache_create_5m,cache_create_1h}, guards_active,
integrity{ok,ccx_used,guard_fired,ccx_calls,native_heavy_calls,note},
init{mcp_servers,plugins,n_tools,n_skills}, session_id, stop_reason`. Raw `-p` payloads are
saved under `results/<session>/raw/<run_id>.json`; session config + env fingerprint in
`meta.json`.

### config.toml

`[run]`: `models`, `repeats`, `budget_usd`, `permission_mode`, `timeout_s`, `strip_mcp`,
`disallowed_tools`, `cost_tolerance` (the cross-check knob, default `0.02`; surfaced as
`Config.cost_tolerance`). `[paths]`: `ccx_bin`, `plugin_hooks` (`= "../plugin/hooks"`, the
canonical pack), `work_root`, `fixtures_root`, `results_dir` (all relative to `bench/`). No
`[prices]` table — cc-transcript's `cost.PRICING` is the source of truth. `[[repos]]`:
`name`, `url`, `ref`, `kind`.

## 5. Arm design (every flag and why)

Both arms run in the **default** Claude config dir — OAuth is keychain-bound, so a
relocated `CLAUDE_CONFIG_DIR` loses auth (verified). The only differences are ccx itself.

Shared argv: `claude -p <prompt> --output-format json --model <model> --json-schema
<schema> --permission-mode bypassPermissions --mcp-config <json>` plus
`--strict-mcp-config` (when `strip_mcp`) and `--disallowedTools <list>`. Same fresh
fixture/OSS checkout as cwd; arms interleave across repeats so neither rides the other's
warm cache.

- **baseline**: `--mcp-config '{"mcpServers":{}}'` (no servers) + `--append-system-prompt
  <BASELINE_CONTROL>` (matched frugality guidance) and no ccx on PATH.
- **ccx**: `--mcp-config` with the `cc-context` facade (`ccx mcp`, local HEAD binary) +
  `--append-system-prompt <LADDER>` + `ccx` prepended to PATH + (when `guards_available`)
  `--settings` registering `uvx --from 'capt-hook>=3.14.0' capt-hook --hooks <plugin_hooks>
  run PreToolUse`, pointed at the canonical `../plugin/hooks` pack.

Why these choices: `--strict-mcp-config` strips the operator's ambient MCP servers
(verified to yield `mcp_servers: []`); ambient *plugins* still load identically in both
arms and cancel in the paired delta (each run records its init surface so symmetry is
measured); `bypassPermissions` gives headless autonomy without changing what the model can
do; the matched control prompt means the delta isolates ccx's tools/guards, not the
ladder's generic "be frugal" advice; `disallowed_tools` (`WebSearch`, `WebFetch`, `Task`)
removes high-variance side channels equally.

## 6. Cost model (prices, cross-check)

`total_cost_usd` (Claude Code's own figure) is the headline. The harness independently
cross-checks it with cc-transcript's cost engine to catch a wrong price table or field:
`from cc_transcript.cost import cost_of`; `cost_of(usage, model) -> CostBreakdown` whose
`.total` is the recomputed dollar figure. cc-transcript's default `PRICING` table (opus
5/25, sonnet 3/15, haiku 1/5, fable 10/50, with cache multipliers 5m=1.25 / 1h=2.0 /
read=0.1) is the **single source of truth** — there is no local price table to drift.

`cost.crosscheck(pr, model, tolerance)` calls `cost_of` and flags any run where
`|recomputed − reported| / reported > tolerance`. The tolerance lives in `config.toml
[run] cost_tolerance` (default `0.02`, surfaced as `Config.cost_tolerance`). Verified exact
to the cent on the real haiku payload: `cost_of(usage,'haiku').total == 0.05759 ==
total_cost_usd`. Prices: see appendix (they match `cost_of`'s `PRICING`); cache multipliers
are universal.

## 7. The corpus

### Categories and graders

| Category | Asks | Schema | Grader | Gold |
|---|---|---|---|---|
| navigation | where is X declared | `{file, line}` | `file_line` (±2 lines) | `{file, line, verify_decl?}` |
| callees | which repo funcs does X call | `{callees:[]}` | `set_match` (equal) | `{callees:[]}` |
| callers | which repo funcs call X | `{callers:[]}` | `set_match` (equal) | `{callers:[]}` |
| intent_search | which file does <NL behavior> | `{file}` | `file_match` | `{file}` |
| diff_review | which functions changed (uncommitted) | `{symbols:[]}` | `set_match` (equal) | `{symbols:[]}` + `setup.edits` |
| targeted_edit | make change Z | `{changed_file, summary}` | `test_run` (rc==0) | `{solution_edits}` (for selftest) |
| non_regression | general knowledge (ccx_helps=false) | `{answer}` | `keywords` (any-of groups) | `{groups:[[…],…]}` |

All graders are deterministic — grading is never done by the model under test.

### Fixture (`fixtures.py`)

A multi-language repo (Go/Python/TS/markdown) with 16 manifest symbols whose lines are
computed from the literal content and whose call edges are validated. Includes a ~25 KB
generated Go file (`internal/gen/generated.go`) so big-file reads trip the large-Read
guard and exercise outline-vs-full-read. The manifest (answer key) is written to a sibling
path **outside** the repo and never copied into a run; the integrity check flags any run
that reads `manifest.json`.

### OSS repos (`config.toml [[repos]]`, `taskgen.oss_tasks`)

`gorilla/mux@v1.8.1` and `pallets/click@8.1.7`. 12 tasks: big-file nav (click `core.py`
is 3042 lines), semantic intent search across many files, multi-file diff review, and a Go
targeted-edit graded with `go test`. Gold is verified against the pinned checkout at build
time (`verify_oss`: file existence; nav declaration within ±tolerance; edit find-strings
present), so authoring drift fails loudly before any spend.

### Inventory (run `uv run python -m ccxbench list-tasks`)

64 tasks: navigation 25, callees 10, callers 8, intent_search 8, diff_review 5,
targeted_edit 4, non_regression 4.

## 8. Integrity and confounds register

| Confound | Handling |
|---|---|
| Only ccx may differ | Same model/prompt/repo/base tools/permission mode/disallowed tools; ambient MCP stripped. |
| Ambient plugins | Load identically in both arms (cancel in the paired delta); init surface recorded per run; report counts distinct plugin sets (1 = symmetric). |
| Cache contamination | Fresh checkout per run; arms interleaved; the bill includes cold cache-creation. |
| Answer-key leak | Manifest kept outside the tree + deleted from workdirs; integrity flags any `manifest.json` read as invalid. |
| Mislabeled arm | ccx arm must use ccx or fire a guard; baseline must not. Counted in the report; headline paired only over both-arms tasks. |
| capt-hook built-ins | `capt-hook run` applies its BUILT-IN guards (styleguide, task/plan/vcs nudges) alongside the `--hooks` pack — no flag excludes them. So the ccx arm's PreToolUse = the cc-context navigation pack + capt-hook built-ins; baseline has none. For the read-only Q&A selections the built-ins are **provably dormant** (they fire only on Python edits, git commits, pending tasks, or plan-mode/numbered-prompt events — none of which occur in headless `-p` Q&A runs). Documented as a confound in `report.py`'s integrity/confounds section. |
| Cost truth | `total_cost_usd` cross-checked against `cc_transcript.cost.cost_of` within tolerance. |
| Stochasticity | R repeats; bootstrap CIs; paired (per-task) comparison. |
| Auth | Default config dir (keychain), not relocated. |
| Grading by the model under test | Avoided — deterministic graders only; no LLM judge in the corpus. |

The canonical `plugin/hooks` pack was refactored from one `ccx_guards.py` into
`read_guards.py`/`search_guards.py`/`vcs_guards.py`/`command_rewrites.py`/`common.py` (all
ccx-navigation: large-Read/rg/git-diff/sed → ccx). capt-hook is at 3.18.0 and the pack
passes (119 tests = 71 built-in + 48 pack).

## 9. Operational runbook

All commands from `bench/`, via `uv run`.

| Command | Effect |
|---|---|
| `uv run python -m ccxbench build-corpus` | Clone repos, build the fixture, verify OSS gold, write `tasks/*.json`. |
| `uv run python -m ccxbench list-tasks` | Count tasks by category. |
| `uv run python -m ccxbench selftest` | Graders pass on gold, fail on wrong (no API spend). The gate before paying. |
| `uv run python -m ccxbench crosscheck <raw.json>` | Read a saved `-p` payload via `parse_print_result` and cross-check its cost vs `total_cost_usd` (`cost_of`). |
| `uv run python -m ccxbench run [filters]` | Real runs → `results/<session>/{runs.jsonl,RESULTS.md,meta.json}`. |
| `uv run python -m ccxbench pilot` | A tiny `run` (1 task/category, 1 repeat) to validate end to end. |
| `uv run python -m ccxbench report <session>` | Rebuild `RESULTS.md` from a session's `runs.jsonl`. |
| `uv run python -m unittest tests.test_harness` | 19 unit tests. |

`run`/`pilot` filters: `--tasks id,id`, `--categories a,b`, `--sample N` (per category),
`--limit N`, `--models a,b`, `--repeats N`.

Operational notes:
- **Resume / partial**: records flush to JSONL per run, so a kill or budget halt leaves a
  readable partial; re-aggregate with `report <session>`.
- **Budget**: a soft ceiling checked before each run; spend accrues after. spawnllm retries
  transient overloads internally, so there is no salvage path — a still-erroring run is
  recorded as an error with cost 0. Set in `config.toml`.
- **Debug a run**: read `results/<session>/raw/<run_id>.json` (the full payload) and the
  record's `integrity.note` / `grade_detail`.
- **Kill a background run**: stop the background task (do not leave it spending).
- **Wall-clock**: sequential, ~30–90 s/run; budget the clock as well as the dollars.

## 10. Part 1 — run sonnet + opus (~$15)

`config.toml [run] budget_usd = 15.0`. High-value selection (12 OSS + 4 big-file fixture
nav), both models, R=3 — where ccx should matter, bounded by the cap. This is the next step
(it may be running).

```bash
cd bench
uv run python -m ccxbench build-corpus
uv run python -m ccxbench selftest
uv run python -m ccxbench run \
  --tasks mux-nav-router-match,mux-nav-getname,click-nav-command,click-nav-option,click-nav-split-opt,mux-intent-dispatch,mux-intent-regexp,click-intent-parser,click-intent-decorators,mux-diff-multifile,click-diff-parser,mux-edit-routecount,nav-Gen0,nav-Gen10,nav-Gen42,nav-GeneratedTotal \
  --models sonnet,opus --repeats 3
```

Choices: a selection (not all 64) because $15 will not cover the full corpus × R × 2
models; R=3 for paired statistical rigor on the selection; guards active via the canonical
`plugin/hooks` pack (capt-hook 3.18.0). Drop `--tasks` and raise the budget to run
everything. Runs ~1–2 h sequentially.

## 11. Part 2 — reading RESULTS.md

Per model: a per-arm table (accuracy, cost-per-correct, mean cost, mean turns,
integrity-OK %, cost-check-OK %); a **headline** paired over both-arms tasks (Δ accuracy
and Δ cost-per-correct, each with a bootstrap CI, plus a verdict that flags the rtk trap);
a non-regression slice (harm check, with the inf-baseline case guarded); and an
integrity/confounds section (mislabeled runs, cost divergences, distinct plugin sets,
guard activity). The citable line is Δ cost-per-correct **with** the accuracy delta — never
one without the other.

## 12. Caveats and known issues

- **Small/easy tasks make ccx look bad** — fixed facade overhead it cannot amortize; weight
  the corpus toward large-context tasks.
- **Weaker models ignore the ladder** when guards do not fire (haiku did); guards and model
  capability both matter.
- **Budget is soft**, not a hard pre-charge; worst case it overshoots by one in-flight run.
- **OSS gold is commit-pinned**; re-pinning means re-verifying (build-time checks cover
  nav/file existence and edit find-strings; re-read diff/edit gold by hand).
- **capt-hook built-ins ride along** — `capt-hook run` always applies its own guards
  alongside the `--hooks` pack; provably dormant for the read-only Q&A selections (see §8).
- **Sequential wall-clock**.
- **cwd matters** — run from `bench/` (`uv run`).

## 13. Extension recipes

- **Add a fixture task**: add to the relevant `taskgen` builder (gold derived from the
  manifest) or as a curated literal; `build-corpus` + `selftest` must stay green.
- **Add an OSS task**: extend `taskgen.oss_tasks()`; give nav tasks a `verify_decl`; ensure
  edit/diff find-strings exist; `build-corpus` verifies gold against the checkout.
- **Add an OSS repo**: add a `[[repos]]` entry; `repos.clone_all` handles it; author its
  tasks; gold is build-verified.
- **Add a grader**: implement in `graders.py`, register in `grade.GRADERS`, add a unit test
  and a `selftest` correct/wrong pair.
- **Add a model / price**: add the family to cc-transcript's `cost.PRICING` (and release
  it); the cross-check guards drift. There is no local price table to edit.
- **Change spend**: `budget_usd`, `repeats`, the `--tasks` selection.

## 14. Campaign to the citable number

| Step | Work | Done when |
|---|---|---|
| 1. Guards on HEAD | ✅ Done — capt-hook 3.18.0 loads the canonical `plugin/hooks` pack; the bench points `--hooks` directly at it (no extraction). | guard probe green on the canonical pack |
| 2. Corpus depth | More OSS repos (varied size/language) + harder multi-hop tasks (call-chain trace, cross-file refactor with the repo's own tests) | ≥3 repos, ≥30 large-context tasks, all gold build-verified |
| 3. Statistical rigor | R≥5 on the final corpus; paired CIs + sign test; disclose any LLM-judged task (none today) | CIs exclude 0 or are reported inconclusive |
| 4. Per-model | Full corpus on the models docs will claim about (opus + sonnet) | a RESULTS.md per model |
| 5. Cost integrity | Keep the `cost_of` cross-check green; bump cc-transcript's `PRICING` for any new family/premium regime | within tolerance every run |
| 6. The doc gate | No doc quotes a % until a green RESULTS.md exists; cite Δ cost-per-correct **and** Δ accuracy together | the README/site references the session |

## 15. Workflow plan

The run is one long background command, not a fan-out. Main agent runs phases in sequence,
reading each before the next.

| Phase | Shape | Verification |
|---|---|---|
| Build + gate | single | `build-corpus` clean; `selftest` green; `crosscheck` exact |
| Run | single (background) | `RESULTS.md` written; integrity-OK high; cost-check OK |
| Read | single | headline Δ has a CI; confounds section clean |
| Campaign step N | per §14 | that step's "done when" met |

## 16. Verification / deliverable

- A green `results/<session>/RESULTS.md` on sonnet (and opus): cache-aware cost-per-correct
  and accuracy per arm, paired delta with CIs, integrity clean.
- One-command reproduction: `cd bench && ./run.sh run --tasks <selection> --models sonnet,opus`.
- The single sentence a doc may cite: "on <corpus> at <model>, ccx changed cost-per-correct
  by X% (95% CI […]) at Δaccuracy Y pp" — and nothing stronger.

## 17. Decisions log

- Real OSS repos (not just a bigger fixture) for the complex tasks — realism where ccx's
  large-context value shows. (User.)
- ~$15 budget across sonnet + opus, R=3 selection. (User.)
- Handoff documents both this run and the full campaign. (User.)
- De-hack (Part C): spawning via spawnllm, parsing via cc-transcript, cost via
  `cost_of`, guards single-sourced at the canonical `plugin/hooks` pack — no hand-rolled
  argv, JSON parsing, retry, local price table, or duplicated guard pack. (User.)
- Guards point `--hooks` directly at the canonical `plugin/hooks` pack (capt-hook 3.18.0
  loads it; 119 tests pass), not an extracted copy. (Part C.)
- Baseline gets a matched control prompt so the delta isolates ccx, not generic advice. (Review.)
- Deterministic graders only; no LLM judge. (rtk: never grade with the model under test.)

## 18. Appendix

### Verified prices (2026-06-21, per MTok)

| Family | input | output | 5m write (1.25×) | 1h write (2×) | cache read (0.1×) |
|---|---|---|---|---|---|
| opus (4.5+) | $5 | $25 | $6.25 | $10 | $0.50 |
| sonnet (4.x) | $3 | $15 | $3.75 | $6 | $0.30 |
| haiku 4.5 | $1 | $5 | $1.25 | $2 | $0.10 |
| fable 5 | $10 | $50 | $12.50 | $20 | $1.00 |

These now live in cc-transcript's `cost.PRICING` (matched to `cost_of`), not a local table;
the figures above are reproduced for reference. Deprecated Opus 4.1/4 use legacy prices; add
their own entries to `PRICING` if benchmarked.

### The real `-p` payload (haiku, abbreviated)

The raw bytes `cc_transcript.parse_print_result` consumes; `total_cost_usd` and `usage` below
are what `cost_of` reproduces exactly (`0.05759`).

```json
{ "type": "result", "is_error": false, "result": "pong",
  "structured_output": {"answer": "pong"},
  "total_cost_usd": 0.05759, "num_turns": 3,
  "usage": {"input_tokens": 28, "output_tokens": 250,
            "cache_read_input_tokens": 51020, "cache_creation_input_tokens": 25605,
            "cache_creation": {"ephemeral_1h_input_tokens": 25605, "ephemeral_5m_input_tokens": 0}},
  "modelUsage": {"claude-haiku-4-5-20251001": {"costUSD": 0.05759, …}} }
```

### Glossary

- **arm** — baseline (native tools) or ccx (facade + ladder + guards).
- **cost-per-correct** — Σ cost / Σ correct, the cache-aware headline.
- **facade MCP** — the `cc-context` server (`ccx mcp`) exposing `mcp__cc-context__*` tools.
- **guard pack** — capt-hook PreToolUse hooks that block token-heavy primitives.
- **integrity** — per-run check that the arm behaved as labeled.
- **rtk trap** — a cost win bought with an accuracy loss; not a savings claim.
