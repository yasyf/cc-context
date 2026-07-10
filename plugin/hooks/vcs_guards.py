"""VCS guards: steer the git/jj invocations that dump a full patch into context.

Four token-bombs, each steered at the compact ``ccx vcs`` equivalent. Where a
faithful, token-bounded rewrite exists it is applied in place (``permissionDecision:
allow`` + a note); where it does not, the command falls back to a hard block:

* a bare/range ``git diff`` / bare ``jj diff`` -> **rewrite** to ``ccx vcs diff``;
* a bare/single-ref ``git show`` dumping a whole patch -> **rewrite** to ``ccx vcs show``;
* ``git log -p`` on a single path -> **rewrite** to ``ccx vcs history``; ``jj log -p``
  and a path-less ``git log -p`` -> block.

Scoped, summarized, or plumbing variants (``git diff -- <path>``, ``jj diff --stat``,
``git show HEAD:file``, ``git log --oneline``) never fire the guard at all.

Beyond the token-bomb rewrites, a one-shot-per-session steering **nudge** points a manual
single-command ``gh run watch`` at ``ccx vcs ship`` — which folds the commit -> push -> watch
cycle into one call — while noting that manual ``gh run`` stays right for tag/release runs and
for resuming after a ship printed ``CI error``. It never blocks; the watch still runs.
"""

from __future__ import annotations

import re
import shlex
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    HookResult,
    Input,
    Rewrite,
    Tool,
    Warn,
    on,
    rewrite_command,
    session_state,
)
from pydantic import BaseModel

from .common import GIT_DIFF_SUMMARY_FLAGS, ccx_bin, is_single_command

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

# The one-shot steer shown when a session watches CI by hand instead of via `ccx vcs ship`.
GH_RUN_WATCH_NUDGE = (
    'Watching CI by hand? `ccx vcs ship -m "<msg>"` runs the whole commit → push → watch cycle in '
    "one call — a jj-aware commit, the push, then `gh run watch --exit-status` on every run for the "
    "pushed SHA, with a per-run report and budget-capped failure logs. Manual `gh run` still fits a "
    "tag/release run (no ship commit) and resuming a watch after a ship printed `CI error`. This "
    "command still runs."
)


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


def _gitdiff_args(cl: CommandLine) -> list[str] | None:
    """The trailing args for ``ccx vcs diff``, or ``None`` to fall back to the block.

    A bare ``git diff`` -> ``[]`` (working tree); a sole ``--cached``/``--staged`` ->
    ``["staged"]``; a sole positional ref (``HEAD~1``, ``A..B``, ``A...B``) -> ``[ref]``
    (the diff path translates git refs, so these resolve even in a jj repo). Any other
    flag (``-w``, ``-U3``, ``--word-diff``) or a second positional has no faithful
    ``ccx vcs diff`` form, so it blocks.
    """
    rest = list(cl.primary.args[1:])
    if not rest:
        return []
    if rest in (["--cached"], ["--staged"]):
        return ["staged"]
    if len(rest) == 1 and not rest[0].startswith("-"):
        return rest
    return None


