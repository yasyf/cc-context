"""Aggregate run records into RESULTS.md: the token-usage savings story.

Per (model x ccx-arm) two paired headlines gate the claim, each ccx-arm against baseline:

  * **Peak context** `H = max over turns of (input + cache_create + cache_read)` — how big
    the context window actually got, straight from transcript usage (never tiktoken).
  * **Total tokens processed** `T = Σ per API call (input + cache_create + cache_read) + Σ output`
    — from the API-reported envelope usage in `runs.jsonl`, cross-checked against the
    transcript recompute within 2%.

Each metric is the **median across repeats** per (task, arm), paired over tasks where both
arms answered correctly, with a bootstrap CI, win/loss/tie, and an exact sign-test p. A ccx
arm PASSes only when its accuracy holds and both CIs exclude zero in ccx's favor. Cost is not
a metric — it never appears. tiktoken appears only in the attribution waterfall, where the
arm-vs-arm ratios cancel its systematic error.
"""

from __future__ import annotations

import json
import math
import random
import statistics
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

from . import tokens, trajectory
from .runner import corpus_sha
from .types import ARMS, DECOMP_TERMS, TrajectoryMetrics

BOOTSTRAP_N = 2000
BOOTSTRAP_SEED = 0
CONSISTENCY_TOL = 0.02
CONTROL_CATEGORY = "non_regression"

BASELINE = "baseline"
CCX_ARMS: tuple[str, ...] = tuple(a for a in ARMS if a != BASELINE)

CAUSE_BY_TERM = {
    "static_overhead": "fixed MCP-schema/prompt overhead on a small task",
    "history": "more or longer turns",
    "tool_result": "a ccx call returned more than the raw equivalent",
    "hook_error": "failed tool calls / guard errors",
    "residual": "tokenizer/accounting residual",
}


@dataclass
class Cell:
    corrects: list[bool] = field(default_factory=list)


def load(jsonl_path: Path) -> list[dict]:
    records = []
    for line in jsonl_path.read_text().splitlines():
        if not line.strip():
            continue
        rec = json.loads(line)
        if "halted" in rec:
            continue
        records.append(rec)
    return records


def by_task(records: list[dict], model: str, arm: str) -> dict[str, Cell]:
    cells: dict[str, Cell] = defaultdict(Cell)
    for r in records:
        if r["model"] != model or r["arm"] != arm:
            continue
        cells[r["task_id"]].corrects.append(bool(r["correct"]))
    return cells


def paired_task_ids(records: list[dict], model: str, ccx_arm: str) -> tuple[list[str], int]:
    """Task ids with >=1 record in BOTH baseline and `ccx_arm` for this model, plus the count dropped."""
    per_arm: dict[str, set[str]] = {BASELINE: set(), ccx_arm: set()}
    for r in records:
        if r.get("model") == model and r.get("arm") in per_arm:
            per_arm[r["arm"]].add(r["task_id"])
    both = per_arm[BASELINE] & per_arm[ccx_arm]
    either = per_arm[BASELINE] | per_arm[ccx_arm]
    return sorted(both), len(either - both)


def percentile_ci(values: list[float]) -> tuple[float, float]:
    if not values:
        return (float("nan"), float("nan"))
    s = sorted(values)
    lo = s[int(0.025 * (len(s) - 1))]
    hi = s[int(0.975 * (len(s) - 1))]
    return (lo, hi)


def bootstrap_ci(savings: list[float]) -> tuple[float, float]:
    """Paired bootstrap CI (2.5/97.5 pct) on the mean per-task savings."""
    n = len(savings)
    if n == 0:
        return (float("nan"), float("nan"))
    rng = random.Random(BOOTSTRAP_SEED)
    means: list[float] = []
    for _ in range(BOOTSTRAP_N):
        means.append(sum(savings[rng.randrange(n)] for _ in range(n)) / n)
    return percentile_ci(means)


