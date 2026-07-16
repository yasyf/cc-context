"""Auto-approve the read-only ccx surface the pack steers Claude onto.

The guards rewrite token-bomb calls toward ccx, but the steered-to calls still pop
permission dialogs. Three approvers answer them with *allow*: the strictly read-only
cc-context MCP tools, ``ccx_code_replace`` previews, and plain read-only ``ccx`` CLI
invocations via Bash.

Everything fails closed. The MCP approvers pin the server name — bare ``Tool()``
suffix-matching would approve a mutating tool a foreign server hides behind a
read-only name. The CLI approver scans the *raw* command text for shell expansion
before trusting parsed structure: the parser drops unquoted ``$(…)`` from argv and
dequoting erases the quoting bash still honors, so structural checks are blind
there — captain-hook's general ``ReadOnlyCommand`` was reverted over exactly this.
This one survives only because the vocabulary is a closed literal set for one
binary; an unknown shape falls through to the normal dialog.
"""

from __future__ import annotations

import re

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

from .common import ccx_bin, is_plain_argv, is_single_command

# Server names the cc-context MCP registers under: direct MCP config vs the
# plugin-installed prefix. Membership is exact — any other server prompts.
CCX_SERVERS = frozenset({"cc-context", "plugin_cc-context_cc-context"})

# The strictly read-only MCP tools. Excluded: ccx_code_edit and ccx_exec (mutating),
# BashFormat (executes arbitrary argv), ccx_code_replace (its preview form has its
# own approver below).
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

# Read-only (family, op) pairs of the ccx CLI. `code replace` is handled separately
# (preview only). Excluded: `code edit`, `vcs ship`, `exec`, `mcp`, and `format`
# entirely — `format -- <cmd>` execs arbitrary argv, and the safe stdin-filter form
# only occurs in pipelines, which the single-command check already rejects.
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

# Shell expansion anywhere in the raw text — `$`, backtick, brace, process
# substitution. Runs on cl.raw before any parsed structure is trusted; a hit means
# the dialog shows even when the parse looks clean. Side effect: ast-grep
# metavariables (`ccx code replace '$A' …`) trip it, so those previews still
# prompt — fail-closed by design, not a bug. Tilde and glob (`~`, `*`) are
# intentionally absent: argument expansion, never execution.
UNSAFE_EXPANSION = re.compile(r"[`${]|<\(|>\(")


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

    ``ccx_code_read`` carrying a truthy ``reveal_secrets`` is excluded — the
    secret-masking escape hatch falls through to the dialog, mirroring the CLI
    ``--reveal-secrets`` carve-out below.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        tool = ccx_mcp_tool(evt.tool_name)
        if tool == "ccx_code_read" and evt.input.raw.get("reveal_secrets"):
            return False
        return tool in READ_ONLY_MCP_TOOLS


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
        if UNSAFE_EXPANSION.search(cl.raw):
            return False
        if not (is_single_command(cl) and is_plain_argv(cl)):
            return False
        if cl.primary.executable != "ccx" and cl.primary.executable != ccx_bin():
            return False
        args = cl.primary.args
        # A bare `--` is cobra's rest-are-positionals marker: it smuggles a flag-shaped
        # string into a shell-out op (git, rg) as an injected flag. Exact token only.
        if "--" in args:
            return False
        if len(args) < 2:
            return False
        if (args[0], args[1]) == ("code", "replace"):
            return not any(a.startswith("--apply") for a in args)
        if (args[0], args[1]) == ("code", "read"):
            # --reveal-secrets prints raw secret material; the masking escape hatch
            # is never self-serve, so any spelling falls through to the dialog.
            return not any(a.startswith("--reveal-secrets") for a in args)
        return (args[0], args[1]) in READ_ONLY_CLI_OPS


class McpTool(CustomCondition):
    """Matches MCP-server tools (``mcp__<server>__<tool>``), which Tool() suffix-matching also accepts."""

    def check(self, evt: BaseHookEvent) -> bool:
        return bool(evt.tool_name) and evt.tool_name.startswith("mcp__")


