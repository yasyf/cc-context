"""Tests for the ``git worktree remove`` block condition and the ``gh run watch`` nudge condition.

Run from the repo root against the captain-hook source env::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_vcs_guards.py

``TestWorktreeRemoveRealGit`` holds the block condition to git itself: a real shell runs each raw
command against a repository with linked worktrees, and every spelling that removes one must match
while every spelling the condition leaves alone must remove nothing.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import NamedTuple

import pytest
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from conftest import make_evt
from hooks.vcs_guards import GhRunWatchSingle, GitWorktreeRemove

MAIN_T = "/transcripts/main.jsonl"
LINKED = ("sib", "wt")
BASH = ("bash", "--noprofile", "--norc", "-c")


def bash_pre(command: str, *, cwd: Path | None = None) -> PreToolUseEvent:
    ctx = HookContext(session=SessionStore(None), transcript=None, settings=None)
    raw = {
        "tool_name": "Bash",
        "tool_input": {"command": command},
        "transcript_path": MAIN_T,
        "cwd": str(cwd) if cwd is not None else None,
    }
    return PreToolUseEvent(_raw=raw, ctx=ctx)


class Outcome(NamedTuple):
    """What one shell line did to the pool: the linked worktrees it removed, its success, its stdout."""

    removed: frozenset[str]
    ok: bool
    stdout: str


@dataclass(frozen=True)
class GitPool:
    """A real repository at ``root/main`` with the ``LINKED`` worktrees beside it."""

    root: Path
    env: dict[str, str]

    def git(self, *args: str) -> None:
        subprocess.run(["git", *args], cwd=self.root / "main", env=self.env, check=True, capture_output=True)

    def run(self, line: str, *, locked: str | None = None) -> Outcome:
        """Run ``line`` in bash from ``main`` and restore every worktree it removed."""
        if locked is not None:
            self.git("worktree", "lock", str(self.root / locked))
        proc = subprocess.run(
            [*BASH, line],
            cwd=self.root / "main",
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
        )
        removed = frozenset(name for name in LINKED if not (self.root / name / ".git").exists())
        for name in sorted(removed):
            self.git("worktree", "add", "--detach", str(self.root / name))
        if locked is not None and locked not in removed:
            self.git("worktree", "unlock", str(self.root / locked))
        return Outcome(removed, proc.returncode == 0, proc.stdout)


@pytest.fixture(scope="module")
def gitpool(tmp_path_factory: pytest.TempPathFactory) -> GitPool:
    """The pool every real-git case shares, run under an environment that reaches no other repository."""
    root = tmp_path_factory.mktemp("gitpool").resolve()
    for directory in ("main/sub", "home"):
        (root / directory).mkdir(parents=True)
    pool = GitPool(
        root,
        {
            "PATH": os.pathsep.join([str(Path(shutil.which("git")).parent), "/usr/bin", "/bin"]),
            "HOME": str(root / "home"),
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull,
            "GIT_CEILING_DIRECTORIES": str(root),
            "GIT_AUTHOR_NAME": "guard",
            "GIT_AUTHOR_EMAIL": "guard@example.com",
            "GIT_COMMITTER_NAME": "guard",
            "GIT_COMMITTER_EMAIL": "guard@example.com",
            "GIT_PAGER": "cat",
        },
    )
    pool.git("init", "-q", "-b", "main")
    pool.git("commit", "-q", "--allow-empty", "-m", "root")
    for name in LINKED:
        pool.git("worktree", "add", "--detach", str(root / name))
    return pool


def removes_worktree(command: str) -> bool:
    evt = make_evt(command)
    return GitWorktreeRemove().check_command_line(evt, evt.cmd.line)


class TestWorktreeRemoveBlocks:
    """Every ``git worktree remove`` that can delete a tree matches, whatever its operand or wrapper."""

    @pytest.mark.parametrize(
        "command",
        [
            "git worktree remove /pool/wt",
            "git worktree remove /pool/wt/",
            "git worktree remove -- /pool/wt",
            'git worktree remove "/pool/my wt"',
            "git worktree remove /pool/my\\ wt",
            "git worktree remove '/pool/$wt'",
            "git worktree remove ~/wt",
            'git worktree remove ~/"my wt"',
            "git worktree remove ~",
            "/usr/bin/git worktree remove /pool/wt",
            '"git" worktree remove /pool/wt',
            "git 'worktree' \"remove\" /pool/wt",
            "git worktree   remove    /pool/wt",
            "git worktree remove \\\n  /pool/wt",
            "git worktree remove --force /pool/wt",
            "git worktree remove -f /pool/wt",
            "git worktree remove /pool/wt --force",
            'git worktree remove "--force" /pool/wt',
            "git worktree remove ../wt",
            "git worktree remove ./sub",
            "git worktree remove .",
            "  git worktree remove ../wt",
            "cd /pool/main && git worktree remove ../wt",
            "cd /pool/main; git worktree remove ../wt",
            "(cd /pool/main) && git worktree remove ../wt",
            "f() { cd /pool/main && git worktree remove ../wt; }",
            "for d in a b; do git worktree remove ../wt; done",
            "(git worktree remove ../wt)",
            "git -C /repo worktree remove /pool/wt",
            "git -C sub worktree remove /pool/wt",
            "git -C -dash worktree remove /pool/wt",
            "git -C ~/repo worktree remove ~/wt",
            "git worktree remove /pool/wt && git worktree prune",
            "git worktree list; git worktree remove /pool/wt; git branch -D x",
            "yes | git worktree remove /pool/wt",
            "git worktree remove /pool/wt > rm.log 2>&1",
            "2>/dev/null git worktree remove /pool/wt",
            "git worktree remove /pool/wt &",
            "if git worktree remove /pool/wt; then echo gone; fi",
            "time git worktree remove /pool/wt",
            "git status\ngit worktree remove /pool/wt\ngit worktree list",
            "git worktree remove $WT",
            'git worktree remove "$HOME/wt"',
            "git worktree remove ${WT}/x",
            "git worktree remove /pool/$(date +%s)",
            "git worktree remove $'/pool/a\\tb'",
            "git worktree remove /pool/wt*",
            "git worktree remove /pool/{a,b}",
            "git worktree remove ~other/wt",
            "git worktree remove $(cat wt.txt)",
            "git worktree remove -f `cat wt.txt` /pool/wt",
            "git worktree remove /pool/wt <(echo x)",
            "git worktree remove wt",
            "git worktree remove .worktrees/wt",
            "git worktree remove -- -wt",
            "git worktree remove ''",
            "git worktree remove -f -f /pool/wt",
            "git worktree remove -ff /pool/wt",
            "git worktree remove --forc /pool/wt",
            "git worktree remove --expire now /pool/wt",
            "git worktree remove --no-force /pool/wt",
            "git worktree remove --expire now -h",
            "git worktree remove $WT -h",
            "git worktree remove",
            "git worktree remove --force",
            "git worktree remove /pool/a /pool/b",
            "sudo git worktree remove /pool/wt",
            "timeout 5 git worktree remove /pool/wt",
            "env GIT_DIR=/x/.git git worktree remove /pool/wt",
            "command git worktree remove /pool/wt",
            "nohup git worktree remove /pool/wt &",
            "echo /pool/wt | xargs git worktree remove",
            "builtin git worktree remove /pool/wt",
            "builtin command git worktree remove /pool/wt",
            "stdbuf -oL git worktree remove /pool/wt",
            "setsid git worktree remove /pool/wt",
            "caffeinate -i git worktree remove /pool/wt",
            "noglob git worktree remove /pool/wt",
            "flock /tmp/lock git worktree remove /pool/wt",
            "watch -n 5 git worktree remove /pool/wt",
            "rtk git worktree remove /pool/wt",
            "mise exec -- git worktree remove /pool/wt",
            "op run -- git worktree remove /pool/wt",
            "find . -name wt -exec git worktree remove {} \\;",
            "sudo stdbuf -oL git worktree remove /pool/wt",
            "GIT_DIR=/x/.git git worktree remove /pool/wt",
            "git -c core.x=y worktree remove /pool/wt",
            "git --git-dir=/x/.git worktree remove /pool/wt",
            "git --no-pager worktree remove /pool/wt",
            "git -C /a --no-pager worktree remove /pool/wt",
            "git -C /a -C b worktree remove /pool/wt",
            "git --attr-source HEAD worktree remove /pool/wt",
            "git --super-prefix sub/ worktree remove /pool/wt",
            "git --shallow-file /dev/null worktree remove /pool/wt",
            "git -C /repo --attr-source HEAD worktree remove /pool/wt",
            'git -C "$REPO" worktree remove /pool/wt',
            "git -C '' worktree remove /pool/wt",
            "bash -c 'git worktree remove /pool/wt'",
            'bash -c "git worktree remove /pool/wt"',
            'eval "git worktree remove /pool/wt"',
            "echo $(git worktree remove /pool/wt)",
            "git worktree remove 2>/dev/null /pool/wt",
            "git worktree 2>&1 remove /pool/wt",
            "git worktree remove /pool/w\\\nt",
            "git -C /re\\\npo worktree remove /pool/wt",
            "gi\\\nt worktree remove /pool/wt",
            "git work\\\ntree remove /pool/wt",
            "git worktree re\\\nmove /pool/wt",
        ],
    )
    def test_matches(self, command: str) -> None:
        assert removes_worktree(command) is True


class TestWorktreeRemoveUntouched:
    """Read-only worktree verbs, help requests, and words that only mention a removal never match."""

    @pytest.mark.parametrize(
        "command",
        [
            "git worktree list",
            "git worktree list --porcelain",
            "git worktree prune",
            "git worktree prune --dry-run",
            "git worktree add ../wt -b feature",
            "git worktree move /pool/a /pool/b",
            "git worktree lock /pool/wt",
            "git worktree unlock /pool/wt",
            "git worktree repair",
            "git -C /repo worktree list",
            "git worktree",
            "git --help worktree remove",
            "git --version worktree remove /pool/wt",
            "git --exec-path worktree remove /pool/wt",
            "git -C /repo --html-path worktree remove /pool/wt",
            "git rm -r docs",
            "git remote remove origin",
            "rm -rf /pool/wt",
            "echo git worktree remove /pool/wt",
            "git commit -m 'git worktree remove /pool/wt'",
            "ssh host 'git worktree remove /pool/wt'",
            "docker exec box sh -c 'git worktree remove /pool/wt'",
            "cat <<EOF\ngit worktree remove /pool/wt\nEOF",
            "stdbuf -oL echo git status",
            "find . -name git -newer worktree",
            "watch -n 5 git worktree list",
            "mise exec -- git worktree prune",
            "builtin cd /pool",
            "echo one \\\n  two",
            "jj workspace forget wt",
            "git worktree remove -h",
            "git worktree remove --help",
            "git worktree remove -h /pool/wt",
            "git worktree remove /pool/wt -h",
            "git -C /repo worktree remove --force /pool/wt --help",
        ],
    )
    def test_no_match(self, command: str) -> None:
        assert removes_worktree(command) is False


class TestWorktreeRemoveRealGit:
    """The block condition held to git itself, over a real repository with linked worktrees."""

    @pytest.mark.parametrize(
        "command, locked",
        [
            ("git -C {root}/main worktree remove {root}/sib", None),
            ("git -C {root}/main -C sub worktree remove {root}/sib", None),
            ("git -C '' worktree remove {root}/sib", None),
            ("git -c core.x=y worktree remove {root}/sib", None),
            ("git -c core.x=y -C {root}/main worktree remove {root}/sib", None),
            ("git --git-dir={root}/main/.git worktree remove {root}/sib", None),
            ("git --git-dir {root}/main/.git worktree remove {root}/sib", None),
            ("git --work-tree={root}/main worktree remove {root}/sib", None),
            ("git --work-tree {root}/main worktree remove {root}/sib", None),
            ("git --namespace=ns worktree remove {root}/sib", None),
            ("git --namespace ns worktree remove {root}/sib", None),
            ("git --config-env=core.x=HOME worktree remove {root}/sib", None),
            ("git --config-env core.x=HOME worktree remove {root}/sib", None),
            ("git --attr-source=HEAD worktree remove {root}/sib", None),
            ("git --attr-source HEAD worktree remove {root}/sib", None),
            ("git --shallow-file /dev/null worktree remove {root}/sib", None),
            ("git --exec-path=/nonexistent worktree remove {root}/sib", None),
            ("git -p worktree remove {root}/sib", None),
            ("git --paginate worktree remove {root}/sib", None),
            ("git -P worktree remove {root}/sib", None),
            ("git --no-pager worktree remove {root}/sib", None),
            ("git --no-replace-objects worktree remove {root}/sib", None),
            ("git --no-optional-locks worktree remove {root}/sib", None),
            ("git --literal-pathspecs worktree remove {root}/sib", None),
            ("git --glob-pathspecs worktree remove {root}/sib", None),
            ("git --noglob-pathspecs worktree remove {root}/sib", None),
            ("git --icase-pathspecs worktree remove {root}/sib", None),
            ("git worktree remove --force {root}/sib", None),
            ("git worktree remove -f {root}/sib", None),
            ("git worktree remove {root}/sib -f", None),
            ("git worktree remove --forc {root}/sib", None),
            ("git worktree remove --fo {root}/sib", None),
            ("git worktree remove --no-force {root}/sib", None),
            ("git worktree remove --end-of-options {root}/sib", None),
            ("git worktree remove -- {root}/sib", None),
            ("git worktree remove {root}/sib --", None),
            ("git worktree remove -f -f {root}/sib", "sib"),
            ("git worktree remove -ff {root}/sib", "sib"),
            ("git worktree remove --force {root}/sib --force", "sib"),
        ],
        ids=[
            "dir",
            "dir_twice",
            "dir_empty",
            "config",
            "config_then_dir",
            "git_dir_attached",
            "git_dir_separate",
            "work_tree_attached",
            "work_tree_separate",
            "namespace_attached",
            "namespace_separate",
            "config_env_attached",
            "config_env_separate",
            "attr_source_attached",
            "attr_source_separate",
            "shallow_file_separate",
            "exec_path_attached",
            "paginate_short",
            "paginate",
            "no_pager_short",
            "no_pager",
            "no_replace_objects",
            "no_optional_locks",
            "literal_pathspecs",
            "glob_pathspecs",
            "noglob_pathspecs",
            "icase_pathspecs",
            "force",
            "force_short",
            "force_after_path",
            "force_abbreviated",
            "force_abbreviated_shorter",
            "force_negated",
            "end_of_options",
            "dashdash",
            "dashdash_after_path",
            "locked_force_twice",
            "locked_force_bundled",
            "locked_force_split",
        ],
    )
    def test_every_spelling_git_removes_under_matches(self, gitpool: GitPool, command: str, locked: str | None) -> None:
        line = command.format(root=gitpool.root)
        assert gitpool.run(line, locked=locked) == Outcome(frozenset({"sib"}), True, "")
        assert removes_worktree(line) is True

    @pytest.mark.parametrize(
        "command",
        [
            "git --version worktree remove {root}/sib",
            "git -v worktree remove {root}/sib",
            "git --help worktree remove {root}/sib",
            "git -h worktree remove {root}/sib",
            "git --exec-path worktree remove {root}/sib",
            "git --html-path worktree remove {root}/sib",
            "git --man-path worktree remove {root}/sib",
            "git --info-path worktree remove {root}/sib",
            "git -C {root}/main --version worktree remove {root}/sib",
            "git --no-pager --exec-path worktree remove {root}/sib",
            "git worktree remove -h",
            "git worktree remove --help",
            "git worktree remove -h {root}/sib",
            "git worktree remove {root}/sib -h",
            "git worktree remove {root}/sib --help",
            "git worktree remove --force {root}/sib -h",
        ],
        ids=[
            "version",
            "version_short",
            "help",
            "help_short",
            "exec_path",
            "html_path",
            "man_path",
            "info_path",
            "version_after_dir",
            "exec_path_after_option",
            "remove_help_short",
            "remove_help",
            "remove_help_before_path",
            "remove_help_after_path",
            "remove_long_help_after_path",
            "remove_help_after_force",
        ],
    )
    def test_every_spelling_left_alone_removes_nothing(self, gitpool: GitPool, command: str) -> None:
        line = command.format(root=gitpool.root)
        assert removes_worktree(line) is False
        assert gitpool.run(line).removed == frozenset()


class TestGhRunWatchSingle:
    """The nudge matches a line that is exactly one ``gh run watch``."""

    def test_matches_single_gh_run_watch(self) -> None:
        evt = bash_pre("gh run watch 123 --exit-status")
        assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is True

    @pytest.mark.parametrize(
        "command",
        [
            "gh run watch 123 --exit-status | tee run.log",
            "cd repo && gh run watch 123",
            "gh run view 123 --log-failed",
            "gh pr list",
        ],
        ids=["piped", "chained", "run_view", "pr_list"],
    )
    def test_other_lines_do_not_match(self, command: str) -> None:
        evt = bash_pre(command)
        assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is False
