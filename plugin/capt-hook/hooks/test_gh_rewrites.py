"""Splice tests for the ``gh_rewrites`` App-quota rewrite.

Run from the repo root against published capt-hook, with ``plugin/capt-hook`` on the path so
the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --no-project --with pytest --with capt-hook \
        pytest plugin/capt-hook/hooks/test_gh_rewrites.py

Each case drives the builder through the real occurrence splice, so the asserted string is the
whole command the shell would run. ``ccx_bin`` and the ``ccx vcs gh --help`` probe are pinned.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest
from conftest import make_evt, probe, run_visit

from hooks import common, gh_rewrites

if TYPE_CHECKING:
    from collections.abc import Callable


@pytest.fixture
def rewritten(monkeypatch: pytest.MonkeyPatch) -> Callable[[str], object]:
    monkeypatch.setattr(common, "ccx_bin", lambda: "ccx")
    monkeypatch.setattr(gh_rewrites, "ccx_bin", lambda: "ccx")
    probe(monkeypatch, "usage: ccx vcs gh -- <gh args>...")
    return lambda command: run_visit(make_evt(command), lambda evt, occ, _ctx: gh_rewrites.gh_app_to(evt, occ))


class TestGhAppTo:
    @pytest.mark.parametrize(
        "command, expected",
        [
            ("gh pr view 12 > /dev/stdout", "ccx vcs gh -- pr view 12 > /dev/stdout"),
            (
                "gh pr view 12 > /dev/stdout | python3 -c 'import json, sys; json.load(sys.stdin)'",
                "ccx vcs gh -- pr view 12 > /dev/stdout | python3 -c 'import json, sys; json.load(sys.stdin)'",
            ),
            ("gh api repos/o/r/pulls/12 2>/dev/null", "ccx vcs gh -- api repos/o/r/pulls/12 2>/dev/null"),
            ("gh pr list >> prs.txt 2>&1", "ccx vcs gh -- pr list >> prs.txt 2>&1"),
            ("2>/dev/null gh pr checks 12", "2>/dev/null ccx vcs gh -- pr checks 12"),
        ],
        ids=["stdout", "stdout-then-pipe", "stderr", "append-and-dup", "leading"],
    )
    def test_a_redirect_survives_the_rewrite_exactly_once(
        self, rewritten: Callable[[str], object], command: str, expected: str
    ) -> None:
        assert rewritten(command) == expected

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("gh pr checks 12", "ccx vcs gh -- pr checks 12"),
            ("gh  pr   checks 12", "ccx vcs gh -- pr checks 12"),
            (
                'gh pr list --search "é draft:false $USER" > prs.txt',
                'ccx vcs gh -- pr list --search "é draft:false $USER" > prs.txt',
            ),
            ("gh pr checks 12 && gh pr merge 12 --squash", "ccx vcs gh -- pr checks 12 && gh pr merge 12 --squash"),
        ],
        ids=["plain", "inner-spaces", "unicode-quotes-expansion", "write-left-alone"],
    )
    def test_the_arguments_are_carried_word_for_word(
        self, rewritten: Callable[[str], object], command: str, expected: str
    ) -> None:
        assert rewritten(command) == expected

    @pytest.mark.parametrize(
        "command",
        ["gh pr view > pr.txt 12", "gh pr merge 12 --squash > merge.txt", "GH_TOKEN=x gh pr checks 12 > checks.txt"],
        ids=["redirect-between-arguments", "write", "env-prefix"],
    )
    def test_a_shape_it_cannot_prove_is_left_alone(self, rewritten: Callable[[str], object], command: str) -> None:
        assert rewritten(command) is None
