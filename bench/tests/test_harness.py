"""Unit tests for the benchmark harness internals (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from ccxbench import integrity
from ccxbench.config import load
from ccxbench.cost import crosscheck
from ccxbench.envelope import Envelope, parse
from ccxbench.grade import grade
from ccxbench.graders import GradeContext, grade_file_line, grade_keywords, grade_set_match
from ccxbench.report import paired_task_ids, verdict
from ccxbench.types import Grader, Task

DATA = Path(__file__).resolve().parent / "data"


def env_from(messages: list[dict]) -> Envelope:
    return parse(json.dumps(messages))


def result_msg(**over: object) -> dict:
    base = {
        "type": "result",
        "is_error": False,
        "result": "ok",
        "structured_output": {"file": "a"},
        "total_cost_usd": 0.01,
        "num_turns": 1,
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
        "mcp_servers": [{"name": m} for m in mcp],
        "plugins": [],
        "tools": ["Bash", "Read"],
        "skills": [],
    }


def bash(cmd: str) -> dict:
    return {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Bash", "input": {"command": cmd}}]}}


def mcp_call(name: str) -> dict:
    return {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": name, "input": {}}]}}


def tool_err(text: str) -> dict:
    return {"type": "user", "message": {"content": [{"type": "tool_result", "is_error": True, "content": text}]}}


class TestEnvelope(unittest.TestCase):
    def test_parse_real_haiku_envelope(self) -> None:
        env = parse((DATA / "haiku_envelope.json").read_text())
        self.assertFalse(env.is_error)
        self.assertEqual(env.structured_output, {"answer": "pong"})
        self.assertAlmostEqual(env.total_cost_usd, 0.05759, places=5)
        self.assertEqual(env.usage.cache_create_1h, 25605)
        self.assertIn("claude-haiku-4-5-20251001", env.model_usage)


class TestCost(unittest.TestCase):
    def test_recompute_matches_real_bill(self) -> None:
        cfg = load()
        env = parse((DATA / "haiku_envelope.json").read_text())
        cc = crosscheck(env, cfg.prices)
        self.assertTrue(cc.within_tolerance, cc.note)
        self.assertLess(cc.rel_delta, 0.01)


class TestIntegrity(unittest.TestCase):
    def test_ccx_arm_facade_used(self) -> None:
        env = env_from([init_msg(["cc-context"]), mcp_call("mcp__cc-context__outline"), result_msg()])
        verdict = integrity.assess(env, "ccx")
        self.assertTrue(verdict.ok)
        self.assertTrue(verdict.ccx_used)

    def test_ccx_arm_bash_ccx_used(self) -> None:
        env = env_from([init_msg(["cc-context"]), bash("ccx outline internal/x.go"), result_msg()])
        verdict = integrity.assess(env, "ccx")
        self.assertTrue(verdict.ccx_used)
        self.assertEqual(verdict.ccx_calls, ["bash:ccx outline"])

    def test_ccx_arm_guard_fired(self) -> None:
        env = env_from([init_msg(["cc-context"]), bash("cat internal/x.go"), tool_err("Blocked: use `ccx outline` instead"), result_msg()])
        verdict = integrity.assess(env, "ccx")
        self.assertTrue(verdict.guard_fired)
        self.assertTrue(verdict.ok)

    def test_ccx_arm_mislabeled_when_unused(self) -> None:
        env = env_from([init_msg(["cc-context"]), bash("echo hi"), result_msg()])
        verdict = integrity.assess(env, "ccx")
        self.assertFalse(verdict.ok)

    def test_baseline_clean(self) -> None:
        env = env_from([init_msg([]), bash("rg foo"), result_msg()])
        verdict = integrity.assess(env, "baseline")
        self.assertTrue(verdict.ok)
        self.assertFalse(verdict.ccx_used)

    def test_baseline_leak_detected(self) -> None:
        env = env_from([init_msg(["cc-context"]), result_msg()])
        verdict = integrity.assess(env, "baseline")
        self.assertFalse(verdict.ok)

    def test_heavy_call_classified(self) -> None:
        env = env_from([init_msg([]), bash("git diff HEAD~1"), result_msg()])
        verdict = integrity.assess(env, "baseline")
        self.assertIn("git-diff", verdict.native_heavy_calls)


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
        env = Envelope.synthetic({"file": "a", "line": 1}, is_error=True)
        self.assertFalse(grade(task, env, None).correct)

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
        env = env_from([init_msg([]), bash("cat manifest.json"), result_msg()])
        v = integrity.assess(env, "baseline")
        self.assertFalse(v.ok)
        self.assertIn("ANSWER KEY", v.note)

    def test_integrity_flags_answer_key_read_tool(self) -> None:
        read = {"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Read", "input": {"file_path": "/x/manifest.json"}}]}}
        env = env_from([init_msg(["cc-context"]), read, mcp_call("mcp__cc-context__ccx_symbol"), result_msg()])
        v = integrity.assess(env, "ccx")
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

    def test_verdict_logic(self) -> None:
        self.assertIn("inconclusive", verdict(0.0, float("nan")))
        self.assertIn("rtk trap", verdict(-2.0, -10.0))
        self.assertIn("equal-or-better", verdict(0.0, -10.0))
        self.assertIn("noise floor", verdict(-0.5, -10.0))
        self.assertIn("not cheaper", verdict(0.0, 5.0))


if __name__ == "__main__":
    unittest.main()
