"""VCS guards: point patch-dumping git/jj reads at ``ccx vcs`` once, block raw ``git worktree remove``, and
nudge a manual ``gh run watch`` toward ``ccx vcs ship``.

A trailing ``# ccx:raw`` comment, or ``CAPT_HOOK_CCX_RAW`` set for the session, runs any of these commands
as written.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    BaseHookEvent,
    Block,
    Cmd,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    HookResult,
    Input,
    PreToolUseEvent,
    Tool,
    Warn,
    hook,
    nudge,
    on,
)

from .common import GIT_DIFF_SUMMARY_FLAGS, is_single_command, output_reaches_terminal

if TYPE_CHECKING:
    from captain_hook.cmd import Call
    from cc_transcript.command import Occurrence, Word

LOG_PATCH_FLAGS = frozenset({"-p", "--patch", "-u"})

SUMMARY_FLAGS = frozenset({*GIT_DIFF_SUMMARY_FLAGS, "-s", "--summary", "--no-patch", "--quiet"})

PATCH_DUMP_NOTE = (
    "`git diff`, `jj diff`, `git show`, and `git log -p` print whole patches into context. "
    "`ccx vcs diff`, `ccx vcs show`, and `ccx vcs history <path>` give bounded per-file summaries."
)

WORKTREE_RM_BLOCK = (
    "Raw `git worktree remove` deletes the tree inline. Run `ccx vcs worktree rm --path <absolute> [--force]` "
    "instead, or end the command with `# ccx:raw` to run it as written."
)

GH_RUN_WATCH_NUDGE = (
    "Watch CI through the ship cycle, not a manual `gh run watch`. Run `ccx vcs ship -m <msg>` to commit, "
    "push, and watch every run in one call."
)

GIT_GLOBAL_VALUE_FLAGS = (
    "-C",
    "-c",
    "--git-dir",
    "--work-tree",
    "--namespace",
    "--config-env",
    "--attr-source",
    "--super-prefix",
    "--shallow-file",
)
GIT_INFO_OPTIONS = ("-h", "--help", "-v", "--version", "--exec-path", "--html-path", "--man-path", "--info-path")
GIT_EXTRA_WRAPPERS = frozenset(
    {
        "builtin",
        "stdbuf",
        "setsid",
        "caffeinate",
        "noglob",
        "nocorrect",
        "ionice",
        "chrt",
        "taskset",
        "arch",
        "unbuffer",
        "flock",
        "watch",
        "chronic",
        "script",
        "strace",
        "ltrace",
        "parallel",
        "find",
        "rtk",
        "op",
        "mise",
        "direnv",
    }
)


def word_name(word: Word) -> str:
    return Path(word.value).name.casefold() if word.value else ""


def git_words(call: Call) -> tuple[Word, ...]:
    """The words after ``git`` in ``call``, looking past a wrapper the SDK leaves in place, or ``()``."""
    words = call.command.words
    if call.name in GIT_EXTRA_WRAPPERS:
        words = words[next((i for i, word in enumerate(words) if i and word_name(word) == "git"), 0) :]
    return words[1:] if words and word_name(words[0]) == "git" else ()


def worktree_remove_tail(words: tuple[Word, ...]) -> tuple[Word, ...] | None:
    """The words after ``worktree remove`` once git's global options are skipped, or ``None``."""
    taken = 0
    while (
        taken < len(words)
        and (arg := words[taken].value or words[taken].raw).startswith("-")
        and arg not in GIT_INFO_OPTIONS
    ):
        taken += 2 if arg in GIT_GLOBAL_VALUE_FLAGS and taken + 1 < len(words) else 1
    if [word.value for word in words[taken : taken + 2]] != ["worktree", "remove"]:
        return None
    return words[taken + 2 :]


def asks_for_help(tail: tuple[Word, ...]) -> bool:
    for word in tail:
        match word.value:
            case "-h" | "--help":
                return True
            case "-f" | "--force":
                continue
            case None | "--":
                return False
            case flag if flag.startswith("-") and flag != "-":
                return False
    return False


def removes_worktree(call: Call) -> bool:
    return (tail := worktree_remove_tail(git_words(call))) is not None and not asks_for_help(tail)


def continuation_joined(cmd: Cmd) -> tuple[Cmd, ...]:
    """``cmd`` re-parsed with its backslash-newlines deleted, as the shell deletes them before reading words."""
    if "\\\n" not in cmd.raw or (joined := Cmd.parse(cmd.raw.replace("\\\n", ""))) is None:
        return ()
    return (joined,)


