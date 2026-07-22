"""Steer shell JSON post-processing pipes toward ``ccx exec``.

``json_guards`` deliberately skips a piped JSON command — ``<cmd --json> | jq``
needs the raw JSON on stdout, so the ``ccx format`` wrap would break the pipe. That
skip is this module's entry point: when a JSON-flagged head command pipes into a
shell filter (``jq``/``awk``/``cut``/``sed``/``python3``), a non-blocking nudge
suggests ``ccx exec`` instead — ``sh()`` the command, ``json.loads`` it
in-sandbox, and return only the projection, so the raw JSON never enters context.

Trust boundary — document, don't police. ``sh()`` runs host-side inside the ccx
process; PreToolUse hooks see only the opaque script string, so no hook can
inspect or rewrite in-sandbox ``sh()`` calls. Do NOT regex the script text here:
that duplicates policy (trivially evadable) whose single source of truth is
``internal/codeexec/sh.go``. exec is a *sanctioned* bypass of the read guards —
``sh("cat big.go")`` inside a script is fine precisely because only the
budget-capped return value enters context — while host safety stays at Bash-tool
parity (the same user privileges the hooks already permit for Bash).

A composition-burst nudge (N read-shaped calls in a row → suggest one exec
script) was considered and deferred: consecutive-call count is a weak proxy for
"composing output" and fires on legitimate exploration.
"""

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

from .common import already_wrapped, head_has_json_output_flag

# The shell filters a JSON pipe projects through. Each marks the tail of the
# pattern this nudge steers: JSON produced at the head, whittled down in-shell.
JSON_FILTERS = ("jq", "awk", "cut", "sed", "python3")


class JsonPipedToFilter(CustomCommandLineCondition):
    """Matches ``<cmd with a JSON-output flag> | jq/awk/cut/sed/python3 …``.

    The head command must carry a JSON-output flag and pipe (not ``&&``/``;``-chain)
    into one of :data:`JSON_FILTERS`. An already-wrapped line is skipped — ``ccx format
    -- <cmd --json> | jq`` still carries ``--json`` in its head args, but its output
    is already compacted, so the steer would be noise.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if len(cl.parts) < 2 or already_wrapped(cl):
            return False
        if not head_has_json_output_flag(cl):
            return False
        return any(
            cmd.executable in JSON_FILTERS and cl.parts[i - 1][1] == "|"
            for i, (cmd, _) in enumerate(cl.parts)
            if i > 0
        )


nudge(
    "Projecting JSON through a pipe? `ccx exec` can sh() the command, json.loads it in-sandbox, "
    "and return only the projection — the raw JSON never enters context. This pipe still runs.",
    only_if=[Tool("Bash"), JsonPipedToFilter()],
    events=Event.PreToolUse,
    tests={
        Input(command="gh pr list --json number,title | jq '.[].title'"): Warn(pattern="ccx exec"),
        Input(command="kubectl get pods -o json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'"): Warn(),
        Input(command="gh pr list --json x | jq '.[]' | head -5"): Warn(pattern="ccx exec"),
        Input(command="gh pr list --json number"): Allow(),  # unpiped — the format rewrite owns it
        Input(command="ps aux | awk '{print $1}'"): Allow(),  # non-JSON pipe
        Input(command="gh pr list --json x && echo done"): Allow(),  # chain, not a pipe
        Input(command="ccx format -- gh pr list --json x | jq ."): Allow(),  # already compacted
        # Already ccx exec — the whole point of the steer; never nudge it about itself.
        Input(
            command="ccx exec 'import json\n"
            'async def main(): return json.loads(await sh("gh pr list --json number"))[0]\n'
            "asyncio.run(main())'"
        ): Allow(),
    },
)
