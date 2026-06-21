"""Assert an arm behaved as labeled, so the comparison is not silently mislabeled.

ccx arm: ccx must actually be exercised (a facade tool call or a Bash `ccx`), or a
guard must have fired. baseline arm: no ccx tool, no guard — native tools only. The
verdict is recorded per run so the report can prove "only ccx differs" rather than
assume it.
"""

from __future__ import annotations

import re

from .envelope import Envelope
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


def read_answer_key(call_input: dict) -> bool:
    """True if a tool call references the gold-answer manifest (a cheat, not navigation)."""
    return any("manifest.json" in str(v) for v in call_input.values())


def assess(env: Envelope, arm: str) -> Integrity:
    """Classify the run's tool activity and judge whether it matches its arm."""
    ccx_calls: list[str] = []
    heavy: list[str] = []
    cheated = False
    for call in env.tool_calls:
        cmd = call.input.get("command", "") if call.name == "Bash" else ""
        if read_answer_key(call.input):
            cheated = True
        if is_ccx_call(call.name, cmd):
            if call.name.startswith(CCX_MCP):
                ccx_calls.append(call.name)
            else:
                parts = cmd.split()
                ccx_calls.append(f"bash:ccx {parts[1]}" if len(parts) > 1 else "bash:ccx")
            continue
        label = heavy_label(cmd)
        if label:
            heavy.append(label)
        if call.name == "Read" and "offset" not in call.input and "limit" not in call.input:
            heavy.append("read-unbounded")

    guard_fired = any(r.is_error and GUARD_HINT.search(r.text) for r in env.tool_results)
    guard_fired = guard_fired or any(GUARD_HINT.search(str(d)) for d in env.permission_denials)

    ccx_used = bool(ccx_calls)
    cc_present = "cc-context" in env.init.mcp_servers

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
