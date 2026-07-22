"""Shared fixtures and helpers for the ``hooks`` guard-pack tests.

The capt-hook pack loader skips ``conftest`` and ``test_*`` modules
(:func:`captain_hook.loader.is_test_module`), so the ``pytest`` import here never
reaches the shipped pack.
"""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
from typing import TYPE_CHECKING

import pytest
from captain_hook import CommandLine, Rewritten, WalkContext
from captain_hook.types import Action, HookResult
from captain_hook.util.shell import normalize_executable, plain_words, resolve_cd

from hooks import common
from hooks.common import ccx_supports

if TYPE_CHECKING:
    from collections.abc import Callable

    from cc_transcript.command import Occurrence

# `ccx code grep --help` text once the rg engine (v0.7.0+) lands vs. before it does. SUPPORTS_HELP
# carries `--ignore-case` but not `--regex`, so it doubles as an old binary (v0.7–v0.10): `-i`/`-w`
# rewrite, but regex/multi-file shapes fall through. REGEX_SUPPORTS_HELP adds `--regex` (v0.11.0+),
# NATIVE_CONTEXT_HELP adds the exact grep/rg context flags, and FILES_WITH_MATCHES_HELP adds the
# file-listing mode on top.
SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [--glob G] ..."
REGEX_SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [-E, --regex] [--glob G] ..."
NATIVE_CONTEXT_HELP = (
    "usage: ccx code grep [-i, --ignore-case] [-w, --word] [-E, --regex] "
    "[-A, --after-context int] [-B, --before-context int] [-C, --context int] [--glob G] ..."
)
FILES_WITH_MATCHES_HELP = (
    "usage: ccx code grep [-i, --ignore-case] [-w, --word] [-E, --regex] "
    "[-A, --after-context int] [-B, --before-context int] [-C, --context int] "
    "[-l, --files-with-matches] [--glob G] ..."
)
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


def make_evt(command: str, cwd: str | Path | None = None) -> SimpleNamespace:
    """A stand-in ``PreToolUseEvent`` for the guard functions under test.

    ``cwd`` pins ``evt.cwd`` so cd-dependent behavior is deterministic; omitted, it defaults to the
    process cwd (which the cd-independent tests set via ``monkeypatch.chdir``). ``block`` mirrors the
    real ``evt.block`` so a ``visit`` verdict returns a genuine ``HookResult``.
    """
    line = CommandLine.parse(command)
    return SimpleNamespace(
        cmd=SimpleNamespace(line=line, raw=command, q=line.q),
        cwd=Path(cwd) if cwd is not None else Path.cwd(),
        block=lambda message: HookResult.of(Action.block, message),
    )


def run_visit(
    evt: SimpleNamespace,
    visit: Callable[[SimpleNamespace, Occurrence, WalkContext], object],
) -> HookResult | str | None:
    """Drive a ``visit=`` callable over ``evt``'s line exactly as the framework's walk does.

    Threads the effective cwd from ``evt.cwd`` through statically resolvable ``cd`` occurrences, builds
    the per-occurrence :class:`WalkContext`, and aggregates the verdicts: a ``HookResult`` aborts the
    walk (block), a ``str``/``Rewritten`` splices, and all-``None`` is a genuine allow. Returns the block
    ``HookResult``, the spliced rewrite string, or ``None`` for an allow.
    """
    cl = evt.cmd.line
    effective = evt.cwd
    replacements: dict[int, str] = {}
    for occ in cl.occurrences:
        command = occ.command
        ctx = WalkContext(
            cwd=effective,
            plain_words=plain_words(command.raw),
            spliceable=command.span is not None and "\\\n" not in command.raw,
        )
        result = visit(evt, occ, ctx)
        if isinstance(result, HookResult):
            return result
        if isinstance(result, Rewritten):
            replacements[occ.index] = result.text
        elif isinstance(result, str):
            replacements[occ.index] = result
        unwrapped = command.unwrapped
        if normalize_executable(unwrapped.executable) == "cd" and not occ.piped:
            effective = resolve_cd(unwrapped.args, effective)
    if replacements and (spliced := cl.splice(replacements)) != cl.raw:
        return spliced
    return None


def probe(monkeypatch: pytest.MonkeyPatch, help_text: str) -> None:
    monkeypatch.setattr(common.subprocess, "run", fake_run(0, stdout=help_text))


@pytest.fixture(autouse=True)
def clear_ccx_supports_cache():
    """Clear the process-wide ``ccx_supports`` probe cache around every test so a probe result
    never leaks between cases."""
    ccx_supports.cache_clear()
    yield
    ccx_supports.cache_clear()
