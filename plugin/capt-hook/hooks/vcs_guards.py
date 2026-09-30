"""VCS guards: steer the git/jj invocations that dump a full patch into context.

Four token-bombs are steered at compact ``ccx vcs`` equivalents when a faithful,
token-bounded rewrite exists:

* a bare/range ``git diff`` / bare ``jj diff`` -> ``ccx vcs diff``;
* a bare/single-ref ``git show`` dumping a whole patch -> ``ccx vcs show``;
* ``git log -p`` on a single path -> ``ccx vcs history``.

Unmapped forms, including ``jj log -p``, pass through unchanged.

A fifth rewrite is about correctness rather than tokens: a bare ``gt restack`` ->
``ccx vcs stack restack``, which fetches first and reaches the stack branches another
working copy holds — branches a bare ``gt restack`` cannot touch at all.

A sixth family moves a deletion rather than a read, and it never rewrites: every raw
``git [-C <dir>] worktree remove [-f|--force] <path>`` blocks, steering to
``ccx vcs worktree rm --path <absolute> [--force]`` — the removal whose tree the ccx cleanup daemon
deletes instead of git deleting it inline. A removal the hook can map gets the exact ccx command to
run in its place; one it cannot — a shell-computed path, an unmapped flag, a relative path whose
directory the line does not prove — gets the reason instead. It blocks rather than rewrites because
a rewrite reaches Claude Code as an approval of the rewritten command and is dropped whenever
another hook rewrites the same line first; a block is neither.

A trailing ``# ccx:raw`` comment, or ``CAPT_HOOK_CCX_RAW`` set for the session, runs any of these
commands as written: no rewrite, no block, and no nudge.

Scoped, summarized, or plumbing variants (``git diff -- <path>``, ``jj diff --stat``,
``git show HEAD:file``, ``git log --oneline``) never fire the guard at all.

Beyond the token-bomb rewrites, a one-shot-per-session steering **nudge** points a manual
single-command ``gh run watch`` at ``ccx vcs ship`` — which folds the commit -> push -> watch
cycle into one call — while noting that manual ``gh run`` stays right for tag/release runs and
for resuming after a ship printed ``CI error``. It never blocks; the watch still runs.
"""

from __future__ import annotations

import glob
import os
import re
import shlex
from pathlib import Path
from typing import TYPE_CHECKING, NamedTuple

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    Command,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    HookResult,
    Input,
    Rewrite,
    Target,
    Tool,
    Warn,
    on,
    rewrite_command_occurrences,
    session_state,
)
from captain_hook.util import reqenv
from pydantic import BaseModel

from .common import GIT_DIFF_SUMMARY_FLAGS, ccx_bin, is_single_command
from .search_common import Decline, resolve_operand

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence, Word

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

# The note disclosing the `gt restack` substitution. `gt sync` has no ccx twin, so it is never rewritten.
GT_RESTACK_NOTE = (
    "Rewrote `gt restack` → `ccx vcs stack restack`: the same restack, replayed from each branch's "
    "recorded base without gt sync. It fetches trunk first and drops every branch whose pull request "
    "landed, moving its children onto what it sat on; a conflict stops in a workspace for "
    "`ccx vcs stack continue`. `gt create` and `gt modify` are untouched. End a command "
    "with `# ccx:raw` to run it as written."
)

WORKTREE_RM_STEER = (
    "BLOCKED: raw `git worktree remove` deletes the tree inline. `ccx vcs worktree rm --path "
    "<absolute-path> [--force]` removes the same working copy and hands the tree's deletion to the "
    "ccx cleanup daemon where one runs."
)
WORKTREE_RM_SPLIT = (
    "- a line continuation splits a word of a `git worktree remove` on this line: write that command on one line."
)
WORKTREE_RM_ESCAPE = "End the command with `# ccx:raw` to run it as written."

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

# The one-shot steer shown when a session watches CI by hand instead of via `ccx vcs ship`.
GH_RUN_WATCH_NUDGE = (
    'Watching CI by hand? `ccx vcs ship -m "<msg>"` runs the whole commit → push → watch cycle in '
    "one call — a jj-aware commit, the push, then `gh run watch --exit-status` on every run for the "
    "pushed SHA, with a per-run report and budget-capped failure logs. Manual `gh run` still fits a "
    "tag/release run (no ship commit) and resuming a watch after a ship printed `CI error`. This "
    "command still runs."
)


class WorktreeRemoval(NamedTuple):
    """One ``git worktree remove`` mapped onto ``ccx vcs worktree rm``, as the shell words to emit.

    ``path`` and ``chdir`` arrive quoted for the shell, or with a leading ``~`` left live for the
    shell to expand as it would have. ``chdir`` is the ``-C`` directory: entered in a subshell, so
    ccx finds the repository where git would have and the caller's own cwd never moves, and through
    ``builtin cd``, so no function or alias named ``cd`` stands in for the ``chdir`` git performs.
    """

    path: str
    force: bool
    chdir: str | None


class RemovalWords(NamedTuple):
    """The words of one ``git worktree remove``: a wrapper ahead of git, git's global options, the rest."""

    wrapper: Word | None
    options: tuple[Word, ...]
    tail: tuple[Word, ...]


def command_has(cmd: Command, *tokens: str) -> bool:
    """Report whether a command's argv carries any of ``tokens`` verbatim.

    Exact whole-token membership keeps a short flag like ``-p`` from
    substring-matching ``--pretty``.
    """
    return any(token in cmd.argv for token in tokens)


def jj_diff_has_pathspec(cmd: Command) -> bool:
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


