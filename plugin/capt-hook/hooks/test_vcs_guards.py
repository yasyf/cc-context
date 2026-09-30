"""Tests for the ``git log -p`` -> ``ccx vcs history`` rewrite, the ``git worktree remove`` ->
``ccx vcs worktree rm --path`` block, and the ``gh run watch`` -> ``ccx vcs ship`` nudge.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_vcs_guards.py

The rewrite is driven through ``logpatch_to`` — the registered ``to=`` builder of the
``LogPatchDump`` family — so these exercise the public rewrite surface, never the
``git_log_history`` helper it delegates to. ``ccx_bin`` is pinned to a fixed path so the
emitted command is deterministic. A shape with no faithful ``ccx vcs history`` form makes
``logpatch_to`` return ``None``, leaving the raw command unchanged.

The no-``--`` branch pins a pathspec only when a sole trailing positional exists
relative to the event cwd; the explicit ``--`` branch remains disk-independent.

The worktree guard is driven through ``steer_worktree_remove_to_ccx``, the registered handler, so
each case asserts the block it returns: the ccx command it names for a removal it maps, or the
reason for one it declines. The shapes that resolve a relative operand classify it against the
filesystem and the event cwd, so they run here over a ``tmp_path`` tree rather than in inline
``tests={}``. ``TestWorktreeRemoveRealGit`` then holds those verdicts to git itself: a real
repository with linked worktrees under one temp root, where a real shell runs each raw command and
then the command the guard names in its place — ``ccx`` there being a stand-in that hands ``--path``
to ``git worktree remove`` — and both must remove the same working copy.

The nudge's one-shot latch is stateful across two invocations — a shape the declarative
``tests={}`` harness cannot express — so its fire/silence behavior is driven end to end here
against a real ``SessionStore`` backed by a temp session dir.
"""

from __future__ import annotations

import os
import re
import shlex
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import NamedTuple

import pytest
from captain_hook import Action, HookResult
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from conftest import make_evt
from hooks import vcs_guards
from hooks.vcs_guards import (
    GhRunWatchNudged,
    GhRunWatchSingle,
    GitWorktreeRemove,
    RawRequested,
    steer_gh_run_watch_to_ship,
    steer_worktree_remove_to_ccx,
)

MAIN_T = "/transcripts/main.jsonl"
FAKE_CCX = "/fake/ccx"
UNREADABLE = (
    "a substitution, a line continuation inside a word, or an unreadable quoting supplies part of it, "
    "not plain literal words"
)
CCX_STAND_IN = """#!/bin/sh
[ "$1 $2 $3 $4" = "vcs worktree rm --path" ] || exit 64
case "$5" in /*) ;; *) exit 65 ;; esac
case "$#:$6" in
  5:) exec git worktree remove "$5" ;;
  6:--force) exec git worktree remove --force "$5" ;;
esac
exit 64
"""
LINKED = ("sib", "wt", "my wt", "home/lane", "main/wt", "main/sub/wt", "pool/solo", "pool/a/feat", "pool/b/feat")
BASH = ("bash", "--noprofile", "--norc", "-c")
SHELLS = [
    pytest.param(BASH, id="bash"),
    pytest.param(
        ("zsh", "-f", "-c"),
        id="zsh",
        marks=pytest.mark.skipif(shutil.which("zsh") is None, reason="zsh is not installed"),
    ),
]


def bash_pre(
    command: str,
    session_dir: Path | None = None,
    *,
    cwd: Path | None = None,
) -> PreToolUseEvent:
    """A ``PreToolUseEvent`` for a Bash ``command``, backed by ``session_dir`` for the one-shot latch."""
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
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


class Removal(NamedTuple):
    """One raw removal to run from ``cwd``, the linked worktree git removes for it, and the state it needs."""

    cwd: str
    command: str
    removed: str | None
    before: str = ""
    after: str = ""
    dirty: str | None = None
    locked: str | None = None


@dataclass(frozen=True)
class GitPool:
    """A real repository at ``root/main`` with the ``LINKED`` worktrees spread around ``root``."""

    root: Path
    env: dict[str, str]

    def git(self, *args: str) -> None:
        subprocess.run(["git", *args], cwd=self.root / "main", env=self.env, check=True, capture_output=True)

    def run(
        self, shell: tuple[str, ...], line: str, cwd: str, *, dirty: str | None = None, locked: str | None = None
    ) -> Outcome:
        """Run ``line`` in ``shell`` from ``cwd`` and restore every worktree it removed."""
        if dirty is not None:
            (self.root / dirty / "untracked").write_text("")
        if locked is not None:
            self.git("worktree", "lock", str(self.root / locked))
        proc = subprocess.run(
            [*shell, line],
            cwd=self.root / cwd,
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
        )
        removed = frozenset(name for name in LINKED if not (self.root / name / ".git").exists())
        for name in sorted(removed):
            self.git("worktree", "add", "--detach", str(self.root / name))
        if dirty is not None and dirty not in removed:
            (self.root / dirty / "untracked").unlink()
        if locked is not None and locked not in removed:
            self.git("worktree", "unlock", str(self.root / locked))
        return Outcome(removed, proc.returncode == 0, proc.stdout)


@pytest.fixture
def repo(tmp_path: Path) -> Path:
    (tmp_path / "f.go").write_text("package main\n")
    (tmp_path / "g.go").write_text("package main\n")
    (tmp_path / "pkg").mkdir()
    return tmp_path


@pytest.fixture
def pin_ccx(monkeypatch: pytest.MonkeyPatch) -> None:
    """Pin ``ccx_bin`` so every rewrite emits a deterministic string."""
    monkeypatch.setattr(vcs_guards, "ccx_bin", lambda: FAKE_CCX)


