"""Tests for the shared guard-pack helpers in ``common.py``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_common.py

``LITERAL_SAFE`` is a pure regex; ``ccx_supports`` shells out to ``ccx … --help``, so
its boundary (``ccx_bin`` resolution and ``subprocess.run``) is monkeypatched — the
``functools.cache`` is cleared around every case so a probe result never leaks between them.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from hooks import common
from hooks.common import LITERAL_SAFE, ccx_supports


class TestLiteralSafe:
    @pytest.mark.parametrize(
        "pattern",
        [
            "foo_bar",  # word chars + underscore
            "foo bar",  # space
            "a.b/c:d",  # dot, slash, colon
            "foo.bar",  # a bare dot is whitelisted — it is not a glob metachar for tilth
            "user@host",
            "a,b",
            "key=value",
            "c++",  # `+` is whitelisted
            "path/to-file",  # trailing `-` in the class is a literal hyphen
        ],
    )
    def test_accepts_literal(self, pattern: str) -> None:
        assert LITERAL_SAFE.match(pattern)

    @pytest.mark.parametrize(
        "pattern",
        [
            "a|b",  # BRE alternation
            "^foo",  # anchor
            "foo$",  # anchor
            "a{2}",  # quantifier
            "(group)",  # group
            "a?b",  # glob/regex metachar
            "*.go",  # glob metachar
            "[abc]",  # class
            "back\\slash",  # escape
            "semi;colon",  # not in the whitelist
            "",  # empty never matches — `+` requires ≥1 char
        ],
    )
    def test_rejects_metachar(self, pattern: str) -> None:
        assert not LITERAL_SAFE.match(pattern)

    def test_dot_whitelisted_star_is_the_rejector(self) -> None:
        # `.` is in the whitelist, so a bare dot passes; `foo.*bar` fails *only* because of `*`.
        assert LITERAL_SAFE.match("foo.bar")
        assert not LITERAL_SAFE.match("foo.*bar")
        assert not LITERAL_SAFE.match("foo*bar")


def _fake_run(returncode: int, stdout: str = "", stderr: str = ""):
    def run(*_args: object, **_kwargs: object) -> SimpleNamespace:
        return SimpleNamespace(returncode=returncode, stdout=stdout, stderr=stderr)

    return run


class TestCcxSupports:
    @pytest.fixture(autouse=True)
    def _clear_cache(self):
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def test_true_on_rc0_and_flag_present(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout="usage: ccx code grep [--ignore-case] ..."))
        assert ccx_supports("code", "grep", flag="--ignore-case")

    def test_true_on_rc0_no_flag_requested(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout="usage: ccx web read ..."))
        assert ccx_supports("web", "read")

    def test_flag_matched_in_stderr(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stderr="  -i, --ignore-case   fold case"))
        assert ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_on_nonzero_rc(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", _fake_run(1, stderr='unknown command "grep"'))
        assert not ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_on_missing_flag(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout="usage: ccx code grep [--glob G] ..."))
        assert not ccx_supports("code", "grep", flag="--ignore-case")

    def test_false_when_ccx_unresolvable(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common, "ccx_bin", lambda: None)

        def _boom(*_args: object, **_kwargs: object) -> object:
            raise AssertionError("subprocess.run must not run when ccx is unresolvable")

        monkeypatch.setattr(common.subprocess, "run", _boom)
        assert not ccx_supports("code", "grep", flag="--ignore-case")
