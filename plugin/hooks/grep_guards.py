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
    Rewrite,
    Tool,
    rewrite_command,
)

from .common import LARGE_READ_BYTES, LITERAL_SAFE, is_single_command
from .search_common import (
    CONTEXT_SHORT,
    GrepCall,
    NON_SOURCE_EXTS,
    build_ccx_grep,
    classify_path,
    grep_glob,
    note_text,
    path_blocked,
    unpiped,
    unquote,
)

if TYPE_CHECKING:
    from cc_transcript.command import Command


# grep flags ccx code grep subsumes as no-ops on its literal, always-recursive, line-numbered
# engine. `-l` and `-F` change output/semantics, so the note discloses their drop; the rest
# (`-r -R -n -H -h -s -I`) are silent. Long-form DROP flags aren't in the set, so `--recursive`
# and friends fall through to the block — conservative, never wrong.
DROP_SHORT = frozenset("rRnHhsIFl")

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


def grep_targets(paths: list[str], include: str | None) -> tuple[str, list[str]] | None:
    """Split grep's path args into a ``(glob, path_operands)`` pair, or ``None`` to block.

    Two or more explicit *existing regular files* (no ``--include``, no ``.``-widening) carry as
    ``ccx code grep`` positionals — the multi-file form (ccx ≥ v0.11.0) — with an empty glob.
    Everything else routes through :func:`grep_glob`: a directory, a lone file (``--glob file`` for
    old-binary compat), an ``--include``, or repo-wide widening yield a glob and no path operands.
    """
    if include is None and not any(p in (".", "./") for p in paths) and len(paths) >= 2:
        if all(classify_path(p) is False for p in paths):
            return "", [p.rstrip("/") for p in paths]
    glob = grep_glob(paths, include)
    return None if glob is None else (glob, [])


def valid_brace(body: str) -> bool:
    """Report whether an ERE interval body (``{m}``/``{m,n}``/``{m,}``) is digits-and-comma only."""
    return re.fullmatch(r"\d+(,\d*)?", body) is not None


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


def grep_parse(cl: CommandLine) -> GrepCall | None:
    """Parse an unpiped ``grep`` into its ccx-rewritable shape, or ``None`` to fall back to block.

    Rewrites only a single command whose flags all fall in the DROP/MAP sets and whose one
    pattern is a plain literal (:data:`LITERAL_SAFE`). Anything regex-shaped, exit-code /
    output-mode oriented (``-c -q -o -v -L -x``), alternate-engine (``-E -P -G``), multi-pattern
    (repeated ``-e``), or reaching outside the repo (absolute / ``~`` / ``..`` path) blocks. A
    value-taking short glued into a bundle (``-rnC3``) blocks too — bundles are DROP-only.
    """
    if not is_single_command(cl):
        return None
    args = cl.primary.args
    pattern: str | None = None
    e_count = 0
    include: str | None = None
    positionals: list[str] = []
    expand = ignore_case = word = dropped_l = dropped_fixed = ere = bre = False
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
                else:
                    if i + 1 >= n or not args[i + 1].isdigit():
                        return None
                    i += 1
                expand = True
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
            elif i + 1 < n and args[i + 1].isdigit():
                i += 1
            else:
                return None
            expand = True
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
        if path_blocked(p):
            return None
    targets = grep_targets(paths, include)
    if targets is None:
        return None
    glob, path_ops = targets
    return GrepCall(
        pattern,
        glob,
        "3" if expand else "",
        ignore_case,
        word,
        dropped_l,
        dropped_fixed,
        count_dropped=True,
        regex=regex,
        paths=tuple(path_ops),
    )


def grep_to(evt: BaseHookEvent) -> str | None:
    parsed = grep_parse(evt.command_line)
    return build_ccx_grep(parsed) if parsed is not None else None


def grep_note(evt: BaseHookEvent) -> str:
    return note_text(evt.command, grep_parse(evt.command_line))


