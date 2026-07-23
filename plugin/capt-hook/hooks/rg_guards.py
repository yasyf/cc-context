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
    Rewritten,
    rewrite_command_occurrences,
)

from .common import LITERAL_SAFE, carries_expansion, ccx_supports
from .search_common import (
    CONTEXT_SHORT,
    GrepCall,
    NON_SOURCE_EXTS,
    bare_paren,
    block_reason,
    build_ccx_grep,
    conditional_cd_precedes,
    downstream_allowed,
    grep_glob,
    has_command_substitution,
    is_transcript_path,
    note_text,
    path_blocked,
    relativize_operand,
    resolve_operand,
    resolved_is_dir,
    search_block,
    tree_size_capped,
    unquote,
)

if TYPE_CHECKING:
    from captain_hook import HookResult, WalkContext
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
        # Cosmetic/match-mode booleans (long spellings of RG_BOOLEAN_SHORT). The flood family
        # (`--hidden`, `--no-ignore*`, `--unrestricted`) stays absent — it must keep gating to None.
        "ignore-case",
        "word-regexp",
        "line-number",
        "no-line-number",
        "case-sensitive",
        "smart-case",
        "with-filename",
        "no-filename",
        "invert-match",
        "line-regexp",
        "fixed-strings",
        "text",
        "quiet",
        "null",
        "heading",
        "no-heading",
        "line-buffered",
    }
)

RG_OUTPUT_BOUNDED_LONG = frozenset({"count", "count-matches", "files-with-matches", "files-without-match"})

# Long flags rg's rewrite parser reads that take no value: rg errors on `--files-with-matches=oops`,
# so an attached value declines the rewrite instead of silently discarding it.
RG_NO_VALUE_LONG = frozenset(
    {"ignore-case", "word-regexp", "fixed-strings", "files-with-matches", "line-number", "no-line-number"}
)

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


def bounded_file_rg(cmd: Command, *, sink: bool = False, cwd: Path | None = None) -> bool:
    """Report whether one ``rg`` statement is a bounded explicit-file search.

    The no-stat lane preserves rg's data-file escape: explicit :data:`NON_SOURCE_EXTS` operands pass
    by suffix, including paths created earlier in a compound command; one that stats as a directory
    falls out of the suffix lane to the capped walk — rg would recurse into it. The stat lane admits existing
    regular files and directory trees whose combined walk stays under the byte and entry caps; count
    and list-only flags waive the size cap only when every operand is a regular file. ``-L`` forfeits
    directory operands (symlink following breaks the walk's bound); ``-z``, ``-r``/``--replace``, and
    ``--color=always`` forfeit both lanes — decompression, replacement, and match decoration multiply
    output past any on-disk size bound.
    A glob, missing operand, or operand-less unpiped rg is unbounded. ``-o``, ``--json``, and
    ``RIPGREP_CONFIG_PATH`` forfeit both lanes because they can multiply output or inject unseen flags.
    An unknown flag qualifies only on an operand-less pipe sink; the lexer finishes its walk so a later
    path can never be hidden by that flag. Numeric ``-m``/``--max-count`` caps matching lines per operand
    but retains the stat lane's size cap because one minified matching line can still be megabytes; the
    no-stat data-ext lane retains that residual long-line exposure.
    """
    if "RIPGREP_CONFIG_PATH" in cmd.env_dict:
        return False
    positionals: list[str] = []
    pattern_from_flag = unknown_flag = max_count = invalid_max_count = False
    output_bounded = only_matching = json = False
    follow = search_zip = replace = color_always = False
    i, n = 0, len(cmd.args)
    while i < n:
        a = cmd.args[i]
        if a == "--":
            positionals.extend(cmd.args[i + 1 :])
            break
        if a == "-" or not a.startswith("-"):
            positionals.append(a)
            i += 1
            continue
        if a.startswith("--"):
            name, sep, val = a[2:].partition("=")
            output_bounded = output_bounded or name in RG_OUTPUT_BOUNDED_LONG
            only_matching = only_matching or name == "only-matching"
            json = json or name == "json"
            replace = replace or name == "replace"
            color_always = color_always or (name == "color" and unquote(val) == "always")
            if name in ("regexp", "file"):
                pattern_from_flag = True
                if not sep:
                    i += 1
            elif name in RG_OP_VALUE_LONG:
                if name == "max-count":
                    count = unquote(val) if sep else cmd.args[i + 1] if i + 1 < n else ""
                    max_count = max_count or count.isdigit()
                    invalid_max_count = invalid_max_count or not count.isdigit()
                if not sep:
                    i += 1
            elif name not in RG_BOOLEAN_LONG:
                unknown_flag = True
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in ("e", "f"):
            pattern_from_flag = True
            if len(a) == 2:
                i += 1
        elif head in RG_OP_VALUE_SHORT:
            replace = replace or head == "r"
            if head == "m":
                count = unquote(body[1:]) if len(a) > 2 else cmd.args[i + 1] if i + 1 < n else ""
                max_count = max_count or count.isdigit()
                invalid_max_count = invalid_max_count or not count.isdigit()
            if len(a) == 2:
                i += 1
        elif all(ch in RG_BOOLEAN_SHORT for ch in body):
            output_bounded = output_bounded or "c" in body or "l" in body
            only_matching = only_matching or "o" in body
            follow = follow or "L" in body
            search_zip = search_zip or "z" in body
        else:
            unknown_flag = True
        i += 1
    operands = positionals if pattern_from_flag else positionals[1:] if positionals else []
    if unknown_flag:
        return sink and not operands
    if invalid_max_count:
        return False
    if only_matching or json or replace or color_always or search_zip:
        return False  # match decoration, replacement, or decompression multiplies output past any size bound
    if not operands:
        return sink
    if max_count and any(has_magic(p) or "{" in p for p in operands):
        return False
    if all(
        not p.endswith("/")
        and Path(p).suffix.lower() in NON_SOURCE_EXTS
        and "{" not in p
        and not resolved_is_dir(p, cwd)
        for p in operands
    ):
        return True
    if any(has_magic(p) or "{" in p or carries_expansion(p) for p in operands):
        return False  # an unexpanded glob/brace/`$`/`~` operand stats as a literal, not as its expansion
    resolved = [resolve_operand(p, cwd) for p in operands]
    if any(r is None for r in resolved):
        return False
    if output_bounded:
        try:
            stats = [r.stat() for r in resolved]
        except OSError:
            return False
        if all(S_ISREG(result.st_mode) for result in stats):
            return True
    if follow and any(resolved_is_dir(p, cwd) for p in operands):
        return False
    return tree_size_capped(operands, cwd=cwd)


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
    """Fold a context count into the old-binary ``--expand`` fallback max."""
    return cand if not current else str(max(int(current), int(cand)))


