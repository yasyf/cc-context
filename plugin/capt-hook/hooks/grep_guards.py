"""Grep guard: rewrite simple literal ``grep`` file search to ``ccx code grep``, block the rest."""

from __future__ import annotations

import re
from pathlib import Path
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

from .common import LARGE_READ_BYTES, LITERAL_SAFE, carries_expansion, ccx_supports
from .search_common import (
    CONTEXT_SHORT,
    GrepCall,
    NON_SOURCE_EXTS,
    build_ccx_grep,
    classify_path,
    conditional_cd_precedes,
    downstream_allowed,
    grep_glob,
    has_command_substitution,
    is_transcript_path,
    note_text,
    path_blocked,
    resolve_operand,
    resolved_is_dir,
    resolved_is_file,
    search_block,
    unquote,
)

if TYPE_CHECKING:
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

# Known-arity grep flag tables for `bounded_file_grep`'s tolerant lexer: to tell a bounded
# explicit-files grep from a tree-wide one it separates a flag's value token from a path operand.
# An UNKNOWN flag leaves the grep unbounded (it then enters the hook), never a wrong allow. `-e`/`-f`
# (and `--regexp`/`--file`) supply the pattern, so no positional is the pattern.
# `-o`/`--only-matching` prints each disjoint non-empty match substring plus a newline, so output stays
# bounded by ~2× the size-capped input and it counts as bounded; `-v`/`--invert-match` stays excluded —
# invert-match prints the non-matching lines, a cat-shaped dump idiom.
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

    Two or more explicit *existing regular files* (no ``--include``, no ``.``-widening) carry as
    ``ccx code grep`` positionals — the multi-file form (ccx ≥ v0.11.0) — with an empty glob.
    Everything else routes through :func:`grep_glob`: a directory, a lone file (``--glob file`` for
    old-binary compat), an ``--include``, or repo-wide widening yield a glob and no path operands.
    Path operands are classified against the filesystem resolved from ``cwd``.
    """
    if include is None and not any(p in (".", "./") for p in paths) and len(paths) >= 2:
        if all(classify_path(p, cwd=cwd) is False for p in paths):
            return "", [p.rstrip("/") for p in paths]
    glob = grep_glob(paths, include, cwd=cwd)
    return None if glob is None else (glob, [])


def valid_brace(body: str) -> bool:
    """Report whether an interval body (``{m}``/``{m,n}``/``{m,}``) is digits-and-comma with every
    bound within GNU grep's ``RE_DUP_MAX`` (32767) ceiling — above it GNU errors while Rust compiles,
    so an oversized interval must not rewrite.
    """
    if re.fullmatch(r"\d+(,\d*)?", body) is None:
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
    is a plain literal (:data:`LITERAL_SAFE`). A pipe-sink occurrence and a wrapper-prefixed grep
    decline here. Dialect-divergent/exotic regexes, exit-code / output-mode shapes
    (``-c -q -o -v -L -x``), PCRE (``-P``), multi-pattern searches (repeated ``-e``), or paths
    reaching outside the repo (absolute / ``~`` / ``..``) block. A value-taking short glued into a
    bundle (``-rnC3``) blocks too — bundles are DROP-only.
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
        elif all(ch in DROP_SHORT or ch in ("E", "G") for ch in body):
            dropped_l = dropped_l or "l" in body
            dropped_fixed = dropped_fixed or "F" in body
            ere = ere or "E" in body
            bre = bre or "G" in body
        else:
            return None  # a bundle carrying a non-DROP char (value short, MAP flag, or -P)
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
    for p in paths:
        if path_blocked(p, cwd=cwd):
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


def grep_to(evt: BaseHookEvent, occ: Occurrence, *, cwd: Path | None = None) -> str | None:
    # A grep whose output feeds a pipe is never rewritten — ccx output must not be spliced into a pipe.
    if occ.next_op == "|" or grep_occurrence_expands(occ.command):
        return None
    parsed = grep_parse(occ, cwd=cwd)
    return build_ccx_grep(parsed) if parsed is not None else None


def bounded_file_grep(cmd: Command, *, sink: bool = False, cwd: Path | None = None) -> bool:
    """Report whether one ``grep`` statement is a bounded search ccx can't rewrite.

    Judges a single grep occurrence on its own flags and operands — not the whole Bash line — so it
    holds per-occurrence inside a pipe or ``&&``/``;`` chain. ``sink`` marks a grep on the receiving
    end of a pipe (``… | grep``): with no path operand it just filters stdin, so an unparseable or
    operand-less sink grep is presumed a bounded filter, whereas the same shape unpiped is not.

    Three shapes qualify as a bounded file search:

    - *data-ext textual*: every operand is an explicit :data:`NON_SOURCE_EXTS` file matched by suffix
      with no stat, so a file created earlier in the same compound command or addressed relative to an
      in-command ``cd`` passes; ``-o`` is fine here (rg parity), but a recursion flag forfeits it (a
      directory named like a data file under ``-r`` would flood).
    - *count/quiet/list-only* (``-c``/``-q``/``-l``/``-L``/their long forms): one line per operand by
      construction — a missing operand yields a single grep error line — so any size and any existence
      passes (a file the same compound creates earlier qualifies); an operand that is an existing
      directory, ends with ``/``, or carries a glob/brace metacharacter (one operand can expand to
      thousands) forfeits, as do recursion and ``-o``.
    - *bounded regular files*: every operand stats as an existing regular file whose sizes sum to no
      more than :data:`~hooks.common.LARGE_READ_BYTES`; ``-o`` forfeits this stat lane, since its
      per-match filename/line/byte prefixes multiply output past the size bound. Numeric
      ``-m``/``--max-count`` caps matching lines per operand but stays in this size-capped lane: one
      minified matching line can still be megabytes. The no-stat data-ext lane retains that residual
      long-line exposure.

    Conservative throughout: a ``GREP_OPTIONS`` env (which can inject ``-r``/``-o`` the parser never
    sees), any env alongside path operands, an uninspectable ``-f`` pattern file, a flag-supplied empty
    pattern, and an unknown flag on an unpiped grep all return ``False`` — never a wrong allow. An
    unknown flag on a pipe sink qualifies only after the full walk proves it has no path operand.
    """
    if "GREP_OPTIONS" in cmd.env_dict:
        return False  # a leading GREP_OPTIONS= injects flags (-r/-o …) that never reach cmd.args
    args = cmd.args
    positionals: list[str] = []
    pattern_from_flag = only_matching = pattern_file = empty_flag_pattern = recursive = output_bounded = False
    max_count = invalid_max_count = False
    unknown_flag = False
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
            if name in BOUNDED_PATTERN_LONG:
                pattern_from_flag = True
                if name == "file":
                    pattern_file = True
                elif sep:
                    empty_flag_pattern = empty_flag_pattern or val == ""
                elif i + 1 < n:
                    empty_flag_pattern = empty_flag_pattern or args[i + 1] == ""
                if not sep:
                    i += 1
            elif name in BOUNDED_VALUE_LONG:
                recursive = recursive or name == "directories"  # any --directories value → assume recursive
                if name == "max-count":
                    count = unquote(val) if sep else args[i + 1] if i + 1 < n else ""
                    max_count = max_count or count.isdigit()
                    invalid_max_count = invalid_max_count or not count.isdigit()
                if not sep:
                    i += 1
            elif name in BOUNDED_BOOL_LONG:
                recursive = recursive or name in ("recursive", "dereference-recursive")
                only_matching = only_matching or name == "only-matching"
                output_bounded = output_bounded or name in (
                    "count", "quiet", "silent", "files-with-matches", "files-without-match"
                )
            else:
                unknown_flag = True
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in BOUNDED_PATTERN_SHORT:
            pattern_from_flag = True
            if head == "f":
                pattern_file = True
            if len(a) == 2 and i + 1 < n:
                if head == "e":
                    empty_flag_pattern = empty_flag_pattern or args[i + 1] == ""
                i += 1
        elif head in BOUNDED_VALUE_SHORT:
            recursive = recursive or head == "d"  # -d [recurse] → assume recursive
            if head == "m":
                count = unquote(body[1:]) if len(a) > 2 else args[i + 1] if i + 1 < n else ""
                max_count = max_count or count.isdigit()
                invalid_max_count = invalid_max_count or not count.isdigit()
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif all(ch in BOUNDED_BOOL_SHORT for ch in body):
            recursive = recursive or "r" in body or "R" in body
            only_matching = only_matching or "o" in body
            output_bounded = output_bounded or any(ch in "cqlL" for ch in body)
        else:
            unknown_flag = True
        i += 1
    if pattern_from_flag:
        empty_positional_pattern = False
        paths = positionals
    elif not positionals:
        return False  # no pattern and no operand
    else:
        empty_positional_pattern = positionals[0] == ""
        paths = positionals[1:]
    if unknown_flag:
        return sink and not paths
    if invalid_max_count:
        return False
    if recursive and not paths:
        return False  # grep -r with no operand recurses the cwd, even as a pipe sink
    if not paths:
        return sink  # a pure stdin filter passes; an unpiped stdin-grep fails as today
    if cmd.env or pattern_file or empty_flag_pattern:
        return False  # env with paths, an uninspectable -f pattern file, or a flag-supplied empty pattern
    if empty_positional_pattern:
        return False  # an empty positional pattern floods every line
    if max_count and any(
        p.endswith("/") or any(ch in p for ch in "*?[{") or resolved_is_dir(p, cwd) for p in paths
    ):
        return False
    # Data files pass by suffix, no stat; recursion forfeits it — a dir named like a data file floods.
    if not recursive and all(not p.endswith("/") and Path(p).suffix.lower() in NON_SOURCE_EXTS for p in paths):
        return True
    if only_matching:
        return False  # -o forfeits the stat lane — per-match prefixes multiply output past the size bound
    if output_bounded and not recursive:
        # -c/-q/-l/-L is one line per operand at any size or existence; directories, unexpanded
        # glob/brace operands, and -r/-R fan back out.
        return not any(p.endswith("/") or any(ch in p for ch in "*?[{") or resolved_is_dir(p, cwd) for p in paths)
    if not all(resolved_is_file(p, cwd) for p in paths):
        return False
    return sum(resolve_operand(p, cwd).stat().st_size for p in paths) <= LARGE_READ_BYTES


def grep_operands(cmd: Command) -> list[str] | None:
    """Extract a ``grep``'s explicit path operands (the pattern excluded), or ``None`` if unparseable.

    A tolerant walk over grep's flag arities (:data:`BOUNDED_BOOL_SHORT` and friends), the grep peer of
    :func:`~hooks.rg_guards.rg_operands`, used only by the shell-expansion decline. It separates path
    operands from the pattern and from flag values, so a ``~``/``$`` in a real path operand is told
    apart from the same char in the pattern (a ``$`` end-anchor) or a flag value (``-f ~/pats``). An
    unknown flag returns ``None`` — the command stays gated, never a wrong allow.
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
        elif not all(ch in BOUNDED_BOOL_SHORT for ch in body):
            return None
        i += 1
    if not pattern_from_flag and positionals:
        return positionals[1:]
    return positionals


