# cc-context benchmark

The token-usage harness for `ccx`. It measures whether cc-context shrinks the
context a model processes at equal-or-better task accuracy, and it refuses to
report a savings number that an accuracy loss bought. No cc-context doc may
quote a savings percentage until this benchmark produces one.

Cost is not a metric here. The harness drives real `claude -p` headless runs,
grades every task deterministically, and reads token counts from the
API-reported usage envelope. A `safety_ceiling_usd` exists to stop a runaway
campaign; it never appears in a report.

## The two headline metrics

Both are paired per task (ccx arm vs baseline), per model, taking the median
across repeats. Both must favor ccx — bootstrap CI excluding zero — for a
verdict to PASS.

- **Peak context** `H`: the largest context the model ever holds — the max over
  turns of input + cache-create + cache-read tokens.
- **Total tokens processed** `T`: everything the model processes across the
  session — the sum over API calls of input + cache-create + cache-read, plus
  all output tokens. Cache reads count at full weight: caching changes price,
  not processing.

Per (model × ccx arm) the report renders mean savings (1 − arm/baseline) with a
paired bootstrap CI, win/loss/tie counts, and an exact sign-test p. The
accuracy gate is absolute: an arm's accuracy must be at or above baseline with
zero per-task regressions, or the verdict is FAIL regardless of the token
numbers. `tiktoken` appears only in the attribution waterfall, where
arm-vs-arm ratios cancel — never in a headline.

## The three arms

| Arm | Tool surface |
|---|---|
| `baseline` | Native tools (Read/Bash/Grep/Glob), ambient MCP stripped, matched control addendum |
| `ccx-mcp` | Baseline plus the cc-context facade MCP, `ccx` on PATH, capt-hook guards, MCP-flavored ladder |
| `ccx-cli` | Baseline plus `ccx` on PATH, guards, CLI-flavored ladder — and zero MCP servers |

The `ccx-cli` arm is the schema-overhead probe: its gap against `ccx-mcp`
isolates what the MCP tool schemas themselves cost in context, while its gap
against baseline measures ccx delivered purely through Bash. The three
system-prompt addenda are token-length-matched within ±15% (asserted by a unit
test), so the paired delta isolates the tools, not the frugality advice.

## The corpus

26 headline tasks over two pinned checkouts — tornado v6.4.1 and click 8.1.7 —
plus 4 non-regression controls that run in an empty workdir.

| Family | n | Shape |
|---|---|---|
| `navigation` | 6 | "where is X defined / what does Y return" against big files |
| `trace` | 4 | multi-file call-chain questions |
| `large_context` | 6 | enumeration predicates over many files |
| `diff_review` | 3 | review a multi-hunk uncommitted patch applied to the workdir |
| `targeted_edit` | 4 | change a behavior; graded by a self-contained offline check |
| `intent_search` | 3 | whole-repo "how does this concept work" |
| `non_regression` | 4 | control — no repo, ccx cannot help; excluded from headlines |

Every headline task carries `gold.traversal_files`: the files a naive baseline
must read to answer. Their bytes must sum to at least 100 KB in the pinned
checkout, which keeps ccx's fixed overhead (a few thousand tokens of schemas
and ladder) under 15% of the naive context. Small tasks that ccx can never
amortize are excluded by construction. `build-corpus` exits nonzero on a floor
violation and `selftest` prints the full floor table. gorilla-mux v1.8.1 stays
in the corpus config for the tool-level microbench, but hosts no headline
tasks: its entire non-test source is ~65 KB, and padding traversal sets to
clear the floor would be dishonest.

Gold answers are derived at build time from the pinned checkouts. No
hardcoded line number survives in generator source, and `build-corpus` is
byte-deterministic. diff-review golds are recomputed from the generated patch
itself, so they cannot drift. The answer key lives outside the repo tree; a
run that reads it is marked invalid, not scored.

## Running it

```bash
cd bench
./run.sh                       # build corpus, self-test graders, run the pilot
./run.sh run                   # full corpus at config.toml settings
python -m ccxbench selftest    # graders + floor table only, no API cost
```

Outputs land in `results/<session>/`: `runs.jsonl` (one record per run),
`meta.json` (arms, models, `corpus_sha`, env fingerprint), and `RESULTS.md`.

| Command | What it does |
|---|---|
| `build-corpus` | Clone the pinned repos, derive golds, write `tasks/*.json` + `tasks/patches/` |
| `list-tasks` | Task count per family |
| `selftest` | Prove every grader both ways and print the floor table (no API cost) |
| `run [filters]` | Real runs, written to `results/<session>/` |
| `pilot` | A small `run` (one task per family, 1 repeat) to validate end to end |
| `report SESSION` | Rebuild `RESULTS.md` from a session's `runs.jsonl` |
| `microbench` | Layer-1 per-intent ccx-vs-native token counts over all three repos |

`run`/`pilot` filters: `--tasks id,id`, `--categories a,b`, `--sample N` (per
family), `--limit N`, `--models a,b`, `--repeats N`, `--ceiling USD`,
`--concurrency N`.

## How confounds are handled

- Only ccx differs: same model, prompt, repo checkout, base tools, permission
  mode, and disallowed tools across all three arms; ambient MCP is stripped
  with `--strict-mcp-config`.
- Isolation is proved, not assumed. Every run records its init surface
  (`mcp_servers`, tool count, plugins). The report's isolation panel asserts
  `ccx-cli` saw zero MCP servers with baseline's tool count and `ccx-mcp` saw
  exactly `cc-context`, and renders a single env fingerprint per session. The
  `ENABLE_TOOL_SEARCH` env leak that once inflated an earlier campaign's
  headline is pinned per-run and re-proved per campaign.
- The arm order rotates every repeat, so no arm systematically runs first or
  rides a warm prompt cache; each run gets a fresh checkout.
- Integrity is checked per arm: a ccx arm must actually use ccx (or trip a
  guard) to count; baseline must not; any cc-context MCP presence in a
  `ccx-cli` run marks it mislabeled. Mislabeled and cheated runs are excluded
  from aggregates and counted in the report.
- Envelope `T` is recomputed from the transcript, and the report counts runs
  outside 2% agreement — the sanity check that replaced the old cost
  crosscheck.
- `meta.json` stamps a `corpus_sha` over `tasks/*.json`; the report warns when
  the corpus on disk has drifted from what ran.

## Note on guards

The capt-hook guard pack loads only if it imports cleanly. Without it the ccx
arms run with ladder (and, for `ccx-mcp`, the facade MCP) alone, and
`RESULTS.md` says so. The guards are ccx's enforcement layer: a weaker model
may ignore the ladder and reach for native tools, which the integrity check
flags either way.
