"""Shared fixtures and helpers for the ``hooks`` guard-pack tests.

The capt-hook pack loader skips ``conftest`` and ``test_*`` modules
(:func:`captain_hook.loader.is_test_module`), so the ``pytest`` import here never
reaches the shipped pack.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from captain_hook import CommandLine

from hooks import common
from hooks.common import ccx_supports

# `ccx code grep --help` text once the rg engine (v0.7.0+) lands vs. before it does. SUPPORTS_HELP
# carries `--ignore-case` but not `--regex`, so it doubles as an old binary (v0.7–v0.10): `-i`/`-w`
# rewrite, but regex/multi-file shapes fall through. REGEX_SUPPORTS_HELP adds `--regex` (v0.11.0+).
SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [--glob G] ..."
REGEX_SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [-E, --regex] [--glob G] ..."
NO_SUPPORT_HELP = "usage: ccx code grep [--glob G] [--expand int] ..."


def fake_run(returncode: int, stdout: str = "", stderr: str = ""):
    """A ``subprocess.run`` double carrying the configured result only for the ``--help`` probe.

    A ``git check-ignore`` call (``path_blocked``'s ignore probe, shelled through the same
    patched ``subprocess.run``) is answered "not ignored" (exit 1); only the ``ccx … --help``
    probe sees ``returncode``/``stdout``. Callers that never shell check-ignore see the plain
    result — the branch is inert for them.
    """

    def run(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
        if cmd[:2] == ["git", "check-ignore"]:
            return SimpleNamespace(returncode=1, stdout="", stderr="")
        return SimpleNamespace(returncode=returncode, stdout=stdout, stderr=stderr)

    return run


def make_evt(command: str) -> SimpleNamespace:
    return SimpleNamespace(command_line=CommandLine.parse(command), command=command)


def probe(monkeypatch: pytest.MonkeyPatch, help_text: str) -> None:
    monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout=help_text))


@pytest.fixture(autouse=True)
def clear_ccx_supports_cache():
    """Clear the process-wide ``ccx_supports`` probe cache around every test so a probe result
    never leaks between cases."""
    ccx_supports.cache_clear()
    yield
    ccx_supports.cache_clear()
