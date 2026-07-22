"""Rewrite ``ls -R`` to a ``ccx repo find "<glob>"`` in place, with a ``note`` back to the
model. A workspace-root or module-cache ``ls`` hard-blocks onto the right ``ccx`` entry
point instead. When ``ccx`` cannot be resolved on disk the rewrite falls back to a hard
block, so the guard never emits a broken ``ccx: command not found``.
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
    rewrite_command,
    rewrite_command_occurrences,
)

from .common import carries_expansion, ccx_bin

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

WORKSPACE_ROOT = re.compile(r"^(~|\$HOME|\$\{HOME\})/Code/?$")


def is_ls_recursive_command(cmd: Command) -> bool:
    """Report whether ``cmd`` is ``ls`` with a recursive flag (bundled or ``--recursive``)."""
    return cmd.executable == "ls" and any(
        x == "--recursive" or (x.startswith("-") and not x.startswith("--") and "R" in x) for x in cmd.args
    )


def ls_recursive_dirs(args: tuple[str, ...]) -> list[str]:
    return [a for a in args if not a.startswith("-")]


def ls_recursive_declines(cmd: Command) -> bool:
    """A dir carrying a leading ``~`` declines: the glob rides in double quotes where ``~``
    stays frozen, so the occurrence falls through to Allow and the shell expands the path.
    """
    dirs = ls_recursive_dirs(cmd.args)
    return bool(dirs and carries_expansion(dirs[0], tilde_only=True))


def ls_glob(args: tuple[str, ...]) -> str:
    dirs = ls_recursive_dirs(args)
    return f"{dirs[0].rstrip('/')}/**" if dirs else "**"


class LsRecursive(CustomCommandLineCondition):
    """Matches a ``;``/``&&``/``|``-joined line carrying an ``ls -R [dir]`` occurrence — a
    recursive listing that walks the whole tree.

    Plain `ls` and `ls -la` stay allowed; only a recursive flag (`-R`, bundled like
    `-laR`, or `--recursive`) matches. Gates the hook on any occurrence being a genuine,
    rewritable recursive `ls` (non-tilde); the per-occurrence `to` then declines a piped
    or redirected occurrence, and rewrites the rest in place — untouched siblings (an
    `echo` before the `;`, for instance) survive byte-for-byte.
    """

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
        return None  # only rewrite what the splice can reproduce in full
    if not is_ls_recursive_command(cmd) or ls_recursive_declines(cmd):
        return None
    if ccx := ccx_bin():
        return f'{shlex.quote(ccx)} repo find "{ls_glob(cmd.args)}"'
    return None


def ls_note(evt: BaseHookEvent, pairs: "list[tuple[Occurrence, str]]") -> str:
    globs = ", ".join(f'"{ls_glob(occ.command.args)}"' for occ, _ in pairs)
    return f"Rewrote `ls -R` → `ccx repo find {globs}`: same paths, token-bounded."


rewrite_command_occurrences(
    only_if=[LsRecursive()],
    to=ls_to,
    block=(
        "BLOCKED: `ls -R` walks the whole tree into context. "
        'Use `ccx repo find "<glob>"`, or the built-in Glob tool, '
        "to find paths by pattern. Plain `ls` and `ls -la` stay allowed."
    ),
    note=ls_note,
    tests={
        Input(command="ls -R"): Rewrite(pattern='repo find "**"'),
        Input(command="ls -laR src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls -R src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls --recursive"): Rewrite(pattern='repo find "**"'),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
        # A leading-`~` dir declines to rewrite — the double-quoted glob would freeze it; the shell expands it.
        Input(command="ls -R ~/proj"): Allow(),
        Input(command="ls -R $d"): Rewrite(pattern='repo find "$d/**"'),  # `$` expands inside the double-quoted glob
        # Compound line: only the `ls -R` occurrence rewrites; the sibling survives verbatim.
        Input(command="echo x; ls -R src"): Rewrite(pattern='echo x; '),
        # Piped and redirected occurrences keep today's exemption — never converted to blocks.
        Input(command="ls -R src | wc -l"): Allow(),
        Input(command="ls -R src > out.txt"): Allow(),
    },
)


class LsWorkspaceRoot(CustomCommandLineCondition):
    """Matches ``ls`` of a workspace or Go module-cache root — a huge, noisy listing —
    in ANY occurrence of a ``;``/``&&``/``|``-joined line, not just the primary command.

    ``ls ~/Code``, ``ls $HOME/Code``, and ``ls ~/go/pkg/mod/...`` dump every sibling repo
    or the whole module cache into context; the move is to resolve the one repo/module by
    name. Plain ``ls`` and ``ls <subdir>`` inside a project stay allowed, flags and all.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            not occ.piped
            and not occ.command.redirects
            and occ.command.executable == "ls"
            and any(is_scan_root(a) for a in occ.command.args if not a.startswith("-"))
            for occ in cl.occurrences
        )


def is_scan_root(path: str) -> bool:
    return bool(WORKSPACE_ROOT.match(path)) or "go/pkg/mod" in path


rewrite_command(
    only_if=[LsWorkspaceRoot()],
    to=lambda evt: None,
    block=(
        "BLOCKED: `ls` of a workspace or module-cache root floods context. "
        "Locating a repo/module? `ccx repo locate <name>`. "
        "Orienting a project? `ccx repo overview`. "
        "Plain `ls` and `ls <subdir>` inside a project stay allowed."
    ),
    tests={
        Input(command="ls ~/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls $HOME/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls -la ~/Code"): Block(pattern="ccx repo overview"),
        Input(command="ls ~/go/pkg/mod/github.com/foo"): Block(pattern="ccx repo locate"),
        Input(command="ls internal"): Allow(),  # a project subdir
        Input(command="ls"): Allow(),
        Input(command="ls src/Code"): Allow(),  # not the workspace root
        # A workspace-root ls anywhere on the line blocks the whole line, even when the
        # primary command is innocuous.
        Input(command="ls ~/Code; echo hi"): Block(pattern="ccx repo locate"),
        # Piped and redirected occurrences keep today's exemption.
        Input(command="ls ~/Code | wc -l"): Allow(),
    },
)
