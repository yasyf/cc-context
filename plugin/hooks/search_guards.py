"""Search guards: nudge identifier-alternation ``rg`` to ``ccx``; block raw ``grep`` file search."""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Tool,
    Warn,
    hook,
    nudge,
)

from .common import IDENT_ALT


class RgIdentAlternation(CustomCommandLineCondition):
    """Matches an ``rg`` whose pattern is an identifier alternation.

    `rg 'fooBar|bazQux' src/` is almost always "find these symbols" — `ccx symbol`
    resolves a definition and its callers in one shot, and `ccx grep` groups hits
    compactly. A single-term search carries no such signal, so it does not match.
    The `grep -r` case is owned by the grep block, not this nudge.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return cl.q.runs("rg") and any(IDENT_ALT.search(a) for a in cl.primary.args)


nudge(
    "Searching for several identifiers? `ccx symbol <name>` (or mcp__cc-context__symbol) "
    "resolves a definition plus its callers in one call; `ccx grep <text>` groups hits "
    "compactly. This rg still runs — just consider the ccx tools for symbol lookups.",
    only_if=[Tool("Bash"), RgIdentAlternation()],
    events=Event.PreToolUse,
    tests={
        Input(command="rg 'fooBar|bazQux' src/"): Warn(pattern="ccx symbol"),
        Input(command="rg 'Foo|Bar|Baz' ."): Warn(),
        Input(command="rg TODO"): Allow(),
        Input(command="rg 'just one term' src/"): Allow(),
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


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), UnpipedGrep()],
    message=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx grep <text>` (or mcp__cc-context__grep) / `ccx search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Escape hatch: pipe it (`… | grep`)."
    ),
    block=True,
    tests={
        Input(command="grep -rn foo src/"): Block(pattern="ccx grep"),
        Input(command="ls | grep foo"): Allow(),
        Input(command="cat x | grep foo | sort"): Allow(),
        Input(command="grep foo file.py | wc -l"): Block(),
        Input(command="grep foo a && echo done"): Block(),
        Input(command="git log --grep=fix"): Allow(),
        Input(command='git log --grep "fix bug"'): Allow(),
    },
)
