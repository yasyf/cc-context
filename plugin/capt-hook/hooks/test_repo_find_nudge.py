"""Session-store tests for the broad-glob ``ccx repo find`` nudge.

The once-per-session latch needs a real :class:`~captain_hook.session.SessionStore` backed by a temp
dir — an inline ``capt-hook test`` event carries no session dir, so ``once`` there always reports
first-sight. Those first-sight/shape rows live inline in ``repo_find_nudge.py``; the repeat-suppression
and cross-surface (Bash + MCP) latch sharing live here, alongside the three units the nudge composes:
the server-name match, the glob-list parser, and the list-breadth predicate.
"""

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


class TestBroadRepoFindLatch:
    """The class-keyed ``once`` latch: one advisory per session, shared across Bash and MCP."""

    def test_second_broad_find_is_silent(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"  # one shared store, as the whole session shares
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"', sd)) is True
        assert BroadRepoFind().check(bash_pre('ccx repo find "**/*"', sd)) is False

    def test_bash_and_mcp_share_one_latch(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"', sd)) is True
        assert BroadRepoFind().check(mcp_pre({"globs": ["**"]}, sd)) is False

    def test_mcp_fires_first(self, tmp_path: Path) -> None:
        assert BroadRepoFind().check(mcp_pre({"globs": ["**"]}, tmp_path / "s")) is True

    def test_non_broad_never_burns_the_latch(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        assert BroadRepoFind().check(bash_pre('ccx repo find "internal/**"', sd)) is False
        # The first real broad find still fires — the shape check runs before the latch.
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"', sd)) is True

    def test_globless_mcp_find_never_burns_the_latch(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        # `globs` is required by the schema, so the server refuses this call — advising it would
        # spend the session's one advisory on a find that never runs.
        assert BroadRepoFind().check(mcp_pre({}, sd)) is False
        assert BroadRepoFind().check(mcp_pre({"globs": ["**"]}, sd)) is True

    def test_globless_bash_find_never_burns_the_latch(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        assert BroadRepoFind().check(bash_pre("ccx repo find", sd)) is False
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"', sd)) is True


class TestMcpRepoFind:
    @pytest.mark.parametrize(
        "tool, want",
        [
            ("mcp__cc-context__ccx_repo_find", True),  # direct-config server name
            ("mcp__plugin_cc-context_cc-context__ccx_repo_find", True),  # plugin-installed prefix
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
            ("**/*.go", True),  # pure-wildcard first segment
            ("[a-z]/**", True),  # char-class first segment counts as wildcard
            ("{a,b}/**", True),  # brace-group first segment counts as wildcard
            ("?/**", True),  # single-char wildcard first segment
            ("internal/**/*.go", False),  # literal first segment
            ("*.go", False),  # literal component in the first segment
            ("[a-z]x/**", False),  # a literal `x` outside the char-class anchors it
            ("cmd/ccx/**", False),
            ("", False),
            # WONTFIX (nudge miss): a nested-brace first segment slips past BROAD_SEGMENT → not broad.
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
            ([], True),  # no globs at all matches everything
            (["!*_test.go"], True),  # exclusion-only selects everything it doesn't exclude
            (["!vendor/**", "!*_test.go"], True),
            (["**"], True),
            (["*.go", "**"], True),  # a broad include in a later slot widens the whitelist
            (["internal/**", "**"], True),
            (["**", "!*_test.go"], True),  # an exclusion can't narrow a broad include back
            (["internal"], False),  # a bare directory is an anchored include
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
        # `--budget`'s value token must be skipped, not mistaken for a positional glob.
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
        # cobra.MinimumNArgs(1) refuses a positional-less find, so there is nothing to judge.
        assert repo_find_globs(bash_pre(command)) is None

    def test_mcp_takes_the_whole_list(self) -> None:
        assert repo_find_globs(mcp_pre({"globs": ["**", "!*_test.go"]})) == ["**", "!*_test.go"]

    @pytest.mark.parametrize(
        "tool_input",
        [{"globs": []}, {"globs": None}],
        ids=["empty_list", "null"],
    )
    def test_mcp_globless_find_is_empty_not_none(self, tool_input: dict[str, object]) -> None:
        # Both are calls the server runs, and both select everything — `[]`, never `None`.
        assert repo_find_globs(mcp_pre(tool_input)) == []

    def test_mcp_missing_globs_key_is_none(self) -> None:
        # The schema is `"required":["globs"]`, so this call is rejected before it lists anything.
        assert repo_find_globs(mcp_pre({})) is None

    def test_mcp_non_list_globs_is_none(self) -> None:
        # `globs` is typed `["null","array"]`, so a bare string is refused on type — same as absent.
        assert repo_find_globs(mcp_pre({"globs": "**"})) is None

    def test_non_find_returns_none(self) -> None:
        assert repo_find_globs(bash_pre("ccx repo overview")) is None
