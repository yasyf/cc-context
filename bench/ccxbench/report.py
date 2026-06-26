"""Aggregate run records into RESULTS.md: the prompt high-water savings story.

The headline is paired **`1 - H_ccx / H_base`** over task pairs where both arms produced a
correct answer — how much smaller ccx kept Claude's context window. Trajectory metrics come
from each run's saved stream-json transcript (`trajectory.from_file`); aggregate cost is
walled off into its own clearly-labeled ledger and never fused with the context numbers.
Correctness is a gate, reported separately and loudly: any task where the arms disagree is
flagged as a regression or improvement.
"""

from __future__ import annotations

import json
import math
import random
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

from . import tokens, trajectory
from .types import DECOMP_TERMS, TrajectoryMetrics

BOOTSTRAP_N = 2000
BOOTSTRAP_SEED = 0

# Cache-weighted cost-equivalent (a $ proxy, NOT the headline): a cache read is an order of
# magnitude cheaper than a fresh prompt token, a 5m cache write a small premium, a 1h write
# larger. These are relative weights over the record's token classes, summed per arm.
COST_WEIGHTS = {
    "cache_read": 0.1,
    "cache_create_5m": 1.25,
    "cache_create_1h": 2.0,
    "input": 1.0,
    "output": 1.0,
}

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

    def accuracy(self) -> float:
        return (sum(self.corrects) / len(self.corrects)) if self.corrects else 0.0


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


def paired_task_ids(records: list[dict], model: str) -> tuple[list[str], int]:
    """Task ids with >=1 record in BOTH arms for this model, and the count dropped."""
    per_arm: dict[str, set[str]] = {"baseline": set(), "ccx": set()}
    for r in records:
        if r.get("model") == model and r.get("arm") in per_arm:
            per_arm[r["arm"]].add(r["task_id"])
    both = per_arm["baseline"] & per_arm["ccx"]
    either = per_arm["baseline"] | per_arm["ccx"]
    return sorted(both), len(either - both)


def percentile_ci(values: list[float]) -> tuple[float, float]:
    if not values:
        return (float("nan"), float("nan"))
    s = sorted(values)
    lo = s[int(0.025 * (len(s) - 1))]
    hi = s[int(0.975 * (len(s) - 1))]
    return (lo, hi)


def bootstrap_ci(savings: list[float]) -> tuple[float, float]:
    """Paired bootstrap CI (2.5/97.5 pct) on the mean per-task high-water savings."""
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


def cost_equivalent(usage: dict) -> float:
    """Cache-weighted cost-equivalent token total for one run's usage (a $ proxy)."""
    return sum(COST_WEIGHTS[k] * float(usage.get(k, 0)) for k in COST_WEIGHTS)


@dataclass
class TaskPair:
    task_id: str
    base: TrajectoryMetrics
    ccx: TrajectoryMetrics

    @property
    def savings(self) -> float:
        return 1.0 - self.ccx.high_water / self.base.high_water


def _run_id(task_id: str, arm: str, model: str, repeat: int) -> str:
    return f"{task_id}__{arm}__{model}__r{repeat}"


def _metrics_for(
    records: list[dict],
    model: str,
    arm: str,
    task_id: str,
    *,
    raw_dir: Path,
    prompt: str,
    counter: tokens.TokenCounter,
) -> TrajectoryMetrics | None:
    """First non-stub trajectory for (model, arm, task_id), or None if none parses."""
    for r in records:
        if r["model"] != model or r["arm"] != arm or r["task_id"] != task_id:
            continue
        path = raw_dir / f"{_run_id(task_id, arm, model, r['repeat'])}.json"
        if not path.exists():
            continue
        m = trajectory.from_file(path, first_prompt=prompt, count=counter.count)
        if m is not None:
            return m
    return None


