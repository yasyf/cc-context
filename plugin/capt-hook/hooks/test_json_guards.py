"""Tests for the ``ccx format`` guard helpers and the learned-nudge condition.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_json_guards.py

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
from captain_hook.events import PostToolUseEvent, PreToolUseEvent
from captain_hook.session import SessionStore
from cc_transcript.command import Occurrence

import hooks.json_guards as json_guards
from conftest import fake_run
from hooks import common
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
from hooks.json_guards import SeenEmittingJson, record_json_shape, wraps


@pytest.fixture(autouse=True)
def state_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Point the shapes store at an isolated temp dir for every test."""
    monkeypatch.setenv("CAPTAIN_HOOK_STATE_DIR", str(tmp_path))
    return tmp_path


def fake_evt(repo_root: Path | None = None) -> SimpleNamespace:
    """A minimal event for the durable store — global scope reads only ``ctx.repo_root``."""
    return SimpleNamespace(ctx=SimpleNamespace(repo_root=repo_root))


def shape(command: str) -> str:
    return command_shape(CommandLine.parse(command))


def occurrence(command: str, *, index: int = 0) -> Occurrence:
    return CommandLine.parse(command).occurrences[index]


class TestWraps:
    def test_plain_single_command(self) -> None:
        assert wraps(occurrence("gh pr list --json number"))

    def test_subshell_inner_occurrence_is_plain(self) -> None:
        assert wraps(occurrence("(gh pr list --json number)"))

    def test_compound_gates_each_occurrence(self) -> None:
        cl = CommandLine.parse("ccx format -- gh pr list --json x; gh issue list --json number; printf done")
        assert not wraps(cl.occurrences[0])
        assert wraps(cl.occurrences[1])
        assert not wraps(cl.occurrences[2])

    def test_streaming_sibling_does_not_suppress_rewrite(self) -> None:
        cl = CommandLine.parse("gh pr list --json number; kubectl get pods -o json --watch")
        assert wraps(cl.occurrences[0])
        assert not wraps(cl.occurrences[1])

    def test_ccx_executable_is_never_wrappable(self) -> None:
        assert not wraps(occurrence("/opt/homebrew/bin/ccx exec --json x"))

    def test_wrapper_around_ccx_format_is_never_wrappable(self) -> None:
        assert not wraps(occurrence("sudo ccx format -- gh pr list --json x"))

    def test_time_prefix_wraps_the_inner_argv(self) -> None:
        assert wraps(occurrence("time gh pr list --json number"))

    def test_non_ascii_argument_wraps(self) -> None:
        assert wraps(occurrence('printf done; gh pr list --search "café" --json number', index=1))

    def test_bare_substitution_never_wraps(self) -> None:
        assert not wraps(occurrence("gh pr view --json x --repo $(git remote get-url origin)"))

    def test_wrap_json_uses_occurrence_raw(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(json_guards, "ccx_bin", lambda: "/tmp/ccx binary")
        monkeypatch.setattr(common.subprocess, "run", fake_run(1, stderr='unknown command "gh"'))
        occ = occurrence('printf done; gh pr list --json number --search "is:open draft:false"', index=1)
        assert json_guards.wrap_json(SimpleNamespace(), occ) == (
            "'/tmp/ccx binary' format -- gh pr list --json number --search \"is:open draft:false\""
        )

    def test_wrap_json_reads_gh_on_the_app_quota(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(json_guards, "ccx_bin", lambda: "/tmp/ccx binary")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/tmp/ccx binary")
        monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout="usage: ccx vcs gh -- <gh args>..."))
        occ = occurrence('printf done; gh pr list --json number --search "is:open draft:false"', index=1)
        assert json_guards.wrap_json(SimpleNamespace(), occ) == (
            "'/tmp/ccx binary' format -- '/tmp/ccx binary' vcs gh -- pr list --json number "
            '--search "is:open draft:false"'
        )


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
        assert has_streaming_flag(CommandLine.parse(command).primary)

    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number",
            "kubectl get pods -o json",
            "gh run watch 123 --json status",
        ],
    )
    def test_negative(self, command: str) -> None:
        assert not has_streaming_flag(CommandLine.parse(command).primary)