def has_blob_ref(cmd: Command) -> bool:
    """Report whether any positional arg is a ``<ref>:<path>`` blob/tree selector."""
    return any(":" in a for a in cmd.args if not a.startswith("-"))


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


def is_git_diff_pager(cmd: Command) -> bool:
    """Report whether ``cmd`` is an unscoped, non-summary ``git diff``."""
    return (
        cmd.executable == "git"
        and bool(cmd.args)
        and cmd.args[0] == "diff"
        and "--" not in cmd.args
        and not command_has(cmd, *GIT_DIFF_SUMMARY_FLAGS)
    )


def is_jj_diff_pager(cmd: Command) -> bool:
    """Report whether ``cmd`` is an unscoped, non-summary ``jj diff``."""
    return (
        cmd.executable == "jj"
        and bool(cmd.args)
        and cmd.args[0] == "diff"
        and not command_has(cmd, *JJ_DIFF_SUMMARY_FLAGS)
        and not jj_diff_has_pathspec(cmd)
    )


def is_git_show_pager(cmd: Command) -> bool:
    """Report whether ``cmd`` is a patch-emitting ``git show``."""
    return (
        cmd.executable == "git"
        and bool(cmd.args)
        and cmd.args[0] == "show"
        and not command_has(cmd, *GIT_DIFF_SUMMARY_FLAGS)
        and not command_has(cmd, *GIT_SHOW_SUPPRESS_FLAGS)
        and not has_blob_ref(cmd)
    )


def is_gt_restack(cmd: Command) -> bool:
    """Report whether ``cmd`` is a bare ``gt restack`` — the whole-stack restack ccx supersedes.

    Bare only. Every ``gt restack`` flag (``--branch``, ``-u``/``--upstack``, ``-d``/``--downstack``,
    ``-o``/``--only``) scopes the restack to part of the stack and ``ccx vcs stack restack`` takes no
    scoping flag, so a flagged restack has no faithful substitution and runs verbatim.
    """
    return cmd.executable == "gt" and list(cmd.args) == ["restack"]


def is_log_patch_dump(cmd: Command) -> bool:
    """Report whether ``cmd`` is a patch-emitting ``git log`` or ``jj log``."""
    return (
        cmd.executable in ("git", "jj")
        and bool(cmd.args)
        and cmd.args[0] == "log"
        and command_has(cmd, *LOG_PATCH_FLAGS)
    )


def head_name(word: Word) -> str:
    return Path(word.value).name if word.value else ""


def worktree_remove_words(cmd: Command) -> RemovalWords | None:
    """Split a ``git worktree remove`` command into its wrapper, global-option, and trailing words.

    ``None`` for every other command — ``worktree list``/``add``/``prune``, a non-git head, a verb the
    global-option arity walk does not reach — so only a positively identified removal ever blocks.
    Two tables widen what the SDK identifies, each because a miss lets the raw removal run. The
    wrapper set adds heads its unwrapping leaves in place (``builtin``, ``stdbuf``, ``flock``, ``find
    -exec``, ``mise exec --`` …): behind one of those, git is the first later word named git. The value-flag table adds
    ``--attr-source``, ``--super-prefix``, and ``--shallow-file``, since an option whose value is
    mistaken for the verb hides the removal. The walk stops at an option that makes git print and
    exit (``--version``, ``--exec-path``, ``--html-path`` …), which removes nothing whatever follows.
    A head outside both tables (``echo git worktree remove``) stays unidentified by design: its
    words are data.
    """
    words = cmd.unwrapped.words
    if not words:
        return None
    start = (
        next((i for i, word in enumerate(words) if i and head_name(word).casefold() == "git"), 0)
        if head_name(words[0]) in GIT_EXTRA_WRAPPERS
        else 0
    )
    if head_name(words[start]).casefold() != "git":
        return None
    rest = words[start + 1 :]
    taken = 0
    while (
        taken < len(rest)
        and (arg := rest[taken].value or rest[taken].raw).startswith("-")
        and arg not in GIT_INFO_OPTIONS
    ):
        taken += 2 if arg in GIT_GLOBAL_VALUE_FLAGS and taken + 1 < len(rest) else 1
    if [word.value for word in rest[taken : taken + 2]] != ["worktree", "remove"]:
        return None
    wrapped = start or len(words) != len(cmd.words)
    return RemovalWords(cmd.words[0] if wrapped else None, rest[:taken], rest[taken + 2 :])


def removal_count(line: CommandLine) -> int:
    return sum(worktree_remove_words(occ.command) is not None for occ in line.occurrences)


def split_removal(raw: str, line: CommandLine) -> bool:
    """Report whether a line continuation hides a removal by splitting one of its identifying words.

    ``gi\\<newline>t worktree remove`` runs as git, but the parser reads the broken word as two. The
    shell deletes a backslash-newline before it reads words, so the same line parsed without them
    shows what runs: a removal that appears only there is one the split hid.
    """
    return "\\\n" in raw and removal_count(CommandLine.parse(raw.replace("\\\n", ""))) > removal_count(line)


def home_anchored(word: Word) -> bool:
    """Report whether ``word`` is an unquoted ``~`` or ``~/…`` and carries no other expansion.

    The shell turns exactly that spelling into an absolute path under ``$HOME``. ``~user``, a quoted
    tilde, a glob, or a brace list each expand differently or not at all, and none of them qualifies.
    """
    return (
        word.expandable
        and word.value is not None
        and (word.raw == "~" or word.raw.startswith("~/"))
        and not glob.has_magic(word.value)
        and "{" not in word.value
    )


