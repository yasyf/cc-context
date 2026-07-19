"""Tests for the stateful re-read / edit-then-read gate.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_session_state.py

The recorder, gate, and reset are exercised end to end against a real ``SessionStore``
backed by a temp session dir and real files on disk (the store internals are never
mocked); only the tool-input payload is synthesized.
"""

from __future__ import annotations

import os
from pathlib import Path

from captain_hook import Action
from captain_hook.context import HookContext
from captain_hook.events import PostToolUseEvent, PreCompactEvent, PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.session_state import (
    LOG_CAP,
    FileAccessLog,
    file_digest,
    gate_reread,
    lookup,
    record_file_access,
    remember,
    reset_on_compact,
)

MAIN_T = "/transcripts/main.jsonl"  # every agent shares one transcript_path in real payloads
AGENT_A = "agent-aaaa"
AGENT_B = "agent-bbbb"


def make_file(path: Path, size: int = 64) -> Path:
    """Create a real file with deterministic content so it can be stat'd and fingerprinted."""
    path.write_text("x" * size)
    return path


def ctx(session_dir: Path) -> HookContext:
    return HookContext(session=SessionStore(session_dir), transcript=None, settings=None)


def read_pre(
    path: Path,
    session_dir: Path,
    *,
    main: str = MAIN_T,
    agent_id: str | None = None,
    offset: int | None = None,
    limit: int | None = None,
    pages: str | None = None,
) -> PreToolUseEvent:
    tool_input: dict[str, object] = {"file_path": str(path)}
    if offset is not None:
        tool_input["offset"] = offset
    if limit is not None:
        tool_input["limit"] = limit
    if pages is not None:
        tool_input["pages"] = pages
    raw: dict[str, object] = {"tool_name": "Read", "tool_input": tool_input, "transcript_path": main}
    if agent_id is not None:
        raw["agent_id"] = agent_id
    return PreToolUseEvent(_raw=raw, ctx=ctx(session_dir))


def post(
    tool: str,
    path: Path,
    session_dir: Path,
    *,
    main: str = MAIN_T,
    agent_id: str | None = None,
    offset: int | None = None,
    limit: int | None = None,
    pages: str | None = None,
    **extra: object,
) -> PostToolUseEvent:
    tool_input: dict[str, object] = {"file_path": str(path), **extra}
    if offset is not None:
        tool_input["offset"] = offset
    if limit is not None:
        tool_input["limit"] = limit
    if pages is not None:
        tool_input["pages"] = pages
    raw: dict[str, object] = {"tool_name": tool, "tool_input": tool_input, "transcript_path": main}
    if agent_id is not None:
        raw["agent_id"] = agent_id
    return PostToolUseEvent(_raw=raw, ctx=ctx(session_dir))


def precompact(session_dir: Path, *, main: str = MAIN_T) -> PreCompactEvent:
    return PreCompactEvent(_raw={"transcript_path": main, "trigger": "auto"}, ctx=ctx(session_dir))


class TestFirstAccess:
    def test_first_full_read_allows_and_records(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        assert gate_reread(read_pre(f, sd)) is None  # nothing logged yet
        record_file_access(post("Read", f, sd))
        record = lookup(read_pre(f, sd), f.resolve())
        assert record is not None
        assert record.kind == "read"
        assert record.digest == file_digest(f)

    def test_sliced_read_is_not_recorded(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd, offset=1, limit=40))
        assert lookup(read_pre(f, sd), f.resolve()) is None

    def test_paged_read_is_not_recorded(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "notes.txt")  # a `pages` window, even on a text path, is partial
        record_file_access(post("Read", f, sd, pages="1-5"))
        assert lookup(read_pre(f, sd), f.resolve()) is None

    def test_media_read_is_not_recorded(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "diagram.png")
        record_file_access(post("Read", f, sd))
        assert lookup(read_pre(f, sd), f.resolve()) is None


