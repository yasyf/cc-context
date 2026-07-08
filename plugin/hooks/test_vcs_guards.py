"""Tests for the ``_git_log_history`` path/count parser behind ``LogPatchDump``.

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
"""

from __future__ import annotations

from pathlib import Path

import pytest

from hooks.vcs_guards import _git_log_history


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