def anchored_word(word: Word) -> str | None:
    """The shell word re-emitting an operand that names one place whatever the cwd, or ``None``.

    An absolute literal is quoted verbatim, so ccx receives the very string git would have. A
    home-anchored word keeps its ``~`` live and quotes only the remainder, so the same shell expands
    it exactly as before. A relative, shell-computed, or otherwise expandable word has no such
    spelling.
    """
    if home_anchored(word):
        return "~/" + shlex.quote(rest) if (rest := word.value[2:]) else word.value
    if word.value is not None and not word.expandable and word.value.startswith("/"):
        return shlex.quote(word.value)
    return None


def shell_expanded(word: Word) -> Decline:
    return Decline(f"`{word.raw}` is expanded by the shell at run time")


def spans_plain_words(occ: Occurrence) -> bool:
    """Report whether the occurrence's source span is exactly its parsed words, each a plain literal.

    The parser lifts a bare command or process substitution out of the argv and keeps an environment
    prefix off it, so the words alone can under-report what the shell runs. Re-splitting the span
    text and demanding the same word list back catches every such shape, and holds the parser's
    dequoting to a second reading before any word is re-quoted. A backslash-newline between words
    is dropped first, as the shell drops it.
    """
    cmd = occ.command
    if cmd.span is None:
        return False
    try:
        split = shlex.split(occ.line.raw.encode()[slice(*cmd.span)].decode().replace("\\\n", ""))
    except ValueError:
        return False
    return split == [word.value for word in cmd.words]


def proven_cwd(occ: Occurrence, cwd: Path | None) -> Path | None:
    """The directory a relative operand of ``occ`` provably resolves against, or ``None``.

    A flattened occurrence list cannot show a ``cd`` scoped to a subshell, skipped by ``||``, negated
    by ``!``, held in a function body, or aimed at a directory an earlier command creates — and a
    removal resolved against the wrong directory names the wrong tree. Two lanes are trusted.
    Nothing but whitespace precedes the command: it runs first, in the event's own ``cwd``. Or the
    line opens with a plain ``cd <absolute>`` and a bare ``&&`` joins the command straight to it: it
    runs solely once that ``cd`` succeeded. The ``cd`` must open the line because an earlier command
    could set a trap, redefine ``cd``, or re-point a symlink the resolution reads; its operand
    carries no ``..`` because the shell's ``cd`` is logical there or physical by option, and the two
    land in different places past a symlink. Everything else returns ``None``.
    """
    line = occ.line.raw.encode()
    start = occ.command.span[0]
    if not line[:start].strip():
        return cwd
    cd = occ.line.occurrences[0]
    if occ.index != 1 or not spans_plain_words(cd) or line[: cd.command.span[0]].strip():
        return None
    match [word.value for word in cd.command.words]:
        case ["cd", target] if target.startswith("/") and ".." not in target.split("/"):
            join = line[cd.command.span[1] : start].replace(b"\\\n", b"")
        case _:
            return None
    return Path(target) if not cd.command.words[1].expandable and join.strip() == b"&&" else None


def removal_base(chdir: Word | None, occ: Occurrence, cwd: Path | None) -> Path | None:
    """The directory git resolves a removal's relative operand against, when the line proves one.

    No ``-C`` leaves the proven cwd itself; a ``-C`` hangs off it, an absolute one replacing it. Even
    an absolute ``-C`` waits on :func:`proven_cwd`: the operand is resolved through the filesystem
    now, and only a removal nothing else on the line precedes is sure to see the same symlinks when
    it runs. A home-anchored ``-C`` is known only to the shell.
    """
    if (proven := proven_cwd(occ, cwd)) is None or (chdir is not None and chdir.expandable):
        return None
    return proven if chdir is None else proven / chdir.value


def chdir_word(word: Word) -> str | Decline:
    """The ``builtin cd -P`` operand standing in for a ``-C`` directory, or why none is faithful.

    ``git -C`` is a bare ``chdir``. A relative directory gains a ``./`` so the shell's ``cd`` neither
    searches ``CDPATH`` nor reads a leading ``-`` as an option, and ``-P`` keeps ``..`` physical.
    """
    if (anchored := anchored_word(word)) is not None:
        return anchored
    if word.value is None or word.expandable:
        return shell_expanded(word)
    if not word.value:
        return Decline("an empty `-C` names no directory")
    return shlex.quote(f"./{word.value}")


