"""Assert an arm behaved as labeled, so the comparison is not silently mislabeled.

baseline: no ccx tool, no guard, no cc-context MCP — native tools only. ccx-mcp: cc-context
loaded AND ccx actually exercised (a facade tool call or a Bash `ccx`); a guard fire alone,
with no ccx use, is mislabeled. ccx-cli: the isolation arm — cc-context MCP present or any
mcp__cc-context__* call is a breach (mislabeled), so a genuine run reaches ccx only through
the command-position Bash `ccx`. The verdict is recorded per run so the report can prove
"only ccx differs" rather than assume it.
"""

from __future__ import annotations

import re
from typing import Mapping

from cc_transcript import (
    BashCall,
    Command,
    CommandLine,
    PrintResult,
    ReadCall,
    ToolResultBlock,
    ToolUseBlock,
    parse_tool_call,
)

from .types import CONTROL_CATEGORY, Integrity

CCX_MCP = "mcp__cc-context__"

# The gold answers live in bench/tasks/*.json (and the legacy manifest.json); any tool input
# referencing that dir — absolute, or a relative traversal like ../../tasks/<id>.json — is
# reading the answer key, not navigating the repo under test. The lookbehind pins `tasks/` to a
# path-segment boundary so `subtasks/` and the like don't false-positive.
ANSWER_KEY = re.compile(r"manifest\.json|(?<![\w.-])tasks/")

# Signatures of a fired ccx guard, taken from the guard pack's deny messages.
GUARD_HINT = re.compile(
    r"ccx\s+(code|repo|vcs)\s+(outline|read|diff|find|symbol|grok|grep|search)|use\s+`?ccx", re.IGNORECASE
)

# A recursive-listing flag cluster (`-R`, `-laR`, …).
LS_RECURSIVE = re.compile(r"-[a-zA-Z]*R")

# git diff variants the guard pack considers bounded.
BOUNDED_GIT_DIFF = ("--stat", "--numstat", "--name-only")


def ccx_command(line: CommandLine) -> Command | None:
    """The line's first `ccx` invocation, or None.

    Command position is by construction: the parsed :class:`~cc_transcript.CommandLine`
    surfaces pipeline segments, `&&`/`;` continuations, and `$(…)` substitutions as
    commands, while an `echo "ccx code outline"` argument never parses as one.
    """
    return next((c for c in line.commands if c.executable == "ccx"), None)


def heavy_label(line: CommandLine) -> str | None:
    """The label of the line's first heavy native primitive the ccx guard pack intercepts, or None."""
    parts = line.parts
    for i, (c, _op) in enumerate(parts):
        match c.executable:
            case "cat" if c.args and all(op != "|" for _, op in parts[i:]):
                return "cat"
            case "sed" if c.args[:1] == ("-n",):
                return "sed-n"
            case "ls" if any(LS_RECURSIVE.match(a) for a in c.args):
                return "ls-R"
            case "git" if c.args[:1] == ("diff",) and not any(f in c.args for f in BOUNDED_GIT_DIFF):
                return "git-diff"
            case "find" if "-name" in c.args[1:]:
                return "find-enum"
    return None


def read_answer_key(call_input: Mapping[str, object]) -> bool:
    """True if a tool call references the gold-answer key (bench tasks dir or manifest)."""
    return any(ANSWER_KEY.search(str(v)) for v in call_input.values())


def denial_is_ccx_guard(denial: Mapping[str, object]) -> bool:
    """A PreToolUse denial whose blocked tool matches a ccx-navigation guard target.

    Under bypassPermissions the capt-hook pack is the only deny source, but a denial record
    carries only the blocked tool and its input — not the deny reason — so a fired guard is
    recognized structurally: the same heavy primitive or unbounded large Read the navigation
    guards intercept. (The deny reason itself surfaces as an is_error tool_result, matched
    separately via GUARD_HINT.)
    """
    tool_input = denial.get("tool_input") or {}
    if not isinstance(tool_input, Mapping):
        return False
    match parse_tool_call(str(denial.get("tool_name", "")), tool_input, on_error="other"):
        case BashCall(command_line=line):
            return heavy_label(line) is not None
        case ReadCall(offset=None, limit=None):
            return True
        case _:
            return False


