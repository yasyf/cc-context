"""Rewrite or block a two-stage ``ccx … | head``/``tail`` pipe.

``ccx`` output is already token-budget-capped and carries an explicit overflow footer, so re-slicing
it with ``head``/``tail`` truncates arbitrarily and drops that footer. Two shapes have a faithful
source-bounded equivalent and rewrite in place; every other ``ccx | head``/``tail`` hard-blocks onto
bounding at the source (``--budget``/``--section``) or ``ccx exec``:

* ``ccx code read <file> --full | head -N`` -> **rewrite** to ``ccx code read <file> --section 1-N``
  (``--full`` dropped, other flags preserved);
* ``ccx repo find <args> | head -N`` -> **rewrite** to ``ccx repo find <args>`` (the pipe stripped —
  ``ccx repo find`` output is deterministic and already budget-capped).

This judges only a ccx *source* piped into a head/tail *sink*. A head/tail on a FILE operand is
:class:`~hooks.headtail_rewrites.HeadTailFile`'s job (a pipe sink is its documented non-goal); a
three-plus-stage pipeline (``ccx … | jq | head``) and a non-ccx source never match here.
"""

from __future__ import annotations

import re
import shlex
from pathlib import Path
from typing import TYPE_CHECKING

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

from .common import ccx_bin
from .headtail_rewrites import headtail_parse

if TYPE_CHECKING:
    from cc_transcript.command import Command

# A leading ``VAR=`` assignment token, for spotting an env-wrapper assignment among the wrapper words.
ASSIGN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")


def prefix_needs_requote(head: Command) -> bool:
    """Whether the env/wrapper prefix carries a value ``shlex.quote`` can't faithfully re-emit.

    A bare ``FOO=val`` lands in ``head.env`` with its value unquoted; a glued ``env FOO=val`` keeps the
    value — inner quotes and all — as a wrapper argv token. A value carrying whitespace or a quote char
    round-trips unfaithfully (``env FOO='two words'`` double-wraps under ``shlex.quote``), so the rewrite
    bails to the block instead of splicing a corrupt prefix.
    """
    stripped = len(head.argv) - len(head.unwrapped.argv)
    values = [v for _k, v in head.env]
    values += [tok.split("=", 1)[1] for tok in head.argv[:stripped] if ASSIGN.match(tok)]
    return any(any(c in v for c in " \t\n'\"") for v in values)


def source_is_ccx(cmd: Command) -> bool:
    """Whether a pipeline's source command runs the ``ccx`` binary, seen through prefix laundering.

    ``unwrapped`` strips a leading ``command``/``env``/``sudo``/… wrapper and any ``VAR=val``
    prefix, so ``command ccx …`` and ``FOO=1 ccx …`` resolve to the same ``ccx`` source a bare
    invocation does — the same way :meth:`Command.runs` defeats the launderers elsewhere in the pack.
    """
    exe = cmd.unwrapped.executable
    return exe == ccx_bin() or Path(exe).name == "ccx"


def source_prefix(head: Command) -> list[str]:
    """The env-assignment and wrapper tokens before ``ccx``, quoted so the rewrite preserves them.

    A rewrite drops anything it does not re-emit, so ``FOO=1 ccx … | head`` would lose ``FOO=1`` and
    ``command ccx … | head`` its ``command`` builtin. This reconstructs both: the leading ``VAR=val``
    assignments (parsed into ``head.env``) plus the wrapper words :meth:`Command.unwrapped` strips.
    """
    stripped = len(head.argv) - len(head.unwrapped.argv)
    return [
        *(f"{k}={shlex.quote(v)}" for k, v in head.env),
        *(shlex.quote(t) for t in head.argv[:stripped]),
    ]


