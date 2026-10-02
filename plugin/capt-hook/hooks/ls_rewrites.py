"""Rewrite ``ls -R`` to ``ccx repo find``; block ``ls`` of a workspace or module-cache root."""

from __future__ import annotations

import re
import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    Block,
    Command,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    hook,
    rewrite_command_occurrences,
)
from captain_hook.util.shell import normalize_executable

from .common import carries_expansion, ccx_bin, rewrote_note

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

WORKSPACE_ROOT = re.compile(r"^(?:~|\$(?:HOME|\{HOME\}))/Code/?$")


def is_ls_recursive_command(cmd: Command) -> bool:
    return normalize_executable(cmd.executable) == "ls" and any(
        x == "--recursive" or (x.startswith("-") and not x.startswith("--") and "R" in x) for x in cmd.args
    )


def ls_recursive_dirs(args: tuple[str, ...]) -> list[str]:
    return [a for a in args if not a.startswith("-")]


def ls_recursive_declines(cmd: Command) -> bool:
    dirs = ls_recursive_dirs(cmd.args)
    return bool(dirs and carries_expansion(dirs[0], tilde_only=True))


def ls_glob(args: tuple[str, ...]) -> str:
    dirs = ls_recursive_dirs(args)
    return f"{dirs[0].rstrip('/')}/**" if dirs else "**"


class LsRecursive(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            not occ.piped
            and not occ.command.redirects
            and is_ls_recursive_command(occ.command)
            and not ls_recursive_declines(occ.command)
            for occ in cl.occurrences
        )


def ls_to(evt: BaseHookEvent, occ: "Occurrence") -> str | None:
    cmd = occ.command
    if occ.piped or cmd.redirects:
        return None
    if not is_ls_recursive_command(cmd) or ls_recursive_declines(cmd):
        return None
    if ccx := ccx_bin():
        return f'{shlex.quote(ccx)} repo find "{ls_glob(cmd.args)}"'
    return None


rewrite_command_occurrences(
    only_if=[LsRecursive()],
    skip_if=[Annotated("raw")],
    to=ls_to,
    block='`ls -R` walks the whole tree into context. Run `ccx repo find "<glob>"` to list paths by pattern.',
    note=rewrote_note('ccx repo find "<glob>"', "same paths, token-bounded"),
    tests={
        Input(command="ls -R"): Rewrite(pattern='repo find "**"'),
        Input(command="ls -laR src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls -R src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls --recursive"): Rewrite(pattern='repo find "**"'),
        Input(command="/bin/ls -R src"): Rewrite(pattern='repo find "src/**"'),
        Input(command='"ls" -R'): Rewrite(pattern='repo find "**"'),
        Input(command="sudo ls -R src"): Allow(),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
        Input(command="ls -R ~/proj"): Allow(),
        Input(command="ls -R $d"): Rewrite(pattern='repo find "$d/**"'),
        Input(command="echo x; ls -R src"): Rewrite(pattern='echo x; '),
        Input(command="ls -R src | wc -l"): Allow(),
        Input(command="ls -R src > out.txt"): Allow(),
        Input(command="ls -R src # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; ls -R src"): Rewrite(pattern='repo find "src/**"'),
    },
)


class LsWorkspaceRoot(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            not occ.piped
            and not occ.command.redirects
            and normalize_executable(occ.command.executable) == "ls"
            and any(is_scan_root(a) for a in occ.command.args if not a.startswith("-"))
            for occ in cl.occurrences
        )


def is_scan_root(path: str) -> bool:
    return bool(WORKSPACE_ROOT.match(path)) or "go/pkg/mod" in path


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), LsWorkspaceRoot()],
    skip_if=[Annotated("raw")],
    message=(
        "`ls` of a workspace or module-cache root floods context. "
        "Run `ccx repo locate <name>` to find a repo or module, or `ccx repo overview` to orient."
    ),
    block=True,
    tests={
        Input(command="ls ~/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls $HOME/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls -la ~/Code"): Block(pattern="ccx repo overview"),
        Input(command="ls ~/go/pkg/mod/github.com/foo"): Block(pattern="ccx repo locate"),
        Input(command="/bin/ls ~/Code"): Block(pattern="ccx repo locate"),
        Input(command='"ls" ~/Code'): Block(pattern="ccx repo locate"),
        Input(command="ls internal"): Allow(),
        Input(command="ls"): Allow(),
        Input(command="ls src/Code"): Allow(),
        Input(command="ls ~/Code; echo hi"): Block(pattern="ccx repo locate"),
        Input(command="ls ~/Code | wc -l"): Allow(),
        Input(command="ls ~/Code # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; ls ~/Code"): Block(pattern="ccx repo locate"),
    },
)
