# Why ccx costs tokens vs a frugal native baseline — mechanism synthesis

**Inputs:** four lane analyses over the 450-run sonnet benchmark (150 runs/arm: baseline, ccx-cli, ccx-mcp; 26 headline tasks × 5 repeats + nonreg controls). Headline losses: ccx-mcp peak context (H) −13.9% (+6,429 tok/run), ccx-cli total tokens (T) −22.4% (+36–45k tok/run depending on dedup accounting), tool-result tokens ~2.3–2.6× baseline in both arms. Accuracy was BETTER on ccx in both arms.

**One-sentence mechanism:** ccx loses tokens through two nearly independent channels — a fixed MCP schema prefix tax (ccx-mcp) and extra billed round-trips that each re-read ~47k of cached context (ccx-cli) — while the visible "3× tool results" inversion is real but a secondary, compounding cost in both arms.

---

## 1. Ranked cost drivers

| Rank | Driver | Arm | Measured magnitude (tok/run) | % of that arm's loss |
|---|---|---|---|---|
| **1** | **Extra billed messages × ~47k cache re-read** — the sequential ccx ladder (orient → symbol → grep → read) plus retry turns; each rung is a separate API message re-billing the full context | ccx-cli | +0.71 billed msgs/run × ~47.2k = **+33.3k** (of +36.1k deduped gap) | **92.4% of ccx-cli's T loss** |
| 1a | └ Guard-block / error retry turns (13.1% of CLI calls error; 31–36 are the guard blocking `ccx … \| grep` pipes the model typed to trim output) | ccx-cli | ~0.45 wasted msgs/run ≈ **up to ~21k** (gross; overlaps 1b/1c) | ≤~60% (gross) |
| 1b | └ Orientation reflex — `ccx repo overview` opens 27% of runs (41/150); 0% in baseline | ccx-cli | 0.27 msgs/run ≈ **~13k** (gross) | ~35–40% (gross) |
| 1c | └ Extra `symbol` rung / read-after-search when the first hit already answered | ccx-cli | ~0.25 msgs/run ≈ **~12k** (gross) | ~35% (gross) |
| **2** | **MCP schema tax** — 18 tool schemas, 13,240 chars ≈ 5,266 tok, injected into the cached prefix; dead-constant per task | ccx-mcp | **+5,316** first-call context (baseline 46,286 → 51,601); re-read every one of ~4.1 turns → ~21k cumulative T exposure | **~83% of ccx-mcp's +6,429 H penalty** |
| **3** | **Tool-result inflation** — per-call payload 2.0× (cli) / 3.9× (mcp) native; same-intent ratios: ccx grep 3.7×, outline 12.9×, symbol 18.6× native grep; outline+grep+symbol = 70% of all ccx tool bytes | both | +2.3k chars/run ≈ +700 tok direct; with per-turn compounding = **~2.7k** of the cli gap; the ~17% residual of mcp's H penalty | **7.6% of ccx-cli's T loss; ~17% of ccx-mcp's H loss** |
| **4** | **Compliance-vs-utility dynamic** — steering text doesn't change what sonnet reaches for (38% of CLI runs type native grep/find/sed first; guard rewrites/blocks produce "adoption"); each explicit ccx call ≈ +1 turn; cheapest repeat used FEWER ccx calls in 22/26 tasks | ccx-cli (mech.) | Not a separate token bucket — it is the *cause* of 1a and part of 1c | (framing of driver 1) |
| **5** | **Addendum/steering text size** | both | ±~50 tok (ladders are *smaller* than the baseline control addendum) | **~0% — cost-neutral** |

### Reconciliations and cross-lane conflicts

- **Sub-driver attributions of driver 1 double-count.** Gross attributions (0.45 retry + 0.27 orient + 0.25 symbol = 0.97 msgs) exceed the net +0.71 because ccx rungs partly displace baseline rungs and the same run often both orients *and* retries. Tool-traffic claims removing retry turns "recovers the bulk" of the gap; turn-inflation attributes the gap to ladder rungs. **Both overclaim individually; jointly they cover the gap with overlap.** Treat retries and ladder rungs as co-equal (~40–60% each, gross) with a shared population of bad runs.
- **Per-turn cost constant: 38k vs 47k vs 48.6k — not a contradiction.** Adoption's ~38k T/turn divides by all turns (baseline 5.40); turn-inflation's 47.2k divides by billed API messages (baseline 3.98). Same phenomenon, different denominator. **Canonical figure: ~47k per billed message** (cache_read 46–51k measured directly).
- **Calls/run: 2.75 (tool-traffic) vs 3.17 (adoption) for baseline.** Different samples (450 all-runs w/ StructuredOutput excluded vs 260 headline runs) and call definitions. Deltas agree (+0.7–0.9 for cli); use tool-traffic's for per-call math, adoption's for adherence rates.
- **Guard-block counts: 31 (tool-traffic, grep-pipe blocks among 68 errors) vs 36 (adoption, all hard blocks incl. 1 ls).** Consistent within counting differences.
- **Batching hypothesis refuted (turn-inflation): baseline never batches (1.000 tools/msg).** Baseline's frugality is fewer sequential rungs, not parallelism. No lane contradicts this, but it kills a plausible prior for the fix design: the lever is *collapsing* the ladder into one billed message, not parallelizing.
- **ccx-mcp direction check:** ccx-mcp makes *fewer* calls than baseline (2.47 vs 2.75) — its inversion is 100% per-call size (3.9×), so drivers 1a–1c are CLI-only; driver 2 is MCP-only; driver 3 is the only cross-arm driver. Static lane confirms ccx-cli pays **+21 tok** fixed — zero schema tax on the CLI path.

