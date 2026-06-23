"""Assert an arm behaved as labeled, so the comparison is not silently mislabeled.

ccx arm: ccx must actually be exercised (a facade tool call or a Bash `ccx`), or a
guard must have fired. baseline arm: no ccx tool, no guard — native tools only. The
verdict is recorded per run so the report can prove "only ccx differs" rather than
assume it.
"""

from __future__ import annotations

import re
from typing import Mapping

from cc_transcript import PrintResult, ToolResultBlock, ToolUseBlock

from .types import Integrity

CCX_MCP = "mcp__cc-context__"
CCX_BASH = re.compile(r"(^|[\s;|&(])ccx\s")

# Heavy native primitives the ccx guard pack is designed to intercept.
HEAVY_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("cat", re.compile(r"(^|[\s;|&(])cat\s+[^|]*$")),
    ("sed-n", re.compile(r"(^|[\s;|&(])sed\s+-n\s")),
    ("ls-R", re.compile(r"(^|[\s;|&(])ls\s+-[a-zA-Z]*R")),
    ("git-diff", re.compile(r"(^|[\s;|&(])git\s+diff(?!\s+--stat|\s+--numstat|\s+--name-only)")),
    ("find-enum", re.compile(r"(^|[\s;|&(])find\s+\S+\s+-name\s")),
)

# Signatures of a fired ccx guard, taken from the guard pack's deny messages.
GUARD_HINT = re.compile(r"ccx\s+(outline|read|diff|find|symbol|grok|grep|search)|use\s+`?ccx", re.IGNORECASE)


def is_ccx_call(name: str, cmd: str) -> bool:
    if name.startswith(CCX_MCP):
        return True
    return name == "Bash" and bool(CCX_BASH.search(cmd))


def heavy_label(cmd: str) -> str | None:
    for label, pat in HEAVY_PATTERNS:
        if pat.search(cmd):
            return label
    return None


def read_answer_key(call_input: Mapping[str, object]) -> bool:
    """True if a tool call references the gold-answer manifest (a cheat, not navigation)."""
    return any("manifest.json" in str(v) for v in call_input.values())


def denial_is_ccx_guard(denial: Mapping[str, object]) -> bool:
    """A PreToolUse denial whose blocked tool matches a ccx-navigation guard target.

    Under bypassPermissions the capt-hook pack is the only deny source, but a denial record
    carries only the blocked tool and its input — not the deny reason — so a fired guard is
    recognized structurally: the same heavy primitive or unbounded large Read the navigation
    guards intercept. (The deny reason itself surfaces as an is_error tool_result, matched
    separately via GUARD_HINT.)
    """
    tool = str(denial.get("tool_name", ""))
    tool_input = denial.get("tool_input") or {}
    if not isinstance(tool_input, Mapping):
        return False
    if tool == "Bash":
        return heavy_label(str(tool_input.get("command", ""))) is not None
    if tool == "Read":
        return "offset" not in tool_input and "limit" not in tool_input
    return False


def assess(pr: PrintResult, arm: str) -> Integrity:
    """Classify the run's tool activity and judge whether it matches its arm."""
    ccx_calls: list[str] = []
    heavy: list[str] = []
    cheated = False
    guard_fired = False
    for message in pr.messages:
        for block in message.blocks:
            if isinstance(block, ToolUseBlock):
                cmd = str(block.input.get("command", "")) if block.name == "Bash" else ""
                if read_answer_key(block.input):
                    cheated = True
                if is_ccx_call(block.name, cmd):
                    if block.name.startswith(CCX_MCP):
                        ccx_calls.append(block.name)
                    else:
                        parts = cmd.split()
                        ccx_calls.append(f"bash:ccx {parts[1]}" if len(parts) > 1 else "bash:ccx")
                    continue
                label = heavy_label(cmd)
                if label:
                    heavy.append(label)
                if block.name == "Read" and "offset" not in block.input and "limit" not in block.input:
                    heavy.append("read-unbounded")
            elif isinstance(block, ToolResultBlock):
                if block.is_error and GUARD_HINT.search(block.content):
                    guard_fired = True

    guard_fired = guard_fired or any(denial_is_ccx_guard(d) for d in pr.permission_denials)

    ccx_used = bool(ccx_calls)
    cc_present = bool(pr.init) and any(s.name == "cc-context" for s in pr.init.mcp_servers)

    if arm == "ccx":
        if not cc_present:
            ok, note = False, "ccx arm but cc-context MCP not loaded"
        elif ccx_used or guard_fired:
            ok, note = True, "ok"
        else:
            ok, note = False, "ccx arm but ccx never used and no guard fired — mislabeled"
    else:
        leaks: list[str] = []
        if ccx_used:
            leaks.append("ccx used in baseline")
        if guard_fired:
            leaks.append("guard fired in baseline")
        if cc_present:
            leaks.append("cc-context MCP present in baseline")
        ok = not leaks
        note = "ok" if ok else "; ".join(leaks)

    if cheated:
        ok, note = False, f"READ ANSWER KEY (manifest.json) — run is invalid; {note}"

    return Integrity(
        ccx_used=ccx_used,
        guard_fired=guard_fired,
        ccx_calls=ccx_calls,
        native_heavy_calls=heavy,
        ok=ok,
        note=note,
    )
