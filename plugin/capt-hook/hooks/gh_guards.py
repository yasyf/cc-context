"""Refuse the ``gh`` calls that spend the gh user's GitHub quota past ccx's gateway.

A ``while`` or ``until`` loop that calls ``gh`` polls until the quota every lander, proof and watch
on the machine shares runs out; a read with ``GH_TOKEN`` or ``GITHUB_TOKEN`` set runs on that token
instead of the GitHub App's.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Tool,
    hook,
)
from captain_hook.bindings import tokens

from .common import gh_read

if TYPE_CHECKING:
    from captain_hook import Call, Command

POLLING_LOOPS = frozenset({"while", "until"})
LOOP_OPENERS = POLLING_LOOPS | {"for", "select"}
COMMAND_LEADERS = frozenset({"do", "then", "else", "elif", "if", "while", "until", "{", "!", "time"})
REDIRECTS = frozenset({"<<<", "<<-", "<<", ">>", "&>", "<", ">"})
TOKEN_VARIABLES = ("GH_TOKEN", "GITHUB_TOKEN")

LOOP_BLOCK = (
    "A shell `while` or `until` loop that calls gh polls GitHub until the quota every lander, proof "
    "and watch on this machine shares runs out. Wait with a Monitor on one script, or a tool's own "
    "blocking wait such as `drive wait <link>`, and read with one `gh api graphql` query per change."
)
TOKEN_BLOCK = (
    "GH_TOKEN or GITHUB_TOKEN on a gh read pins it to that token, off the GitHub App's quota and "
    "onto the token's own pool, which for a personal token is the gh user's. Drop the variable and "
    "let `ccx vcs gh` pick the token."
)


def calls_gh(command: Command) -> bool:
    match Path(command.unwrapped.executable).name, command.unwrapped.args:
        case "gh", _:
            return True
        case "ccx", ("vcs", "gh", *_):
            return True
        case _:
            return False


def loop_spans(script: str, offset: int) -> list[tuple[int, int]]:
    """The character span of each ``while`` or ``until`` loop ``script`` runs, shifted by ``offset``."""
    words = list(tokens(script))
    leading = [
        (index, token.raw)
        for index, token in enumerate(words)
        if not token.operator
        and (
            index == 0
            or (words[index - 1].operator and words[index - 1].raw not in REDIRECTS)
            or (not words[index - 1].operator and words[index - 1].raw in COMMAND_LEADERS)
        )
    ]
    spans = []
    for at, (index, word) in enumerate(leading):
        if word not in POLLING_LOOPS:
            continue
        depth, end = 0, len(script)
        for later, name in leading[at:]:
            depth += (name in LOOP_OPENERS) - (name == "done")
            if not depth:
                end = words[later].end
                break
        spans.append((words[index].start + offset, end + offset))
    return spans


def char_at(text: str, byte: int) -> int:
    return len(text.encode()[:byte].decode(errors="ignore"))


def polled(call: Call) -> bool:
    occ = call.occurrence
    if occ.command.span is None or not calls_gh(call.command):
        return False
    raw, start = occ.line.raw, char_at(occ.line.raw, occ.command.span[0])
    hosted = [] if occ.host is None else occ.host.command.args[-1:]
    spans = [
        *loop_spans(raw, 0),
        *(span for script in hosted if (found := raw.find(script)) >= 0 for span in loop_spans(script, found)),
    ]
    return any(begin <= start < end for begin, end in spans)


class GhInPollingLoop(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(polled(call) for call in evt.cmd.calls())


class GhReadOnPinnedToken(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(
            calls_gh(call.command)
            and any(name in TOKEN_VARIABLES for name, _ in call.command.env)
            and (gh_read(call.command) or Path(call.command.unwrapped.executable).name == "ccx")
            for call in evt.cmd.calls()
        )


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GhInPollingLoop()],
    skip_if=[Annotated("raw")],
    message=LOOP_BLOCK,
    block=True,
    tests={
        Input(command="until gh pr view 1; do sleep 5; done"): Block(pattern="Monitor on one script"),
        Input(command="until s=$(gh pr view 55687 --json state -q .state); [ $s = MERGED ]; do sleep 60; done"): Block(
            pattern="Monitor"
        ),
        Input(command="while true; do gh pr checks 12; sleep 60; done"): Block(pattern="Monitor"),
        Input(command="while true; do\n  gh run view 9 --json status\n  sleep 15\ndone"): Block(pattern="Monitor"),
        Input(command="while :; do /opt/homebrew/bin/gh run view 9; sleep 15; done"): Block(pattern="Monitor"),
        Input(command="while sleep 30; do ccx vcs gh -- pr checks 12; done"): Block(pattern="Monitor"),
        Input(command="echo start; while true; do for n in 1 2; do gh pr view $n; done; sleep 9; done"): Block(
            pattern="Monitor"
        ),
        Input(command="bash -c 'while true; do gh pr view 1; sleep 5; done'"): Block(pattern="Monitor"),
        Input(command="if true; then until gh pr checks 1; do sleep 9; done; fi"): Block(pattern="Monitor"),
        Input(command="while true; do gh api repos/o/r/pulls/1; sleep 5; done"): Block(pattern="Monitor"),
        Input(command="while true; do gh pr view 1; done # ccx:raw"): Allow(),
        Input(command="for n in 1 2 3; do gh pr view $n --json state; done"): Allow(),
        Input(command="gh pr view 1; while read -r line; do echo $line; done < prs.txt"): Allow(),
        Input(command="while read -r n; do echo $n; done < prs.txt && gh pr view 1"): Allow(),
        Input(command="echo 'while true; do gh pr view 1; done'"): Allow(),
        Input(command="gh pr view 1 --json title -q .title | grep while"): Allow(),
        Input(command="until make; do sleep 5; done"): Allow(),
        Input(command="ccx vcs gh -- pr view 1 --json state"): Allow(),
        Input(command="/opt/homebrew/Cellar/gh/2.103.3/bin/gh pr view 1 --json state"): Allow(),
        Input(command="gh.real pr view 1 --json state"): Allow(),
    },
)

hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GhReadOnPinnedToken()],
    skip_if=[Annotated("raw")],
    message=TOKEN_BLOCK,
    block=True,
    tests={
        Input(command="GH_TOKEN=x gh pr checks 12"): Block(pattern="Drop the variable"),
        Input(command="GITHUB_TOKEN=$(cat t) gh api repos/o/r/pulls/1"): Block(pattern="Drop the variable"),
        Input(command="GH_TOKEN=x ccx vcs gh -- pr view 1 --json state"): Block(pattern="Drop the variable"),
        Input(command="GH_TOKEN=x gh pr merge 12 --squash"): Allow(),
        Input(command="GH_TOKEN=x gh api graphql -f query='mutation { x }'"): Allow(),
        Input(command="GH_REPO=o/r gh pr view 1 --json state"): Allow(),
        Input(command="GH_TOKEN=x gh pr view 1 # ccx:raw"): Allow(),
    },
)
