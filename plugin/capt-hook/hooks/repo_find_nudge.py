"""Advisory nudge: a direct, maximally-broad ``ccx repo find`` glob is orientation, not enumeration.

A broad glob (``**``, ``*``, or a pure-wildcard first segment) lists files in path order under a token
budget — the first move should be ``ccx repo overview``, or a glob anchored with a literal component
(``internal/**/*.go``) / a ``--scope``. Fires once per session, non-blocking, on the first-sight Bash
``ccx repo find <glob>`` or the cc-context ``ccx_repo_find`` MCP tool.

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

# `ccx repo find`'s value-taking flags: their next token is a value, not the glob operand.
VALUE_FLAGS = frozenset({"--scope", "--budget"})

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


def repo_find_glob(evt: BaseHookEvent) -> str | None:
    """The glob from a direct ``ccx repo find`` — MCP tool input or Bash argv — else ``None``.

    ``None`` for a scoped call (a non-empty ``--scope``/``scope``), a non-find command, or a find with
    no positional glob — none of which the nudge should touch. The Bash walk consumes value-taking flags
    (:data:`VALUE_FLAGS`) so a ``--budget 2000`` value never masquerades as the glob, and treats an empty
    ``--scope``/``--scope=`` (either token form) as unscoped so the nudge still fires.
    """
    if (tool := evt.tool_name) and mcp_repo_find(tool):
        ti = evt._tool_input
        if ti.get("scope"):
            return None
        return glob if isinstance(glob := ti.get("glob"), str) else None
    cl = evt.cmd.line
    if not cl or Path(cl.primary.executable).name != "ccx" or cl.primary.args[:2] != ("repo", "find"):
        return None
    rest = cl.primary.args[2:]
    scoped = False
    glob: str | None = None
    i, n = 0, len(rest)
    while i < n:
        a = rest[i]
        if a in VALUE_FLAGS:  # two-token flag: consume its value; a non-empty --scope value scopes
            scoped = scoped or (a == "--scope" and i + 1 < n and bool(rest[i + 1]))
            i += 2
            continue
        if a.startswith("--scope="):
            scoped = scoped or bool(a[len("--scope=") :])
            i += 1
            continue
        if a.startswith("-"):
            i += 1
            continue
        if glob is None:
            glob = a
        i += 1
    return None if scoped else glob


class BroadRepoFind(CustomCondition):
    """Matches the first-sight direct broad-glob ``ccx repo find`` (Bash or MCP) with no ``--scope``.

    Fires once per session — the ``once`` self-gate is keyed by this class name (its own SessionStore
    slot), so a scoped call, a literal-component glob, and every later broad-find in the session all
    pass silently. The glob-shape check runs before the latch, so a call that would not nudge never
    burns it.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        glob = repo_find_glob(evt)
        if glob is None or not broad_glob(glob):
            return False
        return evt.ctx.s.once(type(self).__name__, scope="ccx-repo-find")


nudge(
    "Broad-glob find lists files in path order under a token budget — for orientation "
    "ccx repo overview (MCP: ccx_repo_overview) is the right first call; to enumerate, give it a literal "
    "glob component (internal/**/*.go) or a --scope / the scope field.",
    only_if=[Tool("Bash", "ccx_repo_find"), BroadRepoFind()],
    events=Event.PreToolUse,
    max_fires=None,
    tests={
        Input(command='ccx repo find "**"'): Warn(pattern="ccx repo overview"),
        Input(command="ccx repo find '**/*'"): Warn(),
        Input(command="ccx repo find '*'"): Warn(),
        Input(command='ccx repo find "**/*.go"'): Warn(),  # pure-wildcard first segment
        Input(command='ccx repo find --scope= "**"'): Warn(),  # empty --scope value = unscoped → fires
        Input(command="ccx repo find --scope '' \"**\""): Warn(),  # two-token empty --scope = unscoped → fires
        Input(command='ccx repo find --budget 2000 "**"'): Warn(),  # --budget value skipped; `**` is the glob → fires
        Input(command='ccx repo find "[a-z]/**"'): Warn(),  # char-class first segment = wildcard, not literal
        Input(command='ccx repo find "{a,b}/**"'): Warn(),  # brace-group first segment = wildcard
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_repo_find", tool_input={"glob": "**"}): Warn(
            pattern="ccx repo overview"
        ),
        Input(command='ccx repo find --scope internal "**"'): Allow(),  # scoped → silent
        Input(command='ccx repo find --scope=internal "**"'): Allow(),  # non-empty =scope still scoped → silent
        Input(command='ccx repo find --budget 2000 --scope internal "**"'): Allow(),  # --budget value skipped, still scoped → silent
        # WONTFIX (nudge miss): a nested-brace first segment `{a,{b,c}}/**` isn't matched by BROAD_SEGMENT → silent.
        Input(command='ccx repo find "internal/**/*.go"'): Allow(),  # literal first segment → silent
        Input(command='ccx repo find "*.go"'): Allow(),  # literal component in the first segment → silent
        Input(tool="mcp__cc-context__ccx_repo_find", tool_input={"glob": "**", "scope": "internal"}): Allow(),
        Input(command="ccx repo overview"): Allow(),  # not a find
        Input(command='rg foo "**"'): Allow(),  # not ccx repo find
    },
)