def build_pairs(
    records: list[dict],
    model: str,
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
        base = _metrics_for(records, model, "baseline", tid, raw_dir=raw_dir, prompt=prompt, counter=counter)
        ccx = _metrics_for(records, model, "ccx", tid, raw_dir=raw_dir, prompt=prompt, counter=counter)
        if base is None or ccx is None or base.high_water == 0:
            continue
        pairs.append(TaskPair(tid, base, ccx))
    return pairs


def both_correct_tasks(cells: dict[str, dict[str, Cell]], paired: list[str]) -> set[str]:
    """Task ids where every record in BOTH arms graded correct."""
    out: set[str] = set()
    for tid in paired:
        b = cells["baseline"].get(tid)
        c = cells["ccx"].get(tid)
        if b and c and all(b.corrects) and all(c.corrects):
            out.add(tid)
    return out


def headline_section(pairs: list[TaskPair]) -> list[str]:
    lines = ["### High-water headline (ccx vs baseline)", ""]
    if not pairs:
        lines.append("- no both-correct task pairs with parseable transcripts — nothing to compare")
        lines.append("")
        return lines

    savings = [p.savings for p in pairs]
    mean_savings = sum(savings) / len(savings)
    lo, hi = bootstrap_ci(savings)
    h_base = sum(p.base.high_water for p in pairs) / len(pairs)
    h_ccx = sum(p.ccx.high_water for p in pairs) / len(pairs)
    wins = sum(1 for p in pairs if p.ccx.high_water < p.base.high_water)
    losses = sum(1 for p in pairs if p.ccx.high_water > p.base.high_water)
    ties = sum(1 for p in pairs if p.ccx.high_water == p.base.high_water)
    p_value = sign_test_p(wins, losses)

    lines.append(f"- Paired on **{len(pairs)} both-correct task(s)** (token deltas only on both-correct pairs)")
    lines.append(
        f"- Mean high-water savings: **{mean_savings * 100:+.1f}%** "
        f"(95% CI [{lo * 100:+.1f}%, {hi * 100:+.1f}%]) — positive = ccx kept context smaller"
    )
    lines.append(f"- Mean high-water: baseline **{h_base:,.0f}** → ccx **{h_ccx:,.0f}** tokens")
    lines.append(f"- Win/loss/tie (ccx H </>/== base H): **{wins} / {losses} / {ties}**")
    lines.append(f"- Exact paired sign-test p-value: **{p_value:.4f}**")
    lines.append("")
    return lines


def context_ledger_section(metrics: dict[str, list[TrajectoryMetrics]]) -> list[str]:
    lines = ["### Context ledger (unweighted — the size claim)", ""]
    lines.append("| Arm | N | mean high-water | mean tool-output | mean turns | mean tool-calls |")
    lines.append("|---|---|---|---|---|---|")
    for arm in ("baseline", "ccx"):
        ms = metrics[arm]
        n = len(ms)
        if n:
            hw = sum(m.high_water for m in ms) / n
            to = sum(m.cumulative_tool_output for m in ms) / n
            tc = sum(m.turn_count for m in ms) / n
            calls = sum(m.tool_call_count for m in ms) / n
            lines.append(f"| {arm} | {n} | {hw:,.0f} | {to:,.0f} | {tc:.1f} | {calls:.1f} |")
        else:
            lines.append(f"| {arm} | 0 | — | — | — | — |")
    lines.append("")
    return lines


def cost_ledger_section(records: list[dict], model: str) -> list[str]:
    lines = ["### Cost ledger ($ proxy — NOT the headline)", ""]
    lines.append("_Cache-weighted cost-equivalent tokens (cache_read ×0.1, create_5m ×1.25, create_1h ×2.0). "
                 "Walled off from the context ledger above — never the size claim._")
    lines.append("")
    lines.append("| Arm | N | mean cost-equivalent tokens |")
    lines.append("|---|---|---|")
    for arm in ("baseline", "ccx"):
        recs = [r for r in records if r["model"] == model and r["arm"] == arm and "usage" in r]
        n = len(recs)
        if n:
            mean_ce = sum(cost_equivalent(r["usage"]) for r in recs) / n
            lines.append(f"| {arm} | {n} | {mean_ce:,.0f} |")
        else:
            lines.append(f"| {arm} | 0 | — |")
    lines.append("")
    return lines


def waterfall_section(pairs: list[TaskPair]) -> list[str]:
    lines = ["### Per-task waterfall", ""]
    if not pairs:
        lines.append("- no both-correct pairs to chart")
        lines.append("")
        return lines
    lines.append("| Task | H_base | H_ccx | savings% | dominant Δ-bucket |")
    lines.append("|---|---|---|---|---|")
    for p in sorted(pairs, key=lambda x: x.savings, reverse=True):
        delta = _decomp_delta(p)
        bucket = _dominant_delta_bucket(delta)
        lines.append(f"| `{p.task_id}` | {p.base.high_water:,} | {p.ccx.high_water:,} | {p.savings * 100:+.1f}% | {bucket} |")
    lines.append("")
    return lines


def _decomp_delta(p: TaskPair) -> dict[str, int]:
    b = p.base.decomposition
    c = p.ccx.decomposition
    return {term: getattr(c, term) - getattr(b, term) for term in DECOMP_TERMS}


def _dominant_delta_bucket(delta: dict[str, int]) -> str:
    """The decomposition term that moved the high-water mark most (by magnitude)."""
    return max(delta, key=lambda k: abs(delta[k]))


def diagnosis_section(pairs: list[TaskPair]) -> list[str]:
    lines = ["### Auto-diagnosis (tasks where ccx grew the context)", ""]
    regressions = [p for p in pairs if p.ccx.high_water > p.base.high_water]
    if not regressions:
        lines.append("- none — ccx held or shrank the high-water mark on every both-correct task")
        lines.append("")
        return lines
    for p in regressions:
        delta = _decomp_delta(p)
        term = max(delta, key=lambda k: delta[k])
        cause = CAUSE_BY_TERM[term]
        lines.append(f"- `{p.task_id}`: +{p.ccx.high_water - p.base.high_water:,} tokens, dominant Δ `{term}` → {cause}")
    lines.append("")
    return lines


def correctness_panel(records: list[dict], model: str) -> list[str]:
    lines = ["### Correctness panel", ""]
    cells = {arm: by_task(records, model, arm) for arm in ("baseline", "ccx")}
    for arm in ("baseline", "ccx"):
        recs = [r for r in records if r["model"] == model and r["arm"] == arm]
        n = len(recs)
        correct = sum(1 for r in recs if r["correct"])
        acc = (correct / n * 100.0) if n else 0.0
        lines.append(f"- {arm} accuracy: **{acc:.1f}%** ({correct}/{n})")

    paired, _ = paired_task_ids(records, model)
    regressions: list[str] = []
    improvements: list[str] = []
    for tid in paired:
        b = cells["baseline"].get(tid)
        c = cells["ccx"].get(tid)
        if not b or not c:
            continue
        b_ok = all(b.corrects)
        c_ok = all(c.corrects)
        if b_ok and not c_ok:
            regressions.append(tid)
        elif c_ok and not b_ok:
            improvements.append(tid)
    if regressions:
        lines.append("")
        lines.append(f"- ⚠️ **REGRESSION** — baseline correct but ccx wrong on: {', '.join(f'`{t}`' for t in regressions)}")
    if improvements:
        lines.append("")
        lines.append(f"- ✅ **improvement** — ccx correct but baseline wrong on: {', '.join(f'`{t}`' for t in improvements)}")
    if not regressions and not improvements:
        lines.append("")
        lines.append("- no correctness disagreements between arms")
    lines.append("")
    return lines


def render(
    records: list[dict],
    session_id: str,
    *,
    raw_dir: Path | None = None,
    prompts: dict[str, str] | None = None,
    counter: tokens.TokenCounter | None = None,
) -> str:
    """Render RESULTS.md from run records.

    Trajectory sections need each run's saved transcript (`raw_dir/<run_id>.json`), the task
    prompts (`prompts[task_id]`), and a token `counter`. When `raw_dir` is missing the report
    degrades to the correctness panel plus a note, never crashing.
    """
    prompts = prompts or {}
    models = sorted({r["model"] for r in records})
    have_traj = raw_dir is not None and raw_dir.exists()
    if have_traj and counter is None:
        counter = tokens.default_counter()

    lines: list[str] = ["# cc-context benchmark results", ""]
    lines.append(f"Session: `{session_id}` · {len(records)} runs · prompt high-water savings (ccx vs baseline).")
    lines.append("")
    lines.append(
        "Headline = paired **`1 − H_ccx / H_base`** (the largest single-turn prompt, i.e. how big "
        "context actually got) over task pairs where both arms answered correctly. Cost is a "
        "separate, walled-off ledger; correctness is a gate, reported on its own."
    )
    lines.append("")
    if not have_traj:
        lines.append(
            "_No transcripts available — trajectory sections skipped; only the correctness panel is shown._"
        )
        lines.append("")

    for model in models:
        lines.append(f"## Model: {model}")
        lines.append("")

        ok_records = [r for r in records if r.get("integrity", {}).get("ok", True)]
        cells = {arm: by_task(ok_records, model, arm) for arm in ("baseline", "ccx")}

        if have_traj:
            paired, _ = paired_task_ids(ok_records, model)
            both_ok = both_correct_tasks(cells, paired)
            pairs = build_pairs(
                ok_records, model, paired, both_ok, raw_dir=raw_dir, prompts=prompts, counter=counter
            )
            arm_metrics = _all_metrics(ok_records, model, raw_dir=raw_dir, prompts=prompts, counter=counter)
            lines += headline_section(pairs)
            lines += context_ledger_section(arm_metrics)
            lines += cost_ledger_section(records, model)
            lines += waterfall_section(pairs)
            lines += diagnosis_section(pairs)

        lines += correctness_panel(records, model)

    lines.append("---")
    lines.append("")
    lines.append(integrity_section(records))
    return "\n".join(lines) + "\n"


def _all_metrics(
    records: list[dict],
    model: str,
    *,
    raw_dir: Path,
    prompts: dict[str, str],
    counter: tokens.TokenCounter,
) -> dict[str, list[TrajectoryMetrics]]:
    """Every non-stub trajectory per arm (context ledger is unweighted over all runs)."""
    out: dict[str, list[TrajectoryMetrics]] = {"baseline": [], "ccx": []}
    for r in records:
        if r["model"] != model or r["arm"] not in out:
            continue
        path = raw_dir / f"{_run_id(r['task_id'], r['arm'], model, r['repeat'])}.json"
        if not path.exists():
            continue
        m = trajectory.from_file(path, first_prompt=prompts.get(r["task_id"], ""), count=counter.count)
        if m is not None:
            out[r["arm"]].append(m)
    return out


def integrity_section(records: list[dict]) -> str:
    bad = [r for r in records if not r.get("integrity", {}).get("ok", True)]
    cost_bad = [r for r in records if not r.get("cost_ok", True) and not r.get("is_error")]
    out = ["## Integrity & confounds", ""]
    out.append(f"- Runs with integrity failures (arm mislabeled): **{len(bad)}** / {len(records)}")
    for r in bad[:10]:
        out.append(f"  - `{r['task_id']}` [{r['arm']}]: {r.get('integrity', {}).get('note', '')}")
    out.append(f"- Runs where recomputed cost diverged from total_cost_usd: **{len(cost_bad)}**")
    for r in cost_bad[:10]:
        out.append(f"  - `{r['task_id']}` [{r['arm']}]: rel_delta={r.get('cost_rel_delta', 0):.3f}; {r.get('cost_note', '')}")
    plugin_sets = {tuple(r.get("init", {}).get("plugins", [])) for r in records if "init" in r}
    out.append(f"- Distinct ambient plugin sets across runs: **{len(plugin_sets)}** (1 = symmetric across arms)")
    ccx_runs = [r for r in records if r.get("arm") == "ccx"]
    guards_on = sum(1 for r in ccx_runs if r.get("guards_active"))
    if ccx_runs and guards_on == 0:
        out.append("- ⚠️ capt-hook guards were INACTIVE for the ccx arm (pack failed to load); ccx was tested via the facade MCP + ladder only.")
    elif ccx_runs:
        out.append(f"- capt-hook guards active on {guards_on}/{len(ccx_runs)} ccx runs.")
    return "\n".join(out)


def write_report(jsonl_path: Path, out_path: Path) -> str:
    records = load(jsonl_path)
    raw_dir = jsonl_path.parent / "raw"
    prompts = _load_prompts()
    counter = tokens.default_counter()
    md = render(records, jsonl_path.parent.name, raw_dir=raw_dir, prompts=prompts, counter=counter)
    out_path.write_text(md)
    return md


def _load_prompts() -> dict[str, str]:
    from .__main__ import load_corpus

    return {t.id: t.prompt for t in load_corpus()}
