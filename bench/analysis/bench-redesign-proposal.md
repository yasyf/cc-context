# ccx benchmark redesign — proposal

**Status:** proposal for user review. Nothing here is implemented.
**Prompted by:** the verdict on the 450-run sonnet campaign (`results/20260711T202402Z`) — *"again, you have designed a shitty benchmark that does not reflect the value here."* The instrument passed every validity gate (450/450 within 2% of transcript accounting, isolation clean, paired stats sound). The design premise is what failed.

---

## 0. The one-sentence diagnosis

The campaign measured ccx against an **already-optimal baseline in the exact regime where ccx has nothing to save**: single-question tasks, ~4 turns, on repos deep in the model's training data, where modern sonnet greps precisely, reads bounded windows, and often answers from priors. ccx's value is *retained-context compounding across a long, unfamiliar, read-heavy session* — a regime this benchmark truncates by construction. The per-call savings are real (90% output-token reduction, microbench-confirmed); they just never reach the session ledger because the baseline never floods.

---

## 1. Post-mortem — the assumptions that failed

Each assumption below is baked into the current corpus/task design, stated with the measurement that refutes it. All numbers are from `results/20260711T202402Z/RESULTS.md` (sonnet, both-correct paired tasks).

| # | Design assumption | Measured refutation |
|---|---|---|
| A1 | A modern agent **floods context by default**, so bounded outputs save tokens. | Baseline mean tool-output **385–516 tokens/run**, **4.4 turns**, **4.2 tool calls**. Sonnet greps precisely and reads bounded windows *unprompted*. There is no flood to bound. |
| A2 | ccx's structural output is **smaller** than the naive equivalent for the same information. | **Inverted.** ccx tool-result tokens **1,277 (mcp) / 1,110 (cli) vs 385 baseline** — ~3× larger, p<0.001. `outline`/`overview`/`symbol` return def+callers+callees+signatures; the baseline used a 5-line grep window. The microbench's 90% saving is `outline` vs a *full-file Read the baseline never performs*. |
| A3 | `min_traversal_bytes ≥ 100 KB` guarantees the task is heavy enough that ccx overhead is a small fraction. | The **gold traversal** is ≥100 KB; the **agent never traverses it**. It snipes the answer with grep + one bounded read, touching a few KB. The floor gates the theoretical must-read set, not the bytes the baseline actually ingests (<1 KB of tool output). |
| A4 | **Peak context (H)** is a good proxy for ccx's value. | H is dominated by *fixed* overhead — system prompt, tool schemas, ladder addendum, the single largest read — not cumulative tool output. Loading the MCP's 44 tools vs 26 native adds **~6k/task**, which *is* the entire ccx-mcp H loss (−13.9%, **all 23 tasks lose**, dominant bucket `static_overhead` on every single one). Bounded outputs shave a few hundred tokens off a 47.5k baseline — below the fixed-overhead floor. |
| A5 | **Total tokens T** at fixed accuracy isolates efficiency. | T = Σ envelope usage ≈ turns × retained context (each turn re-bills ~40–50k cache-read). ccx-cli added ~1 turn (**5.3 vs 4.4**) → **T −22.4%** [−35.7, −9.0], swamping any per-call saving. T *rewards fewer turns*; ccx's "orient/search first" ladder tends to *add* a turn on trivial tasks. |
| A6 | **tornado + click** are a fair corpus. | Both are deep in training data. The model answers architectural questions from priors, minimizing traversal for *both* arms and shrinking the gap ccx could open. **8/450 runs never touched ccx at all** (integrity-excluded) — the model didn't need it. |
| A7 | **Single-question tasks** represent where ccx is deployed. | ccx's compounding value (every retained token re-billed each turn; destructive compaction at ~155–190k) only bites in **long multi-task sessions**. A 4-turn task never accumulates enough context to compound and never approaches compaction. The benchmark truncates exactly ccx's target regime. |
| A8 | The **flood thesis** (unaided baselines flood) is a given. | **Never tested.** This campaign measured *frugal* baselines. Whether an unprompted baseline floods on harder/unfamiliar/longer tasks is the untested premise — and measuring frugal baselines may itself be the finding that kills the old design. Stage 0 below attacks this first and cheapest. |

### The mechanism in one paragraph

ccx can only help when the baseline's *next action would have been a flood* — a full-file Read, a raw grep over a big tree, a bare `git diff`, a page-dump. On a single, precise, training-familiar question the baseline's next action is a 5-line grep window, so ccx's three fixed costs dominate: (1) **schema/prompt overhead** it can't earn back in 4 turns, (2) **structural output larger than a sniped grep**, (3) **an extra orient turn** whose cache-read re-bill exceeds the output it saved. The ladder addendum ("orient → search → outline first") is *tuned for the flood regime* and actively mis-serves the trivial regime the benchmark actually contained. Fix the regime and the sign flips; keep it and no honest tuning of ccx can win.

