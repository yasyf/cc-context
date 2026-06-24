"""Execute runs: one (task, arm, model, repeat) through real `claude -p`, recorded to JSONL.

Each run is spawned by spawnllm (which owns transient-overload retry and resolves every
operational failure into a `Response.error` rather than raising), parsed by cc-transcript into
a PrintResult, then given an integrity verdict, a cost cross-check, and a deterministic grade.
Records are appended to a JSONL file and the raw payload is saved for audit. The budget is a
soft ceiling checked before each run; spend accrues after each run, so the next run is admitted
only while the running total is under the cap.
"""

from __future__ import annotations

import asyncio
import json
import os
import shutil
from dataclasses import dataclass
from pathlib import Path

import spawnllm
from cc_transcript import PrintResult, parse_print_result

from . import arms, cost, grade, integrity
from .config import Config
from .types import Task


def env_fingerprint() -> list[str]:
    """Names (not values, to avoid leaking secrets) of env vars that can change behavior/cost."""
    return sorted(
        k for k in os.environ if k.startswith(("ANTHROPIC_", "CLAUDE_")) or "THINKING" in k or k.startswith("DISABLE_")
    )


class BudgetExceeded(Exception):
    pass


@dataclass
class Session:
    cfg: Config
    session_id: str
    spent_usd: float = 0.0

    @property
    def runs_dir(self) -> Path:
        return self.cfg.results_dir / self.session_id

    @property
    def jsonl_path(self) -> Path:
        return self.runs_dir / "runs.jsonl"

    def setup(self) -> None:
        (self.runs_dir / "raw").mkdir(parents=True, exist_ok=True)
        meta = {
            "session_id": self.session_id,
            "models": list(self.cfg.models),
            "repeats": self.cfg.repeats,
            "budget_usd": self.cfg.budget_usd,
            "permission_mode": self.cfg.permission_mode,
            "strip_mcp": self.cfg.strip_mcp,
            "disallowed_tools": list(self.cfg.disallowed_tools),
            "env_fingerprint": env_fingerprint(),
        }
        (self.runs_dir / "meta.json").write_text(json.dumps(meta, indent=2))


def record_from(pr: PrintResult, cfg: Config, task: Task, arm: str, model: str, repeat: int, workdir: Path) -> dict:
    integ = integrity.assess(pr, arm)
    cc = cost.crosscheck(pr, model, cfg.cost_tolerance)
    graded = grade.grade(task, pr, workdir)
    u = pr.usage
    cc_5m = u.cache_creation.ephemeral_5m_input_tokens if u.cache_creation else u.cache_creation_input_tokens
    cc_1h = u.cache_creation.ephemeral_1h_input_tokens if u.cache_creation else 0
    init = pr.init
    return {
        "task_id": task.id,
        "category": task.category,
        "arm": arm,
        "model": model,
        "model_ids": list(pr.model_usage),
        "repeat": repeat,
        "ccx_helps": task.ccx_helps,
        "is_error": pr.is_error,
        "correct": graded.correct,
        "grade_detail": graded.detail,
        "total_cost_usd": pr.total_cost_usd,
        "cost_recomputed_usd": cc.recomputed_usd,
        "cost_rel_delta": cc.rel_delta,
        "cost_ok": cc.within_tolerance,
        "cost_note": cc.note,
        "num_turns": pr.num_turns,
        "usage": {
            "input": u.input_tokens,
            "output": u.output_tokens,
            "cache_read": u.cache_read_input_tokens,
            "cache_create_5m": cc_5m,
            "cache_create_1h": cc_1h,
        },
        "guards_active": arms.guards_available(cfg) if arm == "ccx" else None,
        "integrity": {
            "ok": integ.ok,
            "ccx_used": integ.ccx_used,
            "guard_fired": integ.guard_fired,
            "ccx_calls": integ.ccx_calls,
            "native_heavy_calls": integ.native_heavy_calls,
            "note": integ.note,
        },
        "init": {
            "mcp_servers": [s.name for s in init.mcp_servers] if init else [],
            "plugins": sorted(p.name for p in init.plugins) if init else [],
            "n_tools": len(init.tools) if init else 0,
            "n_skills": len(init.skills) if init else 0,
        },
        "session_id": str(pr.session_id),
        "stop_reason": pr.stop_reason,
    }


