"""Wrap JSON-flagged commands in ``ccx format``, and nudge wrapping shapes learned emitting JSON.

The rewrite touches only top-level, unpiped, unredirected, non-streaming occurrences
whose source text is their plain argv, so the splice after ``--`` execs unchanged.
"""

from __future__ import annotations

import shlex
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
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
    rewrite_command_occurrences,
)

from .common import (
    app_read,
    ccx_bin,
    command_shape,
    has_json_output_flag,
    has_streaming_flag,
    is_ccx_command,
    is_single_command,
    json_flagged,
    load_shapes,
    looks_like_json,
    record_shape,
    rewrote_note,
    runs_ccx,
    spells_argv,
)

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence


def wraps(occ: Occurrence) -> bool:
    command = occ.command
    return (
        occ.nesting == 0
        and not occ.piped
        and not command.redirects
        and json_flagged(command.args)
        and not runs_ccx(command)
        and not has_streaming_flag(command)
        and spells_argv(command, occ.line.raw, command.span)
    )


def wrap_json(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if not wraps(occ) or not (ccx := ccx_bin()):
        return None
    return shlex.quote(ccx) + " format -- " + (app_read(occ.command, ccx) or occ.command.raw)


rewrite_command_occurrences(
    skip_if=[Annotated("raw")],
    to=wrap_json,
    note=rewrote_note("ccx format -- <cmd>", "same data, re-encoded to its leanest shape"),
    tests={
        Input(command="gh pr list --json number"): Rewrite(pattern="vcs gh -- pr list --json number"),
        Input(command="kubectl get pods -o json"): Rewrite(pattern="format -- kubectl get pods -o json"),
        Input(command="gh repo view --json name"): Rewrite(pattern="format -- gh repo view --json name"),
        Input(command="terraform output --format=json"): Rewrite(pattern="format --"),
        Input(command="gh pr list --json x | jq .[]"): Allow(),
        Input(command="kubectl get pods -o json > pods.json"): Allow(),
        Input(command="ls -la"): Allow(),
        Input(command="gh pr list --json number # ccx:raw"): Allow(),
        Input(command="gh pr list --json number --search '# ccx:raw'"): Rewrite(pattern="vcs gh -- pr list"),
        Input(command="curl --json '{}' https://api.example.com/v1"): Rewrite(pattern="format -- curl --json"),
        Input(command='gh pr list --json number --search "is:open draft:false"'): Rewrite(
            pattern="vcs gh -- pr list --json number --search"
        ),
        Input(command="GH_HOST=x.example.com gh pr list --json number"): Allow(),
        Input(command="time gh pr list --json number"): Rewrite(pattern="vcs gh -- pr list --json number"),
        Input(command="(gh pr list --json number)"): Rewrite(pattern="vcs gh -- pr list --json number)"),
        Input(command="exec gh pr list --json number"): Allow(),
        Input(command="eval gh pr list --json number"): Allow(),
        Input(command="source render.sh --json"): Allow(),
        Input(command="gh pr view --json x --repo $(git remote get-url origin)"): Allow(),
        Input(command="kubectl get pods -o json --watch"): Allow(),
        Input(command="kubectl get pods -o json -w"): Allow(),
        Input(command="ccx format -- gh pr list --json x"): Allow(),
        Input(command="sudo ccx format -- gh pr list --json x"): Allow(),
        Input(command="gh pr list --json number; printf 'keep  two spaces'"): Rewrite(
            pattern="vcs gh -- pr list --json number; printf 'keep  two spaces'"
        ),
        Input(command="ccx format -- gh pr list --json x; printf done"): Allow(),
        Input(command="gh issue list --json number,title"): Rewrite(pattern="vcs gh -- issue list --json number,title"),
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
    """The stdout of a Bash ``tool_response``, which arrives as a mapping despite its ``str`` typing."""
    match resp:
        case {"stdout": str() as stdout} | (str() as stdout):
            return stdout
        case _:
            return ""


@on(
    Event.PostToolUse,
    only_if=[Tool("Bash")],
    tests={
        Input(tool="Bash", command="some-tool"): Allow(),
        Input(tool="Bash", command="terraform output", output='{"a": 1}'): Allow(),
    },
)
def record_json_shape(evt: BaseHookEvent) -> None:
    cl = evt.cmd.line
    if not is_single_command(cl) or is_ccx_command(cl) or has_json_output_flag(cl):
        return
    if looks_like_json(bash_stdout(evt.tool_response)):
        record_shape(evt, command_shape(cl))


class SeenEmittingJson(CustomCommandLineCondition):
    """Matches a single command whose shape was learned emitting JSON, once per shape per session."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not is_single_command(cl) or is_ccx_command(cl):
            return False
        shape = command_shape(cl)
        return shape in load_shapes(evt) and evt.ctx.s.once(shape, scope="ccx-format")


nudge(
    "This command was seen emitting JSON before — wrap it to save tokens: `ccx format -- <cmd>` "
    "re-encodes JSON stdout to its leanest shape.",
    only_if=[Tool("Bash"), SeenEmittingJson()],
    events=Event.PreToolUse,
    max_fires=None,
    tests={
        Input(command="terraform output"): Warn(pattern="ccx format"),
        Input(command="terraform output", seen={"ccx-format": ["terraform output"]}): Allow(),
        Input(command="ls"): Allow(),
    },
)
