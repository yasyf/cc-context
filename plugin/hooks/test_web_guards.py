"""Tests for the stateful ``WholePageWebFetch`` condition.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_web_guards.py

The ``once``-gated first-attempt-per-URL behavior needs a real ``SessionStore`` backed by a
temp dir — an inline capt-hook test event carries no session dir, so ``once`` there always
returns True and cannot express the same-URL-retry escape hatch. The pure helpers and the
stateless allows (localhost, scheme-less) are covered by the inline tests in web_guards.py.
"""

from __future__ import annotations

from pathlib import Path

from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.web_guards import WholePageWebFetch


def _event(url: str, session_dir: Path) -> PreToolUseEvent:
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    return PreToolUseEvent(_raw={"tool_name": "WebFetch", "tool_input": {"url": url, "prompt": "how do I X"}}, ctx=ctx)


class TestWholePageWebFetch:
    def test_first_attempt_blocks(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        evt = _event("https://docs.example.com/guide", tmp_path / "s1")
        assert cond.check(evt)

    def test_same_url_retry_passes(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        first = _event("https://docs.example.com/guide", session)
        second = _event("https://docs.example.com/guide", session)
        assert cond.check(first)  # first sight → block
        assert not cond.check(second)  # deliberate re-run of the same URL → escape hatch, passes

    def test_distinct_urls_each_block_first_time(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        assert cond.check(_event("https://a.example.com/x", session))
        assert cond.check(_event("https://b.example.com/y", session))  # a different URL is a fresh first sight

    def test_retry_isolated_per_session(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        url = "https://docs.example.com/guide"
        assert cond.check(_event(url, tmp_path / "sessionA"))
        assert cond.check(_event(url, tmp_path / "sessionB"))  # a new session re-arms the block

    def test_local_never_blocks_or_records(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        assert not cond.check(_event("http://localhost:3000/health", session))
        assert not cond.check(_event("http://127.0.0.1:8080/metrics", session))
