"""Rg guard: rewrite simple literal ``rg`` file search to ``ccx code grep``, block the rest."""

from __future__ import annotations

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
    Tool,
    rewrite_command,
)

from .common import LITERAL_SAFE, is_single_command
from .search_common import (
    CONTEXT_SHORT,
    GrepCall,
    NON_SOURCE_EXTS,
    UnpipedSearch,
    build_ccx_grep,
    grep_glob,
    note_text,
    path_blocked,
    unquote,
)

if TYPE_CHECKING:
    from cc_transcript.command import Command


# ripgrep's own DROP table — never grep's: rg's short flags are false friends (`-r` takes a
# value, `-E` is encoding, `-I` is no-filename). `-n`/`-N`/`-s`/`-H`/`-I` are cosmetic; `-l`
# (files-with-matches) and `-F` (fixed-strings) disclose their drop through the note.
RG_DROP_SHORT = frozenset("nNsHIlF")

# rg flags whose next token is a value, for the tolerant `rg_operands` walk (separate from the
# strict rewrite parser). `-e`/`-f`/`--regexp`/`--file` supply the pattern and are handled apart.
RG_OP_VALUE_SHORT = frozenset("gtTABCmrEjMd")

# rg's boolean short flags — they take no value, so `rg_operands` may skip one (or an all-boolean
# bundle) without consuming the next token. A short outside this set ∪ RG_OP_VALUE_SHORT is unknown
# and gates the command (`-d 1` is max-depth: its `1` must not leak in as a phantom pattern).
RG_BOOLEAN_SHORT = frozenset("iwnNsSHIlLcovxFupahqz0")

RG_OP_VALUE_LONG = frozenset(
    {
        "glob",
        "iglob",
        "type",
        "type-not",
        "after-context",
        "before-context",
        "context",
        "max-count",
        "replace",
        "encoding",
        "threads",
        "max-columns",
        "max-depth",
        "max-filesize",
        "sort",
        "sortr",
        "color",
        "colors",
        "type-add",
        "ignore-file",
    }
)


def rg_operands(cmd: Command) -> list[str] | None:
    """Extract an ``rg``'s explicit path operands (the pattern excluded), or ``None`` to stay gated.

    A tolerant walk used only by the non-source exemption — it need not fully parse rg, only
    separate path operands from the pattern and flags. It skips boolean shorts and consumes the
    values of known value-taking flags; ``-e``/``-f``/``--regexp``/``--file`` mark that the pattern
    came from a flag (so no positional is the pattern). Any unrecognized long or short flag returns
    ``None``, which leaves the command gated — the safe direction, never a wrong Allow.
    """
    args = cmd.args
    positionals: list[str] = []
    pattern_from_flag = False
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a == "-" or not a.startswith("-"):
            positionals.append(a)
            i += 1
            continue
        if a.startswith("--"):
            name, sep, _ = a[2:].partition("=")
            if name in ("regexp", "file"):
                pattern_from_flag = True
                if not sep:
                    i += 1
            elif name in RG_OP_VALUE_LONG:
                if not sep:
                    i += 1
            else:
                return None
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in ("e", "f"):
            pattern_from_flag = True
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif head in RG_OP_VALUE_SHORT:
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif not all(ch in RG_BOOLEAN_SHORT for ch in body):
            return None  # unknown short or bundle with a non-boolean char → stay gated
        i += 1
    if not pattern_from_flag and positionals:
        return positionals[1:]
    return positionals


