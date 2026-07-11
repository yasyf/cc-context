"""Reconstruct per-run context accounting from a saved stream-json transcript.

The runner saves the full provider event stream to ``results/<session>/raw/<run>.json``
as a JSON array (not JSONL). Each assistant event carries its own ``message.usage``;
within one logical turn the thinking/text/tool_use blocks arrive as separate assistant
events that repeat the same usage, so a turn is a contiguous run of assistant events
bounded by a ``user`` event (a tool result or the next prompt). Every other stream event
(``rate_limit_event``, ``system``, ``result``, unknown) sits inside whichever turn it lands
in and never opens or closes one — a mid-turn ``rate_limit_event`` used to split a turn in
two, double-counting its (repeated) prompt in ``total_prompt`` and ``turn_count``.

The headline quantity is the prompt **high-water mark**: the largest single-turn prompt
(``input + cache_creation + cache_read``), i.e. how big the context window actually got.
It is decomposed into additive buckets (see `Decomposition`) and reported alongside the
cumulative tool-output tokens that ccx directly controls.

``turn_count`` here (contiguous assistant runs) is a different definition from the
envelope's ``num_turns`` (Claude Code's own turn accounting); the two may legitimately
differ and are not cross-checked against each other.

A run with no turn whose prompt exceeds zero (a session that never reached the model —
e.g. an init-only stub) is excluded: `compute` returns ``None``.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Callable

from .types import Decomposition, ToolCall, TrajectoryMetrics

Count = Callable[[str], int]


def load_events(path: Path) -> list[dict]:
    """Load the raw transcript as the list of provider events it is."""
    data = json.loads(Path(path).read_text())
    if not isinstance(data, list):
        raise ValueError(f"{path}: expected a JSON array of events, got {type(data).__name__}")
    return data


def _prompt_tokens(usage: dict) -> int:
    return (
        int(usage.get("input_tokens") or 0)
        + int(usage.get("cache_creation_input_tokens") or 0)
        + int(usage.get("cache_read_input_tokens") or 0)
    )


def _output_tokens(usage: dict) -> int:
    return int(usage.get("output_tokens") or 0)


def _block_text(block: dict) -> str:
    kind = block.get("type")
    if kind == "text":
        return block.get("text") or ""
    if kind == "thinking":
        return block.get("thinking") or ""
    if kind == "tool_use":
        return f"{block.get('name', '')} {json.dumps(block.get('input', {}), sort_keys=True)}"
    return ""


def _tool_result_text(block: dict) -> str:
    content = block.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(part.get("text") or "" for part in content if isinstance(part, dict))
    return ""


def _arg_summary(block: dict, limit: int = 80) -> str:
    raw = json.dumps(block.get("input", {}), sort_keys=True)
    return raw[:limit]


def _turns(events: list[dict]) -> list[list[dict]]:
    """Group the event stream into turns: contiguous runs of assistant events split on ``user``.

    Only a ``user`` event (a tool result or the next prompt) closes the current turn; every
    other non-assistant event (``rate_limit_event``, ``system``, ``result``, unknown) neither
    extends nor closes it, so a mid-turn ``rate_limit_event`` no longer double-counts a prompt.
    """
    turns: list[list[dict]] = []
    current: list[dict] = []
    for ev in events:
        kind = ev.get("type")
        if kind == "assistant":
            current.append(ev)
        elif kind == "user" and current:
            turns.append(current)
            current = []
    if current:
        turns.append(current)
    return turns


def compute(events: list[dict], *, first_prompt: str, count: Count) -> TrajectoryMetrics | None:
    """Compute the trajectory metrics for one run, or ``None`` if the run is a stub.

    `first_prompt` is the task prompt delivered via ``claude -p`` (it is never echoed as
    an event); it is removed from the turn-1 prompt to isolate the static system + tool
    prefix. Every token count flows through `count` so callers can inject a fake.
    """
    turns = _turns(events)
    turn_prompts = [max((_prompt_tokens(ev["message"]["usage"]) for ev in turn), default=0) for turn in turns]
    turn_outputs = [max((_output_tokens(ev["message"]["usage"]) for ev in turn), default=0) for turn in turns]
    live = [(i, p) for i, p in enumerate(turn_prompts) if p > 0]
    if not live:
        return None

    peak_turn, high_water = max(live, key=lambda ip: ip[1])
    total_prompt = sum(turn_prompts)
    total_output = sum(turn_outputs)

    cumulative_tool_output = 0
    tool_calls: list[ToolCall] = []
    results_by_id: dict[str, str] = {}
    for ev in events:
        if ev.get("type") != "user":
            continue
        content = ev.get("message", {}).get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if block.get("type") == "tool_result":
                text = _tool_result_text(block)
                cumulative_tool_output += count(text)
                results_by_id[block.get("tool_use_id", "")] = text

    for ev in events:
        if ev.get("type") != "assistant":
            continue
        for block in ev["message"].get("content", []):
            if block.get("type") == "tool_use":
                result = results_by_id.get(block.get("id", ""), "")
                tool_calls.append(ToolCall(block.get("name", ""), _arg_summary(block), count(result)))

    decomposition = _decompose(
        events,
        turns,
        peak_turn=peak_turn,
        high_water=high_water,
        turn1_prompt=turn_prompts[live[0][0]],
        first_prompt=first_prompt,
        count=count,
    )

    return TrajectoryMetrics(
        high_water=high_water,
        decomposition=decomposition,
        cumulative_tool_output=cumulative_tool_output,
        turn_count=len(live),
        tool_call_count=len(tool_calls),
        peak_turn=peak_turn,
        tool_calls=tuple(tool_calls),
        total_prompt=total_prompt,
        total_output=total_output,
    )


def _decompose(
    events: list[dict],
    turns: list[list[dict]],
    *,
    peak_turn: int,
    high_water: int,
    turn1_prompt: int,
    first_prompt: str,
    count: Count,
) -> Decomposition:
    static_overhead = max(0, turn1_prompt - count(first_prompt))

    peak_first_event = id(turns[peak_turn][0])
    tool_result = 0
    history = 0
    hook_error = 0
    for ev in events:
        if id(ev) == peak_first_event:
            break
        kind = ev.get("type")
        content = ev.get("message", {}).get("content", [])
        if kind == "assistant":
            history += count("".join(_block_text(b) for b in content))
        elif kind == "user" and isinstance(content, list):
            for block in content:
                if block.get("type") == "tool_result":
                    text = _tool_result_text(block)
                    if block.get("is_error"):
                        hook_error += count(text)
                    else:
                        tool_result += count(text)
                elif block.get("type") == "text":
                    history += count(block.get("text") or "")

    residual = high_water - static_overhead - tool_result - history - hook_error
    return Decomposition(static_overhead, tool_result, history, hook_error, residual)


def from_file(path: Path, *, first_prompt: str, count: Count) -> TrajectoryMetrics | None:
    """Load a saved transcript and compute its metrics, or ``None`` for a stub."""
    return compute(load_events(path), first_prompt=first_prompt, count=count)
