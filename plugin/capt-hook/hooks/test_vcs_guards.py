"""Tests for the ``git log -p`` -> ``ccx vcs history`` rewrite and the ``gh run watch`` -> ``ccx vcs ship`` nudge.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_vcs_guards.py

The rewrite is driven through ``logpatch_to`` — the registered ``to=`` builder of the
``LogPatchDump`` family — so these exercise the public rewrite surface, never the
``git_log_history`` helper it delegates to. ``ccx_bin`` is pinned to a fixed path so the
emitted command is deterministic; a shape with no faithful ``ccx vcs history`` form makes
``logpatch_to`` return ``None`` (the rewrite falls back to the block).

The no-``--`` branch pins a pathspec only when a sole trailing positional exists on disk
(a bare revision like ``HEAD~5`` has no file to hand ``ccx vcs history``), so its coverage
is environment-dependent and lives here rather than in the inline ``tests={}`` matrix: each
case ``chdir``s into a temp dir with a known file. The ``--`` branch and the flag/count
parsing are disk-independent but share the rewrite's contract, so they are pinned here too.

The nudge's one-shot latch is stateful across two invocations — a shape the declarative
``tests={}`` harness cannot express — so its fire/silence behavior is driven end to end here
against a real ``SessionStore`` backed by a temp session dir.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from captain_hook import Action
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks import vcs_guards
from hooks.vcs_guards import (
    GhRunWatchNudged,
    GhRunWatchSingle,
    steer_gh_run_watch_to_ship,
)

MAIN_T = "/transcripts/main.jsonl"
FAKE_CCX = "/fake/ccx"


def bash_pre(command: str, session_dir: Path | None = None) -> PreToolUseEvent:
    """A ``PreToolUseEvent`` for a Bash ``command``, backed by ``session_dir`` for the one-shot latch."""
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    raw = {"tool_name": "Bash", "tool_input": {"command": command}, "transcript_path": MAIN_T}
    return PreToolUseEvent(_raw=raw, ctx=ctx)


@pytest.fixture
def repo(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    (tmp_path / "f.go").write_text("package main\n")
    (tmp_path / "g.go").write_text("package main\n")
    monkeypatch.chdir(tmp_path)
    return tmp_path


@pytest.fixture
def pin_ccx(monkeypatch: pytest.MonkeyPatch) -> None:
    """Pin ``ccx_bin`` so ``logpatch_to`` emits a deterministic rewrite string."""
    monkeypatch.setattr(vcs_guards, "ccx_bin", lambda: FAKE_CCX)


def call_logpatch_to(command: str) -> str | None:
    """Drive ``vcs_guards.logpatch_to`` on ``command``'s primary occurrence — the public rewrite surface."""
    evt = bash_pre(command)
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


class TestLogPatchRewriteBlocks:
    """Shapes with no faithful ``ccx vcs history`` form make ``logpatch_to`` return ``None`` (→ block)."""

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
    def test_blocks(self, pin_ccx: None, command: str) -> None:
        assert call_logpatch_to(command) is None


class TestLogPatchRewriteDiskBranch:
    """The no-``--`` branch: a sole trailing positional is a path iff it exists on disk."""

    def test_sole_existing_positional(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p f.go") == f"{FAKE_CCX} vcs history f.go"

    def test_sole_existing_positional_with_count(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p -3 f.go") == f"{FAKE_CCX} vcs history f.go -n 3"

    @pytest.mark.parametrize(
        "command",
        ["git log -p HEAD~5", "git log -p missing.go"],  # revision vs. missing file — neither is a path on disk
        ids=["bare_revision", "missing_path"],
    )
    def test_nonexistent_positional_is_a_revision(self, pin_ccx: None, repo: Path, command: str) -> None:
        assert call_logpatch_to(command) is None

    def test_two_existing_positionals_block(self, pin_ccx: None, repo: Path) -> None:
        assert call_logpatch_to("git log -p f.go g.go") is None


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
