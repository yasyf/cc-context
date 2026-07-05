"""Tests for the ``ccx format`` guard helpers and the learned-nudge condition.

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
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.common import (
    already_wrapped,
    command_shape,
    has_json_output_flag,
    has_streaming_flag,
    head_has_json_output_flag,
    is_ccx_command,
    is_plain_argv,
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


def fake_evt(repo_root: Path | None = None) -> SimpleNamespace:
    """A minimal event for the durable store — global scope reads only ``ctx.repo_root``."""
    return SimpleNamespace(ctx=SimpleNamespace(repo_root=repo_root))


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


class TestHasStreamingFlag:
    @pytest.mark.parametrize(
        "command",
        [
            "kubectl get pods -o json --watch",
            "kubectl get pods -o json --watch=true",
            "kubectl get pods -o json -w",
            "docker events --format json --follow",
            "some-tool --json -f",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert has_streaming_flag(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number",
            "kubectl get pods -o json",
            "gh run watch 123 --json status",  # `watch` here is a subcommand, not a flag
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not has_streaming_flag(CommandLine.parse(command))


class TestIsPlainArgv:
    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number",
            'gh pr list --json number --search "is:open draft:false"',
            "gh pr list --json x --limit $N",  # bash expands $N after the wrap's --
            # A quoted substitution survives the word-split comparison verbatim;
            # bash expands the spliced raw text after the wrap's -- identically.
            'gh pr list --json number --search "$(cat q.txt)"',
        ],
    )
    def test_positive(self, command: str) -> None:
        assert is_plain_argv(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            # Env prefix: spliced after `ccx format --`, the assignment execs as
            # argv[0] — "executable file not found in $PATH".
            "GH_HOST=x.example.com gh pr list --json number",
            # Subshell: bare parens after `--` are a bash syntax error.
            "(gh pr list --json number)",
            # Shell keyword: `time` after `--` stops being a keyword.
            "time gh pr list --json number",
            # Builtins with no binary counterpart fail as literal argv[0]s.
            "exec gh pr list --json number",
            "eval gh pr list --json number",
            "source render.sh --json",
            ". render.sh --json",
            # Command substitution the parser folded out of args — bail conservatively.
            "gh pr view --json x --repo $(git remote get-url origin)",
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not is_plain_argv(CommandLine.parse(command))


class TestHeadHasJsonOutputFlag:
    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number,title | jq '.[].title'",
            "kubectl get pods -o json | python3 -c 'pass'",
            # head args still carry --json after the wrap — callers must pair this
            # helper with already_wrapped, as exec_guards' JsonPipedToFilter does.
            "ccx format -- gh pr list --json x | jq .",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert head_has_json_output_flag(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "ps aux | awk '{print $1}'",
            "terraform output | jq .",
            "cat pods.json | jq '.items'",
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not head_has_json_output_flag(CommandLine.parse(command))


class TestAlreadyWrapped:
    @pytest.mark.parametrize(
        "command",
        [
            # The wrapped line still carries --json; failing to recognize the wrap
            # would make the json_guards rewrite re-wrap its own output forever.
            "ccx format -- gh pr list --json x",
            "/opt/homebrew/bin/ccx format -- kubectl get pods -o json",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert already_wrapped(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json x",  # bare JSON-flagged command — still gets rewritten
            "ccx repo overview",
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not already_wrapped(CommandLine.parse(command))


class TestIsCcxCommand:
    @pytest.mark.parametrize(
        "command",
        [
            "ccx exec 'async def main(): return 1'",
            "bin/ccx exec --list-tools",
            "/opt/homebrew/bin/ccx repo overview",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert is_ccx_command(CommandLine.parse(command))

    @pytest.mark.parametrize("command", ["gh pr list --json x", "ccxfoo bar"])
    def test_negative(self, command: str) -> None:
        assert not is_ccx_command(CommandLine.parse(command))


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
        evt = fake_evt()
        assert load_shapes(evt) == set()
        record_shape(evt, "gh pr list --json")
        record_shape(evt, "kubectl get pods -o")
        assert load_shapes(evt) == {"gh pr list --json", "kubectl get pods -o"}

    def test_idempotent(self) -> None:
        evt = fake_evt()
        record_shape(evt, "gh pr list --json")
        record_shape(evt, "gh pr list --json")
        assert load_shapes(evt) == {"gh pr list --json"}

    def test_bound_enforced_fifo(self, _state_dir: Path) -> None:
        evt = fake_evt()
        for i in range(256 + 10):
            record_shape(evt, f"tool-{i}")
        shapes = load_shapes(evt)
        assert len(shapes) == 256
        # oldest evicted, newest kept
        assert "tool-0" not in shapes
        assert "tool-265" in shapes
        # the on-disk order is oldest-first, newest-last
        store = _state_dir / "hooks" / "durable" / "global" / "json_shapes.json"
        assert json.loads(store.read_text())["shapes"][-1] == "tool-265"

    def test_rerecord_moves_to_newest(self, _state_dir: Path) -> None:
        evt = fake_evt()
        for i in range(256):
            record_shape(evt, f"tool-{i}")
        record_shape(evt, "tool-0")  # touch the oldest
        record_shape(evt, "fresh")  # push one over the cap
        shapes = load_shapes(evt)
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
        record_shape(fake_evt(), shape("gh issue view 123"))
        cond = SeenEmittingJson()
        session = tmp_path / "s1"
        first = self._event("gh issue view 123", session)
        second = self._event("gh issue view 456", session)  # same shape, different value
        assert cond.check_command_line(first, first.command_line)
        assert not cond.check_command_line(second, second.command_line)  # self-gated within session

    def test_recorded_shape_fires_again_in_new_session(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        a = self._event("terraform output", tmp_path / "sessionA")
        b = self._event("terraform output", tmp_path / "sessionB")
        assert cond.check_command_line(a, a.command_line)
        assert cond.check_command_line(b, b.command_line)

    def test_already_wrapped_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self._event("ccx format -- terraform output", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)

    def test_piped_command_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self._event("terraform output | jq .", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)

    def test_ccx_shape_never_fires_even_if_recorded(self, tmp_path: Path) -> None:
        # The durable store is global and long-lived: a `ccx exec` shape recorded
        # before ccx commands were excluded must not nudge wrapping ccx in ccx format.
        record_shape(fake_evt(), shape("ccx exec 'async def main(): return 1'"))
        cond = SeenEmittingJson()
        evt = self._event("ccx exec 'async def main(): return 2'", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.command_line)
