"""Rewrite positively identified ``find`` enumerations to ``ccx repo find "<glob>"``."""

from __future__ import annotations

import os
import shlex
from dataclasses import replace
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    Input,
    Option,
    Rewrite,
    rewrite_command_occurrences,
)
from captain_hook.command_schemas import FIND

from .common import call_of, carries_expansion, ccx_bin, rewrote_note

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

NAME_FLAGS = ("-name", "-iname")
TYPE_FLAGS = ("-type",)
PRINT_FLAGS = ("-print",)
LISTING_FLAGS = (*NAME_FLAGS, *TYPE_FLAGS, *PRINT_FLAGS)

FIND_LISTING = replace(
    FIND,
    options=(
        *(
            replace(option, flags=tuple(flag for flag in option.flags if flag not in LISTING_FLAGS))
            for option in FIND.options
            if option.name != "command"
        ),
        Option("pattern", NAME_FLAGS),
        Option("type", TYPE_FLAGS),
        Option("print", PRINT_FLAGS, bool),
    ),
)


def find_glob(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    cmd = occ.command
    if occ.piped or cmd.redirects or cmd.executable != "find":
        return None
    arguments = FIND_LISTING.bind(call_of(evt, occ))
    if not arguments.complete or arguments.values.get("predicate") or arguments.values.get("expression"):
        return None
    roots = arguments.values["roots"]
    if len(roots) != 1 or roots[0] is None or carries_expansion(str(roots[0])):
        return None
    root = os.path.normpath(str(roots[0]))
    patterns = arguments.values.get("pattern", ())
    types = arguments.values.get("type", ())
    if patterns:
        prefix = "" if root == "." else f"{root}/"
        return f"{prefix}**/{patterns[0]}"
    if types == ("f",):
        return "**" if root == "." else f"{root}/**"
    return None


def find_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if (glob := find_glob(evt, occ)) is None or (ccx := ccx_bin()) is None:
        return None
    return f'{shlex.quote(ccx)} repo find "{glob}"'


rewrite_command_occurrences(
    skip_if=[Annotated("raw")],
    to=find_to,
    note=rewrote_note('ccx repo find "<glob>"', "same paths, token-bounded"),
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='repo find "**/*.go"'),
        Input(command="find . -name '*.go' | wc -l"): Allow(),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='repo find "src/**/*.PY"'),
        Input(command="find src -type f"): Rewrite(pattern='repo find "src/**"'),
        Input(command="find . -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find .// -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find ./. -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find src// -type f"): Rewrite(pattern='repo find "src/**"'),
        Input(command="find ~/src -type f"): Allow(),
        Input(command="find ~/src -name '*.go'"): Allow(),
        Input(command="find $d -type f"): Allow(),
        Input(command="find . -name"): Allow(),
        Input(command="find . -path '*/gen/*'"): Allow(),
        Input(command="find . -regex '.*\\.go'"): Allow(),
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),
        Input(command="find src lib -name '*.go'"): Allow(),
        Input(command="find . -name '*.go' -mtime -1"): Allow(),
        Input(command="cd src && find . -name '*.go'"): Rewrite(pattern="cd src && "),
        Input(command="find . -name '*.go' # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; find . -name '*.go'"): Rewrite(pattern='repo find "**/*.go"'),
    },
)
