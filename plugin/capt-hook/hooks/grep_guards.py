"""Grep guard: rewrite a tree-shaped ``grep`` file search to ``ccx code grep``, block the unmappable rest; everything else runs raw."""

from __future__ import annotations

import re
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

from .common import LITERAL_SAFE, ccx_supports
from .search_common import (
    CONTEXT_SHORT,
    DEP_STEER,
    GrepCall,
    any_git_ignored,
    build_ccx_grep,
    forfeits_operand,
    grep_glob,
    has_command_substitution,
    has_dependency_segment,
    is_transcript_path,
    note_text,
    path_operands_raw,
    resolved_is_dir,
    search_block,
    unquote,
)

if TYPE_CHECKING:
    from pathlib import Path

    from captain_hook import HookResult, WalkContext
    from cc_transcript.command import Command, Occurrence


# grep flags ccx code grep subsumes as no-ops on its literal, always-recursive, line-numbered
# engine. `-l` and `-F` change output/semantics, so the note discloses their drop; the rest are silent.
DROP_SHORT = frozenset("rRnHhsIFl")
DROP_LONG = frozenset(
    {
        "recursive",
        "dereference-recursive",
        "line-number",
        "with-filename",
        "no-filename",
        "no-messages",
        "files-with-matches",
    }
)

# Long flags that take no value: native grep errors on `--recursive=oops`, so an attached value
# declines the rewrite instead of silently discarding it.
NO_VALUE_LONG = DROP_LONG | frozenset(
    {"ignore-case", "word-regexp", "extended-regexp", "basic-regexp", "fixed-strings"}
)

# Regex metacharacters per grep dialect. A pattern carrying NONE of the active dialect's
# metachars is a plain literal in that dialect (→ literal rewrite when ccx-literal-safe); one
# carrying any is handed to `translate_pattern`, which admits it onto `--regex` only when its
# meaning is identical in grep and the Rust-regex engine. BRE reads `+ ? | ( ) { }` as literal
# (so `a+` under the default is a literal), ERE as metachars.
BRE_METACHARS = frozenset(".*^$[\\")
ERE_METACHARS = BRE_METACHARS | frozenset("+?|(){}")

# Chars the validator treats as a plain literal atom — identical in grep BRE/ERE and Rust regex.
# `.` (the any-char wildcard, also an atom) is admitted here too. Brackets, backslash, quotes,
# backticks, and a non-terminal `$` are excluded: shell-active chars stay out as defense in depth
# atop the downstream shlex-quoting, and bracket/backslash constructs diverge across dialects.
REGEX_ATOM = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_ .:@,=/-")

# Chars whose backslash-escape is a literal of that char in grep BRE/ERE AND Rust regex (`\.` a literal
# dot, `\\` a literal backslash), so `translate_pattern` passes them through verbatim in both dialects.
# Every other backslash escape (`\d`, `\w`, `\b`, `\<`, `\1`) diverges or has no Rust form → refused.
REGEX_ESCAPED_LITERAL = frozenset(".*[]^$\\")

# Known-arity grep flag tables for the tolerant `grep_operands` lexer: to separate a flag's value
# token from a path operand. An UNKNOWN flag makes `grep_operands` return None (no rewrite, the raw
# dir-operand scan then feeds shape detection). `-e`/`-f` (and `--regexp`/`--file`) supply the pattern.
BOUNDED_BOOL_SHORT = frozenset("iwxcqLlrRnHhsIFEGPzaUbTo")
BOUNDED_VALUE_SHORT = frozenset("mABCdD")
BOUNDED_PATTERN_SHORT = frozenset("ef")
BOUNDED_BOOL_LONG = frozenset(
    {
        "ignore-case", "no-ignore-case", "word-regexp", "line-regexp", "count",
        "files-with-matches", "files-without-match", "quiet", "silent",
        "no-filename", "with-filename", "line-number", "recursive", "dereference-recursive",
        "extended-regexp", "fixed-strings", "basic-regexp", "perl-regexp", "null", "null-data",
        "text", "byte-offset", "no-messages", "initial-tab", "color", "colour", "binary", "only-matching",
        "line-buffered",
    }
)
BOUNDED_VALUE_LONG = frozenset(
    {
        "max-count", "after-context", "before-context", "context", "directories", "devices",
        "include", "exclude", "include-dir", "exclude-dir", "exclude-from", "binary-files",
        "label", "group-separator", "context-separator",
    }
)
BOUNDED_PATTERN_LONG = frozenset({"regexp", "file"})


