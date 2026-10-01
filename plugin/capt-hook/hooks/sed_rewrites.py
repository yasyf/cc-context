"""Rewrite ``sed -n A,Bp <file>`` to ``ccx code read --section A-B``."""

from __future__ import annotations

import re
import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandSchema,
    Input,
    Operand,
    Option,
    Rewrite,
    rewrite_command_occurrences,
)

from .common import call_of, carries_expansion, ccx_bin, rewrote_note

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

SED_PRINT = CommandSchema(
    "sed",
    operands=(Operand("script"), Operand("file")),
    options=(Option("quiet", ("-n",), bool),),
)
LINE_RANGE = re.compile(r"(\d+),(\d+)p")


def sed_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if occ.piped or occ.command.redirects or occ.command.executable != "sed":
        return None
    arguments = SED_PRINT.bind(call_of(evt, occ))
    if not arguments.complete or arguments.values.get("quiet") != (True,):
        return None
    script, file = (arguments.values[name][0] for name in ("script", "file"))
    if (line_range := LINE_RANGE.fullmatch(str(script))) is None or file is None or carries_expansion(str(file)):
        return None
    if (ccx := ccx_bin()) is None:
        return None
    start, end = line_range.groups()
    return f"{shlex.quote(ccx)} code read {shlex.quote(str(file))} --section {start}-{end}"


rewrite_command_occurrences(
    to=sed_to,
    note=rewrote_note("ccx code read --section", "same lines, token-bounded"),
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="code read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed -n '5,10p' ~/.claude/cache/changelog.md"): Allow(),
        Input(command="sed -n '1,2p' $d/host.go"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"sed -n 10,40p f.go\")\nasyncio.run(main())'"
        ): Allow(),
        Input(command="echo x; sed -n 10,40p f.go"): Rewrite(pattern='echo x; '),
        Input(command="cat f | sed -n '1,2p'; echo y"): Allow(),
    },
)