class RgNonSourceTargets(CustomCommandLineCondition):
    """Skips the rg gate when every unpiped ``rg`` searches only non-source data files.

    The sanctioned escape hatch: a raw ``rg`` whose explicit path operands are all data files
    (:data:`NON_SOURCE_EXTS`, by a textual suffix check — no stat) runs as-is. A directory or
    source-file operand, a cwd search (no operand), or an unparseable flag shape leaves the
    command gated.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        matched = False
        for i, (cmd, _) in enumerate(cl.parts):
            if cmd.executable != "rg" or (i > 0 and cl.parts[i - 1][1] == "|"):
                continue
            matched = True
            operands = rg_operands(cmd)
            if not operands or any(
                op.endswith("/") or Path(op).suffix.lower() not in NON_SOURCE_EXTS for op in operands
            ):
                return False
        return matched


def fold_expand(current: str, cand: str) -> str:
    """Fold a context count into the running ``--expand`` max — several ``-A/-B/-C`` widen to their superset."""
    return cand if not current else str(max(int(current), int(cand)))


def rg_parse(cl: CommandLine) -> GrepCall | None:
    """Parse an unpiped ``rg`` into its ccx-rewritable shape, or ``None`` to fall back to block.

    Mirrors :func:`grep_parse` over ripgrep's grammar with its own DROP table
    (:data:`RG_DROP_SHORT`). ``-A/-B/-C``/``--context`` map to ``--expand=<count>`` with the count
    preserved (grep drops it); ``-g/--glob`` fills the include slot, gated to a slash-free basename
    glob (rg globs are gitignore-style — only a basename composes faithfully). Any other long flag,
    a repeated ``-e``, a value-taking short, a regex pattern, or an out-of-repo path blocks.
    """
    if not is_single_command(cl):
        return None
    args = cl.primary.args
    pattern: str | None = None
    e_count = 0
    include: str | None = None
    positionals: list[str] = []
    expand = ""
    ignore_case = word = dropped_l = dropped_fixed = False
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a == "-" or not a.startswith("-"):
            positionals.append(a)
            i += 1
            continue
        if a.startswith("--"):
            name, sep, val = a[2:].partition("=")
            if name == "ignore-case":
                ignore_case = True
            elif name == "word-regexp":
                word = True
            elif name in ("after-context", "before-context", "context"):
                if sep:
                    if not unquote(val).isdigit():
                        return None
                    cand = unquote(val)
                elif i + 1 < n and args[i + 1].isdigit():
                    cand = args[i + 1]
                    i += 1
                else:
                    return None
                expand = fold_expand(expand, cand)
            elif name == "glob":
                if include is not None:
                    return None
                if sep:
                    include = unquote(val)
                elif i + 1 < n:
                    include = args[i + 1]
                    i += 1
                else:
                    return None
                if "/" in include:
                    return None
            elif name == "regexp":
                e_count += 1
                if sep:
                    pattern = unquote(val)
                elif i + 1 < n:
                    pattern = args[i + 1]
                    i += 1
                else:
                    return None
            elif name == "fixed-strings":
                dropped_fixed = True
            elif name == "files-with-matches":
                dropped_l = True
            elif name in ("line-number", "no-line-number"):
                pass
            elif name in ("color", "colour"):
                # rg's --color requires a value; consume the space-form token so it can't leak
                # into the positionals as a phantom pattern (the =glued form is already whole).
                if not sep and i + 1 < n:
                    i += 1
            else:
                return None
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in CONTEXT_SHORT:
            if len(body) > 1:
                if not unquote(body[1:]).isdigit():
                    return None
                cand = unquote(body[1:])
            elif i + 1 < n and args[i + 1].isdigit():
                cand = args[i + 1]
                i += 1
            else:
                return None
            expand = fold_expand(expand, cand)
        elif head == "e":
            e_count += 1
            if len(body) > 1:
                pattern = unquote(body[1:])
            elif i + 1 < n:
                pattern = args[i + 1]
                i += 1
            else:
                return None
        elif head == "g":
            if include is not None:
                return None
            if len(body) > 1:
                include = unquote(body[1:])
            elif i + 1 < n:
                include = args[i + 1]
                i += 1
            else:
                return None
            if "/" in include:
                return None
        elif len(body) == 1:
            if head == "i":
                ignore_case = True
            elif head == "w":
                word = True
            elif head in RG_DROP_SHORT:
                dropped_l = dropped_l or head == "l"
                dropped_fixed = dropped_fixed or head == "F"
            else:
                return None  # -o -v -c -u -E -r -t -m -j -M …
        elif all(ch in RG_DROP_SHORT for ch in body):
            dropped_l = dropped_l or "l" in body
            dropped_fixed = dropped_fixed or "F" in body
        else:
            return None  # a bundle carrying a non-DROP char (value short or MAP flag)
        i += 1
    if e_count > 1:
        return None
    if pattern is None:
        if not positionals:
            return None
        pattern, paths = positionals[0], positionals[1:]
    else:
        paths = positionals
    # rg's default engine reads `+` as a quantifier, so a literal ccx rewrite would under-match;
    # grep's BRE `+` is already literal, so this rejection is rg-only (LITERAL_SAFE admits `+`).
    if pattern.startswith("-") or "+" in pattern or not LITERAL_SAFE.match(pattern):
        return None
    for p in paths:
        if path_blocked(p):
            return None
    glob = grep_glob(paths, include)
    if glob is None:
        return None
    return GrepCall(pattern, glob, expand, ignore_case, word, dropped_l, dropped_fixed, count_dropped=False)


def rg_to(evt: BaseHookEvent) -> str | None:
    parsed = rg_parse(evt.command_line)
    return build_ccx_grep(parsed) if parsed is not None else None


def rg_note(evt: BaseHookEvent) -> str:
    return note_text(evt.command, rg_parse(evt.command_line))


rewrite_command(
    only_if=[Tool("Bash"), UnpipedSearch("rg")],
    skip_if=[RgNonSourceTargets()],
    to=rg_to,
    block=(
        "BLOCKED: raw `rg` file search floods context. "
        'Use `ccx code grep <text>` (or mcp__cc-context__ccx_code_grep) for literal text, `ccx code search "<question>"` '
        'for intent, `ccx repo find "<glob>"` to list files. '
        "Dependency source (`.venv`, vendored pkgs): `ccx repo locate <pkg>`, then "
        "`ccx code grep`/`outline`/`read` with the printed path. "
        "Simple literal `rg` auto-rewrites to `ccx code grep`; this one didn't — a regex pattern, an unmappable "
        "flag (`-t`/`-r`/`--no-ignore`/…), an ignored-dir target, or a pipe/chain. "
        "Escape hatches: data files (`.log`/`.json`/`.yaml`/…) as explicit targets run as-is; piped input (`… | rg`) runs as-is."
    ),
    note=rg_note,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, glob-only, context). Path→glob shapes
        # classify each operand against the filesystem, so they live in test_rg_guards.py.
        Input(command="rg foo"): Rewrite(pattern="code grep foo"),  # no path → repo-wide
        Input(command="rg -n foo"): Rewrite(pattern="code grep foo"),  # -n cosmetic
        Input(command="rg -nl foo"): Rewrite(pattern="code grep foo"),  # -l disclosed, dropped
        Input(command="rg -F foo"): Rewrite(pattern="code grep foo"),  # -F disclosed (ccx matches literally)
        Input(command="rg -g '*.go' foo"): Rewrite(pattern="--glob '*.go'"),  # basename glob → include
        Input(command="rg -C 3 foo"): Rewrite(pattern="--expand=3"),  # context count preserved
        Input(command="rg -A 20 foo"): Rewrite(pattern="--expand=20"),  # count carried through, not dropped
        Input(command="rg -A 2 -B 5 TODO"): Rewrite(pattern="--expand=5"),  # several context flags → max, not last-wins
        Input(command="rg -B 5 -A 2 TODO"): Rewrite(pattern="--expand=5"),  # order-independent superset
        Input(command="rg --color always plugin"): Rewrite(pattern="code grep plugin"),  # --color eats its space-form value
        Input(command="rg --color=always plugin"): Rewrite(pattern="code grep plugin"),  # =glued --color still no-ops
        # Block — unmappable shapes fall back to the message:
        Input(command="rg 'foo.*' ."): Block(),  # regex-metachar pattern (LITERAL_SAFE)
        Input(command="rg fo+ file.py"): Block(),  # `+` is an rg quantifier — no faithful literal rewrite
        Input(command="rg -t py foo"): Block(),  # -t takes a value (false friend of grep)
        Input(command="rg --no-ignore foo"): Block(pattern="ccx repo locate"),  # ignore bypass → dependency steer
        Input(command="rg -uu foo"): Block(pattern="ccx repo locate"),  # unrestricted → dependency steer
        Input(command="rg -r repl foo"): Block(),  # -r takes a value — misparse guard
        Input(command="rg -e a -e b ."): Block(),  # multiple -e
        Input(command="rg -m 5 foo ."): Block(),  # -m takes a value
        Input(command="rg -d 1 app.log"): Block(),  # -d (max-depth) takes a value — its 1 must not leak an exemption
        Input(command="rg --files"): Block(pattern="ccx repo find"),  # file listing → repo find
        Input(command="rg foo /etc/hosts"): Block(),  # absolute path
        # The verbatim incident command: hidden `.venv` segment + a pipe → deterministic block, no stat.
        Input(
            command='rg -n "class ToolUse" .venv/lib/python3.13/site-packages/cc_transcript/ -A 20 | head -40'
        ): Block(pattern="ccx repo locate"),
        Input(command="rg foo file.py | wc -l"): Block(),  # pipe-source parity with grep
        # Allow — piped sink (rg consumes stdin), non-source data-file targets, ccx exec pass-through:
        Input(command="cat f | rg foo"): Allow(),
        Input(command="journalctl | rg err | head -5"): Allow(),
        Input(command="rg foo app.log"): Allow(),  # data-file target runs as-is
        Input(command="rg -o 'err.*timeout' server.log"): Allow(),  # regex is fine on a data file
        Input(command="rg -o 'err.*timeout' server.LOG"): Allow(),  # suffix match is case-insensitive (.LOG → .log)
        Input(command="rg foo data.json config.yaml"): Allow(),  # all operands non-source
        Input(command="rg foo logs/app.log | head -5"): Allow(),  # data-file head of a pipe
        # `ccx exec` pass-through is deliberate: sh("rg …") is in-sandbox and budget-capped on return.
        Input(
            command="ccx exec 'async def main(): return await sh(\"rg -n foo src/\")\nasyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("rg -n foo src/")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)
