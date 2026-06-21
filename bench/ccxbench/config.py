"""Load and resolve config.toml into typed settings."""

from __future__ import annotations

import tomllib
from dataclasses import dataclass
from pathlib import Path

BENCH_DIR = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class ModelPrice:
    """Base $/MTok for one model family, matched to a modelUsage key by `match`."""

    match: str
    input: float
    output: float


@dataclass(frozen=True)
class Prices:
    cache_write_5m: float
    cache_write_1h: float
    cache_read: float
    tolerance: float
    models: tuple[ModelPrice, ...]

    def for_model(self, model_id: str) -> ModelPrice | None:
        """Return the price family whose `match` substring is in `model_id`, longest first."""
        lid = model_id.lower()
        candidates = [m for m in self.models if m.match in lid]
        if not candidates:
            return None
        return max(candidates, key=lambda m: len(m.match))


@dataclass(frozen=True)
class Config:
    models: tuple[str, ...]
    repeats: int
    budget_usd: float
    permission_mode: str
    timeout_s: int
    strip_mcp: bool
    disallowed_tools: tuple[str, ...]
    prices: Prices
    ccx_bin: Path
    plugin_hooks: Path
    work_root: Path
    fixtures_root: Path
    results_dir: Path


def resolve_path(base: Path, p: str) -> Path:
    return (base / p).resolve()


def load(path: Path | None = None) -> Config:
    """Parse config.toml (defaults to bench/config.toml) into a resolved Config."""
    cfg_path = path or (BENCH_DIR / "config.toml")
    data = tomllib.loads(cfg_path.read_text())

    run = data["run"]
    paths = data["paths"]
    pr = data["prices"]

    models = tuple(
        ModelPrice(match=m["match"], input=m["input"], output=m["output"])
        for m in pr["models"].values()
    )
    prices = Prices(
        cache_write_5m=pr["cache_write_5m"],
        cache_write_1h=pr["cache_write_1h"],
        cache_read=pr["cache_read"],
        tolerance=pr["tolerance"],
        models=models,
    )

    return Config(
        models=tuple(run["models"]),
        repeats=int(run["repeats"]),
        budget_usd=float(run["budget_usd"]),
        permission_mode=run["permission_mode"],
        timeout_s=int(run["timeout_s"]),
        strip_mcp=bool(run["strip_mcp"]),
        disallowed_tools=tuple(run["disallowed_tools"]),
        prices=prices,
        ccx_bin=resolve_path(BENCH_DIR, paths["ccx_bin"]),
        plugin_hooks=resolve_path(BENCH_DIR, paths["plugin_hooks"]),
        work_root=resolve_path(BENCH_DIR, paths["work_root"]),
        fixtures_root=resolve_path(BENCH_DIR, paths["fixtures_root"]),
        results_dir=resolve_path(BENCH_DIR, paths["results_dir"]),
    )
