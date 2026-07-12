"""Search guards: nudge identifier-alternation and natural-language ``rg``/``grep`` to ``ccx``; rewrite simple literal ``grep``/``rg`` file search to ``ccx code grep``, block the rest."""

from __future__ import annotations

import re
import shlex
import subprocess
from pathlib import Path
from typing import TYPE_CHECKING, NamedTuple

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    Warn,
    nudge,
    rewrite_command,
)

from .common import IDENT_ALT, LARGE_READ_BYTES, LITERAL_SAFE, ccx_bin, ccx_supports, is_single_command

if TYPE_CHECKING:
    from cc_transcript.command import Command

# The two file-search executables the nudges steer. A pipe's primary (last) command
# is what carries the pattern, so `… | grep 'a|b'` matches by its `grep` primary, the
# same way the rg nudge has always matched `… | rg 'a|b'`.
SEARCH_EXECUTABLES = ("rg", "grep")

# An rg/grep pattern that is two or more space-separated all-lowercase words reads as a
# natural-language intent query ("parse the config file"), which `ccx code search`
# answers semantically. Requiring every word lowercase keeps code-literal patterns out:
# `func NewRootCmd` (capital, parens) and a lone `TODO` never match, and any regex
# metacharacter or path separator (`.`, `*`, `|`, `/`) breaks the letters-and-single-
# spaces run so path- and regex-shaped patterns never match either.
NL_PHRASE = re.compile(r"^[a-z]+(?: [a-z]+)+$")

# grep flags ccx code grep subsumes as no-ops on its literal, always-recursive, line-numbered
# engine. `-l` and `-F` change output/semantics, so the note discloses their drop; the rest
# (`-r -R -n -H -h -s -I`) are silent. Long-form DROP flags aren't in the set, so `--recursive`
# and friends fall through to the block — conservative, never wrong.
DROP_SHORT = frozenset("rRnHhsIFl")
# ripgrep's own DROP table — never grep's: rg's short flags are false friends (`-r` takes a
# value, `-E` is encoding, `-I` is no-filename). `-n`/`-N`/`-s`/`-H`/`-I` are cosmetic; `-l`
# (files-with-matches) and `-F` (fixed-strings) disclose their drop through the note.
RG_DROP_SHORT = frozenset("nNsHIlF")
# Short flags naming a numeric context window (`-A/-B/-C N`), all mapped to `--expand`.
CONTEXT_SHORT = frozenset("ABC")
# An `--include` value is a glob, not a pattern, so it skips LITERAL_SAFE — but it must be a
# simple glob (no braces, no spaces) to compose cleanly onto a braced multi-dir root.
INCLUDE_SAFE = re.compile(r"^[\w*?./\[\]-]+$")

# Regex metacharacters per grep dialect. A pattern carrying NONE of the active dialect's
# metachars is a plain literal in that dialect (→ literal rewrite when ccx-literal-safe); one
# carrying any is handed to `_regex_rewritable`, which admits it onto `--regex` only when its
# meaning is identical in grep and the Rust-regex engine. BRE reads `+ ? | ( ) { }` as literal
# (so `a+` under the default is a literal), ERE as metachars.
BRE_METACHARS = frozenset(".*^$[\\")
ERE_METACHARS = BRE_METACHARS | frozenset("+?|(){}")
# Chars the validator treats as a plain literal atom — identical in grep BRE/ERE and Rust regex.
# `.` (the any-char wildcard, also an atom) is admitted here too. Brackets, backslash, quotes,
# backticks, and a non-terminal `$` are excluded: shell-active chars stay out as defense in depth
# atop the downstream shlex-quoting, and bracket/backslash constructs diverge across dialects.
REGEX_ATOM = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_ .:@,=/-")

# Data-file suffixes that make a raw `rg` a sanctioned non-source search (`rg ERROR app.log`),
# exempt from the rg gate. A purely textual `Path.suffix` check — no stat.
NON_SOURCE_EXTS = frozenset(
    {".log", ".txt", ".csv", ".tsv", ".json", ".jsonl", ".ndjson", ".yaml", ".yml", ".toml", ".ini"}
)
# rg flags whose next token is a value, for the tolerant `_rg_operands` walk (separate from the
# strict rewrite parser). `-e`/`-f`/`--regexp`/`--file` supply the pattern and are handled apart.
RG_OP_VALUE_SHORT = frozenset("gtTABCmrEjMd")
# rg's boolean short flags — they take no value, so `_rg_operands` may skip one (or an all-boolean
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

