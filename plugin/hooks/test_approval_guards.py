"""Tests for the read-only approvers in ``approval_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_approval_guards.py

The inline ``tests={}`` cover full dispatch through the PermissionRequest harness;
here the condition internals get what the harness can't express: server-pin parsing
shapes, the raw-scan-before-structure ordering (a quoted ``"$(…)"`` survives every
structural check and only the raw scan rejects it), and — against the real binary —
that every allowlisted ``(family, op)`` is an op today's ccx actually registers.
"""

from __future__ import annotations

import subprocess
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine

from hooks.approval_guards import (
    READ_ONLY_CLI_OPS,
    READ_ONLY_MCP_TOOLS,
    CcxMcpReadOnly,
    CcxReadOnlyCli,
    CcxReplacePreview,
    McpTool,
    ccx_mcp_tool,
)
from hooks.common import ccx_bin, ccx_supports

EVT = SimpleNamespace()  # CcxReadOnlyCli.check_command_line never reads the event


class TestCcxMcpTool:
    @pytest.mark.parametrize(
        ("tool_name", "expected"),
        [
            pytest.param("mcp__cc-context__ccx_code_grep", "ccx_code_grep", id="direct_server"),
            pytest.param("mcp__plugin_cc-context_cc-context__ccx_code_read", "ccx_code_read", id="plugin_server"),
            pytest.param("mcp__evil__ccx_code_grep", None, id="foreign_server"),
            pytest.param("mcp__cc-context-evil__ccx_code_grep", None, id="server_suffix_extended"),
            pytest.param("mcp__xcc-context__ccx_code_grep", None, id="server_prefix_extended"),
            pytest.param("mcp__plugin_cc-context_cc-context_evil__ccx_code_grep", None, id="plugin_server_extended"),
            # an extra `__` stays in the tool suffix, which is then off every allowlist
            pytest.param("mcp__cc-context__ccx__code_grep", "ccx__code_grep", id="extra_segment_stays_in_tool"),
            pytest.param("ccx_code_grep", None, id="bare_name"),
            pytest.param("mcp__cc-context", None, id="missing_tool_part"),
            pytest.param("xmcp__cc-context__ccx_code_grep", None, id="prefix_not_mcp"),
            pytest.param("", None, id="empty"),
            pytest.param(None, None, id="none"),
        ],
    )
    def test_server_pin(self, tool_name: str | None, expected: str | None) -> None:
        assert ccx_mcp_tool(tool_name) == expected

    def test_extra_segment_tool_rejected_by_every_condition(self) -> None:
        evt = SimpleNamespace(tool_name="mcp__cc-context__ccx__code_grep", input=SimpleNamespace(raw={}))
        assert CcxMcpReadOnly().check(evt) is False
        assert CcxReplacePreview().check(evt) is False


class TestCcxReplacePreview:
    @pytest.mark.parametrize(
        ("tool_name", "tool_input", "expected"),
        [
            pytest.param("mcp__cc-context__ccx_code_replace", {"pattern": "a"}, True, id="apply_unset"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": False}, True, id="apply_false"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": True}, False, id="apply_true"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": "yes"}, False, id="apply_truthy_string"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": 1}, False, id="apply_truthy_int"),
            # a *string* "false" is truthy — a client sending it gets the dialog, fail-closed
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": "false"}, False, id="apply_string_false"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": 0}, True, id="apply_zero"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": ""}, True, id="apply_empty_string"),
            pytest.param("mcp__cc-context__ccx_code_replace", {"apply": None}, True, id="apply_none"),
            pytest.param("mcp__evil__ccx_code_replace", {"pattern": "a"}, False, id="foreign_server"),
            pytest.param("mcp__cc-context__ccx_code_grep", {"pattern": "a"}, False, id="wrong_tool"),
        ],
    )
    def test_preview_only(self, tool_name: str, tool_input: dict, expected: bool) -> None:
        evt = SimpleNamespace(tool_name=tool_name, input=SimpleNamespace(raw=tool_input))
        assert CcxReplacePreview().check(evt) is expected


