"""Bound an unbounded ``Read`` of a large text file to a windowed head."""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    CustomInputTypeCondition,
    FileFixture,
    Input,
    ReadCall,
    Rewrite,
    set_tool_input,
)

from .common import LARGE_READ_BYTES, READ_WINDOW_LINES, is_large, is_text

BINARY_FIXTURE = "\x00" * (LARGE_READ_BYTES + 1)


class UnboundedLargeRead(CustomInputTypeCondition[ReadCall]):
    def check_input(self, evt: BaseHookEvent, call: ReadCall) -> bool:
        path = evt.file.path
        return call.offset is None and call.limit is None and is_large(path) and is_text(path)


set_tool_input(
    "limit",
    READ_WINDOW_LINES,
    tool="Read",
    only_if=[UnboundedLargeRead()],
    note=(
        f"Read limited to the first {READ_WINDOW_LINES} lines of this large file. "
        "Run `ccx code outline <path>`, then `ccx code read <path> --section A-B` for the rest."
    ),
    tests={
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.txt")): Rewrite(limit="100"),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="image.png")): Allow(),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="blob.bin")): Allow(),
        Input(tool="Read", file=FileFixture(size=1_024)): Allow(),
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1), offset=1, limit=100): Allow(),
    },
)
