"""Rewrite ``ls -R`` to a ``ccx repo find "<glob>"`` in place, with a ``note`` back to the
model. A workspace-root or module-cache ``ls`` hard-blocks onto the right ``ccx`` entry
point instead. When ``ccx`` cannot be resolved on disk the rewrite falls back to a hard
block, so the guard never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import re

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command,
)

from .common import carries_expansion, ccx_bin

WORKSPACE_ROOT = re.compile(r"^(~|\$HOME|\$\{HOME\})/Code/?$")


class LsRecursive(CustomCommandLineCondition):
    """Matches ``ls -R [dir]`` — a recursive listing that walks the whole tree.

    Plain `ls` and `ls -la` stay allowed; only a recursive flag (`-R`, bundled like
    `-laR`, or `--recursive`) matches. The optional directory argument becomes the
    `ccx repo find "<dir>/**"` glob root, defaulting to `**`. A dir carrying a leading
    ``~`` declines: the glob rides in double quotes where ``~`` stays frozen, so the
    command falls through to Allow and the shell expands the path.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not (
            cl.q.runs("ls")
            and any(
                x == "--recursive" or (x.startswith("-") and not x.startswith("--") and "R" in x)
                for x in cl.primary.args
            )
        ):
            return False
        dirs = [a for a in cl.primary.args if not a.startswith("-")]
        return not (dirs and carries_expansion(dirs[0], tilde_only=True))


def ls_glob(evt: BaseHookEvent) -> str:
    dirs = [a for a in evt.command_line.primary.args if not a.startswith("-")]
    return f"{dirs[0].rstrip('/')}/**" if dirs else "**"


def ls_to(evt: BaseHookEvent) -> str | None:
    if ccx := ccx_bin():
        return f'{ccx} repo find "{ls_glob(evt)}"'
    return None


def ls_note(evt: BaseHookEvent) -> str:
    glob = ls_glob(evt)
    return f'Rewrote `ls -R` → `ccx repo find "{glob}"`: same paths, token-bounded.'


rewrite_command(
    only_if=[LsRecursive()],
    to=ls_to,
    block=(
        "BLOCKED: `ls -R` walks the whole tree into context. "
        'Use `ccx repo find "<glob>"` (or mcp__cc-context__ccx_repo_find), or the built-in Glob tool, '
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
    },
)


class LsWorkspaceRoot(CustomCommandLineCondition):
    """Matches ``ls`` of a workspace or Go module-cache root — a huge, noisy listing.

    ``ls ~/Code``, ``ls $HOME/Code``, and ``ls ~/go/pkg/mod/...`` dump every sibling repo
    or the whole module cache into context; the move is to resolve the one repo/module by
    name. Plain ``ls`` and ``ls <subdir>`` inside a project stay allowed, flags and all.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return cl.q.runs("ls") and any(is_scan_root(a) for a in cl.primary.args if not a.startswith("-"))


def is_scan_root(path: str) -> bool:
    return bool(WORKSPACE_ROOT.match(path)) or "go/pkg/mod" in path


rewrite_command(
    only_if=[LsWorkspaceRoot()],
    to=lambda evt: None,
    block=(
        "BLOCKED: `ls` of a workspace or module-cache root floods context. "
        "Locating a repo/module? `ccx repo locate <name>`. "
        "Orienting a project? `ccx repo overview` (or mcp__cc-context__ccx_repo_overview). "
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
    },
)
