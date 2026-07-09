"""Load and resolve config.toml into typed settings."""

from __future__ import annotations

import tomllib
from dataclasses import dataclass
from pathlib import Path

BENCH_DIR = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class Repo:
    """A pinned OSS repo cloned for complex large-context tasks."""

    name: str
    url: str
    ref: str
    kind: str


@dataclass(frozen=True)
class Config:
    models: tuple[str, ...]
    repeats: int
    max_turns: int
    safety_ceiling_usd: float
    permission_mode: str
    timeout_s: int
    strip_mcp: bool
    disallowed_tools: tuple[str, ...]
    min_traversal_bytes: int
    repos: tuple[Repo, ...]
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
    corpus = data["corpus"]

    repos = tuple(
        Repo(name=r["name"], url=r["url"], ref=r["ref"], kind=r["kind"]) for r in data.get("repos", [])
    )

    return Config(
        models=tuple(run["models"]),
        repeats=int(run["repeats"]),
        max_turns=int(run["max_turns"]),
        safety_ceiling_usd=float(run["safety_ceiling_usd"]),
        permission_mode=run["permission_mode"],
        timeout_s=int(run["timeout_s"]),
        strip_mcp=bool(run["strip_mcp"]),
        disallowed_tools=tuple(run["disallowed_tools"]),
        min_traversal_bytes=int(corpus["min_traversal_bytes"]),
        repos=repos,
        ccx_bin=resolve_path(BENCH_DIR, paths["ccx_bin"]),
        plugin_hooks=resolve_path(BENCH_DIR, paths["plugin_hooks"]),
        work_root=resolve_path(BENCH_DIR, paths["work_root"]),
        fixtures_root=resolve_path(BENCH_DIR, paths["fixtures_root"]),
        results_dir=resolve_path(BENCH_DIR, paths["results_dir"]),
    )