def grep_targets(paths: list[str], include: str | None, *, cwd: Path | None) -> tuple[str, list[str]] | None:
    """Split grep's path args into a ``(glob, path_operands)`` pair, or ``None`` to block.

    Two or more explicit non-directory operands (no ``--include``, no ``.``-widening) carry as
    ``ccx code grep`` positionals — the multi-file form (ccx ≥ v0.11.0) — with an empty glob.
    Everything else routes through :func:`grep_glob`: a directory, a lone file (``--glob file`` for
    old-binary compat), an ``--include``, or repo-wide widening yield a glob and no path operands.
    """
    if include is None and not any(p in (".", "./") for p in paths) and len(paths) >= 2:
        if all(not resolved_is_dir(p, cwd) for p in paths):
            return "", [p.rstrip("/") for p in paths]
    glob = grep_glob(paths, include, cwd=cwd)
    return None if glob is None else (glob, [])


def valid_brace(body: str) -> bool:
    """Report whether an interval body (``{m}``/``{m,n}``/``{m,}``) is digits-and-comma with every
    bound within GNU grep's ``RE_DUP_MAX`` (32767) ceiling — above it GNU errors while Rust compiles,
    so an oversized interval must not rewrite. An over-length body short-circuits before Python's
    int-conversion limit can raise.
    """
    if len(body) > 11 or re.fullmatch(r"\d+(,\d*)?", body) is None:
        return False
    return all(int(part) <= 32767 for part in body.split(",") if part)


def translate_pattern(pattern: str, ere: bool) -> str | None:
    """Translate ``pattern`` to the ``ccx code grep --regex`` (Rust-regex) dialect, or ``None`` when unrewritable.

    A position-aware dialect translation — not a character whitelist, which can't distinguish a literal
    mid-pattern ``^`` (grep) from an anchor (Rust). Admits only constructs whose meaning is identical in
    grep (BRE when ``ere`` is false, else ERE) and Rust regex, emitting each in its Rust spelling; the
    returned string equals the input for an already-ERE pattern. Accepted: plain atoms
    (:data:`REGEX_ATOM`, ``.`` the wildcard); ``*`` (and ``+``/``?`` under ERE) never leading or stacked;
    ``^`` only first and ``$`` only last; ``|`` alternation, balanced ``()`` groups, and digits-only
    ``{m,n}`` intervals (ERE bare, BRE backslashed ``\\|`` ``\\(`` ``\\)`` ``\\{m,n\\}``); the escaped
    literals :data:`REGEX_ESCAPED_LITERAL` verbatim; and, under BRE, bare ``+ ? ( ) { } |`` — literals in
    BRE — emitted backslash-escaped so Rust reads them as literals too. Brackets, backreferences, and any
    other backslash escape are not rewritable.
    """
    n = len(pattern)
    out: list[str] = []
    depth = 0
    quantifiable = False  # a preceding atom a quantifier may bind
    quantifier = False  # the preceding token was itself a quantifier (no stacking)
    i = 0
    while i < n:
        c = pattern[i]
        if c == "\\":
            nxt = pattern[i + 1] if i + 1 < n else ""
            if nxt in REGEX_ESCAPED_LITERAL:
                out.append("\\" + nxt)
                quantifiable, quantifier = True, False
                i += 2
            elif not ere and nxt == "|":
                if not quantifiable:
                    return None
                out.append("|")
                quantifiable = quantifier = False
                i += 2
            elif not ere and nxt == "(":
                depth += 1
                out.append("(")
                quantifiable = quantifier = False
                i += 2
            elif not ere and nxt == ")":
                depth -= 1
                if depth < 0:
                    return None
                out.append(")")
                quantifiable, quantifier = True, False
                i += 2
            elif not ere and nxt in "+?":
                if not quantifiable or quantifier:
                    return None
                out.append(nxt)
                quantifier = True
                i += 2
            elif not ere and nxt == "{":
                if not quantifiable or quantifier:
                    return None  # an interval is a quantifier — GNU BRE rejects a leading or stacked one
                close = pattern.find("\\}", i + 2)
                if close == -1 or not valid_brace(pattern[i + 2 : close]):
                    return None
                out.append("{" + pattern[i + 2 : close] + "}")
                quantifiable = quantifier = True
                i = close + 2
            else:
                return None  # backref \1-\9, \b, \w, \<, \>, a trailing \, … — refused
            continue
        if c in REGEX_ATOM:
            out.append(c)
            quantifiable, quantifier = True, False
        elif c == "*" or (ere and c in "+?"):
            if not quantifiable or quantifier:
                return None
            out.append(c)
            quantifier = True
        elif c == "^":
            if i != 0:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif c == "$":
            if i != n - 1:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == "|":
            if not quantifiable:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == "(":
            depth += 1
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == ")":
            depth -= 1
            if depth < 0:
                return None
            out.append(c)
            quantifiable, quantifier = True, False
        elif ere and c == "{":
            if not quantifiable or quantifier:
                return None  # an interval is a quantifier — a leading or stacked one diverges from Rust
            close = pattern.find("}", i)
            if close == -1 or not valid_brace(pattern[i + 1 : close]):
                return None
            out.append(pattern[i : close + 1])
            quantifiable = quantifier = True
            i = close
        elif not ere and c in "+?(){}|":
            out.append("\\" + c)
            quantifiable, quantifier = True, False
        else:
            return None
        i += 1
    return "".join(out) if depth == 0 else None


