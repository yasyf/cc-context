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

from captain_hook.util.scratch import is_scratch_path
from captain_hook.util.vcs import in_vcs_repo

from .common import IDENT_ALT, ccx_bin, ccx_supports, rewrote_text

if TYPE_CHECKING:
    from collections.abc import Callable

    from captain_hook import Arguments, Call, CommandSchema
    from cc_transcript.command import Word

TRANSCRIPTS = "/".join(("~", ".claude", "projects"))

TRANSCRIPT_SUFFIX = ".".join(("", "jsonl"))

EXAMPLE_SESSION = f"{TRANSCRIPTS}/-Users-me-repo/900424b6-7393-480c-a26a-f1bd21da6e57"

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

NO_TRANSCRIPTS = frozenset({"tool-results", "memory"})

SESSION_ID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

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


def loose_operands(call: Call, schema: CommandSchema) -> list[str]:
    """Every non-option word plus everything after a bare ``--``, skipping a declared value option's value.

    A positive ``glob`` value stays, since it selects paths; a negated one is dropped. Over-includes the
    pattern and an undeclared option's value.
    """
    out: list[str] = []
    after_separator = False
    pending: Option | None = None
    for word in call.command.words[1:]:
        text = word_text(word)
        if pending is not None:
            if pending.name == "glob" and not text.startswith("!"):
                out.append(text)
            pending = None
        elif after_separator or text == "-" or not text.startswith("-"):
            out.append(text)
        elif text == "--":
            after_separator = True
        else:
            pending = awaited_option(text, schema)
    return out


def awaited_option(flag: str, schema: CommandSchema) -> Option | None:
    """The declared value option an option word leaves its value to: a long flag, or a cluster ending in one."""
    takes_value = {alias: option for option in schema.options if option.type is not bool for alias in option.flags}
    if flag.startswith("--"):
        return takes_value.get(flag)
    shorts = [f"-{letter}" for letter in flag[1:]]
    index = next((i for i, short in enumerate(shorts) if short in takes_value), None)
    return takes_value[shorts[-1]] if index == len(shorts) - 1 else None


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
    """Matches a line with an unpiped ``schema`` program call where some such call's path operands satisfy ``targets``.

    Operands come from the strict binding, or :func:`loose_operands` when it stops at an unknown flag.
    """

    schema: CommandSchema
    operands: Callable[[Call], list[str] | None]
    targets: Callable[[list[str], Path | None], bool]

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        program = self.schema.program
        return unpiped(evt, program) and any(
            self.targets(loose_operands(call, self.schema) if (ops := self.operands(call)) is None else ops, call.cwd)
            for call in evt.cmd.calls(program)
        )


def is_transcript_path(p: str) -> bool:
    """Whether a path is a session transcript under the projects store, or a directory or glob reaching one.

    The store, a project, a session, and its ``subagents`` dir hold transcripts; a ``jsonl`` file sits
    at the session or agent level. A session's ``tool-results`` and a project's ``memory`` hold none.
    """
    segs = [seg for seg in p.split("/") if seg]
    pairs = zip(segs, segs[1:])
    if (start := next((i + 2 for i, pair in enumerate(pairs) if pair == (".claude", "projects")), None)) is None:
        return False
    match segs[start:]:
        case [] | [_]:
            return True
        case [_, *under] if any(map(has_glob, under)):
            return globs_transcript(under)
        case [_, leaf]:
            return leaf.endswith(TRANSCRIPT_SUFFIX) or names_session(leaf)
        case [_, session, "subagents"]:
            return names_session(session)
        case [_, session, "subagents", leaf]:
            return names_session(session) and leaf.endswith(TRANSCRIPT_SUFFIX)
        case _:
            return False


def has_glob(seg: str) -> bool:
    return any(c in seg for c in "*?[")


def names_session(seg: str) -> bool:
    return SESSION_ID.fullmatch(seg) is not None


def globs_transcript(under: list[str]) -> bool:
    """Whether a glob below a project can reach a transcript, never through ``tool-results`` or ``memory``."""
    leaf = under[-1]
    return NO_TRANSCRIPTS.isdisjoint(under) and (leaf.endswith(TRANSCRIPT_SUFFIX) or leaf[-1] in "*?]")


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


def scratch_tree(ops: list[str], cwd: Path | None) -> bool:
    """Whether every directory a search walks is a scratch directory outside any git or jj repository."""
    roots = [p for p in ops if resolved_is_dir(p, cwd)] or ["."]
    paths = [resolve_operand(p, cwd) for p in roots]
    return all(
        path is not None and is_scratch_path(resolved := path.resolve()) and not in_vcs_repo(resolved) for path in paths
    )


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
    return rewrote_text("ccx code grep", "; ".join(gains))