def bounded_file_grep(cmd: Command, *, sink: bool = False) -> bool:
    """Report whether one ``grep`` statement is a bounded search ccx can't rewrite.

    Judges a single grep occurrence on its own flags and operands — not the whole Bash line — so it
    holds per-occurrence inside a pipe or ``&&``/``;`` chain. ``sink`` marks a grep on the receiving
    end of a pipe (``… | grep``): with no path operand it just filters stdin, so an unparseable or
    operand-less sink grep is presumed a bounded filter, whereas the same shape unpiped is not.

    Two shapes qualify as a bounded file search:

    - *data-ext textual*: every operand is an explicit :data:`NON_SOURCE_EXTS` file matched by suffix
      with no stat, so a file created earlier in the same compound command or addressed relative to an
      in-command ``cd`` passes; ``-o`` is fine here (rg parity), but a recursion flag forfeits it (a
      directory named like a data file under ``-r`` would flood).
    - *bounded regular files*: every operand stats as an existing regular file whose sizes sum to no
      more than :data:`~hooks.common.LARGE_READ_BYTES` — or, on a count/quiet/list-only grep
      (``-c``/``-q``/``-l``/``-L``/their long forms), any size, since that output is one line per operand
      regardless of file size; ``-o`` forfeits this stat lane, since its per-match filename/line/byte
      prefixes multiply output past the size bound.

    Conservative throughout: a ``GREP_OPTIONS`` env (which can inject ``-r``/``-o`` the parser never
    sees), any env alongside path operands, an uninspectable ``-f`` pattern file, a flag-supplied empty
    pattern, and an unknown flag on an unpiped grep all return ``False`` — never a wrong allow.
    """
    if "GREP_OPTIONS" in cmd.env_dict:
        return False  # a leading GREP_OPTIONS= injects flags (-r/-o …) that never reach cmd.args
    args = cmd.args
    positionals: list[str] = []
    pattern_from_flag = only_matching = pattern_file = empty_flag_pattern = recursive = output_bounded = False
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
                if not sep:
                    i += 1
            elif name in BOUNDED_BOOL_LONG:
                recursive = recursive or name in ("recursive", "dereference-recursive")
                only_matching = only_matching or name == "only-matching"
                output_bounded = output_bounded or name in (
                    "count", "quiet", "silent", "files-with-matches", "files-without-match"
                )
            else:
                return sink
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
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif all(ch in BOUNDED_BOOL_SHORT for ch in body):
            recursive = recursive or "r" in body or "R" in body
            only_matching = only_matching or "o" in body
            output_bounded = output_bounded or any(ch in "cqlL" for ch in body)
        else:
            return sink
        i += 1
    if pattern_from_flag:
        empty_positional_pattern = False
        paths = positionals
    elif not positionals:
        return False  # no pattern and no operand
    else:
        empty_positional_pattern = positionals[0] == ""
        paths = positionals[1:]
    if recursive and not paths:
        return False  # grep -r with no operand recurses the cwd, even as a pipe sink
    if not paths:
        return sink  # a pure stdin filter passes; an unpiped stdin-grep fails as today
    if cmd.env or pattern_file or empty_flag_pattern:
        return False  # env with paths, an uninspectable -f pattern file, or a flag-supplied empty pattern
    if empty_positional_pattern:
        return False  # an empty positional pattern floods every line
    # Data files pass by suffix, no stat; recursion forfeits it — a dir named like a data file floods.
    if not recursive and all(not p.endswith("/") and Path(p).suffix.lower() in NON_SOURCE_EXTS for p in paths):
        return True
    if only_matching:
        return False  # -o forfeits the stat lane — per-match prefixes multiply output past the size bound
    if not all(Path(p).is_file() for p in paths):
        return False
    if output_bounded:
        return True  # -c/-q/-l/-L output is one line per operand, not per match, so file size can't flood
    return sum(Path(p).stat().st_size for p in paths) <= LARGE_READ_BYTES