@pytest.fixture(scope="module")
def gitpool(tmp_path_factory: pytest.TempPathFactory) -> GitPool:
    """The pool every real-git case shares, run under an environment that reaches no other repository.

    ``main/solo`` is a plain directory beside the linked ``pool/solo``, ``link`` a symlink to
    ``main/deep`` with a linked worktree at both ``wt`` and ``main/wt`` — the logical and the physical
    reading of ``link/../wt`` — and ``CDPATH`` points at a decoy holding its own ``sub``. ``ccx`` on
    ``PATH`` is a stand-in that hands ``--path`` to ``git worktree remove`` from its own cwd.
    """
    root = tmp_path_factory.mktemp("gitpool").resolve()
    for directory in ("main/sub", "main/deep", "main/solo", "main/-dash", "outside", "home", "bin", "decoy/sub"):
        (root / directory).mkdir(parents=True)
    (root / "link").symlink_to(root / "main" / "deep")
    (root / "bin" / "ccx").write_text(CCX_STAND_IN)
    (root / "bin" / "ccx").chmod(0o755)
    pool = GitPool(
        root,
        {
            "PATH": os.pathsep.join([str(root / "bin"), str(Path(shutil.which("git")).parent), "/usr/bin", "/bin"]),
            "HOME": str(root / "home"),
            "CDPATH": str(root / "decoy"),
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


@pytest.fixture
def pool(tmp_path: Path) -> Path:
    """A resolved tree holding a checkout with a subdirectory, plus a symlink into a second tree."""
    root = tmp_path.resolve()
    (root / "main" / "sub").mkdir(parents=True)
    (root / "real" / "deep").mkdir(parents=True)
    (root / "link").symlink_to(root / "real" / "deep")
    return root


def worktreerm_steer(command: str, cwd: Path | None = None) -> HookResult | None:
    """The registered handler's verdict for ``command``: the block it returns, or ``None``."""
    return steer_worktree_remove_to_ccx(make_evt(command, cwd))


def steered(result: HookResult | None) -> list[str]:
    """Every ccx command a block tells the caller to run, in line order."""
    assert isinstance(result, HookResult)
    assert result.action is Action.block
    return re.findall(r"run `(.+?)` in its place", result.message)


def declined(result: HookResult | None) -> list[str]:
    """Every reason a block gives for a removal it could not map, in line order."""
    assert isinstance(result, HookResult)
    assert result.action is Action.block
    return re.findall(r"has no exact ccx form: (.+?)\.(?:\n|$)", result.message)


def ccx_rm(path: Path | str) -> str:
    return f"ccx vcs worktree rm --path {shlex.quote(str(path))}"


def call_logpatch_to(command: str, *, cwd: Path | None = None) -> str | None:
    """Drive ``vcs_guards.logpatch_to`` on ``command``'s primary occurrence — the public rewrite surface."""
    evt = bash_pre(command, cwd=cwd)
    return vcs_guards.logpatch_to(evt, evt.cmd.line.occurrences[-1])


class TestLogPatchRewriteDashDash:
    """The ``--`` branch of the ``git log -p`` -> ``ccx vcs history`` rewrite (disk-independent path)."""

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("git log -p -- internal/cli/root.go", f"{FAKE_CCX} vcs history internal/cli/root.go"),
            # `--` is an explicit pathspec marker — the token is taken verbatim, existence aside.
            ("git log -p -- ghost/never-there.go", f"{FAKE_CCX} vcs history ghost/never-there.go"),
            ("git log -p -n 5 -- f.go", f"{FAKE_CCX} vcs history f.go -n 5"),
            ("git log -p -5 -- f.go", f"{FAKE_CCX} vcs history f.go -n 5"),  # glued -N count form
            ("git log -p --max-count=5 -- f.go", f"{FAKE_CCX} vcs history f.go -n 5"),
            ("git log -p --max-count 5 -- f.go", f"{FAKE_CCX} vcs history f.go -n 5"),
            ("git log -p --follow -- f.go", f"{FAKE_CCX} vcs history f.go"),  # --follow dropped
            ("git log --patch -- f.go", f"{FAKE_CCX} vcs history f.go"),  # --patch synonym
            ("git log -u -- f.go", f"{FAKE_CCX} vcs history f.go"),  # -u synonym
        ],
        ids=[
            "plain_path",
            "path_need_not_exist",
            "count_two_token",
            "count_glued_short",
            "count_max_count_equals",
            "count_max_count_two_token",
            "follow_dropped",
            "patch_synonym",
            "u_synonym",
        ],
    )
    def test_rewrites(self, pin_ccx: None, command: str, expected: str) -> None:
        assert call_logpatch_to(command) == expected


class TestLogPatchRewriteResiduals:
    """Shapes with no faithful rewrite return ``None`` and leave the raw command unchanged."""

    @pytest.mark.parametrize(
        "command",
        [
            "git log -p",  # no path
            "git log -p -- a.go b.go",  # two paths after --
            "git log -p --",  # empty --
            "git log -p HEAD -- f.go",  # revision before --
            "git log -p --author=me -- f.go",  # unrecognized flag
            "git log -p -n x -- f.go",  # non-numeric count
            "git log -p -n",  # dangling count flag
        ],
        ids=[
            "no_path",
            "two_paths_after_dashdash",
            "empty_dashdash",
            "revision_before_dashdash",
            "unrecognized_flag",
            "non_numeric_count",
            "dangling_count_flag",
        ],
    )
    def test_allows_raw(self, pin_ccx: None, command: str) -> None:
        assert call_logpatch_to(command) is None


