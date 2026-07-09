"""Clone the pinned OSS repos used for complex, large-context tasks.

Real repos give tasks where ccx's compact reads/search should matter. The ccx guard pack
is no longer staged here — the ccx arm points capt-hook straight at the canonical
`plugin/hooks` pack (see config `plugin_hooks`).
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path

from .config import Config, Repo


def repo_checkout(cfg: Config, name: str) -> Path:
    return cfg.fixtures_root / name


def _at_ref(dest: Path, ref: str) -> bool:
    """True if `dest` has HEAD at the commit `ref` points to with a clean working tree."""
    head = subprocess.run(["git", "-C", str(dest), "rev-parse", "HEAD"], capture_output=True, text=True)
    want = subprocess.run(["git", "-C", str(dest), "rev-parse", f"{ref}^{{commit}}"], capture_output=True, text=True)
    if head.returncode != 0 or want.returncode != 0 or head.stdout.strip() != want.stdout.strip():
        return False
    status = subprocess.run(["git", "-C", str(dest), "status", "--porcelain"], capture_output=True, text=True)
    return status.returncode == 0 and not status.stdout.strip()


def clone(cfg: Config, r: Repo) -> Path:
    """Shallow-clone one repo at its pinned ref.

    Idempotent, but never trusts an existing checkout blindly: it is reused only when HEAD
    resolves to the pinned ref AND the working tree is clean; otherwise it is deleted and
    re-cloned (a cache dir, so convergence beats crashing on a mutated checkout)."""
    dest = repo_checkout(cfg, r.name)
    if dest.exists():
        if _at_ref(dest, r.ref):
            return dest
        print(f"repos: {r.name} checkout not at {r.ref} or dirty — deleting and re-cloning", file=sys.stderr)
        shutil.rmtree(dest)
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
