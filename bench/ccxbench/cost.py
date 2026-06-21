"""Recompute the cache-aware cost from token usage and cross-check `total_cost_usd`.

`total_cost_usd` (computed by Claude Code from its own price table) is the headline.
This module independently recomputes the bill from `modelUsage` x the configured
per-model prices and the universal cache multipliers, then asserts the two agree
within tolerance. A divergence means the price table or the envelope field is wrong.
"""

from __future__ import annotations

from dataclasses import dataclass

from .config import Prices
from .envelope import Envelope


@dataclass(frozen=True)
class CrossCheck:
    recomputed_usd: float
    reported_usd: float
    rel_delta: float
    within_tolerance: bool
    note: str


def recompute(env: Envelope, prices: Prices) -> tuple[float, list[str]]:
    """Recompute total cost (USD) from per-model usage at standard rates. Returns (cost, notes).

    Standard global pricing only. Premium regimes (fast mode, a non-standard service tier, a
    US inference-geo surcharge) are not modeled — they are noted so a resulting cross-check
    divergence is attributed, not mistaken for a price-table error.
    """
    notes: list[str] = []
    if env.fast_mode_state == "on":
        notes.append("fast-mode pricing not modeled")
    if env.usage.service_tier not in ("standard", ""):
        notes.append(f"non-standard service tier not modeled: {env.usage.service_tier}")
    if env.usage.inference_geo not in ("", "global", "not_available"):
        notes.append(f"inference_geo surcharge not modeled: {env.usage.inference_geo}")

    total_cc = env.usage.cache_create_total
    frac_1h = (env.usage.cache_create_1h / total_cc) if total_cc > 0 else 0.0

    if not env.model_usage:
        return 0.0, notes + ["no modelUsage (likely is_error)"]

    if len(env.model_usage) > 1:
        notes.append("multi-model: cache 5m/1h split approximated from run-level ratio")

    total = 0.0
    for model_id, mu in env.model_usage.items():
        price = prices.for_model(model_id)
        if price is None:
            notes.append(f"unknown model price: {model_id}")
            continue
        in_tok = int(mu.get("inputTokens", 0))
        out_tok = int(mu.get("outputTokens", 0))
        cr = int(mu.get("cacheReadInputTokens", 0))
        cc = int(mu.get("cacheCreationInputTokens", 0))
        cc_1h = cc * frac_1h
        cc_5m = cc - cc_1h
        usd = (
            in_tok * price.input
            + out_tok * price.output
            + cr * price.input * prices.cache_read
            + cc_5m * price.input * prices.cache_write_5m
            + cc_1h * price.input * prices.cache_write_1h
        ) / 1_000_000.0
        total += usd
    return total, notes


def crosscheck(env: Envelope, prices: Prices) -> CrossCheck:
    """Compare the recomputed cost to `total_cost_usd` within the configured tolerance."""
    recomputed, notes = recompute(env, prices)
    reported = env.total_cost_usd

    if reported == 0.0:
        within = abs(recomputed) < 1e-6
        rel = 0.0 if within else float("inf")
        if not within:
            notes.append("reported cost is 0 but recompute is not")
        return CrossCheck(recomputed, reported, rel, within, "; ".join(notes))

    rel = abs(recomputed - reported) / reported
    unknown = any(n.startswith("unknown model price") for n in notes)
    within = rel <= prices.tolerance and not unknown
    return CrossCheck(recomputed, reported, rel, within, "; ".join(notes))