def sign_test_p(wins: int, losses: int) -> float:
    """Exact two-sided paired sign-test p-value (ties excluded), computed by hand."""
    n = wins + losses
    if n == 0:
        return 1.0
    k = min(wins, losses)
    tail = sum(math.comb(n, i) for i in range(k + 1)) * 0.5**n
    return min(1.0, 2.0 * tail)


def _run_id(task_id: str, arm: str, model: str, repeat: int) -> str:
    return f"{task_id}__{arm}__{model}__r{repeat}"


def _envelope_tokens(rec: dict) -> int:
    """Total tokens processed for one run, from its API-reported envelope usage."""
    u = rec["usage"]
    return u["input"] + u["cache_read"] + u["cache_create_5m"] + u["cache_create_1h"] + u["output"]


def _metrics(path: Path, prompt: str, counter: tokens.TokenCounter) -> TrajectoryMetrics:
    """Trajectory metrics for a saved transcript."""
    m = trajectory.from_file(path, first_prompt=prompt, count=counter.count)
    if m is None:
        raise ValueError(f"{path}: transcript has no model turn with prompt tokens")
    return m


@dataclass
class ArmAgg:
    """Per-(task, arm) aggregate across repeats: transcript metrics + envelope T per repeat.

    `median_h`/`median_t` are the headline metrics (median across repeats, per metric
    independently). `representative` is the repeat whose high-water is the middle one — a real
    transcript whose decomposition sums to `median_h` (exact for the odd repeat counts we run).
    """

    metrics: list[TrajectoryMetrics]
    envelope_t: list[int]

    @property
    def median_h(self) -> float:
        return statistics.median(m.high_water for m in self.metrics)

    @property
    def median_t(self) -> float:
        return statistics.median(self.envelope_t)

    @property
    def representative(self) -> TrajectoryMetrics:
        ordered = sorted(self.metrics, key=lambda m: m.high_water)
        return ordered[len(ordered) // 2]


@dataclass
class TaskPair:
    task_id: str
    base: ArmAgg
    ccx: ArmAgg


@dataclass
class Headline:
    label: str
    unit: str
    n: int
    mean: float
    lo: float
    hi: float
    base_mean: float
    ccx_mean: float
    wins: int
    losses: int
    ties: int
    p: float

    @property
    def ci_excludes_zero(self) -> bool:
        return self.n > 0 and self.lo > 0


def _arm_agg(
    records: list[dict],
    model: str,
    arm: str,
    task_id: str,
    *,
    raw_dir: Path,
    prompt: str,
    counter: tokens.TokenCounter,
) -> ArmAgg | None:
    recs = [r for r in records if r["model"] == model and r["arm"] == arm and r["task_id"] == task_id]
    if not recs:
        return None
    envelope_t = [_envelope_tokens(r) for r in recs]
    metrics = [
        _metrics(raw_dir / f"{_run_id(task_id, arm, model, r['repeat'])}.json", prompt, counter)
        for r in recs
    ]
    return ArmAgg(metrics=metrics, envelope_t=envelope_t)


def build_pairs(
    records: list[dict],
    model: str,
    ccx_arm: str,
    paired: list[str],
    both_correct: set[str],
    *,
    raw_dir: Path,
    prompts: dict[str, str],
    counter: tokens.TokenCounter,
) -> list[TaskPair]:
    pairs: list[TaskPair] = []
    for tid in paired:
        if tid not in both_correct:
            continue
        prompt = prompts.get(tid, "")
        base = _arm_agg(records, model, BASELINE, tid, raw_dir=raw_dir, prompt=prompt, counter=counter)
        ccx = _arm_agg(records, model, ccx_arm, tid, raw_dir=raw_dir, prompt=prompt, counter=counter)
        if base is None or ccx is None or base.median_h == 0 or base.median_t == 0:
            continue
        pairs.append(TaskPair(tid, base, ccx))
    return pairs


def both_correct_tasks(cells: dict[str, dict[str, Cell]], paired: list[str], ccx_arm: str) -> set[str]:
    """Task ids where every record in BOTH baseline and `ccx_arm` graded correct."""
    out: set[str] = set()
    for tid in paired:
        b = cells[BASELINE].get(tid)
        c = cells[ccx_arm].get(tid)
        if b and c and all(b.corrects) and all(c.corrects):
            out.add(tid)
    return out


def _regressions(cells: dict[str, dict[str, Cell]], paired: list[str], ccx_arm: str) -> tuple[list[str], list[str]]:
    """Paired tasks where baseline held but `ccx_arm` broke (regression), and the reverse."""
    reg: list[str] = []
    imp: list[str] = []
    for tid in paired:
        b = cells[BASELINE].get(tid)
        c = cells[ccx_arm].get(tid)
        if not b or not c:
            continue
        b_ok = all(b.corrects)
        c_ok = all(c.corrects)
        if b_ok and not c_ok:
            reg.append(tid)
        elif c_ok and not b_ok:
            imp.append(tid)
    return reg, imp


def _accuracy(records: list[dict], model: str, arm: str) -> tuple[int, int]:
    recs = [r for r in records if r["model"] == model and r["arm"] == arm]
    return sum(1 for r in recs if r["correct"]), len(recs)


def _headline(pairs: list[TaskPair], kind: str) -> Headline:
    if kind == "h":
        label = "Peak context (H = max single-turn input + cache_create + cache_read)"
        unit = "H"
        base_vals = [p.base.median_h for p in pairs]
        ccx_vals = [p.ccx.median_h for p in pairs]
    else:
        label = "Total tokens processed (T = Σ envelope usage)"
        unit = "T"
        base_vals = [p.base.median_t for p in pairs]
        ccx_vals = [p.ccx.median_t for p in pairs]

    n = len(pairs)
    if n == 0:
        nan = float("nan")
        return Headline(label, unit, 0, nan, nan, nan, nan, nan, 0, 0, 0, 1.0)

    savings = [1.0 - c / b for b, c in zip(base_vals, ccx_vals, strict=True)]
    mean = sum(savings) / n
    lo, hi = bootstrap_ci(savings)
    wins = sum(1 for b, c in zip(base_vals, ccx_vals, strict=True) if c < b)
    losses = sum(1 for b, c in zip(base_vals, ccx_vals, strict=True) if c > b)
    ties = sum(1 for b, c in zip(base_vals, ccx_vals, strict=True) if c == b)
    p = sign_test_p(wins, losses)
    return Headline(
        label,
        unit,
        n,
        mean,
        lo,
        hi,
        sum(base_vals) / n,
        sum(ccx_vals) / n,
        wins,
        losses,
        ties,
        p,
    )


def headline_section(hl: Headline, ccx_arm: str) -> list[str]:
    lines = [f"#### {hl.label}", ""]
    if hl.n == 0:
        lines.append("- no both-correct task pairs with parseable transcripts — nothing to compare")
        lines.append("")
        return lines
    lines.append(f"- Paired on **{hl.n} both-correct task(s)**")
    lines.append(
        f"- Mean savings: **{hl.mean * 100:+.1f}%** "
        f"(95% CI [{hl.lo * 100:+.1f}%, {hl.hi * 100:+.1f}%]) — positive = ccx processed fewer tokens"
    )
    lines.append(f"- Mean {hl.unit}: baseline **{hl.base_mean:,.0f}** → {ccx_arm} **{hl.ccx_mean:,.0f}** tokens")
    lines.append(f"- Win/loss/tie ({ccx_arm} </>/== baseline): **{hl.wins} / {hl.losses} / {hl.ties}**")
    lines.append(f"- Exact paired sign-test p-value: **{hl.p:.4f}**")
    lines.append("")
    return lines


def verdict_section(
    model: str,
    ccx_arm: str,
    headline_records: list[dict],
    cells: dict[str, dict[str, Cell]],
    paired: list[str],
    hl_h: Headline,
    hl_t: Headline,
) -> list[str]:
    c_base, n_base = _accuracy(headline_records, model, BASELINE)
    c_arm, n_arm = _accuracy(headline_records, model, ccx_arm)
    acc_base = c_base / n_base if n_base else 0.0
    acc_arm = c_arm / n_arm if n_arm else 0.0
    reg, imp = _regressions(cells, paired, ccx_arm)

    reasons: list[str] = []
    if acc_arm < acc_base:
        reasons.append(f"accuracy {acc_arm:.1%} < baseline {acc_base:.1%}")
    if reg:
        reasons.append(f"per-task regressions: {', '.join(reg)}")
    if not hl_h.ci_excludes_zero:
        reasons.append("peak-context CI includes 0")
    if not hl_t.ci_excludes_zero:
        reasons.append("total-tokens CI includes 0")
    passed = not reasons

    lines = ["#### Verdict", ""]
    lines.append(f"- **{'PASS' if passed else 'FAIL'}** — {ccx_arm} vs baseline")
    lines.append(f"- accuracy: {ccx_arm} **{acc_arm:.1%}** ({c_arm}/{n_arm}) vs baseline **{acc_base:.1%}** ({c_base}/{n_base})")
    if reg:
        lines.append(f"- ⚠️ regressions: {', '.join(f'`{t}`' for t in reg)}")
    if imp:
        lines.append(f"- improvements: {', '.join(f'`{t}`' for t in imp)}")
    if not passed:
        lines.append(f"- FAIL reasons: {'; '.join(reasons)}")
    lines.append("")
    return lines


def _decomp_delta(pair: TaskPair) -> dict[str, int]:
    b = pair.base.representative.decomposition
    c = pair.ccx.representative.decomposition
    return {term: getattr(c, term) - getattr(b, term) for term in DECOMP_TERMS}


def _dominant_delta_bucket(delta: dict[str, int]) -> str:
    return max(delta, key=lambda k: abs(delta[k]))


def waterfall_section(pairs: list[TaskPair], ccx_arm: str) -> list[str]:
    lines = [f"#### Per-task waterfall ({ccx_arm})", ""]
    if not pairs:
        lines.append("- no both-correct pairs to chart")
        lines.append("")
        return lines
    lines.append("| Task | H_base | H_arm | H savings% | T savings% | dominant Δ-bucket |")
    lines.append("|---|---|---|---|---|---|")
    for p in sorted(pairs, key=lambda x: 1.0 - x.ccx.median_h / x.base.median_h, reverse=True):
        h_sav = 1.0 - p.ccx.median_h / p.base.median_h
        t_sav = 1.0 - p.ccx.median_t / p.base.median_t
        bucket = _dominant_delta_bucket(_decomp_delta(p))
        lines.append(
            f"| `{p.task_id}` | {p.base.median_h:,.0f} | {p.ccx.median_h:,.0f} "
            f"| {h_sav * 100:+.1f}% | {t_sav * 100:+.1f}% | {bucket} |"
        )
    lines.append("")
    return lines


def diagnosis_section(pairs: list[TaskPair], ccx_arm: str) -> list[str]:
    lines = [f"#### Auto-diagnosis ({ccx_arm} grew the context)", ""]
    regressions = [p for p in pairs if p.ccx.median_h > p.base.median_h]
    if not regressions:
        lines.append(f"- none — {ccx_arm} held or shrank the peak on every both-correct task")
        lines.append("")
        return lines
    for p in regressions:
        delta = _decomp_delta(p)
        term = max(delta, key=lambda k: delta[k])
        grew = p.ccx.median_h - p.base.median_h
        lines.append(f"- `{p.task_id}`: +{grew:,.0f} tokens, dominant Δ `{term}` → {CAUSE_BY_TERM[term]}")
    lines.append("")
    return lines


def arm_summary_section(
    records: list[dict],
    model: str,
    *,
    raw_dir: Path,
    prompts: dict[str, str],
    counter: tokens.TokenCounter,
) -> list[str]:
    """Per-arm means over every ok headline run: accuracy, H, T, turns, tool-calls, tool-output."""
    lines = ["### Arm summary (headline tasks, all ok runs)", ""]
    lines.append("| Arm | N | acc | mean H | mean T | mean turns | mean tool-calls | mean tool-output |")
    lines.append("|---|---|---|---|---|---|---|---|")
    for arm in ARMS:
        recs = [r for r in records if r["model"] == model and r["arm"] == arm]
        n = len(recs)
        if not n:
            lines.append(f"| {arm} | 0 | — | — | — | — | — | — |")
            continue
        acc = sum(1 for r in recs if r["correct"]) / n * 100.0
        env_t = [_envelope_tokens(r) for r in recs if "usage" in r]
        mean_t = f"{sum(env_t) / len(env_t):,.0f}" if env_t else "—"
        metrics = [
            _metrics(
                raw_dir / f"{_run_id(r['task_id'], arm, model, r['repeat'])}.json",
                prompts.get(r["task_id"], ""),
                counter,
            )
            for r in recs
            if "usage" in r
        ]
        if metrics:
            k = len(metrics)
            mean_h = f"{sum(m.high_water for m in metrics) / k:,.0f}"
            mean_turns = f"{sum(m.turn_count for m in metrics) / k:.1f}"
            mean_calls = f"{sum(m.tool_call_count for m in metrics) / k:.1f}"
            mean_out = f"{sum(m.cumulative_tool_output for m in metrics) / k:,.0f}"
        else:
            mean_h = mean_turns = mean_calls = mean_out = "—"
        lines.append(f"| {arm} | {n} | {acc:.1f}% | {mean_h} | {mean_t} | {mean_turns} | {mean_calls} | {mean_out} |")
    lines.append("")
    return lines


def isolation_panel(records: list[dict], model: str, meta: dict | None) -> list[str]:
    """Prove only the ccx surface differs: per-arm MCP servers + tool count, from each run's init.

    ccx-cli must show zero MCP servers and a tool count equal to baseline; ccx-mcp exactly the
    cc-context server. Divergences are rendered as BREACH so the structural claim is checked,
    not assumed.
    """
    lines = ["### Isolation panel", ""]

    def identities(arm: str) -> set[tuple[tuple[str, ...], int]]:
        out: set[tuple[tuple[str, ...], int]] = set()
        for r in records:
            if r["model"] != model or r["arm"] != arm or "init" not in r:
                continue
            init = r["init"]
            out.add((tuple(sorted(init["mcp_servers"])), int(init["n_tools"])))
        return out

    base_ids = identities(BASELINE)
    base_tools = next(iter(base_ids))[1] if len(base_ids) == 1 else None

    lines.append("| Arm | MCP servers | n_tools | verdict |")
    lines.append("|---|---|---|---|")
    for arm in ARMS:
        ids = identities(arm)
        if not ids:
            lines.append(f"| {arm} | — | — | no runs |")
            continue
        if len(ids) > 1:
            lines.append(f"| {arm} | (divergent across runs) | — | ⚠️ BREACH: {len(ids)} distinct init identities |")
            continue
        servers, n_tools = next(iter(ids))
        servers_str = ", ".join(servers) if servers else "none"
        verdict = _isolation_verdict(arm, servers, n_tools, base_tools)
        lines.append(f"| {arm} | {servers_str} | {n_tools} | {verdict} |")
    lines.append("")

    if meta is not None:
        fp = meta.get("env_fingerprint", [])
        lines.append(f"- Env fingerprint (shared across arms): `{', '.join(fp) if fp else 'none'}`")
    plugin_sets = {tuple(r["init"]["plugins"]) for r in records if r.get("model") == model and "init" in r}
    lines.append(f"- Distinct ambient plugin sets: **{len(plugin_sets)}** (1 = symmetric across arms)")
    lines.append("")
    return lines


def _isolation_verdict(arm: str, servers: tuple[str, ...], n_tools: int, base_tools: int | None) -> str:
    if arm == BASELINE:
        return "⚠️ BREACH: cc-context MCP present" if "cc-context" in servers else "control (native tools)"
    if arm == "ccx-cli":
        if servers:
            return f"⚠️ BREACH: MCP servers present ({', '.join(servers)})"
        if base_tools is not None and n_tools != base_tools:
            return f"⚠️ BREACH: n_tools {n_tools} != baseline {base_tools}"
        return "OK: zero MCP, tool surface == baseline"
    if arm == "ccx-mcp":
        return "OK: exactly cc-context" if servers == ("cc-context",) else f"⚠️ BREACH: MCP servers {list(servers)} != [cc-context]"
    return f"⚠️ unknown arm {arm}"


def consistency_section(
    records: list[dict],
    *,
    raw_dir: Path,
    prompts: dict[str, str],
    counter: tokens.TokenCounter,
) -> list[str]:
    """Count runs whose transcript-recomputed T is within 2% of the envelope T; list outliers."""
    lines = ["### Envelope vs transcript token accounting", ""]
    within = 0
    total = 0
    outliers: list[tuple[str, str, str, int, int, float]] = []
    for r in records:
        if "usage" not in r:
            continue
        m = _metrics(
            raw_dir / f"{_run_id(r['task_id'], r['arm'], r['model'], r['repeat'])}.json",
            prompts.get(r["task_id"], ""),
            counter,
        )
        env_t = _envelope_tokens(r)
        trans_t = m.total_tokens
        if env_t == 0:
            continue
        rel = abs(env_t - trans_t) / env_t
        total += 1
        if rel <= CONSISTENCY_TOL:
            within += 1
        else:
            outliers.append((r["task_id"], r["arm"], r["model"], env_t, trans_t, rel))

    lines.append(f"- Runs within {CONSISTENCY_TOL:.0%}: **{within} / {total}** (envelope T vs transcript T)")
    for tid, arm, model, env_t, trans_t, rel in sorted(outliers, key=lambda o: o[5], reverse=True)[:10]:
        lines.append(f"  - `{tid}` [{arm}] {model}: envelope {env_t:,} vs transcript {trans_t:,} ({rel * 100:.1f}% off)")
    lines.append("")
    return lines


def control_panel(records: list[dict], model: str) -> list[str]:
    """non_regression family — excluded from every headline, rendered as an accuracy-only control."""
    lines = [f"### Control panel — {CONTROL_CATEGORY} (excluded from headlines, accuracy only)", ""]
    ctl = [r for r in records if r["model"] == model and r["category"] == CONTROL_CATEGORY]
    if not ctl:
        lines.append(f"- no {CONTROL_CATEGORY} tasks in this session")
        lines.append("")
        return lines
    for arm in ARMS:
        recs = [r for r in ctl if r["arm"] == arm]
        n = len(recs)
        correct = sum(1 for r in recs if r["correct"])
        acc = (correct / n * 100.0) if n else 0.0
        lines.append(f"- {arm}: **{acc:.1f}%** ({correct}/{n})")
    lines.append("")
    return lines


def corpus_drift_line(meta: dict) -> str:
    recorded = meta.get("corpus_sha")
    current = corpus_sha()
    if recorded == current:
        return f"Corpus SHA matches build: `{current[:12]}`."
    shown = recorded[:12] if recorded else "—"
    return f"⚠️ **CORPUS DRIFT**: meta `{shown}` != recompute `{current[:12]}` — `bench/tasks/` changed since the run."


def integrity_section(records: list[dict]) -> str:
    bad = [r for r in records if not r.get("integrity", {}).get("ok", True)]
    out = ["## Integrity & confounds", ""]
    out.append(f"- Runs excluded for mislabeling/cheat: **{len(bad)}** / {len(records)}")
    for arm in ARMS:
        out.append(f"  - {arm}: {sum(1 for r in bad if r.get('arm') == arm)}")
    for r in bad[:10]:
        out.append(f"  - `{r['task_id']}` [{r['arm']}] {r.get('model', '')}: {r.get('integrity', {}).get('note', '')}")
    for arm in CCX_ARMS:
        runs = [r for r in records if r.get("arm") == arm]
        if not runs:
            continue
        on = sum(1 for r in runs if r.get("guards_active"))
        if on == 0:
            out.append(f"- ⚠️ capt-hook guards INACTIVE for {arm} (pack failed to load).")
        else:
            out.append(f"- capt-hook guards active on {on}/{len(runs)} {arm} runs.")
    plugin_sets = {tuple(r.get("init", {}).get("plugins", [])) for r in records if "init" in r}
    out.append(f"- Distinct ambient plugin sets across runs: **{len(plugin_sets)}** (1 = symmetric across arms)")
    return "\n".join(out)


def render(
    records: list[dict],
    session_id: str,
    *,
    raw_dir: Path | None = None,
    prompts: dict[str, str] | None = None,
    counter: tokens.TokenCounter | None = None,
    meta: dict | None = None,
) -> str:
    """Render RESULTS.md from run records.

    Trajectory sections need each run's saved transcript (`raw_dir/<run_id>.json`), the task
    prompts (`prompts[task_id]`), and a token `counter`. `meta` (the session's `meta.json`)
    supplies the env fingerprint and the corpus SHA drift check.
    """
    prompts = prompts or {}
    models = sorted({r["model"] for r in records})
    if raw_dir is None:
        raise ValueError("raw_dir is required")
    if not raw_dir.exists():
        raise FileNotFoundError(raw_dir)
    if counter is None:
        counter = tokens.default_counter()

    lines: list[str] = ["# cc-context benchmark results", ""]
    lines.append(f"Session: `{session_id}` · {len(records)} runs · token-usage savings, paired per task, gated on accuracy.")
    lines.append("")
    lines.append(
        "Headlines per model x ccx arm: **peak context** `H = max single-turn (input + cache_create + "
        "cache_read)` and **total tokens** `T = Σ envelope usage`. Each is the median across repeats "
        "per task, paired over both-correct tasks. Positive savings = ccx processed fewer tokens. Cost "
        "is not a metric."
    )
    lines.append("")
    if meta is not None:
        lines.append(corpus_drift_line(meta))
        lines.append("")
    for model in models:
        lines.append(f"## Model: {model}")
        lines.append("")

        ok_records = [r for r in records if r.get("integrity", {}).get("ok", True)]
        headline_records = [r for r in ok_records if r["category"] != CONTROL_CATEGORY]
        hcells = {arm: by_task(headline_records, model, arm) for arm in ARMS}

        for ccx_arm in CCX_ARMS:
            lines.append(f"### {ccx_arm} vs baseline")
            lines.append("")
            paired, _dropped = paired_task_ids(headline_records, model, ccx_arm)
            both_ok = both_correct_tasks(hcells, paired, ccx_arm)
            pairs = build_pairs(
                headline_records, model, ccx_arm, paired, both_ok, raw_dir=raw_dir, prompts=prompts, counter=counter
            )
            hl_h = _headline(pairs, "h")
            hl_t = _headline(pairs, "t")
            lines += headline_section(hl_h, ccx_arm)
            lines += headline_section(hl_t, ccx_arm)
            lines += verdict_section(model, ccx_arm, headline_records, hcells, paired, hl_h, hl_t)
            lines += waterfall_section(pairs, ccx_arm)
            lines += diagnosis_section(pairs, ccx_arm)

        lines += arm_summary_section(headline_records, model, raw_dir=raw_dir, prompts=prompts, counter=counter)
        lines += isolation_panel(records, model, meta)
        lines += control_panel(records, model)

    lines += consistency_section(records, raw_dir=raw_dir, prompts=prompts, counter=counter)

    lines.append("---")
    lines.append("")
    lines.append(integrity_section(records))
    return "\n".join(lines) + "\n"


def write_report(jsonl_path: Path, out_path: Path) -> str:
    records = load(jsonl_path)
    raw_dir = jsonl_path.parent / "raw"
    meta = json.loads((jsonl_path.parent / "meta.json").read_text())
    prompts = _load_prompts()
    counter = tokens.default_counter()
    md = render(records, jsonl_path.parent.name, raw_dir=raw_dir, prompts=prompts, counter=counter, meta=meta)
    out_path.write_text(md)
    return md


def _load_prompts() -> dict[str, str]:
    from .__main__ import load_corpus

    return {t.id: t.prompt for t in load_corpus()}