def grep_occurrence_expands(cmd: Command) -> bool:
    """Whether one grep occurrence carries a shell expansion that forfeits its rewrite.

    Two shapes forfeit the rewrite only — never the block: a ``$(...)``/backtick substitution (the
    parser drops the operand, so a rewrite would silently widen the search to repo-wide) or a
    non-transcript path operand carrying ``~``/``$`` (``shlex.quote`` would freeze it, so ccx would get
    a literal ``~/foo`` that does not exist). Flood-blocking still applies per-occurrence via
    :func:`bounded_file_grep`; the shell runs whatever is neither rewritten nor blocked. The pattern is
    excluded (a ``$`` end-anchor stays rewritable): only operands :func:`grep_operands` resolves as
    paths count.
    """
    return has_command_substitution(cmd.raw) or (
        (ops := grep_operands(cmd)) is not None
        and any(carries_expansion(p) and not is_transcript_path(p) for p in ops)
    )


def grep_bounded(occ: Occurrence, *, cwd: Path | None) -> bool:
    """Whether one grep occurrence runs verbatim — bounded by its own operands or by a downstream sink.

    Two independent lanes bound a grep. :func:`bounded_file_grep` proves its own operands bounded
    (stats resolved against the effective ``cwd``), and :func:`~hooks.search_common.downstream_allowed`
    proves its output pipeline terminates in a bounding sink (``head``/``tail``/``wc``) — the terminal
    caps what enters context, so the exact command runs verbatim rather than splicing ccx into the pipe.
    The downstream lane fails closed on unparseable operands and keeps the policy screens: a session
    transcript, hidden-segment dependency path, or git-ignored/out-of-repo operand blocks with its
    steer regardless of the sink, since that is policy, not boundedness.
    """
    inner = occ.command.unwrapped
    return bounded_file_grep(inner, sink=occ.prev_op == "|", cwd=cwd) or downstream_allowed(
        occ, grep_operands(inner), cwd=cwd
    )


