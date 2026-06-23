"""VCS guard: block a bare/range ``git diff`` that dumps the full patch into context."""

from __future__ import annotations

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Tool,
    hook,
)

from .common import GIT_DIFF_SUMMARY_FLAGS


class GitDiffPager(CustomCommandLineCondition):
    """Matches a ``git diff`` that is neither path-scoped nor a stat-only summary.

    `git diff -- <path>` and `git diff <ref> -- <path>` are scoped; `git diff
    --stat`/`--numstat`/`--name-only`/... are summaries. Everything else (`git diff`,
    `git diff HEAD~1`) dumps the full patch — that is what this matches.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return (
            cl.q.runs("git", "diff")
            and not cl.q.contains_token("--")
            and not any(cl.q.contains_token(flag) for flag in GIT_DIFF_SUMMARY_FLAGS)
        )


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitDiffPager()],
    message=(
        "BLOCKED: `git diff` without a pathspec dumps the full patch into context. "
        "Use `ccx diff` for a compact summary (or mcp__cc-context__diff). "
        "Already know the file? `git diff -- <path>` / `git diff <ref> -- <path>` stays allowed, "
        "as do `git diff --stat`/`--numstat`/`--name-only`. Escape hatch for the full patch: `ccx diff --full`."
    ),
    block=True,
    tests={
        Input(command="git diff"): Block(pattern="ccx diff"),
        Input(command="git diff HEAD~1"): Block(),
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --numstat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff HEAD~1 -- src/x.go"): Allow(),
        Input(command="git status"): Allow(),
    },
)
