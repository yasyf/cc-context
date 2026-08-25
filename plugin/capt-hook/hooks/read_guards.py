"""Bound an unbounded ``Read`` of a large text file — a context-flooding dump.

A ``Read`` with neither ``offset`` nor ``limit`` on a file over :data:`LARGE_READ_BYTES`
pulls the whole file into context, so it is rewritten to a windowed head (``limit`` =
:data:`READ_WINDOW_LINES`), with the note steering to ``ccx code outline`` +
``ccx code read --section`` for the rest. A binary file — an image, a PDF, a compiled
object — passes untouched: Claude renders it rather than reading lines of it, its cost is
image/page tokens rather than bytes, and neither a line window nor a ``ccx`` view of it
exists to steer toward.
"""

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
    ReadCall,
    Rewrite,
    Tool,
    on,
)

from .common import LARGE_READ_BYTES, READ_WINDOW_LINES, is_large, is_text

# A binary fixture for the inline tests: NUL bytes are what `is_text` reads as binary,
# and `FileFixture(size=...)` materializes `x` bytes, which are text.
BINARY_FIXTURE = "\x00" * (LARGE_READ_BYTES + 1)


class UnboundedLargeRead(CustomInputTypeCondition[ReadCall]):
    """Matches a ``Read`` of a large text file with neither ``offset`` nor ``limit`` set.

    The whole point of the offset/limit knobs is to bound how much enters context;
    a Read that sets neither on a text file over :data:`LARGE_READ_BYTES` is the
    unbounded dump this guard exists to stop.
    """

    def check_input(self, evt: BaseHookEvent, call: ReadCall) -> bool:
        path = evt.file.path
        return call.offset is None and call.limit is None and is_large(path) and is_text(path)


def read_note(path: Path) -> str:
    kb = path.stat().st_size // 1000
    with path.open("rb") as f:
        total = sum(1 for _ in f)
    return (
        f"Bounded an unbounded Read of `{path}` (~{kb} KB): showing lines 1-{READ_WINDOW_LINES} of {total} total "
        f"instead of dumping the whole file into context. Map the rest: `ccx code outline {path}`, "
        f"then `ccx code read {path} --section A-B` for the part you need, or re-run Read with offset/limit."
    )


@on(
    Event.PreToolUse,
    only_if=[Tool("Read"), UnboundedLargeRead()],
    tests={
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.txt")): Rewrite(limit="100"),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="image.png")): Allow(),
        Input(tool="Read", file=FileFixture(content=BINARY_FIXTURE, name="blob.bin")): Allow(),
        Input(tool="Read", file=FileFixture(size=1_024)): Allow(),
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1), offset=1, limit=100): Allow(),
    },
)
def bound_large_read(evt: BaseHookEvent) -> HookResult:
    """Window a large text Read to :data:`READ_WINDOW_LINES` lines."""
    return evt.rewrite({**evt._tool_input, "limit": READ_WINDOW_LINES}, note=read_note(evt.file.path))
