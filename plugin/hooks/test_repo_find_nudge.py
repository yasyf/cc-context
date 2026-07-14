"""Session-store tests for the broad-glob ``ccx repo find`` nudge.

The once-per-session latch needs a real :class:`~captain_hook.session.SessionStore` backed by a temp
dir — an inline ``capt-hook test`` event carries no session dir, so ``once`` there always reports
first-sight. Those first-sight/shape rows live inline in ``repo_find_nudge.py``; the repeat-suppression
and cross-surface (Bash + MCP) latch sharing live here.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from hooks.repo_find_nudge import BroadRepoFind, broad_glob, repo_find_glob

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
        assert BroadRepoFind().check(mcp_pre({"glob": "**"}, sd)) is False

    def test_mcp_fires_first(self, tmp_path: Path) -> None:
        assert BroadRepoFind().check(mcp_pre({"glob": "**"}, tmp_path / "s")) is True

    def test_non_broad_never_burns_the_latch(self, tmp_path: Path) -> None:
        sd = tmp_path / "s"
        assert BroadRepoFind().check(bash_pre('ccx repo find --scope internal "**"', sd)) is False
        assert BroadRepoFind().check(bash_pre('ccx repo find "internal/**"', sd)) is False
        # The first real broad find still fires — the shape check runs before the latch.
        assert BroadRepoFind().check(bash_pre('ccx repo find "**"', sd)) is True


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
            "question", "scoped", "ext", "charclass_literal", "pkg", "empty", "nested_brace_wontfix",
        ],
    )
    def test_broad_glob(self, glob: str, want: bool) -> None:
        assert broad_glob(glob) is want


class TestRepoFindGlob:
    def test_scoped_bash_returns_none(self) -> None:
        assert repo_find_glob(bash_pre('ccx repo find --scope internal "**"')) is None

    def test_empty_scope_is_unscoped(self) -> None:
        # An empty `--scope=` value is not a scope: the broad glob is still returned so the nudge fires.
        assert repo_find_glob(bash_pre('ccx repo find --scope= "**"')) == "**"

    def test_empty_scope_twotoken_is_unscoped(self) -> None:
        # Symmetric with `--scope=`: a two-token empty `--scope ''` is unscoped → the glob is returned.
        assert repo_find_glob(bash_pre("ccx repo find --scope '' \"**\"")) == "**"

    def test_budget_value_is_not_the_glob(self) -> None:
        # `--budget`'s value token must be skipped, not mistaken for the positional glob.
        assert repo_find_glob(bash_pre('ccx repo find --budget 2000 "**"')) == "**"

    def test_budget_before_scope_still_scoped(self) -> None:
        # The `--budget` value is consumed, so the later non-empty `--scope` still scopes → None.
        assert repo_find_glob(bash_pre('ccx repo find --budget 2000 --scope internal "**"')) is None

    def test_glued_nonempty_scope_returns_none(self) -> None:
        assert repo_find_glob(bash_pre('ccx repo find --scope=internal "**"')) is None

    def test_scoped_mcp_returns_none(self) -> None:
        assert repo_find_glob(mcp_pre({"glob": "**", "scope": "internal"})) is None

    def test_bash_glob_extracted(self) -> None:
        assert repo_find_glob(bash_pre('ccx repo find "internal/**"')) == "internal/**"

    def test_non_find_returns_none(self) -> None:
        assert repo_find_glob(bash_pre("ccx repo overview")) is None