def grep_parse(occ: Occurrence, *, cwd: Path | None = None) -> GrepCall | None:
    """Parse one direct, unpiped ``grep`` occurrence into its ccx-rewritable shape.

    Rewrites only a direct invocation whose flags all fall in the DROP/MAP sets and whose one pattern
    is a plain literal (:data:`LITERAL_SAFE`) or a dialect-faithful regex (:func:`translate_pattern`).
    A pipe-sink occurrence, a wrapper-prefixed grep, and an env-prefixed grep decline here.
    Exit-code / output-mode shapes (``-c -q -o -v -L -x``), PCRE (``-P``), multi-pattern searches
    (repeated ``-e``), a value-taking short glued into a bundle (``-rnC3``), and out-of-repo path
    operands (absolute / ``~`` / ``..``, via :func:`grep_glob`) all decline the rewrite.
    """
    cmd = occ.command
    if occ.prev_op == "|" or cmd.executable != "grep" or cmd.env:
        return None
    args = cmd.args
    pattern: str | None = None
    e_count = 0
    include: str | None = None
    positionals: list[str] = []
    context_args: list[tuple[str, str]] = []
    ignore_case = word = dropped_l = dropped_fixed = ere = bre = False
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
            if sep and name in NO_VALUE_LONG:
                return None  # native grep rejects `--recursive=oops` — never rewrite past its error
            if name == "ignore-case":
                ignore_case = True
            elif name == "word-regexp":
                word = True
            elif name == "extended-regexp":
                ere = True
            elif name == "basic-regexp":
                bre = True
            elif name == "fixed-strings":
                dropped_fixed = True
            elif name == "perl-regexp":
                return None  # PCRE never maps
            elif name in ("after-context", "before-context", "context"):
                if sep:
                    if not unquote(val).isdigit():
                        return None
                    count = unquote(val)
                else:
                    if i + 1 >= n or not args[i + 1].isdigit():
                        return None
                    count = args[i + 1]
                    i += 1
                context_args.append((f"--{name}", count))
            elif name == "include":
                if include is not None:
                    return None
                if sep:
                    include = unquote(val)
                elif i + 1 < n:
                    include = args[i + 1]
                    i += 1
                else:
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
            elif name in ("color", "colour"):
                pass
            elif name in DROP_LONG:
                dropped_l = dropped_l or name == "files-with-matches"
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
                count = unquote(body[1:])
            elif i + 1 < n and args[i + 1].isdigit():
                count = args[i + 1]
                i += 1
            else:
                return None
            context_args.append((f"-{head}", count))
        elif head == "e":
            e_count += 1
            if len(body) > 1:
                pattern = unquote(body[1:])
            elif i + 1 < n:
                pattern = args[i + 1]
                i += 1
            else:
                return None
        elif head in ("m", "f"):  # -m N (max-count), -f FILE (pattern file) → block
            return None
        elif len(body) == 1:
            if head == "i":
                ignore_case = True
            elif head == "w":
                word = True
            elif head == "E":
                ere = True
            elif head == "G":
                bre = True
            elif head == "P":
                return None  # PCRE never maps
            elif head in DROP_SHORT:
                dropped_l = dropped_l or head == "l"
                dropped_fixed = dropped_fixed or head == "F"
            else:
                return None  # -v -x -c -o -L -q -z, …
        elif all(ch in DROP_SHORT or ch in ("E", "G", "i", "w") for ch in body):
            dropped_l = dropped_l or "l" in body
            dropped_fixed = dropped_fixed or "F" in body
            ere = ere or "E" in body
            bre = bre or "G" in body
            ignore_case = ignore_case or "i" in body
            word = word or "w" in body
        else:
            return None  # a bundle carrying a non-DROP, non-MAP char (value short or -P)
        i += 1
    if e_count > 1:
        return None
    if pattern is None:
        if not positionals:
            return None
        pattern, paths = positionals[0], positionals[1:]
    else:
        paths = positionals
    if not pattern or pattern.startswith("-"):
        return None
    if dropped_fixed and (ere or bre):
        return None  # grep errors on -F with -E/-G (conflicting matchers)
    regex = False
    if dropped_fixed:
        # -F forces literal; a pattern ccx's literal engine can't take faithfully isn't rewritable.
        if not LITERAL_SAFE.match(pattern):
            return None
    elif any(c in (ERE_METACHARS if ere else BRE_METACHARS) for c in pattern):
        translated = translate_pattern(pattern, ere)
        if translated is None:
            return None  # dialect-divergent or exotic regex (backrefs, escapes, PCRE)
        pattern = translated  # BRE spellings (`\|`, `\(…\)`) rewritten to the Rust-regex dialect
        regex = True
    elif not LITERAL_SAFE.match(pattern):
        return None
    targets = grep_targets(paths, include, cwd=cwd)
    if targets is None:
        return None
    glob, path_ops = targets
    native_context = bool(context_args) and ccx_supports("code", "grep", flag="--after-context")
    return GrepCall(
        pattern,
        glob,
        "" if not context_args or native_context else "3",
        tuple(context_args) if native_context else (),
        ignore_case,
        word,
        dropped_l,
        dropped_fixed,
        count_dropped=bool(context_args) and not native_context,
        regex=regex,
        paths=tuple(path_ops),
    )