# Known-arity grep flag tables for `_bounded_file_grep`'s tolerant lexer: to tell a bounded
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


class RgIdentAlternation(CustomCommandLineCondition):
    """Matches an ``rg``/``grep`` whose pattern is an identifier alternation.

    `rg 'fooBar|bazQux' src/` — or the same via `grep` — is almost always "find these
    symbols" — `ccx code symbol` resolves a definition plus its callers in one shot, and
    `ccx code grep` groups hits compactly. A single-term search carries no such signal, so
    it does not match. Bare `grep` file search is still blocked by the grep guard; this
    nudge rides alongside that block with the sharper symbol-lookup steer.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(cl.q.runs(exe) for exe in SEARCH_EXECUTABLES) and any(IDENT_ALT.search(a) for a in cl.primary.args)


nudge(
    "Searching for several identifiers? `ccx code symbol <name>` (or mcp__cc-context__ccx_code_symbol) "
    "resolves a definition plus its callers in one call; `ccx code grep <text>` groups hits "
    "compactly. This search still runs — just consider the ccx tools for symbol lookups.",
    only_if=[Tool("Bash"), RgIdentAlternation()],
    events=Event.PreToolUse,
    tests={
        Input(command="rg 'fooBar|bazQux' src/"): Warn(pattern="ccx code symbol"),
        Input(command="rg 'Foo|Bar|Baz' ."): Warn(),
        Input(command="grep 'fooBar|bazQux' src/"): Warn(pattern="ccx code symbol"),
        Input(command="rg TODO"): Allow(),
        Input(command="grep TODO ."): Allow(),
        Input(command="rg 'just one term' src/"): Allow(),
        # `ccx exec` pass-through is deliberate: the alternation inside the script
        # matches IDENT_ALT, but the line runs ccx, not rg/grep.
        Input(
            command="ccx exec 'async def main(): return await sh(\"rg \\\"fooBar|bazQux\\\" src/\")\n"
            "asyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            "async def main(): return await sh(\"rg 'fooBar|bazQux' src/\")\n"
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)


class NaturalLanguagePhrase(CustomCommandLineCondition):
    """Matches an ``rg``/``grep`` whose pattern is a natural-language phrase.

    `rg "parse the config file"` reads as a question about intent, not a literal string to
    match — `ccx code search "<question>"` answers it semantically. The pattern must be two
    or more all-lowercase words (:data:`NL_PHRASE`); a single word, a code-literal like
    `func NewRootCmd`, a bare `TODO`, and path- or regex-shaped tokens carry no intent
    signal and do not match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(cl.q.runs(exe) for exe in SEARCH_EXECUTABLES) and any(NL_PHRASE.match(a) for a in cl.primary.args)


nudge(
    'Searching for a concept rather than a literal string? `ccx code search "<question>"` '
    "(or mcp__cc-context__ccx_code_search) runs a semantic search that finds code by intent, "
    "not exact text. This search still runs — just consider ccx code search for phrase-shaped queries.",
    only_if=[Tool("Bash"), NaturalLanguagePhrase()],
    events=Event.PreToolUse,
    tests={
        Input(command='rg "parse the config file"'): Warn(pattern="ccx code search"),
        Input(command='grep "load the settings" src/'): Warn(pattern="ccx code search"),
        Input(command='rg "func NewRootCmd"'): Allow(),
        Input(command="rg TODO"): Allow(),
        Input(command="rg parseConfig"): Allow(),
        Input(command='rg "src/config" .'): Allow(),
        Input(command='rg "foo.*bar"'): Allow(),
        # `ccx exec` pass-through is deliberate: the phrase lives inside the script token.
        Input(
            command="ccx exec 'async def main(): return await sh(\"rg \\\"parse the config file\\\"\")\n"
            "asyncio.run(main())'"
        ): Allow(),
    },
)


def _unpiped(cl: CommandLine, exe: str) -> bool:
    """Report whether ``exe`` runs as a file searcher — not merely as a pipe sink consuming stdin."""
    return any(
        cmd.executable == exe and (i == 0 or cl.parts[i - 1][1] != "|") for i, (cmd, _) in enumerate(cl.parts)
    )


