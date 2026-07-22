"""Rewrite each standalone bounded ``head`` file occurrence to ``ccx code read
--section 1-N`` in place, with one merged ``note`` back to the model. Forms with no
line-arithmetic equivalent — ``tail`` on a file, byte mode (`-c`), or multi-file forms
— hard-block the whole Bash line instead. Piped or redirected occurrences and file
tokens carrying shell expansions remain outside the guard. When ``ccx`` cannot be
resolved on disk, a qualifying occurrence falls back to a hard block, so the guard
never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import re
import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    Command,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command_occurrences,
)

from .common import carries_expansion, ccx_bin

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence


def int_or_none(s: str) -> int | None:
    return int(s) if s.isdigit() else None


def headtail_parse(cmd: Command) -> tuple[str, str, int | None, list[str]] | None:
    exe = cmd.executable
    if exe not in ("head", "tail"):
        return None
    mode = "line"
    count: int | None = None
    files: list[str] = []
    args = cmd.args
    i = 0
    while i < len(args):
        a = args[i]
        if a in ("-c", "--bytes"):
            mode = "byte"
            i += 2
        elif a.startswith(("--bytes=", "-c")):
            mode = "byte"
            i += 1
        elif a in ("-n", "--lines"):
            count = int_or_none(args[i + 1].lstrip("+")) if i + 1 < len(args) else None
            i += 2
        elif a.startswith("--lines="):
            count = int_or_none(a.split("=", 1)[1].lstrip("+"))
            i += 1
        elif a.startswith("-n"):
            count = int_or_none(a[2:].lstrip("=+"))
            i += 1
        elif re.fullmatch(r"-\d+", a):
            count = int(a[1:])
            i += 1
        elif a.startswith("-"):
            i += 1
        else:
            files.append(a)
            i += 1
    return exe, mode, count, files


def headtail_file_parts(occ: Occurrence) -> tuple[str, str, int | None, list[str]] | None:
    if occ.piped or occ.command.redirects or (parsed := headtail_parse(occ.command)) is None:
        return None
    return parsed if parsed[3] and not any(carries_expansion(file) for file in parsed[3]) else None


class HeadTailFile(CustomCommandLineCondition):
    """Gate the occurrence rewrite on a standalone named-file ``head``/``tail``.

    The primitive's ``block`` is also its zero-rewrite fallback, so unrelated commands,
    stdin reads, piped or redirected occurrences, and shell-expanding file operands must
    not match the registration. Every occurrence is inspected so a qualifying sibling in
    a compound line still activates the rewrite or line-grained block.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(headtail_file_parts(occ) is not None for occ in cl.occurrences)


def headtail_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if (parsed := headtail_file_parts(occ)) is None:
        return None
    exe, mode, count, files = parsed
    if exe == "head" and mode == "line" and len(files) == 1 and (ccx := ccx_bin()):
        n = count if count is not None else 10
        return f"{shlex.quote(ccx)} code read {shlex.quote(files[0])} --section 1-{n}"
    return None


def headtail_block_if(evt: BaseHookEvent, occ: Occurrence) -> bool:
    if (parsed := headtail_file_parts(occ)) is None:
        return False
    exe, mode, _count, files = parsed
    return exe == "tail" or mode == "byte" or len(files) > 1


def headtail_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    commands = ", ".join(f"`{occ.command.raw}`" for occ, _ in pairs)
    return f"Rewrote {commands} → `ccx code read --section`: same lines, token-bounded."


rewrite_command_occurrences(
    only_if=[HeadTailFile()],
    to=headtail_to,
    block_if=headtail_block_if,
    block=(
        "BLOCKED: `head`/`tail` on a file dumps raw lines into context. "
        "Use `ccx code read <file> --section A-B` for a bounded range, or `ccx code outline <file>` "
        "to map it — the outline's line numbers show where the file ends, so you can bound a tail "
        "operation. "
        "Escape hatch — bounding a pipe's output: keep `<cmd> | head`/`| tail`."
    ),
    note=headtail_note,
    tests={
        Input(command="head -40 f.go"): Rewrite(pattern="code read f.go --section 1-40"),
        Input(command="head -n 40 f.go"): Rewrite(pattern="--section 1-40"),
        Input(command="head f.go"): Rewrite(pattern="--section 1-10"),  # head defaults to 10 lines
        Input(command="tail -n 20 f.go"): Block(pattern="ccx code outline"),
        Input(command="tail -20 f.go"): Block(pattern="ccx code read"),
        Input(command="head -c 100 f.go"): Block(pattern="ccx code outline"),  # byte mode: no line math
        Input(command="head a.go b.go"): Block(pattern="ccx code read"),  # multi-file
        Input(command="rg foo | head -5"): Allow(),  # head-as-sink is fine for THIS hook; the rg source is gated by rg_guards
        Input(command="cat f | tail -20"): Allow(),  # pipe sink — sanctioned
        Input(command="head -5"): Allow(),  # reads stdin, no file operand
        # A `~`/`$` file token declines to rewrite — shlex.quote would freeze it; the real shell expands it.
        Input(command="head -40 $d/host.go"): Allow(),
        Input(command="head -20 ~/.claude/cache/log.txt"): Allow(),
        # `ccx exec` pass-through is deliberate: head inside sh() is the script's business.
        Input(
            command="ccx exec 'async def main(): return await sh(\"head -40 f.go\")\nasyncio.run(main())'"
        ): Allow(),
        Input(command="echo x; head -40 f.go"): Rewrite(pattern="echo x; "),
        Input(command="echo x; tail -20 f.go"): Block(pattern="ccx code read"),
    },
)
