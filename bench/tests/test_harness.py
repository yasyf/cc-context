"""Unit tests for the benchmark harness internals (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import asyncio
import json
import re
import unittest
from pathlib import Path

from cc_transcript import parse_print_result

from ccxbench import taskgen
from ccxbench.__main__ import recompute_lc_predicate
from ccxbench.config import Config, load
from ccxbench.grade import grade, synthetic_result
from ccxbench.graders import GradeContext, grade_file_line, grade_keywords, grade_set_match
from ccxbench.types import Grader, Task

DATA = Path(__file__).resolve().parent / "data"


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


class TestPrintResult(unittest.TestCase):
    def test_parse_real_haiku_envelope(self) -> None:
        pr = parse_print_result((DATA / "haiku_envelope.json").read_bytes())
        self.assertFalse(pr.is_error)
        self.assertEqual(pr.structured_output, {"answer": "pong"})
        self.assertAlmostEqual(pr.total_cost_usd, 0.05759, places=5)
        self.assertEqual(pr.usage.cache_creation.ephemeral_1h_input_tokens, 25605)
        self.assertIn("claude-haiku-4-5-20251001", pr.model_usage)


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


def large_context_corpus_present(cfg: Config) -> bool:
    return all((cfg.fixtures_root / t.repo).is_dir() for t in taskgen.large_context_tasks())


class TestLargeContextBuilder(unittest.TestCase):
    """Every gold member of a large_context task is a real declaration in its fixture file AND
    satisfies the task's predicate (recomputed independently, mirroring verify_oss), and the
    naive grep a frugal baseline would run does NOT isolate the gold — the tasks are not
    grep-defeatable."""

    def test_gold_members_present_and_predicate_holds(self) -> None:
        cfg = load()
        tasks = taskgen.large_context_tasks()
        if not large_context_corpus_present(cfg):
            self.skipTest("large_context corpus absent; run `python -m ccxbench build-corpus` first")
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
        if not large_context_corpus_present(cfg):
            self.skipTest("large_context corpus absent; run `python -m ccxbench build-corpus` first")

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