class UnpipedSearch(CustomCommandLineCondition):
    """Matches a search executable (``grep``/``rg``) that does not consume piped input.

    Parametrized by ``exe`` so one class gates both engines. Allows the stream-filter idiom
    (`… | rg`) while still matching the exe used for file searching, whether standalone,
    heading a pipe, or in a `&&`/`;` chain.
    """

    def __init__(self, exe: str) -> None:
        self.exe = exe

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return _unpiped(cl, self.exe)


class GrepCall(NamedTuple):
    pattern: str
    glob: str  # "" → repo-wide (no --glob)
    expand: str  # "" → no context flag; else the `--expand=<value>` count
    ignore_case: bool
    word: bool
    dropped_l: bool
    dropped_fixed: bool
    count_dropped: bool  # True when `--expand` holds a fixed placeholder, so the note flags the `-A/-B/-C N` count as lost
    regex: bool = False  # pattern rewrites to `--regex` (ran as a regex on the rg engine)
    paths: tuple[str, ...] = ()  # explicit multi-file operands carried as `ccx code grep` positionals


def _classify_path(p: str) -> bool | None:
    """Classify a grep path operand against the filesystem from the hook's cwd.

    ``True`` for an existing directory, ``False`` for an existing file, ``None`` when the
    path is on disk as neither. A real stat is the only faithful test: the old extension
    heuristic mis-globbed an extensionless file (``Makefile`` → ``Makefile/**`` → a silent
    0-match) and a dotted directory (``internal/v2.5`` → treated as a file). A nonexistent
    path has no correct glob, so the caller blocks rather than guess — never a silently
    wrong search.
    """
    path = Path(p)
    if path.is_dir():
        return True
    if path.is_file():
        return False
    return None


def _brace(dirs: list[str]) -> str:
    return dirs[0] if len(dirs) == 1 else "{" + ",".join(dirs) + "}"


def _unquote(s: str) -> str:
    """Strip one matching pair of surrounding quotes.

    The command parser removes quotes wrapping a whole token but keeps them on a *glued*
    flag value — ``--include='*.go'`` arrives with its quotes intact — so a glued value is
    unquoted before it is used as a glob or pattern.
    """
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def _grep_glob(paths: list[str], include: str | None) -> str | None:
    """Build the ``--glob`` body for grep's path args: ``""`` for repo-wide, ``None`` to block.

    A ``.``/``./`` among the paths widens the search to the whole repo — every sibling path
    is a subset, so no ``--glob`` narrows it (an ``--include`` still applies repo-wide as a
    bare ``*.go``). Otherwise each path is classified against the filesystem: directories
    become ``dir/**`` (braced when several: ``{a,b}/**``), a lone file passes through as-is,
    and an ``--include`` glob composes onto the dir roots (``dir/**/*.go``). A nonexistent
    path, mixed file+dir paths, several files, and an include over explicit files have no
    faithful single-glob form → block.
    """
    if any(p in (".", "./") for p in paths):
        if include is None:
            return ""
        return include if INCLUDE_SAFE.match(include) else None
    dirs: list[str] = []
    files: list[str] = []
    for p in paths:
        kind = _classify_path(p)
        if kind is None:
            return None
        (dirs if kind else files).append(p.rstrip("/"))
    if include is not None:
        if not INCLUDE_SAFE.match(include) or files:
            return None
        return include if not dirs else f"{_brace(dirs)}/**/{include}"
    if dirs and files:
        return None
    if files:
        return files[0] if len(files) == 1 else None
    if dirs:
        return f"{_brace(dirs)}/**"
    return ""


def _grep_targets(paths: list[str], include: str | None) -> tuple[str, list[str]] | None:
    """Split grep's path args into a ``(glob, path_operands)`` pair, or ``None`` to block.

    Two or more explicit *existing regular files* (no ``--include``, no ``.``-widening) carry as
    ``ccx code grep`` positionals — the multi-file form (ccx ≥ v0.11.0) — with an empty glob.
    Everything else routes through :func:`_grep_glob`: a directory, a lone file (``--glob file`` for
    old-binary compat), an ``--include``, or repo-wide widening yield a glob and no path operands.
    """
    if include is None and not any(p in (".", "./") for p in paths) and len(paths) >= 2:
        if all(_classify_path(p) is False for p in paths):
            return "", [p.rstrip("/") for p in paths]
    glob = _grep_glob(paths, include)
    return None if glob is None else (glob, [])


