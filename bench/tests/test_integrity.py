"""Unit tests for per-arm structural integrity verdicts (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import unittest

from cc_transcript import parse_print_result

from ccxbench import integrity


def pr_from(messages: list[dict]):
    return parse_print_result(json.dumps(messages).encode())


def init_msg(mcp: list[str]) -> dict:
    return {
        "type": "system",
        "subtype": "init",
        "mcp_servers": [{"name": m, "status": "connected"} for m in mcp],
        "plugins": [],
        "tools": ["Bash", "Read"],
        "skills": [],
    }


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


def tool_use(name: str, tool_input: dict) -> dict:
    return {
        "type": "assistant",
        "session_id": "test",
        "message": {"content": [{"type": "tool_use", "id": "t1", "name": name, "input": tool_input}]},
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


GUARD_ERR = "Blocked: use `ccx code outline` instead"


class TestArmVerdicts(unittest.TestCase):
    """One paired truth table across all three arms: ok iff the arm behaved as labeled."""

    CASES = (
        # (name, messages, arm, want_ok, note_substr)
        ("baseline_clean", [init_msg([]), bash("rg foo")], "baseline", True, "ok"),
        ("baseline_cc_present", [init_msg(["cc-context"])], "baseline", False, "cc-context MCP present"),
        ("baseline_ccx_used", [init_msg([]), bash("ccx code outline x.go")], "baseline", False, "ccx used"),
        ("baseline_guard_fired", [init_msg([]), bash("cat x.go"), tool_err(GUARD_ERR)], "baseline", False, "guard fired"),
        ("mcp_facade_used", [init_msg(["cc-context"]), mcp_call("mcp__cc-context__ccx_code_outline")], "ccx-mcp", True, "ok"),
        ("mcp_bash_ccx_used", [init_msg(["cc-context"]), bash("ccx code outline x.go")], "ccx-mcp", True, "ok"),
        # Fix #8: a guard fire with zero ccx use is mislabeled, not ok — guards alone don't prove ccx ran.
        ("mcp_guard_only_no_ccx", [init_msg(["cc-context"]), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-mcp", False, "guards fired but ccx never used"),
        ("mcp_guard_and_ccx", [init_msg(["cc-context"]), bash("ccx code outline x.go"), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-mcp", True, "ok"),
        ("mcp_cc_absent", [init_msg([]), bash("ccx code outline x.go")], "ccx-mcp", False, "cc-context MCP not loaded"),
        ("mcp_unused", [init_msg(["cc-context"]), bash("echo hi")], "ccx-mcp", False, "ccx never used"),
        ("cli_bash_ccx_used", [init_msg([]), bash("ccx code outline x.go")], "ccx-cli", True, "ok"),
        ("cli_guard_only_no_ccx", [init_msg([]), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-cli", False, "guards fired but ccx never used"),
        ("cli_guard_and_ccx", [init_msg([]), bash("ccx code outline x.go"), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-cli", True, "ok"),
        # The isolation proof: even genuine Bash ccx is mislabeled when cc-context is loaded.
        ("cli_cc_present_breach", [init_msg(["cc-context"]), bash("ccx code outline x.go")], "ccx-cli", False, "isolation breach"),
        ("cli_mcp_call_breach", [init_msg([]), mcp_call("mcp__cc-context__ccx_code_symbol")], "ccx-cli", False, "mcp__cc-context__ tool called"),
        ("cli_unused", [init_msg([]), bash("echo hi")], "ccx-cli", False, "Bash ccx never used"),
    )

    def test_verdicts(self) -> None:
        for name, msgs, arm, want_ok, note_substr in self.CASES:
            with self.subTest(name=name):
                v = integrity.assess(pr_from([*msgs, result_msg()]), arm, "navigation")
                self.assertEqual(v.ok, want_ok, f"{name}: note={v.note!r}")
                self.assertIn(note_substr, v.note)


class TestCheatDetection(unittest.TestCase):
    """Reading the gold manifest invalidates the run in every arm, overriding the arm verdict."""

    CASES = (
        ("baseline_bash", [init_msg([]), bash("cat manifest.json")], "baseline"),
        ("mcp_read", [init_msg(["cc-context"]), tool_use("Read", {"file_path": "/x/manifest.json"}), mcp_call("mcp__cc-context__ccx_code_symbol")], "ccx-mcp"),
        ("cli_read", [init_msg([]), tool_use("Read", {"file_path": "/x/manifest.json"}), bash("ccx code outline x.go")], "ccx-cli"),
    )

    def test_answer_key_invalidates(self) -> None:
        for name, msgs, arm in self.CASES:
            with self.subTest(name=name):
                v = integrity.assess(pr_from([*msgs, result_msg()]), arm, "navigation")
                self.assertFalse(v.ok)
                self.assertIn("ANSWER KEY", v.note)


class TestTasksDirCheatDetection(unittest.TestCase):
    """Fix #3: reading a committed bench/tasks/*.json (the golds) invalidates the run, however the
    path is spelled — absolute, relative traversal, or a bare dir listing."""

    CASES = (
        ("abs_task_json", [init_msg([]), tool_use("Read", {"file_path": "/Users/x/cc-context/bench/tasks/nav-click-command.json"}), bash("ccx code outline x.go")], "ccx-cli"),
        ("rel_traversal", [init_msg([]), bash("cat ../../tasks/nav-click-command.json")], "baseline"),
        ("tasks_dir_listing", [init_msg([]), bash("ls tasks/")], "baseline"),
    )

    def test_tasks_json_invalidates(self) -> None:
        for name, msgs, arm in self.CASES:
            with self.subTest(name=name):
                v = integrity.assess(pr_from([*msgs, result_msg()]), arm, "navigation")
                self.assertFalse(v.ok, f"{name}: note={v.note!r}")
                self.assertIn("ANSWER KEY", v.note)

    def test_subtasks_path_is_not_a_cheat(self) -> None:
        # `subtasks/` must not false-positive as the corpus `tasks/` dir.
        v = integrity.assess(
            pr_from([init_msg([]), bash("cat src/subtasks/runner.py"), result_msg()]), "baseline", "navigation"
        )
        self.assertNotIn("ANSWER KEY", v.note)


class TestCommandPositionCcx(unittest.TestCase):
    """Fix #7: Bash ccx counts only at command position — start of the command, right after a
    shell separator (optionally preceded by env assignments), or inside a command substitution
    (TestSubstitutionNestedCcx) — never as a literal argument word to echo/printf."""

    def test_ccx_as_argument_is_not_a_ccx_use(self) -> None:
        for cmd in ('echo "ccx code outline"', "echo run ccx code outline", "printf 'see ccx'"):
            with self.subTest(cmd=cmd):
                v = integrity.assess(pr_from([init_msg([]), bash(cmd), result_msg()]), "ccx-cli", "navigation")
                self.assertFalse(v.ccx_used, f"{cmd!r} should not count as ccx use")

    def test_ccx_at_command_position_counts(self) -> None:
        cases = [
            "ccx code outline x.go",
            "cat foo && ccx repo overview",
            "rg bar | ccx code grep baz",
            "FOO=1 ccx code read x.go",
            "x=$(ccx repo overview)",
        ]
        for cmd in cases:
            with self.subTest(cmd=cmd):
                v = integrity.assess(pr_from([init_msg([]), bash(cmd), result_msg()]), "ccx-cli", "navigation")
                self.assertTrue(v.ccx_used, f"{cmd!r} should count as ccx use")


class TestSubstitutionNestedCcx(unittest.TestCase):
    """cc-transcript 14.1: `$(…)` and backtick substitutions at every word/argument position join
    command enumeration (nested included), so substitution-nested ccx counts as a genuine use —
    while a quoted literal mention still never does (TestCommandPositionCcx)."""

    CASES = (
        # (name, command, expected ccx_calls)
        ("dollar_paren_argument", "echo $(ccx repo overview)", ["bash:ccx repo overview"]),
        ("backticks_argument", "echo `ccx repo overview`", ["bash:ccx repo overview"]),
        ("nested_in_host_span", "find . -name $(ccx code grep foo)", ["bash:ccx code grep"]),
        ("nested_inside_nested_substitution", "echo $(cat $(ccx repo overview))", ["bash:ccx repo overview"]),
        ("double_quoted_substitution", 'echo "$(ccx code outline x.go)"', ["bash:ccx code outline"]),
    )

    def test_substitution_nested_ccx_counts(self) -> None:
        for name, cmd, want_calls in self.CASES:
            with self.subTest(name=name):
                v = integrity.assess(pr_from([init_msg([]), bash(cmd), result_msg()]), "ccx-cli", "navigation")
                self.assertTrue(v.ccx_used, f"{cmd!r} should count as ccx use")
                self.assertEqual(v.ccx_calls, want_calls)
                self.assertTrue(v.ok, f"note={v.note!r}")


class TestFieldClassification(unittest.TestCase):
    def test_bash_ccx_call_summarized(self) -> None:
        v = integrity.assess(
            pr_from([init_msg([]), bash("ccx code outline internal/x.go"), result_msg()]), "ccx-cli", "navigation"
        )
        self.assertEqual(v.ccx_calls, ["bash:ccx code outline"])
        self.assertTrue(v.ccx_used)

    def test_heavy_native_call_classified(self) -> None:
        v = integrity.assess(pr_from([init_msg([]), bash("git diff HEAD~1"), result_msg()]), "baseline", "navigation")
        self.assertIn("git-diff", v.native_heavy_calls)

    def test_substitution_nested_heavy_classified(self) -> None:
        v = integrity.assess(pr_from([init_msg([]), bash("echo $(git diff HEAD~1)"), result_msg()]), "baseline", "navigation")
        self.assertEqual(v.native_heavy_calls, ["git-diff"])

    def test_guard_fired_via_permission_denials(self) -> None:
        # A denied heavy primitive recorded only in permission_denials (no is_error tool_result)
        # must still count as a ccx-navigation guard fire — detected structurally and reported
        # separately. But (fix #8) a guard fire with zero ccx use is mislabeled, not ok.
        denial = {"tool_name": "Bash", "tool_use_id": "t1", "tool_input": {"command": "find . -name mux.go -type f"}}
        v = integrity.assess(
            pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])]), "ccx-mcp", "navigation"
        )
        self.assertTrue(v.guard_fired)  # guard fire still detected + separately reported
        self.assertFalse(v.ok)
        self.assertIn("guards fired but ccx never used", v.note)

    def test_non_navigation_denial_not_a_guard_fire(self) -> None:
        denial = {"tool_name": "Write", "tool_use_id": "t1", "tool_input": {"file_path": "m.py", "content": "x: Any = 1\n"}}
        v = integrity.assess(
            pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])]), "ccx-mcp", "navigation"
        )
        self.assertFalse(v.guard_fired)
        self.assertFalse(v.ok)

    def test_unknown_arm_raises(self) -> None:
        with self.assertRaises(ValueError):
            integrity.assess(pr_from([init_msg([]), result_msg()]), "ccx", "navigation")


class TestControlCategory(unittest.TestCase):
    """Fix #3: control (non_regression) tasks run in an empty workdir with no code, so a ccx arm
    need not exercise ccx to be OK — but the isolation invariants still hold, and headline tasks
    still require ccx."""

    def test_control_ccx_mcp_no_ccx_is_ok(self) -> None:
        # cc-context loaded (isolation held), no ccx call — OK because it's a control task.
        v = integrity.assess(pr_from([init_msg(["cc-context"]), bash("echo hi"), result_msg()]), "ccx-mcp", "non_regression")
        self.assertTrue(v.ok, f"note={v.note!r}")
        self.assertIn("control", v.note)

    def test_control_ccx_cli_no_ccx_is_ok(self) -> None:
        # zero MCP, no Bash ccx — OK because it's a control task.
        v = integrity.assess(pr_from([init_msg([]), bash("echo hi"), result_msg()]), "ccx-cli", "non_regression")
        self.assertTrue(v.ok, f"note={v.note!r}")
        self.assertIn("control", v.note)

    def test_control_ccx_cli_with_cc_present_still_mislabeled(self) -> None:
        # The isolation breach overrides the control relaxation: cc-context must be absent in ccx-cli.
        v = integrity.assess(pr_from([init_msg(["cc-context"]), bash("echo hi"), result_msg()]), "ccx-cli", "non_regression")
        self.assertFalse(v.ok)
        self.assertIn("isolation breach", v.note)

    def test_headline_ccx_mcp_no_ccx_still_mislabeled(self) -> None:
        # A non-control task with cc-context loaded but zero ccx use stays mislabeled.
        v = integrity.assess(pr_from([init_msg(["cc-context"]), bash("echo hi"), result_msg()]), "ccx-mcp", "navigation")
        self.assertFalse(v.ok)
        self.assertIn("ccx never used", v.note)


if __name__ == "__main__":
    unittest.main()
