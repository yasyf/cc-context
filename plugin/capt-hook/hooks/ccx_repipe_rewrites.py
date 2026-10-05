"""Strip head/tail pipes after ``ccx code read``, ``ccx repo find``, and ``ccx vcs ship``."""

from __future__ import annotations

import shlex
from dataclasses import dataclass
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command,
)

from .common import ccx_bin, command_expands, once_note, rewrote_note
from .headtail_rewrites import headtail_parse

if TYPE_CHECKING:
    from cc_transcript.command import Command

CODE_READ = ("code", "read")
REPO_FIND = ("repo", "find")
VCS_SHIP = ("vcs", "ship")


def source_is_ccx(cmd: Command) -> bool:
    return (
        not cmd.env
        and not command_expands(cmd)
        and (ccx := ccx_bin()) is not None
        and cmd.executable in ("ccx", ccx)
    )


def byte_sink(cmd: Command) -> bool:
    if cmd.env:
        return False
    match list(cmd.args):
        case ["-c" | "--bytes", raw_count]:
            return raw_count.isdigit()
        case _:
            return False


@dataclass(frozen=True)
class CcxPipedToSink(CustomCommandLineCondition):
    family: tuple[str, str]

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if len(cl.occurrences) != 2 or cl.occurrences[1].prev_op != "|":
            return False
        source, sink = cl.occurrences[0].command, cl.occurrences[1].command
        return source_is_ccx(source) and source.args[:2] == self.family and sink.executable in ("head", "tail")


def unpiped(cmd: Command, args: list[str]) -> str:
    return " ".join(shlex.quote(token) for token in [cmd.executable, *args])


def head_count(evt: BaseHookEvent) -> int | None:
    parsed = headtail_parse(evt.cmd.line.primary)
    if parsed is None:
        return None
    exe, count, files = parsed
    return None if exe != "head" or files else count if count is not None else 10


def code_read_to(evt: BaseHookEvent) -> str | None:
    src = evt.cmd.line.head
    args = list(src.args)
    if (count := head_count(evt)) is None or "--full" not in args:
        return None
    kept = [arg for arg in args if arg != "--full"]
    return " ".join([shlex.quote(src.executable), *(shlex.quote(arg) for arg in kept), "--section", f"1-{count}"])


def repo_find_to(evt: BaseHookEvent) -> str | None:
    return None if head_count(evt) is None else unpiped(evt.cmd.line.head, list(evt.cmd.line.head.args))


def vcs_ship_to(evt: BaseHookEvent) -> str | None:
    sink = evt.cmd.line.primary
    parsed = headtail_parse(sink)
    if (parsed is not None and not parsed[2]) or byte_sink(sink):
        return unpiped(evt.cmd.line.head, list(evt.cmd.line.head.args))
    return None


rewrite_command(
    only_if=[CcxPipedToSink(CODE_READ)],
    skip_if=[Annotated("raw")],
    to=code_read_to,
    note=rewrote_note("ccx code read --section 1-N", "same lines, no dropped overflow footer"),
    tests={
        Input(command="ccx code read f.go --full | head -5"): Rewrite(pattern="code read f.go --section 1-5"),
        Input(command="ccx code read f.go --full | head"): Rewrite(pattern="--section 1-10"),
        Input(command="command ccx code read f.go --full | head -5"): Allow(),
        Input(command="FOO=1 ccx code read f.go --full | head -5"): Allow(),
        Input(command="env FOO=1 ccx code read f.go --full | head -5"): Allow(),
        Input(command="env FOO='two words' ccx code read f.go --full | head -5"): Allow(),
        Input(command="FOO='two words' ccx code read f.go --full | head -5"): Allow(),
        Input(command="ccx code read f.go --full | tail -5"): Allow(),
        Input(command="ccx code read f.go --full | head -c 100"): Allow(),
        Input(command="ccx code read f.go --full | head --lines=5"): Allow(),
        Input(command="ccx code read $FILE --full | head -5"): Allow(),
        Input(command="ccx code read f.go --section 1-5"): Allow(),
        Input(command="ccx code read f.go --full | head -5 # ccx:raw"): Allow(),
        Input(command="ccx code read '# ccx:raw' --full | head -5"): Rewrite(pattern="--section 1-5"),
        Input(command="ccx code grep foo | head -5"): Allow(),
        Input(command="ccx code grep foo | jq . | head -3"): Allow(),
        Input(command="rg foo | head -5"): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"ccx code read f --full | head\")\nasyncio.run(main())'"
        ): Allow(),
    },
)

rewrite_command(
    only_if=[CcxPipedToSink(REPO_FIND)],
    skip_if=[Annotated("raw")],
    to=repo_find_to,
    note=once_note("Dropped the `| head` pipe: `ccx repo find` output is already token-budget-capped."),
    tests={
        Input(command='ccx repo find "**/*.go" | head -20'): Rewrite(pattern="repo find '**/*.go'"),
        Input(command="ccx repo find $(printf '**/*.go') | head -20"): Allow(),
        Input(command='ccx repo find "**/*.go" | tail -20'): Allow(),
        Input(command='ccx repo find "**/*.go"'): Allow(),
        Input(command='ccx repo find "**/*.go" | head -20 # ccx:raw'): Allow(),
        Input(command="ccx repo find '# ccx:raw' | head -20"): Rewrite(pattern="repo find"),
    },
)

rewrite_command(
    only_if=[CcxPipedToSink(VCS_SHIP)],
    skip_if=[Annotated("raw")],
    to=vcs_ship_to,
    note=once_note("Dropped the pipe after `ccx vcs ship`, which would mask its exit status."),
    tests={
        Input(command="ccx vcs ship -m fix | tail -20"): Rewrite(pattern="vcs ship -m fix"),
        Input(command="ccx vcs ship -m fix | head -5"): Rewrite(pattern="vcs ship -m fix"),
        Input(command="ccx vcs ship -m fix | tail -c 100"): Rewrite(pattern="vcs ship -m fix"),
        Input(command="ccx vcs ship -m fix | tail -5 f.txt"): Allow(),
        Input(command="ccx vcs ship -m fix | FOO=1 tail -c 100"): Allow(),
        Input(command="ccx vcs ship -m `printf fix` | tail -20"): Allow(),
        Input(command="ccx exec 'x' | head -3"): Allow(),
        Input(command="ccx vcs ship -m fix"): Allow(),
        Input(command="ccx vcs ship -m fix | tail -20 # ccx:raw"): Allow(),
        Input(command="ccx vcs ship -m '# ccx:raw' | tail -20"): Rewrite(pattern="vcs ship -m"),
    },
)