def grep_operands(cmd: Command) -> list[str] | None:
    """Extract a ``grep``'s explicit path operands (the pattern excluded), or ``None`` if unparseable.

    A tolerant walk over grep's flag arities (:data:`BOUNDED_BOOL_SHORT` and friends). It separates path
    operands from the pattern and from flag values, feeding the policy steers (transcript / dependency
    source) and tree-shape detection. An unknown flag returns ``None`` — the steers skip and shape
    detection falls back to a raw dir scan, never a wrong block.
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
            if name in BOUNDED_PATTERN_LONG:
                pattern_from_flag = True
                if not sep:
                    i += 1
            elif name in BOUNDED_VALUE_LONG:
                if not sep:
                    i += 1
            elif name not in BOUNDED_BOOL_LONG:
                return None
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in BOUNDED_PATTERN_SHORT:
            pattern_from_flag = True
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif head in BOUNDED_VALUE_SHORT:
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif "m" in body[1:]:
            prefix, _, count = body.partition("m")
            if body.count("m") != 1 or not all(ch in BOUNDED_BOOL_SHORT for ch in prefix):
                return None
            if not count and i + 1 < n:
                i += 1
        elif not all(ch in BOUNDED_BOOL_SHORT for ch in body):
            return None
        i += 1
    if not pattern_from_flag and positionals:
        return positionals[1:]
    return positionals


def grep_recursive(args: tuple[str, ...]) -> bool:
    """Whether a grep's flags request a recursive walk (``-r``/``-R``/``--recursive``/
    ``--dereference-recursive``). Scanned before the first bare ``--`` and tolerant of unknown flags —
    it feeds tree-shape detection, never a rewrite."""
    for a in args[: args.index("--")] if "--" in args else args:
        if a.startswith("--"):
            if a[2:].partition("=")[0] in ("recursive", "dereference-recursive"):
                return True
        elif a.startswith("-") and a != "-" and ("r" in a[1:] or "R" in a[1:]):
            return True
    return False


def grep_tree_shaped(cmd: Command, *, cwd: Path | None) -> bool:
    """Whether a grep is a directory-wide flood — the one positive shape the block fires on.

    A recursive grep with no path operand (it recurses the cwd), or any grep whose operand is
    ``.``/``..`` or stats as a directory (the emitter would rewrite it to a recursive ``ccx code grep``,
    and an unmappable one steers to ccx). An explicit file, an unstattable ``$VAR``/missing operand, or a
    non-recursive operand-less grep is not tree-shaped — it runs raw. When :func:`grep_operands` cannot
    map an unknown flag it returns ``None``: the shape then rests on a literal ``.``/``..`` token among
    the raw path-like operands paired with a recursive flag (``grep -r --weird foo .`` stays tree-shaped),
    never a filesystem stat of the raw tokens — an operand that happens to name a real dir under an
    unparseable flag must not block a bounded explicit-file search.
    """
    ops = grep_operands(cmd)
    if ops is None:
        return grep_recursive(cmd.args) and any(p.rstrip("/") in (".", "..") for p in path_operands_raw(cmd.args))
    return (grep_recursive(cmd.args) and not ops) or any(resolved_is_dir(p, cwd) for p in ops)


def grep_to(occ: Occurrence, *, cwd: Path | None = None) -> str | None:
    """The ``ccx code grep`` rewrite for a grep occurrence, or ``None`` when the emitter cannot map it."""
    parsed = grep_parse(occ, cwd=cwd)
    return build_ccx_grep(parsed) if parsed is not None else None


class GrepFlood(CustomCommandLineCondition):
    """Match a line carrying any unpiped ``grep`` occurrence.

    A cheap structural gate; :func:`grep_visit` is authoritative, returning a per-occurrence verdict.
    Matching through ``cmd.unwrapped`` keeps wrapper prefixes transparent; a pure ``… | grep`` filter
    (no unpiped grep) stays outside the registration entirely.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occ.command.unwrapped.executable == "grep" and occ.prev_op != "|" for occ in cl.occurrences)


