"""Drive ``ccx toon`` usage: auto-wrap statically JSON-flagged commands, learn the rest.

Commands carrying a JSON-output flag (``--json``, ``-o json``, ``--output=json``,
``--format json``) are **rewritten** in place to ``ccx toon -- <cmd>`` so their JSON
stdout is re-encoded to TOON (or compact JSON) before it floods context. Commands
*observed* emitting JSON at runtime are recorded to a persistent shapes store; next
time a command of that shape runs, the agent gets a **nudge** to wrap it. The rewrite
fires only on a single command (no pipe/redirect — ``--json | jq`` needs raw JSON)
that isn't already wrapped, and emits ``permissionDecision: allow`` so it adds no
extra prompt.
"""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    Warn,
    nudge,
    on,
    rewrite_command,
)

from .common import (
    already_wrapped,
    ccx_bin,
    command_shape,
    has_json_output_flag,
    is_single_command,
    load_shapes,
    looks_like_json,
    record_shape,
)


def _wraps(cl: CommandLine) -> bool:
    return is_single_command(cl) and has_json_output_flag(cl) and not already_wrapped(cl)


def wrap_json(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if cl is None or not _wraps(cl) or not (ccx := ccx_bin()):
        return None
    return f"{ccx} toon -- {evt.command}"


def _wrap_note(evt: BaseHookEvent) -> str:
    return "Wrapped a JSON-emitting command in `ccx toon`: same data, re-encoded to TOON to save tokens."


rewrite_command(
    to=wrap_json,
    note=_wrap_note,
    tests={
        Input(command="gh pr list --json number"): Rewrite(pattern="toon -- gh pr list --json number"),
        Input(command="kubectl get pods -o json"): Rewrite(pattern="toon -- kubectl get pods -o json"),
        Input(command="terraform output --format=json"): Rewrite(pattern="toon --"),
        Input(command="gh pr list --json x | jq .[]"): Allow(),
        Input(command="kubectl get pods -o json > pods.json"): Allow(),
        Input(command="ls -la"): Allow(),
        Input(command="ccx toon -- gh pr list --json x"): Allow(),
    },
)


@on(
    Event.PostToolUse,
    only_if=[Tool("Bash")],
    tests={
        Input(tool="Bash", command="some-tool"): Allow(),
    },
)
def record_json_shape(evt: BaseHookEvent) -> None:
    out = evt.tool_response
    cl = evt.command_line
    if not out or cl is None or not looks_like_json(out):
        return None
    if already_wrapped(cl) or has_json_output_flag(cl) or not is_single_command(cl):
        return None
    record_shape(evt, command_shape(cl))
    return None


class SeenEmittingJson(CustomCommandLineCondition):
    """Matches a single command whose shape was previously observed emitting JSON.

    Fires once per shape per session (self-gated via ``evt.ctx.s.once``), and only
    when the command isn't already wrapped — so the learned nudge advises the agent
    to wrap a command it has watched produce JSON, without nagging on repeats.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if already_wrapped(cl) or not is_single_command(cl):
            return False
        shape = command_shape(cl)
        if shape not in load_shapes(evt):
            return False
        return evt.ctx.s.once(shape, scope="ccx-toon")


nudge(
    "This command was seen emitting JSON before — wrap it to save tokens: `ccx toon -- <cmd>` "
    "re-encodes JSON stdout to TOON (or mcp__cc-context__BashToon runs it and returns the compacted output).",
    only_if=[Tool("Bash"), SeenEmittingJson()],
    events=Event.PreToolUse,
    max_fires=50,
    tests={
        Input(command="ls"): Allow(),
    },
)
