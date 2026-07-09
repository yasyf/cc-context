"""Execute runs: one (task, arm, model, repeat) through real `claude -p`, recorded to JSONL.

Each run is spawned by spawnllm (which owns transient-overload retry and resolves every
operational failure into a `Response.error` rather than raising), parsed by cc-transcript into
a PrintResult, then given an integrity verdict and a deterministic grade. Records are appended
to a JSONL file and the raw payload is saved for audit. The safety ceiling is the sole use of
per-run cost: spend accrues after each run and the next run is admitted only while the running
total is under `cfg.safety_ceiling_usd`; cost never enters the reported metrics.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import shutil
from dataclasses import dataclass
from pathlib import Path

import spawnllm
from cc_transcript import PrintResult, parse_print_result

from . import arms, grade, integrity
from .config import BENCH_DIR, Config
from .types import ARMS, Task

TASKS_DIR = BENCH_DIR / "tasks"


def env_fingerprint() -> list[str]:
    """Names (not values, to avoid leaking secrets) of env vars that can change behavior/cost."""
    return sorted(
        k for k in os.environ if k.startswith(("ANTHROPIC_", "CLAUDE_")) or "THINKING" in k or k.startswith("DISABLE_")
    )


def corpus_sha(tasks_dir: Path = TASKS_DIR) -> str:
    """SHA-256 fingerprint of the task corpus, for drift detection between build and report.

    Recipe (report.py must reproduce it byte-for-byte): concatenate the raw bytes of every
    `bench/tasks/*.json` file — non-recursive, so `tasks/patches/` is excluded — in ascending
    filename order and hash the result. Filenames set the order but are not themselves hashed.
    """
    h = hashlib.sha256()
    for p in sorted(tasks_dir.glob("*.json")):
        h.update(p.read_bytes())
    return h.hexdigest()


class CeilingExceeded(Exception):
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
            "arms": list(ARMS),
            "repeats": self.cfg.repeats,
            "max_turns": self.cfg.max_turns,
            "corpus_sha": corpus_sha(),
            "safety_ceiling_usd": self.cfg.safety_ceiling_usd,
            "permission_mode": self.cfg.permission_mode,
            "strip_mcp": self.cfg.strip_mcp,
            "disallowed_tools": list(self.cfg.disallowed_tools),
            "env_fingerprint": env_fingerprint(),
        }
        (self.runs_dir / "meta.json").write_text(json.dumps(meta, indent=2))


def record_from(pr: PrintResult, cfg: Config, task: Task, arm: str, model: str, repeat: int, workdir: Path) -> dict:
    integ = integrity.assess(pr, arm)
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
        "num_turns": pr.num_turns,
        "usage": {
            "input": u.input_tokens,
            "output": u.output_tokens,
            "cache_read": u.cache_read_input_tokens,
            "cache_create_5m": cc_5m,
            "cache_create_1h": cc_1h,
        },
        "guards_active": arms.guards_available(cfg) if arm in arms.CCX_ARMS else None,
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
    """Run one (task, arm, model, repeat). Returns the record; raises CeilingExceeded first.

    `spawnllm.run` owns transient retry and resolves every operational failure — nonzero exit,
    error envelope, timeout, validation — into `resp.error`, never a raise. The full raw event
    stream `parse_print_result` needs lives in `resp.output.raw` on success and failure alike.
    """
    cfg = sess.cfg
    if sess.spent_usd >= cfg.safety_ceiling_usd:
        raise CeilingExceeded(f"spent ${sess.spent_usd:.4f} >= ceiling ${cfg.safety_ceiling_usd:.2f}")

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

    keep = (not record["integrity"]["ok"]) or pr.is_error
    if not keep:
        shutil.rmtree(workdir, ignore_errors=True)
    return record


def _build_plan(cfg: Config, tasks: list[Task]) -> list[tuple[Task, str, str, int]]:
    """Round-robin every (task, arm, model, repeat): all tasks once per (model, repeat) before
    any task repeats, so a ceiling halt samples every task evenly. The arm order rotates by
    repeat — `ARMS[r:] + ARMS[:r]` — so no arm is systematically first and each leads once per
    len(ARMS) repeats; a task's arms stay adjacent for the paired report."""
    n = len(ARMS)
    plan: list[tuple[Task, str, str, int]] = []
    for model in cfg.models:
        for repeat in range(cfg.repeats):
            order = ARMS[repeat % n :] + ARMS[: repeat % n]
            for task in tasks:
                for arm in order:
                    plan.append((task, arm, model, repeat))
    return plan


async def _run_serial(sess: Session, plan: list[tuple[Task, str, str, int]]) -> list[dict]:
    records: list[dict] = []
    with sess.jsonl_path.open("w") as out:
        for task, arm, model, repeat in plan:
            try:
                rec = await run_one(sess, task, arm, model, repeat)
            except CeilingExceeded as e:
                out.write(json.dumps({"halted": str(e)}) + "\n")
                break
            records.append(rec)
            out.write(json.dumps(rec) + "\n")
            out.flush()
    return records


async def _run_bounded(sess: Session, plan: list[tuple[Task, str, str, int]], concurrency: int) -> list[dict]:
    sem = asyncio.Semaphore(concurrency)
    write_lock = asyncio.Lock()
    records: list[dict] = []
    halted: list[str] = []

    with sess.jsonl_path.open("w") as out:

        async def worker(item: tuple[Task, str, str, int]) -> None:
            task, arm, model, repeat = item
            async with sem:
                if halted:
                    return
                try:
                    rec = await run_one(sess, task, arm, model, repeat)
                except CeilingExceeded as e:
                    halted.append(str(e))
                    return
            async with write_lock:
                records.append(rec)
                out.write(json.dumps(rec) + "\n")
                out.flush()

        await asyncio.gather(*(worker(item) for item in plan))
        if halted:
            out.write(json.dumps({"halted": halted[0]}) + "\n")
    return records


async def run_corpus(sess: Session, tasks: list[Task], *, concurrency: int = 1) -> list[dict]:
    """Execute the round-robin plan (see `_build_plan`), appending each record to runs.jsonl.

    `concurrency == 1` (the default) runs strictly serial, byte-identical to the historical
    single-worker loop. `concurrency > 1` bounds in-flight runs with a semaphore; records are
    written as they complete."""
    sess.setup()
    plan = _build_plan(sess.cfg, tasks)
    if concurrency == 1:
        return await _run_serial(sess, plan)
    return await _run_bounded(sess, plan, concurrency)
