"""Search guards: nudge identifier-alternation and natural-language ``rg``/``grep`` to ``ccx``; rewrite simple literal ``grep`` file search to ``ccx code grep``, block the rest."""

from __future__ import annotations

import re
import shlex
from pathlib import Path
from typing import NamedTuple

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
# Short flags naming a numeric context window (`-A/-B/-C N`), all mapped to `--expand`.
CONTEXT_SHORT = frozenset("ABC")
# An `--include` value is a glob, not a pattern, so it skips LITERAL_SAFE — but it must be a
# simple glob (no braces, no spaces) to compose cleanly onto a braced multi-dir root.
INCLUDE_SAFE = re.compile(r"^[\w*?./\[\]-]+$")


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


class UnpipedGrep(CustomCommandLineCondition):
    """Matches a ``grep`` that does not consume piped input.

    Allows the stream-filter idiom (`… | grep`) while still matching grep used for
    file searching, whether standalone, heading a pipe, or in a `&&`/`;` chain.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            cmd.executable == "grep" and (i == 0 or cl.parts[i - 1][1] != "|") for i, (cmd, _) in enumerate(cl.parts)
        )


class GrepCall(NamedTuple):
    pattern: str
    glob: str  # "" → repo-wide (no --glob)
    expand: bool  # -A/-B/-C present
    ignore_case: bool
    word: bool
    dropped_l: bool
    dropped_fixed: bool


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
    expand = ignore_case = word = dropped_l = dropped_fixed = False
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
            elif head in DROP_SHORT:
                dropped_l = dropped_l or head == "l"
                dropped_fixed = dropped_fixed or head == "F"
            else:
                return None  # -v -x -c -o -L -q -E -P -G -z, …
        elif all(ch in DROP_SHORT for ch in body):
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
    if pattern.startswith("-") or not LITERAL_SAFE.match(pattern):
        return None
    for p in paths:
        if p.startswith(("/", "~")) or ".." in p.split("/") or not LITERAL_SAFE.match(p.rstrip("/")):
            return None
    glob = _grep_glob(paths, include)
    if glob is None:
        return None
    return GrepCall(pattern, glob, expand, ignore_case, word, dropped_l, dropped_fixed)


def _grep_to(evt: BaseHookEvent) -> str | None:
    parsed = _grep_parse(evt.command_line)
    if parsed is None:
        return None
    # `-i`/`-w` need the rg engine (ccx ≥ v0.7.0); a brew-first consumer may hold an older
    # binary, so probe before mapping onto them — a miss falls back to today's block.
    if (parsed.ignore_case or parsed.word) and not ccx_supports("code", "grep", flag="--ignore-case"):
        return None
    ccx = ccx_bin()
    if not ccx:
        return None
    parts = [shlex.quote(ccx), "code", "grep", shlex.quote(parsed.pattern)]
    if parsed.ignore_case:
        parts.append("-i")
    if parsed.word:
        parts.append("-w")
    if parsed.glob:
        parts += ["--glob", shlex.quote(parsed.glob)]
    if parsed.expand:
        parts.append("--expand=3")
    return " ".join(parts)


def _grep_note(evt: BaseHookEvent) -> str:
    parsed = _grep_parse(evt.command_line)
    disclosures: list[str] = []
    if "." in parsed.pattern:
        disclosures.append(
            "`.` matched literally (grep treats it as an any-char wildcard) — use the Grep tool if you meant regex"
        )
    if parsed.dropped_l:
        disclosures.append("`-l` dropped — ccx returns the matching lines, not just filenames")
    if parsed.dropped_fixed:
        disclosures.append("`-F` dropped — ccx grep already matches literally")
    if parsed.expand:
        disclosures.append(
            "`-A/-B/-C N` → `--expand=3` — your context-line count was dropped; on the default engine "
            "`--expand=3` inlines the top 3 matches' full source, not N lines of per-match context"
        )
    tail = f" {'; '.join(disclosures)}." if disclosures else ""
    return f"Rewrote `{evt.command}` → `ccx code grep`: same literal search, token-bounded.{tail}"


rewrite_command(
    only_if=[Tool("Bash"), UnpipedGrep()],
    to=_grep_to,
    block=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx code grep <text>` (or mcp__cc-context__ccx_code_grep) / `ccx code search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Simple literal greps auto-rewrite to `ccx code grep`; this one didn't — a regex/metachar pattern, "
        "an unsupported flag, an absolute path, or a pipe/chain. "
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
        # Block — unmappable shapes fall back to the message:
        Input(command="grep -rnC3 foo src/"): Block(),  # value short glued into a bundle
        Input(command="grep -E 'a|b' src/"): Block(pattern="ccx code grep"),  # alternate engine
        Input(command="grep 'foo.*' ."): Block(),  # regex-metachar pattern (LITERAL_SAFE)
        Input(command="grep -q foo src/"): Block(),  # exit-code contract
        Input(command="grep -c foo src/"): Block(),  # count mode
        Input(command="grep -o foo src/"): Block(),  # only-matching mode
        Input(command="grep -rn foo /etc/hosts"): Block(),  # absolute path
        Input(command="grep -e foo -e bar ."): Block(),  # multiple -e
        Input(command="grep -iw foo src/"): Block(),  # MAP chars bundled → block (engine-independent)
        Input(command="grep -f patterns.txt ."): Block(),  # -f pattern-file
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
