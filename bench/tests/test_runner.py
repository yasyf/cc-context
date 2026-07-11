"""Unit tests for the run scheduler: 3-arm rotation, corpus fingerprint, concurrency plumbing.

Run: cd bench && python -m unittest tests.test_runner
"""

from __future__ import annotations

import asyncio
import dataclasses
import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from ccxbench import runner
from ccxbench.config import Config, load
from ccxbench.runner import Session, corpus_sha, run_corpus
from ccxbench.types import ARMS, Grader, Task


def stub_task(tid: str) -> Task:
    return Task(tid, "navigation", "empty", "p", {}, Grader("file_line"), {"file": "a", "line": 1})


def cfg_for(models: list[str], repeats: int, results_dir: Path) -> Config:
    return dataclasses.replace(load(), models=tuple(models), repeats=repeats, results_dir=results_dir)


class TestArmRotation(unittest.TestCase):
    """`_build_plan` rotates the arm order by repeat (ARMS[r:] + ARMS[:r]) so no arm is
    systematically first, while every task appears once per (model, repeat) with its arms
    adjacent."""

    def _order(self, plan: list[tuple], model: str, repeat: int, task_id: str) -> list[str]:
        return [arm for (t, arm, m, r) in plan if t.id == task_id and m == model and r == repeat]

    def test_rotation_matches_table_per_repeat(self) -> None:
        n = len(ARMS)
        cases = [(repeat, list(ARMS[repeat % n :] + ARMS[: repeat % n])) for repeat in range(2 * n + 1)]
        cfg = cfg_for(["m"], len(cases), Path("/unused"))
        plan = runner._build_plan(cfg, [stub_task("a"), stub_task("b")])
        self.assertEqual(len(plan), 2 * n * len(cases))
        for repeat, expected in cases:
            for tid in ("a", "b"):
                self.assertEqual(self._order(plan, "m", repeat, tid), expected, f"repeat {repeat} task {tid}")

    def test_each_arm_leads_once_per_cycle(self) -> None:
        n = len(ARMS)
        cfg = cfg_for(["m"], n, Path("/unused"))
        plan = runner._build_plan(cfg, [stub_task("a")])
        leads = [self._order(plan, "m", r, "a")[0] for r in range(n)]
        self.assertEqual(sorted(leads), sorted(ARMS))

    def test_tasks_once_per_repeat_before_repeating(self) -> None:
        cfg = cfg_for(["m"], 2, Path("/unused"))
        plan = runner._build_plan(cfg, [stub_task(t) for t in ("a", "b", "c")])
        first_r1 = next(i for i, (_t, _a, _m, r) in enumerate(plan) if r == 1)
        self.assertTrue(all(plan[i][3] == 0 for i in range(first_r1)))
        # Corpus order within a repeat, each task once per arm (adjacent block).
        self.assertEqual(
            [t.id for (t, _a, _m, r) in plan if r == 0],
            [tid for tid in ("a", "b", "c") for _ in ARMS],
        )

    def test_arms_adjacent_per_task(self) -> None:
        n = len(ARMS)
        cfg = cfg_for(["m"], 1, Path("/unused"))
        plan = runner._build_plan(cfg, [stub_task(t) for t in ("a", "b")])
        for i in range(0, len(plan), n):
            block = plan[i : i + n]
            self.assertEqual({t.id for (t, _a, _m, _r) in block}, {block[0][0].id})
            self.assertEqual({a for (_t, a, _m, _r) in block}, set(ARMS))


