"""Rewrite a ``find`` enumeration occurrence to a ``ccx repo find "<glob>"`` in place, with a
``note`` back to the model. Forms with no glob equivalent — a ``-path``/``-regex`` filter, a
``-type f`` walk with no dir — hard-block onto the right ``ccx`` entry point instead. When
``ccx`` cannot be resolved on disk the rewrite falls back to a hard block, so the guard
never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import os
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

ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
NAME_FLAGS = ("-name", "-iname")


def args_type_f(args: tuple[str, ...]) -> bool:
    return any(a == "-type" and i + 1 < len(args) and args[i + 1] == "f" for i, a in enumerate(args))


def is_find_enumeration(cmd: Command) -> bool:
    """Report whether ``cmd`` is ``find`` used to *list* matches (no action flag) — a context
    flood.

    A ``-name``/``-iname``/``-path``/``-regex`` filter or a scoped ``-type f`` walk both
    enumerate paths; a find that ends in an action — ``-exec``, ``-delete``, ``-print0``
    (almost always ``| xargs``) — is doing work, not flooding, so it is left alone. A dir
    operand carrying a leading ``~`` declines: the glob rides in double quotes where ``$``
    expands but ``~`` stays frozen, so the command falls through to Allow and the shell
    expands the path.
    """
    if cmd.executable != "find" or cmd.redirects:
        return False
    args = cmd.args
    if any(a in ACTIONS for a in args):
        return False
    path = args[0] if args and not args[0].startswith("-") else None
    if path is not None and carries_expansion(path, tilde_only=True):
        return False
    has_filter = any(f in args for f in ("-name", "-iname", "-path", "-regex"))
    return has_filter or args_type_f(args)


class FindEnumeration(CustomCommandLineCondition):
    """Matches a ``;``/``&&``/``|``-joined line carrying a rewritable ``find`` enumeration
    occurrence — a context flood. The steer is ``ccx repo find`` (scoped) or ``ccx repo
    overview`` (whole repo) / Glob.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(not occ.piped and is_find_enumeration(occ.command) for occ in cl.occurrences)


def find_glob(args: tuple[str, ...]) -> str | None:
    """The ``ccx repo find`` glob body for an enumeration, or None to hard-block.

    A ``-name``/``-iname`` filter maps to `<dir>/**/<pat>`; a *scoped* ``-type f`` walk to
    `<dir>/**`. A ``-path``/``-regex`` filter (no glob equivalent) and a ``-type f`` with no
    dir *or* a whole-repo dir (orient the repo with ``ccx repo overview`` instead) both
    return None. The dir operand is ``normpath``-cleaned first, so ``.//`` and ``./.`` collapse
    to the root (block) just like a bare ``.``, and ``src//`` to ``src`` (rewrite ``src/**``).
    """
    flag = next((a for a in args if a in NAME_FLAGS), None)
    raw = args[0] if args and not args[0].startswith("-") else None
    path = os.path.normpath(raw) if raw is not None else "."
    if flag:
        prefix = "" if path == "." else f"{path}/"
        return f"{prefix}**/{args[args.index(flag) + 1]}"
    if args_type_f(args):
        return None if path == "." else f"{path}/**"
    return None


def find_to(evt: BaseHookEvent, occ: "Occurrence") -> str | None:
    cmd = occ.command
    if occ.piped or cmd.redirects:
        return None  # only rewrite what the splice can reproduce in full
    if not is_find_enumeration(cmd):
        return None
    glob = find_glob(cmd.args)
    if glob is not None and (ccx := ccx_bin()):
        return f'{shlex.quote(ccx)} repo find "{glob}"'
    return None


def find_note(evt: BaseHookEvent, pairs: "list[tuple[Occurrence, str]]") -> str:
    globs = ", ".join(f'"{find_glob(occ.command.args)}"' for occ, _ in pairs)
    return f"Rewrote `find` → `ccx repo find {globs}`: same paths, token-bounded."


rewrite_command_occurrences(
    only_if=[FindEnumeration()],
    to=find_to,
    block=(
        "BLOCKED: `find` enumeration floods context. "
        'Scoped to a dir? `ccx repo find "<dir>/**"` (mcp__cc-context__ccx_repo_find), or the built-in '
        "Glob tool. Orienting the whole repo? `ccx repo overview` (mcp__cc-context__ccx_repo_overview). "
        "Escape hatch — need an action: keep the `-exec`/`-delete`/`-print0 | xargs` form."
    ),
    note=find_note,
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='repo find "**/*.go"'),
        # A piped enumeration keeps today's exemption — never converted to a block.
        Input(command="find . -name '*.go' | wc -l"): Allow(),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='repo find "src/**/*.PY"'),
        Input(command="find src -type f"): Rewrite(pattern='repo find "src/**"'),  # bare -type f, scoped
        Input(command="find . -type f"): Block(pattern="ccx repo overview"),  # whole-repo walk → orient, not `**`
        Input(command="find -type f"): Block(pattern="ccx repo overview"),  # no dir → orient
        Input(command="find .// -type f"): Block(pattern="ccx repo overview"),  # `.//` normalizes to root → orient
        Input(command="find ./. -type f"): Block(pattern="ccx repo overview"),  # `./.` normalizes to root → orient
        Input(command="find src// -type f"): Rewrite(pattern='repo find "src/**"'),  # trailing slashes cleaned
        # A leading-`~` dir declines to rewrite — the double-quoted glob would freeze it; the shell expands it.
        Input(command="find ~/src -type f"): Allow(),
        Input(command="find ~/src -name '*.go'"): Allow(),
        Input(command="find $d -type f"): Rewrite(pattern='repo find "$d/**"'),  # `$` expands inside the double-quoted glob
        Input(command="find . -path '*/gen/*'"): Block(pattern="ccx repo find"),  # -path stays a block
        Input(command="find . -regex '.*\\.go'"): Block(),  # -regex stays a block
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # -type d is not the file flood we steer
        # Compound line: the `cd` occurrence survives verbatim so the glob roots after it.
        Input(command="cd src && find . -name '*.go'"): Rewrite(pattern="cd src && "),
    },
)
