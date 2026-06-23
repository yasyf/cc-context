"""Clone the pinned OSS repos used for complex, large-context tasks.

Real repos give tasks where ccx's compact reads/search should matter. The ccx guard pack
is no longer staged here — the ccx arm points capt-hook straight at the canonical
`plugin/hooks` pack (see config `plugin_hooks`).
"""

from __future__ import annotations

import subprocess
from pathlib import Path

from .config import Config, Repo


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
