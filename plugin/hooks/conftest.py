"""Shared fixtures and helpers for the ``hooks`` guard-pack tests.

The capt-hook pack loader skips ``conftest`` and ``test_*`` modules
(:func:`captain_hook.loader.is_test_module`), so the ``pytest`` import here never
reaches the shipped pack.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from hooks.common import ccx_supports


def fake_run(returncode: int, stdout: str = "", stderr: str = ""):
    """A ``subprocess.run`` double carrying the configured result only for the ``--help`` probe.

    A ``git check-ignore`` call (``_path_blocked``'s ignore probe, shelled through the same
    patched ``subprocess.run``) is answered "not ignored" (exit 1); only the ``ccx … --help``
    probe sees ``returncode``/``stdout``. Callers that never shell check-ignore see the plain
    result — the branch is inert for them.
    """

    def run(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
        if cmd[:2] == ["git", "check-ignore"]:
            return SimpleNamespace(returncode=1, stdout="", stderr="")
        return SimpleNamespace(returncode=returncode, stdout=stdout, stderr=stderr)

    return run


@pytest.fixture(autouse=True)
def clear_ccx_supports_cache():
    """Clear the process-wide ``ccx_supports`` probe cache around every test so a probe result
    never leaks between cases."""
    ccx_supports.cache_clear()
    yield
    ccx_supports.cache_clear()