def _gitdiff_to(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if not is_single_command(cl):
        return None
    args = _gitdiff_args(cl)
    if args is None or (ccx := ccx_bin()) is None:
        return None
    return " ".join([shlex.quote(ccx), "vcs", "diff", *(shlex.quote(a) for a in args)])


def _gitdiff_note(evt: BaseHookEvent) -> str:
    cl = evt.command_line
    dst = " ".join(["ccx", "vcs", "diff", *(_gitdiff_args(cl) or [])])
    return (
        f"Rewrote `{cl.raw}` → `{dst}`: same change set as a structural per-file summary, "
        "token-bounded. Need the raw hunks? Scope them: `git diff -- <path>`."
    )


rewrite_command(
    only_if=[GitDiffPager()],
    to=_gitdiff_to,
    block=(
        "BLOCKED: `git diff` without a pathspec dumps the full patch into context. "
        "Use `ccx vcs diff` for a compact summary (or mcp__cc-context__ccx_vcs_diff). "
        "Already know the file? `git diff -- <path>` / `git diff <ref> -- <path>` stays allowed, "
        "as do `git diff --stat`/`--numstat`/`--name-only`. Need the raw hunks? Scope them: `git diff -- <path>`."
    ),
    note=_gitdiff_note,
    tests={
        Input(command="git diff"): Rewrite(pattern="vcs diff"),
        Input(command="git diff --cached"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff --staged"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff HEAD~1"): Rewrite(pattern="vcs diff 'HEAD~1'"),  # shlex.quote guards `~`
        Input(command="git diff main..feature"): Rewrite(pattern="vcs diff main..feature"),
        Input(command="git diff main...feature"): Rewrite(pattern="vcs diff main...feature"),
        Input(command="git diff -w"): Block(pattern="ccx vcs diff"),  # flag with no ccx form → block
        Input(command="git diff -U3"): Block(),
        Input(command="git diff --word-diff"): Block(),
        Input(command="git diff HEAD~1 HEAD"): Block(),  # two positionals → block
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --numstat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff HEAD~1 -- src/x.go"): Allow(),
        Input(command="git status"): Allow(),
    },
)


def _jjdiff_to(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if not is_single_command(cl):
        return None
    # Only a bare `jj diff` maps. `jj diff -r REV` is REV-vs-parent while `ccx vcs diff
    # REV` is REV-vs-working — not equivalent — so any arg after `diff` falls to block.
    if list(cl.primary.args) != ["diff"] or (ccx := ccx_bin()) is None:
        return None
    return f"{shlex.quote(ccx)} vcs diff"


def _jjdiff_note(evt: BaseHookEvent) -> str:
    return (
        "Rewrote `jj diff` → `ccx vcs diff`: same working-copy changes as a jj-aware "
        "structural summary, token-bounded. Need the raw hunks? `jj diff <path>`."
    )


rewrite_command(
    only_if=[JjDiffPager()],
    to=_jjdiff_to,
    block=(
        "BLOCKED: `jj diff` without a pathspec dumps the full patch into context. "
        "Use `ccx vcs diff` for a compact, jj-aware summary (or mcp__cc-context__ccx_vcs_diff). "
        "Already know the file? `jj diff <path>` (or `git diff -- <path>`) gives the exact hunks, "
        "as do `jj diff --stat`/`--summary`/`-s`. A revset alone (`jj diff -r <rev>` / `--from`/`--to`) "
        "still needs a path to stay scoped."
    ),
    note=_jjdiff_note,
    tests={
        Input(command="jj diff"): Rewrite(pattern="vcs diff"),
        Input(command="jj diff -r @-"): Block(pattern="ccx vcs diff"),  # revset ≠ ref-vs-working → block
        Input(command="jj diff --from main --to @"): Block(),
        Input(command="jj diff --stat"): Allow(),
        Input(command="jj diff --summary"): Allow(),
        Input(command="jj diff -s"): Allow(),
        Input(command="jj diff internal/cli/root.go"): Allow(),
        Input(command="jj diff -r @- internal/cli/root.go"): Allow(),
        Input(command="jj status"): Allow(),
    },
)


def _gitshow_args(cl: CommandLine) -> list[str] | None:
    """The trailing args for ``ccx vcs show``, or ``None`` to fall back to the block.

    A bare ``git show`` -> ``[]`` (the last commit); a sole positional ref (``HEAD``,
    ``HEAD~1``, a branch/tag/sha) -> ``[ref]`` (``ccx vcs show`` translates git symbolic
    refs, so these resolve even in a jj repo). Any flag the condition did not already
    exclude, or a second positional, has no faithful ``ccx vcs show`` form, so it blocks.
    """
    rest = list(cl.primary.args[1:])
    if not rest:
        return []
    if len(rest) == 1 and not rest[0].startswith("-"):
        return rest
    return None


def _gitshow_to(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if not is_single_command(cl):
        return None
    args = _gitshow_args(cl)
    if args is None or (ccx := ccx_bin()) is None:
        return None
    return " ".join([shlex.quote(ccx), "vcs", "show", *(shlex.quote(a) for a in args)])


def _gitshow_note(evt: BaseHookEvent) -> str:
    cl = evt.command_line
    dst = " ".join(["ccx", "vcs", "show", *(_gitshow_args(cl) or [])])
    return (
        f"Rewrote `{cl.raw}` → `{dst}`: the commit message plus a structural per-file summary, "
        "token-bounded. Need one file? `git show <ref>:<path>` stays allowed."
    )


rewrite_command(
    only_if=[GitShowPager()],
    to=_gitshow_to,
    block=(
        "BLOCKED: `git show <ref>` dumps a full patch into context. "
        "Use `ccx vcs show <ref>` for the commit message plus a structural per-file summary. "
        "Extracting a blob (`git show <ref>:<path>`), a stat-only view (`git show --stat <ref>`), "
        "or plumbing (`git show --no-patch --format=%H <ref>` / `git show -s <ref>`) stay allowed."
    ),
    note=_gitshow_note,
    tests={
        Input(command="git show"): Rewrite(pattern="vcs show"),
        Input(command="git show HEAD"): Rewrite(pattern="vcs show HEAD"),
        Input(command="git show abc123"): Rewrite(pattern="vcs show abc123"),
        Input(command="git show HEAD~1"): Rewrite(pattern="vcs show 'HEAD~1'"),  # shlex.quote guards `~`
        Input(command="git show HEAD~1 HEAD"): Block(pattern="ccx vcs show"),  # two positionals → block
        Input(command="git show --format=%H"): Block(),  # flag with no ccx form → block
        Input(command="git show --stat HEAD"): Allow(),
        Input(command="git show HEAD:internal/cli/root.go"): Allow(),
        Input(command="git show --no-patch --format=%H HEAD"): Allow(),
        Input(command="git show -s HEAD"): Allow(),
        Input(command="git status"): Allow(),
    },
)


def _git_log_history(args: tuple[str, ...]) -> tuple[str, str | None] | None:
    """Map the args after ``git log`` to ``(path, count)`` for ``ccx vcs history``, or ``None``.

    Walks the patch/`--follow`/max-count flags (dropping `--follow`, capturing the
    count from ``-n N``, ``-N``, or ``--max-count[=]N``) and pins the single pathspec:
    the one token after ``--``, or a sole trailing positional that exists on disk. A
    revision positional, a missing path, more than one path, an unparsable count, or
    any unrecognized flag returns ``None`` (fall back to the block).
    """
    count: str | None = None
    positionals: list[str] = []
    path: str | None = None
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            rest = list(args[i + 1 :])
            # A revision before `--` (`git log -p HEAD -- f.go`) is outside the mapped
            # shape, and `history` takes exactly one path.
            if len(rest) != 1 or positionals:
                return None
            path = rest[0]
            break
        if a in ("-p", "--patch", "-u", "--follow"):
            i += 1
        elif a in ("-n", "--max-count"):
            if i + 1 >= n:
                return None
            count = args[i + 1]
            i += 2
        elif a.startswith("--max-count="):
            count = a.split("=", 1)[1]
            i += 1
        elif re.fullmatch(r"-\d+", a):
            count = a[1:]
            i += 1
        elif a.startswith("-"):
            return None  # unrecognized flag → outside the mapped shape
        else:
            positionals.append(a)
            i += 1
    if count is not None and not count.isdigit():
        return None
    if path is None:
        # No `--`: a sole trailing positional counts as the path only if it exists on
        # disk — a bare revision (`git log -p HEAD~5`) has no file to hand `history`.
        if len(positionals) != 1 or not Path(positionals[0]).exists():
            return None
        path = positionals[0]
    return path, count


def _logpatch_to(evt: BaseHookEvent) -> str | None:
    cl = evt.command_line
    if not is_single_command(cl):
        return None
    # `jj log -p` always blocks: `ccx vcs history` is git-backed, so a jj-native log
    # has no faithful mapping.
    if cl.q.runs("jj", "log"):
        return None
    parsed = _git_log_history(cl.primary.args[1:])
    if parsed is None or (ccx := ccx_bin()) is None:
        return None
    path, count = parsed
    out = [shlex.quote(ccx), "vcs", "history", shlex.quote(path)]
    if count is not None:
        out += ["-n", count]
    return " ".join(out)


def _logpatch_note(evt: BaseHookEvent) -> str:
    cl = evt.command_line
    path, count = _git_log_history(cl.primary.args[1:])
    dst = f"ccx vcs history {path}" + (f" -n {count}" if count is not None else "")
    dropped = " `--follow` dropped — history follows renames natively." if "--follow" in cl.primary.args else ""
    return (
        f"Rewrote `{cl.raw}` → `{dst}`: same commits as a per-commit sha + subject + "
        f"changed-symbols summary, token-bounded.{dropped}"
    )


rewrite_command(
    only_if=[LogPatchDump()],
    to=_logpatch_to,
    block=(
        "BLOCKED: `git log -p` / `jj log -p` dumps every commit's full patch into context. "
        "Use `ccx vcs history <path> -n N` for a per-commit sha + subject + changed-symbols summary. "
        "Metadata-only logs (`git log --oneline`, `git log --format=%h -- <path>`, `jj log`) stay allowed."
    ),
    note=_logpatch_note,
    tests={
        Input(command="git log -p -- internal/cli/root.go"): Rewrite(pattern="vcs history internal/cli/root.go"),
        Input(command="git log -p -n 5 -- internal/cli/root.go"): Rewrite(
            pattern="vcs history internal/cli/root.go -n 5"
        ),
        Input(command="git log -p -5 -- internal/cli/root.go"): Rewrite(pattern="-n 5"),  # `-N` count form
        Input(command="git log -p --max-count=5 -- internal/cli/root.go"): Rewrite(pattern="-n 5"),
        Input(command="git log -p --follow -- internal/cli/root.go"): Rewrite(  # --follow dropped
            pattern="vcs history internal/cli/root.go"
        ),
        Input(command="git log -p"): Block(pattern="ccx vcs history"),  # no path → block
        Input(command="git log --patch"): Block(),
        Input(command="git log -p HEAD -- internal/cli/root.go"): Block(),  # revision before `--` → block
        Input(command="jj log -p"): Block(),  # git-backed history → jj log never maps
        Input(command="jj log --patch"): Block(),
        Input(command="git log --oneline -5"): Allow(),
        Input(command="git log --format=%h -- internal/cli/root.go"): Allow(),
        Input(command="git log --pretty=oneline"): Allow(),
        Input(command="jj log"): Allow(),
        Input(command="jj log -r @-"): Allow(),
    },
)


@session_state
class GhRunWatchNudged(BaseModel):
    """One-shot latch: set once the ``gh run watch`` -> ``ccx vcs ship`` steer has fired this session.

    A dedicated model class (its own :class:`SessionStore` slot, keyed by the unique class name so
    it never collides with another hook file's state) records that the nudge has fired, so it is
    shown at most once per session — repeat ``gh run watch`` calls pass silently.
    """

    fired: bool = False


class GhRunWatchSingle(CustomCommandLineCondition):
    """Matches a single-command ``gh run watch …`` — the watch step ``ccx vcs ship`` folds in.

    Scoped to ``gh run watch`` alone: ``gh run list --json`` is already rewritten by
    ``json_guards``' ``wrap_json`` (touching it risks rule interplay), and ``gh run view`` is a
    legitimate failure drill-down — neither is matched. A piped or chained line (``gh run watch …
    | tee``, ``… && gh run watch``) is not a single command, so it falls through untouched.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return is_single_command(cl) and cl.q.runs("gh", "run", "watch")


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GhRunWatchSingle()],
    tests={
        Input(command="gh run watch 123 --exit-status"): Warn(pattern="ccx vcs ship"),
        Input(command="gh run watch 123 --exit-status | tee run.log"): Allow(),  # piped → not single
        Input(command="cd repo && gh run watch 123"): Allow(),  # chained → not single
        Input(command="gh run view 123 --log-failed"): Allow(),  # failure drill-down, not a watch
        Input(command="gh pr list"): Allow(),
    },
)
def steer_gh_run_watch_to_ship(evt: BaseHookEvent) -> HookResult | None:
    """Nudge a manual ``gh run watch`` toward ``ccx vcs ship``, at most once per session.

    ``ship`` folds the commit -> push -> watch-every-run cycle into one call; the nudge fires once
    (the :class:`GhRunWatchNudged` latch) and never blocks, so the watch the model asked for runs.
    """
    state = evt.ctx.s.load(GhRunWatchNudged)
    if state.fired:
        return None
    state.fired = True
    evt.ctx.s[GhRunWatchNudged].set(state)
    return evt.warn(GH_RUN_WATCH_NUDGE)
