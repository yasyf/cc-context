"""Rewrite a bounded ``head`` on a file to ``ccx code read --section 1-N`` in place, with a
``note`` back to the model. Forms with no line-arithmetic equivalent — ``tail`` on a file,
the byte-mode (`-c`) or multi-file forms — hard-block onto the right ``ccx`` entry point
instead. When ``ccx`` cannot be resolved on disk the rewrite falls back to a hard block, so
the guard never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import re
import shlex

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


class HeadTailFile(CustomCommandLineCondition):
    """Matches ``head``/``tail`` reading a named file rather than a piped stream.

    A piped ``<cmd> | head -5`` uses head/tail as a stream *sink* to cap a pipe's output —
    the sanctioned bound for THIS hook — so a pipe or redirect is left alone here. (The pipe
    *source* stands on its own: an ``rg …`` heading such a pipe is now gated by
    ``grep_guards``/``rg_guards``, and a ``ccx …`` source piped into head/tail is gated by
    ``ccx_repipe_rewrites``' :class:`~hooks.ccx_repipe_rewrites.CcxRepipe`; this hook judges
    only the head/tail sink on a FILE operand — a pipe sink is its documented non-goal.) Only
    when head/tail's argv names a file operand is it dumping that file: ``head`` with a line count
    rewrites to a bounded ``ccx code read --section``, while ``tail`` (no start line is knowable)
    and the byte-mode (`-c`) or multi-file forms hard-block. A file token carrying a shell
    expansion (``~``/``$``) declines — ``shlex.quote`` would freeze it, so the command falls
    through to Allow and the real shell expands the path.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # A piped head/tail bounds a stream — leave it; only a named file operand fires.
        if cl.q.uses_redirect():
            return False
        parsed = headtail_parse(cl)
        return parsed is not None and bool(parsed[3]) and not any(carries_expansion(f) for f in parsed[3])


def int_or_none(s: str) -> int | None:
    return int(s) if s.isdigit() else None


def headtail_parse(cl: CommandLine) -> tuple[str, str, int | None, list[str]] | None:
    exe = cl.primary.executable
    if exe not in ("head", "tail"):
        return None
    mode = "line"
    count: int | None = None
    files: list[str] = []
    args = cl.primary.args
    i = 0
    while i < len(args):
        a = args[i]
        if a in ("-c", "--bytes"):
            mode = "byte"
            i += 2
        elif a.startswith(("--bytes=", "-c")):
            mode = "byte"
            i += 1
        elif a in ("-n", "--lines"):
            count = int_or_none(args[i + 1].lstrip("+")) if i + 1 < len(args) else None
            i += 2
        elif a.startswith("--lines="):
            count = int_or_none(a.split("=", 1)[1].lstrip("+"))
            i += 1
        elif a.startswith("-n"):
            count = int_or_none(a[2:].lstrip("=+"))
            i += 1
        elif re.fullmatch(r"-\d+", a):
            count = int(a[1:])
            i += 1
        elif a.startswith("-"):
            i += 1
        else:
            files.append(a)
            i += 1
    return exe, mode, count, files


def headtail_to(evt: BaseHookEvent) -> str | None:
    exe, mode, count, files = headtail_parse(evt.command_line)
    if exe == "head" and mode == "line" and len(files) == 1 and (ccx := ccx_bin()):
        n = count if count is not None else 10
        return f"{ccx} code read {shlex.quote(files[0])} --section 1-{n}"
    return None


def headtail_note(evt: BaseHookEvent) -> str:
    _exe, _mode, count, files = headtail_parse(evt.command_line)
    n = count if count is not None else 10
    return f"Rewrote `head {files[0]}` → `ccx code read --section 1-{n}`: same lines, token-bounded."


rewrite_command(
    only_if=[HeadTailFile()],
    to=headtail_to,
    block=(
        "BLOCKED: `head`/`tail` on a file dumps raw lines into context. "
        "Use `ccx code read <file> --section A-B` for a bounded range, or `ccx code outline <file>` "
        "to map it — the outline's line numbers show where the file ends, so you can bound a tail "
        "(or the mcp__cc-context__ccx_code_read/ccx_code_outline tools). "
        "Escape hatch — bounding a pipe's output: keep `<cmd> | head`/`| tail`."
    ),
    note=headtail_note,
    tests={
        Input(command="head -40 f.go"): Rewrite(pattern="code read f.go --section 1-40"),
        Input(command="head -n 40 f.go"): Rewrite(pattern="--section 1-40"),
        Input(command="head f.go"): Rewrite(pattern="--section 1-10"),  # head defaults to 10 lines
        Input(command="tail -n 20 f.go"): Block(pattern="ccx code outline"),
        Input(command="tail -20 f.go"): Block(pattern="ccx code read"),
        Input(command="head -c 100 f.go"): Block(pattern="ccx code outline"),  # byte mode: no line math
        Input(command="head a.go b.go"): Block(pattern="ccx code read"),  # multi-file
        Input(command="rg foo | head -5"): Allow(),  # head-as-sink is fine for THIS hook; the rg source is gated by rg_guards
        Input(command="cat f | tail -20"): Allow(),  # pipe sink — sanctioned
        Input(command="head -5"): Allow(),  # reads stdin, no file operand
        # A `~`/`$` file token declines to rewrite — shlex.quote would freeze it; the real shell expands it.
        Input(command="head -40 $d/host.go"): Allow(),
        Input(command="head -20 ~/.claude/cache/log.txt"): Allow(),
        # `ccx exec` pass-through is deliberate: head inside sh() is the script's business.
        Input(
            command="ccx exec 'async def main(): return await sh(\"head -40 f.go\")\nasyncio.run(main())'"
        ): Allow(),
    },
)