# All three approvers pin dialog-only so they never compose with repo_find_nudge at
# PreToolUse (capt-hook's default is now PreToolUse | PermissionRequest).
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
        # reveal_secrets prints raw material — a truthy value falls through to the dialog, a falsy one stays approved
        Input(tool="mcp__cc-context__ccx_code_read", tool_input={"path": "main.go", "reveal_secrets": True}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_read", tool_input={"path": "main.go", "reveal_secrets": False}): Allow(
            explicit=True
        ),
        Input(
            tool="mcp__plugin_cc-context_cc-context__ccx_web_search",
            tool_input={"url": "https://go.dev", "query": "generics"},
        ): Allow(explicit=True),
        Input(tool="mcp__cc-context__ccx_code_edit", tool_input={"path": "main.go", "at": "1-2#h"}): Ask(),
        Input(tool="mcp__cc-context__ccx_exec", tool_input={"script": "1"}): Ask(),
        Input(tool="mcp__plugin_cc-context_cc-context__ccx_exec", tool_input={"script": "1"}): Ask(),  # mutating behind the plugin prefix
        Input(tool="mcp__cc-context__BashFormat", tool_input={"command": "ls"}): Ask(),
        # replace is never in the read-only set — its preview form has its own approver
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b"}): Ask(),
        Input(tool="mcp__evil__ccx_code_grep", tool_input={"pattern": "TODO"}): Ask(),  # foreign server
        Input(tool="ccx_code_grep", tool_input={"pattern": "TODO"}): Ask(),  # bare name, no server pin
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
        # a *string* "false" is truthy — a client sending it gets the dialog, fail-closed
        Input(tool="mcp__cc-context__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b", "apply": "false"}): Ask(),
        Input(tool="mcp__evil__ccx_code_replace", tool_input={"pattern": "a", "rewrite": "b"}): Ask(),
        Input(tool="mcp__cc-context__ccx_code_grep", tool_input={"pattern": "a"}): Ask(),  # wrong tool
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
        Input(command="ccx code grep $(whoami)"): Ask(),  # command substitution
        Input(command="ccx code grep `whoami`"): Ask(),  # backtick substitution
        Input(command='ccx code grep "$(whoami)"'): Ask(),  # quoted substitution still executes
        Input(command="ccx code read ${FILE}"): Ask(),  # variable expansion
        Input(command="ccx code grep $FILE"): Ask(),  # bare unbraced variable
        Input(command="ccx code read f.go --section $((1+1))"): Ask(),  # arithmetic expansion
        Input(command="ccx code read f.{go,py}"): Ask(),  # brace expansion, no `$` anywhere
        Input(command=""): Ask(),  # degenerate: nothing parses out
        Input(command="   "): Ask(),
        Input(command="ccx code grep foo | tee /tmp/out"): Ask(),  # pipeline
        Input(command="ccx vcs diff && rm -rf x"): Ask(),  # chain
        Input(command="ccx code grep foo & rm -rf /"): Ask(),  # background chain
        Input(command="ccx vcs diff ; rm -rf x"): Ask(),  # semicolon chain
        Input(command="ccx vcs diff || rm -rf x"): Ask(),  # or-chain
        Input(command="ccx code grep foo\nrm -rf /"): Ask(),  # newline chain
        Input(command="ccx code grep foo > /tmp/out"): Ask(),  # redirect
        Input(command="ccx code grep foo >> /tmp/out"): Ask(),  # append redirect
        Input(command="ccx code grep foo 2> /tmp/err"): Ask(),  # stderr redirect
        Input(command="ccx code grep foo < in"): Ask(),  # stdin redirect
        Input(command="ccx code grep <(cat /etc/passwd)"): Ask(),  # process substitution
        Input(command="ccx vcs diff > >(cat)"): Ask(),  # output process substitution
        Input(command="CCX_DEBUG=1 ccx code grep foo"): Ask(),  # env-assignment prefix
        Input(command="/tmp/evil/ccx code grep foo"): Ask(),  # untrusted binary path
        Input(command="./ccx code grep foo"): Ask(),  # relative path, not the resolved binary
        Input(command="sudo ccx code grep foo"): Ask(),  # no wrapper transparency
        Input(command="env ccx code grep foo"): Ask(),  # env wrapper
        Input(command="exec ccx code grep foo"): Ask(),  # shell-builtin wrapper
        Input(command="ccx code edit f.go --at 1-2#h --content x"): Ask(),
        Input(command="ccx vcs ship -m msg"): Ask(),
        Input(command="ccx exec 'print(1)'"): Ask(),
        Input(command="ccx format -- ls"): Ask(),
        Input(command="ccx vcs show -- --output=/tmp/pwned"): Ask(),  # `--` smuggles a flag to the git shell-out
        Input(command="ccx code grep -- -foo"): Ask(),  # `--` smuggles a flag to the rg shell-out
        Input(command="ccx code replace 'a' 'b' --apply"): Ask(),
        Input(command="ccx code replace 'a' 'b' --apply=false"): Ask(),  # any --apply spelling prompts, fail-closed
        Input(command="ccx code read f.go --reveal-secrets"): Ask(),  # masking escape hatch is not self-serve
        Input(command="ccx code read f.go --reveal-secrets=false"): Ask(),  # any --reveal-secrets spelling prompts, fail-closed
        Input(command="ccx --budget 5 code read f.go"): Ask(),  # global flag before family
        Input(command="ccx code"): Ask(),  # missing op
        Input(tool="mcp__srv__Bash", tool_input={"command": "ccx code grep foo"}): Ask(),  # MCP Bash veto
    },
)