class TestCcxReadOnlyCli:
    @pytest.mark.parametrize(
        ("command", "expected"),
        [
            pytest.param("ccx code grep foo", True, id="grep"),
            pytest.param('ccx repo find "*.go"', True, id="quoted_glob"),
            pytest.param("ccx vcs diff", True, id="vcs_diff"),
            pytest.param("ccx web read https://go.dev/doc --section 2", True, id="web_read"),
            pytest.param("ccx code replace 'a(x)' 'b(x)' internal/", True, id="replace_preview"),
            pytest.param("ccx code grep $(whoami)", False, id="command_substitution"),
            pytest.param("ccx code grep `whoami`", False, id="backtick_substitution"),
            # survives is_plain_argv (shlex and the parser both dequote), so only the raw scan rejects it
            pytest.param('ccx code grep "$(whoami)"', False, id="quoted_substitution"),
            pytest.param("ccx code read ${FILE}", False, id="variable_expansion"),
            pytest.param("ccx code grep $FILE", False, id="bare_variable_expansion"),
            pytest.param("ccx code read f.go --section $((1+1))", False, id="arithmetic_expansion"),
            # brace alone — no `$` or backtick — pins the `{` class of UNSAFE_EXPANSION
            pytest.param("ccx code read f.{go,py}", False, id="brace_expansion"),
            pytest.param("ccx code grep foo | tee /tmp/out", False, id="pipeline"),
            pytest.param("ccx vcs diff && rm -rf x", False, id="and_chain"),
            pytest.param("ccx code grep foo & rm -rf /", False, id="background_chain"),
            pytest.param("ccx vcs diff; rm -rf x", False, id="semicolon_chain"),
            pytest.param("ccx vcs diff || rm -rf x", False, id="or_chain"),
            pytest.param("ccx code grep foo\nrm -rf /", False, id="newline_chain"),
            pytest.param("ccx code grep foo > /tmp/out", False, id="redirect"),
            pytest.param("ccx code grep foo >> /tmp/out", False, id="append_redirect"),
            pytest.param("ccx code grep foo 2> /tmp/err", False, id="stderr_redirect"),
            pytest.param("ccx code grep foo < in", False, id="stdin_redirect"),
            pytest.param("ccx code grep <(cat /etc/passwd)", False, id="process_substitution"),
            pytest.param("ccx vcs diff > >(cat)", False, id="output_process_substitution"),
            pytest.param("CCX_DEBUG=1 ccx code grep foo", False, id="env_prefix"),
            pytest.param("/tmp/evil/ccx code grep foo", False, id="untrusted_path"),
            pytest.param("./ccx code grep foo", False, id="relative_path"),
            pytest.param("sudo ccx code grep foo", False, id="sudo_wrapper"),
            pytest.param("env ccx code grep foo", False, id="env_wrapper"),
            pytest.param("exec ccx code grep foo", False, id="exec_wrapper"),
            pytest.param("ccx code edit f.go --at 1-2#h --content x", False, id="edit"),
            pytest.param("ccx vcs ship -m msg", False, id="ship"),
            pytest.param("ccx exec 'print(1)'", False, id="exec"),
            pytest.param("ccx format -- ls", False, id="format"),
            pytest.param("ccx mcp", False, id="mcp_server"),
            # a bare `--` makes cobra pass what follows as positionals — the flag-injection
            # lane into the git/rg shell-outs, rejected outright
            pytest.param("ccx vcs show -- --output=/tmp/pwned", False, id="separator_flag_smuggle"),
            pytest.param("ccx code grep -- -foo", False, id="separator_bare"),
            pytest.param("ccx code replace 'a' 'b' --apply", False, id="replace_apply"),
            pytest.param("ccx code replace 'a' 'b' --apply=true", False, id="replace_apply_glued"),
            # startswith("--apply") is intentionally fail-closed: even --apply=false prompts
            pytest.param("ccx code replace a b --apply=false", False, id="replace_apply_false"),
            pytest.param("ccx code replace '$A' '$B' f.go", False, id="replace_metavariable"),
            pytest.param("ccx --budget 5 code read f.go", False, id="global_flag_first"),
            pytest.param("ccx code", False, id="missing_op"),
            pytest.param("ccx", False, id="bare_ccx"),
            # a degenerate line parses to primary=None; the single-command gate rejects
            # before primary is dereferenced, so these must reject without raising
            pytest.param("", False, id="empty_command"),
            pytest.param("   ", False, id="whitespace_command"),
        ],
    )
    def test_read_only(self, command: str, expected: bool) -> None:
        assert CcxReadOnlyCli().check_command_line(EVT, CommandLine.parse(command)) is expected

    def test_resolved_ccx_bin_path_allowed(self) -> None:
        ccx = ccx_bin()
        if ccx is None:
            pytest.skip("no ccx binary resolves")
        assert CcxReadOnlyCli().check_command_line(EVT, CommandLine.parse(f"{ccx} code grep foo")) is True

    # the startswith("--apply") fail-close scans for the literal long flag, which a
    # cobra short alias (-a) would walk straight past — sound only while replace
    # registers BoolVar, not BoolVarP
    def test_replace_apply_flag_has_no_short_alias(self) -> None:
        ccx = ccx_bin()
        if ccx is None:
            pytest.skip("no ccx binary resolves")
        proc = subprocess.run([ccx, "code", "replace", "--help"], capture_output=True, text=True)
        help_text = proc.stdout + proc.stderr
        assert "--apply" in help_text
        assert "-a, --apply" not in help_text


