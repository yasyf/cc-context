"""Unit tests for the benchmark harness internals (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import asyncio
import dataclasses
import json
import os
import re
import subprocess
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

from cc_transcript import parse_print_result

from ccxbench import repos, taskgen
from ccxbench.config import Config, Repo, load
from ccxbench.goldgen import recompute_lc_predicate
from ccxbench.grade import grade, synthetic_result
from ccxbench.graders import GradeContext, grade_file_line, grade_keywords, grade_set_match, grade_test_run
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

    def test_file_line_accepts_any_alt_site(self) -> None:
        # A symbol defined in two places: an answer at either the primary or an alternate site passes.
        spec = {"line_tolerance": 2}
        gold = {"file": "tornado/routing.py", "line": 376, "alt_sites": [{"file": "tornado/web.py", "line": 2027}]}
        ctx = GradeContext("", None)
        self.assertTrue(grade_file_line({"file": "tornado/routing.py", "line": 376}, gold, spec, ctx).correct)
        self.assertTrue(grade_file_line({"file": "tornado/web.py", "line": 2028}, gold, spec, ctx).correct)
        self.assertFalse(grade_file_line({"file": "tornado/httputil.py", "line": 376}, gold, spec, ctx).correct)

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


class TestGraderPathPrecedence(unittest.TestCase):
    """Lock the offline grader's isolation: its explicit `PYTHONPATH=<rel>` under cwd=workdir resolves
    the workdir's package ahead of anything the child inherits — the pilot's venv-contamination vector.
    This test locks existing behavior; it does not change the grader env."""

    def _pkg(self, root: Path, marker: str) -> None:
        pkg = root / "foo"
        pkg.mkdir(parents=True)
        (pkg / "__init__.py").write_text(f"MARKER = {marker!r}\n")

    def test_workdir_pythonpath_wins_over_inherited(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            workdir = root / "workdir"
            decoy = root / "decoy"
            self._pkg(workdir / "src", "workdir")
            self._pkg(decoy, "decoy")
            spec = {
                "cmd": "PYTHONPATH=src python3 -c \"import foo; assert foo.MARKER == 'workdir', foo.MARKER\"",
                "timeout_s": 60,
            }
            ctx = GradeContext(result_text="", workdir=workdir)
            # A hostile inherited PYTHONPATH points at the decoy; the cmd's own assignment must win.
            with patch.dict(os.environ, {"PYTHONPATH": str(decoy)}, clear=False):
                res = grade_test_run({}, {}, spec, ctx)
        self.assertTrue(res.correct, res.detail)


class TestTargetedEditSuffix(unittest.TestCase):
    """The edit family pins the test-suite/install variance vectors without leaking grader mechanics."""

    def test_every_edit_prompt_ends_with_suffix(self) -> None:
        for t in taskgen.targeted_edit_tasks():
            self.assertTrue(t.prompt.endswith(taskgen.EDIT_SUFFIX), t.id)

    def test_suffix_does_not_leak_grader_mechanics(self) -> None:
        low = taskgen.EDIT_SUFFIX.lower()
        for banned in ("pytest", "pythonpath", "grader"):
            self.assertNotIn(banned, low)


def large_context_corpus_present(cfg: Config) -> bool:
    return all((cfg.fixtures_root / t.repo).is_dir() for t in taskgen.large_context_tasks())


class TestLargeContextBuilder(unittest.TestCase):
    """Every large_context predicate recomputes to a non-empty, duplicate-free member set from the
    pinned checkout; the set_match grader accepts that recomputed set and rejects wrong and
    incomplete answers; and the get_command task is not defeated by the obvious one-line grep."""

    def _members(self, cfg: Config, t) -> list[str]:
        return sorted(recompute_lc_predicate(cfg.fixtures_root / t.repo, t.gold["lc_predicate"], t.repo))

    def test_predicates_recompute_and_grade(self) -> None:
        cfg = load()
        tasks = taskgen.large_context_tasks()
        if not large_context_corpus_present(cfg):
            self.skipTest("large_context corpus absent; run `python -m ccxbench build-corpus` first")
        self.assertEqual(
            [t.id for t in tasks],
            [
                "lc-click-get-command",
                "lc-click-to-info-dict",
                "lc-click-invoke",
                "lc-tornado-prepare",
                "lc-tornado-initialize",
                "lc-tornado-handler-subclasses",
            ],
        )
        for t in tasks:
            self.assertEqual(t.category, "large_context")
            field = t.grader.spec["field"]
            members = self._members(cfg, t)
            self.assertTrue(members, f"{t.id}: predicate matched nothing")
            self.assertEqual(len(members), len(set(members)), f"{t.id}: duplicate members")
            gold = {field: members}
            ctx = GradeContext("", None)
            good = grade_set_match({field: members}, gold, t.grader.spec, ctx)
            self.assertTrue(good.correct, f"{t.id}: recomputed gold graded incorrect: {good.detail}")
            bad = grade_set_match({field: ["NotAReal"]}, gold, t.grader.spec, ctx)
            self.assertFalse(bad.correct, f"{t.id}: wrong answer graded correct")
            incomplete = grade_set_match({field: members[:-1]}, gold, t.grader.spec, ctx)
            self.assertFalse(incomplete.correct, f"{t.id}: incomplete answer graded correct")

    def test_get_command_not_grep_defeatable(self) -> None:
        """Un-shortcuttability regression: the obvious `grep '^class '` over core.py over-matches,
        so the gold (classes defining get_command directly) is a strict subset of it."""
        cfg = load()
        if not large_context_corpus_present(cfg):
            self.skipTest("large_context corpus absent; run `python -m ccxbench build-corpus` first")
        t = {x.id: x for x in taskgen.large_context_tasks()}["lc-click-get-command"]
        members = set(self._members(cfg, t))
        core = (cfg.fixtures_root / "click" / "src/click/core.py").read_text()
        grep_classes = {
            m.group(1)
            for line in core.splitlines()
            if (m := re.match(r"class (\w+)", line)) and not m.group(1).startswith("_")
        }
        self.assertNotEqual(grep_classes, members)
        self.assertTrue(members < grep_classes, "gold must be a strict subset the grep over-matches")


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


class TestRepoConvergence(unittest.TestCase):
    """Fix #9: an existing .fixtures checkout is trusted only when HEAD is at the pinned ref with a
    clean working tree; a diverged or dirty checkout is deleted and re-cloned rather than reused."""

    def _git(self, *args: str, cwd: Path) -> None:
        subprocess.run(
            ["git", "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", *args],
            cwd=str(cwd),
            check=True,
            capture_output=True,
            text=True,
        )

    def _source_repo(self, root: Path) -> Path:
        src = root / "src"
        src.mkdir()
        self._git("init", "-q", cwd=src)
        (src / "file.txt").write_text("v1\n")
        self._git("add", "-A", cwd=src)
        self._git("commit", "-qm", "one", cwd=src)
        self._git("tag", "v1", cwd=src)
        return src

    def _cfg(self, fixtures: Path, url: str) -> Config:
        return dataclasses.replace(load(), fixtures_root=fixtures, repos=(Repo("x", url, "v1", "go"),))

    def test_clean_checkout_reused(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg = self._cfg(root / "fx", str(self._source_repo(root)))
            dest = repos.clone(cfg, cfg.repos[0])
            self.assertTrue(repos._at_ref(dest, "v1"))
            marker = dest / ".git" / "reused_marker"
            marker.write_text("x")  # survives only if the checkout is reused, not re-cloned
            self.assertEqual(repos.clone(cfg, cfg.repos[0]), dest)
            self.assertTrue(marker.exists())

    def test_dirty_checkout_recloned(self) -> None:
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            cfg = self._cfg(root / "fx", str(self._source_repo(root)))
            dest = repos.clone(cfg, cfg.repos[0])
            marker = dest / ".git" / "reused_marker"
            marker.write_text("x")
            (dest / "file.txt").write_text("dirtied\n")  # working tree no longer clean
            self.assertFalse(repos._at_ref(dest, "v1"))
            self.assertEqual(repos.clone(cfg, cfg.repos[0]), dest)
            self.assertTrue(repos._at_ref(dest, "v1"))  # re-cloned back to the pinned ref, clean
            self.assertFalse(marker.exists())  # the mutated checkout was deleted


if __name__ == "__main__":
    unittest.main()