def assess(pr: PrintResult, arm: str, category: str) -> Integrity:
    """Classify the run's tool activity and judge whether it matches its arm.

    Control tasks (`category == CONTROL_CATEGORY`) run in an empty workdir with no code, so a ccx
    arm is not required to exercise ccx — only its isolation invariants still apply.
    """
    ccx_calls: list[str] = []
    heavy: list[str] = []
    cheated = False
    guard_fired = False
    for message in pr.messages:
        for block in message.blocks:
            if isinstance(block, ToolUseBlock):
                if read_answer_key(block.input):
                    cheated = True
                if block.name.startswith(CCX_MCP):
                    ccx_calls.append(block.name)
                    continue
                match parse_tool_call(block.name, block.input, on_error="other"):
                    case BashCall(command_line=line) if (ccx := ccx_command(line)) is not None:
                        depth = 2 if ccx.args and ccx.args[0] in ("code", "repo", "vcs") else 1
                        ccx_calls.append(" ".join(["bash:ccx", *ccx.args[:depth]]))
                    case BashCall(command_line=line) if (label := heavy_label(line)) is not None:
                        heavy.append(label)
                    case ReadCall(offset=None, limit=None):
                        heavy.append("read-unbounded")
            elif isinstance(block, ToolResultBlock):
                # Rewrite-style fires (allow + updatedInput) are never serialized into -p output,
                # so only deny-style fires are countable here; liveness is proven per session by
                # the arms.guards_available probe and recorded per run as guards_active.
                if block.is_error and GUARD_HINT.search(block.content):
                    guard_fired = True

    guard_fired = guard_fired or any(denial_is_ccx_guard(d) for d in pr.permission_denials)

    ccx_used = bool(ccx_calls)
    mcp_ccx_used = any(c.startswith(CCX_MCP) for c in ccx_calls)
    bash_ccx_used = any(c.startswith("bash:ccx") for c in ccx_calls)
    cc_present = bool(pr.init) and any(s.name == "cc-context" for s in pr.init.mcp_servers)
    is_control = category == CONTROL_CATEGORY

    if arm == "baseline":
        leaks: list[str] = []
        if ccx_used:
            leaks.append("ccx used in baseline")
        if guard_fired:
            leaks.append("guard fired in baseline")
        if cc_present:
            leaks.append("cc-context MCP present in baseline")
        ok = not leaks
        note = "ok" if ok else "; ".join(leaks)
    elif arm == "ccx-mcp":
        if not cc_present:
            ok, note = False, "ccx-mcp arm but cc-context MCP not loaded"
        elif ccx_used:
            ok, note = True, "ok"
        elif is_control:
            ok, note = True, "ok (control: ccx not required)"
        elif guard_fired:
            ok, note = False, "ccx-mcp arm: guards fired but ccx never used — mislabeled"
        else:
            ok, note = False, "ccx-mcp arm but ccx never used and no guard fired — mislabeled"
    elif arm == "ccx-cli":
        if cc_present:
            ok, note = False, "ccx-cli arm but cc-context MCP loaded — isolation breach, mislabeled"
        elif mcp_ccx_used:
            ok, note = False, "ccx-cli arm but mcp__cc-context__ tool called — mislabeled"
        elif bash_ccx_used:
            ok, note = True, "ok"
        elif is_control:
            ok, note = True, "ok (control: ccx not required)"
        elif guard_fired:
            ok, note = False, "ccx-cli arm: guards fired but ccx never used — mislabeled"
        else:
            ok, note = False, "ccx-cli arm but Bash ccx never used and no guard fired — mislabeled"
    else:
        raise ValueError(f"unknown arm: {arm}")

    if cheated:
        ok, note = False, f"READ ANSWER KEY — run is invalid; {note}"

    return Integrity(
        ccx_used=ccx_used,
        guard_fired=guard_fired,
        ccx_calls=ccx_calls,
        native_heavy_calls=heavy,
        ok=ok,
        note=note,
    )
