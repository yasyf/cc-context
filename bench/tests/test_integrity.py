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
        ("mcp_guard_fired", [init_msg(["cc-context"]), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-mcp", True, "ok"),
        ("mcp_cc_absent", [init_msg([]), bash("ccx code outline x.go")], "ccx-mcp", False, "cc-context MCP not loaded"),
        ("mcp_unused", [init_msg(["cc-context"]), bash("echo hi")], "ccx-mcp", False, "ccx never used"),
        ("cli_bash_ccx_used", [init_msg([]), bash("ccx code outline x.go")], "ccx-cli", True, "ok"),
        ("cli_guard_fired", [init_msg([]), bash("cat x.go"), tool_err(GUARD_ERR)], "ccx-cli", True, "ok"),
        # The isolation proof: even genuine Bash ccx is mislabeled when cc-context is loaded.
        ("cli_cc_present_breach", [init_msg(["cc-context"]), bash("ccx code outline x.go")], "ccx-cli", False, "isolation breach"),
        ("cli_mcp_call_breach", [init_msg([]), mcp_call("mcp__cc-context__ccx_code_symbol")], "ccx-cli", False, "mcp__cc-context__ tool called"),
        ("cli_unused", [init_msg([]), bash("echo hi")], "ccx-cli", False, "Bash ccx never used"),
    )

    def test_verdicts(self) -> None:
        for name, msgs, arm, want_ok, note_substr in self.CASES:
            with self.subTest(name=name):
                v = integrity.assess(pr_from([*msgs, result_msg()]), arm)
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
                v = integrity.assess(pr_from([*msgs, result_msg()]), arm)
                self.assertFalse(v.ok)
                self.assertIn("ANSWER KEY", v.note)


class TestFieldClassification(unittest.TestCase):
    def test_bash_ccx_call_summarized(self) -> None:
        v = integrity.assess(pr_from([init_msg([]), bash("ccx code outline internal/x.go"), result_msg()]), "ccx-cli")
        self.assertEqual(v.ccx_calls, ["bash:ccx code outline"])
        self.assertTrue(v.ccx_used)

    def test_heavy_native_call_classified(self) -> None:
        v = integrity.assess(pr_from([init_msg([]), bash("git diff HEAD~1"), result_msg()]), "baseline")
        self.assertIn("git-diff", v.native_heavy_calls)

    def test_guard_fired_via_permission_denials(self) -> None:
        # A denied heavy primitive recorded only in permission_denials (no is_error tool_result)
        # must still count as a ccx-navigation guard fire — detected structurally.
        denial = {"tool_name": "Bash", "tool_use_id": "t1", "tool_input": {"command": "find . -name mux.go -type f"}}
        v = integrity.assess(pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])]), "ccx-mcp")
        self.assertTrue(v.guard_fired)
        self.assertTrue(v.ok)

    def test_non_navigation_denial_not_a_guard_fire(self) -> None:
        denial = {"tool_name": "Write", "tool_use_id": "t1", "tool_input": {"file_path": "m.py", "content": "x: Any = 1\n"}}
        v = integrity.assess(pr_from([init_msg(["cc-context"]), result_msg(permission_denials=[denial])]), "ccx-mcp")
        self.assertFalse(v.guard_fired)
        self.assertFalse(v.ok)

    def test_unknown_arm_raises(self) -> None:
        with self.assertRaises(ValueError):
            integrity.assess(pr_from([init_msg([]), result_msg()]), "ccx")


if __name__ == "__main__":
    unittest.main()
