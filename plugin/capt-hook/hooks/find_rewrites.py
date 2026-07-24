"""Rewrite positively identified ``find`` enumerations to ``ccx repo find "<glob>"``.
Ambiguous or unrewritable forms pass through untouched.
"""

from __future__ import annotations

import os
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

ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
NAME_FLAGS = ("-name", "-iname")


def args_type_f(args: tuple[str, ...]) -> bool:
    return any(a == "-type" and i + 1 < len(args) and args[i + 1] == "f" for i, a in enumerate(args))


def find_name_filter(args: tuple[str, ...]) -> str | None:
    return next((args[i + 1] for i, a in enumerate(args) if a in NAME_FLAGS and i + 1 < len(args)), None)


def is_find_enumeration(cmd: Command) -> bool:
    """Report whether ``cmd`` is ``find`` used to *list* matches (no action flag) — a context
    flood.

    A valid ``-name``/``-iname`` filter or a ``-type f`` walk enumerates paths. Action
    forms, unsupported filters, and expansion-bearing dirs fall through untouched.
    """
    if cmd.executable != "find" or cmd.redirects:
        return False
    args = cmd.args
    if any(a in ACTIONS or a in ("-path", "-regex") for a in args):
        return False
    path = args[0] if args and not args[0].startswith("-") else None
    if path is not None and carries_expansion(path):
        return False
    return find_name_filter(args) is not None or args_type_f(args)


class FindEnumeration(CustomCommandLineCondition):
    """Matches a ``;``/``&&``/``|``-joined line carrying a rewritable ``find`` enumeration
    occurrence — a context flood. The steer is ``ccx repo find`` or Glob.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(not occ.piped and is_find_enumeration(occ.command) for occ in cl.occurrences)


def find_glob(args: tuple[str, ...]) -> str | None:
    """Return the ``ccx repo find`` glob for a positively identified enumeration.

    A ``-name``/``-iname`` filter maps to `<dir>/**/<pat>`; a ``-type f`` walk maps to
    `<dir>/**`, with the repository root represented by `**`.
    """
    raw = args[0] if args and not args[0].startswith("-") else None
    path = os.path.normpath(raw) if raw is not None else "."
    if pattern := find_name_filter(args):
        prefix = "" if path == "." else f"{path}/"
        return f"{prefix}**/{pattern}"
    if args_type_f(args):
        return "**" if path == "." else f"{path}/**"
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
    note=find_note,
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='repo find "**/*.go"'),
        # A piped enumeration keeps today's exemption — never converted to a block.
        Input(command="find . -name '*.go' | wc -l"): Allow(),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='repo find "src/**/*.PY"'),
        Input(command="find src -type f"): Rewrite(pattern='repo find "src/**"'),  # bare -type f, scoped
        Input(command="find . -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find .// -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find ./. -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find src// -type f"): Rewrite(pattern='repo find "src/**"'),  # trailing slashes cleaned
        # Expansion-bearing dirs decline so the shell can expand the original command.
        Input(command="find ~/src -type f"): Allow(),
        Input(command="find ~/src -name '*.go'"): Allow(),
        Input(command="find $d -type f"): Allow(),
        Input(command="find . -name"): Allow(),
        Input(command="find . -path '*/gen/*'"): Allow(),
        Input(command="find . -regex '.*\\.go'"): Allow(),
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # -type d is not the file flood we steer
        # Compound line: the `cd` occurrence survives verbatim so the glob roots after it.
        Input(command="cd src && find . -name '*.go'"): Rewrite(pattern="cd src && "),
    },
)
