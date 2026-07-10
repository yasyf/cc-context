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

from .common import IDENT_ALT, LITERAL_SAFE, ccx_bin, ccx_supports, is_single_command

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

# grep patterns safe to rewrite onto ccx's `--regex` (rg/Rust-regex engine). BRE (grep's default)
# excludes `+ ? | ( ) { }` — literal in BRE but meta in Rust; ERE shares Rust's metachars, so admits
# them. Neither admits backslash, quotes, backticks, or `$` — shell-active chars stay out as defense
# in depth atop the downstream shlex-quoting.
REGEX_SAFE_BRE = re.compile(r"^[\w .:@,=/*^\[\]-]+$")
REGEX_SAFE_ERE = re.compile(r"^[\w .:@,=/*^(){}+?|\[\]-]+$")

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
BOUNDED_BOOL_SHORT = frozenset("iwxvcoqLlrRnHhsIFEGPzaUbT")
BOUNDED_VALUE_SHORT = frozenset("mABCdD")
BOUNDED_PATTERN_SHORT = frozenset("ef")
BOUNDED_BOOL_LONG = frozenset(
    {
        "ignore-case", "no-ignore-case", "word-regexp", "line-regexp", "invert-match", "count",
        "files-with-matches", "files-without-match", "only-matching", "quiet", "silent",
        "no-filename", "with-filename", "line-number", "recursive", "dereference-recursive",
        "extended-regexp", "fixed-strings", "basic-regexp", "perl-regexp", "null", "null-data",
        "text", "byte-offset", "no-messages", "initial-tab", "color", "colour", "binary",
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
    expand = ignore_case = word = dropped_l = dropped_fixed = ere = False
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
                pass  # BRE is grep's default; the flag only confirms it
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
                pass  # BRE is grep's default; the flag only confirms it
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
    if pattern.startswith("-"):
        return None
    regex = False
    if not LITERAL_SAFE.match(pattern):
        dialect = REGEX_SAFE_ERE if ere else REGEX_SAFE_BRE
        if not dialect.match(pattern):
            return None  # exotic regex (backrefs, escapes, PCRE, dialect-divergent metachars)
        regex = True
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
    parts += [shlex.quote(p) for p in parsed.paths]
    return " ".join(parts)


def _note_text(command: str, parsed: GrepCall) -> str:
    disclosures: list[str] = []
    if parsed.regex:
        disclosures.append("the pattern ran as a regex on the rg engine")
    elif "." in parsed.pattern:
        disclosures.append(
            "`.` matched literally (grep treats it as an any-char wildcard) — use the Grep tool if you meant regex"
        )
    if parsed.dropped_l:
        disclosures.append("`-l` dropped — ccx returns the matching lines, not just filenames")
    if parsed.dropped_fixed:
        disclosures.append("`-F` dropped — ccx grep already matches literally")
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


def _bounded_file_grep(cl: CommandLine) -> bool:
    """Report whether a ``grep`` is a bounded search over explicit existing files.

    A single command whose every flag lexes against the known-arity grep tables and whose every path
    operand stats as an existing regular file (absolute paths included). Such a grep floods nothing —
    it reads a fixed, named set of files — so when it is not ccx-rewritable it earns a pass-through.
    The lexer is conservative: an unknown flag or a bundle with a value-taking char returns ``False``
    (the grep then enters the hook), never a wrong allow.
    """
    if not is_single_command(cl):
        return False
    args = cl.primary.args
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
                return False
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
            return False
        i += 1
    paths = positionals if pattern_from_flag else positionals[1:]
    if not paths:
        return False
    return all(Path(p).is_file() for p in paths)


class GrepFlood(CustomCommandLineCondition):
    """Matches an unpiped file-search ``grep`` unless it is a bounded, unrewritable, explicit-files grep.

    Fires so the hook rewrites it (``_grep_to`` yields the command) or blocks it (``_grep_to`` yields
    ``None``). It stays silent only on a bounded explicit-existing-files grep that ccx can't rewrite,
    letting that grep run — the captain-hook contract turns a ``None`` ``to`` under a set ``block``
    into an unconditional block, so this per-case allow lives here in the condition.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not _unpiped(cl, "grep"):
            return False
        if _grep_to(evt) is not None:
            return True
        return not _bounded_file_grep(cl)


rewrite_command(
    only_if=[Tool("Bash"), GrepFlood()],
    to=_grep_to,
    block=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx code grep <text>` (or mcp__cc-context__ccx_code_grep) / `ccx code search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Simple literal and simple-regex greps auto-rewrite to `ccx code grep`, and a grep over explicit "
        "existing files passes through; this one didn't — an exotic regex (backrefs, escapes, PCRE), an "
        "unmappable flag, or a tree-wide unmappable search. "
        "Escape hatch: pipe it (`… | grep`)."
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
        # Regex rewrites — a BRE/ERE-safe pattern now maps onto `ccx code grep --regex` (disk-independent
        # `.` widening; the --regex probe hits the real plugin/bin/ccx, which supports it since v0.11.0):
        Input(command="grep 'foo.*' ."): Rewrite(pattern="--regex"),  # BRE-safe metachars → regex on rg engine
        Input(command="grep -E 'a|b' ."): Rewrite(pattern="--regex"),  # ERE alternation → regex on rg engine
        # Block — unmappable shapes fall back to the message:
        Input(command="grep -rnC3 foo src/"): Block(),  # value short glued into a bundle
        Input(command="grep -P 'x(?=y)' ."): Block(),  # PCRE (-P) never maps; `.` is a dir, not a bounded file
        Input(command="grep -q foo src/"): Block(),  # exit-code contract, tree-wide
        Input(command="grep -c foo src/"): Block(),  # count mode, tree-wide
        Input(command="grep -o foo src/"): Block(),  # only-matching mode, tree-wide
        Input(command="grep -e foo -e bar ."): Block(),  # multiple -e over a dir
        Input(command="grep -iw foo src/"): Block(),  # MAP chars bundled → block (engine-independent)
        Input(command="grep -f patterns.txt ."): Block(),  # -f pattern-file over a dir
        # Allow — an unrewritable grep over an explicit existing file is bounded, so the condition never
        # fires (/etc/hosts is a regular file on every CI OS: macOS + Linux):
        Input(command="grep -rn foo /etc/hosts"): Allow(),  # absolute path, but a bounded existing file
        # Existing block neighbors — a pipe head or `&&` chain is not a single command:
        Input(command="grep foo file.py | wc -l"): Block(),
        Input(command="grep foo a && echo done"): Block(),
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
