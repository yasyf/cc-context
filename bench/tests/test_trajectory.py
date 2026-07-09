"""Unit tests for trajectory attribution (no API calls).

Run: cd bench && python -m unittest discover -s tests
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path

from ccxbench import trajectory
from ccxbench.types import Decomposition

DATA = Path(__file__).resolve().parent / "data"


def fake_count(text: str) -> int:
    """Deterministic stand-in for the tokenizer: 4 chars per token."""
    return len(text) // 4


def assistant(prompt_input: int, cache_create: int, cache_read: int, content: list[dict]) -> dict:
    return {
        "type": "assistant",
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
        "message": {"content": [{"type": "tool_result", "tool_use_id": tool_use_id, "is_error": is_error, "content": content}]},
    }


def synthetic() -> list[dict]:
    # Turn 1 prompt 1000; peak is Turn 2 prompt 3000 (NOT the last turn); Turn 3 prompt 1500.
    return [
        assistant(1000, 0, 0, [{"type": "tool_use", "id": "tu1", "name": "Read", "input": {}}]),
        tool_result("tu1", "A" * 40),
        assistant(0, 0, 3000, [{"type": "tool_use", "id": "tu2", "name": "Grep", "input": {}}]),
        tool_result("tu2", "B" * 80),
        assistant(0, 0, 1500, [{"type": "text", "text": "Z" * 8}]),
    ]


class TestTrajectory(unittest.TestCase):
    def test_high_water_is_peak_turn_not_last(self) -> None:
        m = trajectory.compute(synthetic(), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.high_water, 3000)
        self.assertEqual(m.peak_turn, 1)
        self.assertEqual(m.turn_count, 3)

    def test_decomposition_sums_to_high_water(self) -> None:
        m = trajectory.compute(synthetic(), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        d = m.decomposition
        self.assertEqual(d, Decomposition(static_overhead=999, tool_result=10, history=1, hook_error=0, residual=1990))
        self.assertEqual(d.total, m.high_water)

    def test_static_overhead_attributed_once(self) -> None:
        # Peak (3000) is far above turn-1 (1000); static must stay the turn-1 value, not scale to the peak.
        m = trajectory.compute(synthetic(), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.decomposition.static_overhead, 999)
        self.assertLess(m.decomposition.static_overhead, m.high_water)

    def test_cumulative_tool_output_spans_all_turns(self) -> None:
        m = trajectory.compute(synthetic(), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.cumulative_tool_output, 30)  # count("A"*40)=10 + count("B"*80)=20

    def test_tool_call_series(self) -> None:
        m = trajectory.compute(synthetic(), first_prompt="P" * 4, count=fake_count)
        assert m is not None
        self.assertEqual(m.tool_call_count, 2)
        self.assertEqual([(c.name, c.output_tokens) for c in m.tool_calls], [("Read", 10), ("Grep", 20)])

    def test_error_tool_result_lands_in_hook_error(self) -> None:
        events = [
            assistant(1000, 0, 0, [{"type": "tool_use", "id": "e1", "name": "Bash", "input": {}}]),
            tool_result("e1", "boom" * 10, is_error=True),
            assistant(0, 0, 2000, [{"type": "text", "text": "done"}]),
        ]
        m = trajectory.compute(events, first_prompt="", count=fake_count)
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
        # convention (max per turn) dedupes the repeated same-usage assistant events.
        tests = [
            # name, events, first_prompt, total_prompt, total_output, total_tokens
            ("synthetic", synthetic(), "P" * 4, 5500, 3, 5503),
            ("real", trajectory.load_events(DATA / "trajectory_real.json"), "", 91917, 10, 91927),
        ]
        for name, events, first_prompt, total_prompt, total_output, total_tokens in tests:
            with self.subTest(name):
                m = trajectory.compute(events, first_prompt=first_prompt, count=fake_count)
                assert m is not None
                self.assertEqual(m.total_prompt, total_prompt)
                self.assertEqual(m.total_output, total_output)
                self.assertEqual(m.total_tokens, total_tokens)
                self.assertEqual(m.total_tokens, m.total_prompt + m.total_output)

    def test_load_events_rejects_non_array(self) -> None:
        path = DATA / "_not_an_array.json"
        path.write_text(json.dumps({"type": "result"}))
        try:
            with self.assertRaises(ValueError):
                trajectory.load_events(path)
        finally:
            path.unlink()


if __name__ == "__main__":
    unittest.main()
