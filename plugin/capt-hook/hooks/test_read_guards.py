"""Tests for the large-file Read bound in ``read_guards``."""

from __future__ import annotations

from pathlib import Path

from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore
from captain_hook.testing.helpers import mock_tool_event

from hooks.common import LARGE_READ_BYTES
from hooks.read_guards import UnboundedLargeRead, bound_large_read


def test_large_binary_file_does_not_match(tmp_path: Path) -> None:
    p = tmp_path / "screenshot.png"
    p.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * LARGE_READ_BYTES)
    assert not UnboundedLargeRead().check(mock_tool_event("Read", file=str(p)))


def test_large_text_file_matches(tmp_path: Path) -> None:
    p = tmp_path / "big.go"
    p.write_text("x" * (LARGE_READ_BYTES + 1))
    assert UnboundedLargeRead().check(mock_tool_event("Read", file=str(p)))


def read_event(path: Path) -> PreToolUseEvent:
    return PreToolUseEvent(
        _raw={"tool_name": "Read", "tool_input": {"file_path": str(path)}},
        ctx=HookContext(session=SessionStore(None), transcript=None, settings=None),
    )


def test_long_file_note_names_the_window_total_and_the_rest(tmp_path: Path) -> None:
    p = tmp_path / "big.go"
    p.write_text("x\n" * (LARGE_READ_BYTES // 2 + 1))
    result = bound_large_read(read_event(p))
    assert result.updated_input == {"file_path": str(p), "limit": 100}
    assert result.note == "Showed lines 1-100 of 10001; read the rest with `ccx code read <path> --section 101-10001`."
    assert len(result.note) < 160


def test_few_line_file_is_left_unbounded(tmp_path: Path) -> None:
    p = tmp_path / "minified.json"
    p.write_text("x" * (LARGE_READ_BYTES + 1))
    assert bound_large_read(read_event(p)) is None
