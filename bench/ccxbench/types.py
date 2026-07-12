"""Typed records shared across the harness."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

ARMS: tuple[str, ...] = ("baseline", "ccx-mcp", "ccx-cli")

CATEGORIES: tuple[str, ...] = (
    "navigation",
    "trace",
    "large_context",
    "large_context_diag",
    "diff_review",
    "targeted_edit",
    "intent_search",
    "non_regression",
)

# The control family: an empty workdir, a conceptual question, no code for ccx to navigate.
CONTROL_CATEGORY = "non_regression"

# The diagnostic family: a confounded probe (Stage-1 T7, exec-vs-pipeline) with a real gold but
# excluded from every headline aggregate, like the control family. Reported separately, never paired.
DIAGNOSTIC_CATEGORY = "large_context_diag"

DECOMP_TERMS: tuple[str, ...] = ("static_overhead", "tool_result", "history", "hook_error", "residual")


@dataclass(frozen=True)
class Grader:
    """A grading rule: a `kind` plus its `spec` (parameters, gold-independent)."""

    kind: str
    spec: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"kind": self.kind, "spec": self.spec}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Grader:
        return cls(kind=d["kind"], spec=d.get("spec", {}))


@dataclass(frozen=True)
class Task:
    """One benchmark task: a prompt over a repo with a structured answer and a grader.

    `gold` holds the reference answer the grader checks against. `setup` describes any
    workdir preparation (e.g. an uncommitted patch for diff tasks). `ccx_helps` is False
    for non-regression tasks where ccx is not expected to help — used to report harm.
    `floor_exempt` waives the traversal-bytes size floor for a task whose repo is genuinely
    smaller than the floor (a single-file control, or a whole small repo). `arm_addenda`
    carries a per-arm extra system-prompt line (keyed by arm) appended to that arm's ladder —
    the only per-task steering, used by the exec-vs-pipeline diagnostic.
    """

    id: str
    category: str
    repo: str
    prompt: str
    schema: dict[str, Any]
    grader: Grader
    gold: dict[str, Any]
    ccx_helps: bool = True
    setup: dict[str, Any] = field(default_factory=dict)
    floor_exempt: bool = False
    arm_addenda: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "category": self.category,
            "repo": self.repo,
            "prompt": self.prompt,
            "schema": self.schema,
            "grader": self.grader.to_dict(),
            "gold": self.gold,
            "ccx_helps": self.ccx_helps,
            "setup": self.setup,
            "floor_exempt": self.floor_exempt,
            "arm_addenda": self.arm_addenda,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Task:
        return cls(
            id=d["id"],
            category=d["category"],
            repo=d["repo"],
            prompt=d["prompt"],
            schema=d["schema"],
            grader=Grader.from_dict(d["grader"]),
            gold=d["gold"],
            ccx_helps=d.get("ccx_helps", True),
            setup=d.get("setup", {}),
            floor_exempt=d.get("floor_exempt", False),
            arm_addenda=d.get("arm_addenda", {}),
        )

    @property
    def traversal_files(self) -> tuple[str, ...]:
        """Repo-relative paths a naive baseline must traverse (size-floor metadata).

        Lives inside `gold`, so the model under test never sees it. Empty for task
        families with no traversal floor (e.g. non_regression).
        """
        return tuple(self.gold.get("traversal_files", ()))


@dataclass(frozen=True)
class Usage:
    """Normalized token usage from a result envelope, with the cache-creation split."""

    input: int
    output: int
    cache_read: int
    cache_create_5m: int
    cache_create_1h: int
    service_tier: str
    inference_geo: str

    @property
    def cache_create_total(self) -> int:
        return self.cache_create_5m + self.cache_create_1h


@dataclass(frozen=True)
class GradeResult:
    correct: bool
    detail: str


@dataclass(frozen=True)
class Integrity:
    """Whether an arm behaved as labeled: ccx used iff it is the ccx arm; guards present."""

    ccx_used: bool
    guard_fired: bool
    ccx_calls: list[str]
    native_heavy_calls: list[str]
    ok: bool
    note: str


@dataclass(frozen=True)
class Decomposition:
    """The prompt high-water mark split into additive, disjoint token buckets.

    The five terms sum to the high-water mark exactly: `residual` closes the gap
    between the API-reported peak prompt and our own tokenization of its parts.
    `static_overhead` is the fixed system + tool-schema prefix, counted once (the
    high-water mark is a single-turn snapshot), never summed across turns.
    """

    static_overhead: int
    tool_result: int
    history: int
    hook_error: int
    residual: int

    @property
    def total(self) -> int:
        return self.static_overhead + self.tool_result + self.history + self.hook_error + self.residual

    def dominant(self) -> str:
        terms = {
            "static_overhead": self.static_overhead,
            "tool_result": self.tool_result,
            "history": self.history,
            "hook_error": self.hook_error,
            "residual": self.residual,
        }
        return max(terms, key=lambda k: terms[k])


@dataclass(frozen=True)
class ToolCall:
    """One tool invocation in a trajectory: its name, an argument digest, and the
    token size of the result it injected into context."""

    name: str
    arg_summary: str
    output_tokens: int


@dataclass(frozen=True)
class TrajectoryMetrics:
    """Per-run context accounting reconstructed from a saved stream-json transcript.

    `high_water` is the largest single-turn prompt (input + cache_create + cache_read).
    `cumulative_tool_output` is the total of every tool result injected into context —
    the quantity ccx directly controls. `decomposition` attributes the high-water mark.
    `total_prompt`/`total_output` sum the per-API-call prompt and output tokens across the
    whole trajectory (the transcript-side recompute report.py cross-checks vs the envelope).
    """

    high_water: int
    decomposition: Decomposition
    cumulative_tool_output: int
    turn_count: int
    tool_call_count: int
    peak_turn: int
    tool_calls: tuple[ToolCall, ...]
    total_prompt: int
    total_output: int

    @property
    def total_tokens(self) -> int:
        return self.total_prompt + self.total_output