---

## 2. Fix candidates per driver

### Driver 1 — extra billed messages (ccx-cli, ~33k tok/run)

| Fix | What | Effort | Est. savings | Pointers |
|---|---|---|---|---|
| **F2. Ladder rewrite: chaining as default, kill the orientation reflex** | Steering text must say: (a) for a known symbol go straight to one `ccx code symbol`/`ccx code grep` — never open with `ccx repo overview` on a targeted lookup; (b) chain ccx calls in ONE Bash command (`ccx a; ccx b; ccx c`) or compose via `ccx exec` — never one ccx per Bash. Evidence: `ccx exec` used **0/351** times; the 8.8% of calls that did shell-chain collapsed to one billed message (mechanism proven, just untriggered). | **prompt-only** | Up to the full ~33k/run cli gap if billed msgs return 4.69→3.98; worst 7-rung runs drop ~235k | Product side: cc-context plugin ccx skill / CLAUDE.md ladder (AGENTS.md `## Compact Context`); bench side: `ladder_cli.txt` addendum (`arms.py:271`) |
| **F4. Kill the retry tax: allow `ccx … \| grep` in the guard, or ship a first-class `--filter <pattern>`** | The CLI arm pipes ccx output through `\| grep`/`head` to trim it; the guard blocks raw grep on source → 13.1% error rate, 54/150 runs eat ≥1 retry turn. Whitelisting pipes *downstream of a ccx command* is config; `--filter` is a small code change that also shrinks the payload the model was trimming. | **config-only** (guard) / **code** (flag) | ~0.3–0.45 msgs/run ≈ **14–21k tok/run** on ccx-cli (gross; overlaps F2) | Guard: capt-hook ccx pack (external repo, wired per `capt-hook cc-context wiring`); flag: `internal/cli/` + `internal/render/` |

### Driver 2 — MCP schema tax (ccx-mcp, +5,316 tok/run)

| Fix | What | Effort | Est. savings | Pointers |
|---|---|---|---|---|
| **F3. Prune dead tools (Cut A)** | 9/18 MCP tools were called **0×** in 150 runs (`BashFormat, ccx_code_replace, ccx_exec, ccx_exec_tools, ccx_web_read, ccx_web_search, ccx_web_outline, ccx_code_related, ccx_code_deps`) = 48% of schema chars. Ship a lean default profile (code-nav) with web/exec/format/replace opt-in. | **config-only** (profile flag) / small **code** (profile plumbing) | **~2,530 tok/run** — tax +5,316 → ~+2,790 (−48%) | `internal/mcpserver/` tool registration; lane dump: scratchpad `mcp_tools.json` |
| **F5. Halve schema prose (Cut B)** | 55% of schema chars is NL prose; halving descriptions on the survivors (start: `ccx_code_grep` 1,111c inputSchema, `ccx_code_edit`, `ccx_code_search`) saves ~27% of remaining chars. Note: schema JSON tokenizes at ~2.5 chars/tok — denser than prose, so char cuts over-deliver in tokens. | **code** (description strings) | **~900–1,000 tok/run**, stackable on F3 → combined tax ~+2,000 (**−62%**) | `internal/mcpserver/` tool descriptions |

### Driver 3 — tool-result inflation (both arms)

| Fix | What | Effort | Est. savings | Pointers |
|---|---|---|---|---|
| **F1. Terse "locate" defaults for `ccx code symbol` and `ccx code outline`** | ~60% of this corpus is "where is X"; the symbol header line (`# grok: Command [src/click/core.py:1160]`) already answers it, yet the payload ships body+docstring+callers/callees (median 3,617c, 18.6× native grep). Default to signature + path:line; gate body/callers behind `--body`/`--full`. Outline: default to top-level depth (max seen: 12,253c whole-file dump), `--depth` opt-in. | **code** | **~75–90k tokens/campaign**; per locate call 4,478c → ~250c (**−94%**), erasing the inversion for locate-only tasks (~40% of corpus); also removes follow-up rungs (feeds F2) | `internal/grok/`, `internal/outline/`, `internal/render/` budget caps |
| F6. Trim `ccx code grep` default density | Workhorse op (245 calls), 3.7× native grep per hit block (718c vs 194c median). Tighten context lines / header verbosity toward ~250c. | **code** | ~470c × 245 calls ≈ ~35k chars/campaign direct + compounding | `internal/search/`, `internal/render/` |

