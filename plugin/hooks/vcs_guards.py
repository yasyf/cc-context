"""VCS guards: block the git/jj invocations that dump a full patch into context.

Four token-bombs, each steered at the compact ``ccx vcs`` equivalent:

* a bare/range ``git diff`` or ``jj diff`` with no pathspec -> ``ccx vcs diff``;
* ``git show <ref>`` dumping a whole patch -> ``ccx vcs show <ref>``;
* ``git log -p`` / ``jj log -p`` dumping every commit's patch -> ``ccx vcs history``.

Scoped, summarized, or plumbing variants (``git diff -- <path>``, ``jj diff --stat``,
``git show HEAD:file``, ``git log --oneline``) stay allowed.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

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

if TYPE_CHECKING:
    from captain_hook import ParsedCommand

# `jj diff` is scoped (a positional path) or summarized by one of these; a bare
# diff with neither dumps the full patch.
JJ_DIFF_SUMMARY_FLAGS = ("--stat", "--summary", "-s")

# `jj diff` options that consume the following token as a value (revsets, template,
# tool, context, repo). Their values must not be mistaken for a positional pathspec.
JJ_DIFF_VALUE_FLAGS = (
    "-r",
    "--revisions",
    "--from",
    "--to",
    "-T",
    "--template",
    "--tool",
    "--context",
    "-R",
    "--repository",
)

# `git show` flags that suppress the patch, leaving only metadata — plumbing, not a bomb.
GIT_SHOW_SUPPRESS_FLAGS = ("--no-patch", "-s")

# The patch-emitting `git log` / `jj log` flags. Their presence turns a metadata log
# into a per-commit full-patch dump.
LOG_PATCH_FLAGS = ("-p", "--patch", "-u")


def _primary_has(cl: CommandLine, *tokens: str) -> bool:
    """Report whether the primary command's argv carries any of ``tokens`` verbatim.

    Exact whole-token membership on the primary command only, so a short flag like
    ``-p`` never substring-matches ``--pretty`` and never bleeds from a piped neighbor.
    """
    argv = cl.primary.argv
    return any(token in argv for token in tokens)


def _jj_diff_has_pathspec(cmd: ParsedCommand) -> bool:
    """Report whether ``jj diff`` carries a positional pathspec (revset flags aside).

    Walks the args after the ``diff`` subcommand, skipping value-taking flags and
    their values (:data:`JJ_DIFF_VALUE_FLAGS`), and returns ``True`` on the first
    positional token — an explicit ``--`` also opens a pathspec run.
    """
    args = list(cmd.args)
    i = 1 if args and args[0] == "diff" else 0
    while i < len(args):
        arg = args[i]
        if arg == "--":
            return i + 1 < len(args)
        if arg.startswith("--"):
            if "=" not in arg and arg in JJ_DIFF_VALUE_FLAGS:
                i += 2
                continue
            i += 1
            continue
        if arg.startswith("-") and len(arg) > 1:
            if arg[:2] in JJ_DIFF_VALUE_FLAGS:
                i += 1 if len(arg) > 2 else 2
                continue
            i += 1
            continue
        return True
    return False


def _has_blob_ref(cmd: ParsedCommand) -> bool:
    """Report whether any positional arg is a ``<ref>:<path>`` blob/tree selector."""
    return any(":" in a for a in cmd.args if not a.startswith("-"))


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


class JjDiffPager(CustomCommandLineCondition):
    """Matches a ``jj diff`` that is neither path-scoped nor a stat-only summary.

    `jj diff <path>` (and `jj diff -r <rev> <path>`) is scoped; `jj diff
    --stat`/`--summary`/`-s` are summaries. A bare `jj diff`, `jj diff -r <rev>`, or
    `jj diff --from A --to B` with no path dumps the full patch — that is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return (
            cl.q.runs("jj", "diff")
            and not _primary_has(cl, *JJ_DIFF_SUMMARY_FLAGS)
            and not _jj_diff_has_pathspec(cl.primary)
        )