def worktreerm_parse(occ: Occurrence, cwd: Path | None) -> WorktreeRemoval | Decline | None:
    """Map one occurrence onto ``ccx vcs worktree rm``: the removal, why it has no exact form, or ``None``.

    ``None`` means the command removes nothing — not a ``git worktree remove``, or one asking for
    help — and runs untouched. A :class:`Decline` names the first thing that breaks the proof:

    * a word the shell computes (``$VAR``, a substitution, a glob, a brace list, ``~user``), since
      the hook would be quoting a path it never saw;
    * a flag other than ``-f``/``--force``, or that flag twice — git's override for a locked working
      copy, which ccx never removes;
    * a wrapper, an environment prefix, or a global option other than one ``-C <dir>``, each of which
      changes what git acts on in a way the ccx form cannot carry;
    * a nested payload or substitution, whose cwd and quoting belong to another shell, or a command
      whose words a redirect interleaves, which leaves no span to re-read;
    * anything but exactly one path operand;
    * a bare relative name, which git matches against every registered working copy's path suffix
      before reading it as a path — a lookup only git's own registry can answer;
    * a ``./``- or ``../``-anchored path whose directory :func:`removal_base` cannot prove.

    What survives is an absolute or home-anchored operand passed through as written, or an anchored
    relative one resolved against the proven directory with its parent made physical.
    """
    if (words := worktree_remove_words(occ.command)) is None:
        return None
    operands: list[Word] = []
    forces = 0
    flags_open = True
    for word in words.tail:
        match word.value:
            case None:
                return shell_expanded(word)
            case "--" if flags_open:
                flags_open = False
            case "-h" | "--help" if flags_open:
                return None
            case "-f" | "--force" if flags_open:
                forces += 1
            case flag if flags_open and flag.startswith("-") and flag != "-":
                return Decline(f"`{flag}` is not a flag the hook maps — only `-f`/`--force` is")
            case _:
                operands.append(word)
    if occ.nesting:
        return Decline("it sits inside another command's payload or substitution")
    if words.wrapper is not None:
        return Decline(f"it runs under `{words.wrapper.raw}`, which the ccx form would drop")
    if occ.command.env:
        return Decline(f"the `{occ.command.env[0][0]}=` prefix changes the environment git runs in")
    if occ.command.span is None:
        return Decline("a redirect sits between its words, where the hook cannot re-read them — move it after the path")
    match words.options:
        case ():
            chdir = None
        case (option, directory) if option.value == "-C":
            chdir = directory
        case options:
            stray = options[2] if options[0].value == "-C" and len(options) > 2 else options[0]
            return Decline(f"`{stray.raw}` is a global git option beyond the one `-C <dir>` the hook maps")
    cd = None if chdir is None else chdir_word(chdir)
    if isinstance(cd, Decline):
        return cd
    if not spans_plain_words(occ):
        return Decline(
            "a substitution, a line continuation inside a word, or an unreadable quoting supplies part of it, "
            "not plain literal words"
        )
    match operands:
        case [operand]:
            pass
        case []:
            return Decline("it names no path operand")
        case _:
            return Decline("it names more than one path operand")
    if forces > 1:
        return Decline(
            "a repeated `--force` overrides a lock, and `ccx vcs worktree rm` never removes a locked working copy"
        )
    if (path := anchored_word(operand)) is not None:
        return WorktreeRemoval(path, forces == 1, cd)
    if operand.expandable:
        return shell_expanded(operand)
    if operand.value.split("/", 1)[0] not in (".", ".."):
        return Decline(
            f"`{operand.raw}` is neither absolute nor `./`-anchored, and git matches such a name against "
            "every working copy's path suffix before reading it as a path"
        )
    if (base := removal_base(chdir, occ, cwd)) is None:
        return Decline(f"`{operand.raw}` is relative, and this line does not prove the directory it resolves against")
    return WorktreeRemoval(
        shlex.quote(os.path.normpath(Target(operand.value, operand.raw, base).path)), forces == 1, cd
    )


def raw_marked(raw: str, line: CommandLine) -> bool:
    """Report whether ``raw`` carries a ``# ccx:raw`` marker as a comment rather than as an argument.

    The marker counts only outside every parsed word: inside a quoted string or a path it is data
    the command receives, and must not switch a guard off. A word that holds a nested command's
    whole span is that payload's container, not a literal, so a comment inside the payload counts.
    """
    commands = [(occ.index, occ.command.span) for occ in line.occurrences if occ.command.span is not None]
    literals = [
        word.span
        for occ in line.occurrences
        for word in occ.command.words
        if word.span is not None
        and not any(
            index != occ.index and word.span[0] <= start and end <= word.span[1] for index, (start, end) in commands
        )
    ]
    return any(
        not any(start <= offset < end for start, end in literals)
        for offset in (len(raw[: match.start()].encode()) for match in RAW_MARKER.finditer(raw))
    )


class RawRequested(CustomCommandLineCondition):
    """Matches a line that opts out of every guard here: a ``# ccx:raw`` comment on it, or
    ``CAPT_HOOK_CCX_RAW`` set for the session."""

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return bool(reqenv.getenv(RAW_ENV)) or raw_marked(evt.cmd.raw, cl)


class GitDiffPager(CustomCommandLineCondition):
    """Matches a ``git diff`` that is neither path-scoped nor a stat-only summary.

    `git diff -- <path>` and `git diff <ref> -- <path>` are scoped; `git diff
    --stat`/`--numstat`/`--name-only`/... are summaries. Everything else (`git diff`,
    `git diff HEAD~1`) dumps the full patch — that is what this matches.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occurrence_can_rewrite(occ) and is_git_diff_pager(occ.command) for occ in cl.occurrences)


class JjDiffPager(CustomCommandLineCondition):
    """Matches a ``jj diff`` that is neither path-scoped nor a stat-only summary.

    `jj diff <path>` (and `jj diff -r <rev> <path>`) is scoped; `jj diff
    --stat`/`--summary`/`-s` are summaries. A bare `jj diff`, `jj diff -r <rev>`, or
    `jj diff --from A --to B` with no path dumps the full patch — that is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occurrence_can_rewrite(occ) and is_jj_diff_pager(occ.command) for occ in cl.occurrences)


class GitShowPager(CustomCommandLineCondition):
    """Matches a ``git show`` that dumps a full patch.

    A blob/tree extraction (`git show <ref>:<path>`), a stat-only view (`git show
    --stat <ref>`), and patch-suppressed plumbing (`git show --no-patch`/`-s`) are
    allowed; a bare `git show` or `git show <ref>` dumping the whole patch is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occurrence_can_rewrite(occ) and is_git_show_pager(occ.command) for occ in cl.occurrences)


class LogPatchDump(CustomCommandLineCondition):
    """Matches ``git log`` / ``jj log`` carrying a patch flag (`-p`/`--patch`/`-u`).

    Metadata-only logs (`git log --oneline`, `git log --format=%h -- <path>`, `jj
    log`) are allowed; adding `-p` turns the log into a per-commit full-patch dump,
    which is the match.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occurrence_can_rewrite(occ) and is_log_patch_dump(occ.command) for occ in cl.occurrences)


