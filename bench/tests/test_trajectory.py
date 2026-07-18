"""Unit tests for trajectory attribution (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from cc_transcript import PrintResult, parse_print_result

from ccxbench import trajectory
from ccxbench.types import Decomposition

DATA = Path(__file__).resolve().parent / "data"


def fake_count(text: str) -> int:
    """Deterministic stand-in for the tokenizer: 4 chars per token."""
    return len(text) // 4


def assistant(prompt_input: int, cache_create: int, cache_read: int, content: list[dict]) -> dict:
    return {
        "type": "assistant",
        "session_id": "s1",
        "message": {
            "content": content,
            "usage": {
                "input_tokens": prompt_input,
                "cache_creation_input_tokens": cache_create,
                "cache_read_input_tokens": cache_read,
                "output_tokens": 1,
            },
        },
    }


def tool_result(tool_use_id: str, content: str, is_error: bool = False) -> dict:
    return {
        "type": "user",
        "session_id": "s1",
        "message": {"content": [{"type": "tool_result", "tool_use_id": tool_use_id, "is_error": is_error, "content": content}]},
    }


def rate_limit_event() -> dict:
    return {"type": "rate_limit_event", "rate_limit": {"status": "allowed"}}


def system_event() -> dict:
    return {"type": "system", "subtype": "init", "mcp_servers": [], "plugins": [], "tools": [], "skills": []}


def result_event() -> dict:
    return {
        "type": "result",
        "subtype": "success",
        "is_error": False,
        "num_turns": 1,
        "total_cost_usd": 0.0,
        "session_id": "s1",
        "usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
        "modelUsage": {},
        "permission_denials": [],
    }


def pr_of(events: list[dict]) -> PrintResult:
    return parse_print_result(json.dumps(events).encode())


def synthetic() -> list[dict]:
    # Turn 1 prompt 1000; peak is Turn 2 prompt 3000 (NOT the last turn); Turn 3 prompt 1500.
    return [
        assistant(1000, 0, 0, [{"type": "tool_use", "id": "tu1", "name": "Read", "input": {}}]),
        tool_result("tu1", "A" * 40),
        assistant(0, 0, 3000, [{"type": "tool_use", "id": "tu2", "name": "Grep", "input": {}}]),
        tool_result("tu2", "B" * 80),
        assistant(0, 0, 1500, [{"type": "text", "text": "Z" * 8}]),
        result_event(),
    ]


class TestTrajectory(unittest.TestCase):
    def test_high_water_is_peak_turn_not_last(self) -> None:
        m = trajectory.compute(pr_of(synthetic()), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.high_water, 3000)
        self.assertEqual(m.peak_turn, 1)
        self.assertEqual(m.turn_count, 3)

    def test_decomposition_sums_to_high_water(self) -> None:
        m = trajectory.compute(pr_of(synthetic()), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        d = m.decomposition
        self.assertEqual(d, Decomposition(static_overhead=999, tool_result=10, history=1, hook_error=0, residual=1990))
        self.assertEqual(d.total, m.high_water)

    def test_static_overhead_attributed_once(self) -> None:
        # Peak (3000) is far above turn-1 (1000); static must stay the turn-1 value, not scale to the peak.
        m = trajectory.compute(pr_of(synthetic()), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.decomposition.static_overhead, 999)
        self.assertLess(m.decomposition.static_overhead, m.high_water)

    def test_cumulative_tool_output_spans_all_turns(self) -> None:
        m = trajectory.compute(pr_of(synthetic()), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.cumulative_tool_output, 30)  # count("A"*40)=10 + count("B"*80)=20

    def test_tool_call_series(self) -> None:
        m = trajectory.compute(pr_of(synthetic()), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.tool_call_count, 2)
        self.assertEqual([(c.name, c.output_tokens) for c in m.tool_calls], [("Read", 10), ("Grep", 20)])

    def test_error_tool_result_lands_in_hook_error(self) -> None:
        events = [
            assistant(1000, 0, 0, [{"type": "tool_use", "id": "e1", "name": "Bash", "input": {}}]),
            tool_result("e1", "boom" * 10, is_error=True),
            assistant(0, 0, 2000, [{"type": "text", "text": "done"}]),
            result_event(),
        ]
        m = trajectory.compute(pr_of(events), first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.decomposition.hook_error, fake_count("boom" * 10))
        self.assertEqual(m.decomposition.tool_result, 0)
        self.assertEqual(m.decomposition.total, m.high_water)

    def test_stub_run_excluded(self) -> None:
        m = trajectory.from_file(DATA / "trajectory_stub.json", first_prompt="", count=fake_count)
        self.assertIsNone(m)

    def test_real_transcript_parses(self) -> None:
        m = trajectory.from_file(DATA / "trajectory_real.json", first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.high_water, 23254)
        self.assertEqual(m.decomposition.total, m.high_water)
        self.assertGreater(m.turn_count, 0)

    def test_totals_sum_per_api_call(self) -> None:
        # Per-call prompt/output summed across the whole trajectory; the turn-settling
        # convention (max per turn) dedupes the repeated same-usage assistant messages.
        tests = [
            # name, print result, first_prompt, total_prompt, total_output, total_tokens
            ("synthetic", pr_of(synthetic()), "P" * 4, 5500, 3, 5503),
            ("real", parse_print_result((DATA / "trajectory_real.json").read_bytes()), "", 91917, 10, 91927),
        ]
        for name, pr, first_prompt, total_prompt, total_output, total_tokens in tests:
            with self.subTest(name):
                m = trajectory.compute(pr, first_prompt=first_prompt, count=fake_count)
                assert m is not None
                self.assertEqual(m.total_prompt, total_prompt)
                self.assertEqual(m.total_output, total_output)
                self.assertEqual(m.total_tokens, total_tokens)
                self.assertEqual(m.total_tokens, m.total_prompt + m.total_output)

    def test_mid_turn_rate_limit_does_not_split_turn(self) -> None:
        # Two same-usage assistant messages are one logical turn; a rate_limit_event between them
        # must not split it (which would count the repeated 1000-token prompt twice).
        events = [
            assistant(1000, 0, 0, [{"type": "tool_use", "id": "tu1", "name": "Read", "input": {}}]),
            rate_limit_event(),
            assistant(1000, 0, 0, [{"type": "text", "text": "..."}]),
            result_event(),
        ]
        m = trajectory.compute(pr_of(events), first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.turn_count, 1)
        self.assertEqual(m.total_prompt, 1000)

    def test_user_event_still_splits_turns(self) -> None:
        events = [
            assistant(1000, 0, 0, [{"type": "tool_use", "id": "tu1", "name": "Read", "input": {}}]),
            tool_result("tu1", "x" * 40),
            assistant(0, 0, 2000, [{"type": "text", "text": "done"}]),
            result_event(),
        ]
        m = trajectory.compute(pr_of(events), first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.turn_count, 2)
        self.assertEqual(m.total_prompt, 3000)

    def test_leading_system_and_trailing_result_do_not_bound_turns(self) -> None:
        events = [
            system_event(),
            assistant(1000, 0, 0, [{"type": "tool_use", "id": "tu1", "name": "Read", "input": {}}]),
            rate_limit_event(),
            assistant(1000, 0, 0, [{"type": "text", "text": "..."}]),
            result_event(),
        ]
        m = trajectory.compute(pr_of(events), first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.turn_count, 1)
        self.assertEqual(m.total_prompt, 1000)

    def test_interleaved_parallel_tool_message_billed_once(self) -> None:
        # msg_shared makes two parallel tool calls with the first tool_result interleaved between the
        # tool_use blocks, so its messages straddle a user message and land in two turns (nav-tornado
        # ev5-ev9 shape). Billing by message id counts it once (101,000, not the per-turn 151,000).
        def a(mid: str, blocks: list[dict], cache_read: int, out: int) -> dict:
            return {
                "type": "assistant",
                "session_id": "s1",
                "message": {
                    "id": mid,
                    "content": blocks,
                    "usage": {"input_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": cache_read, "output_tokens": out},
                },
            }

        events = [
            a("msg_shared", [{"type": "thinking", "thinking": "..."}], 50000, 4),
            a("msg_shared", [{"type": "tool_use", "id": "t1", "name": "X", "input": {}}], 50000, 4),
            tool_result("t1", "r" * 40),
            a("msg_shared", [{"type": "tool_use", "id": "t2", "name": "Y", "input": {}}], 50000, 4),
            tool_result("t2", "r" * 40),
            a("msg_final", [{"type": "text", "text": "done"}], 51000, 6),
            result_event(),
        ]
        m = trajectory.compute(pr_of(events), first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.total_prompt, 101_000)
        self.assertEqual(m.total_output, 10)
        self.assertEqual(m.high_water, 51_000)
        # The interleaved user message still splits the turns: [thinking, tool_use1] | [tool_use2] | [final].
        self.assertEqual(m.turn_count, 3)

    def test_real_fixture_nonreg_bigo_prompt_not_doubled(self) -> None:
        # A real run carrying a mid-turn rate_limit_event: pre-fix the split doubled total_prompt
        # to 103,056; post-fix it is one turn counted once. Usage-derived fields only.
        m = trajectory.from_file(DATA / "nonreg-bigo__ccx-mcp__sonnet__r0.json", first_prompt="", count=fake_count)
        assert m is not None
        self.assertEqual(m.turn_count, 1)
        self.assertEqual(m.total_prompt, 51_528)

    def test_real_fixture_trace_tornado_envelope_consistency(self) -> None:
        # A flagged envelope-vs-transcript outlier pre-fix; post-fix the transcript recompute lands
        # within the 2% consistency tolerance. Assert usage-derived fields only (never tool output).
        m = trajectory.from_file(
            DATA / "trace-tornado-parse-body__baseline__sonnet__r0.json", first_prompt="", count=fake_count
        )
        assert m is not None
        self.assertEqual(m.turn_count, 4)
        self.assertEqual(m.total_prompt, 186_466)
        envelope_t = 186_965  # recorded envelope T for this run (results/20260711T010914Z/runs.jsonl)
        rel = abs(envelope_t - m.total_tokens) / envelope_t
        self.assertLessEqual(rel, 0.02)

    def test_from_file_rejects_non_array(self) -> None:
        path = DATA / "_not_an_array.json"
        path.write_text(json.dumps({"type": "result"}))
        try:
            with self.assertRaises(ValueError):
                trajectory.from_file(path, first_prompt="", count=fake_count)
        finally:
            path.unlink()


if __name__ == "__main__":
    unittest.main()
