"""Rewrite ``sed -n A,Bp <file>`` — a numeric line-range dump — to ``ccx code read
--section`` in place, with a ``note`` back to the model.
"""

from __future__ import annotations

import re
import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
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


class SedLineRange(CustomCommandLineCondition):
    """Matches a ``;``/``&&``/``|``-joined line carrying a ``sed -n 'A,Bp' <file>``
    occurrence — a numeric line-range extract from a file.

    Allows sed in a pipe (it consumes a stream, not a named file) and allows
    substitution (`sed 's/.../.../'`); only the standalone numeric-range print of a
    file argument is the `ccx code read --section` case this rewrites. A file token
    carrying a shell expansion (``~``/``$``) declines: ``shlex.quote`` would freeze it,
    so the command falls through to Allow and the real shell expands the path.
    """

    RANGE = re.compile(r"^(\d+),(\d+)p$")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            (parts := sed_parts(occ.command)) is not None
            and not carries_expansion(parts[2])
            and not occ.piped
            and not occ.command.redirects
            for occ in cl.occurrences
        )


def sed_parts(cmd: Command) -> tuple[str, str, str] | None:
    if not cmd.runs("sed", "-n") or len(cmd.args) != 3:
        return None
    m = SedLineRange.RANGE.match(cmd.args[1].strip("'\""))
    if not m:
        return None
    return m.group(1), m.group(2), cmd.args[2]


def sed_to(evt: BaseHookEvent, occ: "Occurrence") -> str | None:
    cmd = occ.command
    if occ.piped or cmd.redirects:
        return None  # only rewrite what the splice can reproduce in full
    if (parts := sed_parts(cmd)) is None or carries_expansion(parts[2]):
        return None
    start, end, file = parts
    if ccx := ccx_bin():
        return f"{shlex.quote(ccx)} code read {shlex.quote(file)} --section {start}-{end}"
    return None


def sed_note(evt: BaseHookEvent, pairs: "list[tuple[Occurrence, str]]") -> str:
    commands = ", ".join(f"`{occ.command.raw}`" for occ, _ in pairs)
    return f"Rewrote {commands} → `ccx code read --section`: same lines, token-bounded."


rewrite_command_occurrences(
    only_if=[SedLineRange()],
    to=sed_to,
    note=sed_note,
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="code read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        # A `~`/`$` file token declines to rewrite — shlex.quote would freeze it; the real shell expands it.
        Input(command="sed -n '5,10p' ~/.claude/cache/changelog.md"): Allow(),
        Input(command="sed -n '1,2p' $d/host.go"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
        # `ccx exec` pass-through is deliberate: the script is one opaque ccx token,
        # so a sed inside sh() never parses as this rule's sed.
        Input(
            command="ccx exec 'async def main(): return await sh(\"sed -n 10,40p f.go\")\nasyncio.run(main())'"
        ): Allow(),
        Input(command="echo x; sed -n 10,40p f.go"): Rewrite(pattern='echo x; '),
        Input(command="cat f | sed -n '1,2p'; echo y"): Allow(),
    },
)
