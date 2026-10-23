"""Warn once on an unbounded ``Read`` of a large text file, naming the sliced ``ccx code read`` view."""

from __future__ import annotations

from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    CustomInputTypeCondition,
    Event,
    FileFixture,
    HookResult,
    Input,
    PreToolUseEvent,
    ReadCall,
    Tool,
    Warn,
    on,
)

from .common import LARGE_READ_BYTES, READ_WINDOW_LINES, is_large, is_text

BINARY_FIXTURE = "\x00" * (LARGE_READ_BYTES + 1)

LONG_TEXT_FIXTURE = "line\n" * (LARGE_READ_BYTES // 5 + 1)


class UnboundedLargeRead(CustomInputTypeCondition[ReadCall]):
    def check_input(self, evt: BaseHookEvent, call: ReadCall) -> bool:
        path = evt.file.path
        return call.offset is None and call.limit is None and is_large(path) and is_text(path)


def line_count(path: Path) -> int:
    return (data := path.read_bytes()).count(b"\n") + (not data.endswith(b"\n"))


@on(
    Event.PreToolUse,
    only_if=[Tool("Read"), UnboundedLargeRead()],
    max_fires=1,
    tests={
        Input(tool="Read", file=FileFixture(content=LONG_TEXT_FIXTURE, name="big.txt")): Warn(
            pattern="ccx code read <path> --section"
        ),
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1, name="one-line.txt")): Allow(),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="image.png")): Allow(),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="blob.bin")): Allow(),
        Input(tool="Read", file=FileFixture(size=1_024)): Allow(),
        Input(tool="Read", file=FileFixture(content=LONG_TEXT_FIXTURE), offset=1, limit=100): Allow(),
    },
)
def warn_large_read(evt: PreToolUseEvent) -> HookResult | None:
    if (total := line_count(evt.file.path)) <= READ_WINDOW_LINES:
        return None
    return evt.context(
        f"This file has {total} lines. "
        "Run `ccx code read <path> --section A-B` for the slice you need, or pass offset/limit to Read."
    )