class GrepFlood(CustomCommandLineCondition):
    """Matches a file-search ``grep`` unless every ``grep`` occurrence is a bounded, unrewritable grep.

    Fires so the hook rewrites it (``grep_to`` yields the command) or blocks it (``grep_to`` yields
    ``None``). The ``unpiped`` guard keeps a line with no unpiped ``grep`` out of scope (a pure
    ``… | grep`` filter). Once in scope it stays silent only when *every* ``grep`` on the line — sink
    greps included, since a sink grep with file operands ignores stdin and searches those files — is a
    bounded explicit-files or data-file grep that ccx can't rewrite. The captain-hook contract turns a
    ``None`` ``to`` under a set ``block`` into an unconditional block, so this per-occurrence allow
    lives here in the condition.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not unpiped(cl, "grep"):
            return False
        if grep_to(evt) is not None:
            return True
        return any(
            not bounded_file_grep(cmd, sink=i > 0 and cl.parts[i - 1][1] == "|")
            for i, (cmd, _) in enumerate(cl.parts)
            if cmd.executable == "grep"
        )


rewrite_command(
    only_if=[Tool("Bash"), GrepFlood()],
    to=grep_to,
    block=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx code grep <text>` (or mcp__cc-context__ccx_code_grep) / `ccx code search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Simple literal and simple-regex greps auto-rewrite to `ccx code grep`; a grep whose explicit "
        "targets are all data files (`.log`/`.json`/`.yaml`/…) or existing files under the size cap "
        "(count/quiet/list-only greps — `-c`/`-q`/`-l`/`-L` — run regardless of size) runs "
        "as-is, even inside pipes and `&&`/`;` chains. This one didn't qualify — a tree-wide or directory "
        "search, a recursive flag, an unmappable flag, or an over-cap/missing source-file target. "
        "Escape hatch: pipe input into it (`… | grep`)."
    ),
    note=grep_note,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, `.` widens, include-only). Path→glob
        # shapes classify each operand against the filesystem, so they live in test_grep_guards.py
        # (TestGrepPathGlobbing) where a tmp tree and pinned cwd make the classification deterministic.
        Input(command="grep -rn foo"): Rewrite(pattern="code grep foo"),  # recursive, no path → repo-wide
        Input(command="grep -rn --include='*.go' foo ."): Rewrite(pattern="--glob '*.go'"),  # `.` + include → repo-wide glob
        Input(command="grep -rl foo ."): Rewrite(pattern="code grep foo"),  # -l is a no-op; `.` → repo-wide
        Input(command="grep -rn foo . src/"): Rewrite(pattern="code grep foo"),  # `.` sibling widens to whole repo, no --glob
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
        # Per-occurrence blocks — one qualifying grep can't launder a tree-wide/recursive/no-operand grep:
        Input(command="grep -r foo src/ | head"): Block(),  # recursive tree search; the pipe doesn't exempt it
        Input(command="grep -i points data.json && grep foo ."): Block(),  # data-file grep + a `.` tree search
        Input(command="grep -r foo logs.json"): Block(),  # -r forfeits the data-ext textual escape
        Input(command="echo hi; grep -o foo"): Block(),  # no operand in a compound → not bounded
        # -o forfeits the STAT lane (per-match prefixes multiply output); data-ext keeps -o (above):
        Input(command="grep -o localhost /etc/hosts"): Block(),  # -o floods the stat path on a non-data file
        Input(command="grep -oHnb . AGENTS.md"): Block(),  # -oHnb = filename/line/byte prefixes per match on a source file
        # Holes closed by the adversarial review — env injection, sink-with-operands, flag-supplied patterns:
        Input(command="grep -q localhost /etc/hosts | grep -r . /"): Block(),  # sink grep w/ operands ignores stdin, recurses /
        Input(command="GREP_OPTIONS=-r grep -o needle dir.json"): Block(),  # GREP_OPTIONS injects -r past the parser
        Input(command="grep --regexp= data.json"): Block(),  # empty flag-supplied pattern floods every line
        Input(command="grep -e '' data.json"): Block(),  # empty -e pattern floods every line
        Input(command="grep -f pats.txt data.json"): Block(),  # -f pattern file is uninspectable
        # Existing block neighbors — each unpiped grep is judged on its own operands; a nonexistent
        # source-ext target isn't bounded, so the line still blocks:
        Input(command="grep foo ghost.py | wc -l"): Block(),
        Input(command="grep foo ghost && echo done"): Block(),
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
