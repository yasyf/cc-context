"""Parse the `claude -p --output-format json` message array into a result envelope.

The output is a JSON array of messages. The element with type=="result" carries the
cache-aware cost, token usage (with the 5m/1h cache-creation split), per-model usage,
and the `--json-schema` structured output. The system/init element records the actual
tool, MCP, and plugin surface; assistant/user elements expose tool_use / tool_result
blocks used by the integrity check.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

from .types import Usage


@dataclass(frozen=True)
class ToolCall:
    name: str
    input: dict[str, Any]


@dataclass(frozen=True)
class ToolResult:
    text: str
    is_error: bool


@dataclass(frozen=True)
class InitInfo:
    mcp_servers: list[str]
    plugins: list[str]
    tools: list[str]
    n_skills: int


@dataclass(frozen=True)
class Envelope:
    is_error: bool
    result_text: str
    structured_output: object
    total_cost_usd: float
    num_turns: int
    usage: Usage
    model_usage: dict[str, Any]
    permission_denials: list[dict[str, Any]]
    session_id: str
    fast_mode_state: str
    stop_reason: str
    init: InitInfo
    tool_calls: list[ToolCall]
    tool_results: list[ToolResult]
    messages: list[dict[str, Any]] = field(default_factory=list)

    @classmethod
    def synthetic(cls, structured_output: object, result_text: str = "", is_error: bool = False) -> Envelope:
        """A zero-cost envelope for grading a known answer (grader self-tests, unit tests)."""
        return cls(
            is_error=is_error,
            result_text=result_text,
            structured_output=structured_output,
            total_cost_usd=0.0,
            num_turns=0,
            usage=Usage(0, 0, 0, 0, 0, "standard", ""),
            model_usage={},
            permission_denials=[],
            session_id="synthetic",
            fast_mode_state="off",
            stop_reason="end_turn",
            init=InitInfo([], [], [], 0),
            tool_calls=[],
            tool_results=[],
            messages=[],
        )


def read_usage(result: dict[str, Any]) -> Usage:
    u = result.get("usage", {})
    cc = u.get("cache_creation") or {}
    flat = int(u.get("cache_creation_input_tokens", 0))
    cc_5m = int(cc.get("ephemeral_5m_input_tokens", flat if not cc else 0))
    cc_1h = int(cc.get("ephemeral_1h_input_tokens", 0))
    return Usage(
        input=int(u.get("input_tokens", 0)),
        output=int(u.get("output_tokens", 0)),
        cache_read=int(u.get("cache_read_input_tokens", 0)),
        cache_create_5m=cc_5m,
        cache_create_1h=cc_1h,
        service_tier=str(u.get("service_tier", "")),
        inference_geo=str(u.get("inference_geo", "")),
    )


def read_init(messages: list[dict[str, Any]]) -> InitInfo:
    for m in messages:
        if m.get("type") == "system" and m.get("subtype") == "init":
            return InitInfo(
                mcp_servers=[s if isinstance(s, str) else s.get("name", "") for s in m.get("mcp_servers", [])],
                plugins=[p.get("name", "") if isinstance(p, dict) else str(p) for p in m.get("plugins", [])],
                tools=list(m.get("tools", [])),
                n_skills=len(m.get("skills", [])),
            )
    return InitInfo(mcp_servers=[], plugins=[], tools=[], n_skills=0)


def scan_tool_calls(messages: list[dict[str, Any]]) -> list[ToolCall]:
    calls: list[ToolCall] = []
    for m in messages:
        if m.get("type") != "assistant":
            continue
        for block in m.get("message", {}).get("content", []):
            if isinstance(block, dict) and block.get("type") == "tool_use":
                calls.append(ToolCall(name=block.get("name", ""), input=block.get("input", {}) or {}))
    return calls


def block_text(content: object) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for b in content:
            if isinstance(b, dict):
                t = b.get("text") or b.get("content")
                if isinstance(t, str):
                    parts.append(t)
            elif isinstance(b, str):
                parts.append(b)
        return " ".join(parts)
    return str(content)


def scan_tool_results(messages: list[dict[str, Any]]) -> list[ToolResult]:
    results: list[ToolResult] = []
    for m in messages:
        if m.get("type") != "user":
            continue
        content = m.get("message", {}).get("content", [])
        if not isinstance(content, list):
            continue
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_result":
                results.append(
                    ToolResult(
                        text=block_text(block.get("content", "")),
                        is_error=bool(block.get("is_error", False)),
                    )
                )
    return results


def parse(stdout: str) -> Envelope:
    """Parse headless stdout (a JSON array) into an Envelope. Raises on malformed input."""
    data = json.loads(stdout)
    if not isinstance(data, list):
        raise ValueError(f"expected a JSON array of messages, got {type(data).__name__}")

    result = next((m for m in data if m.get("type") == "result"), None)
    if result is None:
        raise ValueError("no result message in headless output")

    return Envelope(
        is_error=bool(result.get("is_error", False)),
        result_text=str(result.get("result", "")),
        structured_output=result.get("structured_output"),
        total_cost_usd=float(result.get("total_cost_usd", 0.0)),
        num_turns=int(result.get("num_turns", 0)),
        usage=read_usage(result),
        model_usage=result.get("modelUsage", {}) or {},
        permission_denials=list(result.get("permission_denials", []) or []),
        session_id=str(result.get("session_id", "")),
        fast_mode_state=str(result.get("fast_mode_state", "")),
        stop_reason=str(result.get("stop_reason", "")),
        init=read_init(data),
        tool_calls=scan_tool_calls(data),
        tool_results=scan_tool_results(data),
        messages=data,
    )