class TestLogPatchRewritePositional:
    """The no-``--`` branch maps only a sole positional existing relative to the event cwd."""

    def test_sole_existing_file(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p f.go", cwd=repo) == f"{FAKE_CCX} vcs history f.go"

    def test_sole_existing_directory(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p pkg", cwd=repo) == f"{FAKE_CCX} vcs history pkg"

    def test_sole_existing_positional_with_count(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p -3 f.go", cwd=repo) == f"{FAKE_CCX} vcs history f.go -n 3"

    @pytest.mark.parametrize("command", ["git log -p HEAD~5", "git log -p missing.go"])
    def test_nonexistent_positional_allows_raw(self, pin_ccx: None, repo: Path, command: str) -> None:
        assert call_logpatch_to(command, cwd=repo) is None

    def test_unresolvable_positional_allows_raw(self, pin_ccx: None) -> None:
        assert call_logpatch_to("git log -p f.go") is None

    def test_two_positionals_allow_raw(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p f.go g.go", cwd=repo) is None


class TestMissingCcx:
    """An unresolved ``ccx`` binary leaves every canonical raw command unchanged."""

    def test_allows_raw(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(vcs_guards, "ccx_bin", lambda: None)
        for builder, command in (
            (vcs_guards.gitdiff_to, "git diff"),
            (vcs_guards.jjdiff_to, "jj diff"),
            (vcs_guards.gitshow_to, "git show"),
            (vcs_guards.logpatch_to, "git log -p -- f.go"),
        ):
            evt = bash_pre(command)
            assert builder(evt, evt.cmd.line.occurrences[-1]) is None


class TestWorktreeRemoveAnchored:
    """An absolute or home-anchored operand maps whatever the cwd, re-quoted for the shell."""

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("git worktree remove /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
            ("git worktree remove /pool/wt/", "ccx vcs worktree rm --path /pool/wt/"),
            ("git worktree remove -- /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
            ('git worktree remove "/pool/my wt"', "ccx vcs worktree rm --path '/pool/my wt'"),
            ("git worktree remove '/pool/my wt'", "ccx vcs worktree rm --path '/pool/my wt'"),
            ("git worktree remove /pool/my\\ wt", "ccx vcs worktree rm --path '/pool/my wt'"),
            ("git worktree remove /pool/'my wt'/x", "ccx vcs worktree rm --path '/pool/my wt/x'"),
            ("git worktree remove '/pool/$wt'", "ccx vcs worktree rm --path '/pool/$wt'"),
            ("git worktree remove '/pool/wt*'", "ccx vcs worktree rm --path '/pool/wt*'"),
            ("git worktree remove '/pool/a;b'", "ccx vcs worktree rm --path '/pool/a;b'"),
            ('git worktree remove "/pool/it\'s"', "ccx vcs worktree rm --path '/pool/it'\"'\"'s'"),
            ("git worktree remove /pool/wté", "ccx vcs worktree rm --path '/pool/wté'"),
            ("git worktree remove ~/wt", "ccx vcs worktree rm --path ~/wt"),
            ('git worktree remove ~/"my wt"', "ccx vcs worktree rm --path ~/'my wt'"),
            ("git worktree remove ~", "ccx vcs worktree rm --path ~"),
            ("/usr/bin/git worktree remove /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
            ('"git" worktree remove /pool/wt', "ccx vcs worktree rm --path /pool/wt"),
            ("git 'worktree' \"remove\" /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
            ("git worktree   remove    /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
            ("git worktree remove \\\n  /pool/wt", "ccx vcs worktree rm --path /pool/wt"),
        ],
        ids=[
            "absolute",
            "trailing_slash_verbatim",
            "dashdash",
            "double_quoted_space",
            "single_quoted_space",
            "escaped_space",
            "partly_quoted",
            "quoted_dollar_stays_literal",
            "quoted_glob_stays_literal",
            "quoted_operator_stays_literal",
            "embedded_single_quote",
            "non_ascii",
            "home",
            "home_quoted_tail",
            "bare_home",
            "git_by_path",
            "quoted_git",
            "quoted_verb",
            "extra_whitespace",
            "continuation_between_words",
        ],
    )
    def test_names_the_ccx_command(self, command: str, expected: str) -> None:
        assert steered(worktreerm_steer(command)) == [expected]

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("git worktree remove --force /pool/wt", "ccx vcs worktree rm --path /pool/wt --force"),
            ("git worktree remove -f /pool/wt", "ccx vcs worktree rm --path /pool/wt --force"),
            ("git worktree remove /pool/wt --force", "ccx vcs worktree rm --path /pool/wt --force"),
            ('git worktree remove "--force" /pool/wt', "ccx vcs worktree rm --path /pool/wt --force"),
            ("git worktree remove -f -- /pool/wt", "ccx vcs worktree rm --path /pool/wt --force"),
            ("git worktree remove -f '/pool/my wt'", "ccx vcs worktree rm --path '/pool/my wt' --force"),
            ("git worktree remove -f ~/wt", "ccx vcs worktree rm --path ~/wt --force"),
        ],
        ids=["long", "short", "after_path", "quoted_flag", "before_dashdash", "quoted_path", "home"],
    )
    def test_force_rides_along(self, command: str, expected: str) -> None:
        assert steered(worktreerm_steer(command)) == [expected]

    def test_block_names_the_raw_command_and_the_escape(self) -> None:
        result = worktreerm_steer("git worktree remove -f '/pool/my wt' 2>&1 | tail -3")
        assert isinstance(result, HookResult)
        assert result.message == (
            "BLOCKED: raw `git worktree remove` deletes the tree inline. `ccx vcs worktree rm --path "
            "<absolute-path> [--force]` removes the same working copy and hands the tree's deletion to the "
            "ccx cleanup daemon where one runs.\n"
            "- `git worktree remove -f '/pool/my wt' 2>&1`: run `ccx vcs worktree rm --path '/pool/my wt' --force` "
            "in its place.\n"
            "End the command with `# ccx:raw` to run it as written."
        )

    def test_quoted_tilde_is_a_relative_name(self, pool: Path) -> None:
        assert (
            "is neither absolute nor `./`-anchored"
            in declined(worktreerm_steer('git worktree remove "~/wt"', pool / "main"))[0]
        )


class TestWorktreeRemoveRelative:
    """A ``./``- or ``../``-anchored operand resolves against the directory the line proves."""

    @pytest.mark.parametrize(
        "command, cwd, target",
        [
            ("git worktree remove ../wt", "main", "wt"),
            ("git worktree remove ./sub", "main", "main/sub"),
            ("git worktree remove ./sub/", "main", "main/sub"),
            ("git worktree remove .", "main/sub", "main/sub"),
            ("git worktree remove ..", "main/sub", "main"),
            ("git worktree remove ../../wt", "main/sub", "wt"),
            ("git worktree remove ./sub/../gone", "main", "main/gone"),
            ("git worktree remove -- ./-wt", "main", "main/-wt"),
            ("git worktree remove './my wt'", "main", "main/my wt"),
            ("git worktree remove ../wt", "link", "real/wt"),
            ("git worktree remove ./wt", "link", "real/deep/wt"),
            ("  git worktree remove ../wt", "main", "wt"),
        ],
        ids=[
            "parent",
            "child",
            "child_trailing_slash",
            "dot",
            "dotdot",
            "two_up",
            "inner_dotdot",
            "dash_leading_name",
            "quoted_space",
            "parent_of_symlinked_cwd_is_physical",
            "child_of_symlinked_cwd_is_physical",
            "leading_whitespace",
        ],
    )
    def test_first_command_resolves_against_event_cwd(self, pool: Path, command: str, cwd: str, target: str) -> None:
        assert steered(worktreerm_steer(command, pool / cwd)) == [ccx_rm(pool / target)]

    def test_force_rides_a_relative_operand(self, pool: Path) -> None:
        assert steered(worktreerm_steer("git worktree remove -f ../wt", pool / "main")) == [
            f"{ccx_rm(pool / 'wt')} --force"
        ]

    def test_final_component_symlink_stays_literal(self, pool: Path) -> None:
        assert steered(worktreerm_steer("git worktree remove ./link", pool)) == [ccx_rm(pool / "link")]

    @pytest.mark.parametrize(
        "template, target",
        [
            ("cd {pool}/main && git worktree remove ../wt", "wt"),
            ("cd {pool}/main&&git worktree remove ../wt", "wt"),
            ("cd {pool}/main && \\\n  git worktree remove ../wt", "wt"),
            ("cd {pool}/main &&\n  git worktree remove ../wt", "wt"),
            ("cd '{pool}/main' && git worktree remove ./sub", "main/sub"),
            ("cd {pool}/main/ && git worktree remove ../wt", "wt"),
            ("  cd {pool}/main && git worktree remove ../wt", "wt"),
            ("cd {pool}/link && git worktree remove ../wt", "real/wt"),
        ],
        ids=[
            "cd_and",
            "cd_and_unspaced",
            "cd_and_continuation",
            "cd_and_newline",
            "quoted_cd",
            "cd_trailing_slash",
            "cd_indented",
            "cd_through_symlink_is_physical",
        ],
    )
    def test_opening_absolute_cd_joined_by_and_proves_the_directory(
        self, pool: Path, template: str, target: str
    ) -> None:
        assert steered(worktreerm_steer(template.format(pool=pool), pool / "real")) == [ccx_rm(pool / target)]

    @pytest.mark.parametrize(
        "template",
        [
            "cd {pool}/main; git worktree remove ../wt",
            "cd {pool}/main\ngit worktree remove ../wt",
            "cd {pool}/main || git worktree remove ../wt",
            "cd {pool}/main & git worktree remove ../wt",
            "cd {pool}/main | git worktree remove ../wt",
            "(cd {pool}/main); git worktree remove ../wt",
            "(cd {pool}/main) && git worktree remove ../wt",
            "{{ cd {pool}/main; }} && git worktree remove ../wt",
            "! cd {pool}/main && git worktree remove ../wt",
            "true || cd {pool}/main && git worktree remove ../wt",
            "echo x | cd {pool}/main && git worktree remove ../wt",
            "if true; then cd {pool}/main && git worktree remove ../wt; fi",
            "f() {{ cd {pool}/main && git worktree remove ../wt; }}",
            "cd {pool}/main 2>/dev/null && git worktree remove ../wt",
            "cd {pool}/link/.. && git worktree remove ../wt",
            "cd {pool}/main/../main && git worktree remove ../wt",
            "git fetch; cd {pool}/main && git worktree remove ../wt",
            "git fetch && cd {pool}/main && git worktree remove ../wt",
            "git fetch\ncd {pool}/main && git worktree remove ../wt",
            "mkdir -p {pool}/new; cd {pool}/new && git worktree remove ../wt",
            "ln -sfn {pool}/main {pool}/link; cd {pool}/link && git worktree remove ../wt",
            'trap "cd {pool}/real" DEBUG; cd {pool}/main && git worktree remove ../wt',
            "cd() {{ :; }}; true; cd {pool}/main && git worktree remove ../wt",
            "cd {pool}/main && git status && git worktree remove ../wt",
            "cd main && git worktree remove ../wt",
            "cd ~/main && git worktree remove ../wt",
            "cd {pool}/ma* && git worktree remove ../wt",
            'cd "$POOL" && git worktree remove ../wt',
            "cd -P {pool}/main && git worktree remove ../wt",
            "builtin cd {pool}/main && git worktree remove ../wt",
            "pushd {pool}/main && git worktree remove ../wt",
            "git status && git worktree remove ../wt",
            "f() {{ git worktree remove ../wt; }}; cd {pool}/main; f",
            "for d in a b; do git worktree remove ../wt; done",
            "(git worktree remove ../wt)",
            "time git worktree remove ../wt",
        ],
        ids=[
            "cd_semicolon",
            "cd_newline",
            "cd_or",
            "cd_background",
            "cd_pipe",
            "cd_in_subshell_then_semicolon",
            "cd_in_subshell_then_and",
            "cd_in_group",
            "cd_negated",
            "cd_behind_or",
            "cd_behind_pipe",
            "cd_under_if",
            "cd_in_function_body",
            "cd_redirected",
            "cd_through_symlink_dotdot",
            "cd_inner_dotdot",
            "cd_after_semicolon",
            "cd_after_and",
            "cd_after_newline",
            "cd_into_directory_made_earlier",
            "cd_through_symlink_repointed_earlier",
            "cd_under_debug_trap",
            "cd_redefined_earlier",
            "cd_not_adjacent",
            "cd_relative",
            "cd_home",
            "cd_glob",
            "cd_variable",
            "cd_with_flag",
            "cd_wrapped",
            "pushd",
            "after_other_command",
            "function_body_run_later",
            "loop_body",
            "subshell",
            "time_keyword",
        ],
    )
    def test_unproven_directory_is_declined(self, pool: Path, template: str) -> None:
        assert declined(worktreerm_steer(template.format(pool=pool), pool / "real")) == [
            "`../wt` is relative, and this line does not prove the directory it resolves against"
        ]

    def test_second_relative_removal_is_declined_beside_the_first(self, pool: Path) -> None:
        result = worktreerm_steer("git worktree remove ../a && git worktree remove ../b", pool / "main")
        assert steered(result) == [ccx_rm(pool / "a")]
        assert declined(result) == [
            "`../b` is relative, and this line does not prove the directory it resolves against"
        ]

    def test_unknown_event_cwd_is_declined(self) -> None:
        result = steer_worktree_remove_to_ccx(bash_pre("git worktree remove ../wt"))
        assert declined(result) == [
            "`../wt` is relative, and this line does not prove the directory it resolves against"
        ]


class TestWorktreeRemoveChdir:
    """A global ``-C <dir>`` rides along as a subshell ``builtin cd -P``, leaving the caller's cwd alone."""

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("git -C /repo worktree remove /pool/wt", "(builtin cd -P /repo && ccx vcs worktree rm --path /pool/wt)"),
            (
                "git -C /repo worktree remove --force /pool/wt",
                "(builtin cd -P /repo && ccx vcs worktree rm --path /pool/wt --force)",
            ),
            (
                "git -C '/my repo' worktree remove '/pool/my wt'",
                "(builtin cd -P '/my repo' && ccx vcs worktree rm --path '/pool/my wt')",
            ),
            ("git -C sub worktree remove /pool/wt", "(builtin cd -P ./sub && ccx vcs worktree rm --path /pool/wt)"),
            (
                "git -C ../other worktree remove /pool/wt",
                "(builtin cd -P ./../other && ccx vcs worktree rm --path /pool/wt)",
            ),
            ("git -C -dash worktree remove /pool/wt", "(builtin cd -P ./-dash && ccx vcs worktree rm --path /pool/wt)"),
            (
                "git -C 'my repo' worktree remove /pool/wt",
                "(builtin cd -P './my repo' && ccx vcs worktree rm --path /pool/wt)",
            ),
            ("git -C ~/repo worktree remove ~/wt", "(builtin cd -P ~/repo && ccx vcs worktree rm --path ~/wt)"),
            (
                "git status; git -C sub worktree remove /pool/wt",
                "(builtin cd -P ./sub && ccx vcs worktree rm --path /pool/wt)",
            ),
            (
                "2>/dev/null git -C /repo worktree remove /pool/wt",
                "(builtin cd -P /repo && ccx vcs worktree rm --path /pool/wt)",
            ),
        ],
        ids=[
            "absolute_dir",
            "absolute_dir_force",
            "quoted_dir_and_path",
            "relative_dir_gains_dot_slash",
            "parent_dir",
            "dash_leading_dir",
            "quoted_relative_dir",
            "home_dir",
            "relative_dir_needs_no_proven_cwd",
            "leading_redirect",
        ],
    )
    def test_anchored_operand(self, command: str, expected: str) -> None:
        assert steered(worktreerm_steer(command)) == [expected]

    def test_relative_operand_under_absolute_dir_ignores_the_event_cwd(self, pool: Path) -> None:
        assert steered(worktreerm_steer(f"git -C {pool}/main worktree remove ../wt", pool / "real")) == [
            f"(builtin cd -P {pool}/main && {ccx_rm(pool / 'wt')})"
        ]

    def test_relative_operand_under_symlinked_dir_is_physical(self, pool: Path) -> None:
        assert steered(worktreerm_steer(f"git -C {pool}/link worktree remove ../wt", pool)) == [
            f"(builtin cd -P {pool}/link && {ccx_rm(pool / 'real' / 'wt')})"
        ]

    def test_relative_operand_under_relative_dir_joins_the_event_cwd(self, pool: Path) -> None:
        assert steered(worktreerm_steer("git -C sub worktree remove ../wt -f", pool / "main")) == [
            f"(builtin cd -P ./sub && {ccx_rm(pool / 'main' / 'wt')} --force)"
        ]

    def test_relative_operand_under_relative_dir_after_proving_cd(self, pool: Path) -> None:
        command = f"cd {pool}/main && git -C sub worktree remove ./wt"
        assert steered(worktreerm_steer(command, pool / "real")) == [
            f"(builtin cd -P ./sub && {ccx_rm(pool / 'main' / 'sub' / 'wt')})"
        ]

    @pytest.mark.parametrize(
        "template",
        [
            "git status; git -C sub worktree remove ../wt",
            "git status; git -C {pool}/main worktree remove ../wt",
            "ln -sfn {pool}/main {pool}/link; git -C {pool}/link worktree remove ../wt",
            "git -C ~/repo worktree remove ../wt",
        ],
        ids=[
            "relative_dir_off_an_unproven_cwd",
            "absolute_dir_after_another_command",
            "symlinked_dir_repointed_earlier",
            "home_dir",
        ],
    )
    def test_relative_operand_under_unprovable_dir_is_declined(self, pool: Path, template: str) -> None:
        assert declined(worktreerm_steer(template.format(pool=pool), pool / "main")) == [
            "`../wt` is relative, and this line does not prove the directory it resolves against"
        ]


class TestWorktreeRemoveWholeLine:
    """Every removal on a line gets its own verdict, wherever pipes, redirects, and siblings put it."""

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("git worktree remove /pool/wt && git worktree prune", ["ccx vcs worktree rm --path /pool/wt"]),
            (
                "git worktree list; git worktree remove /pool/wt; git branch -D x",
                ["ccx vcs worktree rm --path /pool/wt"],
            ),
            ("git worktree remove /pool/wt 2>&1 | tail -3", ["ccx vcs worktree rm --path /pool/wt"]),
            ("yes | git worktree remove /pool/wt", ["ccx vcs worktree rm --path /pool/wt"]),
            ("git worktree remove /pool/wt > rm.log 2>&1", ["ccx vcs worktree rm --path /pool/wt"]),
            ("2>/dev/null git worktree remove /pool/wt", ["ccx vcs worktree rm --path /pool/wt"]),
            ("git worktree remove /pool/wt &", ["ccx vcs worktree rm --path /pool/wt"]),
            ("if git worktree remove /pool/wt; then echo gone; fi", ["ccx vcs worktree rm --path /pool/wt"]),
            ("time git worktree remove /pool/wt", ["ccx vcs worktree rm --path /pool/wt"]),
            ("git status\ngit worktree remove /pool/wt\ngit worktree list", ["ccx vcs worktree rm --path /pool/wt"]),
            (
                "git worktree remove /pool/a && git worktree remove -f '/pool/b c'",
                ["ccx vcs worktree rm --path /pool/a", "ccx vcs worktree rm --path '/pool/b c' --force"],
            ),
        ],
        ids=[
            "and_sibling",
            "semicolon_siblings",
            "piped_with_stderr",
            "fed_by_pipe",
            "redirected",
            "leading_redirect",
            "backgrounded",
            "if_condition",
            "time_keyword",
            "multiline",
            "two_removals",
        ],
    )
    def test_each_removal_is_steered(self, command: str, expected: list[str]) -> None:
        assert steered(worktreerm_steer(command)) == expected

    def test_mapped_and_declined_removals_share_one_block(self) -> None:
        result = worktreerm_steer("git worktree remove /pool/a && git worktree remove $B")
        assert steered(result) == ["ccx vcs worktree rm --path /pool/a"]
        assert declined(result) == ["`$B` is expanded by the shell at run time"]


class TestWorktreeRemoveDeclined:
    """A removal with no provably identical ccx form blocks with the reason in place of a command."""

    @pytest.mark.parametrize(
        "command, reason",
        [
            ("git worktree remove $WT", "`$WT` is expanded by the shell at run time"),
            ('git worktree remove "$HOME/wt"', '`"$HOME/wt"` is expanded by the shell at run time'),
            ("git worktree remove ${WT}/x", "`${WT}/x` is expanded by the shell at run time"),
            ("git worktree remove /pool/$(date +%s)", "`/pool/$(date +%s)` is expanded by the shell at run time"),
            ("git worktree remove $'/pool/a\\tb'", "`$'/pool/a\\tb'` is expanded by the shell at run time"),
            ("git worktree remove /pool/wt*", "`/pool/wt*` is expanded by the shell at run time"),
            ("git worktree remove /pool/{a,b}", "`/pool/{a,b}` is expanded by the shell at run time"),
            ("git worktree remove ./wt?", "`./wt?` is expanded by the shell at run time"),
            ("git worktree remove ~other/wt", "`~other/wt` is expanded by the shell at run time"),
            ("git worktree remove ~/wt*", "`~/wt*` is expanded by the shell at run time"),
            ("git worktree remove -f $WT /pool/wt", "`$WT` is expanded by the shell at run time"),
            ("for w in a b; do git worktree remove $w; done", "`$w` is expanded by the shell at run time"),
            (
                "git worktree remove $(cat wt.txt)",
                UNREADABLE,
            ),
            (
                "git worktree remove -f `cat wt.txt` /pool/wt",
                UNREADABLE,
            ),
            (
                "git worktree remove /pool/wt <(echo x)",
                UNREADABLE,
            ),
            (
                "git worktree remove wt",
                "`wt` is neither absolute nor `./`-anchored, and git matches such a name against every working "
                "copy's path suffix before reading it as a path",
            ),
            (
                "git worktree remove .worktrees/wt",
                "`.worktrees/wt` is neither absolute nor `./`-anchored, and git matches such a name against every "
                "working copy's path suffix before reading it as a path",
            ),
            (
                "git worktree remove -- -wt",
                "`-wt` is neither absolute nor `./`-anchored, and git matches such a name against every working "
                "copy's path suffix before reading it as a path",
            ),
            (
                "git worktree remove ''",
                "`''` is neither absolute nor `./`-anchored, and git matches such a name against every working "
                "copy's path suffix before reading it as a path",
            ),
            (
                "git -C /repo worktree remove wt",
                "`wt` is neither absolute nor `./`-anchored, and git matches such a name against every working "
                "copy's path suffix before reading it as a path",
            ),
            (
                "git worktree remove -f -f /pool/wt",
                "a repeated `--force` overrides a lock, and `ccx vcs worktree rm` never removes a locked working copy",
            ),
            (
                "git worktree remove --force /pool/wt --force",
                "a repeated `--force` overrides a lock, and `ccx vcs worktree rm` never removes a locked working copy",
            ),
            ("git worktree remove -ff /pool/wt", "`-ff` is not a flag the hook maps — only `-f`/`--force` is"),
            ("git worktree remove --forc /pool/wt", "`--forc` is not a flag the hook maps — only `-f`/`--force` is"),
            (
                "git worktree remove --expire now /pool/wt",
                "`--expire` is not a flag the hook maps — only `-f`/`--force` is",
            ),
            (
                "git worktree remove --no-force /pool/wt",
                "`--no-force` is not a flag the hook maps — only `-f`/`--force` is",
            ),
            ("git worktree remove", "it names no path operand"),
            ("git worktree remove --force", "it names no path operand"),
            ("git worktree remove /pool/a /pool/b", "it names more than one path operand"),
            ("sudo git worktree remove /pool/wt", "it runs under `sudo`, which the ccx form would drop"),
            ("timeout 5 git worktree remove /pool/wt", "it runs under `timeout`, which the ccx form would drop"),
            ("env GIT_DIR=/x/.git git worktree remove /pool/wt", "it runs under `env`, which the ccx form would drop"),
            ("command git worktree remove /pool/wt", "it runs under `command`, which the ccx form would drop"),
            ("nohup git worktree remove /pool/wt &", "it runs under `nohup`, which the ccx form would drop"),
            ("echo /pool/wt | xargs git worktree remove", "it runs under `xargs`, which the ccx form would drop"),
            ("builtin git worktree remove /pool/wt", "it runs under `builtin`, which the ccx form would drop"),
            (
                "builtin command git worktree remove /pool/wt",
                "it runs under `builtin`, which the ccx form would drop",
            ),
            ("stdbuf -oL git worktree remove /pool/wt", "it runs under `stdbuf`, which the ccx form would drop"),
            ("setsid git worktree remove /pool/wt", "it runs under `setsid`, which the ccx form would drop"),
            ("caffeinate -i git worktree remove /pool/wt", "it runs under `caffeinate`, which the ccx form would drop"),
            ("noglob git worktree remove /pool/wt", "it runs under `noglob`, which the ccx form would drop"),
            ("flock /tmp/lock git worktree remove /pool/wt", "it runs under `flock`, which the ccx form would drop"),
            ("watch -n 5 git worktree remove /pool/wt", "it runs under `watch`, which the ccx form would drop"),
            ("rtk git worktree remove /pool/wt", "it runs under `rtk`, which the ccx form would drop"),
            ("mise exec -- git worktree remove /pool/wt", "it runs under `mise`, which the ccx form would drop"),
            ("op run -- git worktree remove /pool/wt", "it runs under `op`, which the ccx form would drop"),
            (
                "find . -name wt -exec git worktree remove {} \\;",
                "it runs under `find`, which the ccx form would drop",
            ),
            ("git worktree remove /pool/w\\\nt", UNREADABLE),
            ("git -C /re\\\npo worktree remove /pool/wt", UNREADABLE),
            ("sudo stdbuf -oL git worktree remove /pool/wt", "it runs under `sudo`, which the ccx form would drop"),
            (
                "GIT_DIR=/x/.git git worktree remove /pool/wt",
                "the `GIT_DIR=` prefix changes the environment git runs in",
            ),
            (
                "git -c core.x=y worktree remove /pool/wt",
                "`-c` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git --git-dir=/x/.git worktree remove /pool/wt",
                "`--git-dir=/x/.git` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git --no-pager worktree remove /pool/wt",
                "`--no-pager` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git -C /a --no-pager worktree remove /pool/wt",
                "`--no-pager` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git -C /a -C b worktree remove /pool/wt",
                "`-C` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git --attr-source HEAD worktree remove /pool/wt",
                "`--attr-source` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git --super-prefix sub/ worktree remove /pool/wt",
                "`--super-prefix` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git --shallow-file /dev/null worktree remove /pool/wt",
                "`--shallow-file` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            (
                "git -C /repo --attr-source HEAD worktree remove /pool/wt",
                "`--attr-source` is a global git option beyond the one `-C <dir>` the hook maps",
            ),
            ('git -C "$REPO" worktree remove /pool/wt', '`"$REPO"` is expanded by the shell at run time'),
            ("git -C /re* worktree remove /pool/wt", "`/re*` is expanded by the shell at run time"),
            ("git -C '' worktree remove /pool/wt", "an empty `-C` names no directory"),
            ("bash -c 'git worktree remove /pool/wt'", "it sits inside another command's payload or substitution"),
            ('bash -c "git worktree remove /pool/wt"', "it sits inside another command's payload or substitution"),
            ('eval "git worktree remove /pool/wt"', "it sits inside another command's payload or substitution"),
            ("echo $(git worktree remove /pool/wt)", "it sits inside another command's payload or substitution"),
            (
                "git worktree remove 2>/dev/null /pool/wt",
                "a redirect sits between its words, where the hook cannot re-read them — move it after the path",
            ),
            (
                "git worktree 2>&1 remove /pool/wt",
                "a redirect sits between its words, where the hook cannot re-read them — move it after the path",
            ),
        ],
        ids=[
            "variable",
            "quoted_variable",
            "braced_variable",
            "inline_substitution",
            "ansi_c_quoting",
            "glob",
            "brace_list",
            "relative_glob",
            "other_users_home",
            "home_glob",
            "variable_beside_a_literal",
            "loop_variable",
            "bare_substitution",
            "bare_backticks_beside_a_literal",
            "process_substitution",
            "bare_name",
            "hidden_pool_name",
            "dash_name_after_dashdash",
            "empty_operand",
            "bare_name_under_dir",
            "double_force_short",
            "double_force_split",
            "bundled_force",
            "abbreviated_force",
            "unknown_flag",
            "negated_force",
            "no_operand",
            "force_without_operand",
            "two_operands",
            "sudo",
            "timeout",
            "env",
            "command_builtin",
            "nohup",
            "xargs",
            "builtin",
            "builtin_command",
            "stdbuf",
            "setsid",
            "caffeinate",
            "noglob",
            "flock",
            "watch",
            "rtk",
            "mise_exec",
            "op_run",
            "find_exec",
            "continuation_inside_the_operand",
            "continuation_inside_the_dir",
            "sudo_then_stdbuf",
            "env_prefix",
            "config_override",
            "git_dir",
            "no_pager",
            "option_after_dir",
            "two_dirs",
            "attr_source_value",
            "super_prefix_value",
            "shallow_file_value",
            "attr_source_after_dir",
            "variable_dir",
            "glob_dir",
            "empty_dir",
            "single_quoted_payload",
            "double_quoted_payload",
            "eval_payload",
            "command_substitution",
            "interleaved_redirect",
            "redirect_before_verb",
        ],
    )
    def test_names_the_reason(self, command: str, reason: str) -> None:
        result = worktreerm_steer(command)
        assert declined(result) == [reason]
        assert steered(result) == []
        assert result.message.endswith("End the command with `# ccx:raw` to run it as written.")

    @pytest.mark.parametrize(
        "command",
        [
            "gi\\\nt worktree remove /pool/wt",
            "git work\\\ntree remove /pool/wt",
            "git worktree re\\\nmove /pool/wt",
        ],
        ids=["split_git", "split_worktree", "split_remove"],
    )
    def test_continuation_splitting_an_identifying_word_blocks(self, command: str) -> None:
        result = worktreerm_steer(command)
        assert isinstance(result, HookResult)
        assert result.action is Action.block
        assert (
            "- a line continuation splits a word of a `git worktree remove` on this line: write that command on "
            "one line."
        ) in result.message
        evt = make_evt(command)
        assert GitWorktreeRemove().check_command_line(evt, evt.cmd.line) is True


class TestWorktreeRemoveUntouched:
    """Read-only worktree verbs, help requests, and unrelated removals never reach a verdict."""

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
        ],
        ids=[
            "list",
            "list_porcelain",
            "prune",
            "prune_dry_run",
            "add",
            "move",
            "lock",
            "unlock",
            "repair",
            "list_under_dir",
            "bare_worktree",
            "global_help",
            "global_version",
            "global_exec_path",
            "global_html_path_after_dir",
            "git_rm",
            "remote_remove",
            "plain_rm",
            "echoed",
            "commit_message",
            "ssh_payload",
            "docker_payload",
            "heredoc_body",
            "extra_wrapper_without_git",
            "find_without_removal",
            "watch_without_removal",
            "mise_exec_without_removal",
            "builtin_without_git",
            "continuation_without_removal",
            "jj_workspace",
        ],
    )
    def test_no_verdict(self, command: str) -> None:
        evt = make_evt(command)
        assert steer_worktree_remove_to_ccx(evt) is None
        assert GitWorktreeRemove().check_command_line(evt, evt.cmd.line) is False

    @pytest.mark.parametrize(
        "command",
        [
            "git worktree remove -h",
            "git worktree remove --help",
            "git worktree remove -h /pool/wt",
            "git worktree remove /pool/wt -h",
            "git -C /repo worktree remove --force /pool/wt --help",
        ],
        ids=["short", "long", "before_a_path", "after_a_path", "after_force_under_dir"],
    )
    def test_help_request_passes_the_gate_and_gets_no_verdict(self, command: str) -> None:
        evt = make_evt(command)
        assert GitWorktreeRemove().check_command_line(evt, evt.cmd.line) is True
        assert steer_worktree_remove_to_ccx(evt) is None

    @pytest.mark.parametrize(
        "command",
        [
            "git worktree remove /pool/wt",
            "git -C /repo worktree remove -f /pool/wt",
            "git status && git worktree remove $WT",
            "sudo git worktree remove /pool/wt",
            "stdbuf -oL git worktree remove /pool/wt",
            "bash -c 'git worktree remove /pool/wt'",
        ],
        ids=["plain", "under_dir", "chained", "wrapped", "extra_wrapper", "nested"],
    )
    def test_condition_matches_every_removal(self, command: str) -> None:
        evt = make_evt(command)
        assert GitWorktreeRemove().check_command_line(evt, evt.cmd.line) is True


