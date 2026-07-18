"""B1/B2 behavioral readouts for the Stage-1 flood probe.

Stage 1's go/no-go is not a token CI (6 tasks under-power the bootstrap) but two per-run
behavioral rates read off a completed session's records and transcripts:

* **B1 — baseline read-to-verify (flood) rate.** On a wide enumeration, did the baseline answer
  from bounded searches, or open a large file to verify? A run floods when it dumps a whole file
  (an unbounded ``Read`` or a ``cat``) or pulls a large slice into context (a ``Read`` result over
  ``FLOOD_READ_TOKENS``). This is the dominant variable — ccx's token flip is a consequence of it.
* **B2 — ccx compact-lane adherence.** Did the ccx arm stay compact — a scoped ``ccx code grep`` or
  an ``ccx exec`` script — or self-flood with the orient reflex (``ccx repo overview``) and per-file
  ``ccx code outline``? A run is compact when it used grep/exec, ran at most ``MAX_OUTLINE_CALLS``
  outlines, and never reached for repo overview.

Both read from a completed session dir (``results/<session>/{runs.jsonl,raw/}``); no model runs.
"""

from __future__ import annotations

import fnmatch
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from . import trajectory
from .tokens import local_count
from .types import ARMS, TrajectoryMetrics

BASELINE = "baseline"
CCX_ARMS: tuple[str, ...] = tuple(a for a in ARMS if a != BASELINE)

# A single Read/dump result this large means a whole file (or a big slice of one) entered context —
# the read-to-verify flood the wide tasks are designed to provoke. ~4 K tokens ≈ ~16 KB of source.
FLOOD_READ_TOKENS = 4000
# More than this many `ccx code outline` calls is the per-file self-flood the ladder rewrite forbids.
MAX_OUTLINE_CALLS = 2
# native_heavy_calls labels that denote a whole-file dump or an unbounded open (see integrity.py).
FLOOD_HEAVY: tuple[str, ...] = ("cat", "read-unbounded")


def _local_counter(text: str) -> int:
    return local_count(text, "m")


def _run_id(record: dict) -> str:
    return f"{record['task_id']}__{record['arm']}__{record['model']}__r{record['repeat']}"


def max_read_tokens(metrics: TrajectoryMetrics) -> int:
    """The largest single ``Read`` tool result the run pulled into context (0 if it never read)."""
    return max((tc.output_tokens for tc in metrics.tool_calls if tc.name == "Read"), default=0)


def run_flooded(record: dict, metrics: TrajectoryMetrics | None) -> bool:
    """B1 signal: did this (baseline) run open a large file — a dump, an unbounded Read, or a big slice?"""
    heavy = record.get("integrity", {}).get("native_heavy_calls", [])
    if any(h in FLOOD_HEAVY for h in heavy):
        return True
    return metrics is not None and max_read_tokens(metrics) >= FLOOD_READ_TOKENS


def run_compact(record: dict) -> bool:
    """B2 signal: did this (ccx) run stay in the compact lane — grep/exec, few outlines, no overview?"""
    calls = record.get("integrity", {}).get("ccx_calls", [])
    used_scoped = any("grep" in c or "exec" in c for c in calls)
    n_outline = sum(1 for c in calls if "outline" in c)
    used_overview = any("overview" in c for c in calls)
    return used_scoped and n_outline <= MAX_OUTLINE_CALLS and not used_overview


@dataclass(frozen=True)
class RunBehavior:
    """One run's behavioral readout. `flooded` is set only for the baseline arm, `compact` only for
    the ccx arms; the other stays None."""

    task_id: str
    arm: str
    model: str
    repeat: int
    flooded: bool | None
    compact: bool | None
    max_read_tokens: int


@dataclass(frozen=True)
class BehaviorReport:
    runs: tuple[RunBehavior, ...]
    b1_flooded: int
    b1_total: int
    b2_compact: int
    b2_total: int

    @property
    def b1_rate(self) -> float:
        return self.b1_flooded / self.b1_total if self.b1_total else float("nan")

    @property
    def b2_rate(self) -> float:
        return self.b2_compact / self.b2_total if self.b2_total else float("nan")


def load_records(session_dir: Path) -> list[dict]:
    """Every run record in a session's runs.jsonl, dropping the ceiling-halt marker line."""
    records: list[dict] = []
    for line in (session_dir / "runs.jsonl").read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        d = json.loads(line)
        if "halted" in d:
            continue
        records.append(d)
    return records


def _run_metrics(raw_dir: Path, record: dict, count: Callable[[str], int]) -> TrajectoryMetrics | None:
    path = raw_dir / f"{_run_id(record)}.json"
    if not path.exists():
        return None
    try:
        return trajectory.from_file(path, first_prompt="", count=count)
    except (ValueError, KeyError):
        return None


def compute(session_dir: Path, *, task_glob: str | None = None, count: Callable[[str], int] = _local_counter) -> BehaviorReport:
    """B1/B2 over a session's integrity-ok, non-error runs (optionally a task-id fnmatch glob).

    B1 is measured over the baseline arm, B2 over the ccx arms; a run counts toward exactly one.
    """
    raw_dir = session_dir / "raw"
    runs: list[RunBehavior] = []
    b1_flooded = b1_total = b2_compact = b2_total = 0
    for r in load_records(session_dir):
        if r.get("is_error") or not r.get("integrity", {}).get("ok", True):
            continue
        if task_glob and not fnmatch.fnmatch(r["task_id"], task_glob):
            continue
        metrics = _run_metrics(raw_dir, r, count)
        mrt = max_read_tokens(metrics) if metrics else 0
        flooded = compact = None
        if r["arm"] == BASELINE:
            flooded = run_flooded(r, metrics)
            b1_total += 1
            b1_flooded += int(flooded)
        elif r["arm"] in CCX_ARMS:
            compact = run_compact(r)
            b2_total += 1
            b2_compact += int(compact)
        runs.append(RunBehavior(r["task_id"], r["arm"], r["model"], int(r.get("repeat", 0)), flooded, compact, mrt))
    return BehaviorReport(tuple(runs), b1_flooded, b1_total, b2_compact, b2_total)


def render(report: BehaviorReport) -> str:
    """A short text summary of the two rates plus the per-run flood/compact flags."""
    lines = [
        f"B1 baseline read-to-verify (flood) rate: {report.b1_flooded}/{report.b1_total}"
        + (f" = {report.b1_rate:.0%}" if report.b1_total else " (no baseline runs)"),
        f"B2 ccx compact-lane adherence:           {report.b2_compact}/{report.b2_total}"
        + (f" = {report.b2_rate:.0%}" if report.b2_total else " (no ccx runs)"),
        "",
    ]
    for rb in report.runs:
        flag = "flood" if rb.flooded else ("compact" if rb.compact else ("bounded" if rb.arm == BASELINE else "self-flood"))
        lines.append(f"  {rb.task_id:34}{rb.arm:10}{rb.model:8}r{rb.repeat}  read≤{rb.max_read_tokens:>7}  {flag}")
    return "\n".join(lines) + "\n"
