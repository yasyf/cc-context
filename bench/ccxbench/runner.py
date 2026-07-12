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
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path

import spawnllm
from cc_transcript import ModelUsage, PrintResult, parse_print_result

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

    Recipe (report.py must reproduce it byte-for-byte): in ascending relative-path order,
    concatenate the raw bytes of every `bench/tasks/*.json` AND every `bench/tasks/patches/*.patch`
    — the diff patches are runtime inputs, so they are folded in — then hash the result. The
    relative paths set the order but are not themselves hashed.
    """
    h = hashlib.sha256()
    files = list(tasks_dir.glob("*.json")) + list((tasks_dir / "patches").glob("*.patch"))
    for p in sorted(files, key=lambda p: p.relative_to(tasks_dir).as_posix()):
        h.update(p.read_bytes())
    return h.hexdigest()


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

    def setup(self, expected_runs: int) -> None:
        (self.runs_dir / "raw").mkdir(parents=True, exist_ok=True)
        # Fail loud now if the pinned ccx binary is missing, and stand up the baseline `ccx`-not-found
        # shim, before any run spends money against a mis-pinned or contaminating environment.
        ccx_version = arms.validate_ccx_bin(self.cfg)
        shim = arms.ensure_baseline_shim(self.runs_dir)
        meta = {
            "session_id": self.session_id,
            "models": list(self.cfg.models),
            "arms": list(ARMS),
            "repeats": self.cfg.repeats,
            "expected_runs": expected_runs,
            "max_turns": self.cfg.max_turns,
            "corpus_sha": corpus_sha(),
            "ccx_version": ccx_version,
            "safety_ceiling_usd": self.cfg.safety_ceiling_usd,
            "permission_mode": self.cfg.permission_mode,
            "strip_mcp": self.cfg.strip_mcp,
            "disallowed_tools": list(self.cfg.disallowed_tools),
            "env_fingerprint": env_fingerprint(),
            "run_path": {arm: arms.run_path(self.cfg, ccx=arm in arms.CCX_ARMS, shim_dir=shim) for arm in ARMS},
        }
        (self.runs_dir / "meta.json").write_text(json.dumps(meta, indent=2))


def main_model_usage(model_usage: Mapping[str, ModelUsage], model: str) -> ModelUsage:
    """The billed per-model usage for the run's main model — the entry whose id carries `model`.

    T is sourced from the per-model totals (retry-INCLUSIVE; they reconstruct `total_cost_usd`), not
    the result envelope's top-level usage, which drops retried/superseded calls that were still
    billed. Fails loud when the main model is missing or ambiguous; the haiku title helper is
    excluded because its id carries no main-model alias.
    """
    matching = [mu for mid, mu in model_usage.items() if model in mid]
    if len(matching) != 1:
        raise ValueError(f"expected exactly one model_usage entry matching {model!r}, got {sorted(model_usage)}")
    return matching[0]


def record_from(pr: PrintResult, cfg: Config, task: Task, arm: str, model: str, repeat: int, workdir: Path) -> dict:
    integ = integrity.assess(pr, arm, task.category)
    graded = grade.grade(task, pr, workdir)
    mu = main_model_usage(pr.model_usage, model)
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
            "input": mu.input_tokens,
            "output": mu.output_tokens,
            "cache_read": mu.cache_read_input_tokens,
            "cache_create": mu.cache_creation_input_tokens,
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
        "model_ids": [],
        "repeat": repeat,
        "ccx_helps": task.ccx_helps,
        "is_error": True,
        "correct": False,
        "grade_detail": reason,
        "total_cost_usd": 0.0,
        "harness_error": reason,
    }


async def run_one(sess: Session, task: Task, arm: str, model: str, repeat: int) -> dict:
    """Run one (task, arm, model, repeat) through claude and return its record.

    `spawnllm.run` owns transient retry and resolves every operational failure — nonzero exit,
    error envelope, timeout, validation — into `resp.error`, never a raise. The full raw event
    stream `parse_print_result` needs lives in `resp.output.raw` on success and failure alike.
    Ceiling admission and spend accounting are the caller's job (`_run_serial`/`_run_bounded`).
    """
    cfg = sess.cfg
    run_id = f"{task.id}__{arm}__{model}__r{repeat}"
    workdir = arms.prepare_workdir(cfg, task, arm, run_id)
    spec = arms.build_run_spec(cfg, task, arm, model, workdir, shim_dir=arms.baseline_shim_dir(sess.runs_dir))
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

    keep = (not record["integrity"]["ok"]) or pr.is_error
    if not keep:
        shutil.rmtree(workdir, ignore_errors=True)
    return record


def _build_plan(cfg: Config, tasks: list[Task]) -> list[tuple[Task, str, str, int]]:
    """Round-robin every (task, arm, model, repeat): all tasks once per (model, repeat) before
    any task repeats, so a ceiling halt samples every task evenly. The arm order rotates by
    repeat — `ARMS[r:] + ARMS[:r]` — so no arm is systematically first and each leads once per
    len(ARMS) repeats; a task's arms stay adjacent for the paired report. With repeats=5 and 3
    arms the rotation doesn't divide evenly, so lead counts land at 2/2/1 per task — negligible
    and accepted (the paired delta cancels arm-order effects)."""
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
    ceiling = sess.cfg.safety_ceiling_usd
    with sess.jsonl_path.open("w") as out:
        for task, arm, model, repeat in plan:
            if sess.spent_usd >= ceiling:
                out.write(json.dumps({"halted": f"spent ${sess.spent_usd:.4f} >= ceiling ${ceiling:.2f}"}) + "\n")
                break
            rec = await run_one(sess, task, arm, model, repeat)
            sess.spent_usd += rec["total_cost_usd"]
            records.append(rec)
            out.write(json.dumps(rec) + "\n")
            out.flush()
    return records


async def _run_bounded(sess: Session, plan: list[tuple[Task, str, str, int]], concurrency: int) -> list[dict]:
    sem = asyncio.Semaphore(concurrency)
    write_lock = asyncio.Lock()
    admit_lock = asyncio.Lock()
    ceiling = sess.cfg.safety_ceiling_usd
    records: list[dict] = []
    halted: list[str] = []
    # Admission accounting: every in-flight run reserves the max single-run cost seen so far
    # (1.0 USD until the first run completes). A run is admitted only while spent + the reservations
    # for the in-flight runs plus this candidate stays under the ceiling, so N workers can't all
    # clear a stale check.
    state = {"in_flight": 0, "max_single": 1.0, "any_done": False}

    with sess.jsonl_path.open("w") as out:

        async def worker(item: tuple[Task, str, str, int]) -> None:
            task, arm, model, repeat = item
            async with sem:
                if halted:
                    return
                async with admit_lock:
                    # Reserve for the in-flight runs AND this candidate's own run, so admitting it
                    # can't push projected spend past the ceiling.
                    if sess.spent_usd + (state["in_flight"] + 1) * state["max_single"] >= ceiling:
                        halted.append(
                            f"spent ${sess.spent_usd:.4f} + {state['in_flight'] + 1} reservation(s) >= ceiling ${ceiling:.2f}"
                        )
                        return
                    state["in_flight"] += 1
                rec = await run_one(sess, task, arm, model, repeat)
                cost = rec["total_cost_usd"]
                async with admit_lock:
                    sess.spent_usd += cost
                    state["max_single"] = cost if not state["any_done"] else max(state["max_single"], cost)
                    state["any_done"] = True
                    state["in_flight"] -= 1
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
    single-worker loop. `concurrency > 1` bounds in-flight runs with a semaphore and admits
    each only when spend plus live reservations clears the ceiling; records are written as they
    complete."""
    plan = _build_plan(sess.cfg, tasks)
    sess.setup(expected_runs=len(plan))
    if concurrency == 1:
        return await _run_serial(sess, plan)
    return await _run_bounded(sess, plan, concurrency)
