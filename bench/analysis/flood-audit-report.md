# Stage 0 — real-session flood-rate audit + guard-rewrite ceiling (Design A)

**Question (A8, the one untested premise of the ccx thesis):** do real, unaided Claude Code
sessions actually flood context with oversized tool output — enough that the guard pack's
bounded-output rewrites have real headroom?

**One-line answer:** Yes, they flood — but *small*. Unaided sessions are **not** the frugal
creatures the 4-turn bench baseline suggested (68% of real sessions do ≥1 flood; floods are
13% of tool-result tokens). But the guard-rewrite **ceiling is ~3.6–3.9% of total billed
tokens**, entirely dominated by large-`Read` bounding, and roughly **cancelled by the MCP
schema tax (~2.4–3.9% of T)**. The mechanism is alive but bounded to single-digit-% of spend,
concentrated in a read-heavy minority of sessions. Qualified GO to Design B/C — **ccx-CLI only**,
targeting the read-heavy tail with a capacity/compaction metric, not a marginal-% claim.

---

## Data & method

- **Deterministic transcript analysis only** — no agent runs, no API spend. Token counts are `chars/4`.
- **Corpus resolution.** The two stores collapse to one: `~/.cc-pool/accounts/*/projects/*` is a
  **16× mirror** of `~/.claude/projects` (41,424 files → **2,589 unique session UUIDs**; every unique
  basename has a `~/.claude` copy; **zero** cc-pool-only sessions; the most-mirrored session had 16
  byte-identical copies). The first analysis pass was silently inflated 15× by these mirrors (336
  identical 33 KB memory-file reads masqueraded as 336 floods). **Corpus = the 2,461 processed
  `~/.claude/projects` sessions.**
- **Classification** (thresholds read from `plugin/hooks/common.py`: `LARGE_READ_BYTES=20_000`,
  window `READ_WINDOW_LINES=100`; `GIT_DIFF_SUMMARY_FLAGS`; the literal/ident heuristics):
  1. `read_large` — a `Read` with **no** `offset`/`limit` whose returned text **>20 KB** (what the guard windows).
  2. `raw_grep` — `grep`/`rg`/`egrep`/`ag` as a pipeline **producer** (tree grep).
  3. `bare_git` — `git diff`/`show` with no pathspec/`--stat`; `git log -p`.
  4. `page_dump` — bare `curl`/`wget` (unpiped) or `WebFetch`.
  5. everything else = non-flood.
- **Contamination gate (excludes ccx-instrumented behavior).** A session is dropped if it **used ccx**
  (`mcp__cc-context__*` tool, or `ccx` at a Bash command position) **or fired a guard** (tool_result
  contains the `BLOCKED: … ccx …` / "floods context" / "Map it first" signature). This is the task's
  own heuristic for "unaided-native."
- **Sidechain note.** `isSidechain` is uniformly `false`; Claude Code writes each subagent to its **own
  transcript file**, so every file = one context. Subagent (opus/sonnet) work is therefore captured as
  standalone sessions, not lost.

### Sample (after exclusions)

| | count |
|---|---|
| processed (`~/.claude`) | 2,461 |
| excluded: trivial (<20 events) | 1,940 |
| excluded: **ccx used** | 63 |
| excluded: **guard fired** | 20 |
| excluded: no tool calls | 12 |
| **clean sessions** | **426** (419 after fork/resume prefix-dedup — changes nothing) |
| clean tool calls | 45,051 |

Contamination among **non-trivial** sessions: **83/521 = 15.9%.** (Lower than expected for a ccx-dev
machine because much ccx use is delegated into subagents and the newest sessions — most contaminated —
are a minority of all history.)

**Composition.** Dominant model: opus 272, fable 133, sonnet 17, haiku 4. Repo class: ccx-ecosystem
291, ccx-naive 135. **Per session (median): 466 events, 64 assistant turns, 80 tool calls, and 9.6M
billed tokens** (p90 53M). These are ~46× longer than the bench's 208k-T, 4-turn tasks — the regime the
bench truncated (confirms post-mortem A7).

---

## 1. Flood rate

