"""Typed records shared across the harness."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

ARMS: tuple[str, ...] = ("baseline", "ccx")

CATEGORIES: tuple[str, ...] = (
    "navigation",
    "callees",
    "callers",
    "diff_review",
    "intent_search",
    "targeted_edit",
    "structural_replace",
    "structural_search",
    "non_regression",
    "large_context",
)


@dataclass(frozen=True)
class Symbol:
    """A ground-truth symbol in the fixture repo: where it is declared and its edges."""

    name: str
    file: str
    decl: str
    kind: str
    callees: tuple[str, ...]
    callers: tuple[str, ...]


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
        )


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