def _git_ignored(p: str) -> bool:
    """Best-effort ``git check-ignore``: ``True`` only when git reports ``p`` ignored.

    Runs from the hook's cwd — where the search would run. Anything but a clean ignore hit (a
    tracked path, git absent, not a repo) returns ``False`` so the rewrite still proceeds.
    """
    try:
        proc = subprocess.run(["git", "check-ignore", "-q", p], capture_output=True)
    except OSError:
        return False
    return proc.returncode == 0


def _path_blocked(p: str) -> bool:
    """Report whether a grep/rg path operand must fall through to the block.

    Rejects paths reaching outside the repo (absolute, ``~``, ``..``), non-literal paths, any
    path with a hidden segment (``.venv``, ``node_modules/.cache``), and — best-effort — paths
    ``git check-ignore`` reports ignored. Rewriting a search inside an ignored or hidden dir to a
    ``--glob`` that a stale ``ccx`` silently 0-matches is worse than blocking with the
    dependency-source steer, so those operands block instead.
    """
    stripped = p.rstrip("/")
    if p.startswith(("/", "~")) or not LITERAL_SAFE.match(stripped):
        return True
    segments = stripped.split("/")
    if ".." in segments:
        return True
    if any(seg.startswith(".") and seg not in (".", "..") for seg in segments):
        return True
    return _git_ignored(p)


def _valid_brace(body: str) -> bool:
    """Report whether an ERE interval body (``{m}``/``{m,n}``/``{m,}``) is digits-and-comma only."""
    return re.fullmatch(r"\d+(,\d*)?", body) is not None


def _regex_rewritable(pattern: str, ere: bool) -> bool:
    """Report whether ``pattern`` maps faithfully onto ``ccx code grep --regex`` (the Rust-regex engine).

    A position-aware dialect check — not a character whitelist, which can't distinguish a literal
    mid-pattern ``^`` (grep) from an anchor (Rust). Admits only constructs whose meaning is identical
    in grep (BRE when ``ere`` is false, else ERE) and Rust regex: plain atoms (:data:`REGEX_ATOM`,
    ``.`` the wildcard), ``*`` (and ``+``/``?`` under ERE) never leading or stacked, ``^`` only first
    and ``$`` only last, and — ERE only — ``|`` alternation, balanced ``()`` groups, and digits-only
    ``{m,n}`` intervals. Brackets, backslashes, and any other char are not rewritable.
    """
    n = len(pattern)
    depth = 0
    quantifiable = False  # a preceding atom a quantifier may bind
    quantifier = False  # the preceding token was itself a quantifier (no stacking)
    i = 0
    while i < n:
        c = pattern[i]
        if c in REGEX_ATOM:
            quantifiable, quantifier = True, False
        elif c == "*" or (ere and c in "+?"):
            if not quantifiable or quantifier:
                return False
            quantifier = True
        elif c == "^":
            if i != 0:
                return False
            quantifiable = quantifier = False
        elif c == "$":
            if i != n - 1:
                return False
            quantifiable = quantifier = False
        elif ere and c == "|":
            if not quantifiable:
                return False
            quantifiable = quantifier = False
        elif ere and c == "(":
            depth += 1
            quantifiable = quantifier = False
        elif ere and c == ")":
            depth -= 1
            if depth < 0:
                return False
            quantifiable, quantifier = True, False
        elif ere and c == "{":
            close = pattern.find("}", i)
            if close == -1 or not _valid_brace(pattern[i + 1 : close]):
                return False
            quantifiable = quantifier = True
            i = close
        else:
            return False
        i += 1
    return depth == 0


def _grep_parse(cl: CommandLine) -> GrepCall | None:
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
                    if not _unquote(val).isdigit():
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
                    include = _unquote(val)
                elif i + 1 < n:
                    include = args[i + 1]
                    i += 1
                else:
                    return None
            elif name == "regexp":
                e_count += 1
                if sep:
                    pattern = _unquote(val)
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
                if not _unquote(body[1:]).isdigit():
                    return None
            elif i + 1 < n and args[i + 1].isdigit():
                i += 1
            else:
                return None
            expand = True
        elif head == "e":
            e_count += 1
            if len(body) > 1:
                pattern = _unquote(body[1:])
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
        if not _regex_rewritable(pattern, ere):
            return None  # dialect-divergent or exotic regex (backrefs, escapes, PCRE)
        regex = True
    elif not LITERAL_SAFE.match(pattern):
        return None
    for p in paths:
        if _path_blocked(p):
            return None
    targets = _grep_targets(paths, include)
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