def error_record(cfg: Config, task: Task, arm: str, model: str, repeat: int, reason: str) -> dict:
    return {
        "task_id": task.id,
        "category": task.category,
        "arm": arm,
        "model": model,
        "repeat": repeat,
        "ccx_helps": task.ccx_helps,
        "is_error": True,
        "correct": False,
        "grade_detail": reason,
        "total_cost_usd": 0.0,
        "harness_error": reason,
    }


async def run_one(sess: Session, task: Task, arm: str, model: str, repeat: int) -> dict:
    """Run one (task, arm, model, repeat). Returns the record; raises BudgetExceeded first.

    `spawnllm.run` owns transient retry and resolves every operational failure — nonzero exit,
    error envelope, timeout, validation — into `resp.error`, never a raise. The full raw event
    stream `parse_print_result` needs lives in `resp.output.raw` on success and failure alike.
    """
    cfg = sess.cfg
    if sess.spent_usd >= cfg.budget_usd:
        raise BudgetExceeded(f"spent ${sess.spent_usd:.4f} >= budget ${cfg.budget_usd:.2f}")

    run_id = f"{task.id}__{arm}__{model}__r{repeat}"
    workdir = arms.prepare_workdir(cfg, task, arm, run_id)
    spec = arms.build_run_spec(cfg, task, arm, model, workdir)
    resp = await spawnllm.run(spec)

    (sess.runs_dir / "raw" / f"{run_id}.json").write_text(resp.output.raw or "")
    if resp.error is not None and not resp.output.raw.strip():
        return error_record(cfg, task, arm, model, repeat, resp.error.msg)
    if not resp.output.raw.strip():
        return error_record(cfg, task, arm, model, repeat, "empty output")

    try:
        pr = parse_print_result(resp.output.raw.encode())
    except (ValueError, KeyError, StopIteration) as e:
        return error_record(cfg, task, arm, model, repeat, f"parse failed: {e}")

    record = record_from(pr, cfg, task, arm, model, repeat, workdir)
    sess.spent_usd += pr.total_cost_usd

    keep = (not record["integrity"]["ok"]) or (not record["cost_ok"]) or pr.is_error
    if not keep:
        shutil.rmtree(workdir, ignore_errors=True)
    return record


async def run_corpus(
    sess: Session,
    tasks: list[Task],
    *,
    interleave: bool = True,
) -> list[dict]:
    """Run every (task, arm, model, repeat) round-robin: all tasks once per (model, repeat)
    before any task repeats, so a budget halt samples every task evenly instead of clipping
    late-alphabet tasks. Arms still interleave per repeat so neither rides the other's cache,
    and each task's (baseline, ccx) pair stays adjacent for the paired report."""
    cfg = sess.cfg
    sess.setup()
    plan: list[tuple[Task, str, str, int]] = []
    for model in cfg.models:
        for repeat in range(cfg.repeats):
            order = ("baseline", "ccx") if (repeat % 2 == 0 or not interleave) else ("ccx", "baseline")
            for task in tasks:
                for arm in order:
                    plan.append((task, arm, model, repeat))

    records: list[dict] = []
    with sess.jsonl_path.open("w") as out:
        for task, arm, model, repeat in plan:
            try:
                rec = await run_one(sess, task, arm, model, repeat)
            except BudgetExceeded as e:
                out.write(json.dumps({"halted": str(e)}) + "\n")
                break
            records.append(rec)
            out.write(json.dumps(rec) + "\n")
            out.flush()
    return records