class TestCorpusSha(unittest.TestCase):
    """corpus_sha hashes the sorted `*.json` contents of a tasks dir plus its `patches/*.patch`
    runtime inputs, deterministically."""

    def test_deterministic_and_order_independent(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "b.json").write_text('{"id": "b"}')
            (d / "a.json").write_text('{"id": "a"}')
            self.assertEqual(corpus_sha(d), corpus_sha(d))

    def test_content_edit_changes_sha(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "a.json").write_text('{"id": "a"}')
            before = corpus_sha(d)
            (d / "a.json").write_text('{"id": "a", "x": 1}')
            self.assertNotEqual(before, corpus_sha(d))

    def test_added_task_changes_sha(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "a.json").write_text('{"id": "a"}')
            before = corpus_sha(d)
            (d / "b.json").write_text('{"id": "b"}')
            self.assertNotEqual(before, corpus_sha(d))

    def test_patches_included_but_nested_json_ignored(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "a.json").write_text('{"id": "a"}')
            before = corpus_sha(d)
            (d / "patches").mkdir()
            (d / "patches" / "a.patch").write_text("diff --git")
            # A patch is a runtime input: folding it in changes the digest.
            with_patch = corpus_sha(d)
            self.assertNotEqual(before, with_patch)
            # A nested *.json under patches/ is neither a corpus json (non-recursive) nor a patch.
            (d / "patches" / "nested.json").write_text('{"deep": true}')
            self.assertEqual(with_patch, corpus_sha(d))

    def test_patch_content_edit_changes_sha(self) -> None:
        with TemporaryDirectory() as tmp:
            d = Path(tmp)
            (d / "a.json").write_text('{"id": "a"}')
            (d / "patches").mkdir()
            (d / "patches" / "a.patch").write_text("diff --git a b")
            before = corpus_sha(d)
            (d / "patches" / "a.patch").write_text("diff --git c d")
            self.assertNotEqual(before, corpus_sha(d))