**Accuracy caveat for F1/F6:** accuracy was *better* on ccx, and the one truly-native cheapest run (`lc-tornado-initialize` mcp r0, −42%) was likely wrong because it skipped verification the richer ccx payloads bought. Payload cuts must be gated on an accuracy re-run; a `--body` escape hatch keeps the depth available when the task is comprehension, not location.

### Driver 5 — addenda: no action. Cost-neutral (±50 tok); steering *content* matters (F2), size does not.

---

## 3. What this means for benchmark design

**Properties of THIS benchmark, not of ccx:**
- **Frugal, grep-first baseline on a familiar corpus.** click/tornado are in training data; sonnet knows roughly where things live, so one targeted `grep -n "def <name>"` (153–558 chars) answers most tasks. Orientation, semantic search, and rich symbol context add near-zero marginal value here. On an unfamiliar or private corpus the baseline's first grep misses more often and ccx's ladder amortizes better.
- **No flood conditions.** The baseline never dumped a huge file, so ccx's core value prop — token-bounded output with explicit overflow instead of a 50k-token Read — never got to fire (the "flood thesis" remains untested; native max tool result was 4,285 chars). A benchmark with 20KB+ files and deep-read tasks would exercise the failure mode ccx exists to prevent.
- **Short sessions (~4 turns) maximize the schema tax's share.** +5,316 fixed tokens is 11.5% of a 46k first-call context; in a 40-turn session with a 150k context it's noise on H and the compounding tool-result savings would dominate. The −13.9% H headline is partly a session-length artifact.
- **Locate-shaped tasks reward a locate-shaped tool.** ~60% of tasks need `path:line` only. The corpus measures ccx's worst case: paying comprehension-payload prices for location questions. (This is still a fair finding — F1 says the tool should price-discriminate — but the magnitude won't generalize to trace/edit/comprehension workloads.)
- **T-as-raw-token-sum overweights turns.** T counts cache_read tokens at par; at real cache-read pricing (~0.1×) each extra billed message costs ~4.7k effective, not 47k, and ccx-cli's cost gap is far smaller than its token gap. Report cost-weighted T alongside raw T in the redo.
- **Guard + ladder are bench conditions, not fixed ccx properties.** The 13.1% retry tax is the specific interaction of this guard config with this model's piping habit — tunable (F4) without touching ccx core.

**Properties of ccx that any benchmark would find:** the MCP schema tax (driver 2), the per-call payload ratios (driver 3), and the ladder's round-trip count (driver 1b/1c) are product facts, reproducible anywhere.

## 4. What would NOT be fixed

- **Any MCP surface pays schema tax.** ~45% of schema chars is irreducible structure (names, types, enums, braces); even after F3+F5 the floor is ~+2,000 tok/run for a lean 9-tool inventory. The only zero-tax path is the CLI (+21 tok measured). This is a protocol property, not a ccx defect — every MCP server on the market pays it, in the cached prefix, every turn.
- **Per-billed-message context re-read.** ~47k/message is harness + pricing architecture. ccx can reduce *how many* messages (F1/F2/F4) but can never make a round-trip free; any tool that adds one exploratory step loses ~47k raw T to a baseline that skips it.
- **Model preference is native.** Sonnet reaches for grep/find/sed/head by muscle memory; steering text alone achieved ~0 preference shift (38% of CLI runs typed native first; adoption was guard-manufactured). Enforcement is the adoption mechanism, and enforcement has a cost floor (rewrite ≈ free, block ≈ +1 turn). Short of model training or much stronger affordances, some compliance tax persists.
- **Parity is the ceiling on this task class.** A one-line answer costs baseline 153 chars; no token-bounded structured tool beats that. After all fixes, ccx on familiar-corpus locate tasks converges to baseline, not below it. The wins have to come from task classes this benchmark doesn't contain (floods, unfamiliar corpora, long sessions, comprehension).
- **The accuracy edge may be paid for in tokens.** ccx arms were more accurate; part of the payload "waste" is verification context the model actually used. Fully closing the token gap risks trading away the one headline ccx won.

---
*Lane artifacts: scratchpad `mcp_dump.py` / `mcp_tools.json` (schemas), `records.json` / `extract2.py` (tool traffic), `lib.py` / `seq.py` (adoption). Bench pointer: `arms.py:271` (addendum injection).*