class GitWorktreeRemove(CustomCommandLineCondition):
    """Matches a ``git worktree remove`` anywhere on the line that is not a help request."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(removes_worktree(call) for cmd in (evt.cmd, *continuation_joined(evt.cmd)) for call in cmd.calls())


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitWorktreeRemove()],
    skip_if=[Annotated("raw")],
    message=WORKTREE_RM_BLOCK,
    block=True,
    tests={
        Input(command="git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm --path"),
        Input(command="git worktree remove --force /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove ~/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="/usr/bin/git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git -C /repo worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git status; git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove /tmp/wt 2>&1 | tail -3"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove \\\n  /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove $WT"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove --expire now /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove"): Block(pattern="ccx vcs worktree rm"),
        Input(command="sudo git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="xargs git worktree remove"): Block(pattern="ccx vcs worktree rm"),
        Input(command="stdbuf -oL git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="mise exec -- git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="find . -name wt -exec git worktree remove {} \\;"): Block(pattern="ccx vcs worktree rm"),
        Input(command="GIT_DIR=/x/.git git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git -c core.x=y worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git --attr-source HEAD worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="bash -c 'git worktree remove /tmp/wt'"): Block(pattern="ccx vcs worktree rm"),
        Input(command="echo $(git worktree remove /tmp/wt)"): Block(pattern="ccx vcs worktree rm"),
        Input(command="gi\\\nt worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="printf '%s\\n' '# ccx:raw'; git worktree remove /tmp/wt"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove '/tmp/#ccx:raw'"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git commit -m 'note # ccx:raw' && git worktree remove /tmp/wt"): Block(pattern="worktree rm"),
        Input(command="git worktree remove /tmp/wt # ccx:rawish"): Block(pattern="ccx vcs worktree rm"),
        Input(command="git worktree remove /tmp/wt # ccx:raw"): Allow(),
        Input(command="bash -c 'git worktree remove /tmp/wt # ccx:raw'"): Allow(),
        Input(command="sudo git worktree remove /tmp/wt # ccx:raw"): Allow(),
        Input(command="git worktree remove -h"): Allow(),
        Input(command="git worktree remove --help"): Allow(),
        Input(command="git --help worktree remove"): Allow(),
        Input(command="git --version worktree remove /tmp/wt"): Allow(),
        Input(command="git --exec-path worktree remove /tmp/wt"): Allow(),
        Input(command="git worktree list"): Allow(),
        Input(command="git worktree prune"): Allow(),
        Input(command="git worktree add ../wt -b feature"): Allow(),
        Input(command="git rm -r docs"): Allow(),
        Input(command="echo git worktree remove /tmp/wt"): Allow(),
        Input(command="find . -name git -newer worktree"): Allow(),
        Input(command="watch -n 5 git worktree list"): Allow(),
        Input(command="git commit -m 'git worktree remove /tmp/wt'"): Allow(),
        Input(command="ssh host 'git worktree remove /tmp/wt'"): Allow(),
    },
)


class PatchDump(CustomCommandLineCondition):
    """Matches a ``git diff``, ``jj diff``, ``git show``, or ``git log -p`` whose whole patch reaches context."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(dumps_patch(occ) for occ in cl.occurrences)


def dumps_patch(occ: Occurrence) -> bool:
    cmd = occ.command
    if not output_reaches_terminal(occ) or not SUMMARY_FLAGS.isdisjoint(cmd.args):
        return False
    match cmd.executable, cmd.args:
        case "git", ("diff", *rest):
            return "--" not in rest
        case "jj", ("diff", *rest):
            return all(arg.startswith("-") for arg in rest)
        case "git", ("show", *rest):
            return not any(":" in arg for arg in rest if not arg.startswith("-"))
        case "git", ("log", *rest):
            return not LOG_PATCH_FLAGS.isdisjoint(rest)
        case _:
            return False


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), PatchDump()],
    skip_if=[Annotated("raw")],
    max_fires=1,
    tests={
        Input(command="git diff"): Warn(pattern="ccx vcs diff"),
        Input(command="git diff --cached"): Warn(),
        Input(command="git diff HEAD~1"): Warn(),
        Input(command="git diff main...feature"): Warn(),
        Input(command="jj diff"): Warn(pattern="ccx vcs diff"),
        Input(command="git show"): Warn(pattern="ccx vcs show"),
        Input(command="git show HEAD~1"): Warn(),
        Input(command="git log -p -- internal/cli/root.go"): Warn(pattern="ccx vcs history"),
        Input(command="git log --patch"): Warn(),
        Input(command="git status && git diff"): Warn(),
        Input(command="git diff; echo '# ccx:raw'"): Warn(),
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff > out.patch"): Allow(),
        Input(command="git diff | head"): Allow(),
        Input(command="git diff # ccx:raw"): Allow(),
        Input(command="jj diff --stat"): Allow(),
        Input(command="jj diff -s"): Allow(),
        Input(command="jj diff --summary"): Allow(),
        Input(command="jj diff internal/cli/root.go"): Allow(),
        Input(command="git diff --quiet"): Allow(),
        Input(command="git show --format=format:%h HEAD"): Warn(),
        Input(command="git show HEAD:internal/cli/root.go"): Allow(),
        Input(command="git show -s HEAD"): Allow(),
        Input(command="git show --no-patch --format=%H HEAD"): Allow(),
        Input(command="git show --stat HEAD"): Allow(),
        Input(command="git log --oneline -5"): Allow(),
        Input(command="git log -- internal/cli/root.go"): Allow(),
        Input(command="git status"): Allow(),
    },
)
def steer_patch_dump(evt: PreToolUseEvent) -> HookResult:
    return evt.context(PATCH_DUMP_NOTE)


class GhRunWatchSingle(CustomCommandLineCondition):
    """Matches a line that is one ``gh run watch`` command and nothing else."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return is_single_command(cl) and cl.q.runs("gh", "run", "watch")


nudge(
    GH_RUN_WATCH_NUDGE,
    only_if=[Tool("Bash"), GhRunWatchSingle()],
    skip_if=[Annotated("raw")],
    events=Event.PreToolUse,
    max_fires=1,
    tests={
        Input(command="gh run watch 123 --exit-status"): Warn(pattern="ccx vcs ship"),
        Input(command="gh run watch 123 --exit-status | tee run.log"): Allow(),
        Input(command="cd repo && gh run watch 123"): Allow(),
        Input(command="gh run view 123 --log-failed"): Allow(),
        Input(command="gh pr list"): Allow(),
        Input(command="gh run watch 123 --exit-status # ccx:raw"): Allow(),
    },
)
