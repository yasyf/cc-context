"""Tests for the stateful ``WholePageWebFetch`` condition and page-dump rewrite.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_web_guards.py

The ``once``-gated first-attempt-per-URL behavior needs a real ``SessionStore`` backed by a
temp dir — an inline capt-hook test event carries no session dir, so ``once`` there always
returns True and cannot express the same-URL-retry escape hatch. The pure helpers and the
stateless allows (localhost, scheme-less) are covered by the inline tests in web_guards.py.

The ``curl``/``wget`` → ``ccx web read`` rewrite is gated on ``ccx_supports("web", "read")``.
The probe boundary (``ccx_bin`` + ``subprocess.run``) is monkeypatched here to force support
on/off.

``page_dump_to``/``page_dump_note`` are occurrence-scoped (``rewrite_command_occurrences``), so
each takes an ``Occurrence`` alongside the event rather than a whole-line stand-in. ``evt_occ``
parses a command line and hands back the ``(evt, Occurrence)`` pair each builder takes — the
same convention :mod:`test_cat_rewrites` established for its own occurrence-scoped rewrite.
"""

from __future__ import annotations

import shlex
from pathlib import Path

import pytest
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from conftest import fake_run, make_evt
from hooks import common, web_guards
from cc_transcript.command import Occurrence

from hooks.web_guards import (
    WholePageWebFetch,
    page_dump_note,
    page_dump_to,
)


def webfetch_event(url: str, session_dir: Path) -> PreToolUseEvent:
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    return PreToolUseEvent(_raw={"tool_name": "WebFetch", "tool_input": {"url": url, "prompt": "how do I X"}}, ctx=ctx)


