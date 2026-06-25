"""Aggregate run records into bench/RESULTS.md: the cache-aware cost-per-correct number.

Per model and arm: accuracy, cost-per-correct (sum cost / sum correct), mean cost and
turns, and the token-class breakdown. The headline is the paired baseline-vs-ccx delta
in accuracy and cost-per-correct, with bootstrap CIs over tasks (the independent unit).
A cost win bought with an accuracy loss is reported as such — never a bare "tokens saved".
Non-regression tasks (ccx_helps=False) are reported separately to surface any harm.
"""

from __future__ import annotations

import json
import math
import random
import statistics
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

BOOTSTRAP_N = 2000
BOOTSTRAP_SEED = 0


@dataclass
class Cell:
    costs: list[float] = field(default_factory=list)
    corrects: list[bool] = field(default_factory=list)

    def accuracy(self) -> float:
        return (sum(self.corrects) / len(self.corrects)) if self.corrects else 0.0

    def cost_per_correct(self) -> float:
        n_correct = sum(self.corrects)
        return (sum(self.costs) / n_correct) if n_correct else float("inf")


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


def by_task(records: list[dict], model: str, arm: str, helps: bool | None) -> dict[str, Cell]:
    cells: dict[str, Cell] = defaultdict(Cell)
    for r in records:
        if r["model"] != model or r["arm"] != arm:
            continue
        if helps is not None and r.get("ccx_helps", True) != helps:
            continue
        cell = cells[r["task_id"]]
        cell.costs.append(float(r["total_cost_usd"]))
        cell.corrects.append(bool(r["correct"]))
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


def arm_aggregate(cells: dict[str, Cell], task_ids: list[str]) -> tuple[float, float]:
    total_cost = 0.0
    total_correct = 0
    total_n = 0
    for tid in task_ids:
        c = cells.get(tid)
        if c is None:
            continue
        total_cost += sum(c.costs)
        total_correct += sum(c.corrects)
        total_n += len(c.corrects)
    accuracy = (total_correct / total_n) if total_n else 0.0
    cpc = (total_cost / total_correct) if total_correct else float("inf")
    return accuracy, cpc


def bootstrap_delta(
    base: dict[str, Cell], ccx: dict[str, Cell], task_ids: list[str]
) -> dict[str, object]:
    """Bootstrap CIs (2.5/97.5 pct) for Δaccuracy (pp) and Δcost-per-correct (%) over tasks.

    Each iteration resamples once and contributes to both deltas. A cost delta is undefined
    when a resample yields zero correct in an arm; those are counted, not silently dropped.
    """
    rng = random.Random(BOOTSTRAP_SEED)
    d_acc: list[float] = []
    d_cpc: list[float] = []
    n = len(task_ids)
    if n == 0:
        return {"d_accuracy_pp": (0.0, 0.0), "d_cpc_pct": (0.0, 0.0), "cpc_undefined": BOOTSTRAP_N}
    for _ in range(BOOTSTRAP_N):
        sample = [task_ids[rng.randrange(n)] for _ in range(n)]
        a_base, c_base = arm_aggregate(base, sample)
        a_ccx, c_ccx = arm_aggregate(ccx, sample)
        d_acc.append((a_ccx - a_base) * 100.0)
        if c_base not in (0.0, float("inf")) and c_ccx != float("inf"):
            d_cpc.append((c_ccx - c_base) / c_base * 100.0)
    return {
        "d_accuracy_pp": percentile_ci(d_acc),
        "d_cpc_pct": percentile_ci(d_cpc),
        "cpc_undefined": BOOTSTRAP_N - len(d_cpc),
    }


def percentile_ci(values: list[float]) -> tuple[float, float]:
    if not values:
        return (float("nan"), float("nan"))
    s = sorted(values)
    lo = s[int(0.025 * (len(s) - 1))]
    hi = s[int(0.975 * (len(s) - 1))]
    return (lo, hi)


def token_breakdown(records: list[dict], model: str, arm: str) -> dict[str, int]:
    totals = defaultdict(int)
    for r in records:
        if r["model"] != model or r["arm"] != arm or "usage" not in r:
            continue
        for k, v in r["usage"].items():
            totals[k] += int(v)
    return dict(totals)


def fmt_cost(x: float) -> str:
    return "n/a (0 correct)" if x == float("inf") else f"${x:.4f}"


def fmt_money(x: float) -> str:
    return f"${x:.4f}"


