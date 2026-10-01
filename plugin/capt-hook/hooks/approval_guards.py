"""Auto-approve the read-only ccx surface (MCP tools, replace previews, plain read-only CLI calls)."""

from __future__ import annotations

from captain_hook import (
    Allow,
    Ask,
    BaseHookEvent,
    CommandLine,
    CustomCommandLineCondition,
    CustomCondition,
    Event,
    Input,
    Tool,
    approve,
)

from .common import CCX_SERVERS, ccx_bin, command_expands, is_plain_argv, is_single_command

READ_ONLY_MCP_TOOLS = frozenset(
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

READ_ONLY_CLI_OPS = frozenset(
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

EXPANSION_MARKERS = ("`", "$", "{", "<(", ">(")


def ccx_mcp_tool(tool_name: str | None) -> str | None:
    """Return the tool suffix of a server-pinned cc-context MCP name, else ``None``."""
    if not tool_name:
        return None
    match tool_name.split("__", 2):
        case ["mcp", server, tool] if server in CCX_SERVERS:
            return tool
        case _:
            return None


class CcxMcpReadOnly(CustomCondition):
    """Matches the strictly read-only cc-context MCP tools, server-pinned by exact name.

    Any call carrying a truthy ``reveal_secrets`` is excluded — every masking
    surface (read, grep, symbol, outline, diff) exposes the escape hatch, and it
    falls through to the dialog on all of them, mirroring the CLI
    ``--reveal-secrets`` carve-out below.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        if evt.input.raw.get("reveal_secrets"):
            return False
        return ccx_mcp_tool(evt.tool_name) in READ_ONLY_MCP_TOOLS


class CcxReplacePreview(CustomCondition):
    """Matches a cc-context ``ccx_code_replace`` preview — ``apply`` unset or falsy.

    A truthy check, not a boolean compare: ``apply`` set to any truthy value
    (``true``, ``"yes"``, ``1``) prompts.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        return ccx_mcp_tool(evt.tool_name) == "ccx_code_replace" and not evt.input.raw.get("apply")


class CcxReadOnlyCli(CustomCommandLineCondition):
    """Matches one plain ``ccx <family> <op> …`` on the read-only allowlist.

    In order: no expansion in the raw text, exactly one command whose raw text is
    its argv (no pipe, redirect, chain, or env prefix), the executable is literally
    ``ccx`` or the resolved :func:`ccx_bin` path (a bare ``Path(…).name`` match
    would approve ``/tmp/evil/ccx``), no bare ``--`` separator anywhere in the args,
    and ``(family, op)`` is on the literal allowlist. Global flags before the family
    (``ccx --budget 5 code read``) fall through to the dialog.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not (is_single_command(cl) and is_plain_argv(cl)):
            return False
        if command_expands(cl.primary, EXPANSION_MARKERS):
            return False
        if cl.primary.executable != "ccx" and cl.primary.executable != ccx_bin():
            return False
        args = tuple(word.value for word in cl.primary.words[1:])
        if "--" in args:
            return False
        if len(args) < 2:
            return False
        if any(a.startswith("--reveal-secrets") for a in args):
            return False
        if (args[0], args[1]) == ("code", "replace"):
            return not any(a.startswith("--apply") for a in args)
        return (args[0], args[1]) in READ_ONLY_CLI_OPS


class McpTool(CustomCondition):
    """Matches MCP-server tools (``mcp__<server>__<tool>``), which Tool() suffix-matching also accepts."""

    def check(self, evt: BaseHookEvent) -> bool:
        return bool(evt.tool_name) and evt.tool_name.startswith("mcp__")


approve(
    "ccx read-only mcp",
    events=Event.PermissionRequest,
    only_if=[CcxMcpReadOnly()],
    tests={
        Input(tool="mcp__cc-context__ccx_code_grep", tool_input={"pattern": "TODO"}): Allow(explicit=True),
        Input(tool="mcp__cc-context__ccx_vcs_diff", tool_input={}): Allow(explicit=True),
        Input(tool="mcp__cc-context__ccx_exec_tools", tool_input={}): Allow(explicit=True),
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_code_read", tool_input={"path": "main.go"}): Allow(
            explicit=True
        ),
        Input(tool="mcp__cc-context__ccx_code_read", tool_input={"path": "main.go", "reveal_secrets": True}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_read", tool_input={"path": "main.go", "reveal_secrets": False}): Allow(
            explicit=True
        ),
        Input(tool="mcp__cc-context__ccx_code_grep", tool_input={"text": "KEY", "reveal_secrets": True}): Ask(),
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_vcs_diff", tool_input={"reveal_secrets": True}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_symbol", tool_input={"name": "x", "reveal_secrets": True}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_outline", tool_input={"path": "f.go", "reveal_secrets": True}): Ask(),
        Input(
            tool="mcp__plugin_cc-context_cc-context__ccx_web_search",
            tool_input={"url": "https://go.dev", "query": "generics"},
        ): Allow(explicit=True),
        Input(tool="mcp__cc-context__ccx_code_edit", tool_input={"path": "main.go", "at": "1-2#h"}): Ask(),
        Input(tool="mcp__cc-context__ccx_exec", tool_input={"script": "1"}): Ask(),
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_exec", tool_input={"script": "1"}): Ask(),
        Input(tool="mcp__cc-context__BashFormat", tool_input={"command": "ls"}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b"}): Ask(),
        Input(tool="mcp__evil__ccx_code_grep", tool_input={"pattern": "TODO"}): Ask(),
        Input(tool="ccx_code_grep", tool_input={"pattern": "TODO"}): Ask(),
    },
)


approve(
    "ccx replace preview",
    events=Event.PermissionRequest,
    only_if=[CcxReplacePreview()],
    tests={
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b"}): Allow(
            explicit=True
        ),
        Input(
            tool="mcp__plugin_cc-context_cc-context__ccx_code_replace",
            tool_input={"pattern": "a", "rewrite": "b", "apply": False},
        ): Allow(explicit=True),
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b", "apply": True}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b", "apply": "false"}): Ask(),
        Input(tool="mcp__evil__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b"}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_grep", tool_input={"pattern": "a"}): Ask(),
    },
)


approve(
    "ccx read-only cli",
    events=Event.PermissionRequest,
    only_if=[Tool("Bash"), CcxReadOnlyCli()],
    skip_if=[McpTool()],
    tests={
        Input(command="ccx code grep foo"): Allow(explicit=True),
        Input(command='ccx repo find "*.go"'): Allow(explicit=True),
        Input(command="ccx vcs diff"): Allow(explicit=True),
        Input(command="ccx web read https://go.dev/doc --section 2"): Allow(explicit=True),
        Input(command="ccx code read f.go"): Allow(explicit=True),
        Input(command="ccx code replace 'fmt.Println(x)' 'slog.Info(x)' internal/"): Allow(explicit=True),
        Input(command="ccx code grep $(whoami)"): Ask(),
        Input(command="ccx code grep `whoami`"): Ask(),
        Input(command='ccx code grep "$(whoami)"'): Ask(),
        Input(command="ccx code read ${FILE}"): Ask(),
        Input(command="ccx code grep $FILE"): Ask(),
        Input(command="ccx code read f.go --section $((1+1))"): Ask(),
        Input(command="ccx code read f.{go,py}"): Ask(),
        Input(command=""): Ask(),
        Input(command="   "): Ask(),
        Input(command="ccx code grep foo | tee /tmp/out"): Ask(),
        Input(command="ccx vcs diff && rm -rf x"): Ask(),
        Input(command="ccx code grep foo & rm -rf /"): Ask(),
        Input(command="ccx vcs diff ; rm -rf x"): Ask(),
        Input(command="ccx vcs diff || rm -rf x"): Ask(),
        Input(command="ccx code grep foo\nrm -rf /"): Ask(),
        Input(command="ccx code grep foo > /tmp/out"): Ask(),
        Input(command="ccx code grep foo >> /tmp/out"): Ask(),
        Input(command="ccx code grep foo 2> /tmp/err"): Ask(),
        Input(command="ccx code grep foo < in"): Ask(),
        Input(command="ccx code grep <(cat /etc/passwd)"): Ask(),
        Input(command="ccx vcs diff > >(cat)"): Ask(),
        Input(command="CCX_DEBUG=1 ccx code grep foo"): Ask(),
        Input(command="/tmp/evil/ccx code grep foo"): Ask(),
        Input(command="./ccx code grep foo"): Ask(),
        Input(command="sudo ccx code grep foo"): Ask(),
        Input(command="env ccx code grep foo"): Ask(),
        Input(command="exec ccx code grep foo"): Ask(),
        Input(command="ccx code edit f.go --at 1-2#h --content x"): Ask(),
        Input(command="ccx vcs ship -m msg"): Ask(),
        Input(command="ccx exec 'print(1)'"): Ask(),
        Input(command="ccx format -- ls"): Ask(),
        Input(command="ccx vcs show -- --output=/tmp/pwned"): Ask(),
        Input(command="ccx code grep -- -foo"): Ask(),
        Input(command="ccx code replace 'a' 'b' --apply"): Ask(),
        Input(command="ccx code replace 'a' 'b' --apply=false"): Ask(),
        Input(command="ccx code read f.go --reveal-secrets"): Ask(),
        Input(command="ccx code read f.go --reveal-secrets=false"): Ask(),
        Input(command="ccx code grep KEY --reveal-secrets"): Ask(),
        Input(command="ccx vcs show --reveal-secrets"): Ask(),
        Input(command="ccx vcs history f.go --reveal-secrets"): Ask(),
        Input(command="ccx --budget 5 code read f.go"): Ask(),
        Input(command="ccx code"): Ask(),
        Input(tool="mcp__srv__Bash", tool_input={"command": "ccx code grep foo"}): Ask(),
    },
)