class GitShowPager(CustomCommandLineCondition):
    """Matches a ``git show`` that dumps a full patch.

    A blob/tree extraction (`git show <ref>:<path>`), a stat-only view (`git show
    --stat <ref>`), and patch-suppressed plumbing (`git show --no-patch`/`-s`) are
    allowed; a bare `git show` or `git show <ref>` dumping the whole patch is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return (
            cl.q.runs("git", "show")
            and not _primary_has(cl, *GIT_DIFF_SUMMARY_FLAGS)
            and not _primary_has(cl, *GIT_SHOW_SUPPRESS_FLAGS)
            and not _has_blob_ref(cl.primary)
        )


class LogPatchDump(CustomCommandLineCondition):
    """Matches ``git log`` / ``jj log`` carrying a patch flag (`-p`/`--patch`/`-u`).

    Metadata-only logs (`git log --oneline`, `git log --format=%h -- <path>`, `jj
    log`) are allowed; adding `-p` turns the log into a per-commit full-patch dump,
    which is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        is_log = cl.q.runs("git", "log") or cl.q.runs("jj", "log")
        return is_log and _primary_has(cl, *LOG_PATCH_FLAGS)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitDiffPager()],
    message=(
        "BLOCKED: `git diff` without a pathspec dumps the full patch into context. "
        "Use `ccx vcs diff` for a compact summary (or mcp__cc-context__ccx_vcs_diff). "
        "Already know the file? `git diff -- <path>` / `git diff <ref> -- <path>` stays allowed, "
        "as do `git diff --stat`/`--numstat`/`--name-only`. Need the raw hunks? Scope them: `git diff -- <path>`."
    ),
    block=True,
    tests={
        Input(command="git diff"): Block(pattern="ccx vcs diff"),
        Input(command="git diff HEAD~1"): Block(),
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --numstat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff HEAD~1 -- src/x.go"): Allow(),
        Input(command="git status"): Allow(),
    },
)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), JjDiffPager()],
    message=(
        "BLOCKED: `jj diff` without a pathspec dumps the full patch into context. "
        "Use `ccx vcs diff` for a compact, jj-aware summary (or mcp__cc-context__ccx_vcs_diff). "
        "Already know the file? `jj diff <path>` (or `git diff -- <path>`) gives the exact hunks, "
        "as do `jj diff --stat`/`--summary`/`-s`. A revset alone (`jj diff -r <rev>` / `--from`/`--to`) "
        "still needs a path to stay scoped."
    ),
    block=True,
    tests={
        Input(command="jj diff"): Block(pattern="ccx vcs diff"),
        Input(command="jj diff -r @-"): Block(),
        Input(command="jj diff --from main --to @"): Block(),
        Input(command="jj diff --stat"): Allow(),
        Input(command="jj diff --summary"): Allow(),
        Input(command="jj diff -s"): Allow(),
        Input(command="jj diff internal/cli/root.go"): Allow(),
        Input(command="jj diff -r @- internal/cli/root.go"): Allow(),
        Input(command="jj status"): Allow(),
    },
)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitShowPager()],
    message=(
        "BLOCKED: `git show <ref>` dumps a full patch into context. "
        "Use `ccx vcs show <ref>` for the commit message plus a structural per-file summary. "
        "Extracting a blob (`git show <ref>:<path>`), a stat-only view (`git show --stat <ref>`), "
        "or plumbing (`git show --no-patch --format=%H <ref>` / `git show -s <ref>`) stay allowed."
    ),
    block=True,
    tests={
        Input(command="git show"): Block(pattern="ccx vcs show"),
        Input(command="git show HEAD"): Block(),
        Input(command="git show abc123"): Block(),
        Input(command="git show --stat HEAD"): Allow(),
        Input(command="git show HEAD:internal/cli/root.go"): Allow(),
        Input(command="git show --no-patch --format=%H HEAD"): Allow(),
        Input(command="git show -s HEAD"): Allow(),
        Input(command="git status"): Allow(),
    },
)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), LogPatchDump()],
    message=(
        "BLOCKED: `git log -p` / `jj log -p` dumps every commit's full patch into context. "
        "Use `ccx vcs history <path> -n N` for a per-commit sha + subject + changed-symbols summary. "
        "Metadata-only logs (`git log --oneline`, `git log --format=%h -- <path>`, `jj log`) stay allowed."
    ),
    block=True,
    tests={
        Input(command="git log -p"): Block(pattern="ccx vcs history"),
        Input(command="git log --patch"): Block(),
        Input(command="git log -p -- internal/cli/root.go"): Block(),
        Input(command="jj log -p"): Block(),
        Input(command="jj log --patch"): Block(),
        Input(command="git log --oneline -5"): Allow(),
        Input(command="git log --format=%h -- internal/cli/root.go"): Allow(),
        Input(command="git log --pretty=oneline"): Allow(),
        Input(command="jj log"): Allow(),
        Input(command="jj log -r @-"): Allow(),
    },
)
