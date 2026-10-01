"""Shared ``grep``/``rg`` binding, policy screens, and the ``ccx code grep`` emitter, plus the search nudges."""

from __future__ import annotations

import re
import shlex
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, NamedTuple

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Option,
    Tool,
    Warn,
    nudge,
)

from .common import IDENT_ALT, ccx_bin, ccx_supports, rewrote_note

if TYPE_CHECKING:
    from collections.abc import Callable

    from captain_hook import Arguments, Call, CommandSchema
    from cc_transcript.command import Word

TRANSCRIPTS = "/".join(("~", ".claude", "projects"))

TRANSCRIPT_STEER = (
    "Session transcripts are searched with `cc-transcript`, never raw `grep` or `rg`. "
    "Run `cc-transcript grep '<pattern>' <transcript>`."
)

DEP_STEER = (
    "Dependency and VCS-internal source floods context. "
    "Spawn the `cc-context:dep-reader` agent with the package and your question."
)

SEARCH_EXECUTABLES = ("rg", "grep")

NL_PHRASE = re.compile(r"^[a-z]+(?: [a-z]+)+$")

INCLUDE_SAFE = re.compile(r"^[\w*?./\[\]-]+$")

DEPENDENCY_SEGMENTS = frozenset({".git", ".jj", ".hg", ".svn", ".venv", "node_modules", "site-packages", "dist-packages"})

CONTEXT_FLAGS = {
    "after": "-A",
    "before": "-B",
    "context": "-C",
    "after_long": "--after-context",
    "before_long": "--before-context",
    "context_long": "--context",
}

CONTEXT_OPTIONS = tuple(Option(name, (flag,)) for name, flag in CONTEXT_FLAGS.items())


class RgIdentAlternation(CustomCommandLineCondition):
    """Matches an ``rg``/``grep`` whose pattern is an identifier alternation (``fooBar|bazQux``)."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(cl.q.runs(exe) for exe in SEARCH_EXECUTABLES) and any(IDENT_ALT.search(a) for a in cl.primary.args)


nudge(
    "Searching for several identifiers? Run `ccx code symbol <name>` to get each definition with its callers.",
    only_if=[Tool("Bash"), RgIdentAlternation()],
    events=Event.PreToolUse,
    tests={
        Input(command="rg 'fooBar|bazQux' src/"): Warn(pattern="ccx code symbol"),
        Input(command="rg 'Foo|Bar|Baz' ."): Warn(pattern="ccx code symbol"),
        Input(command="grep 'fooBar|bazQux' src/"): Warn(pattern="ccx code symbol"),
        Input(command="rg TODO"): Allow(),
        Input(command="grep TODO ."): Allow(),
        Input(command="rg 'just one term' src/"): Allow(),
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
    """Matches an ``rg``/``grep`` whose pattern is two or more lowercase words (:data:`NL_PHRASE`)."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(cl.q.runs(exe) for exe in SEARCH_EXECUTABLES) and any(NL_PHRASE.match(a) for a in cl.primary.args)


nudge(
    'Searching for a concept, not a literal string? Run `ccx code search "<question>"`.',
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
        Input(
            command="ccx exec 'async def main(): return await sh(\"rg \\\"parse the config file\\\"\")\n"
            "asyncio.run(main())'"
        ): Allow(),
    },
)


class GrepCall(NamedTuple):
    pattern: str
    glob: str
    expand: str
    context_args: tuple[tuple[str, str], ...]
    ignore_case: bool
    word: bool
    dropped_l: bool
    fixed: bool
    count_dropped: bool
    regex: bool = False
    paths: tuple[str, ...] = ()


class Decline(NamedTuple):
    reason: str


def word_text(word: Word, value: object = None) -> str:
    """A bound word as text: its dequoted value, or its source spelling when an expansion taints it."""
    if isinstance(value, str):
        return value
    return word.value if word.value is not None else word.raw


def bound_texts(arguments: Arguments, name: str) -> list[str]:
    return [
        word_text(word, value)
        for word, value in zip(arguments.words.get(name, ()), arguments.values.get(name, ()), strict=True)
    ]


def unparsed(schema: CommandSchema, arguments: Arguments) -> bool:
    """Whether binding stopped at an option the schema does not declare."""
    flags = {flag for option in schema.options for flag in option.flags}
    return bool(arguments.unread) and arguments.unread[0].value not in flags


def glued_value(call: Call, flags: frozenset[str]) -> bool:
    """Whether a value-less long flag in ``flags`` carries an ``=value`` the tool itself rejects."""
    for word in call.command.words[1:]:
        if word.value == "--":
            return False
        if word.value is not None and word.value.partition("=")[0] in flags and "=" in word.value:
            return True
    return False


def loose_operands(call: Call) -> list[str]:
    """Every non-option word plus everything after a bare ``--``; over-includes the pattern."""
    out: list[str] = []
    after_separator = False
    for word in call.command.words[1:]:
        text = word_text(word)
        if after_separator or text == "-" or not text.startswith("-"):
            out.append(text)
        elif text == "--":
            after_separator = True
    return out


def context_flags(arguments: Arguments) -> list[tuple[str, str]]:
    """The ``-A/-B/-C`` family bindings as ``(flag, count)`` pairs in source order."""
    bound = [
        (word.span or (0, 0), flag, text)
        for name, flag in CONTEXT_FLAGS.items()
        for word, text in zip(arguments.words.get(name, ()), bound_texts(arguments, name), strict=True)
    ]
    return [(flag, text) for _, flag, text in sorted(bound)]


