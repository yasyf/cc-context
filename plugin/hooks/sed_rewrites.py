"""Rewrite ``sed -n A,Bp <file>`` — a numeric line-range dump — to ``ccx code read
--section`` in place, with a ``note`` back to the model. When ``ccx`` cannot be resolved on
disk the rewrite falls back to a hard block, so the guard never emits a broken
``ccx: command not found``.
"""

from __future__ import annotations

import re
import shlex

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command,
)

from .common import ccx_bin


class SedLineRange(CustomCommandLineCondition):
    """Matches ``sed -n 'A,Bp' <file>`` — a numeric line-range extract from a file.

    Allows sed in a pipe (it consumes a stream, not a named file) and allows
    substitution (`sed 's/.../.../'`); only the standalone numeric-range print of a
    file argument is the `ccx code read --section` case this rewrites.
    """

    RANGE = re.compile(r"^(\d+),(\d+)p$")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # A piped sed reads a stream, not the trailing file token — leave it alone.
        return not cl.q.uses_redirect() and sed_parts(cl) is not None


def sed_parts(cl: CommandLine) -> tuple[str, str, str] | None:
    cmd = cl.primary
    if cmd is None or not cmd.runs("sed", "-n") or len(cmd.args) != 3:
        return None
    m = SedLineRange.RANGE.match(cmd.args[1].strip("'\""))
    if not m:
        return None
    return m.group(1), m.group(2), cmd.args[2]


def sed_to(evt: BaseHookEvent) -> str | None:
    start, end, file = sed_parts(evt.command_line)
    if ccx := ccx_bin():
        return f"{ccx} code read {shlex.quote(file)} --section {start}-{end}"
    return None


def sed_note(evt: BaseHookEvent) -> str:
    start, end, file = sed_parts(evt.command_line)
    return f"Rewrote `sed -n {start},{end}p {file}` → `ccx code read --section`: same lines, token-bounded."


rewrite_command(
    only_if=[SedLineRange()],
    to=sed_to,
    block=(
        "BLOCKED: `sed -n A,Bp <file>` is a line-range dump. "
        "Use `ccx code read <file> --section A-B` (or mcp__cc-context__ccx_code_read) — it returns the "
        "same lines with structure. Escape hatch: pipe it (`cat <file> | sed -n 'A,Bp'`)."
    ),
    note=sed_note,
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="code read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
        # `ccx exec` pass-through is deliberate: the script is one opaque ccx token,
        # so a sed inside sh() never parses as this rule's sed.
        Input(
            command="ccx exec 'async def main(): return await sh(\"sed -n 10,40p f.go\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
