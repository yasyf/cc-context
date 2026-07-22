"""Rg guard: rewrite simple literal ``rg`` file search to ``ccx code grep``, block the rest."""

from __future__ import annotations

from glob import has_magic
from pathlib import Path
from stat import S_ISREG
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    PreToolUseEvent,
    Rewrite,
    rewrite_command_occurrences,
)

from .common import LARGE_READ_BYTES, LITERAL_SAFE, carries_expansion
from .search_common import (
    CONTEXT_SHORT,
    GrepCall,
    NON_SOURCE_EXTS,
    build_ccx_grep,
    grep_glob,
    has_command_substitution,
    is_transcript_path,
    note_text,
    path_blocked,
    search_block,
    unquote,
)

if TYPE_CHECKING:
    from cc_transcript.command import Command, Occurrence


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

RG_BOOLEAN_LONG = frozenset(
    {
        "count",
        "count-matches",
        "files-with-matches",
        "files-without-match",
        "json",
        "only-matching",
    }
)

RG_OUTPUT_BOUNDED_LONG = frozenset({"count", "count-matches", "files-with-matches", "files-without-match"})

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
            elif name not in RG_BOOLEAN_LONG:
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


def bounded_file_rg(cmd: Command, *, sink: bool = False) -> bool:
    """Report whether one ``rg`` statement is a bounded explicit-file search.

    The no-stat lane preserves rg's data-file escape: explicit :data:`NON_SOURCE_EXTS` operands pass
    by suffix, including paths created earlier in a compound command. The stat lane admits only
    existing regular-file operands whose total size is no more than :data:`LARGE_READ_BYTES`; count
    and list-only flags waive the size cap, but never the regular-file requirement. Because rg recurses
    by default, a directory, glob, missing operand, or operand-less unpiped rg is unbounded. ``-o``,
    ``--json``, and ``RIPGREP_CONFIG_PATH`` forfeit both lanes because they can multiply output or inject
    unseen flags.
    """
    if "RIPGREP_CONFIG_PATH" in cmd.env_dict:
        return False
    if (operands := rg_operands(cmd)) is None:
        return sink
    output_bounded = only_matching = json = False
    i, n = 0, len(cmd.args)
    while i < n:
        a = cmd.args[i]
        if a == "--":
            break
        if a == "-" or not a.startswith("-"):
            i += 1
            continue
        if a.startswith("--"):
            name, sep, _ = a[2:].partition("=")
            output_bounded = output_bounded or name in RG_OUTPUT_BOUNDED_LONG
            only_matching = only_matching or name == "only-matching"
            json = json or name == "json"
            if name in {"regexp", "file"} | RG_OP_VALUE_LONG and not sep:
                i += 1
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in ("e", "f") or head in RG_OP_VALUE_SHORT:
            if len(a) == 2:
                i += 1
        else:
            output_bounded = output_bounded or "c" in body or "l" in body
            only_matching = only_matching or "o" in body
        i += 1
    if only_matching or json:
        return False
    if not operands:
        return sink
    if all(not p.endswith("/") and Path(p).suffix.lower() in NON_SOURCE_EXTS for p in operands):
        return True
    if any(has_magic(p) for p in operands):
        return False
    try:
        stats = [Path(p).stat() for p in operands]
    except OSError:
        return False
    if not all(S_ISREG(result.st_mode) for result in stats):
        return False
    return output_bounded or sum(result.st_size for result in stats) <= LARGE_READ_BYTES


def rg_occurrence_expands(cmd: Command) -> bool:
    """Whether one rg occurrence carries a shell expansion that forfeits its rewrite.

    Mirrors :func:`~hooks.grep_guards.grep_occurrence_expands`: a ``$(...)``/backtick substitution (the
    parser drops the operand, so a rewrite would silently widen the search) or a non-transcript path
    operand carrying ``~``/``$`` (:func:`~hooks.search_common.build_ccx_grep` ``shlex.quote``s each path,
    and single quotes freeze ``~``/``$``, so ccx would get a literal ``~/foo`` that does not exist). The
    rewrite is forfeited, never the block — a raw rg is recursive by default, so an unverifiable operand
    is a flood the shell must not run. The pattern is excluded (a ``$`` end-anchor stays rewritable):
    only operands :func:`rg_operands` resolves as paths count. A transcript operand is never singled out
    here — it blocks and is steered at cc-transcript by :func:`~hooks.search_common.search_block`.
    """
    return has_command_substitution(cmd.raw) or (
        (ops := rg_operands(cmd)) is not None
        and any(carries_expansion(p) and not is_transcript_path(p) for p in ops)
    )


def fold_expand(current: str, cand: str) -> str:
    """Fold a context count into the running ``--expand`` max — several ``-A/-B/-C`` widen to their superset."""
    return cand if not current else str(max(int(current), int(cand)))


def rg_parse(occ: Occurrence) -> GrepCall | None:
    """Parse one direct, unpiped ``rg`` occurrence into its ccx-rewritable shape.

    Mirrors :func:`grep_parse` over ripgrep's grammar with its own DROP table
    (:data:`RG_DROP_SHORT`). ``-A/-B/-C``/``--context`` map to ``--expand=<count>`` with the count
    preserved (grep drops it); ``-g/--glob`` fills the include slot, gated to a slash-free basename
    glob (rg globs are gitignore-style — only a basename composes faithfully). Any other long flag,
    a repeated ``-e``, a value-taking short, a regex pattern, or an out-of-repo path blocks.
    """
    cmd = occ.command
    if occ.prev_op == "|" or cmd.executable != "rg":
        return None
    args = cmd.args
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


