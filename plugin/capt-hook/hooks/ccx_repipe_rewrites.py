"""Rewrite three canonical two-stage ``ccx … | head``/``tail`` pipes.

``ccx`` output is already token-budget-capped and carries an explicit overflow footer, so re-slicing
it with ``head``/``tail`` truncates arbitrarily and drops that footer. Three shapes have a faithful
source-bounded equivalent and rewrite in place:

* ``ccx code read <file> --full | head -N`` -> **rewrite** to ``ccx code read <file> --section 1-N``
  (``--full`` dropped, other flags preserved);
* ``ccx repo find <args> | head -N`` -> **rewrite** to ``ccx repo find <args>`` (the pipe stripped —
  ``ccx repo find`` output is deterministic and already budget-capped);
* ``ccx vcs ship <args> | head/tail`` -> **rewrite** to ``ccx vcs ship <args>`` (the pipe stripped —
  ship's report is already lean and budget-capped, and the pipe would mask ship's exit status; this
  branch alone also reaches a ``tail`` sink and ``head -c`` byte mode).

Every other shape runs unchanged, including env-prefixed or wrapped ccx invocations.
"""

from __future__ import annotations

import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command,
)

from .common import ccx_bin
from .headtail_rewrites import headtail_parse

if TYPE_CHECKING:
    from cc_transcript.command import Command


def source_is_ccx(cmd: Command) -> bool:
    return (
        not cmd.env
        and not any(char in cmd.raw for char in "$`")
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


class CcxRepipe(CustomCommandLineCondition):
    """Matches a literal ccx source piped directly to a head/tail sink."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if len(cl.parts) != 2 or cl.parts[0][1] != "|":
            return False
        return source_is_ccx(cl.head) and cl.primary.executable in ("head", "tail")


def repipe_to(evt: BaseHookEvent) -> str | None:
    cl = evt.cmd.line
    src = cl.head
    args = list(src.args)
    ccx = src.executable
    parsed = headtail_parse(cl.primary)
    if args[:2] == ["vcs", "ship"] and (
        (parsed is not None and not parsed[3]) or byte_sink(cl.primary)
    ):
        return " ".join(shlex.quote(t) for t in [ccx, *args])
    if parsed is None:
        return None
    exe, _mode, count, files = parsed
    if exe != "head" or files:
        return None
    if args[:2] == ["code", "read"] and "--full" in args:
        n = count if count is not None else 10
        kept = [a for a in args if a != "--full"]
        return " ".join([shlex.quote(ccx), *(shlex.quote(a) for a in kept), "--section", f"1-{n}"])
    if args[:2] == ["repo", "find"]:
        return " ".join(shlex.quote(t) for t in [ccx, *args])
    return None


def repipe_note(evt: BaseHookEvent) -> str:
    args = evt.cmd.line.head.args
    if args[:2] == ("repo", "find"):
        return "Dropped `| head` — `ccx repo find` output is already token-budget-capped."
    if args[:2] == ("vcs", "ship"):
        return "Dropped the pipe — ship's report is already lean and budget-capped, and the pipe would mask ship's exit status."
    return "Rewrote `ccx code read --full | head -N` → `--section 1-N`: same lines, no dropped overflow footer."


rewrite_command(
    only_if=[CcxRepipe()],
    to=repipe_to,
    note=repipe_note,
    tests={
        Input(command="ccx code read f.go --full | head -5"): Rewrite(pattern="code read f.go --section 1-5"),
        Input(command="ccx code read f.go --full | head"): Rewrite(pattern="--section 1-10"),  # head defaults to 10
        Input(command='ccx repo find "**/*.go" | head -20'): Rewrite(pattern="repo find '**/*.go'"),
        Input(command="ccx vcs ship -m fix | tail -20"): Rewrite(pattern="vcs ship -m fix"),
        Input(command="ccx vcs ship -m fix | head -5"): Rewrite(pattern="vcs ship -m fix"),
        Input(command="ccx vcs ship -m fix | tail -c 100"): Rewrite(pattern="vcs ship -m fix"),  # byte-mode sink also strips
        Input(command="command ccx code read f.go --full | head -5"): Allow(),
        Input(command="FOO=1 ccx code read f.go --full | head -5"): Allow(),
        Input(command="env FOO=1 ccx code read f.go --full | head -5"): Allow(),
        Input(command="env FOO='two words' ccx code read f.go --full | head -5"): Allow(),
        Input(command="FOO='two words' ccx code read f.go --full | head -5"): Allow(),
        Input(command="ccx code grep foo | head -5"): Allow(),
        Input(command="ccx code read f.go --full | tail -5"): Allow(),
        Input(command="ccx code read f.go --full | head -c 100"): Allow(),
        Input(command="ccx code read f.go --full | head --lines=5"): Allow(),
        Input(command="ccx vcs ship -m fix | tail -5 f.txt"): Allow(),
        Input(command="ccx vcs ship -m fix | FOO=1 tail -c 100"): Allow(),
        Input(command="ccx exec 'x' | head -3"): Allow(),
        Input(command="ccx code read $FILE --full | head -5"): Allow(),
        Input(command="ccx repo find $(printf '**/*.go') | head -20"): Allow(),
        Input(command="ccx vcs ship -m `printf fix` | tail -20"): Allow(),
        Input(command="rg foo | head -5"): Allow(),  # non-ccx source → not ours
        Input(command="ccx code grep foo | jq . | head -3"): Allow(),  # three stages → not ours
        Input(command="ccx code read f.go --section 1-5"): Allow(),  # no pipe → not ours
        # `ccx exec` pass-through: the outer line is one command (no top-level pipe), so it never matches.
        Input(
            command="ccx exec 'async def main(): return await sh(\"ccx code read f --full | head\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
