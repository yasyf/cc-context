"""VCS guards: rewrite patch-dumping git/jj reads to ``ccx vcs``, block raw ``git worktree remove``, and
nudge a manual ``gh run watch`` toward ``ccx vcs ship``.

A trailing ``# ccx:raw`` comment, or ``CAPT_HOOK_CCX_RAW`` set for the session, runs any of these commands
as written.
"""

from __future__ import annotations

import os
import re
import shlex
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    Cmd,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    Input,
    Rewrite,
    Runs,
    Tool,
    Warn,
    ast_grep,
    hook,
    nudge,
    rewrite_command_occurrences,
)
from captain_hook.util import reqenv

from .common import ccx_bin, is_single_command, rewrote_note
from .search_common import resolve_operand

if TYPE_CHECKING:
    from captain_hook.cmd import Call
    from cc_transcript.command import Occurrence, Word

LOG_PATCH_FLAGS = frozenset({"-p", "--patch", "-u"})

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

RAW_MARKER = re.compile(r"#\s*ccx:raw\b")
RAW_ENV = "CAPT_HOOK_CCX_RAW"


def occurrence_can_rewrite(occ: Occurrence) -> bool:
    """Report whether an occurrence can be replaced without dropping shell semantics."""
    cmd = occ.command
    return (
        not occ.piped
        and not cmd.redirects
        and not cmd.env
        and all(word.value is not None for word in cmd.words)
        and not any(child.host is not None and child.host.index == occ.index for child in occ.line.occurrences)
    )


def rewritable_args(occ: Occurrence, executable: str) -> tuple[str, ...]:
    """The arguments of a rewritable ``executable`` occurrence, or ``()`` for any other occurrence."""
    return occ.command.args if occurrence_can_rewrite(occ) and occ.command.executable == executable else ()


def ccx_command(*args: str) -> str | None:
    return None if (ccx := ccx_bin()) is None else shlex.join([ccx, *args])


def payload_sources(cl: CommandLine) -> list[str]:
    """The dequoted words of ``cl`` that hold a nested command's payload, such as the script of ``bash -c``."""
    nested = [occ.command.span for occ in cl.occurrences if occ.nesting and occ.command.span is not None]
    return [
        word.value
        for occ in cl.occurrences
        for word in occ.command.words
        if word.value is not None
        and word.span is not None
        and any(word.span[0] <= start and end <= word.span[1] for start, end in nested)
    ]


