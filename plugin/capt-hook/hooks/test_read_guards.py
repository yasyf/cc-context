"""Tests for the large-file Read warning in ``read_guards``."""

from __future__ import annotations

from pathlib import Path

from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore
from captain_hook.testing.helpers import mock_tool_event
from captain_hook.types import Action

from hooks.common import LARGE_READ_BYTES
from hooks.read_guards import UnboundedLargeRead, warn_large_read


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


def test_long_file_warns_with_the_line_count_and_leaves_the_read_whole(tmp_path: Path) -> None:
    p = tmp_path / "big.go"
    p.write_text("x\n" * (LARGE_READ_BYTES // 2 + 1))
    result = warn_large_read(read_event(p))
    assert result.action is Action.warn
    assert result.updated_input is None
    assert "10001 lines" in result.message
    assert "ccx code read <path> --section" in result.message


def test_few_line_file_stays_silent(tmp_path: Path) -> None:
    p = tmp_path / "minified.json"
    p.write_text("x" * (LARGE_READ_BYTES + 1))
    assert warn_large_read(read_event(p)) is None
