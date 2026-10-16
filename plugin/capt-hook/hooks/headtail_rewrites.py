"""Rewrite a standalone bounded single-file ``head`` to ``ccx code read --section 1-N``."""

from __future__ import annotations

import os
import shlex
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    Command,
    CommandLine,
    CustomCommandLineCondition,
    FileFixture,
    Input,
    Rewrite,
    rewrite_command_occurrences,
)

from .common import carries_expansion, ccx_bin, command_expands, output_reaches_terminal, rewrote_note

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence


MAX_COUNT_DIGITS = 4000


def line_count(raw: str) -> int | None:
    return int(raw) if raw.isascii() and raw.isdigit() and len(raw) <= MAX_COUNT_DIGITS else None


def headtail_parse(cmd: Command) -> tuple[str, int | None, list[str]] | None:
    exe = cmd.executable
    if exe not in ("head", "tail") or cmd.env:
        return None
    match list(cmd.args):
        case []:
            return exe, None, []
        case ["-n", raw_count, *files] if (count := line_count(raw_count)) is not None:
            return exe, count, files
        case [short_count, *files] if short_count.startswith("-") and (count := line_count(short_count[1:])) is not None:
            return exe, count, files
        case files if not any(arg.startswith("-") for arg in files):
            return exe, None, files
        case _:
            return None


def headtail_file_parts(occ: Occurrence) -> tuple[str, int | None, list[str]] | None:
    cmd = occ.command
    if not output_reaches_terminal(occ) or command_expands(cmd) or (parsed := headtail_parse(cmd)) is None:
        return None
    return parsed if parsed[2] and not any(carries_expansion(file) for file in parsed[2]) else None


def headtail_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if (parsed := headtail_file_parts(occ)) is None:
        return None
    exe, count, files = parsed
    if exe != "head" or len(files) != 1:
        return None
    path = Path(files[0])
    if not path.is_absolute():
        if evt.cwd is None:
            return None
        path = evt.cwd / path
    if not os.path.isfile(path) or (ccx := ccx_bin()) is None:
        return None
    n = count if count is not None else 10
    return f"{shlex.quote(ccx)} code read {shlex.quote(files[0])} --section 1-{n}"


class HeadTailFile(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(headtail_to(evt, occ) is not None for occ in cl.occurrences)


rewrite_command_occurrences(
    only_if=[HeadTailFile()],
    skip_if=[Annotated("raw")],
    to=headtail_to,
    note=rewrote_note("ccx code read --section", "same lines, token-bounded"),
    tests={
        Input(command="head -40 {file}", file=FileFixture(size=64, name="f.go")): Rewrite(
            pattern="f.go --section 1-40"
        ),
        Input(command="head -n 40 {file}", file=FileFixture(size=64, name="f.go")): Rewrite(
            pattern="--section 1-40"
        ),
        Input(command="head {file}", file=FileFixture(size=64, name="f.go")): Rewrite(
            pattern="--section 1-10"
        ),
        Input(command="tail -n 20 f.go"): Allow(),
        Input(command="tail -20 f.go"): Allow(),
        Input(command="head -c 100 f.go"): Allow(),
        Input(command="head --bytes=100 f.go"): Allow(),
        Input(command="head --lines=40 f.go"): Allow(),
        Input(command="head -n40 f.go"): Allow(),
        Input(command=f"head -n {'9' * 5000} f.go"): Allow(),
        Input(command=f"head -{'9' * 5000} f.go"): Allow(),
        Input(command="head a.go b.go"): Allow(),
        Input(command="FOO=1 head -40 f.go"): Allow(),
        Input(command="env FOO=1 head -40 f.go"): Allow(),
        Input(command="rg foo | head -5"): Allow(),
        Input(command="cat f | tail -20"): Allow(),
        Input(command="head -5"): Allow(),
        Input(command="head -40 /nonexistent/capt-hook-doctrine.go"): Allow(),
        Input(command="head -40 $d/host.go"): Allow(),
        Input(command="head -20 ~/.claude/cache/log.txt"): Allow(),
        Input(command="head -40 $(printf f.go)"): Allow(),
        Input(command="head -40 `printf f.go`"): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"head -40 f.go\")\nasyncio.run(main())'"
        ): Allow(),
        Input(command="echo x; head -40 {file}", file=FileFixture(size=64, name="f.go")): Rewrite(
            pattern="echo x; "
        ),
        Input(command="echo x; tail -20 f.go"): Allow(),
        Input(command="head -n 5 {file}", file=FileFixture(size=64, name="file.py")): Rewrite(
            pattern="file.py --section 1-5"
        ),
        Input(command="TITLE=$(head -1 {file})", file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command='gh pr edit 1 --title "$(head -1 {file})"', file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command="TITLE=`head -1 {file}`", file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command='bash -c "head -1 {file}"', file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command="TITLE=$(git log -1 --format=%s | head -1)"): Allow(),
        Input(command="head -1 {file} | cat", file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command="{ head -1 {file}; } > out.txt", file=FileFixture(size=64, name="commit.txt")): Allow(),
        Input(command="head -40 {file} # ccx:raw", file=FileFixture(size=64, name="f.go")): Allow(),
        Input(command="echo '# ccx:raw'; head -40 {file}", file=FileFixture(size=64, name="f.go")): Rewrite(
            pattern="--section 1-40"
        ),
    },
)
