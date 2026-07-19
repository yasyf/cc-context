"""Shared primitives for the ``grep``/``rg`` search guards, plus the nudges steering identifier-alternation and natural-language ``rg``/``grep`` to ``ccx``."""

from __future__ import annotations

import re
import shlex
import subprocess
from pathlib import Path
from typing import TYPE_CHECKING, NamedTuple

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Tool,
    Warn,
    nudge,
)

from .common import IDENT_ALT, LITERAL_SAFE, ccx_bin, ccx_supports

if TYPE_CHECKING:
    from collections.abc import Callable

    from cc_transcript.command import Command

# The cc-transcript steer: sent when a gated grep/rg's operands are ALL session transcripts.
TRANSCRIPT_STEER = (
    "BLOCKED: Session transcripts: use cc-transcript (list / grep / show), never raw grep or "
    "ccx code grep (mcp__cc-context__ccx_code_grep) — it reads the .jsonl by session/turn/tool "
    "without flooding context."
)

# Appended to the engine's block message on a line mixing a transcript operand with an ordinary flood.
TRANSCRIPT_APPEND = (
    "Also — the ~/.claude/projects operand is a session transcript: use cc-transcript "
    "(list / grep / show), not raw grep or ccx code grep (mcp__cc-context__ccx_code_grep)."
)


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

# Short flags naming a numeric context window (`-A/-B/-C N`), all mapped to `--expand`.
CONTEXT_SHORT = frozenset("ABC")

# An `--include` value is a glob, not a pattern, so it skips LITERAL_SAFE — but it must be a
# simple glob (no braces, no spaces) to compose cleanly onto a braced multi-dir root.
INCLUDE_SAFE = re.compile(r"^[\w*?./\[\]-]+$")

# Data-file suffixes that make a raw `rg` a sanctioned non-source search (`rg ERROR app.log`),
# exempt from the rg gate. A purely textual `Path.suffix` check — no stat.
NON_SOURCE_EXTS = frozenset(
    {".log", ".txt", ".csv", ".tsv", ".json", ".jsonl", ".ndjson", ".yaml", ".yml", ".toml", ".ini"}
)


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


def unpiped(cl: CommandLine, exe: str) -> bool:
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
        return unpiped(cl, self.exe)


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


def is_transcript_path(p: str) -> bool:
    """Whether a grep/rg path operand targets a Claude session transcript — under ``.claude/projects/``.

    Covers the projects dir itself and any ``*.jsonl`` transcript inside it; a ``.jsonl`` elsewhere is
    ordinary data, not a transcript. Load-bearing for the ``~``/``$`` decline: a transcript path
    carrying ``~`` (``~/.claude/projects/…``) must stay *blocked* (a raw recursive grep there floods
    context) rather than fall through to Allow like an ordinary ``~``-path.
    """
    return ".claude/projects" in p


def has_command_substitution(raw: str) -> bool:
    """Whether a command's raw text carries a ``$(...)`` or backtick substitution the parser drops.

    tree-sitter folds a standalone ``$(...)``/backtick operand out of the argv, so ``grep foo
    $(printf /p)`` parses to just the pattern — a rewrite would silently search repo-wide instead of
    the produced path. The raw text still shows the construct, so a guard declines on it and lets the
    real shell run the command.
    """
    return "$(" in raw or "`" in raw


def search_block(
    evt: BaseHookEvent,
    exe: str,
    operands: Callable[[Command], list[str] | None],
    default: str,
    *,
    cl: CommandLine | None = None,
) -> str:
    """The block message for a gated grep/rg, tuned per the line's transcript operands.

    Gathers every path operand across the ``exe`` commands on the line. No transcript operand keeps the
    engine's own ``default``; when *every* operand is a session transcript (``~/.claude/projects/…``) the
    whole steer is :data:`TRANSCRIPT_STEER`; a *mixed* line — a transcript operand alongside an ordinary
    flood — keeps ``default`` and appends one :data:`TRANSCRIPT_APPEND` line so neither steer is lost.
    Every occurrence is inspected through its unwrapped command, so wrapper-prefixed searches contribute
    their operands too — the same scope the guard's wrapper-transparent condition uses.
    """
    command_line = cl or evt.command_line
    if command_line is None:
        return default
    ops = [
        p
        for occ in command_line.occurrences
        if (cmd := occ.command.unwrapped).executable == exe
        for p in (operands(cmd) or ())
    ]
    transcript = [p for p in ops if is_transcript_path(p)]
    if not transcript:
        return default
    if len(transcript) == len(ops):
        return TRANSCRIPT_STEER
    return f"{default}\n{TRANSCRIPT_APPEND}"


def classify_path(p: str) -> bool | None:
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


def brace(dirs: list[str]) -> str:
    return dirs[0] if len(dirs) == 1 else "{" + ",".join(dirs) + "}"


def unquote(s: str) -> str:
    """Strip one matching pair of surrounding quotes.

    The command parser removes quotes wrapping a whole token but keeps them on a *glued*
    flag value — ``--include='*.go'`` arrives with its quotes intact — so a glued value is
    unquoted before it is used as a glob or pattern.
    """
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def grep_glob(paths: list[str], include: str | None) -> str | None:
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
        kind = classify_path(p)
        if kind is None:
            return None
        (dirs if kind else files).append(p.rstrip("/"))
    if include is not None:
        if not INCLUDE_SAFE.match(include) or files:
            return None
        return include if not dirs else f"{brace(dirs)}/**/{include}"
    if dirs and files:
        return None
    if files:
        return files[0] if len(files) == 1 else None
    if dirs:
        return f"{brace(dirs)}/**"
    return ""


def git_ignored(p: str) -> bool:
    """Best-effort ``git check-ignore``: ``True`` only when git reports ``p`` ignored.

    Runs from the hook's cwd — where the search would run. Anything but a clean ignore hit (a
    tracked path, git absent, not a repo) returns ``False`` so the rewrite still proceeds.
    """
    try:
        proc = subprocess.run(["git", "check-ignore", "-q", p], capture_output=True)
    except OSError:
        return False
    return proc.returncode == 0


def path_blocked(p: str) -> bool:
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
    return git_ignored(p)


def build_ccx_grep(parsed: GrepCall) -> str | None:
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


def note_text(command: str, parsed: GrepCall) -> str:
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
