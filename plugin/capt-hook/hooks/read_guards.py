"""Bound an unbounded ``Read`` of a large file — a context-flooding dump.

A ``Read`` with neither ``offset`` nor ``limit`` on a file over :data:`LARGE_READ_BYTES`
pulls the whole file into context. A large *text* file is rewritten to a windowed head
(``limit`` = :data:`READ_WINDOW_LINES`), with the note steering to ``ccx code outline`` +
``ccx code read --section`` for the rest; a binary/notebook file — an image, a PDF, a
notebook — has no useful line window, so it stays a hard block.
"""

from __future__ import annotations

from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
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

from .common import LARGE_READ_BYTES, READ_WINDOW_LINES, is_large

# Extensions a line window can't sensibly bound: an image/PDF/notebook read whole is not a
# line dump to head, so these keep the hard block instead of a windowed rewrite.
BINARY_READ_EXTS = frozenset({".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb"})

# The block message kept for binary/notebook reads — the message the whole guard used before
# the text-file rewrite landed.
BINARY_READ_MESSAGE = (
    "BLOCKED: unbounded Read of a large file (>20KB) floods context. "
    "Map it first: `ccx code outline <path>` (or mcp__cc-context__ccx_code_outline), then "
    "`ccx code read <path> --section A-B` (or mcp__cc-context__ccx_code_read) for the part you need. "
    "Escape hatch — whole file: `ccx code read <path> --full`, or re-run Read with offset/limit."
)


class UnboundedLargeRead(CustomInputTypeCondition[ReadCall]):
    """Matches a ``Read`` of a large file with neither ``offset`` nor ``limit`` set.

    The whole point of the offset/limit knobs is to bound how much enters context;
    a Read that sets neither on a file over :data:`LARGE_READ_BYTES` is the
    unbounded dump this guard exists to stop.
    """

    def check_input(self, evt: BaseHookEvent, call: ReadCall) -> bool:
        return call.offset is None and call.limit is None and is_large(evt.file.path)


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
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1, name="image.png")): Block(
            pattern="ccx code outline"
        ),
        Input(tool="Read", file=FileFixture(size=1_024)): Allow(),
        Input(tool="Read", file=FileFixture(size=LARGE_READ_BYTES + 1), offset=1, limit=100): Allow(),
    },
)
def bound_large_read(evt: BaseHookEvent) -> HookResult:
    """Window a large text Read to :data:`READ_WINDOW_LINES` lines; hard-block binary/notebook reads."""
    path = evt.file.path
    if path.suffix.lower() in BINARY_READ_EXTS:
        return evt.block(BINARY_READ_MESSAGE)
    return evt.rewrite({**evt._tool_input, "limit": READ_WINDOW_LINES}, note=read_note(path))