def rg_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if rg_occurrence_expands(occ.command):
        return None
    parsed = rg_parse(occ)
    return build_ccx_grep(parsed) if parsed is not None else None


def rg_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    return "\n".join(
        note_text(occ.command.raw, parsed)
        for occ, _ in pairs
        if (parsed := rg_parse(occ)) is not None
    )


class RgFlood(CustomCommandLineCondition):
    """Match a line carrying an actionable unpiped ``rg`` occurrence.

    Direct rewritable rgs activate the splice lane; an unrewritable rg activates the block lane unless
    it is a bounded explicit-file/data-file search. Matching through ``cmd.unwrapped`` makes wrapper
    prefixes transparent to the condition, while :func:`rg_to` stays direct-only so a qualifying
    wrapped rg blocks instead of dropping its wrapper.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        rgs = [occ for occ in cl.occurrences if occ.command.unwrapped.executable == "rg"]
        return any(occ.prev_op != "|" for occ in rgs) and any(
            (occ.prev_op != "|" and rg_to(evt, occ) is not None)
            or not bounded_file_rg(occ.command.unwrapped, sink=occ.prev_op == "|")
            for occ in rgs
        )


def rg_block_if(evt: BaseHookEvent, occ: Occurrence) -> bool:
    cmd = occ.command
    if (inner := cmd.unwrapped).executable != "rg" or cmd.span is None:
        return inner.executable == "rg"
    return rg_to(evt, occ) is None


def rg_block(evt: PreToolUseEvent, cl: CommandLine) -> str:
    return search_block(
        evt,
        "rg",
        rg_operands,
        "BLOCKED: raw `rg` file search floods context. "
        "Use `ccx code grep <text>` for literal text, "
        '`ccx code search "<question>"` for intent, `ccx repo find "<glob>"` to list files. '
        "Several terms? One call covers them: `ccx code grep 'a|b|c' --regex`. "
        "Dependency source (`.venv`, vendored pkgs): spawn the `cc-context:dep-reader` agent "
        "with the package and your question — it returns cited conclusions, never the source. "
        "Inline: `ccx repo locate <pkg>` (CLI-only), then `ccx code grep`/`outline`/`read` with the printed path. "
        "Simple literal `rg` auto-rewrites to `ccx code grep`; this one didn't — a regex pattern, an "
        "unmappable flag (`-t`/`-r`/`--no-ignore`/…), an ignored-dir target, an expansion "
        "(`~`/`$`/`$(…)`), a wrapper, or an unbounded recursive target. Explicit data files and existing "
        "regular files under the size cap run as-is; count/list-only searches run regardless of file size. "
        "Escape hatch: pipe input into it (`… | rg`).",
        cl=cl,
    )


rewrite_command_occurrences(
    only_if=[RgFlood()],
    to=rg_to,
    block_if=rg_block_if,
    block=rg_block,
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
        Input(command="printf 'left  side'; rg foo"): Rewrite(pattern="printf 'left  side'; "),
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
        Input(command="rg foo /etc/hosts"): Allow(),  # deliberate flip: bounded regular-file stat lane
        # The verbatim incident command: hidden `.venv` segment + a pipe → deterministic block, no stat.
        Input(
            command='rg -n "class ToolUse" .venv/lib/python3.13/site-packages/cc_transcript/ -A 20 | head -40'
        ): Block(pattern="ccx repo locate"),
        Input(command="rg foo file.py | wc -l"): Block(),  # pipe-source parity with grep
        Input(command="rg -c foo ."): Block(),  # count is bounded only over explicit regular files
        Input(command="sudo rg foo ."): Block(),  # wrapper-transparent condition; direct-only rewrite
        # Allow — piped sink (rg consumes stdin), non-source data-file targets, ccx exec pass-through:
        Input(command="cat f | rg foo"): Allow(),
        Input(command="journalctl | rg err | head -5"): Allow(),
        Input(command="rg foo app.log"): Allow(),  # data-file target runs as-is
        Input(command="rg -c foo app.log"): Allow(),  # count-only data-file rg escapes without stat
        # A `~`/`$` operand forfeits the rewrite but NOT the block — rg is recursive by default, so an
        # unverifiable operand is a flood. A data-ext operand stays in the no-stat lane by suffix.
        Input(command="rg foo ~/notes.md"): Block(pattern="floods context"),
        Input(command="rg -n foo $d/host.go"): Block(pattern="floods context"),
        Input(command="rg foo $d/app.log"): Allow(),  # data-ext expansion operand stays exempt (suffix, no stat)
        Input(command="rg foo ~/.claude/projects/"): Block(pattern="cc-transcript"),  # transcript → cc-transcript steer
        # Mixed transcript + flood: the sibling `rg bar .` fires the line; the block carries the cc-transcript line too.
        Input(command="rg foo ~/.claude/projects/main.jsonl; rg bar ."): Block(pattern="cc-transcript"),
        # Substitution drops the `$(…)`/backtick operand → rewrite forfeited; the operand-less rg floods → block.
        Input(command="rg foo $(printf /tmp/target)"): Block(pattern="floods context"),
        Input(command="rg -n foo `printf x`"): Block(pattern="floods context"),
        # Per-occurrence: the `$(…)` rg forfeits its rewrite, the sibling `rg bar .` tree search floods → the line blocks.
        Input(command="rg foo $(printf /tmp/t); rg bar ."): Block(pattern="floods context"),
        Input(command="rg -o 'err.*timeout' server.log"): Block(),  # deliberate flip: -o forfeits both bounded lanes
        Input(command="rg -o 'err.*timeout' server.LOG"): Block(),  # deliberate flip: suffix case cannot override -o
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
