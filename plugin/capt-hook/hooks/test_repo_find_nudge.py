"""Tests for the broad-glob ``ccx repo find`` nudge."""

from __future__ import annotations

from pathlib import Path

import pytest
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.repo_find_nudge import BroadRepoFind, broad_find, broad_glob, mcp_repo_find, repo_find_globs

MCP_TOOL = "mcp__plugin_cc-context_cc-context__ccx_repo_find"


def bash_pre(command: str, session_dir: Path | None = None) -> PreToolUseEvent:
    """A Bash ``PreToolUseEvent`` backed by ``session_dir`` for the once-per-session latch."""
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    return PreToolUseEvent(_raw={"tool_name": "Bash", "tool_input": {"command": command}}, ctx=ctx)


def mcp_pre(tool_input: dict[str, object], session_dir: Path | None = None) -> PreToolUseEvent:
    """An ``mcp__…__ccx_repo_find`` ``PreToolUseEvent`` backed by ``session_dir``."""
    ctx = HookContext(session=SessionStore(session_dir), transcript=None, settings=None)
    return PreToolUseEvent(_raw={"tool_name": MCP_TOOL, "tool_input": tool_input}, ctx=ctx)


class TestBroadRepoFind:
    def test_bash_broad_find_matches(self) -> None:
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"')) is True

    def test_mcp_broad_find_matches(self) -> None:
        assert BroadRepoFind().check(mcp_pre({"globs": ["**"]})) is True

    def test_anchored_find_does_not_match(self) -> None:
        assert BroadRepoFind().check(bash_pre('ccx repo find "internal/**"')) is False

    def test_globless_finds_do_not_match(self) -> None:
        assert BroadRepoFind().check(mcp_pre({})) is False
        assert BroadRepoFind().check(bash_pre("ccx repo find")) is False


class TestMcpRepoFind:
    @pytest.mark.parametrize(
        "tool, want",
        [
            ("mcp__cc-context__ccx_repo_find", True),
            ("mcp__plugin_cc-context_cc-context__ccx_repo_find", True),
            ("mcp__other__ccx_repo_find", False),
            ("mcp__cc-context__ccx_code_grep", False),
            ("Bash", False),
        ],
        ids=["direct", "plugin", "foreign_server", "other_tool", "not_mcp"],
    )
    def test_mcp_repo_find(self, tool: str, want: bool) -> None:
        assert mcp_repo_find(tool) is want


class TestBroadGlob:
    @pytest.mark.parametrize(
        "glob, want",
        [
            ("**", True),
            ("**/*", True),
            ("*", True),
            ("*/**", True),
            ("**/*.go", True),
            ("[a-z]/**", True),
            ("{a,b}/**", True),
            ("?/**", True),
            ("internal/**/*.go", False),
            ("*.go", False),
            ("[a-z]x/**", False),
            ("cmd/ccx/**", False),
            ("", False),
            ("{a,{b,c}}/**", False),
        ],
        ids=[
            "star2", "star2slashstar", "star", "starslash2", "star2_ext", "charclass", "brace",
            "question", "literal_first", "ext", "charclass_literal", "pkg", "empty", "nested_brace_wontfix",
        ],
    )
    def test_broad_glob(self, glob: str, want: bool) -> None:
        assert broad_glob(glob) is want


class TestBroadFind:
    @pytest.mark.parametrize(
        "globs, want",
        [
            ([], True),
            (["!*_test.go"], True),
            (["!vendor/**", "!*_test.go"], True),
            (["**"], True),
            (["*.go", "**"], True),
            (["internal/**", "**"], True),
            (["**", "!*_test.go"], True),
            (["internal"], False),
            (["*.go"], False),
            (["*.go", "!vendor/**"], False),
            (["internal/**", "cmd/**"], False),
        ],
        ids=[
            "empty", "exclude_only", "excludes_only_many", "star2", "broad_second", "broad_after_anchor",
            "broad_then_exclude", "bare_dir", "ext", "anchored_plus_exclusion", "two_anchored",
        ],
    )
    def test_broad_find(self, globs: list[str], want: bool) -> None:
        assert broad_find(globs) is want


class TestRepoFindGlobs:
    def test_budget_value_is_not_a_glob(self) -> None:
        assert repo_find_globs(bash_pre('ccx repo find --budget 2000 "**"')) == ["**"]

    def test_every_positional_is_collected(self) -> None:
        assert repo_find_globs(bash_pre("ccx repo find '*.go' '!vendor/**' '**'")) == ["*.go", "!vendor/**", "**"]

    def test_double_dash_is_not_a_glob(self) -> None:
        assert repo_find_globs(bash_pre('ccx repo find -- "**"')) == ["**"]

    def test_bash_glob_extracted(self) -> None:
        assert repo_find_globs(bash_pre('ccx repo find "internal/**"')) == ["internal/**"]

    @pytest.mark.parametrize(
        "command",
        ["ccx repo find", "ccx repo find --budget 2000"],
        ids=["bare", "flags_only"],
    )
    def test_bash_globless_find_is_none(self, command: str) -> None:
        assert repo_find_globs(bash_pre(command)) is None

    def test_mcp_takes_the_whole_list(self) -> None:
        assert repo_find_globs(mcp_pre({"globs": ["**", "!*_test.go"]})) == ["**", "!*_test.go"]

    @pytest.mark.parametrize(
        "tool_input",
        [{"globs": []}, {"globs": None}],
        ids=["empty_list", "null"],
    )
    def test_mcp_globless_find_is_empty_not_none(self, tool_input: dict[str, object]) -> None:
        assert repo_find_globs(mcp_pre(tool_input)) == []

    def test_mcp_missing_globs_key_is_none(self) -> None:
        assert repo_find_globs(mcp_pre({})) is None

    def test_mcp_non_list_globs_is_none(self) -> None:
        assert repo_find_globs(mcp_pre({"globs": "**"})) is None

    def test_non_find_returns_none(self) -> None:
        assert repo_find_globs(bash_pre("ccx repo overview")) is None
