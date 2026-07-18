"""Reconstruct per-run context accounting from a saved stream-json transcript.

The runner saves the full provider event stream to ``results/<session>/raw/<run>.json``
as a JSON array (not JSONL). `cc_transcript.parse_print_result` lifts it into a typed
:class:`~cc_transcript.PrintResult`; the conversational elements become
:class:`~cc_transcript.PrintMessage` views carrying the per-message API usage and the
billed API-call id (``message.usage``/``message.id``, cc-transcript >= 14.1). The stream
fragments a single API response into several assistant messages (thinking, then each
tool_use) that all repeat that call's usage and share one ``id``.

Two accountings run over the messages, at different granularities:

* **Turns** are the conversational display unit: a contiguous run of assistant messages
  bounded by a user message (a tool result or the next prompt). ``turn_count`` counts
  them. Non-conversational stream events (``rate_limit_event``, ``system``, ``result``,
  unknown) never enter ``PrintResult.messages``, so they never open or close one.
* **Token totals** (``total_prompt``/``total_output``) bill by message ``id`` — the billed
  API-call identifier — counting each call exactly once. This is robust to the two ways one
  call's messages get split apart in the stream: a ``rate_limit_event`` landing between them
  (same turn) and, for parallel tool calls, a ``tool_result`` interleaved between the
  tool_use blocks (which lands them in *different* turns). A per-turn sum double-counts the
  latter; billing by id does not. (Assistant messages lacking an id fall back to per-turn
  max — real transcripts always carry one.)

The headline quantity is the prompt **high-water mark**: the largest single billed prompt
(``input + cache_creation + cache_read``), i.e. how big the context window actually got —
equal to ``max(turn_prompts)`` since every turn's max is some call's prompt. It is
decomposed into additive buckets (see `Decomposition`) and reported alongside the cumulative
tool-output tokens that ccx directly controls.

``turn_count`` here (contiguous assistant runs) is a different definition from the envelope's
``num_turns`` (Claude Code's own turn accounting); the two may legitimately differ and are
not cross-checked against each other.

A run with no turn whose prompt exceeds zero (a session that never reached the model —
e.g. an init-only stub) is excluded: `compute` returns ``None``.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Callable, Sequence

from cc_transcript import (
    PrintMessage,
    PrintResult,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    Usage,
    parse_print_result,
)

from .types import Decomposition, ToolCall, TrajectoryMetrics

Count = Callable[[str], int]


def _usage(msg: PrintMessage) -> Usage:
    if msg.usage is None:
        raise ValueError(f"assistant message carries no usage: {msg.id or msg.uuid}")
    return msg.usage


def _prompt_tokens(usage: Usage) -> int:
    return usage.input_tokens + usage.cache_creation_input_tokens + usage.cache_read_input_tokens


def _block_text(block: object) -> str:
    match block:
        case TextBlock(text=text):
            return text
        case ThinkingBlock(thinking=thinking):
            return thinking
        case ToolUseBlock(name=name) as tool_use:
            return f"{name} {_args_json(tool_use)}"
        case _:
            return ""


def _args_json(block: ToolUseBlock) -> str:
    return json.dumps(dict(block.input), sort_keys=True)


def _arg_summary(block: ToolUseBlock, limit: int = 80) -> str:
    return _args_json(block)[:limit]


def _turns(messages: Sequence[PrintMessage]) -> list[list[PrintMessage]]:
    """Group the messages into turns: contiguous runs of assistant messages split on user ones.

    Only a user message (a tool result or the next prompt) closes the current turn; the
    non-conversational stream events (``rate_limit_event``, ``system``, ``result``, unknown)
    never parse into messages, so a mid-turn ``rate_limit_event`` cannot double-count a prompt.
    """
    turns: list[list[PrintMessage]] = []
    current: list[PrintMessage] = []
    for msg in messages:
        if msg.role == "assistant":
            current.append(msg)
        elif current:
            turns.append(current)
            current = []
    if current:
        turns.append(current)
    return turns


def _billed(turns: list[list[PrintMessage]]) -> tuple[list[int], list[int]]:
    """Per-billed-call (prompt, output) tokens: one entry per API call, not per stream fragment.

    A call is a distinct message ``id`` (all its messages repeat the same usage), billed once even
    when an interleaved ``tool_result`` splits it across turns — or a ``rate_limit_event`` splits it
    within one. Assistant messages lacking an ``id`` fall back to their turn's max, preserving the
    pre-message-id collapse of same-usage fragments in a single turn.
    """
    seen: set[str] = set()
    prompts: list[int] = []
    outputs: list[int] = []
    for turn in turns:
        idless_prompt = 0
        idless_output = 0
        for msg in turn:
            usage = _usage(msg)
            if msg.id is None:
                idless_prompt = max(idless_prompt, _prompt_tokens(usage))
                idless_output = max(idless_output, usage.output_tokens)
            elif msg.id not in seen:
                seen.add(msg.id)
                prompts.append(_prompt_tokens(usage))
                outputs.append(usage.output_tokens)
        if idless_prompt or idless_output:
            prompts.append(idless_prompt)
            outputs.append(idless_output)
    return prompts, outputs


def compute(pr: PrintResult, *, first_prompt: str, count: Count) -> TrajectoryMetrics | None:
    """Compute the trajectory metrics for one run, or ``None`` if the run is a stub.

    `first_prompt` is the task prompt delivered via ``claude -p`` (it is never echoed as
    an event); it is removed from the turn-1 prompt to isolate the static system + tool
    prefix. Every token count flows through `count` so callers can inject a fake.
    """
    messages = pr.messages
    turns = _turns(messages)
    turn_prompts = [max((_prompt_tokens(_usage(msg)) for msg in turn), default=0) for turn in turns]
    live = [(i, p) for i, p in enumerate(turn_prompts) if p > 0]
    if not live:
        return None

    peak_turn, high_water = max(live, key=lambda ip: ip[1])
    # Token totals bill once per API call (message id), so a call split across turns by an interleaved
    # tool_result — or within a turn by a rate_limit_event — is counted once, not per fragment.
    # high_water == max(turn_prompts) == the max billed prompt, so peak_turn stays coupled to it.
    billed_prompts, billed_outputs = _billed(turns)
    total_prompt = sum(billed_prompts)
    total_output = sum(billed_outputs)

    cumulative_tool_output = 0
    tool_calls: list[ToolCall] = []
    results_by_id: dict[str, str] = {}
    for msg in messages:
        if msg.role != "user":
            continue
        for block in msg.blocks:
            if isinstance(block, ToolResultBlock):
                cumulative_tool_output += count(block.content)
                results_by_id[block.tool_use_id] = block.content

    for msg in messages:
        if msg.role != "assistant":
            continue
        for block in msg.blocks:
            if isinstance(block, ToolUseBlock):
                tool_calls.append(ToolCall(block.name, _arg_summary(block), count(results_by_id.get(block.id, ""))))

    decomposition = _decompose(
        messages,
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
    messages: Sequence[PrintMessage],
    turns: list[list[PrintMessage]],
    *,
    peak_turn: int,
    high_water: int,
    turn1_prompt: int,
    first_prompt: str,
    count: Count,
) -> Decomposition:
    static_overhead = max(0, turn1_prompt - count(first_prompt))

    peak_first = turns[peak_turn][0]
    tool_result = 0
    history = 0
    hook_error = 0
    for msg in messages:
        if msg is peak_first:
            break
        if msg.role == "assistant":
            history += count("".join(_block_text(b) for b in msg.blocks))
            continue
        for block in msg.blocks:
            match block:
                case ToolResultBlock(is_error=True, content=text):
                    hook_error += count(text)
                case ToolResultBlock(content=text):
                    tool_result += count(text)
                case TextBlock(text=text):
                    history += count(text)

    residual = high_water - static_overhead - tool_result - history - hook_error
    return Decomposition(static_overhead, tool_result, history, hook_error, residual)


def from_file(path: Path, *, first_prompt: str, count: Count) -> TrajectoryMetrics | None:
    """Parse a saved transcript and compute its metrics, or ``None`` for a stub."""
    return compute(parse_print_result(Path(path).read_bytes()), first_prompt=first_prompt, count=count)