class GtRestack(CustomCommandLineCondition):
    """Matches a bare ``gt restack`` — the restack ``ccx vcs stack restack`` strictly supersedes.

    ``gt create`` and ``gt modify`` are commit mechanics ccx itself routes through gt, and a
    flag-scoped restack has no ccx form; neither is matched.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occurrence_can_rewrite(occ) and is_gt_restack(occ.command) for occ in cl.occurrences)


class GitWorktreeRemove(CustomCommandLineCondition):
    """Matches a line carrying a ``git worktree remove`` anywhere — top level, wrapped, or nested.

    A cheap structural gate; :func:`steer_worktree_remove_to_ccx` is authoritative. ``git worktree
    list``/``add``/``prune`` and every other git verb stay outside the registration entirely.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return removal_count(cl) > 0 or split_removal(evt.cmd.raw, cl)


def gtrestack_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if not occurrence_can_rewrite(occ) or not is_gt_restack(occ.command):
        return None
    if (ccx := ccx_bin()) is None:
        return None
    return " ".join([shlex.quote(ccx), "vcs", "stack", "restack"])


rewrite_command_occurrences(
    only_if=[GtRestack()],
    skip_if=[RawRequested()],
    to=gtrestack_to,
    note=GT_RESTACK_NOTE,
    tests={
        Input(command="gt restack"): Rewrite(pattern="vcs stack restack"),
        Input(command="cd repo && gt restack"): Rewrite(pattern="cd repo && "),  # sibling stays verbatim
        Input(command="gt restack && gt submit"): Rewrite(pattern=" && gt submit"),  # submit untouched
        # The commit mechanics CLAUDE.md deliberately routes through gt — never rewritten:
        Input(command="gt create -m 'feat: x'"): Allow(),
        Input(command="gt modify"): Allow(),
        Input(command="gt modify -a"): Allow(),
        Input(command="gt submit"): Allow(),
        Input(command="gt sync"): Allow(),
        # Scoped restacks have no `ccx vcs stack restack` form (it always drives the whole stack):
        Input(command="gt restack --upstack"): Allow(),
        Input(command="gt restack -d"): Allow(),
        Input(command="gt restack --branch feat/x"): Allow(),
        Input(command="gt restack | tee restack.log"): Allow(),  # piped → post-processing
        Input(command="gt restack > out.txt"): Allow(),  # redirect → not rewritable in place
        Input(command="GT_DEBUG=1 gt restack"): Allow(),  # env prefix → runs verbatim
        Input(command="git restack"): Allow(),  # not gt
        Input(command="gt restack # ccx:raw"): Allow(),
    },
)


def worktreerm_command(removal: WorktreeRemoval) -> str:
    rm = " ".join(["ccx", "vcs", "worktree", "rm", "--path", removal.path, *(["--force"] if removal.force else [])])
    return rm if removal.chdir is None else f"(builtin cd -P {removal.chdir} && {rm})"


