from __future__ import annotations

from captain_hook import (
    Allow,
    Event,
    FreshSession,
    FromSubagent,
    Input,
    Prompt,
    Warn,
    nudge,
)

nudge(
    str(Prompt.load("tools/ccx_tools_nudge")),
    only_if=[FreshSession()],
    skip_if=[FromSubagent()],
    events=Event.SessionStart,
    max_fires=1,
    tests={
        Input(source="startup"): Warn(pattern="max_results 20"),
        Input(source="clear"): Warn(pattern="ToolSearch"),
        Input(source="resume"): Allow(),
        Input(source="compact"): Allow(),
        Input(source="startup", agent_id="sub-1"): Allow(),
    },
)
