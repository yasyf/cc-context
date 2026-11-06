"""Run a raw ``git rebase`` in a shallow clone through ``ccx vcs rebase``, which first deepens to the merge base."""

from __future__ import annotations

import shlex
import subprocess
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    Input,
    PreToolUseEvent,
    Rewrite,
    Rewritten,
    WalkContext,
    rewrite_command_occurrences,
)

from .common import ccx_bin, ccx_supports, first_sight, rewrote_text, spells_argv

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

REBASE_NOTE = rewrote_text(
    "ccx vcs rebase -- <git rebase args>",
    "same arguments and exit code, after it deepens this shallow clone to the merge base",
)

SHALLOW = {"git -C /usr rev-parse --is-shallow-repository": "true\n"}


def shallow_clone(cwd: Path | None) -> bool:
    if cwd is None:
        return False
    probe = subprocess.run(
        ["git", "-C", str(cwd), "rev-parse", "--is-shallow-repository"], capture_output=True, text=True
    )
    return probe.returncode == 0 and probe.stdout.strip() == "true"


def rebase_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> Rewritten | None:
    command = occ.command
    if (
        occ.nesting
        or not ctx.spliceable
        or Path(command.executable).name != "git"
        or command.args[:1] != ("rebase",)
        or not spells_argv(command, occ.line.raw, command.span)
        or not shallow_clone(ctx.cwd)
        or not (ccx := ccx_bin())
        or not ccx_supports("vcs", "rebase", flag="shallow clone")
    ):
        return None
    return Rewritten(
        " ".join([shlex.quote(ccx), "vcs", "rebase", "--", *(word.raw for word in command.words[2:])]),
        note=first_sight(evt, REBASE_NOTE),
    )


rewrite_command_occurrences(
    skip_if=[Annotated("raw")],
    visit=rebase_visit,
    tests={
        Input(command="git rebase origin/main", cwd="/usr", commands=SHALLOW): Rewrite(
            pattern="vcs rebase -- origin/main"
        ),
        Input(command="git rebase -q --onto origin/main HEAD~3", cwd="/usr", commands=SHALLOW): Rewrite(
            pattern="vcs rebase -- -q --onto origin/main HEAD~3"
        ),
        Input(command="git rebase --continue", cwd="/usr", commands=SHALLOW): Rewrite(
            pattern="vcs rebase -- --continue"
        ),
        Input(command="git fetch -q origin && git rebase origin/main", cwd="/usr", commands=SHALLOW): Rewrite(
            pattern="git fetch -q origin && "
        ),
        Input(
            command="cd /usr/lib && git rebase origin/main",
            cwd="/",
            commands={"git -C /usr/lib rev-parse --is-shallow-repository": "true\n"},
        ): Rewrite(pattern="vcs rebase -- origin/main"),
        Input(
            command="git rebase origin/main",
            cwd="/usr",
            commands={"git -C /usr rev-parse --is-shallow-repository": "false\n"},
        ): Allow(),
        Input(command="git rebase origin/main # ccx:raw", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="git -C /usr rebase origin/main", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="GIT_SEQUENCE_EDITOR=true git rebase -i origin/main", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="echo git rebase origin/main", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="echo $(git rebase origin/main)", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="git merge origin/main", cwd="/usr", commands=SHALLOW): Allow(),
        Input(command="ccx vcs rebase -- origin/main", cwd="/usr", commands=SHALLOW): Allow(),
    },
)