def _build_ccx_grep(parsed: GrepCall) -> str | None:
    """Assemble the ``ccx code grep`` rewrite for a parsed grep/rg call, or ``None`` to block.

    ``-i``/``-w`` need the rg engine (ccx ≥ v0.7.0) and ``--regex``/multi-file operands need ccx ≥
    v0.11.0; a brew-first consumer may hold an older binary, so probe before mapping onto them — a
    miss falls back to today's block (an old binary hard-errors on the extra positionals). An
    unresolvable ``ccx`` also blocks.
    """
    if (parsed.ignore_case or parsed.word) and not ccx_supports("code", "grep", flag="--ignore-case"):
        return None
    if (parsed.regex or parsed.paths) and not ccx_supports("code", "grep", flag="--regex"):
        return None
    ccx = ccx_bin()
    if not ccx:
        return None
    parts = [shlex.quote(ccx), "code", "grep", shlex.quote(parsed.pattern)]
    if parsed.ignore_case:
        parts.append("-i")
    if parsed.word:
        parts.append("-w")
    if parsed.regex:
        parts.append("--regex")
    if parsed.glob:
        parts += ["--glob", shlex.quote(parsed.glob)]
    if parsed.expand:
        parts.append(f"--expand={parsed.expand}")
    if parsed.paths:
        # `--` so cobra reads every operand as a file positional — a flag-like name (`--regex`,
        # a `-x` file) after grep's own `--` must not re-parse as a ccx flag and flip the search.
        parts.append("--")
        parts += [shlex.quote(p) for p in parsed.paths]
    return " ".join(parts)


def _note_text(command: str, parsed: GrepCall) -> str:
    disclosures: list[str] = []
    if parsed.regex:
        disclosures.append("the pattern ran as a regex on the rg engine")
    else:
        # Literal-mode disclosures, kept out of the regex branch so the note never claims both.
        if "." in parsed.pattern and not parsed.dropped_fixed:
            disclosures.append(
                "`.` matched literally (grep treats it as an any-char wildcard) — use the Grep tool if you meant regex"
            )
        if parsed.dropped_fixed:
            disclosures.append("`-F` dropped — ccx grep already matches literally")
    if parsed.dropped_l:
        disclosures.append("`-l` dropped — ccx returns the matching lines, not just filenames")
    if parsed.expand:
        if parsed.count_dropped:
            disclosures.append(
                "`-A/-B/-C N` → `--expand=3` — your context-line count was dropped; on the default engine "
                "`--expand=3` inlines the top 3 matches' full source, not N lines of per-match context"
            )
        else:
            disclosures.append(
                f"`-A/-B/-C N` → `--expand={parsed.expand}` — on the default engine `--expand={parsed.expand}` inlines "
                f"the top {parsed.expand} matches' full source, not N lines of per-match context"
            )
    tail = f" {'; '.join(disclosures)}." if disclosures else ""
    kind = "regex" if parsed.regex else "literal"
    return f"Rewrote `{command}` → `ccx code grep`: same {kind} search, token-bounded.{tail}"


def _grep_to(evt: BaseHookEvent) -> str | None:
    parsed = _grep_parse(evt.command_line)
    return _build_ccx_grep(parsed) if parsed is not None else None


def _grep_note(evt: BaseHookEvent) -> str:
    return _note_text(evt.command, _grep_parse(evt.command_line))


def _bounded_file_grep(cmd: Command, *, sink: bool = False) -> bool:
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
      more than :data:`~hooks.common.LARGE_READ_BYTES`; ``-o`` forfeits this stat lane, since its
      per-match filename/line/byte prefixes multiply output past the size bound.

    Conservative throughout: a ``GREP_OPTIONS`` env (which can inject ``-r``/``-o`` the parser never
    sees), any env alongside path operands, an uninspectable ``-f`` pattern file, a flag-supplied empty
    pattern, and an unknown flag on an unpiped grep all return ``False`` — never a wrong allow.
    """
    if "GREP_OPTIONS" in cmd.env_dict:
        return False  # a leading GREP_OPTIONS= injects flags (-r/-o …) that never reach cmd.args
    args = cmd.args
    positionals: list[str] = []
    pattern_from_flag = only_matching = pattern_file = empty_flag_pattern = recursive = False
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
    return sum(Path(p).stat().st_size for p in paths) <= LARGE_READ_BYTES


class GrepFlood(CustomCommandLineCondition):
    """Matches a file-search ``grep`` unless every ``grep`` occurrence is a bounded, unrewritable grep.

    Fires so the hook rewrites it (``_grep_to`` yields the command) or blocks it (``_grep_to`` yields
    ``None``). The ``_unpiped`` guard keeps a line with no unpiped ``grep`` out of scope (a pure
    ``… | grep`` filter). Once in scope it stays silent only when *every* ``grep`` on the line — sink
    greps included, since a sink grep with file operands ignores stdin and searches those files — is a
    bounded explicit-files or data-file grep that ccx can't rewrite. The captain-hook contract turns a
    ``None`` ``to`` under a set ``block`` into an unconditional block, so this per-occurrence allow
    lives here in the condition.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not _unpiped(cl, "grep"):
            return False
        if _grep_to(evt) is not None:
            return True
        return any(
            not _bounded_file_grep(cmd, sink=i > 0 and cl.parts[i - 1][1] == "|")
            for i, (cmd, _) in enumerate(cl.parts)
            if cmd.executable == "grep"
        )


