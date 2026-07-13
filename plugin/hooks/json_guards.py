"""Drive ``ccx format`` usage: auto-wrap statically JSON-flagged commands, learn the rest.

Commands carrying a JSON-output flag (``--json``, ``-o json``, ``--output=json``,
``--format json``) are **rewritten** in place to ``ccx format -- <cmd>`` so their JSON
stdout is re-encoded to the leanest shape for its data — a table (CSV/TSV/markdown),
TRON, JSONL, prose, or compact JSON, with TOON only for large uniform arrays —
before it floods context. Commands
*observed* emitting JSON at runtime are recorded to a persistent shapes store; next
time a command of that shape runs, the agent gets a **nudge** to wrap it. The rewrite
fires only on a single command (no pipe/redirect — ``--json | jq`` needs raw JSON)
that isn't already wrapped, is plain argv (no env prefix, subshell, or shell keyword
— the spliced raw text must re-parse as words after ``--``), and isn't streaming
(``--watch``/``--follow`` never exits, and the wrapper buffers stdout until exit).
It emits ``permissionDecision: allow`` so it adds no extra prompt.
"""

from __future__ import annotations

import shlex

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    nudge,
    on,
    rewrite_command,
)

from .common import (
    already_wrapped,
    ccx_bin,
    command_shape,
    has_json_output_flag,
    has_streaming_flag,
    is_ccx_command,
    is_plain_argv,
    is_single_command,
    load_shapes,
    looks_like_json,
    record_shape,
)


def wraps(cl: CommandLine) -> bool:
    return (
        is_single_command(cl)
        and has_json_output_flag(cl)
        and not already_wrapped(cl)
        and not has_streaming_flag(cl)
        and is_plain_argv(cl)
    )


def wrap_json(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if cl is None or not wraps(cl) or not (ccx := ccx_bin()):
        return None
    return f"{shlex.quote(ccx)} format -- {evt.command}"


def wrap_note(evt: BaseHookEvent) -> str:
    return "Wrapped a JSON-emitting command in `ccx format`: same data, re-encoded to its leanest shape to save tokens."


rewrite_command(
    to=wrap_json,
    note=wrap_note,
    tests={
        Input(command="gh pr list --json number"): Rewrite(pattern="format -- gh pr list --json number"),
        Input(command="kubectl get pods -o json"): Rewrite(pattern="format -- kubectl get pods -o json"),
        Input(command="terraform output --format=json"): Rewrite(pattern="format --"),
        Input(command="gh pr list --json x | jq .[]"): Allow(),
        Input(command="kubectl get pods -o json > pods.json"): Allow(),
        Input(command="ls -la"): Allow(),
        # curl's `--json` names a request *body*, but the response it signals is JSON — exactly
        # what `ccx format` compacts. Without the wrap an unpiped `curl --json … <api>` dumps its
        # (typically large) JSON response raw into context, so the wrap is intentional here.
        Input(command="curl --json '{}' https://api.example.com/v1"): Rewrite(
            pattern="format -- curl --json"
        ),
        # A quoted argument still word-splits to the parsed argv, so the wrap fires.
        Input(command='gh pr list --json number --search "is:open draft:false"'): Rewrite(
            pattern="format -- gh pr list --json number --search"
        ),
        # Non-argv shapes: after `ccx format --` an env-assignment prefix execs as
        # argv[0] ("executable file not found"), a subshell is a bash syntax error,
        # and `time` stops being a shell keyword — the rewrite bails on each.
        Input(command="GH_HOST=x.example.com gh pr list --json number"): Allow(),
        Input(command="(gh pr list --json number)"): Allow(),
        Input(command="time gh pr list --json number"): Allow(),
        # Builtins with no binary counterpart: after `--` they exec as literal
        # binaries ("executable file not found") — the rewrite bails on each.
        Input(command="exec gh pr list --json number"): Allow(),
        Input(command="eval gh pr list --json number"): Allow(),
        Input(command="source render.sh --json"): Allow(),
        # Watch/follow commands never exit, but the wrapper buffers stdout and
        # converts only after exit — wrapping one is silence until Bash times out.
        Input(command="kubectl get pods -o json --watch"): Allow(),
        Input(command="kubectl get pods -o json -w"): Allow(),
        # already_wrapped must recognize the ccx format wrap, or this rewrite
        # would re-wrap its own output forever (the wrapped line still has --json).
        Input(command="ccx format -- gh pr list --json x"): Allow(),
        # `ccx exec` pass-through is deliberate: the script is one opaque token, so a
        # `--json` inside it never reads as this command's own JSON-output flag.
        Input(
            command="ccx exec 'import json\n"
            'async def main(): return json.loads(await sh("gh pr list --json number"))\n'
            "asyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("kubectl get pods -o json")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)


def bash_stdout(resp: object) -> str:
    """Extract the stdout text from a Bash ``PostToolUse`` ``tool_response``.

    Claude Code delivers a Bash result as a structured mapping (``{"stdout": ...,
    "stderr": ..., "interrupted": ...}``), despite the event's ``str | None`` typing —
    a known upstream annotation gap — so the JSON-shape learner reads ``stdout`` from
    it. A plain string (the declared shape, and other tools) passes through, and
    anything else has no stdout to shape.
    """
    if isinstance(resp, dict):
        stdout = resp.get("stdout")
        return stdout if isinstance(stdout, str) else ""
    return resp if isinstance(resp, str) else ""


@on(
    Event.PostToolUse,
    only_if=[Tool("Bash")],
    tests={
        Input(tool="Bash", command="some-tool"): Allow(),
    },
)
def record_json_shape(evt: BaseHookEvent) -> None:
    out = bash_stdout(evt.tool_response)
    cl = evt.command_line
    if not out or cl is None or not looks_like_json(out):
        return None
    if already_wrapped(cl) or is_ccx_command(cl) or has_json_output_flag(cl) or not is_single_command(cl):
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
        # `is_ccx_command` also covers shapes learned before ccx commands were
        # excluded from the recorder — the durable store is global and long-lived.
        if already_wrapped(cl) or is_ccx_command(cl) or not is_single_command(cl):
            return False
        shape = command_shape(cl)
        if shape not in load_shapes(evt):
            return False
        return evt.ctx.s.once(shape, scope="ccx-format")


nudge(
    "This command was seen emitting JSON before — wrap it to save tokens: `ccx format -- <cmd>` "
    "re-encodes JSON stdout to its leanest shape (or mcp__cc-context__BashFormat runs it and "
    "returns the compacted output).",
    only_if=[Tool("Bash"), SeenEmittingJson()],
    events=Event.PreToolUse,
    max_fires=50,
    tests={
        Input(command="ls"): Allow(),
    },
)