| pattern | occ | per 100 calls | per session | % of sessions with ≥1 | result size (tok): med / p90 / max |
|---|---|---|---|---|---|
| `read_large` | 239 | 0.53 | 0.56 | **31.5%** | 6,777 / 12,243 / 16,348 |
| `raw_grep` | 1,235 | 2.74 | 2.90 | 50.7% | 145 / 670 / 5,689 |
| `bare_git` | 43 | 0.10 | 0.10 | 5.9% | 624 / 1,736 / 3,041 |
| `page_dump` | 109 | 0.24 | 0.26 | 8.7% | 255 / 526 / 10,588 |
| `cat_dump` | 1 | 0.00 | 0.00 | 0.2% | — |
| **ALL FLOOD** | **1,627** | **3.61** | **3.82** | **67.6%** | — |

**Read frugality (the load-bearing sub-finding).** Of 5,893 `Read` calls, **55.6% carry no offset/limit**
("unbounded"), yet only **4.1% (239) actually return >20 KB** — the guard's trigger. Unbounded-read
sizes: median 915 tok, p90 4,084, p99 11,478. **96% of reads are already small enough that the guard
never fires.** The model reads small files and precise windows unprompted; the flood is a thin tail, not
the default.

---

## 2. Token share

- **Total tool-result tokens = 17.4M = only 0.2% of billed T** as one-time output. When cache-read
  re-billing is accounted (each tool output is re-billed every subsequent turn), tool output is
  **27.8% of billed T**; the remaining ~72% is fixed overhead + non-tool history re-billed each turn
  (confirms post-mortem A4/A5: **T is dominated by retained context, not tool output**).
- **Floods = 13.3% of tool-result tokens** (first-appearance); **16.9%** of retained tool tokens.
  `read_large` alone is **10.8%** of tool-result tokens — grep/git/web are a combined 2.5%.
- **The "Read = 71.6% of token spend" figure no longer holds.** Read is now **41.7% of tool-result
  tokens** (and only ~11% of *billed T* compounded). The delegation era (orchestrator + subagents) plus
  proactive bounding moved the mix: `Bash` "other" output (4.6M tok) rivals Read (7.2M tok incl. floods).

---

## 3. Guard-rewrite ceiling

`expected saving = Σ (result_tokens × reduction)` over flood calls. Reduction factors: `read_large`/`cat_dump`
**0.90** (the guard windows a >20 KB read to 100 lines — well-founded, ≈85–90%), `bare_git` 0.70,
`page_dump` 0.80, `raw_grep` **0.40** (labeled assumption — the rg engine carries a per-call *premium*
over `grep -n` per the microbench; grep's real value is turn-level, not bytes). **These are generous → this is a CEILING, not realized value.**

| basis | saving | % of billed T | per session |
|---|---|---|---|
| first-appearance, all patterns | 1.91M tok | **0.02%** | 4,476 tok |
| first-appearance, reads-only | 1.70M tok | 0.02% | 3,988 tok |
| **retained (cache-read compounded), all** | ~354M tok | **3.93%** | — |
| **retained, reads-only** | ~326M tok | **3.61%** | — |

- **Headline ceiling ≈ 3.6–3.9% of total billed tokens** — the compounded figure is the meaningful one,
  because a token removed from context saves cache-read re-billing on every later turn.
- **Highly skewed:** per-session retained ceiling is **median 0.18%, p90 6.18%, max 28.9%**; only **34%
  of sessions clear 1%.** The value lives in a read-heavy tail, not the median session.
- **The kicker — schema tax roughly cancels it for the MCP arm.** The ccx-MCP arm's +18 tools add
  ~6k tokens of schema, re-billed via cache-read every turn: 6,000 × 64 median turns ≈ **2.44% of billed
  T** (median 3.94%/session, p90 11%). So **ccx-MCP's fixed tax ≈ its entire flood-rewrite ceiling** →
  near break-even at best (and MCP tool-results can be *larger* per the post-mortem A2). **Only ccx-CLI
  (no schema tax) keeps the full ~3.9% headroom.**

*Conservatism note:* ~25 `Bash` >20 KB dumps (`find`/`ls -R`/`cat`/test output) and some bounded-but-large
reads sit in "other," uncounted as floods. Counting them generously nudges the token share ~13→~15% and
the ceiling proportionally — it does not change the verdict.

---

## 4. Verdict

**The flood premise is confirmed but bounded — a qualified GO, not a kill.**

- **Real unaided sessions do flood** — 67.6% do ≥1 flood, 31.5% do ≥1 large unbounded read, floods are
  13% of tool-result tokens. The bench baseline's frugality (385–516 tok/run) was **a short-task
  artifact** (4 turns, one sniped grep), *not* an intrinsic property of unaided agents. In the long real
  regime (median 9.6M T, 80 tool calls) the flood tail is genuinely present.
- **But the realized ceiling is single-digit-% of spend** — ~3.6–3.9% of billed T with generous
  assumptions, concentrated in a read-heavy minority (p90 6.2%). It is **not** the dominant cost;
  retained context/overhead is (72% of T). ccx's per-call bounding only addresses the ~28% that is tool
  output, of which floods are ~17% — a ceiling of a ceiling.
- **Reads are the whole game.** `read_large` is 10.8% of tool tokens and ~3.6% of the T-ceiling;
  grep/git/web together are noise (<0.5% of T). Any redesign should optimize for the large-Read case.
- **MCP is disadvantaged on real sessions** — its schema tax (~2.4–3.9% of T, compounding) matches or
  exceeds the flood ceiling. Run Design B/C with **ccx-CLI**; give ccx-MCP a third arm only where the
  post-mortem's amortization argument (Design C, 15-task sessions) can actually bite.

**Implication for the staged plan:** don't chase a marginal-token-% headline (the ceiling caps it at
~4%). Design C's **session-capacity / compaction-count** framing is the right one — a read-heavy,
long-session work order where ccx-CLI's compounded read-bounding lets the agent go further before the
retained context forces compaction. Size work orders to be **read-heavy** (that's where the 28.9%-max
tail lives), or the effect stays in the noise.