def render(records: list[dict], session_id: str) -> str:
    models = sorted({r["model"] for r in records})
    arms = ("baseline", "ccx")
    lines: list[str] = []
    lines.append("# cc-context benchmark results")
    lines.append("")
    lines.append(f"Session: `{session_id}` · {len(records)} runs · the cache-aware bill per correct answer.")
    lines.append("")
    lines.append(
        "Cost is the real API bill (input + output + cache_create + cache_read) that Claude "
        "Code reports as `total_cost_usd`, cross-checked against per-model prices. "
        "Accuracy is deterministic task success. A cost win with an accuracy loss is the rtk "
        "trap, flagged below."
    )
    lines.append("")

    for model in models:
        lines.append(f"## Model: {model}")
        lines.append("")
        lines.append("| Arm | N | Accuracy | Cost/correct | Mean cost | Mean turns | Integrity OK | Cost-check OK |")
        lines.append("|---|---|---|---|---|---|---|---|")
        ok_records = [r for r in records if r.get("integrity", {}).get("ok", True)]
        n_excluded = len([r for r in records if r["model"] == model]) - len([r for r in ok_records if r["model"] == model])
        cells = {arm: by_task(ok_records, model, arm, None) for arm in arms}
        all_tasks = sorted({r["task_id"] for r in records if r["model"] == model})
        for arm in arms:
            arm_recs = [r for r in records if r["model"] == model and r["arm"] == arm]
            n = len(arm_recs)
            acc, cpc = arm_aggregate(cells[arm], all_tasks)
            mean_cost = statistics.fmean([r["total_cost_usd"] for r in arm_recs]) if arm_recs else 0.0
            turns = [r["num_turns"] for r in arm_recs if "num_turns" in r]
            mean_turns = statistics.fmean(turns) if turns else 0.0
            integ_ok = sum(1 for r in arm_recs if r.get("integrity", {}).get("ok")) / n if n else 0.0
            cost_ok = sum(1 for r in arm_recs if r.get("cost_ok")) / n if n else 0.0
            lines.append(
                f"| {arm} | {n} | {acc * 100:.1f}% | {fmt_cost(cpc)} | {fmt_money(mean_cost)} | "
                f"{mean_turns:.1f} | {integ_ok * 100:.0f}% | {cost_ok * 100:.0f}% |"
            )
        lines.append("")
        lines.append(
            "_N counts all runs for the arm; Accuracy and Cost/correct count only integrity-OK "
            "runs (Integrity OK shows the share), so their denominator is N minus mislabeled runs._"
        )
        lines.append("")

        paired, dropped = paired_task_ids(ok_records, model)
        a_base, c_base = arm_aggregate(cells["baseline"], paired)
        a_ccx, c_ccx = arm_aggregate(cells["ccx"], paired)
        ci = bootstrap_delta(cells["baseline"], cells["ccx"], paired)
        acc_ci = ci["d_accuracy_pp"]
        cpc_ci = ci["d_cpc_pct"]
        d_acc = (a_ccx - a_base) * 100.0
        d_cpc = ((c_ccx - c_base) / c_base * 100.0) if c_base not in (0.0, float("inf")) else float("nan")
        lines.append("### Headline (ccx vs baseline)")
        lines.append("")
        lines.append(
            f"- Paired on {len(paired)} tasks present in both arms"
            + (f" ({dropped} dropped for one-arm-only data)" if dropped else "")
            + (f"; **{n_excluded} mislabeled run(s) excluded** from this aggregate (see Integrity below)" if n_excluded else "")
        )
        lines.append(
            f"- Δ accuracy: **{d_acc:+.1f} pp** "
            f"(95% CI [{acc_ci[0]:+.1f}, {acc_ci[1]:+.1f}])"
        )
        cpc_str = f"{d_cpc:+.1f}%" if not math.isnan(d_cpc) else "undefined (baseline 0 correct)"
        tied = acc_ci_straddles_zero(acc_ci)
        if tied and not math.isnan(d_cpc):
            lines.append(
                f"- Δ cost (efficiency, not a per-correct win): **{cpc_str}** "
                f"(95% CI [{cpc_ci[0]:+.1f}, {cpc_ci[1]:+.1f}]) — accuracy CI straddles 0, so "
                "this is a cost-efficiency signal, NOT a cheaper-per-correct-answer claim"
            )
        else:
            lines.append(
                f"- Δ cost-per-correct: **{cpc_str}** "
                f"(95% CI [{cpc_ci[0]:+.1f}, {cpc_ci[1]:+.1f}]) "
                "(negative = ccx cheaper per correct answer)"
            )
        if ci["cpc_undefined"]:
            lines.append(f"  - note: {ci['cpc_undefined']}/{BOOTSTRAP_N} bootstrap resamples had an undefined cost-per-correct (an arm drew 0 correct)")
        lines.append(verdict(d_acc, d_cpc, acc_ci))
        lines.append("")

        nr_base = by_task(records, model, "baseline", helps=False)
        nr_ccx = by_task(records, model, "ccx", helps=False)
        nr_tasks = sorted(set(nr_base) | set(nr_ccx))
        if nr_tasks:
            nb_acc, nb_cpc = arm_aggregate(nr_base, nr_tasks)
            nc_acc, nc_cpc = arm_aggregate(nr_ccx, nr_tasks)
            lines.append("### Non-regression (tasks ccx should not help)")
            lines.append("")
            lines.append(
                f"- accuracy baseline {nb_acc * 100:.1f}% → ccx {nc_acc * 100:.1f}%; "
                f"cost/correct baseline {fmt_cost(nb_cpc)} → ccx {fmt_cost(nc_cpc)}"
            )
            acc_ok = nc_acc >= nb_acc - 1e-9
            if nb_cpc in (0.0, float("inf")) or nc_cpc == float("inf"):
                lines.append("- cost-harm check: undefined (an arm had 0 correct on non-regression tasks)")
                cost_ok = True
            else:
                cost_delta = (nc_cpc - nb_cpc) / nb_cpc * 100.0
                cost_ok = cost_delta <= 10.0
                lines.append(f"- cost/correct Δ {cost_delta:+.1f}% (parity band ±10%)")
            lines.append(
                "- " + ("no harm detected" if acc_ok and cost_ok else "POSSIBLE HARM — ccx changed a task it should not affect")
            )
            lines.append("")

        lines.append("### Token classes (summed)")
        lines.append("")
        lines.append("| Arm | input | output | cache_read | cache_create_5m | cache_create_1h |")
        lines.append("|---|---|---|---|---|---|")
        for arm in arms:
            t = token_breakdown(records, model, arm)
            lines.append(
                f"| {arm} | {t.get('input', 0)} | {t.get('output', 0)} | {t.get('cache_read', 0)} | "
                f"{t.get('cache_create_5m', 0)} | {t.get('cache_create_1h', 0)} |"
            )
        lines.append("")

    lines.append("---")
    lines.append("")
    lines.append(integrity_section(records))
    return "\n".join(lines) + "\n"