class TestConcurrency(unittest.TestCase):
    """The --concurrency flag threads into run_corpus as a semaphore. Default (1) stays strictly
    serial with records in plan order; higher values bound in-flight runs and cover every run."""

    def _run(self, task_ids: list[str], *, concurrency: int, repeats: int = 1):
        recorded: list[tuple] = []
        peak = {"cur": 0, "max": 0}

        async def fake_run_one(sess, task, arm, model, repeat):
            peak["cur"] += 1
            peak["max"] = max(peak["max"], peak["cur"])
            await asyncio.sleep(0)  # yield so bounded overlap is observable
            recorded.append((task.id, arm, model, repeat))
            peak["cur"] -= 1
            return {"task_id": task.id, "arm": arm, "model": model, "repeat": repeat, "total_cost_usd": 0.0}

        orig = runner.run_one
        runner.run_one = fake_run_one
        try:
            with TemporaryDirectory() as tmp:
                cfg = cfg_for(["m"], repeats, Path(tmp))
                sess = Session(cfg=cfg, session_id="t")
                records = asyncio.run(run_corpus(sess, [stub_task(t) for t in task_ids], concurrency=concurrency))
                jsonl = [json.loads(ln) for ln in sess.jsonl_path.read_text().splitlines() if ln.strip()]
            return recorded, records, jsonl, peak["max"]
        finally:
            runner.run_one = orig

    def test_serial_default_is_plan_order(self) -> None:
        recorded, records, jsonl, peak_max = self._run(["a", "b"], concurrency=1)
        expected = [(t.id, arm, m, r) for (t, arm, m, r) in runner._build_plan(cfg_for(["m"], 1, Path("/x")), [stub_task("a"), stub_task("b")])]
        self.assertEqual(recorded, expected)  # executed in plan order
        self.assertEqual([(j["task_id"], j["arm"], j["model"], j["repeat"]) for j in jsonl], expected)
        self.assertEqual(len(records), len(expected))
        self.assertEqual(peak_max, 1)  # never two in flight

    def test_bounded_concurrency_covers_all_runs(self) -> None:
        recorded, records, jsonl, peak_max = self._run(["a", "b", "c"], concurrency=2)
        expected = {(t.id, arm, m, r) for (t, arm, m, r) in runner._build_plan(cfg_for(["m"], 1, Path("/x")), [stub_task(t) for t in ("a", "b", "c")])}
        self.assertEqual(set(recorded), expected)  # every run executed
        self.assertEqual(len(jsonl), len(expected))  # every run written
        self.assertEqual(peak_max, 2)  # the semaphore permits exactly the cap, not more

    def test_meta_json_records_arms_turns_and_corpus_sha(self) -> None:
        with TemporaryDirectory() as tmp:
            cfg = cfg_for(["m"], 2, Path(tmp))
            sess = Session(cfg=cfg, session_id="s")
            sess.setup(expected_runs=99)
            meta = json.loads((sess.runs_dir / "meta.json").read_text())
        self.assertEqual(meta["arms"], list(ARMS))
        self.assertEqual(meta["max_turns"], cfg.max_turns)
        self.assertEqual(meta["safety_ceiling_usd"], cfg.safety_ceiling_usd)
        self.assertEqual(meta["expected_runs"], 99)
        self.assertEqual(meta["corpus_sha"], corpus_sha())
        self.assertEqual(len(meta["corpus_sha"]), 64)  # sha256 hex

    def test_run_corpus_writes_expected_runs_from_plan(self) -> None:
        async def fake_run_one(sess, task, arm, model, repeat):
            return {"task_id": task.id, "arm": arm, "model": model, "repeat": repeat, "total_cost_usd": 0.0}

        orig = runner.run_one
        runner.run_one = fake_run_one
        try:
            with TemporaryDirectory() as tmp:
                cfg = cfg_for(["m"], 2, Path(tmp))
                sess = Session(cfg=cfg, session_id="e")
                tasks = [stub_task(t) for t in ("a", "b")]
                asyncio.run(run_corpus(sess, tasks))
                meta = json.loads((sess.runs_dir / "meta.json").read_text())
        finally:
            runner.run_one = orig
        # expected_runs == len(plan) == tasks x arms x models x repeats.
        self.assertEqual(meta["expected_runs"], len(tasks) * len(ARMS) * 1 * 2)

    def test_admission_reserves_against_in_flight_spend(self) -> None:
        """Under concurrency, admission reserves the max single-run cost (1.0 before any run
        completes) for every in-flight run plus the candidate, so workers can't all clear a stale
        ceiling check. With ceiling 3.5 and reservation 1.0, at most 3 start before the halt:
        (in_flight+1)*1.0 < 3.5 admits while in_flight is 0/1/2, then in_flight=3 halts."""
        peak = {"cur": 0, "max": 0}

        async def fake_run_one(sess, task, arm, model, repeat):
            peak["cur"] += 1
            peak["max"] = max(peak["max"], peak["cur"])
            await asyncio.sleep(0.05)  # hold so concurrent admission is observable
            peak["cur"] -= 1
            return {"task_id": task.id, "arm": arm, "model": model, "repeat": repeat, "total_cost_usd": 1.0}

        orig = runner.run_one
        runner.run_one = fake_run_one
        try:
            with TemporaryDirectory() as tmp:
                cfg = dataclasses.replace(cfg_for(["m"], 1, Path(tmp)), safety_ceiling_usd=3.5)
                sess = Session(cfg=cfg, session_id="adm")
                tasks = [stub_task(t) for t in ("a", "b", "c", "d")]  # 4 x 3 arms = 12 runs
                records = asyncio.run(run_corpus(sess, tasks, concurrency=8))
                jsonl = [json.loads(ln) for ln in sess.jsonl_path.read_text().splitlines() if ln.strip()]
        finally:
            runner.run_one = orig
        self.assertEqual(peak["max"], 3)  # not 8 (the semaphore cap) — admission bounds it
        self.assertEqual(len(records), 3)  # only the admitted runs completed
        self.assertTrue(any("halted" in j for j in jsonl))  # a halt marker was written


if __name__ == "__main__":
    unittest.main()
