"""Advisory nudge: a direct ``ccx repo find`` that selects the whole tree is orientation, not enumeration.

``repo find`` takes an ordered gitignore-style glob list, so breadth is a property of the list, not of
any one glob: with no includes at all — an empty list, ``null``, or only ``!`` exclusions — the call selects
everything it does not exclude, and once an include makes the list a whitelist, one broad include
(``**``, ``*``, a pure-wildcard first segment) widens it back to the whole tree. Either way the call
lists files in path order under a token budget — the first move should be ``ccx repo overview``, or a
list carrying an include anchored by a literal component (``internal/**/*.go``). Fires once per
session, non-blocking, on the first-sight Bash ``ccx repo find <globs...>`` or the cc-context
``ccx_repo_find`` MCP tool.

No conflict with the read-only auto-approval in ``approval_guards``: those approvers pin
``events=Event.PermissionRequest`` explicitly (the ``approve()`` default is now
``PreToolUse | PermissionRequest``), this nudge registers on ``Event.PreToolUse``. Captain-hook
dispatches each event separately, so the two never compose in one ``dispatch`` and the advisory is
never swallowed by the approval (only a same-event approval beats a warn).
"""

from __future__ import annotations

import re
from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    CustomCondition,
    Event,
    Input,
    Tool,
    Warn,
    nudge,
)

# The cc-context MCP server names — direct config vs the plugin-installed prefix (mirrors approval_guards).
CCX_SERVERS = frozenset({"cc-context", "plugin_cc-context_cc-context"})

# `ccx repo find`'s one value-taking flag: its next token is a value, not a glob operand.
VALUE_FLAGS = frozenset({"--budget"})

# A glob whose whole body, or whose first path segment, is pure wildcards enumerates the tree in path
# order — a literal component anywhere in the first segment (``internal/**``, ``*.go``) anchors it.
BROAD_GLOBS = frozenset({"**", "**/*", "*", "*/**"})

# A first path segment of only wildcard constructs — ``*``/``?`` and whole ``[...]``/``{...}`` groups —
# has no literal anchor; one literal char outside a group (``.go`` in ``*.go``) breaks the match.
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
    """Every glob of a direct ``ccx repo find`` — MCP tool input or Bash argv — else ``None``.

    ``None`` means "nothing here for the nudge to judge": not a direct find, or a call the surface
    will refuse before it lists anything. Both surfaces demand the operand — the MCP schema is
    ``"required":["globs"]`` and the CLI is ``cobra.MinimumNArgs(1)`` — so an absent or non-list
    ``globs`` key and a globless ``ccx repo find`` are ``None``, since advising a call that never
    runs would spend the session's one advisory on nothing. A ``null`` or empty ``globs`` (its
    schema is ``["null","array"]``) is a real call that selects the whole tree, so it maps to
    ``[]``. Globs are
    positionals, so the Bash walk collects every non-flag token and consumes value-taking flags
    (:data:`VALUE_FLAGS`) to keep a ``--budget 2000`` value from masquerading as one; a negated glob
    (``!*_test.go``) carries no leading ``-`` and so counts as the positional it is.
    """
    if (tool := evt.tool_name) and mcp_repo_find(tool):
        match evt._tool_input:
            case {"globs": list(globs)}:
                return globs
            case {"globs": None}:
                return []
            case _:
                return None
    cl = evt.cmd.line
    if not cl or Path(cl.primary.executable).name != "ccx" or cl.primary.args[:2] != ("repo", "find"):
        return None
    rest = cl.primary.args[2:]
    out: list[str] = []
    i, n = 0, len(rest)
    while i < n:
        a = rest[i]
        if a in VALUE_FLAGS:
            i += 2
            continue
        if not a.startswith("-"):
            out.append(a)
        i += 1
    return out or None


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
    """Matches the first-sight direct whole-tree ``ccx repo find`` (Bash or MCP).

    Fires once per session — the ``once`` self-gate is keyed by this class name (its own SessionStore
    slot), so a list whose includes are all anchored and every later broad find in the session pass
    silently. The list-breadth check runs before the latch, so a call that would not nudge never burns
    it.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        globs = repo_find_globs(evt)
        if globs is None or not broad_find(globs):
            return False
        return evt.ctx.s.once(type(self).__name__, scope="ccx-repo-find")


nudge(
    "A find whose globs select the whole tree lists files in path order under a token budget — for "
    "orientation ccx repo overview (MCP: ccx_repo_overview) is the right first call; to enumerate, give "
    "it an include anchored by a literal component (internal/**/*.go) and no broad one.",
    only_if=[Tool("Bash", "ccx_repo_find"), BroadRepoFind()],
    events=Event.PreToolUse,
    max_fires=None,
    tests={
        Input(command='ccx repo find "**"'): Warn(pattern="ccx repo overview"),
        Input(command="ccx repo find '**/*'"): Warn(),
        Input(command="ccx repo find '*'"): Warn(),
        Input(command='ccx repo find "**/*.go"'): Warn(),  # pure-wildcard first segment
        Input(command='ccx repo find --budget 2000 "**"'): Warn(),  # --budget value skipped; `**` is the glob → fires
        Input(command='ccx repo find -- "**"'): Warn(),  # `--` is a flag token, `**` the positional
        Input(command='ccx repo find "[a-z]/**"'): Warn(),  # char-class first segment = wildcard, not literal
        Input(command='ccx repo find "{a,b}/**"'): Warn(),  # brace-group first segment = wildcard
        Input(command='ccx repo find "*.go" "**"'): Warn(),  # a broad include in any slot widens the whitelist
        Input(command='ccx repo find "internal/**" "**"'): Warn(),  # …anchored first entry included
        Input(command="ccx repo find '!*_test.go'"): Warn(),  # exclusion-only: everything it doesn't exclude
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_repo_find", tool_input={"globs": ["**"]}): Warn(
            pattern="ccx repo overview"
        ),
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": ["**"]}): Warn(),  # direct-config server name
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": []}): Warn(),  # empty list selects everything
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": None}): Warn(),  # schema is ["null","array"]
        # WONTFIX (nudge miss): a nested-brace first segment `{a,{b,c}}/**` isn't matched by BROAD_SEGMENT → silent.
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={}): Allow(),  # globs is required — the server refuses
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": "**"}): Allow(),  # non-list — type-refused
        Input(command="ccx repo find"): Allow(),  # cobra.MinimumNArgs(1) refuses — never a call to advise
        Input(command="ccx repo find --budget 2000"): Allow(),  # …flags don't make it one
        Input(command='ccx repo find "internal/**/*.go"'): Allow(),  # literal first segment → silent
        Input(command='ccx repo find "*.go"'): Allow(),  # literal component in the first segment → silent
        Input(command="ccx repo find internal"): Allow(),  # a bare directory is an anchored include
        Input(command="ccx repo find '*.go' '!vendor/**'"): Allow(),  # anchored include + exclusion → silent
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"globs": ["internal/**"]}): Allow(),
        Input(command="ccx repo overview"): Allow(),  # not a find
        Input(command='rg foo "**"'): Allow(),  # not ccx repo find
    },
)