---

## 2. Candidate designs

Four designs, ordered cheapest-falsifier → definitive. Each isolates a distinct value mechanism. **Honesty rule enforced throughout:** the baseline always gets its best natural behavior (full native tools, frugal grep, priors); any design that only wins by hobbling the baseline is disqualified and is flagged as such under RISKS.

---

### Design A — Guard-value + real flood-rate audit *(no agent runs; the Stage-0 falsifier)*

**Value mechanism isolated:** the guard pack's rewrite savings **× the rate at which real baselines actually flood.** Directly attacks A8 — the untested premise — before spending a dollar on agent runs.

- **Corpus strategy:** real long-session transcripts (the source of the project's own "Read = 71.6% of token spend" figure — `cc-transcript` can enumerate them), plus a fixed suite of flood-prone command patterns (raw grep on a big tree, full-file Read, bare `git diff`, page-dump curl).
- **Task shape:** none — this is measurement, not agent behavior. Two numbers: (1) per-pattern output-token reduction under the guard rewrite (extends the existing microbench), (2) the **observed frequency** of each flood pattern in real unaided sessions.
- **Metrics:** `expected session saving = Σ (per-rewrite reduction × observed frequency)`. Plus the headline audit number: **what fraction of real-session Read/grep/diff calls are floods the guard would rewrite?**
- **Arms:** n/a (mechanical). Real-session baseline is the only "arm."
- **Expected cost:** ~free (transcript analysis + deterministic token counts). Hours, not dollars.
- **Power sketch:** deterministic; needs only enough real sessions (~30–50) to estimate flood frequency with a tight CI.
- **RISKS:** measures *potential*, not *realized*, value — it's the ceiling, not what an agent that has ccx actually ends up spending. Honest only if framed as "max the guards could save at observed flood rate R." **But**: if even the ceiling is small because real sessions are as frugal as the bench baseline, the whole thesis is falsified here for near-zero cost — which is why it goes first.

---

### Design B — Flood-inducing task shapes *(reuses the entire harness; the Stage-1 pilot)*

**Value mechanism isolated:** (a) baseline would ingest far more than needed + (d) large files — **defeat grep-sniping.** Tasks whose gold *provably* requires synthesizing content spread across many large files, so a single grep can't snipe the answer.

- **Corpus strategy:** the **existing** tornado + click checkouts (or gorilla-mux, reviving the dead Go machinery). No new corpus needed for the pilot.
- **Task shape:** answers spread wide with no single discriminating literal — e.g. *"every place that constructs an `X` and how each configures `Y`"* (X built in 12 files), *"which of these 30 handlers set header `Z` and under what condition"*, *"trace this value through the pipeline"* across 8 modules. A grep returns 50+ hits each needing a read; precise sniping fails; the agent must ingest structure. This is where `ccx code search`, `outline`, and the **`ccx exec` fan-out lane** should let ccx see structure without reading every file while the baseline greps-then-reads-N.
- **Metrics:** H, T, and — the direct falsifier of A2 — **tool-result tokens**. If the current *inverted* tool-result result (ccx 3× worse) **flips to favor ccx here**, the per-call mechanism is real and realizes under the right task shape.
- **Arms:** baseline vs ccx-cli (+ ccx-mcp optional). Keep all three if cost allows; the schema tax is a known −6k the flood must overcome.
- **Expected cost:** moderate — reuses arms/integrity/stats verbatim, swaps only task shapes. ~6–10 new tasks × 5 repeats × 2 arms × 1 model = 60–100 runs.
- **Power sketch:** same paired-bootstrap machinery; 6–10 tasks × 5 repeats gives the current campaign's ~9% median-noise floor.
- **RISKS:** grep may *still* win if the model greps a discriminating token and reads only `-A/-B` matches — so questions must have **semantic** spread, not a shared literal, or the task is disqualified (baseline gets grep; if grep snipes it, ccx legitimately shouldn't win). Push too far and you're testing *search quality* (semble) rather than *bounded output* — keep the answer mechanically gradable and the traversal genuinely wide.

---

### Design C — Session-length realism *(the headline redesign; Stage-2)*

**Value mechanism isolated:** (b) **retained-context compounding.** One agent session handles a **work order of 8–15 sequential tasks in the same repo with no context reset**, so bounded outputs keep the running context smaller, every subsequent turn re-bills less cache, and compaction is deferred or avoided. This is the *only* design that measures the thesis ccx was built for.

- **Corpus strategy:** ideally run on Design D's unfamiliar/obfuscated corpus (kills priors); acceptable on tornado/click for a first pass. The work order must have **breadth** (touch many parts of the repo, forcing genuinely new reads at each step) with enough continuity to be one plausible session.
- **Task shape:** a scripted 8–15-step "work order" — a realistic mix of nav/trace/large-context/edit steps a developer would do in one sitting — replayed identically for every arm. No reset between steps.
- **Metrics (capacity, not marginal %):**
  - **Session capacity** — at a fixed pre-compaction budget (~155k), **how many steps does each arm complete correctly?** (baseline compacts at step 8, ccx reaches step 13 → a *capability* difference, far more legible than a marginal token %).
  - **Compaction events** — count + token cost per session.
  - **Cumulative T** to complete the fixed work order; **H trajectory** (does baseline cross 155k while ccx stays under?).
  - **Time-to-Nth-step** / tasks-before-first-compaction.
- **Arms:** baseline vs ccx-cli vs ccx-mcp. **This is the first design where ccx-mcp need not auto-lose** — the fixed +6k schema tax amortizes to ~1% over 15 tasks and 500k+ T, and MCP tool-result savings can dominate. Genuinely interesting question, worth the third arm here.
- **Expected cost:** high per run (long sessions, 500k–1.5M T each) but **far fewer runs** — each session is one dense, informative unit. ~8–12 work orders × 3–5 repeats × 2–3 arms ≈ 48–108 long sessions. Fewer than 450, more informative each.
- **Power sketch:** each session = one paired unit, so power comes from #work-orders × repeats, not #questions. 8–12 work orders × 3–5 repeats supports a paired bootstrap on session-T and a sign test on compaction-count. Size work orders to push a *baseline* toward compaction, or the capacity metric has no headroom to show.
- **RISKS:** (1) if steps are too *related*, the agent answers step 5 from context already loaded at step 2 — no new read, ccx's read-time saving vanishes; work orders need breadth. (2) If the harness or the agent proactively compacts/resets, the effect dies — must run one continuous session and log compaction rather than trigger it. (3) Long sessions are stochastic; per-session variance is higher than per-question — repeats matter more. (4) Baseline must keep frugal per-call behavior; ccx wins only via *accumulation*, never by making the baseline read more.

---

### Design D — Prior-defeating corpus *(a modifier on B/C; can also stand alone)*

**Value mechanism isolated:** (c) **unfamiliar corpus** — force real traversal by removing the priors the model currently substitutes for reading (A6).

- **Corpus strategy, cleanest first:**
  - **Obfuscated forks** (recommended): rename every symbol/module in a real repo (e.g. tornado) so structure — and thus gold derivability and baseline accuracy — is preserved, but priors can't map. Deterministic, reversible, gold re-derives from the AST exactly as today.
  - **Post-cutoff repos:** real repos created after the model's knowledge cutoff. Authentic, but gold must be authored fresh and quality varies.
  - **Machine-generated codebases:** synthetic large modules with novel names. Fully controllable size, but risk being *too uniform* — easy to grep, defeating the purpose.
- **Task shape:** the same families, now requiring real reads because priors can't shortcut.
- **Metrics:** H/T/tool-result as today, but now with real traversal happening in *both* arms.
- **Arms:** baseline vs ccx-cli (+ mcp).
- **Expected cost:** moderate (obfuscation is a build-time transform; reuses everything downstream).
- **RISKS — the central honesty risk:** if the corpus is too alien, **baseline accuracy collapses and you've built an accuracy benchmark, not a token benchmark.** Mandatory gate: **baseline accuracy must stay ≥ ~85%** so the token comparison is apples-to-apples on both-correct tasks. Obfuscated forks are safest (structure and difficulty preserved); generated code is riskiest (uniformity). Report baseline accuracy as a first-class result, not a footnote.

---

### (Cross-cutting axis) — weaker-model agent

Not a standalone corpus but an **axis to fold into B/C/D:** run the same tasks with a weaker agent (haiku, or an older/smaller model). Thesis: **ccx's value is largest for models with worse tool discipline** — a weak model floods where sonnet is frugal. If ccx flattens a weak model's flooding, that's an honest, deployable claim: *"use ccx when your agent has poor tool discipline."* Expect savings to *grow as capability drops*. Cheap (weak models = cheap runs). **Risk:** weak-model accuracy may fall below the pairing threshold (same accuracy-benchmark trap) — gate on pairable accuracy. Best run as a third model axis in whichever design ships, not as its own campaign.

---

## 3. Recommendation — staged path, cheapest falsifier first

The thesis has one load-bearing, untested premise (A8: *do real baselines flood?*). Spend the least money to falsify it before scaling.

1. **Stage 0 — Design A (guard-value + flood-rate audit).** ~Free. Measure the real-session flood rate and the guard-rewrite ceiling. **Kill switch:** if real sessions are as frugal as the bench baseline (low flood rate, small ceiling), the thesis needs rethinking *before* any campaign — surface that as the finding rather than spending on Stage 2.
2. **Stage 1 — Design B (flood-inducing tasks) on existing repos, small N.** Cheap; reuses the whole harness. **Go/no-go:** does the inverted tool-result result flip to favor ccx when the answer spreads across many files? If yes, the per-call mechanism realizes under the right task shape and is worth scaling. If no, the mechanism doesn't survive contact with a frugal baseline even under load — a real finding.
3. **Stage 2 — Design C (session-length) on Design D's obfuscated corpus, with the weak-model axis.** The expensive, definitive campaign — run *only* if Stages 0–1 show the mechanism alive. Headline metric: **session capacity** (steps completed before compaction) and **compaction count**, not marginal token %.

Rationale: Stages 0–1 cost a rounding error against Stage 2 and can each independently kill or green-light the thesis. Stage 2 is where the honest, legible claim lives ("ccx lets this agent do 60% more work before it compacts") — but it's only worth its cost once the mechanism is confirmed to exist.

---

## 4. What the current corpus / harness can be reused for

The **harness is solid and should be preserved wholesale** — the corpus and task shapes are what failed. Directly reusable:

- **Arm isolation** (`arms.py`): fresh config dir, ambient MCP/plugins/settings stripped, PATH curation excluding `.venv`/`/usr/local/bin`/stray shims, the `command not found` shim that surfaces stray-ccx leaks, the live `guards_available` probe, length-matched addenda (±15%). Keep verbatim across all designs.
- **Integrity gating** (`integrity.py`): the ccx-actually-used detector (`CCX_BASH` command-position regex + `mcp__cc-context__` prefix), heavy-native-primitive detection, guard-fire detection, and the **answer-key contamination** check. This is what excluded the 8 mislabeled runs — essential for every design and *more* important in long sessions where a single stray flood can invalidate a session.
- **Paired statistics** (`report.py`): median-across-repeats, bootstrap CI, exact sign test, both-correct pairing. Transfers to per-session units in Design C unchanged.
- **Token accounting**: envelope-vs-transcript reconciliation (450/450 within 2%) and the `trajectory.py` 5-bucket decomposition (`static_overhead / tool_result / history / hook_error / residual`) — the decomposition is *exactly* the tool that will show the sign flip in Designs B/C.
- **Gold derivation** (`goldgen.py`): deterministic AST/regex recompute that fails loud on drift — extends cleanly to obfuscated forks (Design D re-derives gold from the renamed AST) and to Go (the dead `go_funcs`/`go_callers`/`go_iface` machinery, currently unused because *no Go task exists*, can be revived by Design B/D).
- **Microbench** (`microbench.py`): the per-call harness is the seed of Design A's guard-value calc.

What to **retire or rebuild:** the 26-task single-question corpus over 2 familiar Python repos (`taskgen.py`), the `min_traversal_bytes` floor as a *sufficiency* proxy (it gates the theoretical set, not ingested bytes — replace with a metric on bytes the baseline actually reads), and H as a headline metric for short tasks (it's fixed-overhead-dominated — demote to a co-metric; lead with session capacity / compaction / cumulative T).

---

## Appendix — key figures cited (sonnet campaign, `20260711T202402Z`)

| Quantity | baseline | ccx-mcp | ccx-cli |
|---|---|---|---|
| Accuracy | 91.5% | **94.5%** | **95.2%** |
| Mean H (peak context) | 47,770 | 54,199 | 49,025 |
| Mean T (total tokens) | 208,399 | 213,595 | 252,915 |
| Mean turns | 4.4 | 4.1 | 5.3 |
| Mean tool calls | 4.2 | 3.9 | 5.1 |
| Mean tool-output tokens | 516 | 1,350 | 1,294 |
| n_tools (schema surface) | 26 | 44 | 26 |

Paired headlines: ccx-mcp H **−13.9%** [−15.0, −12.6] (all 23 tasks lose, dominant bucket `static_overhead`); ccx-cli T **−22.4%** [−35.7, −9.0]; tool-result tokens **−480%/−479%** (ccx ~3× baseline, p<0.001). Accuracy favors ccx — **accuracy is not the problem.** Microbench: **90% per-call output-token saving, real** — off the baseline's actual path. 8/450 runs integrity-excluded for never invoking ccx.
