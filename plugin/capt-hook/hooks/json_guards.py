"""Drive ``ccx format`` usage: auto-wrap statically JSON-flagged commands, learn the rest.

Commands carrying a JSON-output flag (``--json``, ``-o json``, ``--output=json``,
``--format json``) are **rewritten** in place to ``ccx format -- <cmd>`` so their JSON
stdout is re-encoded to the leanest shape for its data — a table (CSV/TSV/markdown),
TRON, JSONL, prose, or compact JSON, with TOON only for large uniform arrays —
before it floods context. Commands
*observed* emitting JSON at runtime are recorded to a persistent shapes store; next
time a command of that shape runs, the agent gets a **nudge** to wrap it. The rewrite
fires on each eligible top-level occurrence of a compound line, leaving its siblings
byte-for-byte intact. Piped and redirected occurrences stay raw (``--json | jq`` needs
JSON), as do occurrences that are already wrapped, aren't plain argv (an env prefix,
subshell, or shell keyword cannot safely follow ``--``), are streaming
(``--watch``/``--follow`` never exits, and the wrapper buffers stdout until exit), or are
nested below top level — a ``$(…)`` substitution or ``eval`` payload whose output is
captured, not dumped to context, and whose splice cannot survive its enclosing quote
layers. It emits ``permissionDecision: allow`` so it adds no extra prompt.
"""

from __future__ import annotations

import shlex
from pathlib import Path
from typing import TYPE_CHECKING

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
    rewrite_command_occurrences,
)

from .common import (
    SHELL_WORD_EXECUTABLES,
    STREAMING_FLAGS,
    already_wrapped,
    ccx_bin,
    command_shape,
    has_json_output_flag,
    is_ccx_command,
    is_single_command,
    json_flagged,
    load_shapes,
    looks_like_json,
    record_shape,
)

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence


def occurrence_has_json_output_flag(occ: Occurrence) -> bool:
    return json_flagged(occ.command.args)


def occurrence_has_streaming_flag(occ: Occurrence) -> bool:
    return any(arg.split("=", 1)[0] in STREAMING_FLAGS for arg in occ.command.args)


def occurrence_is_plain_argv(occ: Occurrence) -> bool:
    cmd = occ.command
    if cmd.env or cmd.executable in SHELL_WORD_EXECUTABLES:
        return False
    try:
        words = shlex.split(cmd.raw)
    except ValueError:
        return False
    return words == list(cmd.argv)


def occurrence_already_wrapped(occ: Occurrence) -> bool:
    cmd = occ.command
    return "ccx format" in cmd.raw or Path(cmd.executable).name == "ccx"


def wraps(occ: Occurrence) -> bool:
    return (
        occ.nesting == 0
        and not occ.piped
        and not occ.command.redirects
        and occurrence_has_json_output_flag(occ)
        and not occurrence_already_wrapped(occ)
        and not occurrence_has_streaming_flag(occ)
        and occurrence_is_plain_argv(occ)
    )


def wrap_json(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if not wraps(occ) or not (ccx := ccx_bin()):
        return None
    return shlex.quote(ccx) + " format -- " + occ.command.raw


def wrap_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    return "Wrapped a JSON-emitting command in `ccx format`: same data, re-encoded to its leanest shape to save tokens."


rewrite_command_occurrences(
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
        # An env-assignment prefix execs as argv[0] after `ccx format --`
        # ("executable file not found"), so it bails. `time`, though, is a wrapper the parser
        # unwraps — the plain-argv `gh` occurrence splices in place with `time` preserved ahead
        # of it (the wrap reaches through the prefix, which is exactly what the JSON guard wants).
        Input(command="GH_HOST=x.example.com gh pr list --json number"): Allow(),
        Input(command="time gh pr list --json number"): Rewrite(pattern="format -- gh pr list --json number"),
        # Occurrence splicing preserves the subshell delimiters outside the command's
        # span, so its inner plain argv is now safe to wrap in place.
        Input(command="(gh pr list --json number)"): Rewrite(
            pattern="format -- gh pr list --json number)"
        ),
        # `exec`/`source` are shell-word builtins that bail at the top level (they can't follow
        # `--`). `eval` splits its payload into a *nested* occurrence: the inner `gh` is plain
        # argv, but its splice can't survive the eval quote layers, so the nesting gate declines
        # it cleanly rather than leaning on a swallowed splice failure.
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
        # Compound rewrite: only the JSON occurrence changes; its sibling stays byte-identical.
        Input(command="gh pr list --json number; printf 'keep  two spaces'"): Rewrite(
            pattern="format -- gh pr list --json number; printf 'keep  two spaces'"
        ),
        # Re-evaluating a previously spliced occurrence is a no-op even though its args retain --json.
        Input(command="ccx format -- gh pr list --json x; printf done"): Allow(),
        # A plain single command keeps the original whole-command rewrite behavior.
        Input(command="gh issue list --json number,title"): Rewrite(
            pattern="format -- gh issue list --json number,title"
        ),
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
    cl = evt.cmd.line
    if not out or not cl or not looks_like_json(out):
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