class GrepFlood(CustomCommandLineCondition):
    """Match a line carrying any unpiped ``grep`` occurrence.

    A cheap structural gate — cwd-blind, so it cannot judge boundedness or rewritability (those turn on
    the effective cwd, which only the walk sees). :func:`grep_visit` is authoritative: it returns a
    verdict per occurrence, and a line whose every grep is a bounded search yields all-``None`` (a genuine
    allow). Matching through ``cmd.unwrapped`` keeps wrapper prefixes transparent; a pure ``… | grep``
    filter (no unpiped grep) stays outside the registration entirely.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occ.command.unwrapped.executable == "grep" and occ.prev_op != "|" for occ in cl.occurrences)


def grep_block(evt: PreToolUseEvent, cl: CommandLine) -> str:
    return search_block(
        evt,
        "grep",
        grep_operands,
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx code grep <text>` / `ccx code search` for code; the built-in Grep tool or `rg` for "
        "literal content in non-source files. Several terms? One call covers them: "
        "`ccx code grep 'a|b|c' --regex`. "
        "Simple literal and simple-regex greps auto-rewrite to `ccx code grep`; a grep whose explicit "
        "targets are all data files (`.log`/`.json`/`.yaml`/…) or existing files under the size cap "
        "(count/quiet/list-only greps — `-c`/`-q`/`-l`/`-L` — run regardless of size or existence) runs "
        "as-is, even inside pipes and `&&`/`;` chains. This one didn't qualify — a tree-wide or directory "
        "search, a recursive flag, an unmappable flag, or an over-cap/missing source-file target. "
        "Escape hatch: pipe input into it (`… | grep`).",
        cl=cl,
    )


def grep_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Per-occurrence verdict for the ``visit=`` walk, threading the effective ``cwd``.

    Non-grep occurrences pass (``None``). For a grep, the effective ``cwd`` is trusted only when the
    walk carried one (``ctx.cwd``), the raw line has no bare ``(`` (the parser flattens subshells, so
    an untrusted line would leak a ``(cd … )`` cwd into siblings), and no preceding ``cd`` is gated
    behind ``&&``/``||`` (:func:`~hooks.search_common.conditional_cd_precedes` — the shell may
    short-circuit it, leaving the grep in the original cwd); declined, every stat lane fails closed
    rather than falling back to the process cwd. A rewritable grep splices to ``ccx code grep`` when
    the occurrence is spliceable (else blocks); an unrewritable grep runs verbatim when it is a
    bounded search (:func:`grep_bounded`) and blocks otherwise. Blocking rides a :class:`HookResult`
    that aborts the walk, discarding any sibling rewrite.
    """
    if occ.command.unwrapped.executable != "grep":
        return None
    base = (
        ctx.cwd
        if ctx.cwd is not None and "(" not in evt.cmd.line.raw and not conditional_cd_precedes(occ)
        else None
    )
    if (text := grep_to(evt, occ, cwd=base)) is not None:
        if ctx.spliceable:
            return Rewritten(text, note=note_text(occ.command.raw, grep_parse(occ, cwd=base)))
        return evt.block(grep_block(evt, evt.cmd.line))
    if grep_bounded(occ, cwd=base):
        return None
    return evt.block(grep_block(evt, evt.cmd.line))