class TestGate:
    def test_full_reread_blocks(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))
        result = gate_reread(read_pre(f, sd))
        assert result is not None
        assert result.action is Action.block
        assert "already read" in result.message
        assert str(f.resolve()) in result.message

    def test_sliced_reread_allows(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))
        assert gate_reread(read_pre(f, sd, offset=10, limit=100)) is None

    def test_edit_then_full_read_blocks(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Edit", f, sd, old_string="x", new_string="y"))
        result = gate_reread(read_pre(f, sd))
        assert result is not None
        assert result.action is Action.block
        assert "you just edited" in result.message
        assert "ccx vcs diff" in result.message

    def test_write_then_full_read_blocks(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Write", f, sd, content="new"))
        result = gate_reread(read_pre(f, sd))
        assert result is not None
        assert result.action is Action.block
        assert "you just edited" in result.message

    def test_unseen_path_allows(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        record_file_access(post("Read", make_file(tmp_path / "a.txt"), sd))
        other = make_file(tmp_path / "b.txt")
        assert gate_reread(read_pre(other, sd)) is None


class TestPagedAndMedia:
    """Finding #1 — PDF/image page-range and media reads must never be gated as redundant."""

    def test_second_pdf_page_range_allowed(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "report.pdf")
        record_file_access(post("Read", f, sd, pages="1-5"))  # first page range
        assert gate_reread(read_pre(f, sd, pages="6-10")) is None  # second range must pass
        assert gate_reread(read_pre(f, sd, pages="1-5")) is None  # even the same range passes

    def test_pdf_full_read_after_pages_allowed(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "report.pdf")
        record_file_access(post("Read", f, sd, pages="1-5"))
        assert gate_reread(read_pre(f, sd)) is None  # a no-`pages` PDF read is still media -> pass

    def test_image_reread_allowed(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "diagram.png")
        record_file_access(post("Read", f, sd))
        assert gate_reread(read_pre(f, sd)) is None

    def test_media_gate_bypasses_even_when_recorded(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "diagram.png")
        remember(read_pre(f, sd), f.resolve(), "read", file_digest(f))  # force a record past the recorder
        assert gate_reread(read_pre(f, sd)) is None  # gate still refuses to block media


class TestContentFingerprint:
    """Finding #2 — a content change reopens the file even when mtime/size are preserved."""

    def test_content_change_preserving_mtime_allows(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = tmp_path / "v.txt"
        f.write_text("version: 1")
        record_file_access(post("Read", f, sd))
        orig = f.stat()
        f.write_text("version: 2")  # same byte length as "version: 1"
        os.utime(f, (orig.st_mtime, orig.st_mtime))  # restore original mtime
        after = f.stat()
        assert after.st_mtime == orig.st_mtime  # mtime preserved
        assert after.st_size == orig.st_size  # size preserved
        assert gate_reread(read_pre(f, sd)) is None  # content differs -> must pass

    def test_external_content_change_allows(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))
        f.write_text("y" * 128)  # a real out-of-session edit
        assert gate_reread(read_pre(f, sd)) is None

    def test_mtime_bump_same_content_still_blocks(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))
        bumped = f.stat().st_mtime + 100
        os.utime(f, (bumped, bumped))  # touch: mtime moves, content identical
        result = gate_reread(read_pre(f, sd))
        assert result is not None
        assert result.action is Action.block  # identical content is still in context


class TestContextIsolation:
    """Finding #3 — one agent's read/edit must never block another agent's first read."""

    def test_subagent_read_does_not_block_main(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"  # one shared session store, as a subagent and its parent share
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd, agent_id=AGENT_A))  # subagent reads
        assert gate_reread(read_pre(f, sd)) is None  # main agent's first read

    def test_subagent_edit_does_not_block_main(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Edit", f, sd, agent_id=AGENT_A, old_string="x", new_string="y"))
        assert gate_reread(read_pre(f, sd)) is None  # main agent's first read of a subagent-edited file

    def test_main_read_does_not_block_subagent(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))  # main agent reads
        assert gate_reread(read_pre(f, sd, agent_id=AGENT_A)) is None  # subagent's first read

    def test_sibling_subagents_isolated(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd, agent_id=AGENT_A))
        assert gate_reread(read_pre(f, sd, agent_id=AGENT_B)) is None  # a different subagent's first read

    def test_subagent_reread_blocks_same_subagent(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd, agent_id=AGENT_A))
        result = gate_reread(read_pre(f, sd, agent_id=AGENT_A))  # same context re-read
        assert result is not None
        assert result.action is Action.block


class TestPathNormalization:
    def test_relative_and_absolute_spellings_collapse(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        (tmp_path / "sub").mkdir()
        f = make_file(tmp_path / "a.txt")
        detour = tmp_path / "sub" / ".." / "a.txt"  # resolves to the same file
        record_file_access(post("Read", detour, sd))
        result = gate_reread(read_pre(f, sd))
        assert result is not None
        assert result.action is Action.block


class TestCompactionReset:
    def test_precompact_reset_reopens_reads(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        f = make_file(tmp_path / "a.txt")
        record_file_access(post("Read", f, sd))
        assert gate_reread(read_pre(f, sd)) is not None  # blocked before compaction
        reset_on_compact(precompact(sd))
        assert gate_reread(read_pre(f, sd)) is None  # log wiped -> re-read allowed


class TestBound:
    def test_log_capped_oldest_evicted(self, tmp_path: Path) -> None:
        evt = read_pre(make_file(tmp_path / "seed.txt"), tmp_path / "s")
        for i in range(LOG_CAP + 10):
            remember(evt, Path(f"/repo/file-{i}.txt"), "read", f"digest-{i}")
        log = evt.ctx.s.load(FileAccessLog)
        assert len(log.accesses) == LOG_CAP
        paths = {r.path for r in log.accesses}
        assert "/repo/file-0.txt" not in paths  # oldest evicted
        assert f"/repo/file-{LOG_CAP + 9}.txt" in paths  # newest kept
