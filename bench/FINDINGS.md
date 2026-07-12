# ccx token benchmark — July 2026 campaign findings

**Verdict: on this benchmark's regime, ccx costs tokens instead of saving them — on both models, with tight confidence intervals — while measurably improving answer accuracy.** The instrument is sound; the design premise ("an unaided agent floods context, ccx bounds it") is what failed. Per-call savings are real (microbench: 90% output-token reduction). They never reach the session ledger because modern baselines don't flood on single-question tasks over familiar code.

Both campaigns ran the released Homebrew ccx (v0.12.0), 450 runs each (30 tasks × 3 arms × 5 repeats), all validity gates green. Total spend: ~$200.

## Headline results

Paired per task on both-correct tasks, median across 5 repeats, bootstrap CI over tasks, sign test. Negative = ccx processed **more** tokens. T is billed main-model usage (retry-inclusive; reconstructs `total_cost_usd` exactly).

| Metric | sonnet ccx-mcp | sonnet ccx-cli | opus ccx-mcp | opus ccx-cli |
|---|---|---|---|---|
| Peak context H | **−13.9%** [−15.0, −12.6] | −2.5% [−3.5, −1.7] | **−16.7%** [−18.1, −15.5] | −3.3% [−4.9, −1.9] |
| Total tokens T | −8.5% [−20.0, +3.0] | **−22.4%** [−35.7, −9.0] | **−35.2%** [−58.5, −16.5] | −16.5% [−33.2, −2.3] |
| Tool-result tokens | ~3.3× baseline (p=0.0005) | ~2.9× baseline (p<0.0001) | ~3.5× baseline (p=0.0009) | ~4.0× baseline (p=0.0001) |
| Accuracy | 94.5% vs 91.5% | 95.2% vs 91.5% | 93.8% vs 93.8% | **96.1%** vs 93.8% |

Verdicts: **FAIL** on all four (model × arm) cells. Accuracy favors ccx in three of four — accuracy was never the problem.

Sessions: `results/20260711T202402Z` (sonnet), `results/20260712T033604Z` (opus). Earlier pilots: `results/20260709T144422Z`, `results/20260711T010914Z`.

## Why ccx loses here — four measured mechanisms

Full decomposition: [analysis/mechanism-report.md](analysis/mechanism-report.md).

1. **Round-trip inflation dominates ccx-cli's T loss (92%).** Each extra billed API call re-reads ~47k of cached context. The ladder's orient-first reflex (`repo overview` opens 27% of runs; baseline: 0%), sequential ccx rungs, and command-error retries add ~0.7 billed calls per run. `ccx exec`, the composition lane built to collapse exactly this, was used 0 times in 351 ccx-cli runs.
2. **The MCP schema tax is ~83% of ccx-mcp's H penalty.** 18 tool schemas ≈ 5.3k tokens in the cached prefix, paid every task. Nine of the 18 tools (48% of schema chars) were never called once. ccx-mcp actually makes *fewer* tool calls than baseline — its inversion is purely per-call size.
3. **ccx's structural output is larger than the call it displaces.** `symbol` returns 18.6× and `outline` 12.9× the native equivalent — because the baseline's equivalent is a 5-line grep window, not the full-file Read the microbench assumed. The 90% per-call saving is real only against a flood that never happens here.
4. **Adoption was guard-manufactured, not persuaded.** Sonnet reached ccx mostly via guard rewrites of its native commands; 8 sonnet + 2 opus runs ignored ccx entirely despite the ladder prompt (correctly integrity-excluded).

The dominant fixable retry tax was one ergonomic gap: models persistently write `--section 30,40` where ccx demanded `30-40` — 26% of all ccx-cli command errors. Fixed in ccx as of this campaign's follow-up commits (`internal/anchor` comma alias).

## The reality check that reframes the thesis

The flood premise itself had never been measured. [analysis/flood-audit-report.md](analysis/flood-audit-report.md) audits 426 real (non-bench) Claude Code sessions on this machine:

- 67.6% of real sessions contain at least one flood-shaped call, but floods are only **13.3% of tool-result tokens**, and the historical "Read = 71.6% of spend" figure no longer holds (Read is 41.7% of tool tokens; 96% of unbounded reads are already small).
- The guard-rewrite ceiling — the most ccx could save — is **3.6–3.9% of billed T**, heavily skewed (median session 0.18%, p90 6.2%, max 28.9%). The value lives in a read-heavy tail, not the typical session.
- At real session lengths, the MCP schema tax compounds to ~2.4–3.9% of T — roughly cancelling the entire ceiling. **The CLI path, with zero schema tax, is the only lane with positive headroom.**
- Weak-model signal (small n): haiku floods 2.4× more often with a ~9.8% ceiling — the honest deployment claim may be "use ccx where tool discipline is poor."
- Real sessions median 9.6M billed T over 64 turns — ~46× this benchmark's tasks. Retained-context compounding and compaction, the regime ccx was built for, is exactly what a 4-turn benchmark cannot exhibit.

## What survives the verdict

- Per-call bounded output is real: the microbench holds at 90% saving, regression-guarded. It just needs a flood to bound.
- The accuracy value is verified in transcripts: in `diff-tornado-response`, baseline misattributes a diff hunk while ccx-mcp's structural diff locates it 5/5 ([analysis/stageE-audit.md](analysis/stageE-audit.md)). Part of ccx's token cost bought correctness.
- The capacity thesis is untested, not refuted: whether bounded outputs let one session complete more work before compaction is the headline question of the redesign ([analysis/bench-redesign-proposal.md](analysis/bench-redesign-proposal.md)) — session-length work orders, prior-defeating corpora, flood-inducing task shapes, and a weak-model axis, staged cheapest-falsifier-first.

## Validity notes

- **Accounting lineage.** Three defects found and fixed during the campaigns, each with committed regression tests: turn-splitting double-counts (mid-turn `rate_limit_event`), parallel-tool interleaving double-counts (billing now aggregates by `message.id`), and the retry-exclusive envelope (T now sources from billed `modelUsage`, which the envelope undercounted on 18/448 opus runs — asymmetrically in ccx's favor). Final cross-checks: billing reconstruction clean on 898/898 runs.
- **Stream completeness.** 9 opus captures are missing ~one billed call each from the saved stream (capture loss, likely the same family as the 64 KiB truncation that voided 2 runs — open investigation). Billed T is complete regardless; transcript-sourced H is theoretically exposed but empirically unaffected.
- **Grading caveats.** The opus control-panel drop (85/80/75%) is one style-sensitive grader (`nonreg-binsearch` demanded the literal word "sort"); the `trace-tornado-target-delegate` "regression" is gold ambiguity (the symbol is defined at two sites). Both golds repaired for future campaigns; historical grades stand as graded with these caveats. Infra-lost runs (`is_error`) are excluded from accuracy denominators.
- **Power ceiling.** CIs resample over tasks (n ≤ 26 pairs), not runs; 5 repeats put per-task median noise near 9%. Effects of the measured size (14–35%) resolve decisively; a true ≤5% effect would not.

## Product changes motivated by these findings

Landed: `--section` comma-range alias (`A,B` ≡ `A-B`, CLI + MCP + edit). Proposed, pending decisions: terser `symbol`/`outline` defaults (−94% per locate call, needs an accuracy gate), MCP schema slimming (prune or halve descriptions; ~2.5–5k tokens per session prefix), ladder rewrite (chain ccx calls in one Bash invocation; drop orient-first on targeted lookups), and outline windowing / grep context flags models reached for and missed.
