"""Tests for the stateful ``WholePageWebFetch`` condition and the gated page-dump rewrite.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_web_guards.py

The ``once``-gated first-attempt-per-URL behavior needs a real ``SessionStore`` backed by a
temp dir — an inline capt-hook test event carries no session dir, so ``once`` there always
returns True and cannot express the same-URL-retry escape hatch. The pure helpers and the
stateless allows (localhost, scheme-less) are covered by the inline tests in web_guards.py.

The ``curl``/``wget`` → ``ccx web read`` rewrite is gated on ``ccx_supports("web", "read")``,
so its outcome is environment-dependent and cannot live in the inline ``tests={}``. Here the
probe boundary (``ccx_bin`` + ``subprocess.run``) is monkeypatched to force support on/off, with
the ``ccx_supports`` cache cleared around every case so a probe result never leaks between them.

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
from hooks.web_guards import (
    WholePageWebFetch,
    assignment_captured,
    page_dump_blocks,
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
    return evt, evt.command_line.occurrences[index]


def force_web_support(monkeypatch: pytest.MonkeyPatch, *, ok: bool) -> None:
    """Force the ``ccx_supports("web", "read")`` gate on/off around the page-dump rewrite.

    `page_dump_to` builds the command with `web_guards.ccx_bin`; `ccx_supports` probes via
    `common.ccx_bin` + `common.subprocess.run` — patch all three.
    """
    monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
    monkeypatch.setattr(web_guards, "ccx_bin", lambda: "/fake/ccx")
    rc, out = (0, "Usage: ccx web read <url> --full") if ok else (1, 'unknown command "web"')
    monkeypatch.setattr(common.subprocess, "run", fake_run(rc, stdout=out))


class TestAssignmentCapturedDump:
    """Assignment-captured page dumps are sinks, never readability rewrites."""

    @pytest.mark.parametrize(
        "command",
        [
            "H=$(curl -sL https://example.com/)",
            "export H=$(curl https://example.com/page)",
            "H=`curl -sL https://example.com/`",
            "H=$(wget -qO- https://example.com/)",
            "H=$(timeout 10 curl https://example.com/)",
            "H=$(curl https://example.com | jq .)",
            'H="$(curl -sL https://example.com/)"',
            "H=$( curl -sL https://example.com/ )",
            "H=$(VAR=1 curl https://example.com/)",
            'H="pre $(curl https://example.com/) post"',
            "H=x$(curl https://example.com/)y",
            "H=${X:-$(curl https://example.com/)}",
            "local h=$(curl https://example.com/)",
            "declare -r h=$(curl https://example.com/)",
            "H+=$(curl https://example.com/)",
            "H1=$(curl https://example.com/)",
        ],
    )
    def test_captured_dump_is_allowed_untouched(
        self, monkeypatch: pytest.MonkeyPatch, command: str
    ) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(command)
        assert assignment_captured(occ.line.raw, occ.command.span)
        assert page_dump_to(evt, occ) is None
        assert not page_dump_blocks(evt, occ)
        assert not web_guards.PageDumpToStdout().check(evt)

    def test_rewritable_sibling_survives_captured_assignment(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        force_web_support(monkeypatch, ok=True)
        evt = make_evt("A=$(true); curl https://example.com")
        _, curl_occ = evt.command_line.occurrences
        assert not assignment_captured(evt.command_line.raw, curl_occ.command.span)
        assert page_dump_to(evt, curl_occ) == "/fake/ccx web read https://example.com --full"
        assert web_guards.PageDumpToStdout().check(evt)

    def test_test_expression_host_keeps_todays_lane(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt = make_evt('[ -n "$(curl https://example.com)" ]')
        curl_occ = next(o for o in evt.command_line.occurrences if o.command.executable == "curl")
        assert not assignment_captured(evt.command_line.raw, curl_occ.command.span)
        assert page_dump_to(evt, curl_occ) == "/fake/ccx web read https://example.com --full"

    def test_completed_assignment_before_substitution_keeps_todays_lane(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # `H=1` completes before the substitution word starts — the dump isn't this assignment's value.
        force_web_support(monkeypatch, ok=True)
        evt = make_evt('export H=1 "$(curl https://example.com/)"')
        curl_occ = next(o for o in evt.command_line.occurrences if o.command.executable == "curl")
        assert not assignment_captured(evt.command_line.raw, curl_occ.command.span)
        assert page_dump_to(evt, curl_occ) == "/fake/ccx web read https://example.com/ --full"

    def test_captured_sibling_leaves_bare_dump_active(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt = make_evt("H=$(curl https://a.example/); curl -s https://b.example/")
        captured, bare = evt.command_line.occurrences
        assert assignment_captured(evt.command_line.raw, captured.command.span)
        assert page_dump_to(evt, captured) is None
        assert not page_dump_blocks(evt, captured)
        assert page_dump_to(evt, bare) == "/fake/ccx web read https://b.example/ --full"

    def test_accepted_hole_is_allowed(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt = make_evt('H=$(curl https://example.com/); echo "$H"')
        assert not web_guards.PageDumpToStdout().check(evt)

    def test_nested_substitution_is_parser_invisible(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ("H=$(echo $(curl https://example.com))")
        assert occ.command.executable == "echo"
        assert occ.command.args == ()
        assert page_dump_to(evt, occ) is None
        assert not page_dump_blocks(evt, occ)
        assert not web_guards.PageDumpToStdout().check(evt)


class TestPageDumpRewrite:
    """The ``curl``/``wget`` → ``ccx web read --full`` rewrite, gated on ``ccx_supports``."""

    @pytest.mark.parametrize(
        "command,url",
        [
            ("curl https://example.com", "https://example.com"),
            ("curl -sL https://example.com", "https://example.com"),
            ("curl -sSfL https://example.com/page", "https://example.com/page"),
            ("curl --silent --location https://example.com", "https://example.com"),
            ("curl --compressed https://example.com", "https://example.com"),
            ("wget -qO- https://example.com", "https://example.com"),
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

    def test_unsupported_falls_back_to_block(self, monkeypatch: pytest.MonkeyPatch) -> None:
        force_web_support(monkeypatch, ok=False)
        evt, occ = evt_occ("curl https://example.com")
        assert page_dump_to(evt, occ) is None

    @pytest.mark.parametrize(
        "command",
        [
            "curl -s https://api.example.com/v1/data",  # api.* host
            "curl https://example.com/data.json",  # .json path
            "curl https://example.com/api/v1",  # `api` path segment
            "curl -H 'X-Auth: t' https://example.com",  # header flag → un-mappable
            "curl --url https://example.com/x",  # --url spelling
            "timeout 10 curl https://example.com/x",  # wrapper prefix — occurrence_rewrite_url never unwraps
        ],
    )
    def test_supported_still_blocks_unmappable(
        self, monkeypatch: pytest.MonkeyPatch, command: str
    ) -> None:
        # Even with `ccx web read` present, an un-mappable shape returns None (→ block), proving
        # the block reason is the shape, not the gate.
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(command)
        assert page_dump_to(evt, occ) is None

    def test_compound_line_rewrites_the_curl_occurrence_only(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Deliberate flip: occurrences now judge independently, so `&&`'s curl still rewrites.
        force_web_support(monkeypatch, ok=True)
        evt = make_evt("curl https://example.com && echo done")
        curl_occ, echo_occ = evt.command_line.occurrences
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
        occ_a, occ_b = evt.command_line.occurrences
        pairs = [(occ_a, page_dump_to(evt, occ_a)), (occ_b, page_dump_to(evt, occ_b))]
        note = page_dump_note(evt, pairs)
        assert "ccx web read https://example.com/a --full" in note
        assert "ccx web read https://example.com/b --full" in note
        assert "ccx web outline <url>" in note
        assert 'ccx web search <url> "<question>"' in note


class TestApiUrlDecision:
    """Finding 7: the api/JSON heuristic reads the query string and the `/graphql` endpoint too,
    since readability would shred what those return — but a benign query must stay rewritable.

    Routed through the `to` builder (`page_dump_to`) that consumes the heuristic, with the
    `ccx web read` gate forced on so a None result is the api block, not the missing surface:
    an api URL falls back to the block (`page_dump_to` returns None), a non-api URL rewrites.
    """

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
    def test_api_url_falls_back_to_block(self, monkeypatch: pytest.MonkeyPatch, url: str) -> None:
        force_web_support(monkeypatch, ok=True)
        evt, occ = evt_occ(f"curl {shlex.quote(url)}")
        assert page_dump_to(evt, occ) is None

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
