"""Tests for the ``_git_log_history`` parser and the ``gh run watch`` -> ``ccx vcs ship`` nudge.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_vcs_guards.py

The no-``--`` branch pins a pathspec only when a sole trailing positional exists on
disk (a bare revision like ``HEAD~5`` has no file to hand ``ccx vcs history``), so its
coverage is environment-dependent and lives here rather than in the inline ``tests={}``
matrix: each case ``chdir``s into a temp dir with a known file. The ``--`` branch and
the flag/count parsing are disk-independent but share the parser's contract, so they
are pinned here too.

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

from hooks.vcs_guards import (
    GhRunWatchNudged,
    GhRunWatchSingle,
    _git_log_history,
    steer_gh_run_watch_to_ship,
)

MAIN_T = "/transcripts/main.jsonl"


def _bash_pre(command: str, session_dir: Path | None = None) -> PreToolUseEvent:
    """A ``PreToolUseEvent`` for a Bash ``command``, backed by ``session_dir`` for the one-shot latch."""
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    raw = {"tool_name": "Bash", "tool_input": {"command": command}, "transcript_path": MAIN_T}
    return PreToolUseEvent(_raw=raw, ctx=ctx)


@pytest.fixture
def _repo(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    (tmp_path / "f.go").write_text("package main\n")
    (tmp_path / "g.go").write_text("package main\n")
    monkeypatch.chdir(tmp_path)
    return tmp_path


class TestGitLogHistoryDashDash:
    """The ``--`` branch pins the path with no disk check (disk-independent)."""

    def test_plain_path(self) -> None:
        assert _git_log_history(("-p", "--", "internal/cli/root.go")) == ("internal/cli/root.go", None)

    def test_path_need_not_exist(self) -> None:
        # `--` is an explicit pathspec marker — the token is taken verbatim, existence aside.
        assert _git_log_history(("-p", "--", "ghost/never-there.go")) == ("ghost/never-there.go", None)

    def test_count_two_token(self) -> None:
        assert _git_log_history(("-p", "-n", "5", "--", "f.go")) == ("f.go", "5")

    def test_count_glued_short(self) -> None:
        assert _git_log_history(("-p", "-5", "--", "f.go")) == ("f.go", "5")

    def test_count_max_count_equals(self) -> None:
        assert _git_log_history(("-p", "--max-count=5", "--", "f.go")) == ("f.go", "5")

    def test_count_max_count_two_token(self) -> None:
        assert _git_log_history(("-p", "--max-count", "5", "--", "f.go")) == ("f.go", "5")

    def test_follow_dropped(self) -> None:
        assert _git_log_history(("-p", "--follow", "--", "f.go")) == ("f.go", None)

    def test_patch_synonyms(self) -> None:
        assert _git_log_history(("--patch", "--", "f.go")) == ("f.go", None)
        assert _git_log_history(("-u", "--", "f.go")) == ("f.go", None)


class TestGitLogHistoryBlocks:
    """Shapes with no faithful ``ccx vcs history`` form return ``None`` (→ block)."""

    def test_no_path(self) -> None:
        assert _git_log_history(("-p",)) is None

    def test_two_paths_after_dashdash(self) -> None:
        assert _git_log_history(("-p", "--", "a.go", "b.go")) is None

    def test_empty_dashdash(self) -> None:
        assert _git_log_history(("-p", "--")) is None

    def test_revision_before_dashdash(self) -> None:
        assert _git_log_history(("-p", "HEAD", "--", "f.go")) is None

    def test_unrecognized_flag(self) -> None:
        assert _git_log_history(("-p", "--author=me", "--", "f.go")) is None

    def test_non_numeric_count(self) -> None:
        assert _git_log_history(("-p", "-n", "x", "--", "f.go")) is None

    def test_dangling_count_flag(self) -> None:
        assert _git_log_history(("-p", "-n")) is None


class TestGitLogHistoryDiskBranch:
    """The no-``--`` branch: a sole trailing positional is a path iff it exists on disk."""

    def test_sole_existing_positional(self, _repo: Path) -> None:
        assert _git_log_history(("-p", "f.go")) == ("f.go", None)

    def test_sole_existing_positional_with_count(self, _repo: Path) -> None:
        assert _git_log_history(("-p", "-3", "f.go")) == ("f.go", "3")

    def test_nonexistent_positional_is_a_revision(self, _repo: Path) -> None:
        # `git log -p HEAD~5` — the positional is a revision, not a file, so no path.
        assert _git_log_history(("-p", "HEAD~5")) is None
        assert _git_log_history(("-p", "missing.go")) is None

    def test_two_existing_positionals_block(self, _repo: Path) -> None:
        assert _git_log_history(("-p", "f.go", "g.go")) is None


class TestGhRunWatchNudge:
    """The one-shot steer from a manual ``gh run watch`` toward ``ccx vcs ship``."""

    def test_fires_on_gh_run_watch(self, tmp_path: Path) -> None:
        result = steer_gh_run_watch_to_ship(_bash_pre("gh run watch 123 --exit-status", tmp_path / "s"))
        assert result is not None
        assert result.action is Action.warn
        assert "ccx vcs ship" in result.message

    def test_second_invocation_is_silent(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"  # one shared session store, as the whole session shares
        first = steer_gh_run_watch_to_ship(_bash_pre("gh run watch 123 --exit-status", sd))
        assert first is not None and first.action is Action.warn
        # A later watch in the same session finds the latch set and stays quiet.
        assert steer_gh_run_watch_to_ship(_bash_pre("gh run watch 456 --exit-status", sd)) is None
        assert GhRunWatchNudged(fired=True) == _bash_pre("x", sd).ctx.s.load(GhRunWatchNudged)

    def test_matches_single_gh_run_watch(self) -> None:
        evt = _bash_pre("gh run watch 123 --exit-status")
        assert GhRunWatchSingle().check_command_line(evt, evt.command_line) is True

    def test_piped_or_chained_not_matched(self) -> None:
        # `json_guards`/`ship` own the pipe; a chained line is not a single command → no steer.
        for cmd in ("gh run watch 123 --exit-status | tee run.log", "cd repo && gh run watch 123"):
            evt = _bash_pre(cmd)
            assert GhRunWatchSingle().check_command_line(evt, evt.command_line) is False

    def test_gh_run_view_not_matched(self) -> None:
        # `gh run view --log-failed` is a legitimate failure drill-down, never a watch.
        evt = _bash_pre("gh run view 123 --log-failed")
        assert GhRunWatchSingle().check_command_line(evt, evt.command_line) is False

    def test_gh_pr_list_not_matched(self) -> None:
        evt = _bash_pre("gh pr list")
        assert GhRunWatchSingle().check_command_line(evt, evt.command_line) is False
