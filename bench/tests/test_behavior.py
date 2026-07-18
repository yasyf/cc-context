"""Unit tests for the B1/B2 behavioral readouts (no API calls).

Run: cd bench && python -m unittest tests.test_behavior
"""

from __future__ import annotations

import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from ccxbench import behavior


def _rec(
    task: str,
    arm: str,
    *,
    ccx_calls: tuple[str, ...] = (),
    heavy: tuple[str, ...] = (),
    ok: bool = True,
    is_error: bool = False,
    model: str = "sonnet",
    repeat: int = 0,
    category: str = "large_context",
) -> dict:
    return {
        "task_id": task,
        "category": category,
        "arm": arm,
        "model": model,
        "repeat": repeat,
        "is_error": is_error,
        "correct": True,
        "integrity": {"ok": ok, "ccx_calls": list(ccx_calls), "native_heavy_calls": list(heavy)},
    }


def _usage(input_tokens: int) -> dict:
    return {"input_tokens": input_tokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 1}


def _transcript(read_chars: int) -> list[dict]:
    """A minimal stream: one Read tool call whose result is `read_chars` long, plus a closing turn."""
    return [
        {"type": "assistant", "session_id": "test", "message": {"id": "m1", "usage": _usage(100), "content": [
            {"type": "tool_use", "id": "r1", "name": "Read", "input": {"file_path": "web.py"}}]}},
        {"type": "user", "session_id": "test", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "r1", "content": "x" * read_chars}]}},
        {"type": "assistant", "session_id": "test", "message": {"id": "m2", "usage": _usage(200), "content": [
            {"type": "text", "text": "done"}]}},
        {
            "type": "result",
            "subtype": "success",
            "is_error": False,
            "num_turns": 1,
            "total_cost_usd": 0.0,
            "session_id": "test",
            "usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
            "modelUsage": {},
            "permission_denials": [],
        },
    ]


def _session(tmp: str, records: list[dict], transcripts: dict[str, list[dict]]) -> Path:
    session = Path(tmp) / "s"
    (session / "raw").mkdir(parents=True)
    with (session / "runs.jsonl").open("w") as f:
        for r in records:
            f.write(json.dumps(r) + "\n")
    for run_id, events in transcripts.items():
        (session / "raw" / f"{run_id}.json").write_text(json.dumps(events))
    return session


class TestSignals(unittest.TestCase):
    def test_run_flooded_on_heavy_dump_without_transcript(self) -> None:
        self.assertTrue(behavior.run_flooded(_rec("t", "baseline", heavy=("cat",)), None))
        self.assertTrue(behavior.run_flooded(_rec("t", "baseline", heavy=("read-unbounded",)), None))

    def test_run_not_flooded_when_bounded(self) -> None:
        self.assertFalse(behavior.run_flooded(_rec("t", "baseline", heavy=("sed-n",)), None))

    def test_run_compact_requires_scoped_and_bounded(self) -> None:
        self.assertTrue(behavior.run_compact(_rec("t", "ccx-cli", ccx_calls=("bash:ccx code grep",))))
        self.assertTrue(behavior.run_compact(_rec("t", "ccx-cli", ccx_calls=("bash:ccx exec",))))
        # repo overview (orient reflex) disqualifies, even alongside a grep.
        self.assertFalse(behavior.run_compact(
            _rec("t", "ccx-cli", ccx_calls=("bash:ccx repo overview", "bash:ccx code grep"))))
        # more than MAX_OUTLINE_CALLS outlines is the per-file self-flood.
        self.assertFalse(behavior.run_compact(_rec(
            "t", "ccx-cli", ccx_calls=("bash:ccx code grep", "bash:ccx code outline",
                                       "bash:ccx code outline", "bash:ccx code outline"))))
        # no scoped grep/exec at all → not compact.
        self.assertFalse(behavior.run_compact(_rec("t", "ccx-cli", ccx_calls=("bash:ccx code outline",))))


class TestCompute(unittest.TestCase):
    def test_b1_and_b2_rates(self) -> None:
        records = [
            _rec("flood-t3-tornado-close", "baseline", repeat=0),  # big read -> flood
            _rec("flood-t3-tornado-close", "baseline", repeat=1),  # small read -> bounded
            _rec("flood-t3-tornado-close", "baseline", repeat=2, heavy=("cat",)),  # dump -> flood
            _rec("flood-t3-tornado-close", "ccx-cli", repeat=0, ccx_calls=("bash:ccx code grep",)),  # compact
            _rec("flood-t3-tornado-close", "ccx-cli", repeat=1,
                 ccx_calls=("bash:ccx repo overview",)),  # self-flood
            _rec("flood-t3-tornado-close", "ccx-cli", repeat=2, ccx_calls=("bash:ccx exec",)),  # compact
        ]
        transcripts = {
            "flood-t3-tornado-close__baseline__sonnet__r0": _transcript(5000),
            "flood-t3-tornado-close__baseline__sonnet__r1": _transcript(100),
            "flood-t3-tornado-close__baseline__sonnet__r2": _transcript(100),
        }
        with TemporaryDirectory() as tmp:
            session = _session(tmp, records, transcripts)
            rep = behavior.compute(session, count=len)  # count=len -> chars are tokens, deterministic
        self.assertEqual((rep.b1_flooded, rep.b1_total), (2, 3))
        self.assertEqual((rep.b2_compact, rep.b2_total), (2, 3))
        self.assertAlmostEqual(rep.b1_rate, 2 / 3)
        self.assertAlmostEqual(rep.b2_rate, 2 / 3)

    def test_excludes_errors_and_integrity_failures(self) -> None:
        records = [
            _rec("t", "baseline", repeat=0, is_error=True, heavy=("cat",)),
            _rec("t", "baseline", repeat=1, ok=False, heavy=("cat",)),
            _rec("t", "baseline", repeat=2, heavy=("cat",)),  # the only counted baseline run
        ]
        with TemporaryDirectory() as tmp:
            session = _session(tmp, records, {})
            rep = behavior.compute(session, count=len)
        self.assertEqual((rep.b1_flooded, rep.b1_total), (1, 1))

    def test_task_glob_filter(self) -> None:
        records = [
            _rec("flood-t3-tornado-close", "baseline", repeat=0, heavy=("cat",)),
            _rec("nav-click-command", "baseline", repeat=0, category="navigation", heavy=("cat",)),
        ]
        with TemporaryDirectory() as tmp:
            session = _session(tmp, records, {})
            rep = behavior.compute(session, task_glob="flood-*", count=len)
        self.assertEqual(rep.b1_total, 1)
        self.assertEqual(rep.runs[0].task_id, "flood-t3-tornado-close")

    def test_halt_marker_line_ignored(self) -> None:
        records = [_rec("flood-t3-tornado-close", "baseline", heavy=("cat",))]
        with TemporaryDirectory() as tmp:
            session = _session(tmp, records, {})
            with (session / "runs.jsonl").open("a") as f:
                f.write(json.dumps({"halted": "spent $1 >= ceiling"}) + "\n")
            rep = behavior.compute(session, count=len)
        self.assertEqual(rep.b1_total, 1)


if __name__ == "__main__":
    unittest.main()