class RawRequested(CustomCommandLineCondition):
    """Matches a ``# ccx:raw`` shell comment on the line, or ``CAPT_HOOK_CCX_RAW`` set for the session."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return bool(reqenv.getenv(RAW_ENV)) or any(
            RAW_MARKER.search(comment.text)
            for source in (evt.cmd.raw, *payload_sources(cl))
            for comment in ast_grep.comments(source, "bash")
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
    skip_if=[RawRequested()],
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
        Input(command="git worktree remove /tmp/wt # ccx:raw"): Allow(),
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


def gitdiff_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    match rewritable_args(occ, "git"):
        case ("diff",):
            return ccx_command("vcs", "diff")
        case ("diff", "--cached" | "--staged"):
            return ccx_command("vcs", "diff", "staged")
        case ("diff", ref) if not ref.startswith("-"):
            return ccx_command("vcs", "diff", ref)
        case _:
            return None


rewrite_command_occurrences(
    only_if=[Runs("git", "diff")],
    skip_if=[RawRequested()],
    to=gitdiff_to,
    note=rewrote_note(
        "ccx vcs diff", "the same change set as a per-file summary; scope it with `git diff -- <path>` for raw hunks"
    ),
    tests={
        Input(command="git diff"): Rewrite(pattern="vcs diff"),
        Input(command="git diff --cached"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff --staged"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff HEAD~1"): Rewrite(pattern="vcs diff 'HEAD~1'"),
        Input(command="git diff main..feature"): Rewrite(pattern="vcs diff main..feature"),
        Input(command="git diff main...feature"): Rewrite(pattern="vcs diff main...feature"),
        Input(command="git diff -w"): Allow(),
        Input(command="git diff -U3"): Allow(),
        Input(command="git diff --word-diff"): Allow(),
        Input(command="git diff HEAD~1 HEAD"): Allow(),
        Input(command="REV=HEAD git diff"): Allow(),
        Input(command="git diff $REV"): Allow(),
        Input(command="git diff $(git rev-parse HEAD)"): Allow(),
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --numstat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff HEAD~1 -- src/x.go"): Allow(),
        Input(command="git status"): Allow(),
        Input(command="git status && git diff"): Rewrite(pattern="git status && "),
        Input(command="git diff; echo done"): Rewrite(pattern="vcs diff; echo done"),
        Input(command="git diff > out.patch"): Allow(),
        Input(command="git diff | head"): Allow(),
        Input(command="git diff # ccx:raw"): Allow(),
    },
)


def jjdiff_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    return ccx_command("vcs", "diff") if rewritable_args(occ, "jj") == ("diff",) else None


rewrite_command_occurrences(
    only_if=[Runs("jj", "diff")],
    skip_if=[RawRequested()],
    to=jjdiff_to,
    note=rewrote_note(
        "ccx vcs diff", "the same working-copy changes as a per-file summary; scope it with `jj diff <path>` for raw hunks"
    ),
    tests={
        Input(command="jj diff"): Rewrite(pattern="vcs diff"),
        Input(command="jj diff -r @-"): Allow(),
        Input(command="jj diff --from main --to @"): Allow(),
        Input(command="REV=@- jj diff"): Allow(),
        Input(command="jj diff --stat"): Allow(),
        Input(command="jj diff --summary"): Allow(),
        Input(command="jj diff -s"): Allow(),
        Input(command="jj diff internal/cli/root.go"): Allow(),
        Input(command="jj diff -r @- internal/cli/root.go"): Allow(),
        Input(command="jj status"): Allow(),
        Input(command="jj diff # ccx:raw"): Allow(),
    },
)


def gitshow_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    match rewritable_args(occ, "git"):
        case ("show",):
            return ccx_command("vcs", "show")
        case ("show", ref) if not ref.startswith("-") and ":" not in ref:
            return ccx_command("vcs", "show", ref)
        case _:
            return None


rewrite_command_occurrences(
    only_if=[Runs("git", "show")],
    skip_if=[RawRequested()],
    to=gitshow_to,
    note=rewrote_note(
        "ccx vcs show", "the commit message plus a per-file summary; `git show <ref>:<path>` still prints one file"
    ),
    tests={
        Input(command="git show"): Rewrite(pattern="vcs show"),
        Input(command="git show HEAD"): Rewrite(pattern="vcs show HEAD"),
        Input(command="git show abc123"): Rewrite(pattern="vcs show abc123"),
        Input(command="git show HEAD~1"): Rewrite(pattern="vcs show 'HEAD~1'"),
        Input(command="git show HEAD~1 HEAD"): Allow(),
        Input(command="git show --format=%H"): Allow(),
        Input(command="REV=HEAD git show"): Allow(),
        Input(command="git show $REV"): Allow(),
        Input(command="git show $(git rev-parse HEAD)"): Allow(),
        Input(command="git show --stat HEAD"): Allow(),
        Input(command="git show HEAD:internal/cli/root.go"): Allow(),
        Input(command="git show --no-patch --format=%H HEAD"): Allow(),
        Input(command="git show -s HEAD"): Allow(),
        Input(command="git status"): Allow(),
        Input(command="git show # ccx:raw"): Allow(),
    },
)


def git_log_history(args: tuple[str, ...], *, cwd: Path | None) -> tuple[str, str | None] | None:
    """Map ``git log`` arguments to the ``(path, count)`` of ``ccx vcs history``, or ``None``.

    Patch flags and ``--follow`` drop, ``-n N``, ``-N``, and ``--max-count[=]N`` set the count, and the one
    pathspec is the token after ``--`` or a sole positional that exists relative to ``cwd``.
    """
    count: str | None = None
    positionals: list[str] = []
    path: str | None = None
    i = 0
    while i < len(args):
        arg = args[i]
        if arg == "--":
            rest = args[i + 1 :]
            if len(rest) != 1 or positionals:
                return None
            path = rest[0]
            break
        if arg in LOG_PATCH_FLAGS or arg == "--follow":
            i += 1
        elif arg in ("-n", "--max-count"):
            if i + 1 >= len(args):
                return None
            count = args[i + 1]
            i += 2
        elif arg.startswith("--max-count="):
            count = arg.removeprefix("--max-count=")
            i += 1
        elif arg.startswith("-") and arg[1:].isdigit():
            count = arg[1:]
            i += 1
        elif arg.startswith("-"):
            return None
        else:
            positionals.append(arg)
            i += 1
    if count is not None and not count.isdigit():
        return None
    if path is None:
        if len(positionals) != 1 or (resolved := resolve_operand(positionals[0], cwd)) is None:
            return None
        if not os.path.exists(resolved):
            return None
        path = positionals[0]
    return path, count


def logpatch_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    match rewritable_args(occ, "git"):
        case ("log", *rest) if not LOG_PATCH_FLAGS.isdisjoint(rest):
            parsed = git_log_history(tuple(rest), cwd=evt.cwd)
        case _:
            return None
    if parsed is None:
        return None
    path, count = parsed
    return ccx_command("vcs", "history", path, *(("-n", count) if count is not None else ()))


rewrite_command_occurrences(
    only_if=[Runs("git", "log")],
    skip_if=[RawRequested()],
    to=logpatch_to,
    note=rewrote_note("ccx vcs history", "a per-commit sha, subject, and changed-symbols summary that follows renames"),
    tests={
        Input(command="git log -p -- internal/cli/root.go"): Rewrite(pattern="vcs history internal/cli/root.go"),
        Input(command="git log -p -n 5 -- internal/cli/root.go"): Rewrite(
            pattern="vcs history internal/cli/root.go -n 5"
        ),
        Input(command="git log -p -5 -- internal/cli/root.go"): Rewrite(pattern="-n 5"),
        Input(command="git log -p --max-count=5 -- internal/cli/root.go"): Rewrite(pattern="-n 5"),
        Input(command="git log -p --follow -- internal/cli/root.go"): Rewrite(pattern="vcs history internal/cli/root.go"),
        Input(command="git log -p HEAD~5", cwd="."): Allow(),
        Input(command="git log -p AGENTS.md", cwd="."): Rewrite(pattern="vcs history AGENTS.md"),
        Input(command="git log -p -- whatever"): Rewrite(pattern="vcs history whatever"),
        Input(command="git log -p"): Allow(),
        Input(command="git log --patch"): Allow(),
        Input(command="git log -p HEAD -- internal/cli/root.go"): Allow(),
        Input(command="git log -p --author=me -- internal/cli/root.go"): Allow(),
        Input(command="git log -p -n x -- internal/cli/root.go"): Allow(),
        Input(command="jj log -p"): Allow(),
        Input(command="jj log --patch"): Allow(),
        Input(command="FILE=internal/cli/root.go git log -p $FILE"): Allow(),
        Input(command="git log -p $(find . -name root.go)"): Allow(),
        Input(command="git log --oneline -5"): Allow(),
        Input(command="git log --format=%h -- internal/cli/root.go"): Allow(),
        Input(command="git log --pretty=oneline"): Allow(),
        Input(command="git log -- internal/cli/root.go"): Allow(),
        Input(command="jj log"): Allow(),
        Input(command="jj log -r @-"): Allow(),
        Input(command="git log -p -- whatever # ccx:raw"): Allow(),
    },
)


class GhRunWatchSingle(CustomCommandLineCondition):
    """Matches a line that is one ``gh run watch`` command and nothing else."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return is_single_command(cl) and cl.q.runs("gh", "run", "watch")


nudge(
    GH_RUN_WATCH_NUDGE,
    only_if=[Tool("Bash"), GhRunWatchSingle()],
    skip_if=[RawRequested()],
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
