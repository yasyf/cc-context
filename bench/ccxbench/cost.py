"""Cross-check `total_cost_usd` against an independent cache-aware recompute.

`total_cost_usd` (computed by Claude Code from its own price table) is the headline.
This module recomputes the bill from the run's token usage via cc-transcript's `cost_of`
(whose default `PRICING` table — opus/sonnet/haiku/fable with cache multipliers — is the
single source of truth) and asserts the two agree within tolerance. A divergence means a
real pricing or usage problem, not a bench bug.
"""

from __future__ import annotations

from dataclasses import dataclass

from cc_transcript import PrintResult
from cc_transcript.cost import cost_of


@dataclass(frozen=True)
class CrossCheck:
    recomputed_usd: float
    reported_usd: float
    rel_delta: float
    within_tolerance: bool
    note: str


def crosscheck(pr: PrintResult, model: str, tolerance: float) -> CrossCheck:
    """Compare `cost_of(usage, model)` to `total_cost_usd` within tolerance.

    Standard global pricing only. Premium regimes (fast mode, a non-standard service tier,
    a US inference-geo surcharge) are not modeled — they are noted so a resulting divergence
    is attributed, not mistaken for a price-table error.
    """
    notes: list[str] = []
    if pr.fast_mode_state == "on":
        notes.append("fast-mode pricing not modeled")
    if pr.usage.service_tier not in (None, "standard", ""):
        notes.append(f"non-standard service tier not modeled: {pr.usage.service_tier}")
    if pr.usage.inference_geo not in (None, "", "global", "not_available"):
        notes.append(f"inference_geo surcharge not modeled: {pr.usage.inference_geo}")

    reported = pr.total_cost_usd
    if not pr.model_usage:
        recomputed = 0.0
        notes.append("no modelUsage (likely is_error)")
    else:
        recomputed = cost_of(pr.usage, model).total

    if reported == 0.0:
        within = abs(recomputed) < 1e-6
        rel = 0.0 if within else float("inf")
        if not within:
            notes.append("reported cost is 0 but recompute is not")
        return CrossCheck(recomputed, reported, rel, within, "; ".join(notes))

    rel = abs(recomputed - reported) / reported
    within = rel <= tolerance
    return CrossCheck(recomputed, reported, rel, within, "; ".join(notes))