## 5. Weak-model axis — UNDERPOWERED, directionally suggestive

| dominant model | sessions | median T | flood/100 calls | retained ceiling (% of T) |
|---|---|---|---|---|
| opus | 272 | 10.6M | **2.51** | 4.44% |
| fable | 133 | 11.2M | 5.60 | 2.82% |
| sonnet | 17 | 0.4M | 4.63 | 4.78% |
| haiku | 4 | 0.2M | **6.12** | **9.75%** |

Directionally consistent with "**weaker/smaller sessions flood more**" — haiku 6.1 vs opus 2.5 floods/100,
and haiku's ceiling (9.8% of T) is 2× opus's. **But sonnet (n=17) and haiku (n=4) are far too small to
claim anything**, and role confounds model (fable = orchestrator, opus = worker). **This machine cannot
supply a clean weak-model sample** — every real session runs fable/opus. The weak-model prediction must be
tested with a *deliberate* haiku/sonnet campaign (fold into Design B/C per the cross-cutting axis), not
mined from these transcripts.

## 6. Honesty caveats / sample biases

1. **Ceiling ≠ realized value.** Everything above is what the guards *could* save at the observed flood
   rate — not what an agent that has ccx actually spends. Generous reductions (read 0.90 = window rewrite),
   and the retained calc assumes flood tokens persist for all subsequent turns (`turns_remaining`, measured
   in tool-calls) and that the agent never needed the bounded-away content.
2. **ccx-dev-heavy machine.** 68% of clean sessions are ccx-ecosystem repos whose CLAUDE.md carries the ccx
   ladder ("outline first, bounded reads"), which *should* suppress flooding. **Mitigant:** the eco-vs-naive
   split is nearly identical (3.61 vs 3.60 floods/100; 13.5% vs 13.0% of tokens) — the ladder does **not**
   measurably suppress flooding in practice, so the eco-heavy sample is not badly biased on flood rate.
3. **Small clean weak-model n** (§5). No haiku/sonnet conclusions.
4. **Mirror + trivial attrition.** The corpus is really 2,589 sessions (cc-pool adds none); 79% are trivial
   (<20 events, mostly cron/quick sessions) and excluded — the 426 clean set is the substantive long
   sessions, which is the right unit but is a 17% slice of all sessions.
5. **`chars/4` tokenization** throughout (deterministic, offline) — not the API counter; fine for a ceiling.

---

### Reproduce
`flood_extract.py` (claude-store only, contamination gate, usage/T capture) → `flood_records.json`;
`flood_analyze.py` (rates/shares/model split), `flood_ceiling.py` (T-denominated ceiling),
`flood_final.py` (schema-tax cross-check, per-model), `flood_dedup2.py` (fork-dedup), `flood_dupcheck.py`
/ `flood_basecheck.py` (mirror proof). All in this scratchpad.
