"""Execute runs: one (task, arm, model, repeat) through real `claude -p`, recorded to JSONL.

Each run gets a fresh workdir, the arm's invocation, a parsed envelope, an integrity
verdict, a cost cross-check, and a deterministic grade. Records are appended to a JSONL
file and the raw envelope is saved for audit. The budget is a soft ceiling checked before
each run; spend (including cost salvaged from unparseable runs) accrues after each run,
so the next run is admitted only while the running total is under the cap.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path

from . import arms, cost, grade, integrity
from .config import Config
from .envelope import Envelope, parse
from .types import Task

COST_RE = re.compile(r'"total_cost_usd"\s*:\s*([0-9.]+)')


def salvage_cost(text: str) -> float:
    """Best-effort extract the largest total_cost_usd from raw stdout (for unparseable runs)."""
    vals = [float(m) for m in COST_RE.findall(text or "")]
    return max(vals) if vals else 0.0


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


def record_from(env: Envelope, cfg: Config, task: Task, arm: str, model: str, repeat: int, workdir: Path) -> dict:
    integ = integrity.assess(env, arm)
    cc = cost.crosscheck(env, cfg.prices)
    graded = grade.grade(task, env, workdir)
    u = env.usage
    return {
        "task_id": task.id,
        "category": task.category,
        "arm": arm,
        "model": model,
        "model_ids": list(env.model_usage),
        "repeat": repeat,
        "ccx_helps": task.ccx_helps,
        "is_error": env.is_error,
        "correct": graded.correct,
        "grade_detail": graded.detail,
        "total_cost_usd": env.total_cost_usd,
        "cost_recomputed_usd": cc.recomputed_usd,
        "cost_rel_delta": cc.rel_delta,
        "cost_ok": cc.within_tolerance,
        "cost_note": cc.note,
        "num_turns": env.num_turns,
        "usage": {
            "input": u.input,
            "output": u.output,
            "cache_read": u.cache_read,
            "cache_create_5m": u.cache_create_5m,
            "cache_create_1h": u.cache_create_1h,
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
            "mcp_servers": env.init.mcp_servers,
            "plugins": sorted(env.init.plugins),
            "n_tools": len(env.init.tools),
            "n_skills": env.init.n_skills,
        },
        "session_id": env.session_id,
        "stop_reason": env.stop_reason,
    }


def error_record(cfg: Config, task: Task, arm: str, model: str, repeat: int, reason: str, cost_usd: float = 0.0) -> dict:
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
        "total_cost_usd": cost_usd,
        "harness_error": reason,
    }


def run_one(sess: Session, task: Task, arm: str, model: str, repeat: int) -> dict:
    """Run one (task, arm, model, repeat). Returns the record; raises BudgetExceeded first."""
    cfg = sess.cfg
    if sess.spent_usd >= cfg.budget_usd:
        raise BudgetExceeded(f"spent ${sess.spent_usd:.4f} >= budget ${cfg.budget_usd:.2f}")

    run_id = f"{task.id}__{arm}__{model}__r{repeat}"
    workdir = arms.prepare_workdir(cfg, task, arm, run_id)
    argv, env, cwd = arms.build_command(cfg, task, arm, model, workdir)

    try:
        proc = subprocess.run(argv, env=env, cwd=str(cwd), capture_output=True, text=True, timeout=cfg.timeout_s)
    except subprocess.TimeoutExpired:
        return error_record(cfg, task, arm, model, repeat, f"timeout after {cfg.timeout_s}s")

    (sess.runs_dir / "raw" / f"{run_id}.json").write_text(proc.stdout or proc.stderr or "")
    if not proc.stdout.strip():
        return error_record(cfg, task, arm, model, repeat, f"empty stdout (rc={proc.returncode}): {proc.stderr[:200]}")

    try:
        envlp = parse(proc.stdout)
    except (ValueError, json.JSONDecodeError) as e:
        salvaged = salvage_cost(proc.stdout)
        sess.spent_usd += salvaged
        return error_record(cfg, task, arm, model, repeat, f"parse failed: {e}", cost_usd=salvaged)

    record = record_from(envlp, cfg, task, arm, model, repeat, workdir)
    sess.spent_usd += envlp.total_cost_usd

    keep = (not record["integrity"]["ok"]) or (not record["cost_ok"]) or envlp.is_error
    if not keep:
        shutil.rmtree(workdir, ignore_errors=True)
    return record


def run_corpus(
    sess: Session,
    tasks: list[Task],
    *,
    interleave: bool = True,
) -> list[dict]:
    """Run every (task, arm, model, repeat). Arms interleave so neither rides the other's cache."""
    cfg = sess.cfg
    sess.setup()
    plan: list[tuple[Task, str, str, int]] = []
    for task in tasks:
        for model in cfg.models:
            for repeat in range(cfg.repeats):
                order = ("baseline", "ccx") if (repeat % 2 == 0 or not interleave) else ("ccx", "baseline")
                for arm in order:
                    plan.append((task, arm, model, repeat))

    records: list[dict] = []
    with sess.jsonl_path.open("w") as out:
        for task, arm, model, repeat in plan:
            try:
                rec = run_one(sess, task, arm, model, repeat)
            except BudgetExceeded as e:
                out.write(json.dumps({"halted": str(e)}) + "\n")
                break
            records.append(rec)
            out.write(json.dumps(rec) + "\n")
            out.flush()
    return records