class TestIsPlainArgv:
    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json number",
            'gh pr list --json number --search "is:open draft:false"',
            "gh pr list --json x --limit $N",
            'gh pr list --json number --search "$(cat q.txt)"',
            'gh pr list --search "café ü" --json number',
            "gh  pr list\t--json number",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert is_plain_argv(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "GH_HOST=x.example.com gh pr list --json number",
            "(gh pr list --json number)",
            "time gh pr list --json number",
            "exec gh pr list --json number",
            "eval gh pr list --json number",
            "source render.sh --json",
            ". render.sh --json",
            "gh pr view --json x --repo $(git remote get-url origin)",
            "kubectl get pods -o json > pods.json",
            "gh pr list --json number # trailing",
            "printf done; gh pr list --json number",
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
            "ccx format -- gh pr list --json x",
            "/opt/homebrew/bin/ccx format -- kubectl get pods -o json",
            "ccx format -- gh pr list --json x | jq .",
            "sudo ccx format -- gh pr list --json x",
        ],
    )
    def test_positive(self, command: str) -> None:
        assert already_wrapped(CommandLine.parse(command))

    @pytest.mark.parametrize(
        "command",
        [
            "gh pr list --json x",
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
            '{"a": 1}\n{"a": 2}\n',
            '\n{"a": 1}\n\n{"a": 2}\n',
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
            '[1, 2,',
        ],
    )
    def test_not_json(self, text: str) -> None:
        assert not looks_like_json(text)

    @pytest.mark.parametrize("value", [b'{"a": 1}', b'{"a": 1}\n{"a": 2}\n'])
    def test_bytes_json(self, value: bytes) -> None:
        assert looks_like_json(value)

    @pytest.mark.parametrize("value", [{"stdout": "{}"}, None, 42, ["{}"], b"", b"plain log"])
    def test_non_text_returns_false(self, value: object) -> None:
        assert not looks_like_json(value)


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

    def test_bound_enforced_fifo(self, state_dir: Path) -> None:
        evt = fake_evt()
        for i in range(256 + 10):
            record_shape(evt, f"tool-{i}")
        shapes = load_shapes(evt)
        assert len(shapes) == 256
        assert "tool-0" not in shapes
        assert "tool-265" in shapes
        store = state_dir / "hooks" / "durable" / "global" / "json_shapes.json"
        assert json.loads(store.read_text())["shapes"][-1] == "tool-265"

    def test_rerecord_moves_to_newest(self, state_dir: Path) -> None:
        evt = fake_evt()
        for i in range(256):
            record_shape(evt, f"tool-{i}")
        record_shape(evt, "tool-0")
        record_shape(evt, "fresh")
        shapes = load_shapes(evt)
        assert "tool-0" in shapes
        assert "tool-1" not in shapes


class TestSeenEmittingJson:
    def pre_event(self, command: str, session_dir: Path) -> PreToolUseEvent:
        ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
        return PreToolUseEvent(_raw={"tool_name": "Bash", "tool_input": {"command": command}}, ctx=ctx)

    def test_unseen_shape_does_not_fire(self, tmp_path: Path) -> None:
        cond = SeenEmittingJson()
        evt = self.pre_event("terraform output", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.cmd.line)

    def test_recorded_shape_fires_once_per_session(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("gh issue view 123"))
        cond = SeenEmittingJson()
        session = tmp_path / "s1"
        first = self.pre_event("gh issue view 123", session)
        second = self.pre_event("gh issue view 456", session)
        assert cond.check_command_line(first, first.cmd.line)
        assert not cond.check_command_line(second, second.cmd.line)

    def test_recorded_shape_fires_again_in_new_session(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        a = self.pre_event("terraform output", tmp_path / "sessionA")
        b = self.pre_event("terraform output", tmp_path / "sessionB")
        assert cond.check_command_line(a, a.cmd.line)
        assert cond.check_command_line(b, b.cmd.line)

    def test_already_wrapped_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self.pre_event("ccx format -- terraform output", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.cmd.line)

    def test_piped_command_does_not_fire(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("terraform output"))
        cond = SeenEmittingJson()
        evt = self.pre_event("terraform output | jq .", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.cmd.line)

    def test_ccx_shape_never_fires_even_if_recorded(self, tmp_path: Path) -> None:
        record_shape(fake_evt(), shape("ccx exec 'async def main(): return 1'"))
        cond = SeenEmittingJson()
        evt = self.pre_event("ccx exec 'async def main(): return 2'", tmp_path / "s1")
        assert not cond.check_command_line(evt, evt.cmd.line)


class TestRecordJsonShape:
    """`record_json_shape` reads the real Bash `tool_response`, which is a dict.

    Claude Code delivers a Bash result as `{"stdout": ..., "stderr": ...,
    "interrupted": ...}` — despite the event's `str | None` typing — so the recorder
    must pull `stdout` from the mapping and never `.strip()` the dict itself. The
    prior test fed no `tool_response` at all, so the crash went unseen.
    """

    RESP = {"stdout": "", "stderr": "", "interrupted": False, "isImage": False, "noOutputExpected": False}

    def post_event(self, command: str, tool_response: object, session_dir: Path) -> PostToolUseEvent:
        ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
        return PostToolUseEvent(
            _raw={"tool_name": "Bash", "tool_input": {"command": command}, "tool_response": tool_response},
            ctx=ctx,
        )

    def test_dict_json_stdout_records_shape(self, tmp_path: Path) -> None:
        evt = self.post_event("terraform output", {**self.RESP, "stdout": '{"a": 1}'}, tmp_path / "s1")
        record_json_shape(evt)
        assert load_shapes(evt) == {shape("terraform output")}

    def test_dict_plain_text_stdout_records_nothing(self, tmp_path: Path) -> None:
        evt = self.post_event("which ccx", {**self.RESP, "stdout": "/opt/homebrew/bin/ccx\nv0.6.1"}, tmp_path / "s2")
        record_json_shape(evt)
        assert load_shapes(evt) == set()

    def test_string_json_stdout_still_records_shape(self, tmp_path: Path) -> None:
        evt = self.post_event("kubectl get pods -o wide", '[{"id": 1}, {"id": 2}]', tmp_path / "s3")
        record_json_shape(evt)
        assert load_shapes(evt) == {shape("kubectl get pods -o wide")}

    def test_empty_dict_stdout_records_nothing(self, tmp_path: Path) -> None:
        evt = self.post_event("echo hi", self.RESP, tmp_path / "s4")
        record_json_shape(evt)
        assert load_shapes(evt) == set()
