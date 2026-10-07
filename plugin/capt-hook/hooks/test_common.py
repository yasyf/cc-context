"""Tests for the shared guard-pack helpers in ``common.py``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_common.py

``LITERAL_SAFE`` is a pure regex; ``ccx_supports`` shells out to ``ccx … --help``, so
its boundary (``ccx_bin`` resolution and ``subprocess.run``) is monkeypatched — the
``functools.cache`` is cleared around every case so a probe result never leaks between them.
"""

from __future__ import annotations

import pytest
from captain_hook import CommandLine

from conftest import fake_run

from hooks import common
from hooks.common import LITERAL_SAFE, ccx_supports, gh_read, rewrote_text


class TestLiteralSafe:
    @pytest.mark.parametrize(
        "pattern",
        [
            "foo_bar",
            "foo bar",
            "a.b/c:d",
            "foo.bar",
            "user@host",
            "a,b",
            "key=value",
            "c++",
            "path/to-file",
        ],
    )
    def test_accepts_literal(self, pattern: str) -> None:
        assert LITERAL_SAFE.match(pattern)

    @pytest.mark.parametrize(
        "pattern",
        [
            "a|b",
            "^foo",
            "foo$",
            "a{2}",
            "(group)",
            "a?b",
            "*.go",
            "[abc]",
            "back\\slash",
            "semi;colon",
            "",
        ],
    )
    def test_rejects_metachar(self, pattern: str) -> None:
        assert not LITERAL_SAFE.match(pattern)

    def test_dot_whitelisted_star_is_the_rejector(self) -> None:
        assert LITERAL_SAFE.match("foo.bar")
        assert not LITERAL_SAFE.match("foo.*bar")
        assert not LITERAL_SAFE.match("foo*bar")


class TestCcxSupports:
    def test_true_on_rc0_and_flag_present(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout="usage: ccx code grep [--ignore-case] ..."))
        assert ccx_supports("code", "grep", flag="--ignore-case")

    def test_true_on_rc0_no_flag_requested(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout="usage: ccx web read ..."))
        assert ccx_supports("web", "read")

    def test_flag_matched_in_stderr(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", fake_run(0, stderr="  -i, --ignore-case   fold case"))
        assert ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_on_nonzero_rc(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", fake_run(1, stderr='unknown command "grep"'))
        assert not ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_on_missing_flag(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout="usage: ccx code grep [--glob G] ..."))
        assert not ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_when_ccx_unresolvable(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: None)

        def boom(*_args: object, **_kwargs: object) -> object:
            raise AssertionError("subprocess.run must not run when ccx is unresolvable")

        monkeypatch.setattr(common.subprocess, "run", boom)
        assert not ccx_supports("code", "grep", flag="--ignore-case")


@pytest.mark.parametrize(
    ("command", "reads"),
    [
        pytest.param("gh pr checks 12 --watch", True, id="pr-checks"),
        pytest.param("/opt/homebrew/bin/gh run view 34 --log-failed", True, id="absolute-gh"),
        pytest.param("gh api repos/o/r/pulls/12 --jq .state", True, id="api-get"),
        pytest.param("gh pr merge 12 --squash", False, id="pr-merge"),
        pytest.param("gh api -X GET repos/o/r/pulls", False, id="api-named-method"),
        pytest.param("gh api repos/o/r/issues/1/comments -fbody=hi", False, id="api-glued-field"),
        pytest.param("gh api graphql -f query=q", False, id="api-graphql"),
        pytest.param("gh pr", False, id="bare-group"),
        pytest.param("hub pr view 12", False, id="other-binary"),
    ],
)
def test_gh_read(command: str, reads: bool) -> None:
    assert gh_read(CommandLine.parse(command).primary) is reads


def test_rewrote_text_names_the_replacement_without_the_original() -> None:
    assert rewrote_text("ccx code read --full", "same content") == "Rewrote the command to `ccx code read --full`: same content."
