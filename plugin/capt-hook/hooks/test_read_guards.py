"""Tests for the large-file Read condition in ``read_guards``."""

from __future__ import annotations

from pathlib import Path

from captain_hook.testing.helpers import mock_tool_event

from hooks.common import LARGE_READ_BYTES
from hooks.read_guards import UnboundedLargeRead


def test_large_binary_file_does_not_match(tmp_path: Path) -> None:
    p = tmp_path / "screenshot.png"
    p.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * LARGE_READ_BYTES)
    assert not UnboundedLargeRead().check(mock_tool_event("Read", file=str(p)))


def test_large_text_file_matches(tmp_path: Path) -> None:
    p = tmp_path / "big.go"
    p.write_text("x" * (LARGE_READ_BYTES + 1))
    assert UnboundedLargeRead().check(mock_tool_event("Read", file=str(p)))