def grep_block(evt: PreToolUseEvent, cl: CommandLine) -> str:
    return search_block(
        evt,
        "grep",
        grep_operands,
        "BLOCKED: raw `grep` for a recursive/tree-wide file search floods context. "
        "Use `ccx code grep <text>` / `ccx code search` for code; the built-in Grep tool or `rg` for "
        "literal content in non-source files. Several terms? One call covers them: "
        "`ccx code grep 'a|b|c' --regex`. Simple tree greps auto-rewrite to `ccx code grep`; this one "
        "didn't map — a PCRE/exotic-regex pattern, an unmappable flag, or a mixed/out-of-repo target. "
        "Escape hatch: pipe input into it (`… | grep`), or name explicit files.",
        cl=cl,
    )


def grep_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Per-occurrence verdict under the fail-open doctrine.

    Non-grep occurrences pass. The policy steers scan the parsed path operands (pattern excluded),
    falling back to the raw path-like tokens (:func:`path_operands_raw`) when the flag walk can't parse —
    an unparseable flag can't blind them, though the fallback over-matches (the pattern token rides
    along): a transcript operand blocks with the cc-transcript steer; a dependency segment (``.venv/…``,
    ``node_modules/…``, ``.git/…``) or a directory operand the repo's own ``git check-ignore`` reports
    ignored (``dist/``, a generated tree) blocks with the dep-reader steer — all fire even through
    pipes, while an ignored plain file stays bounded and runs raw. A grep consuming or feeding a pipe
    runs verbatim (post-processing). A grep that is not tree-shaped runs raw (explicit-file searches are bounded
    by their operands). A tree-shaped grep whose raw text carries a ``$(…)``/backtick substitution runs raw
    (the parser drops the operand, so a rewrite would silently widen scope). A path operand carrying a
    shell expansion or glob metachar forfeits the rewrite and runs raw (never a lossy emission). Otherwise
    an unmappable shape (``-P``, an exotic regex, an unknown flag over ``.``) blocks with the flood steer,
    while a mappable shape the local ``ccx`` binary is too old (or absent) to emit runs raw — infra
    unavailability never blocks. A block rides a :class:`HookResult` that aborts the walk, discarding any
    sibling rewrite.
    """
    inner = occ.command.unwrapped
    if inner.executable != "grep":
        return None
    ops = grep_operands(inner)
    steer_ops = path_operands_raw(inner.args) if ops is None else ops
    if any(is_transcript_path(p) for p in steer_ops):
        return evt.block(grep_block(evt, evt.cmd.line))
    if any(has_dependency_segment(p) for p in steer_ops) or any_git_ignored(steer_ops, cwd=ctx.cwd):
        return evt.block(DEP_STEER)
    if occ.prev_op == "|" or occ.next_op == "|":
        return None
    if not grep_tree_shaped(inner, cwd=ctx.cwd):
        return None
    if has_command_substitution(occ.command.raw):
        return None
    if ops and any(forfeits_operand(p) for p in ops):
        return None
    parsed = grep_parse(occ, cwd=ctx.cwd)
    if parsed is None:
        return evt.block(grep_block(evt, evt.cmd.line))
    text = build_ccx_grep(parsed)
    if text is None:
        return None
    if ctx.spliceable:
        return Rewritten(text, note=note_text(occ.command.raw, parsed))
    return evt.block(grep_block(evt, evt.cmd.line))


rewrite_command_occurrences(
    only_if=[GrepFlood()],
    visit=grep_visit,
    tests={
        # Rewrite — tree-shaped via a `.` operand or `-r` with no path (disk-independent). Path→glob
        # shapes classify each operand against the filesystem, so they live in test_grep_guards.py
        # (TestGrepPathGlobbing) where a tmp tree and pinned cwd make the classification deterministic.
        Input(command="grep -rn foo"): Rewrite(pattern="code grep foo"),  # recursive, no path → repo-wide
        Input(command="grep --recursive foo ."): Rewrite(pattern="code grep foo"),  # long recursive no-op
        Input(command="grep -rn --include='*.go' foo ."): Rewrite(pattern="--glob '*.go'"),  # `.` + include → repo-wide glob
        Input(command="grep -A 7 foo ."): Rewrite(pattern="-A=7"),  # native context count preserved
        Input(command="grep -rn foo . src/"): Rewrite(pattern="code grep foo"),  # `.` sibling widens to whole repo, no --glob
        Input(command="echo x; grep -r foo ."): Rewrite(pattern="echo x; "),  # splice only grep; sibling stays byte-identical
        Input(command="grep -ri foo"): Rewrite(pattern="code grep foo"),  # -ri bundle maps -i (probe); recursive repo-wide
        Input(command="grep -riw foo"): Rewrite(pattern="-i -w"),  # -riw bundle maps both MAP shorts; recursive
        Input(command="grep foo ."): Rewrite(pattern="code grep foo"),  # `.` operand → tree-shaped → repo-wide rewrite
        # Regex rewrites — a validator-cleared pattern maps onto `ccx code grep --regex` (disk-independent
        # `.` widening; the --regex probe hits the real plugin/bin/ccx, which supports it since v0.11.0):
        Input(command="grep 'foo.*' ."): Rewrite(pattern="--regex"),  # BRE-safe metachars → regex on rg engine
        Input(command="grep -E 'a|b' ."): Rewrite(pattern="--regex"),  # ERE alternation → regex on rg engine
        Input(command="grep -E 'a+' ."): Rewrite(pattern="--regex"),  # ERE `+` is a quantifier → validator → regex
        Input(command="grep 'a+' ."): Rewrite(pattern="code grep a+"),  # BRE `+` is literal → literal rewrite, no --regex
        Input(command="grep 'a\\|b' ."): Rewrite(pattern="--regex"),  # BRE `\|` → ERE `|` alternation, rewritten to regex
        Input(command="grep 'x\\(ab\\)\\+' ."): Rewrite(pattern="--regex"),  # BRE group + `\+` → `(ab)+` on the rg engine
        Input(command="grep 'foo$' ."): Rewrite(pattern="--regex"),  # `$` in the PATTERN is an anchor, not a path
        # Block — tree-shaped (via `.`) but the emitter can't map the flag/pattern:
        Input(command="grep -rnC3 foo ."): Block(),  # value short glued into a bundle
        Input(command="grep --recursive=oops foo ."): Block(),  # native grep rejects a value on a no-value long
        Input(command="grep -P 'x(?=y)' ."): Block(),  # PCRE (-P) never maps; `.` is a dir → tree-shaped
        Input(command="grep 'a^b' ."): Block(),  # BRE mid-pattern `^`: literal in grep, an anchor in Rust → not rewritable
        Input(command="grep -F 'foo.*' ."): Block(),  # -F forces literal; `foo.*` isn't ccx-literal-safe → no --regex flip
        Input(command="grep -E -F foo ."): Block(),  # -F with -E: conflicting matchers, grep errors → block
        Input(command="grep -q foo ."): Block(),  # exit-code contract over a dir
        Input(command="grep -c foo ."): Block(),  # count mode over a dir
        Input(command="grep -o foo ."): Block(),  # -o over a dir → tree-wide
        Input(command="grep -e foo -e bar ."): Block(),  # multiple -e over a dir
        Input(command="grep -f patterns.txt ."): Block(),  # -f pattern-file over a dir
        Input(command="GREP_OPTIONS=-v grep foo ."): Block(),  # env-prefixed grep over `.` can't rewrite (env unseen)
        # Allow — explicit-file searches run raw (not tree-shaped): a file operand, a `~`/`$`/missing
        # path, or an absolute file is bounded by its operand, so the guard never fires. Flip of prior
        # Block rows — the whole point of the fail-open doctrine:
        Input(command="grep foo /var/log/x.log"): Allow(),  # absolute data file → runs raw
        Input(command="grep foo ~/notes.md"): Allow(),  # `~` file operand → not a dir → runs raw
        Input(command="grep foo ghost.py"): Allow(),  # missing operand → not a dir → runs raw
        Input(command="grep -n foo $d/host.go"): Allow(),  # `$VAR` operand → unstattable → runs raw
        Input(command="grep foo ~/app.log"): Allow(),  # `~` data file → runs raw
        Input(command="grep -q pat missing.html"): Allow(),  # missing file → not tree-shaped
        Input(command="grep -l foo a.html b.html"): Allow(),  # two file operands → not tree-shaped
        Input(command="grep -o localhost /etc/hosts"): Allow(),  # -o over an absolute file → not tree-shaped
        Input(command="grep -oHnb . AGENTS.md"): Allow(),  # prefixes over a single file → not tree-shaped
        Input(command="grep -c needle *"): Allow(),  # unexpanded glob operand → not a dir → runs raw
        Input(command="grep -c needle file{1..10000}"): Allow(),  # unexpanded brace operand → runs raw
        Input(command="grep -r foo logs.json"): Allow(),  # -r on a missing operand → not a dir → runs raw
        # The incident regression class: a bounded-output grep over a file the same compound creates,
        # any downstream pipe present — every one runs raw now:
        Input(
            command="curl -sL -o /tmp/ch-live.html https://yasyf.github.io/captain-hook/ && "
            "wc -c < /tmp/ch-live.html && for m in 'id=\"links\"' gd-hero; do "
            "printf '%s: %s\\n' \"$m\" \"$(grep -c \"$m\" /tmp/ch-live.html)\"; done"
        ): Allow(),  # -c over a file the curl creates later in the same compound
        Input(command="grep -i err app.log | head"): Allow(),  # a downstream pipe → post-processing
        Input(command="grep foo ghost.py | wc -l"): Allow(),  # any pipe → post-processing, runs raw
        Input(command="grep -oi points b_jetblue_jun.json"): Allow(),  # -o on a single data file → not tree-shaped
        Input(command="echo x > gen.json; grep -i points gen.json"): Allow(),  # created earlier in the compound
        Input(command="cd sub && grep foo notes.json"): Allow(),  # cd-relative file operand → not tree-shaped
        # Downstream pipe → allow (the pipe is the post-processing; the sink-quality walk is gone):
        Input(command="grep -r foo src/ | head"): Allow(),
        Input(command="grep -r x . | head -n 100000"): Allow(),  # over-cap sink no longer matters — any pipe allows
        Input(command="grep -r x . | head -c 100000000"): Allow(),  # a byte-count sink is irrelevant now
        Input(command="grep -r x . | tee /tmp/out | head"): Allow(),  # tee is irrelevant — any pipe allows
        Input(command="grep -rn foo | sed '1,20p'"): Allow(),  # unbounded sed no longer matters
        Input(command="grep -r foo src/ | grep -v x"): Allow(),  # both greps piped → runs raw
        Input(command="grep -rn foo . | sort"): Allow(),  # non-sink terminal no longer matters
        Input(command="grep -r foo . | tail -f"): Allow(),  # following tail no longer matters
        # Transcript policy steer — fires even through a downstream pipe (checked before the pipe):
        Input(command="grep -r foo ~/.claude/projects/"): Block(pattern="cc-transcript"),
        Input(command="grep -r foo ~/.claude/projects/ | head"): Block(pattern="cc-transcript"),
        # Dep policy steer — a VCS-store segment blocks textually; ignored-dir shapes rest on
        # `git check-ignore` and live in pytest (TestDependencyDirTargets), never inline.
        Input(command="grep -r foo .git/ | head"): Block(pattern="ccx repo locate"),
        # Mixed transcript + flood: the block carries BOTH the default steer and the cc-transcript line.
        Input(command="grep foo ~/.claude/projects/main.jsonl; grep -v bar ."): Block(pattern="cc-transcript"),
        # Per-occurrence splices: a non-tree-shaped sibling stays byte-identical while the tree grep splices.
        Input(command="grep -i points data.json && grep foo ."): Rewrite(pattern="grep -i points data.json && "),
        Input(command="grep -c foo data.json && grep -rn bar ."): Rewrite(pattern="grep -c foo data.json && "),
        Input(command="grep foo $d/host.go; grep bar ."): Rewrite(pattern="grep foo $d/host.go; "),  # $VAR sibling runs raw
        # Per-occurrence blocks: one flooding grep aborts the whole line.
        Input(command="grep -c foo . && grep -rn bar ."): Block(),  # `-c` over `.` floods → vetoes the line
        Input(command="echo x; grep -c foo ."): Block(),  # one flooding grep blocks the compound line
        # Substitution forfeits the rewrite (parser drops the operand); the rest runs raw under fail-open.
        Input(command="grep -r foo $(dir)"): Allow(),  # tree via -r, but `$(…)` forfeits the rewrite → runs raw
        Input(command="grep foo $(printf /tmp/target)"): Allow(),  # substitution operand → not tree-shaped → runs raw
        Input(command="grep -n foo `printf x`"): Allow(),  # backtick operand → runs raw
        Input(command="grep -r . . '$(printf x)'"): Allow(),  # `$(` in raw forfeits the rewrite → runs raw (doctrine 5)
        # Fix 5: a path operand carrying an expansion/glob metachar forfeits the rewrite → runs raw
        # (never a block, never a lossy/mis-globbed emission). `~` and `[` are the two triggers below.
        Input(command="grep -r . . ~/notes.md"): Allow(),  # `~` operand → forfeit → runs raw
        Input(command="grep -r foo 'src[old]/' ."): Allow(),  # `[old]` would mis-glob as a class → forfeit → runs raw
        # Fix 3: an unknown grep flag makes `grep_operands` return None; tree-shape then needs a recursive
        # flag AND a literal `.`/`..` token — never a stat of the raw tokens.
        Input(command="grep -r --weird foo ."): Block(),  # `-r` + literal `.` → tree-shaped; `--weird` unmappable → block
        # Fix 2: `.claude/projects` must be consecutive path segments — a lookalike substring is not a transcript.
        Input(command="grep needle docs/x.claude/projects-notes.md"): Allow(),  # not a transcript, not a dir → runs raw
        # Wrapper transparency — the 2026-07-17 decision: a wrapped tree grep stays gated (direct-only rewrite).
        Input(command="sudo grep foo ."): Block(),
        Input(command="timeout 10 grep foo ."): Block(),
        # Existing Allow neighbors — piped grep, non-grep, ccx exec pass-through:
        Input(command="ls | grep foo"): Allow(),
        Input(command="cat x | grep foo | sort"): Allow(),
        Input(command="git log --grep=fix"): Allow(),
        Input(command='git log --grep "fix bug"'): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"grep -rn foo src/\")\nasyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("grep -rn foo src/")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)