def rg_parse(occ: Occurrence, *, cwd: Path | None = None) -> GrepCall | None:
    """Parse one direct, unpiped ``rg`` occurrence into its ccx-rewritable shape.

    Mirrors :func:`grep_parse` over ripgrep's grammar with its own DROP table
    (:data:`RG_DROP_SHORT`). ``-A/-B/-C``/their long forms map to the same native ccx flags when
    available, with ``--expand`` retained for old binaries; ``-g/--glob`` fills the include slot,
    gated to a slash-free basename glob (rg globs are gitignore-style — only a basename composes
    faithfully). Any other long flag, a repeated ``-e``, a value-taking short, a regex pattern, or an
    out-of-repo path blocks. An in-repo absolute path operand is relativized before classification, so
    ``rg -i foo /abs/repo/src`` rewrites like the relative spelling.
    """
    cmd = occ.command
    if occ.prev_op == "|" or cmd.executable != "rg" or cmd.env:
        return None
    args = cmd.args
    pattern: str | None = None
    e_count = 0
    include: str | None = None
    positionals: list[str] = []
    expand = ""
    context_args: list[tuple[str, str]] = []
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
            if sep and name in RG_NO_VALUE_LONG:
                return None  # rg rejects `--files-with-matches=oops` — never rewrite past its error
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
                context_args.append((f"--{name}", cand))
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
            context_args.append((f"-{head}", cand))
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
        elif all(ch in RG_DROP_SHORT or ch in ("i", "w") for ch in body):
            dropped_l = dropped_l or "l" in body
            dropped_fixed = dropped_fixed or "F" in body
            ignore_case = ignore_case or "i" in body
            word = word or "w" in body
        else:
            return None  # a bundle carrying a non-DROP, non-MAP char (value short)
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
    paths = [relativize_operand(p, cwd=cwd) for p in paths]
    for p in paths:
        if path_blocked(p, cwd=cwd):
            return None
    glob = grep_glob(paths, include, cwd=cwd)
    if glob is None:
        return None
    native_context = bool(context_args) and ccx_supports("code", "grep", flag="--after-context")
    return GrepCall(
        pattern,
        glob,
        "" if native_context else expand,
        tuple(context_args) if native_context else (),
        ignore_case,
        word,
        dropped_l,
        dropped_fixed,
        count_dropped=False,
    )


def rg_to(evt: BaseHookEvent, occ: Occurrence, *, cwd: Path | None = None) -> str | None:
    # An rg whose output feeds a pipe is never rewritten — ccx output must not be spliced into a pipe.
    if occ.next_op == "|" or rg_occurrence_expands(occ.command):
        return None
    parsed = rg_parse(occ, cwd=cwd)
    return build_ccx_grep(parsed) if parsed is not None else None


