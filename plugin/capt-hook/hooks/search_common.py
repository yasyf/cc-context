"""Shared primitives for the ``grep``/``rg`` search guards, plus the nudges steering identifier-alternation and natural-language ``rg``/``grep`` to ``ccx``."""

from __future__ import annotations

import re
import shlex
import subprocess
import sys
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

from .common import IDENT_ALT, ccx_bin, ccx_supports

if TYPE_CHECKING:
    from collections.abc import Callable

    from cc_transcript.command import Command

# The cc-transcript steer: sent when a gated grep/rg's operands are ALL session transcripts.
TRANSCRIPT_STEER = (
    "BLOCKED: Session transcripts: use cc-transcript (list / grep / show), never raw grep or "
    "ccx code grep — it reads the .jsonl by session/turn/tool without flooding context."
)

# Appended to the engine's block message on a line mixing a transcript operand with an ordinary flood.
TRANSCRIPT_APPEND = (
    "Also — the ~/.claude/projects operand is a session transcript: use cc-transcript "
    "(list / grep / show), not raw grep or ccx code grep."
)

# The dep-reader steer: sent when a grep/rg targets a VCS-store segment or a git-ignored operand.
DEP_STEER = (
    "BLOCKED: Dependency or VCS-internal source (`.venv`, `node_modules`, `.git`, vendored packages) "
    "floods context. Spawn the `cc-context:dep-reader` agent with the package and your question — it "
    "returns cited conclusions, never the source. Inline: `ccx repo locate <pkg>` (CLI-only), then "
    "`ccx code grep`/`outline`/`read` with the printed path."
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

# Short flags naming a numeric context window (`-A/-B/-C N`).
CONTEXT_SHORT = frozenset("ABC")

# An `--include` value is a glob, not a pattern — but it must be a simple glob (no braces,
# no spaces) to compose cleanly onto a braced multi-dir root.
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
    "Searching for several identifiers? `ccx code symbol <name>` resolves a definition plus its callers "
    "in one call; `ccx code grep 'a|b' --regex` runs them as one search and groups hits compactly.",
    only_if=[Tool("Bash"), RgIdentAlternation()],
    events=Event.PreToolUse,
    tests={
        Input(command="rg 'fooBar|bazQux' src/"): Warn(pattern="ccx code symbol"),
        Input(command="rg 'Foo|Bar|Baz' ."): Warn(pattern="--regex"),
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
    "runs a semantic search that finds code by intent, not exact text.",
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


class GrepCall(NamedTuple):
    pattern: str
    glob: str  # "" → repo-wide (no --glob)
    expand: str  # old-binary fallback: "" or the `--expand=<value>` count
    context_args: tuple[tuple[str, str], ...]  # native ccx context flags with exact counts
    ignore_case: bool
    word: bool
    dropped_l: bool  # input requested file-list output; older ccx binaries still drop it
    dropped_fixed: bool
    count_dropped: bool  # True when `--expand` holds a fixed placeholder, so the note flags the `-A/-B/-C N` count as lost
    regex: bool = False  # pattern rewrites to `--regex` (ran as a regex on the rg engine)
    paths: tuple[str, ...] = ()  # explicit multi-file operands carried as `ccx code grep` positionals


def is_transcript_path(p: str) -> bool:
    """Whether a grep/rg path operand targets a Claude session transcript — under ``.claude/projects/``.

    Matches the consecutive path segments ``.claude`` then ``projects`` (split on ``/``), so the
    projects dir itself or any transcript nested under it is caught while a lookalike substring
    (``docs/x.claude/projects-notes.md``) is not. A purely textual check — a ``$VAR`` hiding such a
    path is not caught (accepted under fail-open).
    """
    segs = p.split("/")
    return any(segs[i] == ".claude" and segs[i + 1] == "projects" for i in range(len(segs) - 1))


def has_dependency_segment(p: str) -> bool:
    """Whether a path has an unambiguous dependency-source or VCS-store ``/``-segment — names
    that mean dep source wherever they appear, in-repo or out. Exact segment membership
    (``a.venv/b`` and ``.github/workflows`` never match), purely textual, no stat."""
    return any(
        seg in (".git", ".jj", ".hg", ".svn", ".venv", "node_modules", "site-packages", "dist-packages")
        for seg in p.split("/")
    )


def any_git_ignored(ops: list[str], *, cwd: Path | None) -> bool:
    """Whether any directory operand is git-ignored, per the cwd repo's own rules. A file
    operand is bounded by itself and a ``:``-led one is pathspec magic — both skipped; ``~``
    expands so a home-anchored path still resolves. One batched ``git check-ignore`` answers
    the common case; its 128 (one bad operand poisons a batch) falls back per-operand so a
    bad path can't mask an ignored one. Git absent or an untrusted ``cwd`` → ``False``."""
    if cwd is None:
        return False
    expanded = (str(Path(p).expanduser()) if p.startswith("~") else p for p in ops)
    # `.`/`..` is the search root, never dep source — and the one shape on every tree grep.
    candidates = (p for p in expanded if p.rstrip("/") not in (".", "..") and not p.startswith(":"))
    dirs = [p for p in candidates if resolved_is_dir(p, cwd)]
    if not dirs:
        return False

    def check(paths: list[str]) -> int:
        try:
            return subprocess.run(["git", "check-ignore", "--", *paths], capture_output=True, cwd=cwd).returncode
        except OSError:
            return 128

    rc = check(dirs)
    if rc == 128 and len(dirs) > 1:
        return any(check([d]) == 0 for d in dirs)
    return rc == 0


def forfeits_operand(p: str) -> bool:
    """Whether a path operand carries a shell expansion (leading ``~``, ``$``) or a glob metachar
    (``*``, ``?``, ``[``) no faithful ``ccx code grep`` rewrite can preserve.

    ``shlex.quote`` would freeze a ``~``/``$`` into a literal ccx never expands, and a ``[``/``*``/``?``
    reaches ccx's glob engine as a character class or wildcard the operand never meant — so the
    occurrence forfeits the rewrite and runs raw, never a block and never a lossy emission. Callers pass
    path operands only; a ``$`` anchor inside the pattern stays rewritable.
    """
    return p.startswith("~") or any(c in p for c in "$*?[")


def forfeits_count(args: tuple[str, ...]) -> bool:
    """Whether any bare numeric token exceeds Python's int-string conversion limit.

    A pathological ``-A <5000-digit>`` context count that :func:`~hooks.rg_guards.fold_expand`'s
    ``int()`` refuses; the occurrence forfeits the rewrite and runs raw rather than crash on the
    conversion. ``sys.get_int_max_str_digits() == 0`` disables the limit, so nothing overflows.
    """
    return (limit := sys.get_int_max_str_digits()) != 0 and any(a.isdigit() and len(a) > limit for a in args)


def has_command_substitution(raw: str) -> bool:
    """Whether a command's raw text carries a ``$(...)`` or backtick substitution the parser drops.

    tree-sitter folds a standalone ``$(...)``/backtick operand out of the argv, so ``grep foo
    $(printf /p)`` parses to just the pattern — a rewrite would silently search repo-wide instead of
    the produced path. The raw text still shows the construct, so a rewrite is forfeited and the real
    shell runs the command.
    """
    return "$(" in raw or "`" in raw


def path_operands_raw(args: tuple[str, ...]) -> list[str]:
    """Every positional-shaped arg — non-flag tokens plus everything after a bare ``--``.

    Tolerant of unknown flags, so it feeds tree-shape detection when the arity walk
    (:func:`~hooks.grep_guards.grep_operands` / :func:`~hooks.rg_guards.rg_operands`) returns ``None``
    and cannot separate the pattern from the paths. It over-includes the pattern token; a dir-operand
    scan over it is still sound because a pattern is rarely a directory.
    """
    out: list[str] = []
    seen_double_dash = False
    for a in args:
        if seen_double_dash or a == "-" or not a.startswith("-"):
            out.append(a)
        elif a == "--":
            seen_double_dash = True
    return out


def resolve_operand(p: str, cwd: Path | None) -> Path | None:
    """Resolve a grep/rg path operand against ``cwd`` for a stat, or ``None`` when unresolvable.

    An absolute operand resolves regardless of ``cwd``; a relative one needs a ``cwd``. With none, a
    relative operand is unresolvable and the one ``is_dir`` probe fails open (not a directory).
    """
    path = Path(p)
    if path.is_absolute():
        return path
    return cwd / path if cwd is not None else None


def resolved_is_dir(p: str, cwd: Path | None) -> bool:
    """Whether ``p`` is a directory operand: ``.``/``..`` (always the cwd/parent) or a path resolving
    against ``cwd`` to an existing directory. An unstattable operand (``$VAR``, missing, no trusted
    ``cwd``) is not a directory — the fail-open direction."""
    if p.rstrip("/") in (".", ".."):
        return True
    return (path := resolve_operand(p, cwd)) is not None and path.is_dir()


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
    their operands too. An occurrence whose flags the arity walk can't map (``operands`` returns ``None``)
    falls back to its raw path-like tokens, so an unparseable flag never blinds the transcript steer.
    """
    command_line = cl or evt.cmd.line
    if not command_line:
        return default
    ops: list[str] = []
    for occ in command_line.occurrences:
        cmd = occ.command.unwrapped
        if cmd.executable != exe:
            continue
        parsed = operands(cmd)
        ops.extend(path_operands_raw(cmd.args) if parsed is None else parsed)
    transcript = [p for p in ops if is_transcript_path(p)]
    if not transcript:
        return default
    if len(transcript) == len(ops):
        return TRANSCRIPT_STEER
    return f"{default}\n{TRANSCRIPT_APPEND}"


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


def grep_glob(paths: list[str], include: str | None, *, cwd: Path | None) -> str | None:
    """Build the ``--glob`` body for a tree-shaped search's path args: ``""`` for repo-wide, ``None`` to block.

    A ``.``/``./`` among the paths widens the search to the whole repo — every sibling path is a subset,
    so no ``--glob`` narrows it (an ``--include`` still applies repo-wide as a bare ``*.go``). Otherwise
    each operand is a directory (:func:`resolved_is_dir` → ``dir/**``, braced when several: ``{a,b}/**``)
    or a non-directory (a lone file passes through as-is; an unstattable ``$VAR``/missing path lands here
    too under fail-open), and an ``--include`` glob composes onto the dir roots (``dir/**/*.go``). Mixed
    file+dir paths, several non-directory operands, and an include over explicit files have no faithful
    single-glob form → block. An out-of-repo operand (absolute, ``~``, or a ``..`` segment) has no
    repo-relative glob either — it forfeits the rewrite rather than emit a glob ccx would 0-match.
    """
    if any(p.startswith(("/", "~")) or ".." in p.rstrip("/").split("/") for p in paths):
        return None
    if any(p in (".", "./") for p in paths):
        if include is None:
            return ""
        return include if INCLUDE_SAFE.match(include) else None
    dirs: list[str] = []
    files: list[str] = []
    for p in paths:
        (dirs if resolved_is_dir(p, cwd) else files).append(p.rstrip("/"))
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


def build_ccx_grep(parsed: GrepCall) -> str | None:
    """Assemble the ``ccx code grep`` rewrite for a parsed grep/rg call, or ``None`` to block.

    ``-i``/``-w`` need the rg engine (ccx ≥ v0.7.0), ``--regex``/multi-file operands need ccx ≥
    v0.11.0, and ``-l`` needs the file-list output mode. A brew-first consumer may hold an older
    binary, so probe before mapping onto them. An unresolvable ``ccx`` also blocks.
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
    files_list = parsed.dropped_l and ccx_supports("code", "grep", flag="--files-with-matches")
    if files_list:
        parts.append("-l")
    if parsed.glob:
        parts += ["--glob", shlex.quote(parsed.glob)]
    if not files_list:
        # ccx hard-errors on -l with -A/-B/-C, and native grep -l ignores context — suppress it.
        parts += [f"{flag}={count}" for flag, count in parsed.context_args]
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
    if parsed.dropped_l and not ccx_supports("code", "grep", flag="--files-with-matches"):
        disclosures.append("`-l` dropped — ccx returns the matching lines, not just filenames")
    if parsed.expand:
        if parsed.count_dropped:
            disclosures.append(
                "`-A/-B/-C N` → `--expand=3` — your context-line count was dropped; "
                "`--expand=3` adds 3 context lines around each hit"
            )
        else:
            disclosures.append(
                f"`-A/-B/-C N` → `--expand={parsed.expand}` — `--expand={parsed.expand}` adds "
                f"{parsed.expand} context lines around each hit"
            )
    tail = f" {'; '.join(disclosures)}." if disclosures else ""
    kind = "regex" if parsed.regex else "literal"
    return f"Rewrote `{command}` → `ccx code grep`: same {kind} search, token-bounded.{tail}"