def worktreerm_verdict(occ: Occurrence, cwd: Path | None) -> str | None:
    match worktreerm_parse(occ, cwd):
        case None:
            return None
        case Decline(reason):
            return f"- `{occ.command.raw}` has no exact ccx form: {reason}."
        case removal:
            return f"- `{occ.command.raw}`: run `{worktreerm_command(removal)}` in its place."


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitWorktreeRemove()],
    skip_if=[RawRequested()],
    tests={
        Input(command="git worktree remove /tmp/wt"): Block(
            pattern="`git worktree remove /tmp/wt`: run `ccx vcs worktree rm --path /tmp/wt` in its place"
        ),
        Input(command="git worktree remove --force /tmp/wt"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt --force` in"
        ),
        Input(command="git worktree remove -f /tmp/wt"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt --force` in"
        ),
        Input(command="git worktree remove /tmp/wt --force"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt --force` in"
        ),
        Input(command="git worktree remove -- /tmp/wt"): Block(pattern="run `ccx vcs worktree rm --path /tmp/wt` in"),
        Input(command='git worktree remove "/tmp/my wt"'): Block(
            pattern="run `ccx vcs worktree rm --path '/tmp/my wt'` in"
        ),
        Input(command="git worktree remove /tmp/my\\ wt"): Block(
            pattern="run `ccx vcs worktree rm --path '/tmp/my wt'` in"
        ),
        Input(command="git worktree remove '/tmp/$wt'"): Block(pattern="rm --path '/tmp/\\$wt'` in its place"),
        Input(command="git worktree remove ~/wt"): Block(pattern="run `ccx vcs worktree rm --path ~/wt` in"),
        Input(command='git worktree remove ~/"my wt"'): Block(pattern="run `ccx vcs worktree rm --path ~/'my wt'` in"),
        Input(command="/usr/bin/git worktree remove /tmp/wt"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt` in"
        ),
        Input(command="git -C /repo worktree remove /tmp/wt"): Block(
            pattern="run `\\(builtin cd -P /repo && ccx vcs worktree rm --path /tmp/wt\\)` in its place"
        ),
        Input(command="git -C '/my repo' worktree remove -f /tmp/wt"): Block(
            pattern="run `\\(builtin cd -P '/my repo' && ccx vcs worktree rm --path /tmp/wt --force\\)` in"
        ),
        Input(command="git -C sub worktree remove /tmp/wt"): Block(pattern="`\\(builtin cd -P \\./sub && ccx vcs"),
        Input(command="git -C ~/repo worktree remove ~/wt"): Block(pattern="`\\(builtin cd -P ~/repo && ccx vcs"),
        Input(command="git status; git worktree remove /tmp/wt"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt` in"
        ),
        Input(command="git worktree remove /tmp/wt 2>&1 | tail -3"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt` in"
        ),
        Input(command="2>/dev/null git -C /repo worktree remove /tmp/wt"): Block(
            pattern="builtin cd -P /repo && ccx vcs"
        ),
        Input(command="git worktree remove \\\n  /tmp/wt"): Block(
            pattern="run `ccx vcs worktree rm --path /tmp/wt` in"
        ),
        Input(command="git worktree remove /tmp/a && git worktree remove -f /tmp/b"): Block(
            pattern=(
                "rm --path /tmp/a` in its place.\n"
                "- `git worktree remove -f /tmp/b`: run `ccx vcs worktree rm --path /tmp/b --force`"
            )
        ),
        Input(command="git worktree remove $WT"): Block(
            pattern="has no exact ccx form: `\\$WT` is expanded by the shell"
        ),
        Input(command='git worktree remove "$HOME/wt"'): Block(pattern="expanded by the shell"),
        Input(command="git worktree remove -f $WT"): Block(pattern="expanded by the shell"),
        Input(command="git worktree remove $(cat wt.txt)"): Block(pattern="substitution"),
        Input(command="git worktree remove -f `cat wt.txt` /tmp/wt"): Block(pattern="substitution"),
        Input(command="git worktree remove /tmp/wt <(echo x)"): Block(pattern="substitution"),
        Input(command="git worktree remove /tmp/wt*"): Block(pattern="expanded by the shell"),
        Input(command="git worktree remove /tmp/{a,b}"): Block(pattern="expanded by the shell"),
        Input(command="git worktree remove ~other/wt"): Block(pattern="expanded by the shell"),
        Input(command="for w in a b; do git worktree remove $w; done"): Block(pattern="expanded by the shell"),
        Input(command="git worktree remove wt"): Block(pattern="path suffix"),
        Input(command="git worktree remove .worktrees/wt"): Block(pattern="path suffix"),
        Input(command="git -C /repo worktree remove wt"): Block(pattern="path suffix"),
        Input(command="(cd /tmp); git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="pushd /tmp && git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="cd /tmp || git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="! cd /tmp && git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="git -C ~/repo worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="cd /tmp/link/.. && git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="git status; cd /tmp && git worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="git status; git -C /repo worktree remove ./wt"): Block(pattern="does not prove the directory"),
        Input(command="git worktree remove -f -f /tmp/wt"): Block(pattern="locked working copy"),
        Input(command="git worktree remove -ff /tmp/wt"): Block(pattern="`-ff` is not a flag"),
        Input(command="git worktree remove --expire now /tmp/wt"): Block(pattern="`--expire` is not a flag"),
        Input(command="git worktree remove"): Block(pattern="no path operand"),
        Input(command="git worktree remove /tmp/a /tmp/b"): Block(pattern="more than one path operand"),
        Input(command="sudo git worktree remove /tmp/wt"): Block(pattern="runs under `sudo`"),
        Input(command="timeout 5 git worktree remove /tmp/wt"): Block(pattern="runs under `timeout`"),
        Input(command="xargs git worktree remove"): Block(pattern="runs under `xargs`"),
        Input(command="builtin command git worktree remove /tmp/wt"): Block(pattern="runs under `builtin`"),
        Input(command="stdbuf -oL git worktree remove /tmp/wt"): Block(pattern="runs under `stdbuf`"),
        Input(command="noglob git worktree remove /tmp/wt"): Block(pattern="runs under `noglob`"),
        Input(command="flock /tmp/lock git worktree remove /tmp/wt"): Block(pattern="runs under `flock`"),
        Input(command="mise exec -- git worktree remove /tmp/wt"): Block(pattern="runs under `mise`"),
        Input(command="find . -name wt -exec git worktree remove {} \\;"): Block(pattern="runs under `find`"),
        Input(command="GIT_DIR=/x/.git git worktree remove /tmp/wt"): Block(pattern="`GIT_DIR=` prefix"),
        Input(command="git -c core.x=y worktree remove /tmp/wt"): Block(pattern="`-c` is a global git option"),
        Input(command="git --git-dir=/x/.git worktree remove /tmp/wt"): Block(pattern="global git option"),
        Input(command="git --attr-source HEAD worktree remove /tmp/wt"): Block(pattern="`--attr-source` is a global"),
        Input(command="git -C /a -C b worktree remove /tmp/wt"): Block(pattern="`-C` is a global git option"),
        Input(command='git -C "$REPO" worktree remove /tmp/wt'): Block(pattern="expanded by the shell"),
        Input(command="bash -c 'git worktree remove /tmp/wt'"): Block(pattern="payload or substitution"),
        Input(command="echo $(git worktree remove /tmp/wt)"): Block(pattern="payload or substitution"),
        Input(command="git worktree remove 2>/dev/null /tmp/wt"): Block(pattern="redirect sits between its words"),
        Input(command="gi\\\nt worktree remove /tmp/wt"): Block(pattern="line continuation splits a word"),
        Input(command="printf '%s\\n' '# ccx:raw'; git worktree remove /tmp/wt"): Block(
            pattern="rm --path /tmp/wt` in"
        ),
        Input(command="git worktree remove '/tmp/#ccx:raw'"): Block(pattern="rm --path '/tmp/#ccx:raw'` in"),
        Input(command="git worktree remove /tmp/wt # ccx:raw"): Allow(),
        Input(command="git worktree remove --force /tmp/wt # ccx:raw"): Allow(),
        Input(command="git worktree remove $WT # ccx:raw"): Allow(),
        Input(command="sudo git worktree remove /tmp/wt # ccx:raw"): Allow(),
        Input(command="git worktree remove -h"): Allow(),
        Input(command="git worktree remove --help"): Allow(),
        Input(command="git --help worktree remove"): Allow(),
        Input(command="git --version worktree remove /tmp/wt"): Allow(),
        Input(command="git --exec-path worktree remove /tmp/wt"): Allow(),
        Input(command="git worktree list"): Allow(),
        Input(command="git worktree list --porcelain"): Allow(),
        Input(command="git worktree prune"): Allow(),
        Input(command="git worktree add ../wt -b feature"): Allow(),
        Input(command="git worktree move /tmp/a /tmp/b"): Allow(),
        Input(command="git worktree lock /tmp/wt"): Allow(),
        Input(command="git rm -r docs"): Allow(),
        Input(command="rm -rf /tmp/wt"): Allow(),
        Input(command="echo git worktree remove /tmp/wt"): Allow(),
        Input(command="find . -name git -newer worktree"): Allow(),
        Input(command="watch -n 5 git worktree list"): Allow(),
        Input(command="git commit -m 'git worktree remove /tmp/wt'"): Allow(),
        Input(command="ssh host 'git worktree remove /tmp/wt'"): Allow(),
    },
)
def steer_worktree_remove_to_ccx(evt: BaseHookEvent) -> HookResult | None:
    """Block every raw ``git worktree remove`` on the line, one verdict per removal.

    Running one raw deletes the tree inline, the very thing the ccx form exists to avoid, so nothing
    falls through: a removal :func:`worktreerm_parse` maps is answered with the exact ccx command to
    run in its place, one it declines with the reason, and one a line continuation hides with the
    instruction to unsplit it. Nothing is rewritten. A rewrite would reach Claude Code as an
    approval of a deletion the caller never saw, and the dispatcher keeps only the first rewrite of
    a line, so another hook's rewrite of a sibling command would let the raw removal run; a block
    outranks every rewrite. Lines with no removal, help requests, and the read-only worktree verbs
    return ``None``.
    """
    verdicts = [
        verdict for occ in evt.cmd.line.occurrences if (verdict := worktreerm_verdict(occ, evt.cwd)) is not None
    ]
    if split_removal(evt.cmd.raw, evt.cmd.line):
        verdicts.append(WORKTREE_RM_SPLIT)
    return evt.block("\n".join([WORKTREE_RM_STEER, *verdicts, WORKTREE_RM_ESCAPE])) if verdicts else None


def gitdiff_args(cmd: Command) -> list[str] | None:
    """The trailing args for ``ccx vcs diff``, or ``None`` to leave the command unchanged.

    A bare ``git diff`` -> ``[]`` (working tree); a sole ``--cached``/``--staged`` ->
    ``["staged"]``; a sole positional ref (``HEAD~1``, ``A..B``, ``A...B``) -> ``[ref]``
    (the diff path translates git refs, so these resolve even in a jj repo). Any other
    flag (``-w``, ``-U3``, ``--word-diff``) or a second positional has no faithful
    ``ccx vcs diff`` form.
    """
    rest = list(cmd.args[1:])
    if not rest:
        return []
    if rest in (["--cached"], ["--staged"]):
        return ["staged"]
    if len(rest) == 1 and not rest[0].startswith("-"):
        return rest
    return None


def gitdiff_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    cmd = occ.command
    if not occurrence_can_rewrite(occ) or not is_git_diff_pager(cmd):
        return None
    args = gitdiff_args(cmd)
    if args is None or (ccx := ccx_bin()) is None:
        return None
    return " ".join([shlex.quote(ccx), "vcs", "diff", *(shlex.quote(a) for a in args)])


def gitdiff_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    rewrites = []
    for occ, _ in pairs:
        dst = " ".join(["ccx", "vcs", "diff", *(shlex.quote(a) for a in gitdiff_args(occ.command) or [])])
        rewrites.append(f"`{occ.command.raw}` → `{dst}`")
    return (
        f"Rewrote {', '.join(rewrites)}: same change set as a structural per-file summary, "
        "token-bounded. Need the raw hunks? Scope them: `git diff -- <path>`."
    )


rewrite_command_occurrences(
    only_if=[GitDiffPager()],
    skip_if=[RawRequested()],
    to=gitdiff_to,
    note=gitdiff_note,
    tests={
        Input(command="git diff"): Rewrite(pattern="vcs diff"),
        Input(command="git diff --cached"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff --staged"): Rewrite(pattern="vcs diff staged"),
        Input(command="git diff HEAD~1"): Rewrite(pattern="vcs diff 'HEAD~1'"),  # shlex.quote guards `~`
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
    cmd = occ.command
    if not occurrence_can_rewrite(occ) or not is_jj_diff_pager(cmd):
        return None
    # `jj diff -r REV` is REV-vs-parent while `ccx vcs diff REV` is REV-vs-working.
    if list(cmd.args) != ["diff"] or (ccx := ccx_bin()) is None:
        return None
    return f"{shlex.quote(ccx)} vcs diff"


def jjdiff_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    rewrites = ", ".join(f"`{occ.command.raw}` → `ccx vcs diff`" for occ, _ in pairs)
    return (
        f"Rewrote {rewrites}: same working-copy changes as a jj-aware "
        "structural summary, token-bounded. Need the raw hunks? `jj diff <path>`."
    )


rewrite_command_occurrences(
    only_if=[JjDiffPager()],
    skip_if=[RawRequested()],
    to=jjdiff_to,
    note=jjdiff_note,
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
    },
)


def gitshow_args(cmd: Command) -> list[str] | None:
    """The trailing args for ``ccx vcs show``, or ``None`` to leave the command unchanged.

    A bare ``git show`` -> ``[]`` (the last commit); a sole positional ref (``HEAD``,
    ``HEAD~1``, a branch/tag/sha) -> ``[ref]`` (``ccx vcs show`` translates git symbolic
    refs, so these resolve even in a jj repo). Any flag the condition did not already
    exclude, or a second positional, has no faithful ``ccx vcs show`` form.
    """
    rest = list(cmd.args[1:])
    if not rest:
        return []
    if len(rest) == 1 and not rest[0].startswith("-"):
        return rest
    return None


def gitshow_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    cmd = occ.command
    if not occurrence_can_rewrite(occ) or not is_git_show_pager(cmd):
        return None
    args = gitshow_args(cmd)
    if args is None or (ccx := ccx_bin()) is None:
        return None
    return " ".join([shlex.quote(ccx), "vcs", "show", *(shlex.quote(a) for a in args)])


def gitshow_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    rewrites = []
    for occ, _ in pairs:
        dst = " ".join(["ccx", "vcs", "show", *(shlex.quote(a) for a in gitshow_args(occ.command) or [])])
        rewrites.append(f"`{occ.command.raw}` → `{dst}`")
    return (
        f"Rewrote {', '.join(rewrites)}: the commit message plus a structural per-file summary, "
        "token-bounded. Need one file? `git show <ref>:<path>` stays allowed."
    )


rewrite_command_occurrences(
    only_if=[GitShowPager()],
    skip_if=[RawRequested()],
    to=gitshow_to,
    note=gitshow_note,
    tests={
        Input(command="git show"): Rewrite(pattern="vcs show"),
        Input(command="git show HEAD"): Rewrite(pattern="vcs show HEAD"),
        Input(command="git show abc123"): Rewrite(pattern="vcs show abc123"),
        Input(command="git show HEAD~1"): Rewrite(pattern="vcs show 'HEAD~1'"),  # shlex.quote guards `~`
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
    },
)


def git_log_history(cmd: Command, *, cwd: Path | None) -> tuple[str, str | None] | None:
    """Map a ``git log`` command to ``(path, count)`` for ``ccx vcs history``, or ``None``.

    Walks the patch/`--follow`/max-count flags (dropping `--follow`, capturing the
    count from ``-n N``, ``-N``, or ``--max-count[=]N``) and pins the single pathspec:
    the one token after ``--``, or a sole trailing positional that exists relative
    to ``cwd``. No path, more than one path, an unparsable count, an unstattable
    positional, or any unrecognized flag returns ``None``.
    """
    args = cmd.args[1:]
    count: str | None = None
    positionals: list[str] = []
    path: str | None = None
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            rest = list(args[i + 1 :])
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
            return None
        else:
            positionals.append(a)
            i += 1
    if count is not None and not count.isdigit():
        return None
    if path is None:
        if len(positionals) != 1 or (resolved := resolve_operand(positionals[0], cwd)) is None:
            return None
        try:
            exists = resolved.exists()
        except OSError:
            return None
        if not exists:
            return None
        path = positionals[0]
    return path, count


def logpatch_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    cmd = occ.command
    if not occurrence_can_rewrite(occ) or not is_log_patch_dump(cmd):
        return None
    if cmd.executable == "jj":
        return None
    parsed = git_log_history(cmd, cwd=evt.cwd)
    if parsed is None or (ccx := ccx_bin()) is None:
        return None
    path, count = parsed
    out = [shlex.quote(ccx), "vcs", "history", shlex.quote(path)]
    if count is not None:
        out += ["-n", count]
    return " ".join(out)


def logpatch_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    rewrites = []
    for occ, _ in pairs:
        path, count = git_log_history(occ.command, cwd=evt.cwd)
        dst = f"ccx vcs history {shlex.quote(path)}" + (f" -n {count}" if count is not None else "")
        rewrites.append(f"`{occ.command.raw}` → `{dst}`")
    dropped = (
        " `--follow` dropped — history follows renames natively."
        if any("--follow" in occ.command.args for occ, _ in pairs)
        else ""
    )
    return (
        f"Rewrote {', '.join(rewrites)}: same commits as a per-commit sha + subject + "
        f"changed-symbols summary, token-bounded.{dropped}"
    )


rewrite_command_occurrences(
    only_if=[LogPatchDump()],
    skip_if=[RawRequested()],
    to=logpatch_to,
    note=logpatch_note,
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
    skip_if=[RawRequested()],
    tests={
        Input(command="gh run watch 123 --exit-status"): Warn(pattern="ccx vcs ship"),
        Input(command="gh run watch 123 --exit-status | tee run.log"): Allow(),  # piped → not single
        Input(command="cd repo && gh run watch 123"): Allow(),  # chained → not single
        Input(command="gh run view 123 --log-failed"): Allow(),  # failure drill-down, not a watch
        Input(command="gh pr list"): Allow(),
        Input(command="gh run watch 123 --exit-status # ccx:raw"): Allow(),
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
