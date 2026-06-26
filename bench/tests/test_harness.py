"""Unit tests for the benchmark harness internals (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import asyncio
import dataclasses
import json
import re
import tempfile
import unittest
from pathlib import Path

from cc_transcript import parse_print_result
from cc_transcript.cost import cost_of

from ccxbench import integrity, taskgen
from ccxbench.__main__ import recompute_lc_predicate
from ccxbench.config import load
from ccxbench.cost import crosscheck
from ccxbench.grade import grade, synthetic_result
from ccxbench.graders import GradeContext, grade_file_line, grade_keywords, grade_set_match
from ccxbench.report import paired_task_ids, render
from ccxbench.runner import Session, run_corpus
from ccxbench.types import Grader, Task

DATA = Path(__file__).resolve().parent / "data"


def pr_from(messages: list[dict]):
    return parse_print_result(json.dumps(messages).encode())


def result_msg(**over: object) -> dict:
    base = {
        "type": "result",
        "is_error": False,
        "result": "ok",
        "structured_output": {"file": "a"},
        "total_cost_usd": 0.01,
        "num_turns": 1,
        "session_id": "test",
        "usage": {
            "input_tokens": 1,
            "output_tokens": 1,
            "cache_read_input_tokens": 0,
            "cache_creation_input_tokens": 0,
            "cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": 0},
        },
        "modelUsage": {},
        "permission_denials": [],
    }
    base.update(over)
    return base


def init_msg(mcp: list[str]) -> dict:
    return {
        "type": "system",
        "subtype": "init",
        "mcp_servers": [{"name": m, "status": "connected"} for m in mcp],
        "plugins": [],
        "tools": ["Bash", "Read"],
        "skills": [],
    }


def tool_use(name: str, tool_input: dict, _id: str = "t1") -> dict:
    return {
        "type": "assistant",
        "session_id": "test",
        "message": {"content": [{"type": "tool_use", "id": _id, "name": name, "input": tool_input}]},
    }


def bash(cmd: str) -> dict:
    return tool_use("Bash", {"command": cmd})


def mcp_call(name: str) -> dict:
    return tool_use(name, {})


def tool_err(text: str) -> dict:
    return {
        "type": "user",
        "session_id": "test",
        "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "is_error": True, "content": text}]},
    }


class TestPrintResult(unittest.TestCase):
    def test_parse_real_haiku_envelope(self) -> None:
        pr = parse_print_result((DATA / "haiku_envelope.json").read_bytes())
        self.assertFalse(pr.is_error)
        self.assertEqual(pr.structured_output, {"answer": "pong"})
        self.assertAlmostEqual(pr.total_cost_usd, 0.05759, places=5)
        self.assertEqual(pr.usage.cache_creation.ephemeral_1h_input_tokens, 25605)
        self.assertIn("claude-haiku-4-5-20251001", pr.model_usage)


class TestCost(unittest.TestCase):
    def test_crosscheck_matches_real_bill(self) -> None:
        cfg = load()
        pr = parse_print_result((DATA / "haiku_envelope.json").read_bytes())
        cc = crosscheck(pr, "haiku", cfg.cost_tolerance)
        self.assertTrue(cc.within_tolerance, cc.note)
        self.assertLess(cc.rel_delta, 0.01)

    def test_cost_of_exact_to_the_cent(self) -> None:
        pr = parse_print_result((DATA / "haiku_envelope.json").read_bytes())
        # cost_of accepts either the family alias or the full model id (it resolves the family).
        self.assertAlmostEqual(cost_of(pr.usage, "haiku").total, 0.05759, places=5)
        self.assertAlmostEqual(cost_of(pr.usage, "claude-haiku-4-5-20251001").total, 0.05759, places=5)

    def test_no_spurious_premium_note_when_tier_geo_absent(self) -> None:
        # service_tier / inference_geo are None when the keys are absent; a None must not be
        # mis-attributed as a non-standard tier / inference-geo surcharge.
        pr = synthetic_result({"a": 1})
        cc = crosscheck(pr, "haiku", 0.02)
        self.assertNotIn("not modeled: None", cc.note)
        self.assertNotIn("service tier", cc.note)
        self.assertNotIn("inference_geo", cc.note)


class TestIntegrity(unittest.TestCase):
    def test_ccx_arm_facade_used(self) -> None:
        pr = pr_from([init_msg(["cc-context"]), mcp_call("mcp__cc-context__outline"), result_msg()])
        v = integrity.assess(pr, "ccx")
        self.assertTrue(v.ok)
        self.assertTrue(v.ccx_used)

    def test_ccx_arm_bash_ccx_used(self) -> None:
        pr = pr_from([init_msg(["cc-context"]), bash("ccx outline internal/x.go"), result_msg()])
        v = integrity.assess(pr, "ccx")
        self.assertTrue(v.ccx_used)
        self.assertEqual(v.ccx_calls, ["bash:ccx outline"])

    def test_ccx_arm_guard_fired(self) -> None:
        pr = pr_from(
            [init_msg(["cc-context"]), bash("cat internal/x.go"), tool_err("Blocked: use `ccx outline` instead"), result_msg()]
        )
        v = integrity.assess(pr, "ccx")
        self.assertTrue(v.guard_fired)
        self.assertTrue(v.ok)

    def test_ccx_arm_mislabeled_when_unused(self) -> None:
        pr = pr_from([init_msg(["cc-context"]), bash("echo hi"), result_msg()])
        v = integrity.assess(pr, "ccx")
        self.assertFalse(v.ok)

    def test_baseline_clean(self) -> None:
        pr = pr_from([init_msg([]), bash("rg foo"), result_msg()])
        v = integrity.assess(pr, "baseline")
        self.assertTrue(v.ok)
        self.assertFalse(v.ccx_used)

    def test_baseline_leak_detected(self) -> None:
        pr = pr_from([init_msg(["cc-context"]), result_msg()])
        v = integrity.assess(pr, "baseline")
        self.assertFalse(v.ok)

    def test_heavy_call_classified(self) -> None:
        pr = pr_from([init_msg([]), bash("git diff HEAD~1"), result_msg()])
        v = integrity.assess(pr, "baseline")
        self.assertIn("git-diff", v.native_heavy_calls)

    def test_ccx_arm_guard_fired_via_permission_denials(self) -> None:
        # A denied heavy primitive recorded only in permission_denials (no is_error tool_result)
        # must still count as a ccx-navigation guard fire — detected structurally, not via the
        # deny reason (which the denial record does not carry).
        denial = {"tool_name": "Bash", "tool_use_id": "t1", "tool_input": {"command": "find . -name mux.go -type f"}}
        pr = pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])])
        v = integrity.assess(pr, "ccx")
        self.assertTrue(v.guard_fired)
        self.assertTrue(v.ok)

    def test_ccx_arm_non_navigation_denial_not_a_guard_fire(self) -> None:
        # A capt-hook built-in denial (e.g. styleguide on a Write) is NOT a ccx-navigation guard,
        # so it must not falsely validate a ccx run where ccx was never exercised.
        denial = {"tool_name": "Write", "tool_use_id": "t1", "tool_input": {"file_path": "m.py", "content": "x: Any = 1\n"}}
        pr = pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])])
        v = integrity.assess(pr, "ccx")
        self.assertFalse(v.guard_fired)
        self.assertFalse(v.ok)


class TestGraders(unittest.TestCase):
    def test_file_line_tolerance(self) -> None:
        spec = {"line_tolerance": 2}
        gold = {"file": "internal/calc/calc.go", "line": 15}
        ctx = GradeContext("", None)
        self.assertTrue(grade_file_line({"file": "internal/calc/calc.go", "line": 16}, gold, spec, ctx).correct)
        self.assertFalse(grade_file_line({"file": "internal/calc/calc.go", "line": 20}, gold, spec, ctx).correct)
        self.assertFalse(grade_file_line({"file": "other.go", "line": 15}, gold, spec, ctx).correct)

    def test_set_match_equal_normalizes(self) -> None:
        spec = {"field": "callees", "mode": "equal", "lower": True}
        gold = {"callees": ["Add", "Double"]}
        ctx = GradeContext("", None)
        self.assertTrue(grade_set_match({"callees": ["double", "add"]}, gold, spec, ctx).correct)
        self.assertFalse(grade_set_match({"callees": ["add"]}, gold, spec, ctx).correct)

    def test_errored_run_is_incorrect(self) -> None:
        task = Task("t", "navigation", "fixture", "p", {}, Grader("file_line"), {"file": "a", "line": 1})
        pr = synthetic_result({"file": "a", "line": 1}, is_error=True)
        self.assertFalse(grade(task, pr, None).correct)

    def test_set_match_superset(self) -> None:
        spec = {"field": "callees", "mode": "superset", "lower": True}
        gold = {"callees": ["Add", "Double"]}
        ctx = GradeContext("", None)
        self.assertTrue(grade_set_match({"callees": ["add", "double", "extra"]}, gold, spec, ctx).correct)
        self.assertFalse(grade_set_match({"callees": ["add"]}, gold, spec, ctx).correct)

    def test_keyword_groups_all_of_any_of(self) -> None:
        spec = {"field": "answer"}
        gold = {"groups": [["sort"], ["half", "halve", "middle"]]}
        ctx = GradeContext("", None)
        self.assertTrue(grade_keywords({"answer": "keep the sorted array, inspect the middle"}, gold, spec, ctx).correct)
        self.assertFalse(grade_keywords({"answer": "I have no idea, sorry"}, gold, spec, ctx).correct)
        self.assertFalse(grade_keywords({"answer": "it must be sorted"}, gold, spec, ctx).correct)


class TestAnswerKeyAndPairing(unittest.TestCase):
    def test_integrity_flags_answer_key_bash(self) -> None:
        pr = pr_from([init_msg([]), bash("cat manifest.json"), result_msg()])
        v = integrity.assess(pr, "baseline")
        self.assertFalse(v.ok)
        self.assertIn("ANSWER KEY", v.note)

    def test_integrity_flags_answer_key_read_tool(self) -> None:
        read = tool_use("Read", {"file_path": "/x/manifest.json"})
        pr = pr_from([init_msg(["cc-context"]), read, mcp_call("mcp__cc-context__ccx_symbol"), result_msg()])
        v = integrity.assess(pr, "ccx")
        self.assertFalse(v.ok)

    def test_paired_task_ids(self) -> None:
        recs = [
            {"model": "m", "arm": "baseline", "task_id": "a"},
            {"model": "m", "arm": "ccx", "task_id": "a"},
            {"model": "m", "arm": "baseline", "task_id": "b"},
        ]
        both, dropped = paired_task_ids(recs, "m")
        self.assertEqual(both, ["a"])
        self.assertEqual(dropped, 1)


def stub_task(tid: str) -> Task:
    return Task(tid, "navigation", "fixture", "p", {}, Grader("file_line"), {"file": "a", "line": 1})


class TestRoundRobinOrder(unittest.TestCase):
    """run_corpus must visit every task once per (model, repeat) before any task repeats,
    keeping the per-repeat arm interleave and the adjacent (baseline, ccx) pair per task."""

    def _plan(self, task_ids: list[str], models: list[str], repeats: int) -> list[tuple]:
        cfg = load()
        recorded: list[tuple] = []

        async def fake_run_one(sess, task, arm, model, repeat):
            recorded.append((task.id, arm, model, repeat))
            return {"task_id": task.id, "arm": arm, "model": model, "repeat": repeat}

        import ccxbench.runner as runner

        orig = runner.run_one
        runner.run_one = fake_run_one
        try:
            with tempfile.TemporaryDirectory() as tmp:
                cfg2 = dataclasses.replace(
                    cfg, models=tuple(models), repeats=repeats, results_dir=Path(tmp)
                )
                sess = Session(cfg=cfg2, session_id="t")
                asyncio.run(run_corpus(sess, [stub_task(t) for t in task_ids]))
        finally:
            runner.run_one = orig
        return recorded

    def test_all_tasks_once_per_repeat_before_repeating(self) -> None:
        plan = self._plan(["a", "b", "c"], ["m"], repeats=2)
        # 3 tasks x 2 arms x 2 repeats = 12 runs.
        self.assertEqual(len(plan), 12)
        # First 6 runs (repeat 0) must cover every task once before repeat 1 starts.
        repeat0_tasks = [tid for (tid, _arm, _m, rep) in plan if rep == 0]
        self.assertEqual(sorted(set(repeat0_tasks)), ["a", "b", "c"])
        # The first time we see repeat==1 must come AFTER all repeat==0 runs.
        first_r1 = next(i for i, (_t, _a, _m, rep) in enumerate(plan) if rep == 1)
        self.assertTrue(all(plan[i][3] == 0 for i in range(first_r1)))
        # Task order within a repeat is the corpus order, each task twice (its two arms).
        self.assertEqual([t for (t, _a, _m, rep) in plan if rep == 0], ["a", "a", "b", "b", "c", "c"])

    def test_arm_interleave_preserved(self) -> None:
        plan = self._plan(["a", "b"], ["m"], repeats=2)
        # repeat 0 leads with baseline, repeat 1 leads with ccx (cache-fairness flip).
        r0 = [(t, a) for (t, a, _m, rep) in plan if rep == 0]
        r1 = [(t, a) for (t, a, _m, rep) in plan if rep == 1]
        self.assertEqual(r0, [("a", "baseline"), ("a", "ccx"), ("b", "baseline"), ("b", "ccx")])
        self.assertEqual(r1, [("a", "ccx"), ("a", "baseline"), ("b", "ccx"), ("b", "baseline")])

    def test_paired_arms_adjacent_per_task(self) -> None:
        plan = self._plan(["a", "b", "c"], ["m"], repeats=1)
        # Each task's baseline and ccx runs must sit next to each other.
        for i in range(0, len(plan), 2):
            self.assertEqual(plan[i][0], plan[i + 1][0])
            self.assertEqual({plan[i][1], plan[i + 1][1]}, {"baseline", "ccx"})


class TestIntegrityExclusion(unittest.TestCase):
    """Mislabeled runs (integrity.ok == False) are dropped from the paired aggregate and the
    headline, but still listed (and counted) in the integrity section."""

    def _rec(self, tid: str, arm: str, correct: bool, cost: float, ok: bool) -> dict:
        return {
            "task_id": tid,
            "category": "navigation",
            "arm": arm,
            "model": "m",
            "repeat": 0,
            "correct": correct,
            "total_cost_usd": cost,
            "num_turns": 1,
            "cost_ok": True,
            "usage": {"input": 1, "output": 1, "cache_read": 0, "cache_create_5m": 0, "cache_create_1h": 0},
            "integrity": {"ok": ok, "note": "mislabeled" if not ok else ""},
        }

    def test_paired_task_ids_drops_mislabeled(self) -> None:
        recs = [
            self._rec("a", "baseline", True, 0.01, ok=True),
            self._rec("a", "ccx", True, 0.01, ok=True),
            self._rec("b", "baseline", True, 0.01, ok=True),
            self._rec("b", "ccx", True, 0.01, ok=False),  # ccx side mislabeled
        ]
        ok_records = [r for r in recs if r["integrity"]["ok"]]
        both, _dropped = paired_task_ids(ok_records, "m")
        self.assertEqual(both, ["a"])  # b excluded: its only ccx run was mislabeled

    def test_render_excludes_mislabeled_and_reports_count(self) -> None:
        recs = [
            self._rec("a", "baseline", True, 0.01, ok=True),
            self._rec("a", "ccx", True, 0.02, ok=True),
            self._rec("b", "baseline", True, 0.01, ok=True),
            self._rec("b", "ccx", False, 0.50, ok=False),  # mislabeled, would skew cost
        ]
        md = render(recs, "sess")
        # The mislabeled run is still listed in the integrity section.
        self.assertIn("integrity failures (arm mislabeled): **1**", md)
        self.assertIn("`b` [ccx]", md)


class FakeCounter:
    """A deterministic stand-in for tokens.TokenCounter (no network)."""

    def count(self, text: str) -> int:
        return len(text) // 4


def assistant_turn(prompt_tokens: int) -> dict:
    """One assistant event whose usage gives the turn this prompt high-water."""
    return {
        "type": "assistant",
        "message": {
            "content": [{"type": "text", "text": "ok"}],
            "usage": {
                "input_tokens": prompt_tokens,
                "cache_creation_input_tokens": 0,
                "cache_read_input_tokens": 0,
            },
        },
    }


class TestHighWaterRender(unittest.TestCase):
    """render reconstructs paired high-water savings from hand-built transcripts with known
    per-turn usage, lists every both-correct task in the waterfall, and degrades gracefully
    (correctness panel only) when no transcripts are available."""

    H = {  # (task, arm) -> high-water prompt tokens
        ("t1", "baseline"): 1000,
        ("t1", "ccx"): 600,
        ("t2", "baseline"): 2000,
        ("t2", "ccx"): 1000,
    }

    def _rec(self, tid: str, arm: str) -> dict:
        return {
            "task_id": tid,
            "category": "navigation",
            "arm": arm,
            "model": "m",
            "repeat": 0,
            "correct": True,
            "total_cost_usd": 0.01,
            "num_turns": 1,
            "cost_ok": True,
            "usage": {"input": 1, "output": 1, "cache_read": 0, "cache_create_5m": 0, "cache_create_1h": 0},
            "integrity": {"ok": True, "note": ""},
        }

    def _setup_raw(self, raw_dir: Path) -> None:
        raw_dir.mkdir(parents=True, exist_ok=True)
        for (tid, arm), hw in self.H.items():
            events = [assistant_turn(hw)]
            (raw_dir / f"{tid}__{arm}__m__r0.json").write_text(json.dumps(events))

    def test_high_water_headline_and_waterfall(self) -> None:
        recs = [self._rec(t, a) for t in ("t1", "t2") for a in ("baseline", "ccx")]
        with tempfile.TemporaryDirectory() as tmp:
            raw = Path(tmp) / "raw"
            self._setup_raw(raw)
            md = render(
                recs,
                "sess",
                raw_dir=raw,
                prompts={"t1": "p1", "t2": "p2"},
                counter=FakeCounter(),
            )
        # Both tasks save (ccx H < baseline H): savings sign is positive.
        self.assertIn("High-water headline", md)
        self.assertIn("Mean high-water savings: **+", md)
        # Win/loss/tie: ccx wins both.
        self.assertIn("**2 / 0 / 0**", md)
        # Waterfall lists both tasks.
        self.assertIn("`t1`", md)
        self.assertIn("`t2`", md)
        # Correctness panel renders with both arms at 100%.
        self.assertIn("Correctness panel", md)
        self.assertIn("baseline accuracy: **100.0%**", md)
        self.assertIn("ccx accuracy: **100.0%**", md)

    def test_render_without_raw_dir_degrades(self) -> None:
        recs = [self._rec(t, a) for t in ("t1", "t2") for a in ("baseline", "ccx")]
        md = render(recs, "sess")
        self.assertIn("no transcripts available", md.lower())
        self.assertIn("Correctness panel", md)
        self.assertNotIn("High-water headline", md)


class TestLargeContextBuilder(unittest.TestCase):
    """Every gold member of a large_context task is a real declaration in its fixture file AND
    satisfies the task's predicate (recomputed independently, mirroring verify_oss), and the
    naive grep a frugal baseline would run does NOT isolate the gold — the tasks are not
    grep-defeatable."""

    def test_gold_members_present_and_predicate_holds(self) -> None:
        cfg = load()
        tasks = taskgen.large_context_tasks()
        self.assertEqual(
            [t.id for t in tasks],
            ["click-enum-get-command-classes", "mux-enum-addmatcher-callers", "mux-enum-matcher-impls"],
        )
        for t in tasks:
            self.assertEqual(t.category, "large_context")
            field = t.grader.spec["field"]
            gold = t.gold[field]
            self.assertEqual(len(gold), len(set(gold)), f"{t.id} gold has duplicates")
            for rel, decl in t.gold["verify_decls"]:
                path = cfg.fixtures_root / t.repo / rel
                self.assertTrue(path.exists(), f"{t.id}: {rel} missing")
                self.assertIn(decl, path.read_text(), f"{t.id}: decl {decl!r} absent from {rel}")
            for member in gold:
                pat = re.compile(rf"\b{re.escape(member)}\b")
                self.assertTrue(
                    any(pat.search(decl) for _rel, decl in t.gold["verify_decls"]),
                    f"{t.id}: gold member {member!r} has no decl",
                )
            # Independently recompute the predicate from the fixtures and assert it equals gold.
            recomputed = recompute_lc_predicate(cfg.fixtures_root / t.repo, t.gold["lc_predicate"], t.repo)
            self.assertEqual(
                {m.lower() for m in recomputed},
                {m.lower() for m in gold},
                f"{t.id}: predicate recompute {sorted(recomputed)} != gold {sorted(gold)}",
            )

    def test_builder_gold_grades_correct(self) -> None:
        for t in taskgen.large_context_tasks():
            field = t.grader.spec["field"]
            ctx = GradeContext("", None)
            good = grade_set_match({field: list(t.gold[field])}, t.gold, t.grader.spec, ctx)
            self.assertTrue(good.correct, f"{t.id}: gold answer graded incorrect: {good.detail}")
            bad = grade_set_match({field: ["NotAReal"]}, t.gold, t.grader.spec, ctx)
            self.assertFalse(bad.correct, f"{t.id}: wrong answer graded correct")
            # An incomplete answer (gold minus one member) must also grade incorrect.
            partial = list(t.gold[field])[:-1]
            incomplete = grade_set_match({field: partial}, t.gold, t.grader.spec, ctx)
            self.assertFalse(incomplete.correct, f"{t.id}: incomplete answer graded correct")

    def test_naive_grep_does_not_equal_gold(self) -> None:
        """Encode un-shortcuttability as a regression: the obvious one-liner a frugal baseline
        runs over-/under-matches, so its result is NOT the gold set."""
        cfg = load()
        tasks = {t.id: t for t in taskgen.large_context_tasks()}

        # Flavor 1: `grep '^class '` over core.py yields ALL public classes, not just the 3
        # that define get_command.
        t1 = tasks["click-enum-get-command-classes"]
        core = (cfg.fixtures_root / "click" / "src/click/core.py").read_text()
        grep_classes = {
            m.group(1)
            for line in core.splitlines()
            if (m := re.match(r"class (\w+)", line)) and not m.group(1).startswith("_")
        }
        gold1 = set(t1.gold["classes"])
        self.assertNotEqual(grep_classes, gold1)
        self.assertTrue(gold1 < grep_classes, "gold must be a strict subset the grep over-matches")

        # Flavor 3: a frugal `grep matcher` scrapes interface/field/helper tokens, not the
        # implementer type names — Go interfaces are implicit, so it both misses implementers
        # whose name lacks "matcher" (Route, Router, routeRegexp) and over-includes non-types
        # (addMatcher, matchers, the interface itself).
        t3 = tasks["mux-enum-matcher-impls"]
        muxdir = cfg.fixtures_root / "gorilla-mux"
        grep_matcher_tokens = set()
        for go in muxdir.glob("*.go"):
            if go.name.endswith("_test.go"):
                continue
            grep_matcher_tokens |= set(re.findall(r"\b(\w*[Mm]atcher\w*)\b", go.read_text()))
        gold3 = set(t3.gold["types"])
        self.assertNotEqual(grep_matcher_tokens, gold3)
        self.assertTrue(gold3 - grep_matcher_tokens, "grep `matcher` must miss some implementers")
        self.assertTrue(grep_matcher_tokens - gold3, "grep `matcher` must over-match non-types")


class TestStructuredRun(unittest.TestCase):
    """spawnllm.run over a RunSpec.schema must inject the task's JSON-Schema dict as
    --json-schema, expose the FULL raw event stream on resp.output.raw (success and failure
    alike), and retry a transient failure before resolving — faked at the acapture_cli seam,
    not a real CLI."""

    def _spec(self, schema: dict | None = None):
        from spawnllm import ClaudeConfig, RunSpec

        return RunSpec(
            prompt="p",
            model="sonnet",
            schema=schema,
            max_attempts=3,
            timeout=5,
            provider_configs={"claude": ClaudeConfig()},
        )

    def test_injects_schema_and_keeps_full_output_raw(self) -> None:
        import spawnllm
        from spawnllm.backends import base
        from spawnllm.backends.claude import ClaudeCliBackend
        from spawnllm.proc import RunResult

        schema = {"type": "object", "properties": {"file": {"type": "string"}}}
        full_stream = json.dumps([result_msg(structured_output={"file": "a"})])
        seen: dict = {}

        async def fake_acapture(argv, **kw):
            seen["argv"], seen["kw"] = argv, kw
            return RunResult(full_stream, "", 0)

        orig = base.acapture_cli
        base.acapture_cli = fake_acapture
        try:
            resp = asyncio.run(spawnllm.run(self._spec(schema), backend=ClaudeCliBackend()))
        finally:
            base.acapture_cli = orig

        # The full event-stream array (what parse_print_result needs) survives on output.raw,
        # not the collapsed result text.
        self.assertEqual(resp.output.raw, full_stream)
        self.assertIsNone(resp.error)
        self.assertIn("--json-schema", seen["argv"])
        self.assertEqual(seen["argv"][seen["argv"].index("--json-schema") + 1], json.dumps(schema))
        # A schema run forces --output-format json so the result envelope is machine-readable.
        self.assertIn("--output-format", seen["argv"])
        self.assertEqual(seen["argv"][seen["argv"].index("--output-format") + 1], "json")
        self.assertEqual(seen["kw"]["input"], "p")

    def test_retries_transient_then_succeeds(self) -> None:
        import sys

        import spawnllm
        from spawnllm.backends import base
        from spawnllm.backends.claude import ClaudeCliBackend
        from spawnllm.proc import RunResult

        run_mod = sys.modules["spawnllm.run"]
        done = json.dumps([result_msg()])
        outcomes = [RunResult("", "Overloaded (529)", 1), RunResult(done, "", 0)]

        async def fake_acapture(argv, **kw):
            return outcomes.pop(0)

        orig_cap, orig_backoff = base.acapture_cli, run_mod.backoff
        base.acapture_cli = fake_acapture
        run_mod.backoff = lambda _attempt: 0.0
        try:
            resp = asyncio.run(spawnllm.run(self._spec(), backend=ClaudeCliBackend()))
        finally:
            base.acapture_cli, run_mod.backoff = orig_cap, orig_backoff

        self.assertIsNone(resp.error)
        self.assertEqual(resp.output.raw, done)
        self.assertEqual(outcomes, [])

    def test_timeout_resolves_to_error_not_raise(self) -> None:
        import spawnllm
        from spawnllm.backends import base
        from spawnllm.backends.claude import ClaudeCliBackend

        async def fake_acapture(argv, **kw):
            raise TimeoutError("slow")

        orig = base.acapture_cli
        base.acapture_cli = fake_acapture
        try:
            resp = asyncio.run(spawnllm.run(self._spec(), backend=ClaudeCliBackend()))
        finally:
            base.acapture_cli = orig

        self.assertIsNone(resp.result)
        self.assertIsNotNone(resp.error)
        self.assertIsInstance(resp.error.ex, TimeoutError)


if __name__ == "__main__":
    unittest.main()