def unpiped(evt: BaseHookEvent, program: str) -> bool:
    return any(call.occurrence.prev_op != "|" for call in evt.cmd.calls(program))


@dataclass(frozen=True)
class UnpipedSearch(CustomCommandLineCondition):
    """Matches a line carrying a ``program`` call that does not read a pipe."""

    program: str

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return unpiped(evt, self.program)


@dataclass(frozen=True)
class SearchTargets(CustomCommandLineCondition):
    """Matches a line with an unpiped ``program`` call where some ``program`` call's path operands satisfy ``targets``.

    Operands come from the strict binding, or :func:`loose_operands` when it stops at an unknown flag.
    """

    program: str
    operands: Callable[[Call], list[str] | None]
    targets: Callable[[list[str], Path | None], bool]

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        calls = evt.cmd.calls(self.program)
        return unpiped(evt, self.program) and any(
            self.targets(loose_operands(call) if (ops := self.operands(call)) is None else ops, call.cwd)
            for call in calls
        )


def is_transcript_path(p: str) -> bool:
    """Whether a path has the consecutive segments ``.claude`` then ``projects``."""
    segs = p.split("/")
    return any(segs[i] == ".claude" and segs[i + 1] == "projects" for i in range(len(segs) - 1))


def has_dependency_segment(p: str) -> bool:
    return any(seg in DEPENDENCY_SEGMENTS for seg in p.split("/"))


def targets_transcript(ops: list[str], cwd: Path | None) -> bool:
    return any(is_transcript_path(p) for p in ops)


def targets_dependency(ops: list[str], cwd: Path | None) -> bool:
    return any(has_dependency_segment(p) for p in ops) or any_git_ignored(ops, cwd=cwd)


def git_check_ignore(paths: list[str], cwd: Path) -> int:
    return subprocess.run(["git", "check-ignore", "--", *paths], capture_output=True, cwd=cwd).returncode


def any_git_ignored(ops: list[str], *, cwd: Path | None) -> bool:
    """Whether a directory operand is git-ignored by the ``cwd`` repo; files, ``.``/``..``, and ``:`` pathspecs are skipped.

    A batch exit of 128 (one bad operand) retries each directory alone.
    """
    if cwd is None:
        return False
    expanded = (str(Path(p).expanduser()) if p.startswith("~") else p for p in ops)
    candidates = (p for p in expanded if p.rstrip("/") not in (".", "..") and not p.startswith(":"))
    dirs = [p for p in candidates if resolved_is_dir(p, cwd)]
    if not dirs:
        return False
    rc = git_check_ignore(dirs, cwd)
    if rc == 128 and len(dirs) > 1:
        return any(git_check_ignore([d], cwd) == 0 for d in dirs)
    return rc == 0


def forfeits_operand(p: str) -> bool:
    """Whether a path operand carries a ``~``/``$`` expansion or a glob metachar no rewrite preserves."""
    return p.startswith("~") or any(c in p for c in "$*?[")


def forfeits_count(args: list[str] | tuple[str, ...]) -> bool:
    """Whether a numeric token exceeds Python's int-string conversion limit."""
    return (limit := sys.get_int_max_str_digits()) != 0 and any(a.isdigit() and len(a) > limit for a in args)


def forfeits_substitution(call: Call) -> bool:
    """Whether a word carries ``$(...)`` or backtick text; a substituted operand would leave the rewrite unscoped."""
    return call.substituted or any("$(" in word.raw or "`" in word.raw for word in call.command.words)


def resolve_operand(p: str, cwd: Path | None) -> Path | None:
    path = Path(p)
    if path.is_absolute():
        return path
    return cwd / path if cwd is not None else None


def resolved_is_dir(p: str, cwd: Path | None) -> bool:
    if p.rstrip("/") in (".", ".."):
        return True
    return (path := resolve_operand(p, cwd)) is not None and path.is_dir()


def brace(dirs: list[str]) -> str:
    return dirs[0] if len(dirs) == 1 else "{" + ",".join(dirs) + "}"


def grep_glob(paths: list[str], include: str | None, *, cwd: Path | None) -> str | None:
    """The ``--glob`` body for a search's path operands: ``""`` repo-wide, ``None`` when no single in-repo glob exists.

    ``.`` widens to the whole repo, a directory becomes ``dir/**`` (braced when several), a lone file
    passes through, and ``include`` composes onto directory roots.
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
    """The ``ccx code grep`` command for ``parsed``, or ``None`` when the local ``ccx`` cannot express it."""
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
        parts += [f"{flag}={count}" for flag, count in parsed.context_args]
        if parsed.expand:
            parts.append(f"--expand={parsed.expand}")
    if parsed.paths:
        parts.append("--")
        parts += [shlex.quote(p) for p in parsed.paths]
    return " ".join(parts)


def search_note(parsed: GrepCall) -> str:
    gains = ["same regex search on the rg engine, token-bounded" if parsed.regex else "same literal search, token-bounded"]
    if not parsed.regex and "." in parsed.pattern and not parsed.fixed:
        gains.append("`.` matches literally")
    if parsed.dropped_l and not ccx_supports("code", "grep", flag="--files-with-matches"):
        gains.append("matching lines, not a file list")
    if parsed.expand:
        gains.append(
            "3 context lines, not your `-A/-B/-C` count" if parsed.count_dropped else "context folded into one `--expand`"
        )
    return rewrote_note("ccx code grep", "; ".join(gains))
