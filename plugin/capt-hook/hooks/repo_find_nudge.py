"""Nudge a direct whole-tree ``ccx repo find`` toward ``ccx repo overview``."""

from __future__ import annotations

import re

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandSchema,
    CustomCondition,
    Event,
    Input,
    Operand,
    Option,
    Tool,
    Warn,
    nudge,
)

from .common import CCX_SERVERS

REPO_FIND = CommandSchema(
    "ccx",
    operands=(Operand("subcommand", count=2), Operand("globs", count="*")),
    options=(Option("budget", ("--budget",), int),),
)

BROAD_GLOBS = frozenset({"**", "**/*", "*", "*/**"})

BROAD_SEGMENT = re.compile(r"^(?:\[[^\]]*\]|\{[^}]*\}|[*?])+$")


def mcp_repo_find(tool: str) -> bool:
    """Whether ``tool`` is the cc-context ``ccx_repo_find`` MCP tool, server-pinned by exact name."""
    match tool.split("__", 2):
        case ["mcp", server, "ccx_repo_find"] if server in CCX_SERVERS:
            return True
        case _:
            return False


def broad_glob(glob: str) -> bool:
    """Whether a repo-find glob is maximally broad — its whole body or first segment is pure wildcards."""
    g = glob.strip()
    if not g:
        return False
    if g in BROAD_GLOBS:
        return True
    return BROAD_SEGMENT.fullmatch(g.split("/", 1)[0]) is not None


def repo_find_globs(evt: BaseHookEvent) -> list[str] | None:
    if (tool := evt.tool_name) and mcp_repo_find(tool):
        match evt.input.raw:
            case {"globs": list(globs)}:
                return globs
            case {"globs": None}:
                return []
            case _:
                return None
    return next(iter(bash_repo_find_globs(evt)), None)


def bash_repo_find_globs(evt: BaseHookEvent) -> list[list[str]]:
    bound = (REPO_FIND.bind(call) for call in evt.cmd.calls("ccx"))
    return [
        list(arguments.values["globs"])
        for arguments in bound
        if arguments.values.get("subcommand") == ("repo", "find") and arguments.values.get("globs")
    ]


def broad_find(globs: list[str]) -> bool:
    """Whether a whole glob list selects the tree — the list-level reading of :func:`broad_glob`.

    Mirrors ``MatchGlobs`` (``internal/backend/globmatch.go``): an empty list matches everything, an
    exclusion-only list everything it does not exclude, and any include turns the list into a whitelist
    whose union widens with each entry. So no includes at all is broad however many exclusions trail
    it, and once there are includes one broad one is enough — ``ccx repo find '*.go' '**'`` enumerates
    the repo exactly as ``'**'`` alone does, and a later anchored include cannot narrow it back.
    """
    includes = [g for g in globs if not g.startswith("!")]
    return not includes or any(broad_glob(g) for g in includes)


class BroadRepoFind(CustomCondition):
    def check(self, evt: BaseHookEvent) -> bool:
        if (tool := evt.tool_name) and mcp_repo_find(tool):
            globs = repo_find_globs(evt)
            return globs is not None and broad_find(globs)
        return any(broad_find(globs) for globs in bash_repo_find_globs(evt))


nudge(
    "This `ccx repo find` selects the whole tree and lists files in path order. "
    "Run `ccx repo overview` to orient, or anchor the first glob on a literal directory such as `internal/**`.",
    only_if=[Tool("Bash", "ccx_repo_find"), BroadRepoFind()],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(command='ccx repo find "**"'): Warn(pattern="ccx repo overview"),
        Input(command="ccx repo find '**/*'"): Warn(),
        Input(command="ccx repo find '*'"): Warn(),
        Input(command='ccx repo find "**/*.go"'): Warn(),
        Input(command='ccx repo find --budget 2000 "**"'): Warn(),
        Input(command='ccx repo find -- "**"'): Warn(),
        Input(command='ccx repo find "[a-z]/**"'): Warn(),
        Input(command='ccx repo find "{a,b}/**"'): Warn(),
        Input(command='ccx repo find "*.go" "**"'): Warn(),
        Input(command='ccx repo find "internal/**" "**"'): Warn(),
        Input(command="ccx repo find '!*_test.go'"): Warn(),
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_repo_find", tool_input={"globs": ["**"]}): Warn(
            pattern="ccx repo overview"
        ),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": ["**"]}): Warn(),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": []}): Warn(),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": None}): Warn(),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={}): Allow(),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": "**"}): Allow(),
        Input(command="ccx repo find"): Allow(),
        Input(command="ccx repo find --budget 2000"): Allow(),
        Input(command='ccx repo find "internal/**/*.go"'): Allow(),
        Input(command='ccx repo find "*.go"'): Allow(),
        Input(command="ccx repo find internal"): Allow(),
        Input(command="ccx repo find '*.go' '!vendor/**'"): Allow(),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": ["internal/**"]}): Allow(),
        Input(command="ccx repo overview"): Allow(),
        Input(command='rg foo "**"'): Allow(),
    },
)
