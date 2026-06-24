"""Unit tests for the benchmark harness internals (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from cc_transcript import parse_print_result
from cc_transcript.cost import cost_of

from ccxbench import integrity
from ccxbench.config import load
from ccxbench.cost import crosscheck
from ccxbench.grade import grade, synthetic_result
from ccxbench.graders import GradeContext, grade_file_line, grade_keywords, grade_set_match
from ccxbench.report import paired_task_ids, verdict
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

    def test_verdict_logic(self) -> None:
        self.assertIn("inconclusive", verdict(0.0, float("nan")))
        self.assertIn("rtk trap", verdict(-2.0, -10.0))
        self.assertIn("equal-or-better", verdict(0.0, -10.0))
        self.assertIn("noise floor", verdict(-0.5, -10.0))
        self.assertIn("not cheaper", verdict(0.0, 5.0))


class TestDrive(unittest.TestCase):
    """drive must inject the task's JSON-Schema dict as --json-schema, hand back the FULL raw
    stdout (not spawnllm's collapsed result text), and reuse spawnllm's transient retry."""

    def _spec(self):
        from spawnllm import ClaudeConfig, RunSpec

        return RunSpec(
            prompt="p",
            model="sonnet",
            max_attempts=3,
            timeout=5,
            provider_configs={"claude": ClaudeConfig(output_format="json")},
        )

    def test_injects_schema_and_returns_full_stdout(self) -> None:
        import ccxbench.runner as runner
        from spawnllm.backends.base import Invocation
        from spawnllm.proc import RunResult
        from spawnllm import Response

        schema = {"type": "object", "properties": {"file": {"type": "string"}}}
        full_stream = json.dumps([result_msg(structured_output={"file": "a"})])
        seen: dict = {}

        class FakeBackend:
            def invocation(self, spec):
                return Invocation(["claude", "-p", "--output-format", "json"], spec.prompt)

            def env(self):
                return {}

            def to_response(self, raw, *, returncode, stderr, spec):
                return Response(error=None, result="ok")

        def fake_capture(argv, **kw):
            seen["argv"], seen["kw"] = argv, kw
            return RunResult(full_stream, "", 0)

        orig_sel, orig_cap = runner.spawnllm.select_backend, runner.capture_cli
        runner.spawnllm.select_backend = lambda: FakeBackend()
        runner.capture_cli = fake_capture
        try:
            rr = runner.drive(self._spec(), schema)
        finally:
            runner.spawnllm.select_backend, runner.capture_cli = orig_sel, orig_cap

        self.assertEqual(rr.stdout, full_stream)
        self.assertIn("--json-schema", seen["argv"])
        self.assertEqual(seen["argv"][seen["argv"].index("--json-schema") + 1], json.dumps(schema))
        # The base argv (with its single --output-format json) is preserved, not rebuilt.
        self.assertEqual(seen["argv"][:4], ["claude", "-p", "--output-format", "json"])
        self.assertEqual(seen["kw"]["input"], "p")

    def test_retries_transient_then_succeeds(self) -> None:
        import ccxbench.runner as runner
        from spawnllm.backends.base import Invocation
        from spawnllm.proc import RunResult
        from spawnllm import Response

        outcomes = [RunResult("", "Overloaded (529)", 1), RunResult("done", "", 0)]

        class FakeBackend:
            def invocation(self, spec):
                return Invocation(["claude", "-p"], spec.prompt)

            def env(self):
                return {}

            def to_response(self, raw, *, returncode, stderr, spec):
                return Response(error="Overloaded (529)" if returncode else None, result=raw)

        def fake_capture(argv, **kw):
            return outcomes.pop(0)

        orig_sel, orig_cap, orig_sleep = (
            runner.spawnllm.select_backend,
            runner.capture_cli,
            runner.time.sleep,
        )
        runner.spawnllm.select_backend = lambda: FakeBackend()
        runner.capture_cli = fake_capture
        runner.time.sleep = lambda _s: None
        try:
            rr = runner.drive(self._spec(), {})
        finally:
            (runner.spawnllm.select_backend, runner.capture_cli, runner.time.sleep) = (
                orig_sel,
                orig_cap,
                orig_sleep,
            )

        self.assertEqual(rr.stdout, "done")
        self.assertEqual(rr.returncode, 0)
        self.assertEqual(outcomes, [])


if __name__ == "__main__":
    unittest.main()
