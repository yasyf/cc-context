"""Rewrite a ``find`` enumeration to a ``ccx repo find "<glob>"`` in place, with a ``note``
back to the model. Forms with no glob equivalent — a ``-path``/``-regex`` filter, a
``-type f`` walk with no dir — hard-block onto the right ``ccx`` entry point instead. When
``ccx`` cannot be resolved on disk the rewrite falls back to a hard block, so the guard
never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import os

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


class FindEnumeration(CustomCommandLineCondition):
    """Matches ``find`` used to *list* matches (no action flag) — a context flood.

    A ``-name``/``-iname``/``-path``/``-regex`` filter or a scoped ``-type f`` walk both
    enumerate paths; a find that ends in an action — `-exec`, `-delete`, `-print0`
    (almost always `| xargs`) — is doing work, not flooding, so it is left alone. The
    steer is `ccx repo find` (scoped) or `ccx repo overview` (whole repo) / Glob. A dir
    operand carrying a leading ``~`` declines: the glob rides in double quotes where ``$``
    expands but ``~`` stays frozen, so the command falls through to Allow and the shell
    expands the path.
    """

    ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
    NAME_FLAGS = ("-name", "-iname")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not cl.q.runs("find") or cl.q.uses_redirect():
            return False
        if any(cl.q.contains_token(a) for a in self.ACTIONS):
            return False
        args = cl.primary.args
        path = args[0] if args and not args[0].startswith("-") else None
        if path is not None and carries_expansion(path, tilde_only=True):
            return False
        has_filter = any(cl.q.contains_token(f) for f in ("-name", "-iname", "-path", "-regex"))
        return has_filter or args_type_f(args)


def args_type_f(args: tuple[str, ...]) -> bool:
    return any(a == "-type" and i + 1 < len(args) and args[i + 1] == "f" for i, a in enumerate(args))


def find_glob(args: tuple[str, ...]) -> str | None:
    """The ``ccx repo find`` glob body for an enumeration, or None to hard-block.

    A ``-name``/``-iname`` filter maps to `<dir>/**/<pat>`; a *scoped* ``-type f`` walk to
    `<dir>/**`. A ``-path``/``-regex`` filter (no glob equivalent) and a ``-type f`` with no
    dir *or* a whole-repo dir (orient the repo with ``ccx repo overview`` instead) both
    return None. The dir operand is ``normpath``-cleaned first, so ``.//`` and ``./.`` collapse
    to the root (block) just like a bare ``.``, and ``src//`` to ``src`` (rewrite ``src/**``).
    """
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    raw = args[0] if args and not args[0].startswith("-") else None
    path = os.path.normpath(raw) if raw is not None else "."
    if flag:
        prefix = "" if path == "." else f"{path}/"
        return f"{prefix}**/{args[args.index(flag) + 1]}"
    if args_type_f(args):
        return None if path == "." else f"{path}/**"
    return None


def find_to(evt: BaseHookEvent) -> str | None:
    glob = find_glob(evt.command_line.primary.args)
    if glob is not None and (ccx := ccx_bin()):
        return f'{ccx} repo find "{glob}"'
    return None


def find_note(evt: BaseHookEvent) -> str:
    args = evt.command_line.primary.args
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    src = f"{flag} {args[args.index(flag) + 1]}" if flag else "-type f"
    path = args[0] if args and not args[0].startswith("-") else "."
    return f'Rewrote `find {path} {src}` → `ccx repo find "{find_glob(args)}"`: same paths, token-bounded.'


rewrite_command(
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
    },
)
