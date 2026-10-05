"""Tests for the once-per-agent rewrite notes, driven through the registered ``sed`` rewrite.

Run from the repo root with ``plugin/`` on the path::

    PYTHONPATH=plugin/capt-hook uv run --no-project --with capt-hook --with pytest \\
        python -m pytest plugin/capt-hook/hooks/test_rewrite_notes.py
"""

from __future__ import annotations

from pathlib import Path

import pytest
from captain_hook.app import _state
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks import sed_rewrites
from hooks.common import first_sight, rewrote_note

SED = "sed -n 10,40p f.go"
SED_NOTE = "Rewrote the command to `ccx code read --section`: same lines, token-bounded."


def bash(command: str, session_dir: Path, *, agent_id: str | None = None) -> PreToolUseEvent:
    raw: dict[str, object] = {"tool_name": "Bash", "tool_input": {"command": command}}
    if agent_id is not None:
        raw["agent_id"] = agent_id
    return PreToolUseEvent(
        _raw=raw, ctx=HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    )


@pytest.fixture
def sed_hook(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(sed_rewrites, "ccx_bin", lambda: "ccx")
    return next(
        h.handler
        for h in _state.hooks
        if h.handler and sed_rewrites.sed_to in (cell.cell_contents for cell in h.handler.__closure__ or ())
    )


def test_first_rewrite_carries_the_note_and_the_repeat_drops_it(sed_hook, tmp_path: Path) -> None:
    first, second = (sed_hook(bash(SED, tmp_path)) for _ in range(2))
    assert first.note == SED_NOTE
    assert second.note is None
    assert first.updated_input["command"] == second.updated_input["command"] == "ccx code read f.go --section 10-40"


def test_each_agent_sees_the_note_once(sed_hook, tmp_path: Path) -> None:
    assert sed_hook(bash(SED, tmp_path)).note == SED_NOTE
    assert sed_hook(bash(SED, tmp_path, agent_id="agent-a")).note == SED_NOTE
    assert sed_hook(bash(SED, tmp_path, agent_id="agent-a")).note is None
    assert sed_hook(bash(SED, tmp_path, agent_id="agent-b")).note == SED_NOTE


def test_each_note_kind_shows_once(tmp_path: Path) -> None:
    evt = bash("ls", tmp_path)
    find_note, cat_note = rewrote_note("ccx repo find", "same paths"), rewrote_note("ccx code read --full", "same content")
    assert [find_note(evt), cat_note(evt), find_note(evt), cat_note(evt)] == [
        "Rewrote the command to `ccx repo find`: same paths.",
        "Rewrote the command to `ccx code read --full`: same content.",
        None,
        None,
    ]


def test_a_new_session_shows_the_note_again(tmp_path: Path) -> None:
    assert first_sight(bash("ls", tmp_path / "a"), "note") == "note"
    assert first_sight(bash("ls", tmp_path / "a"), "note") is None
    assert first_sight(bash("ls", tmp_path / "b"), "note") == "note"