class CcxRepipe(CustomCommandLineCondition):
    """Matches a two-stage ``ccx … | head``/``tail`` pipe — ccx source, head/tail sink.

    Exactly two stages joined by a pipe, the first a ``ccx`` invocation, the last ``head`` or
    ``tail``. The two source-bounded shapes rewrite (via :func:`repipe_to`); every other ccx|head/tail
    (a ``tail`` sink, ``head -c`` byte mode, or any other subcommand) hard-blocks. A three-plus-stage
    pipeline and a non-ccx source fall through untouched — and a head/tail on a FILE operand is
    :class:`~hooks.headtail_rewrites.HeadTailFile`'s job (a pipe sink is its documented non-goal).
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if len(cl.parts) != 2 or cl.parts[0][1] != "|":
            return False
        return source_is_ccx(cl.head) and cl.primary.executable in ("head", "tail")


def repipe_to(evt: BaseHookEvent) -> str | None:
    """The source-bounded rewrite for the two faithful shapes, else ``None`` to hard-block."""
    cl = evt.cmd.line
    if prefix_needs_requote(cl.head):  # a re-quote-needing env value would corrupt the spliced prefix → block
        return None
    _exe, mode, count, files = headtail_parse(cl.primary)
    if _exe != "head" or mode != "line" or files:  # tail, byte mode, or a file operand → block
        return None
    src = cl.head.unwrapped
    args = list(src.args)
    ccx = src.executable
    prefix = source_prefix(cl.head)
    if args[:2] == ["code", "read"] and "--full" in args:
        n = count if count is not None else 10
        kept = [a for a in args if a != "--full"]
        return " ".join([*prefix, shlex.quote(ccx), *(shlex.quote(a) for a in kept), "--section", f"1-{n}"])
    if args[:2] == ["repo", "find"]:
        return " ".join([*prefix, *(shlex.quote(t) for t in [ccx, *args])])
    return None


def repipe_note(evt: BaseHookEvent) -> str:
    if evt.cmd.line.head.unwrapped.args[:2] == ("repo", "find"):
        return "Dropped `| head` — `ccx repo find` output is already token-budget-capped."
    return "Rewrote `ccx code read --full | head -N` → `--section 1-N`: same lines, no dropped overflow footer."


rewrite_command(
    only_if=[CcxRepipe()],
    to=repipe_to,
    block=(
        "BLOCKED: ccx output is already token-budget-capped — re-truncating with head/tail slices "
        "arbitrarily and drops the overflow footer. Bound at the source: --budget N (tokens) or "
        "--section A-B; post-process with `ccx exec`. Bounded readers: "
        "`ccx code read` (ccx_code_read), `ccx code grep` (ccx_code_grep), `ccx repo find` (ccx_repo_find)."
    ),
    note=repipe_note,
    tests={
        Input(command="ccx code read f.go --full | head -5"): Rewrite(pattern="code read f.go --section 1-5"),
        Input(command="ccx code read f.go --full | head"): Rewrite(pattern="--section 1-10"),  # head defaults to 10
        Input(command='ccx repo find "**/*.go" | head -20'): Rewrite(pattern="repo find '**/*.go'"),
        # Prefix laundering no longer bypasses: `command`/`env` wrappers unwrap to the ccx source, and
        # the leading `VAR=val`/wrapper prefix rides into the rewrite verbatim instead of being dropped.
        Input(command="command ccx code read f.go --full | head -5"): Rewrite(pattern="code read f.go --section 1-5"),
        Input(command="FOO=1 ccx code read f.go --full | head -5"): Rewrite(
            pattern="FOO=1 ccx code read f.go --section 1-5"
        ),
        Input(command="env FOO=1 ccx code read f.go --full | head -5"): Rewrite(
            pattern="env FOO=1 ccx code read f.go --section 1-5"
        ),
        # An env value needing re-quoting (whitespace/quotes) would corrupt the spliced prefix → block, not rewrite.
        Input(command="env FOO='two words' ccx code read f.go --full | head -5"): Block(pattern="--budget"),
        Input(command="FOO='two words' ccx code read f.go --full | head -5"): Block(pattern="--budget"),
        Input(command="ccx code grep foo | head -5"): Block(pattern="--budget"),  # other subcommand → block
        Input(command="ccx code read f.go --full | tail -5"): Block(pattern="--budget"),  # tail → block
        Input(command="ccx code read f.go --full | head -c 100"): Block(pattern="--budget"),  # byte mode → block
        Input(command="ccx exec 'x' | head -3"): Block(pattern="--budget"),  # ccx exec source → block
        Input(command="rg foo | head -5"): Allow(),  # non-ccx source → not ours
        Input(command="ccx code grep foo | jq . | head -3"): Allow(),  # three stages → not ours
        Input(command="ccx code read f.go --section 1-5"): Allow(),  # no pipe → not ours
        # `ccx exec` pass-through: the outer line is one command (no top-level pipe), so it never matches.
        Input(
            command="ccx exec 'async def main(): return await sh(\"ccx code read f --full | head\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