def rg_bounded(occ: Occurrence, *, cwd: Path | None) -> bool:
    """Whether one rg occurrence runs verbatim — bounded by its own operands or by a downstream sink.

    Mirrors :func:`~hooks.grep_guards.grep_bounded`: :func:`bounded_file_rg` proves its own operands
    bounded (stats resolved against the effective ``cwd``), and
    :func:`~hooks.search_common.downstream_allowed` proves its output pipeline terminates in a bounding
    sink (``head``/``tail``/``wc`` or a ``sed``/``awk`` head-equivalent), so the exact command runs
    verbatim rather than splicing ccx into the pipe. The directory and downstream lanes fail closed on
    unparseable operands and keep the policy screens — a session transcript, hidden-segment dependency
    path (``.venv/…``), or git-ignored/out-of-repo operand blocks with its steer regardless of the sink,
    since that is policy, not boundedness.
    """
    inner = occ.command.unwrapped
    operands = rg_operands(inner)
    if (
        # A RIPGREP_CONFIG_PATH assignment anywhere on the line (a prior `export` included) injects
        # flags the parser never sees; the sink lane survives it — the terminal caps output regardless.
        "RIPGREP_CONFIG_PATH" not in occ.line.raw
        and bounded_file_rg(inner, sink=occ.prev_op == "|", cwd=cwd)
        and (
            operands is None
            or not any(resolved_is_dir(p, cwd) and path_blocked(p, cwd=cwd) for p in operands)
        )
    ):
        return True
    return downstream_allowed(occ, operands, cwd=cwd)


class RgFlood(CustomCommandLineCondition):
    """Match a line carrying any unpiped ``rg`` occurrence.

    A cheap structural gate — cwd-blind, so it cannot judge boundedness or rewritability (those turn on
    the effective cwd, which only the walk sees). :func:`rg_visit` is authoritative: it returns a verdict
    per occurrence, and a line whose every rg is a bounded search yields all-``None`` (a genuine allow).
    Matching through ``cmd.unwrapped`` keeps wrapper prefixes transparent; a pure ``… | rg`` filter (no
    unpiped rg) stays outside the registration entirely.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occ.command.unwrapped.executable == "rg" and occ.prev_op != "|" for occ in cl.occurrences)


def rg_block(evt: PreToolUseEvent, cl: CommandLine, *, reason: str | None = None) -> str:
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
        "Simple literal `rg` auto-rewrites to `ccx code grep`; an rg runs as-is when its explicit targets "
        "are all data files, existing files — or directories (rg recurses by default) — whose bytes total "
        "under the size cap (count/list-only searches over regular files run regardless of file size), or "
        "when its pipeline ends in a bounding sink (`head`, in-cap `tail -n`, `wc`, `sed -n '1,20p'`, "
        "`awk 'NR<=20'`). This one didn't qualify — a regex pattern, an unmappable flag "
        "(`-t`/`-r`/`--no-ignore`/…), an ignored-dir target, an expansion (`~`/`$`/`$(…)`), a wrapper, "
        "an over-cap or unwalkable target, or an unbounded recursive target. "
        "Escape hatch: pipe input into it (`… | rg`).",
        cl=cl,
        reason=reason,
    )


def rg_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Per-occurrence verdict for the ``visit=`` walk, threading the effective ``cwd``.

    Mirrors :func:`~hooks.grep_guards.grep_visit`. Non-rg occurrences pass (``None``). For an rg, the
    effective ``cwd`` is trusted only when the walk carried one (``ctx.cwd``), the raw line has no bare
    ``(`` outside quoting (:func:`~hooks.search_common.bare_paren` — the parser flattens subshells, so
    an untrusted line would leak a ``(cd … )`` cwd into siblings; a paren inside a quoted pattern keeps
    trust), and no preceding ``cd`` is gated behind ``&&``/``||``
    (:func:`~hooks.search_common.conditional_cd_precedes` — the shell may short-circuit it); declined,
    every stat lane fails closed rather than falling back to the process cwd. A rewritable rg splices
    to ``ccx code grep`` when the occurrence is spliceable (else blocks); an unrewritable rg runs
    verbatim when it is a bounded search (:func:`rg_bounded`) and blocks otherwise. Blocking rides a
    :class:`HookResult` that aborts the walk, discarding any sibling rewrite.
    """
    if occ.command.unwrapped.executable != "rg":
        return None
    base = (
        ctx.cwd
        if ctx.cwd is not None and not bare_paren(evt.cmd.line.raw) and not conditional_cd_precedes(occ)
        else None
    )
    if (text := rg_to(evt, occ, cwd=base)) is not None:
        if ctx.spliceable:
            return Rewritten(text, note=note_text(occ.command.raw, rg_parse(occ, cwd=base)))
        return evt.block(rg_block(evt, evt.cmd.line, reason=block_reason(occ, cwd=base)))
    if rg_bounded(occ, cwd=base):
        return None
    return evt.block(rg_block(evt, evt.cmd.line, reason=block_reason(occ, cwd=base)))