class TestAllowlists:
    # Exact-set equality, not a count: a typo'd, removed, or swapped-in entry must
    # fail the suite even when the size stays the same.
    def test_mcp_allowlist_exact(self) -> None:
        assert READ_ONLY_MCP_TOOLS == frozenset(
            {
                "ccx_code_search",
                "ccx_code_related",
                "ccx_code_outline",
                "ccx_code_read",
                "ccx_code_symbol",
                "ccx_code_deps",
                "ccx_code_grep",
                "ccx_repo_find",
                "ccx_repo_overview",
                "ccx_vcs_diff",
                "ccx_web_outline",
                "ccx_web_read",
                "ccx_web_search",
                "ccx_exec_tools",
            }
        )

    def test_cli_allowlist_exact(self) -> None:
        assert READ_ONLY_CLI_OPS == frozenset(
            {
                ("code", "read"),
                ("code", "outline"),
                ("code", "search"),
                ("code", "grep"),
                ("code", "symbol"),
                ("code", "grok"),
                ("code", "deps"),
                ("code", "related"),
                ("repo", "overview"),
                ("repo", "find"),
                ("repo", "locate"),
                ("vcs", "diff"),
                ("vcs", "show"),
                ("vcs", "history"),
                ("web", "outline"),
                ("web", "read"),
                ("web", "search"),
            }
        )

    @pytest.mark.parametrize("name", ["ccx_code_edit", "ccx_code_replace", "ccx_exec", "BashFormat"])
    def test_mutating_mcp_tools_excluded(self, name: str) -> None:
        assert name not in READ_ONLY_MCP_TOOLS

    @pytest.mark.parametrize(("family", "op"), sorted(READ_ONLY_CLI_OPS | {("code", "replace")}))
    def test_allowlisted_op_registered(self, family: str, op: str) -> None:
        if ccx_bin() is None:
            pytest.skip("no ccx binary resolves")
        assert ccx_supports(family, op), f"allowlisted `ccx {family} {op}` is not a registered ccx op"


class TestMcpToolVeto:
    """The ``skip_if`` veto on the CLI approver, pinned by both halves.

    ``Tool("Bash")`` suffix-matches ``mcp__srv__Bash``, and the carried command alone
    would approve — so the ``McpTool`` veto is the only thing between a foreign
    server's Bash lookalike and an auto-allow. The inline
    ``Input(tool="mcp__srv__Bash", …) -> Ask()`` covers the full dispatch.
    """

    def test_mcp_bash_lookalike_trips_the_veto(self) -> None:
        assert McpTool().check(SimpleNamespace(tool_name="mcp__srv__Bash")) is True

    def test_carried_command_alone_would_approve(self) -> None:
        assert CcxReadOnlyCli().check_command_line(EVT, CommandLine.parse("ccx code grep foo")) is True

    def test_native_bash_does_not_trip_the_veto(self) -> None:
        assert McpTool().check(SimpleNamespace(tool_name="Bash")) is False
