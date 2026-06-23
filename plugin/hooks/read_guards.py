"""Guard against an unbounded ``Read`` of a large file — a context-flooding dump."""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CustomInputTypeCondition,
    Event,
    FileFixture,
    Input,
    ReadCall,
    Tool,
    hook,
)

from .common import LARGE_READ_BYTES, is_large


class UnboundedLargeRead(CustomInputTypeCondition[ReadCall]):
    """Matches a ``Read`` of a large file with neither ``offset`` nor ``limit`` set.

    The whole point of the offset/limit knobs is to bound how much enters context;
    a Read that sets neither on a file over :data:`LARGE_READ_BYTES` is the
    unbounded dump this guard exists to stop.
    """

    def check_input(self, evt: BaseHookEvent, call: ReadCall) -> bool:
        return call.offset is None and call.limit is None and is_large(evt.file.path)


hook(
    Event.PreToolUse,
    only_if=[Tool("Read"), UnboundedLargeRead()],
    message=(
        "BLOCKED: unbounded Read of a large file (>20KB) floods context. "
        "Map it first: `ccx outline <path>` (or mcp__cc-context__outline), then "
        "`ccx read <path> --section A-B` (or mcp__cc-context__read) for the part you need. "
        "Escape hatch — whole file: `ccx read <path> --full`, or re-run Read with offset/limit."
    ),
    block=True,
    tests={
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1)): Block(pattern="ccx outline"),
        Input(tool="Read", file=FileFixture(size=1_024)): Allow(),
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1), offset=1, limit=100): Allow(),
    },
)