def acc_ci_straddles_zero(acc_ci: tuple[float, float]) -> bool:
    """True when the accuracy CI includes 0 — accuracy is statistically tied between arms."""
    lo, hi = acc_ci
    if math.isnan(lo) or math.isnan(hi):
        return True
    return lo <= 0.0 <= hi


def verdict(d_acc: float, d_cpc: float, acc_ci: tuple[float, float]) -> str:
    if math.isnan(d_cpc):
        return "- (inconclusive — baseline had 0 correct answers)"
    tied = acc_ci_straddles_zero(acc_ci)
    if d_cpc >= 0:
        if tied:
            return (
                "- ❌ ccx is not cheaper per correct answer in this run (accuracy is tied, so "
                "this is pure overhead — NOT a per-correct win)."
            )
        return "- ❌ ccx is not cheaper per correct answer in this run."
    if d_acc < -1.0 and not tied:
        return "- ⚠️ **rtk trap**: ccx is cheaper but less accurate — not an honest savings claim."
    if tied:
        return (
            "- efficiency, not a per-correct win: ccx spends less but accuracy is statistically "
            f"tied (accuracy CI [{acc_ci[0]:+.1f}, {acc_ci[1]:+.1f}] straddles 0), so this is a "
            "cost-efficiency signal, not a cheaper-per-correct-answer claim."
        )
    if d_acc >= 0:
        return "- ✅ ccx is cheaper per correct answer at equal-or-better accuracy."
    return f"- ccx is cheaper per correct answer; accuracy change {d_acc:+.1f} pp is within the 1 pp noise floor (see CI)."


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
        out.append(
            "  - the ccx arm's PreToolUse runs capt-hook: the cc-context navigation pack "
            "(large-Read / rg / git-diff / sed → ccx) plus capt-hook's built-in guards "
            "(styleguide, task/plan/vcs nudges). The built-ins are present but dormant for "
            "read-only Q&A tasks — no Python edits, commits, pending tasks, or plan events fire them."
        )
    return "\n".join(out)


def write_report(jsonl_path: Path, out_path: Path) -> str:
    records = load(jsonl_path)
    md = render(records, jsonl_path.parent.name)
    out_path.write_text(md)
    return md
