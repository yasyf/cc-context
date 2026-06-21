"""Clone pinned OSS repos and stage the guard pack used by the ccx arm.

Real repos give complex, large-context tasks where ccx's compact reads/search should
matter. The guard pack is sourced from a git ref (HEAD's working copy is mid-rework and
fails to import on current capt-hook), so the ccx arm runs the released, loadable guards.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

from .config import BENCH_DIR, Config, Repo

REPO_ROOT = BENCH_DIR.parent


def repo_checkout(cfg: Config, name: str) -> Path:
    return cfg.fixtures_root / name


def clone(cfg: Config, r: Repo) -> Path:
    """Shallow-clone one repo at its pinned ref (idempotent)."""
    dest = repo_checkout(cfg, r.name)
    if dest.exists():
        return dest
    cfg.fixtures_root.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ["git", "clone", "--depth", "1", "--branch", r.ref, r.url, str(dest)],
        check=True,
        capture_output=True,
        text=True,
    )
    return dest


def clone_all(cfg: Config) -> dict[str, Path]:
    return {r.name: clone(cfg, r) for r in cfg.repos}


def extract_guards(cfg: Config) -> None:
    """Write the guard pack from guards_ref into the plugin_hooks dir for the ccx arm."""
    cfg.plugin_hooks.mkdir(parents=True, exist_ok=True)
    out = subprocess.run(
        ["git", "-C", str(REPO_ROOT), "show", f"{cfg.guards_ref}:plugin/hooks/ccx_guards.py"],
        check=True,
        capture_output=True,
        text=True,
    )
    (cfg.plugin_hooks / "ccx_guards.py").write_text(out.stdout)