rewrite_command_occurrences(
    only_if=[RgFlood()],
    visit=rg_visit,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, glob-only, context). Path→glob shapes
        # classify each operand against the filesystem, so they live in test_rg_guards.py.
        Input(command="rg foo"): Rewrite(pattern="code grep foo"),  # no path → repo-wide
        Input(command="rg -n foo"): Rewrite(pattern="code grep foo"),  # -n cosmetic
        Input(command="rg -nl foo"): Rewrite(pattern="code grep foo"),  # probe-specific -l suffix lives in pytest
        Input(command="rg -F foo"): Rewrite(pattern="code grep foo"),  # -F disclosed (ccx matches literally)
        Input(command="rg -in foo"): Rewrite(pattern="-i"),  # MAP `i` bundled with cosmetic `n` → mapped
        Input(command="rg -g '*.go' foo"): Rewrite(pattern="--glob '*.go'"),  # basename glob → include
        Input(command="rg -C 3 foo"): Rewrite(pattern="-C=3"),  # native context count preserved
        Input(command="rg -A 20 foo"): Rewrite(pattern="-A=20"),  # native context count preserved
        Input(command="rg -A 2 -B 5 TODO"): Rewrite(pattern="-A=2 -B=5"),  # native flags preserve both counts
        Input(command="rg -B 5 -A 2 TODO"): Rewrite(pattern="-B=5 -A=2"),  # native flag order preserved
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
        Input(command="rg --files-with-matches=oops foo"): Block(),  # rg rejects a value on a no-value long
        Input(command="rg --files"): Block(pattern="ccx repo find"),  # file listing → repo find
        Input(command="rg foo /etc/hosts"): Allow(),  # deliberate flip: bounded regular-file stat lane
        # The verbatim incident command: a hidden `.venv` segment is a policy steer, so the downstream
        # `head -40` never launders it — the dependency-source block stands, no stat.
        Input(
            command='rg -n "class ToolUse" .venv/lib/python3.13/site-packages/cc_transcript/ -A 20 | head -40'
        ): Block(pattern="ccx repo locate"),
        # The amendment is narrow: a non-hidden operand under the same bounded terminal runs verbatim.
        Input(command="rg -n foo src/ -A 20 | head -40"): Allow(),
        # Unparseable operands fail the downstream lane closed: `--hidden` makes rg_operands None, so
        # the sink cannot launder the `.venv/` target past the dependency-source steer.
        Input(command="rg --hidden foo .venv/ | head"): Block(pattern="ccx repo locate"),
        # Long cosmetic/match-mode booleans parse like their short forms, so the bounded sink allows;
        # the flood family (`--hidden`/`--no-ignore*`/`--unrestricted`) stays out of the set and gated.
        Input(command="rg --fixed-strings foo internal/ | head"): Allow(),
        Input(command="rg --line-number foo internal/ | head"): Allow(),
        Input(command="rg --smart-case foo . | head -20"): Allow(),
        Input(command="rg --no-ignore foo . | head"): Block(pattern="ccx repo locate"),  # sink never launders it
        # Downstream-bounded lane: a recognized terminal caps context, so the direct rg runs
        # verbatim (flip of a former Block row); a non-bounding terminal (`cat`) still blocks.
        Input(command="rg foo file.py | wc -l"): Allow(),
        Input(command="rg foo | sed -n '1,20p'"): Allow(),
        Input(command="rg foo | sed '1,20p'"): Block(),
        Input(command="rg -l foo | cat"): Block(),  # cat is not a bounding sink → the flood still blocks
        Input(command="rg -c foo"): Block(),  # operand-less count is still tree-wide
        Input(command="rg -c foo data.json && rg bar ."): Rewrite(
            pattern="rg -c foo data.json && "
        ),  # bounded sibling stays byte-identical while the tree rg splices
        Input(command="rg -c foo src/ && rg bar ."): Block(),  # flooding sibling still vetoes the line
        Input(command="RIPGREP_CONFIG_PATH=rg.conf rg foo ."): Block(),  # rewriting must not delete the env prefix
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
        # Mixed transcript + flood: the unrewritable sibling fires; the block carries the cc-transcript line too.
        Input(command="rg foo ~/.claude/projects/main.jsonl; rg -v bar ."): Block(pattern="cc-transcript"),
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