rewrite_command_occurrences(
    only_if=[GrepFlood()],
    visit=grep_visit,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, `.` widens, include-only). Path→glob
        # shapes classify each operand against the filesystem, so they live in test_grep_guards.py
        # (TestGrepPathGlobbing) where a tmp tree and pinned cwd make the classification deterministic.
        Input(command="grep -rn foo"): Rewrite(pattern="code grep foo"),  # recursive, no path → repo-wide
        Input(command="grep --recursive foo ."): Rewrite(pattern="code grep foo"),  # long recursive no-op
        Input(command="grep -rn --include='*.go' foo ."): Rewrite(pattern="--glob '*.go'"),  # `.` + include → repo-wide glob
        Input(command="grep -A 7 foo"): Rewrite(pattern="-A=7"),  # native context count preserved
        Input(command="grep -rl foo ."): Rewrite(pattern="code grep foo"),  # probe-specific -l suffix lives in pytest
        Input(command="grep -rn foo . src/"): Rewrite(pattern="code grep foo"),  # `.` sibling widens to whole repo, no --glob
        Input(command="echo x; grep -r foo ."): Rewrite(pattern="echo x; "),  # splice only grep; sibling stays byte-identical
        # Regex rewrites — a validator-cleared pattern maps onto `ccx code grep --regex` (disk-independent
        # `.` widening; the --regex probe hits the real plugin/bin/ccx, which supports it since v0.11.0):
        Input(command="grep 'foo.*' ."): Rewrite(pattern="--regex"),  # BRE-safe metachars → regex on rg engine
        Input(command="grep -E 'a|b' ."): Rewrite(pattern="--regex"),  # ERE alternation → regex on rg engine
        Input(command="grep -E 'a+' ."): Rewrite(pattern="--regex"),  # ERE `+` is a quantifier → validator → regex
        Input(command="grep 'a+' ."): Rewrite(pattern="code grep a+"),  # BRE `+` is literal → literal rewrite, no --regex
        Input(command="grep 'a\\|b' ."): Rewrite(pattern="--regex"),  # BRE `\|` → ERE `|` alternation, rewritten to regex
        Input(command="grep 'x\\(ab\\)\\+' ."): Rewrite(pattern="--regex"),  # BRE group + `\+` → `(ab)+` on the rg engine
        # Block — unmappable shapes fall back to the message:
        Input(command="grep -rnC3 foo src/"): Block(),  # value short glued into a bundle
        Input(command="grep --recursive=oops foo ."): Block(),  # native grep rejects a value on a no-value long
        Input(command="grep -P 'x(?=y)' ."): Block(),  # PCRE (-P) never maps; `.` is a dir, not a bounded file
        Input(command="grep 'a^b' ."): Block(),  # BRE mid-pattern `^`: literal in grep, an anchor in Rust → not rewritable
        Input(command="grep -F 'foo.*' ."): Block(),  # -F forces literal; `foo.*` isn't ccx-literal-safe → no --regex flip
        Input(command="grep -E -F foo ."): Block(),  # -F with -E: conflicting matchers, grep errors → block
        Input(command="grep -q foo src/"): Block(),  # exit-code contract, tree-wide
        Input(command="grep -c foo src/"): Block(),  # count mode, tree-wide
        Input(command="grep -o foo src/"): Block(),  # -o output is bounded, but src/ is a dir → tree-wide
        Input(command="grep -e foo -e bar ."): Block(),  # multiple -e over a dir
        Input(command="grep -iw foo src/"): Block(),  # MAP chars bundled → block (engine-independent)
        Input(command="grep -f patterns.txt ."): Block(),  # -f pattern-file over a dir
        # Allow — an unrewritable grep over an explicit existing file is bounded, so the condition never
        # fires (/etc/hosts is a regular file on every CI OS: macOS + Linux):
        Input(command="grep -rn foo /etc/hosts"): Allow(),  # absolute path, but a bounded existing file
        # Count/quiet/list-only greps run at any size or existence — a missing operand (e.g. created
        # earlier in the same compound) is one grep error line; directory-shaped operands stay blocked:
        Input(
            command="curl -sL -o /tmp/ch-live.html https://yasyf.github.io/captain-hook/ && "
            "wc -c < /tmp/ch-live.html && for m in 'id=\"links\"' gd-hero; do "
            "printf '%s: %s\\n' \"$m\" \"$(grep -c \"$m\" /tmp/ch-live.html)\"; done"
        ): Allow(),  # the incident: -c over a file the curl creates later in the same compound
        Input(command="grep -q pat missing.html"): Allow(),  # -q on a missing file: one error line + exit code
        Input(command="grep -l foo a.html b.html"): Allow(),  # -l: at most one line per (missing) operand
        Input(command="grep -c needle file{1..10000}"): Block(),  # brace expansion multiplies operands post-eval
        Input(command="grep -c needle *"): Block(),  # unexpanded glob — operand count unknown at eval time
        # Per-occurrence data-file passthrough (the incident class) — data-ext operands pass by suffix
        # with no stat, so each grep runs even inside pipes and `&&`/`;` chains, and a file created
        # earlier in the same compound or reached via an in-command `cd` still qualifies:
        Input(
            command="cd /tmp/scratch && gog --account user@example.com --readonly --json gmail get MSGID "
            "--json > b_jetblue_jun.json; grep -oiE '[0-9][0-9,]{2,} ?(points|TrueBlue)' b_jetblue_jun.json "
            "| head; grep -oiE 'mosaic( [0-9])?' b_jetblue_jun.json | sort | uniq -c"
        ): Allow(),
        Input(command="grep -oi points b_jetblue_jun.json"): Allow(),  # standalone -o on a data file (data-ext keeps -o)
        Input(command="echo x > gen.json; grep -i points gen.json"): Allow(),  # created earlier in the compound
        Input(command="grep -i err app.log | head"): Allow(),  # a downstream pipe no longer disqualifies
        Input(command="cd sub && grep foo notes.json"): Allow(),  # cd-relative data file
        # Downstream-bounded lane: a direct grep whose output pipeline ends in a head/tail/wc sink runs
        # verbatim — the sink caps context, ccx is never spliced into a pipe. Flip of a former Block row.
        Input(command="grep -r foo src/ | head"): Allow(),
        Input(command="grep -r x . | head -n 20"): Allow(),  # in-cap sink count keeps the lane
        Input(command="grep -r x . | head -n 100000"): Block(),  # over-cap line count re-opens the flood
        Input(command="grep -r x . | head -c 100000000"): Block(),  # a 100MB byte count is no sink
        Input(command="grep -r x . | tail -c 100000000"): Block(),  # tail counts cap the same way
        Input(command="grep --line-buffered -r foo . | head"): Allow(),  # long cosmetic boolean parses; the sink bounds
        # ANY intermediate `tee` defeats the sink — a deliberate blanket, not just terminal devices.
        Input(command="grep -r x . | head"): Allow(),  # tee neighbor: same sink, no intermediate stage
        Input(command="grep -r x . | tee /tmp/out | head"): Block(),  # file-target tee blocks too
        Input(command="grep -r x . | tee /dev/tty | head"): Block(),
        Input(command="grep -r x . | tee /dev/stderr | wc -l"): Block(),
        Input(command="grep -r x . | wc --files0-from=-"): Block(),  # wc reading a file list fans out
        # A pipe never launders a flood: a non-bounding terminal keeps the block.
        Input(command="grep -r foo src/ | grep -v x"): Block(),  # terminal is grep -v (a filter), not a sink
        Input(command="grep -rn foo . | sort"): Block(),  # sort is not a bounding sink → no rewrite into the pipe
        Input(command="grep -r foo . | tail -f"): Block(),  # tail -f follows forever → unbounded
        Input(command="grep -rn foo . | tail -n +5"): Block(),  # tail -n +5 prints from an offset to EOF → unbounded
        Input(command="grep -r foo ~/.claude/projects/ | head"): Block(pattern="cc-transcript"),  # transcript: policy over the sink
        # The incident regression, verbatim: two direct greps piped through filters to a terminal `head`.
        Input(
            command="cd ~/Code/cc-skills && grep -rl 'wrangler' plugins docs tools plugin 2>/dev/null "
            "| grep -v node_modules | head; grep -rl 'deploy.sh' plugins docs 2>/dev/null | head"
        ): Allow(),
        # Per-occurrence blocks — one qualifying grep can't launder a tree-wide/recursive/no-operand grep:
        Input(command="grep -i points data.json && grep foo ."): Rewrite(
            pattern="grep -i points data.json && "
        ),  # bounded data-file sibling stays byte-identical while the tree grep splices
        Input(command="grep -c foo data.json && grep -rn bar ."): Rewrite(
            pattern="grep -c foo data.json && "
        ),  # bounded output sibling no longer vetoes the splice
        Input(command="grep -c foo src/ && grep -rn bar ."): Block(),  # flooding sibling still vetoes the line
        Input(command="grep -r foo logs.json"): Block(),  # -r forfeits the data-ext textual escape
        Input(command="echo hi; grep -o foo"): Block(),  # no operand in a compound → not bounded
        Input(command="echo x; grep -c foo ."): Block(),  # one flooding grep blocks the whole compound line
        # -o forfeits the STAT lane (per-match prefixes multiply output); data-ext keeps -o (above):
        Input(command="grep -o localhost /etc/hosts"): Block(),  # -o floods the stat path on a non-data file
        Input(command="grep -oHnb . AGENTS.md"): Block(),  # -oHnb = filename/line/byte prefixes per match on a source file
        # Holes closed by the adversarial review — env injection, sink-with-operands, flag-supplied patterns:
        Input(command="grep -q localhost /etc/hosts | grep -r . /"): Block(),  # sink grep w/ operands ignores stdin, recurses /
        Input(command="GREP_OPTIONS=-v grep foo ."): Block(),  # rewriting must not delete the env prefix's semantics
        Input(command="GREP_OPTIONS=-r grep -o needle dir.json"): Block(),  # GREP_OPTIONS injects -r past the parser
        Input(command="grep --regexp= data.json"): Block(),  # empty flag-supplied pattern floods every line
        Input(command="grep -e '' data.json"): Block(),  # empty -e pattern floods every line
        Input(command="grep -f pats.txt data.json"): Block(),  # -f pattern file is uninspectable
        # A `wc` terminal bounds the pipeline, so the nonexistent-source-ext grep runs verbatim (flip of a
        # former Block row); the `&&` neighbor has no bounding sink, so it still blocks.
        Input(command="grep foo ghost.py | wc -l"): Allow(),
        Input(command="grep foo ghost && echo done"): Block(),
        # A `~`/`$` operand forfeits the rewrite but NOT the block — unverifiable → flood.
        Input(command="grep foo ~/notes.md"): Block(pattern="floods context"),
        Input(command="grep -n foo $d/host.go"): Block(pattern="floods context"),
        Input(command="grep foo ~/app.log"): Allow(),  # data-ext operand stays bounded by suffix (no stat)
        Input(command="grep -r . . ~/notes.md"): Block(),  # `-r .` flood is real; `~` only forfeits the rewrite
        Input(command="grep -r foo ~/.claude/projects/"): Block(pattern="cc-transcript"),  # transcript steer
        Input(command="grep 'foo$' ."): Rewrite(pattern="--regex"),  # `$` in the PATTERN is an anchor, not a path
        # Substitution drops the `$(…)`/backtick operand → rewrite forfeited; the operand-less grep floods → block.
        Input(command="grep foo $(printf /tmp/target)"): Block(pattern="floods context"),
        Input(command="grep -n foo `printf x`"): Block(pattern="floods context"),
        Input(command="grep -r . . '$(printf x)'"): Block(),  # crude detector forfeits the rewrite; `-r .` blocks
        Input(command="grep foo ."): Rewrite(pattern="code grep foo"),  # control: plain path still rewrites
        # Per-occurrence: the `$d` grep forfeits its rewrite, the `.` tree search floods → the line blocks.
        Input(command="grep foo $d/host.go; grep bar ."): Block(pattern="floods context"),
        # Mixed transcript + flood: the block carries BOTH the default steer and the cc-transcript line.
        Input(command="grep foo ~/.claude/projects/main.jsonl; grep -v bar ."): Block(pattern="cc-transcript"),
        Input(command="sudo grep foo ."): Block(),  # Approved wrapper-transparency decision (2026-07-17).
        Input(command="timeout 10 grep foo ."): Block(),  # Approved wrapper-transparency decision (2026-07-17).
        # Existing Allow neighbors — condition unchanged (piped grep, non-grep, ccx exec):
        Input(command="ls | grep foo"): Allow(),
        Input(command="cat x | grep foo | sort"): Allow(),
        Input(command="git log --grep=fix"): Allow(),
        Input(command='git log --grep "fix bug"'): Allow(),
        # `ccx exec` pass-through is deliberate: sh("grep …") is in-sandbox and
        # budget-capped on return — internal/codeexec/sh.go owns its policy, not hooks.
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
