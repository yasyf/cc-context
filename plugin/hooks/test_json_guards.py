"""Tests for the ``ccx toon`` guard helpers and the learned-nudge condition.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_json_guards.py

The pure helpers are tested directly; the stateful ``SeenEmittingJson`` condition is
exercised with a real ``SessionStore`` and a real shapes file under a temp
``$CAPTAIN_HOOK_STATE_DIR`` (the store internals are never mocked).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from captain_hook import CommandLine
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.common import (
    MAX_SHAPES,
    command_shape,
    has_json_output_flag,
    looks_like_json,
    load_shapes,
    record_shape,
)
from hooks.json_guards import SeenEmittingJson


@pytest.fixture(autouse=True)
def _state_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Point the shapes store at an isolated temp dir for every test."""
    monkeypatch.setenv("CAPTAIN_HOOK_STATE_DIR", str(tmp_path))
    return tmp_path


def shape(command: str) -> str:
    return command_shape(CommandLine.parse(command))


class TestCommandShape:
    def test_argument_values_collapse(self) -> None:
        assert shape("gh issue view 123") == shape("gh issue view 456")

    def test_flag_values_collapse(self) -> None:
        assert shape("gh pr list --json number") == shape("gh pr list --json title")

    def test_flag_order_collapses(self) -> None:
        assert shape("kubectl get pods -o json -n kube") == shape("kubectl get pods -n kube -o json")

    def test_distinct_subcommands_differ(self) -> None:
        assert shape("gh issue view 1") != shape("gh pr view 1")

    def test_distinct_flags_differ(self) -> None:
        assert shape("gh pr list --json number") != shape("gh pr list --state open")

    def test_shape_value(self) -> None:
        assert shape("gh pr list --json number") == "gh pr list --json"


class TestHasJsonOutputFlag:
    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number",
            "kubectl get pods -o json",
            "kubectl get pods -ojson",
            "kubectl get pods -o=json",
            "terraform output --output json",
            "terraform output --output=json",
            "docker inspect x --format json",
            "docker inspect x --format=json",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert has_json_output_flag(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "ls -la",
            "kubectl get pods -o wide",
            "kubectl get pods -o yaml",
            "docker inspect x --format '{{.Id}}'",
            "terraform output",
            "gh pr list --state open",
            "cat foo.json",
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not has_json_output_flag(CommandLine.parse(command))


class TestLooksLikeJson:
    @pytest.mark.parametrize(
        "text",
        [
            '{"a": 1}',
            "  [1, 2, 3]  ",
            '[{"id": 1}, {"id": 2}]',
            '{"a": 1}\n{"a": 2}\n',  # NDJSON
            '\n{"a": 1}\n\n{"a": 2}\n',  # NDJSON with blank lines
            '"a string"',
            "42",
            "true",
            "null",
        ],
    )
    def test_json(self, text: str) -> None:
        assert looks_like_json(text)

    @pytest.mark.parametrize(
        "text",
        [
            "",
            "   ",
            "[this is prose that starts with a bracket]",
            "{not json}",
            "plain log line\nanother log line",
            "Error: something failed",
            '{"a": 1} trailing garbage',
            '[1, 2,',  # truncated
        ],
    )
    def test_not_json(self, text: str) -> None:
        assert not looks_like_json(text)


class TestShapesStore:
    def test_round_trip(self) -> None:
        assert load_shapes() == set()
        record_shape("gh pr list --json")
        record_shape("kubectl get pods -o")
        assert load_shapes() == {"gh pr list --json", "kubectl get pods -o"}

    def test_idempotent(self) -> None:
        record_shape("gh pr list --json")
        record_shape("gh pr list --json")
        assert load_shapes() == {"gh pr list --json"}

    def test_atomic_write_leaves_no_temp(self, _state_dir: Path) -> None:
        record_shape("gh pr list --json")
        assert not list(_state_dir.glob("*.tmp"))

    def test_bound_enforced_fifo(self, _state_dir: Path) -> None:
        for i in range(MAX_SHAPES + 10):
            record_shape(f"tool-{i}")
        shapes = load_shapes()
        assert len(shapes) == MAX_SHAPES
        # oldest evicted, newest kept
        assert "tool-0" not in shapes
        assert f"tool-{MAX_SHAPES + 9}" in shapes
        # the on-disk order is oldest-first, newest-last
        order = json.loads((_state_dir / "ccx-json-shapes.json").read_text())
        assert order[-1] == f"tool-{MAX_SHAPES + 9}"

    def test_rerecord_moves_to_newest(self, _state_dir: Path) -> None:
        for i in range(MAX_SHAPES):
            record_shape(f"tool-{i}")
        record_shape("tool-0")  # touch the oldest
        record_shape("fresh")  # push one over the cap
        shapes = load_shapes()
        assert "tool-0" in shapes  # survived eviction by being touched
        assert "tool-1" not in shapes  # became the new oldest, evicted


class TestSeenEmittingJson:
    def _event(self, command: str, session_dir: Path) -> PreToolUseEvent:
        ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
        return PreToolUseEvent(_raw={"tool_name": "Bash", "tool_input": {"command": command}}, ctx=ctx)

    def test_unseen_shape_does_not_fire(self, tmp_path: Path) -> None:
        cond = SeenEmittingJson()
        evt = self._event("terraform output", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)

    def test_recorded_shape_fires_once_per_session(self, tmp_path: Path) -> None:
        record_shape(shape("gh issue view 123"))
        cond = SeenEmittingJson()
        session = tmp_path / "s1"
        first = self._event("gh issue view 123", session)
        second = self._event("gh issue view 456", session)  # same shape, different value
        assert cond.check_command_line(first, first.command_line)
        assert not cond.check_command_line(second, second.command_line)  # self-gated within session

    def test_recorded_shape_fires_again_in_new_session(self, tmp_path: Path) -> None:
        record_shape(shape("terraform output"))
        cond = SeenEmittingJson()
        a = self._event("terraform output", tmp_path / "sessionA")
        b = self._event("terraform output", tmp_path / "sessionB")
        assert cond.check_command_line(a, a.command_line)
        assert cond.check_command_line(b, b.command_line)

    def test_already_wrapped_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self._event("ccx toon -- terraform output", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)

    def test_piped_command_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self._event("terraform output | jq .", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)