class TestWholePageWebFetch:
    def test_first_attempt_blocks(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        evt = webfetch_event("https://docs.example.com/guide", tmp_path / "s1")
        assert cond.check(evt)

    def test_same_url_retry_passes(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        first = webfetch_event("https://docs.example.com/guide", session)
        second = webfetch_event("https://docs.example.com/guide", session)
        assert cond.check(first)  # first sight → block
        assert not cond.check(second)  # deliberate re-run of the same URL → escape hatch, passes

    def test_distinct_urls_each_block_first_time(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        assert cond.check(webfetch_event("https://a.example.com/x", session))
        assert cond.check(webfetch_event("https://b.example.com/y", session))  # a different URL is a fresh first sight

    def test_retry_isolated_per_session(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        url = "https://docs.example.com/guide"
        assert cond.check(webfetch_event(url, tmp_path / "sessionA"))
        assert cond.check(webfetch_event(url, tmp_path / "sessionB"))  # a new session re-arms the block

    def test_local_never_blocks_or_records(self, tmp_path: Path) -> None:
        cond = WholePageWebFetch()
        session = tmp_path / "s1"
        assert not cond.check(webfetch_event("http://localhost:3000/health", session))
        assert not cond.check(webfetch_event("http://127.0.0.1:8080/metrics", session))


def evt_occ(command: str, index: int = 0):
    evt = make_evt(command)
    return evt, evt.cmd.line.occurrences[index]


def force_web_support(monkeypatch: pytest.MonkeyPatch, *, ok: bool) -> None:
    """Force the ``ccx_supports("web", "read")`` gate on/off around the page-dump rewrite.

    `page_dump_to` builds the command with `web_guards.ccx_bin`; `ccx_supports` probes via
    `common.ccx_bin` + `common.subprocess.run` — patch all three.
    """
    monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
    monkeypatch.setattr(web_guards, "ccx_bin", lambda: "/fake/ccx")
    rc, out = (0, "Usage: ccx web read <url> --full") if ok else (1, 'unknown command "web"')
    monkeypatch.setattr(common.subprocess, "run", fake_run(rc, stdout=out))


class TestPageDumpRewrite:
    """The ``curl``/``wget`` → ``ccx web read --full`` rewrite, gated on ``ccx_supports``."""

    @pytest.mark.parametrize(
        "command,url",
        [
            ("curl https://example.com", "https://example.com"),
            ("curl -- https://example.com", "https://example.com"),
            ("curl -sL https://example.com", "https://example.com"),
            ("curl -s https://example.com 2>/dev/null", "https://example.com"),
            ("curl https://example.com 2>/dev/null", "https://example.com"),
            ("curl -sSL https://example.com/page", "https://example.com/page"),
            ("curl --silent --location https://example.com", "https://example.com"),
            ("curl http://10.0.0.5/status", "http://10.0.0.5/status"),
            ("wget -qO- https://example.com", "https://example.com"),
            ("wget -qO- -- https://example.com", "https://example.com"),
            ("wget -qO - https://example.com", "https://example.com"),
            ("wget -O - https://example.com", "https://example.com"),
            ("wget --output-document=- https://example.com", "https://example.com"),
            ("wget -q -O - https://example.com", "https://example.com"),
        ],
    )
    def test_supported_rewrites_to_web_read(
        self, monkeypatch: pytest.MonkeyPatch, command: str, url: str
    ) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(command)
        assert page_dump_to(evt, occ) == f"/fake/ccx web read {url} --full"

    def test_query_string_url_is_shell_quoted(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ("curl 'https://example.com/p?a=1&b=2'")
        assert page_dump_to(evt, occ) == "/fake/ccx web read 'https://example.com/p?a=1&b=2' --full"

    def test_benign_query_string_still_rewrites(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Finding 7: a query string that is *not* JSON/GraphQL (`?page=2`) still rewrites — the
        # api heuristic must not block every URL that happens to carry a query.
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ("curl -s 'https://example.com/list?page=2'")
        assert page_dump_to(evt, occ) == "/fake/ccx web read 'https://example.com/list?page=2' --full"

    def test_unsupported_surface_does_not_rewrite(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=False)
        evt, occ = evt_occ("curl https://example.com")
        assert page_dump_to(evt, occ) is None

    def test_unresolvable_binary_does_not_rewrite(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(web_guards, "ccx_bin", lambda: None)
        evt, occ = evt_occ("curl https://example.com")
        assert page_dump_to(evt, occ) is None

    @pytest.mark.parametrize(
        "command",
        [
            "curl -H 'X-Auth: t' https://example.com",
            "curl --url https://example.com/x",
            "curl -f https://example.com",
            "curl --compressed https://example.com",
            "timeout 10 curl https://example.com/x",
            "sudo curl https://example.com/x",
            "env TOKEN=x curl https://example.com/x",
            "TOKEN=x curl https://example.com/x",
        ],
    )
    def test_non_plain_shape_does_not_rewrite(
        self, monkeypatch: pytest.MonkeyPatch, command: str
    ) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(command)
        assert page_dump_to(evt, occ) is None

    @pytest.mark.parametrize(
        "command",
        [
            "timeout 10 curl https://example.com/x",
            "sudo curl https://example.com/x",
            "env TOKEN=x curl https://example.com/x",
            "TOKEN=x curl https://example.com/x",
            'curl -H "Token: $(get-token)" https://example.com/x',
            'echo "$(curl https://example.com/x)"',
        ],
    )
    def test_ambiguous_shape_does_not_fire(self, command: str) -> None:
        assert not web_guards.PageDumpToStdout().check(make_evt(command))

    def test_compound_line_rewrites_the_curl_occurrence_only(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Deliberate flip: occurrences now judge independently, so `&&`'s curl still rewrites.
        force_web_support(monkeypatch, ok=True)
        evt = make_evt("curl https://example.com && echo done")
        curl_occ, echo_occ = evt.cmd.line.occurrences
        assert page_dump_to(evt, curl_occ) == "/fake/ccx web read https://example.com --full"
        assert page_dump_to(evt, echo_occ) is None

    def test_note_names_target_and_steers(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ("curl -sL https://example.com/guide")
        text = page_dump_to(evt, occ)
        note = page_dump_note(evt, [(occ, text)])
        assert "ccx web read https://example.com/guide --full" in note
        assert "NOT the raw HTML" in note
        assert "ccx web outline https://example.com/guide" in note
        assert "ccx web search https://example.com/guide" in note

    def test_note_falls_back_to_placeholder_for_multiple_urls(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Two distinct URLs across pairs: one trailing suggestion can't name both, so it stays generic.
        force_web_support(monkeypatch, ok=True)
        evt = make_evt("curl https://example.com/a && curl https://example.com/b")
        occ_a, occ_b = evt.cmd.line.occurrences
        pairs = [(occ_a, page_dump_to(evt, occ_a)), (occ_b, page_dump_to(evt, occ_b))]
        note = page_dump_note(evt, pairs)
        assert "ccx web read https://example.com/a --full" in note
        assert "ccx web read https://example.com/b --full" in note
        assert "ccx web outline <url>" in note
        assert 'ccx web search <url> "<question>"' in note


class TestApiUrlDecision:
    """API-shaped URLs do not fire the page guard; benign page URLs still rewrite."""

    @pytest.mark.parametrize(
        "url",
        [
            "https://api.example.com/x",  # api.* host
            "https://example.com/data.json",  # .json path
            "https://example.com/api/v1",  # `api` path segment
            "https://example.com/graphql",  # /graphql endpoint
            "https://example.com/v1/graphql/",  # trailing slash tolerated
            "https://example.com/data?format=json",  # json in query
            "https://example.com/q?query=graphql",  # graphql in query
            "https://example.com/x?a=1&out=JSON",  # case-insensitive query match
        ],
    )
    def test_api_url_does_not_fire(self, monkeypatch: pytest.MonkeyPatch, url: str) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(f"curl {shlex.quote(url)}")
        assert page_dump_to(evt, occ) is None
        assert not web_guards.PageDumpToStdout().check(evt)

    @pytest.mark.parametrize(
        "url",
        [
            "https://example.com/guide",
            "https://example.com/list?page=2",  # benign query — must still be rewritable
            "https://example.com/article?ref=home",
            "https://example.com/graphql-tutorial",  # not the /graphql endpoint, and no api signal
        ],
    )
    def test_non_api_url_rewrites(self, monkeypatch: pytest.MonkeyPatch, url: str) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(f"curl {shlex.quote(url)}")
        assert page_dump_to(evt, occ) == f"/fake/ccx web read {shlex.quote(url)} --full"