class TestRawRequested:
    """The explicit escape: a ``# ccx:raw`` comment on the line, or ``CAPT_HOOK_CCX_RAW`` in the environment."""

    @pytest.mark.parametrize(
        "command, requested",
        [
            ("git worktree remove $WT # ccx:raw", True),
            ("git worktree remove /pool/wt #ccx:raw", True),
            ("git worktree remove /pool/wt;# ccx:raw", True),
            ("sudo git worktree remove /pool/wt && echo done # ccx:raw", True),
            ("# ccx:raw\ngit worktree remove /pool/wt", True),
            ("bash -c 'git diff # ccx:raw'", True),
            ("git worktree remove $WT", False),
            ("git worktree remove /pool/wt # ccx:rawish", False),
            ("git worktree remove /pool/wt # raw", False),
            ("git worktree remove '/pool/#ccx:raw'", False),
            ("git worktree remove ../#ccx:raw", False),
            ("printf '%s\\n' '# ccx:raw'; git worktree remove /pool/wt", False),
            ("git commit -m 'explain # ccx:raw' && git worktree remove /pool/wt", False),
            ("bash -c 'echo \"# ccx:raw\"; git worktree remove /pool/wt'", False),
        ],
        ids=[
            "marker",
            "marker_unspaced",
            "marker_after_semicolon",
            "marker_after_chain",
            "marker_on_its_own_line",
            "marker_in_nested_payload_comment",
            "absent",
            "longer_word",
            "other_comment",
            "inside_quoted_path",
            "inside_bare_path",
            "inside_string_argument",
            "inside_commit_message",
            "inside_nested_string_argument",
        ],
    )
    def test_marker(self, monkeypatch: pytest.MonkeyPatch, command: str, requested: bool) -> None:
        monkeypatch.delenv(vcs_guards.RAW_ENV, raising=False)
        evt = make_evt(command)
        assert RawRequested().check_command_line(evt, evt.cmd.line) is requested

    def test_env(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv(vcs_guards.RAW_ENV, "1")
        evt = make_evt("git worktree remove $WT")
        assert RawRequested().check_command_line(evt, evt.cmd.line) is True

    def test_empty_env_is_not_a_request(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv(vcs_guards.RAW_ENV, "")
        evt = make_evt("git worktree remove $WT")
        assert RawRequested().check_command_line(evt, evt.cmd.line) is False


class TestWorktreeRemoveRealGit:
    """The guard's verdicts held to git itself, over a real repository with linked worktrees.

    Each case runs the raw command in a real shell, then the command the block names in its place,
    and demands the same working copy gone both times. The rest pins what the declines rest on: a
    bare name is matched by suffix before it is read as a path, every option spelling that still
    removes is blocked, and every spelling the guard leaves alone removes nothing.
    """

    @pytest.mark.parametrize("shell", SHELLS)
    @pytest.mark.parametrize(
        "case",
        [
            pytest.param(Removal("main", "git worktree remove {root}/sib", "sib"), id="absolute"),
            pytest.param(Removal("main", "git worktree remove {root}/sib/", "sib"), id="absolute_trailing_slash"),
            pytest.param(Removal("main", "git worktree remove {root}/pool/../sib", "sib"), id="absolute_dotdot"),
            pytest.param(
                Removal("main", "git worktree remove {root}/link/../wt", "main/wt"),
                id="absolute_dotdot_past_symlink_is_physical",
            ),
            pytest.param(Removal("main", 'git worktree remove "{root}/my wt"', "my wt"), id="quoted_space"),
            pytest.param(Removal("main", "git worktree remove ~/lane", "home/lane"), id="home"),
            pytest.param(Removal("main", "git worktree remove ../sib", "sib"), id="parent"),
            pytest.param(Removal("main", "git worktree remove ./wt", "main/wt"), id="child"),
            pytest.param(Removal("main", "git worktree remove -- ../sib", "sib"), id="dashdash"),
            pytest.param(Removal("main", "git worktree remove \\\n  ../sib", "sib"), id="continuation_between_words"),
            pytest.param(Removal("main/sub", "git worktree remove ../wt", "main/wt"), id="parent_from_subdirectory"),
            pytest.param(Removal("main/sub", "git worktree remove ./wt", "main/sub/wt"), id="child_from_subdirectory"),
            pytest.param(Removal("main", "git worktree remove ./solo", None), id="anchored_name_is_never_a_suffix"),
            pytest.param(Removal("main", "git worktree remove ./missing", None), id="missing_path"),
            pytest.param(Removal("main", "git worktree remove .", None), id="main_working_copy"),
            pytest.param(
                Removal("link", "git worktree remove ../wt", "main/wt"), id="parent_of_symlinked_cwd_is_physical"
            ),
            pytest.param(Removal("sib", "git worktree remove ../wt", "wt"), id="from_a_linked_worktree"),
            pytest.param(Removal("sib", "git worktree remove .", "sib"), id="own_cwd"),
            pytest.param(Removal("outside", "git worktree remove {root}/sib", None), id="cwd_outside_any_repository"),
            pytest.param(Removal("outside", "git worktree remove ../sib", None), id="relative_outside_any_repository"),
            pytest.param(
                Removal("outside", "git worktree remove ../sib", "sib", before="cd {root}/main && "),
                id="opening_cd",
            ),
            pytest.param(
                Removal("outside", "git worktree remove ../wt", "main/wt", before="cd {root}/link && "),
                id="opening_cd_through_symlink_is_physical",
            ),
            pytest.param(Removal("outside", "git -C {root}/main worktree remove {root}/sib", "sib"), id="dir_absolute"),
            pytest.param(
                Removal("outside", "git -C {root}/main worktree remove ../sib", "sib"),
                id="dir_absolute_relative_operand",
            ),
            pytest.param(
                Removal("main", "git -C sub worktree remove ../wt", "main/wt"), id="dir_relative_parent_operand"
            ),
            pytest.param(
                Removal("main", "git -C sub worktree remove ./wt", "main/sub/wt"), id="dir_relative_child_operand"
            ),
            pytest.param(Removal("main/sub", "git -C .. worktree remove ./wt", "main/wt"), id="dir_dotdot"),
            pytest.param(
                Removal("outside", "git -C {root}/link worktree remove ../wt", "main/wt"),
                id="dir_symlink_is_physical",
            ),
            pytest.param(
                Removal("outside", "git -C {root}/link/.. worktree remove ./wt", "main/wt"),
                id="dir_dotdot_past_symlink_is_physical",
            ),
            pytest.param(Removal(".", "git -C link worktree remove ../wt", "main/wt"), id="dir_relative_symlink"),
            pytest.param(
                Removal(".", "git -C link/.. worktree remove ./wt", "main/wt"),
                id="dir_relative_dotdot_past_symlink",
            ),
            pytest.param(Removal("main", "git -C -dash worktree remove {root}/sib", "sib"), id="dir_dash_leading"),
            pytest.param(Removal("outside", "git -C ~/../main worktree remove {root}/sib", "sib"), id="dir_home"),
            pytest.param(Removal("main", "git -C {root}/missing worktree remove {root}/sib", None), id="dir_missing"),
            pytest.param(Removal("main", "git -C missing worktree remove {root}/sib", None), id="dir_relative_missing"),
            pytest.param(
                Removal("main", "git -C {root}/outside worktree remove {root}/sib", None),
                id="dir_outside_any_repository",
            ),
            pytest.param(Removal("main", "git -C {root}/bin/ccx worktree remove {root}/sib", None), id="dir_is_a_file"),
            pytest.param(
                Removal("main", "git -C sub worktree remove {root}/sib", "sib", after="; pwd -P"),
                id="dir_leaves_the_callers_cwd",
            ),
            pytest.param(Removal("main", "git worktree remove {root}/sib", None, dirty="sib"), id="dirty_unforced"),
            pytest.param(
                Removal("main", "git worktree remove --force {root}/sib", "sib", dirty="sib"), id="dirty_forced"
            ),
            pytest.param(
                Removal("main", "git -C sub worktree remove ../wt -f", "main/wt", dirty="main/wt"),
                id="dirty_forced_under_dir",
            ),
            pytest.param(Removal("main", "git worktree remove -f {root}/sib", None, locked="sib"), id="locked_forced"),
        ],
    )
    def test_named_command_removes_what_git_removes(
        self, gitpool: GitPool, shell: tuple[str, ...], case: Removal
    ) -> None:
        before, command, after = (part.format(root=gitpool.root) for part in (case.before, case.command, case.after))
        result = worktreerm_steer(before + command + after, gitpool.root / case.cwd)
        [named] = steered(result)
        assert declined(result) == []
        assert f"- `{command}`: run `{named}` in its place." in result.message
        raw = gitpool.run(shell, before + command + after, case.cwd, dirty=case.dirty, locked=case.locked)
        assert raw.removed == frozenset(filter(None, [case.removed]))
        assert raw.ok is (case.removed is not None)
        assert gitpool.run(shell, before + named + after, case.cwd, dirty=case.dirty, locked=case.locked) == raw

    @pytest.mark.parametrize(
        "cwd, command, removed",
        [
            ("main", "git worktree remove solo", "pool/solo"),
            ("main", "git worktree remove pool/solo", "pool/solo"),
            ("main", "git worktree remove lane", "home/lane"),
            ("main", "git worktree remove 'my wt'", "my wt"),
            ("main", "git worktree remove a/feat", "pool/a/feat"),
            ("main", "git worktree remove feat", None),
            ("main", "git worktree remove wt", "main/wt"),
            ("sib", "git worktree remove wt", None),
            ("outside", "git -C {root}/main worktree remove solo", "pool/solo"),
        ],
        ids=[
            "suffix_beats_the_directory_in_cwd",
            "suffix_of_two_components",
            "suffix_under_home",
            "suffix_with_a_space",
            "suffix_long_enough_to_be_unique",
            "ambiguous_suffix_with_no_such_path",
            "ambiguous_suffix_falls_back_to_cwd",
            "ambiguous_suffix_from_another_cwd",
            "suffix_under_dir",
        ],
    )
    def test_bare_name_is_matched_by_suffix_before_path(
        self, gitpool: GitPool, cwd: str, command: str, removed: str | None
    ) -> None:
        line = command.format(root=gitpool.root)
        [reason] = declined(worktreerm_steer(line, gitpool.root / cwd))
        assert reason.endswith(
            "git matches such a name against every working copy's path suffix before reading it as a path"
        )
        assert gitpool.run(BASH, line, cwd).removed == frozenset(filter(None, [removed]))
        assert (gitpool.root / "main" / "solo").is_dir()

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
    def test_every_spelling_git_removes_under_is_blocked(
        self, gitpool: GitPool, command: str, locked: str | None
    ) -> None:
        line = command.format(root=gitpool.root)
        assert gitpool.run(BASH, line, "main", locked=locked) == Outcome(frozenset({"sib"}), True, "")
        assert worktreerm_steer(line, gitpool.root / "main").action is Action.block

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
    def test_every_spelling_the_guard_leaves_alone_removes_nothing(self, gitpool: GitPool, command: str) -> None:
        line = command.format(root=gitpool.root)
        assert worktreerm_steer(line, gitpool.root / "main") is None
        assert gitpool.run(BASH, line, "main").removed == frozenset()

    @pytest.mark.skipif(shutil.which("dash") is None, reason="dash is not installed")
    def test_dir_form_removes_nothing_in_a_shell_without_builtin(self, gitpool: GitPool) -> None:
        [named] = steered(worktreerm_steer("git -C sub worktree remove ../wt", gitpool.root / "main"))
        assert gitpool.run(("dash", "-c"), named, "main") == Outcome(frozenset(), False, "")


class TestGhRunWatchNudge:
    """The one-shot steer from a manual ``gh run watch`` toward ``ccx vcs ship``."""

    def test_fires_on_gh_run_watch(self, tmp_path: Path) -> None:
        result = steer_gh_run_watch_to_ship(bash_pre("gh run watch 123 --exit-status", tmp_path / "s"))
        assert result is not None
        assert result.action is Action.warn
        assert "ccx vcs ship" in result.message

    def test_second_invocation_is_silent(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"  # one shared session store, as the whole session shares
        first = steer_gh_run_watch_to_ship(bash_pre("gh run watch 123 --exit-status", sd))
        assert first is not None and first.action is Action.warn
        # A later watch in the same session finds the latch set and stays quiet.
        assert steer_gh_run_watch_to_ship(bash_pre("gh run watch 456 --exit-status", sd)) is None
        assert GhRunWatchNudged(fired=True) == bash_pre("x", sd).ctx.s.load(GhRunWatchNudged)

    def test_matches_single_gh_run_watch(self) -> None:
        evt = bash_pre("gh run watch 123 --exit-status")
        assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is True

    def test_piped_or_chained_not_matched(self) -> None:
        # `json_guards`/`ship` own the pipe; a chained line is not a single command → no steer.
        for cmd in ("gh run watch 123 --exit-status | tee run.log", "cd repo && gh run watch 123"):
            evt = bash_pre(cmd)
            assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is False

    def test_gh_run_view_not_matched(self) -> None:
        # `gh run view --log-failed` is a legitimate failure drill-down, never a watch.
        evt = bash_pre("gh run view 123 --log-failed")
        assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is False

    def test_gh_pr_list_not_matched(self) -> None:
        evt = bash_pre("gh pr list")
        assert GhRunWatchSingle().check_command_line(evt, evt.cmd.line) is False