rewrite_command(
    only_if=[Tool("Bash"), GrepFlood()],
    to=_grep_to,
    block=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx code grep <text>` (or mcp__cc-context__ccx_code_grep) / `ccx code search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Simple literal and simple-regex greps auto-rewrite to `ccx code grep`; a grep whose explicit "
        "targets are all data files (`.log`/`.json`/`.yaml`/…) or existing files under the size cap runs "
        "as-is, even inside pipes and `&&`/`;` chains. This one didn't qualify — a tree-wide or directory "
        "search, a recursive flag, an unmappable flag, or an over-cap/missing source-file target. "
        "Escape hatch: pipe input into it (`… | grep`)."
    ),
    note=_grep_note,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, `.` widens, include-only). Path→glob
        # shapes classify each operand against the filesystem, so they live in test_search_guards.py
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


def _rg_operands(cmd: Command) -> list[str] | None:
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
            operands = _rg_operands(cmd)
            if not operands or any(
                op.endswith("/") or Path(op).suffix.lower() not in NON_SOURCE_EXTS for op in operands
            ):
                return False
        return matched


def _fold_expand(current: str, cand: str) -> str:
    """Fold a context count into the running ``--expand`` max — several ``-A/-B/-C`` widen to their superset."""
    return cand if not current else str(max(int(current), int(cand)))


def _rg_parse(cl: CommandLine) -> GrepCall | None:
    """Parse an unpiped ``rg`` into its ccx-rewritable shape, or ``None`` to fall back to block.

    Mirrors :func:`_grep_parse` over ripgrep's grammar with its own DROP table
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
                    if not _unquote(val).isdigit():
                        return None
                    cand = _unquote(val)
                elif i + 1 < n and args[i + 1].isdigit():
                    cand = args[i + 1]
                    i += 1
                else:
                    return None
                expand = _fold_expand(expand, cand)
            elif name == "glob":
                if include is not None:
                    return None
                if sep:
                    include = _unquote(val)
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
                    pattern = _unquote(val)
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
                if not _unquote(body[1:]).isdigit():
                    return None
                cand = _unquote(body[1:])
            elif i + 1 < n and args[i + 1].isdigit():
                cand = args[i + 1]
                i += 1
            else:
                return None
            expand = _fold_expand(expand, cand)
        elif head == "e":
            e_count += 1
            if len(body) > 1:
                pattern = _unquote(body[1:])
            elif i + 1 < n:
                pattern = args[i + 1]
                i += 1
            else:
                return None
        elif head == "g":
            if include is not None:
                return None
            if len(body) > 1:
                include = _unquote(body[1:])
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
        if _path_blocked(p):
            return None
    glob = _grep_glob(paths, include)
    if glob is None:
        return None
    return GrepCall(pattern, glob, expand, ignore_case, word, dropped_l, dropped_fixed, count_dropped=False)


def _rg_to(evt: BaseHookEvent) -> str | None:
    parsed = _rg_parse(evt.command_line)
    return _build_ccx_grep(parsed) if parsed is not None else None


def _rg_note(evt: BaseHookEvent) -> str:
    return _note_text(evt.command, _rg_parse(evt.command_line))


rewrite_command(
    only_if=[Tool("Bash"), UnpipedSearch("rg")],
    skip_if=[RgNonSourceTargets()],
    to=_rg_to,
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
    note=_rg_note,
    tests={
        # Rewrite — disk-independent shapes only (repo-wide, glob-only, context). Path→glob shapes
        # classify each operand against the filesystem, so they live in test_search_guards.py.
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
