"""Steer shell JSON post-processing pipes toward ``ccx exec``."""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Tool,
    Warn,
    nudge,
)
from captain_hook.util.shell import normalize_executable

from .common import already_wrapped, head_has_json_output_flag

JSON_FILTERS = ("jq", "awk", "cut", "sed", "python3")


class JsonPipedToFilter(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return (
            not already_wrapped(cl)
            and head_has_json_output_flag(cl)
            and any(
                occ.prev_op == "|" and normalize_executable(occ.command.executable) in JSON_FILTERS
                for occ in cl.occurrences
            )
        )


nudge(
    "A JSON command piped into a shell filter dumps raw JSON into context. "
    "Run it through `ccx exec` with `sh()` and `json.loads` to return only the projection.",
    only_if=[Tool("Bash"), JsonPipedToFilter()],
    events=Event.PreToolUse,
    tests={
        Input(command="gh pr list --json number,title | jq '.[].title'"): Warn(pattern="ccx exec"),
        Input(command="kubectl get pods -o json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'"): Warn(),
        Input(command="gh pr list --json x | jq '.[]' | head -5"): Warn(pattern="ccx exec"),
        Input(command="gh pr list --json number,title | /usr/bin/jq '.[].title'"): Warn(pattern="ccx exec"),
        Input(command='gh pr list --json number,title | "jq" .'): Warn(pattern="ccx exec"),
        Input(command="gh pr list --json number"): Allow(),
        Input(command="ps aux | awk '{print $1}'"): Allow(),
        Input(command="gh pr list --json x && echo done"): Allow(),
        Input(command="ccx format -- gh pr list --json x | jq ."): Allow(),
        Input(
            command="ccx exec 'import json\n"
            'async def main(): return json.loads(await sh("gh pr list --json number"))[0]\n'
            "asyncio.run(main())'"
        ): Allow(),
    },
)
