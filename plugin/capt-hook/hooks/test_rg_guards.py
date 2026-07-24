"""Tests for the version-gated rg rewrites in ``rg_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_rg_guards.py

The ``-i``/``-w`` rewrites are gated on ``ccx_supports("code", "grep", flag="--ignore-case")``,
which shells out to ``ccx … --help`` — an environment-dependent probe, so those shapes can
never live in inline ``tests={}`` (they would rewrite or block depending on the local binary).
Here the probe boundary (``ccx_bin`` + ``subprocess.run``) is monkeypatched and the
``ccx_supports`` cache is cleared around every case so a result never leaks between them;
``search_common.ccx_bin`` is pinned too so the rewritten command is deterministic.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest
from captain_hook.events import PreToolUseEvent
from captain_hook.types import HookResult
from cc_transcript.command import Occurrence

from conftest import (
    FILES_WITH_MATCHES_HELP,
    NATIVE_CONTEXT_HELP,
    NO_SUPPORT_HELP,
    REGEX_SUPPORTS_HELP,
    SUPPORTS_HELP,
    fake_run,
    make_evt,
    probe,
    run_visit,
)
from hooks import common, grep_guards, rg_guards, search_common
from hooks.common import ccx_supports


def event_occurrence(command: str, index: int = 0) -> tuple[PreToolUseEvent, Occurrence]:
    evt = make_evt(command)
    return evt, evt.cmd.line.occurrences[index]


def rg_rewrite(command: str, index: int = 0) -> str | None:
    evt, occ = event_occurrence(command, index)
    return rg_guards.rg_to(occ, cwd=evt.cwd)


def rg_verdict(command: str) -> HookResult | str | None:
    """The whole-line verdict from the ``visit=`` walk: a block ``HookResult``, a rewrite string, or
    ``None`` for a genuine allow (the thinned ``RgFlood`` gate is no longer the allow signal)."""
    return run_visit(make_evt(command), rg_guards.rg_visit)


def rg_rewrite_note(command: str) -> str:
    evt, occ = event_occurrence(command)
    parsed = rg_guards.rg_parse(occ, cwd=evt.cwd)
    assert parsed is not None
    return search_common.note_text(occ.command.raw, parsed)


def grep_rewrite(command: str) -> str | None:
    evt, occ = event_occurrence(command)
    return grep_guards.grep_to(occ, cwd=evt.cwd)


def grep_verdict(command: str) -> HookResult | str | None:
    return run_visit(make_evt(command), grep_guards.grep_visit)


class TestRgIgnoreCaseWord:
    """`rg -i`/`-w`/`--ignore-case` gate exactly as grep's do — through the same
    `ccx_supports("code","grep",flag="--ignore-case")` probe, mocked at the `subprocess.run`
    boundary — and an rg without `-i`/`-w` must never shell that probe.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `src/` must exist: `rg_parse` classifies path operands against the filesystem, so a
        # bare cwd would block these at parse instead of exercising the gate.
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("rg -i foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),
            ("rg -w foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("rg --ignore-case foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),  # long form
        ],
    )
    def test_rewrites_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        probe(monkeypatch, SUPPORTS_HELP)
        assert rg_rewrite(command) == expected

    @pytest.mark.parametrize("command", ["rg -i foo src/", "rg -w foo src/"])
    def test_blocks_when_flag_absent(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        # `--help` exits 0 but without `--ignore-case` (an older binary) → fall back to block.
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert rg_rewrite(command) is None

    def test_blocks_when_probe_errors(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common.subprocess, "run", fake_run(1, stderr='unknown flag "--ignore-case"'))
        assert rg_rewrite("rg -i foo src/") is None

    def test_ungated_shape_never_probes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A bare `rg foo src/` (no -i/-w) rewrites without shelling any subprocess.
        def no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            raise AssertionError("an rg without -i/-w must shell no subprocess")

        monkeypatch.setattr(common.subprocess, "run", no_probe)
        assert rg_rewrite("rg foo src/") == "/fake/ccx code grep foo --glob 'src/**'"


class TestRgNativeContext:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("rg -A 2 -B5 --context=7 foo", "/fake/ccx code grep foo -A=2 -B=5 --context=7"),
            ("rg --after-context 9 foo", "/fake/ccx code grep foo --after-context=9"),
        ],
    )
    def test_maps_exact_flags_when_supported(
        self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str
    ) -> None:
        probe(monkeypatch, NATIVE_CONTEXT_HELP)
        assert rg_rewrite(command) == expected

    def test_old_binary_keeps_expand_fallback(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert rg_rewrite("rg -A 2 -B 5 foo") == "/fake/ccx code grep foo --expand=5"


class TestRgFilesWithMatches:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize("command", ["rg -l foo", "rg --files-with-matches foo"])
    def test_maps_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        probe(monkeypatch, FILES_WITH_MATCHES_HELP)
        assert rg_rewrite(command) == "/fake/ccx code grep foo -l"
        assert rg_rewrite_note(command) == (
            f"Rewrote `{command}` → `ccx code grep`: same literal search, token-bounded."
        )

    @pytest.mark.parametrize("command", ["rg -l foo", "rg --files-with-matches foo"])
    def test_old_binary_keeps_drop_and_disclosure(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        probe(monkeypatch, NATIVE_CONTEXT_HELP)
        assert rg_rewrite(command) == "/fake/ccx code grep foo"
        assert rg_rewrite_note(command) == (
            f"Rewrote `{command}` → `ccx code grep`: same literal search, token-bounded. "
            "`-l` dropped — ccx returns the matching lines, not just filenames."
        )

    def test_l_with_context_suppresses_context_flags(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # ccx hard-errors on `-l` with `-A/-B/-C`, and native rg -l ignores context — emit `-l` alone.
        probe(monkeypatch, FILES_WITH_MATCHES_HELP)
        assert rg_rewrite("rg -l -A 2 foo") == "/fake/ccx code grep foo -l"


class TestRgPathGlobbing:
    """rg path operands share grep's on-disk classifier (`grep_glob`): a directory → `dir/**`,
    an explicit file passes through, several dirs brace together. Exact equality catches a wrongly
    narrowed `--glob`.
    """

    @pytest.fixture
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        (tmp_path / "src").mkdir()
        (tmp_path / "internal").mkdir()
        (tmp_path / "file.py").write_text("x\n")
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)
        return tmp_path

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("rg foo src/", "/fake/ccx code grep foo --glob 'src/**'"),
            # `.py` isn't in NON_SOURCE_EXTS — an explicit source file is gated (not exempted), so it rewrites.
            ("rg foo file.py", "/fake/ccx code grep foo --glob file.py"),
            ("rg foo src/ internal/", "/fake/ccx code grep foo --glob '{src,internal}/**'"),  # braced multi-dir
        ],
    )
    def test_disk_classified_globs(self, tree: Path, command: str, expected: str) -> None:
        assert rg_rewrite(command) == expected

    def test_nonexistent_path_runs_raw(self, tree: Path) -> None:
        # An absent path is not a directory → not tree-shaped → runs raw under fail-open.
        assert rg_verdict("rg foo nonexistent/") is None


class TestDependencyDirTargets:
    """Dependency-source operands are a policy steer: an unambiguous dep segment (`.venv/…`,
    `node_modules/…`) blocks textually regardless of on-disk existence, and a directory operand
    the cwd repo's own `git check-ignore` reports ignored blocks the same way — both even through
    a downstream pipe. Everything git doesn't claim (`~/.config/…`, an ignored plain file, a
    dep-lookalike pattern) runs raw. The block lands at the verdict, not in the emitter —
    `rg_to`/`grep_to` would happily glob an ignored dir.
    """

    @pytest.fixture(autouse=True)
    def repo(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        monkeypatch.setenv("GIT_CONFIG_GLOBAL", os.devnull)
        monkeypatch.setenv("GIT_CONFIG_SYSTEM", os.devnull)
        subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
        (tmp_path / ".gitignore").write_text("generated/\n*.log\n")
        (tmp_path / "generated").mkdir()
        (tmp_path / "app.log").write_text("err\n")
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)
        return tmp_path

    def test_rg_dep_segment_blocks_with_dep_steer(self) -> None:
        verdict = rg_verdict("rg -n 'class ToolUse' .venv/lib/ -A 20 | head")
        assert isinstance(verdict, HookResult) and "ccx repo locate" in verdict.message

    def test_rg_unparseable_flag_cannot_blind_the_steer(self) -> None:
        verdict = rg_verdict("rg --hidden needle .venv/ | head")
        assert isinstance(verdict, HookResult) and "ccx repo locate" in verdict.message

    def test_grep_dep_segment_blocks_with_dep_steer(self) -> None:
        verdict = grep_verdict("grep -rn foo .venv/")
        assert isinstance(verdict, HookResult) and "ccx repo locate" in verdict.message

    def test_grep_undotted_dep_segment_blocks(self) -> None:
        verdict = grep_verdict("grep -r foo node_modules/express | head")
        assert isinstance(verdict, HookResult) and "ccx repo locate" in verdict.message

    def test_ignored_dir_blocks_via_check_ignore(self) -> None:
        verdict = grep_verdict("grep -rn foo generated/")
        assert isinstance(verdict, HookResult) and "ccx repo locate" in verdict.message

    def test_ignored_file_runs_raw(self) -> None:
        assert grep_verdict("grep -i err app.log | head") is None

    def test_home_dotdirs_run_raw(self) -> None:
        assert rg_verdict("rg -l x ~/.claude/plugins/") is None
        assert grep_verdict("grep -rn foo ~/.config/fish/") is None

    def test_dep_lookalike_pattern_runs_raw(self) -> None:
        assert grep_verdict("grep -rn '.venv' README.md") is None


class TestRgOccurrenceRewrite:
    def test_compound_splices_only_rg(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        evt = make_evt("printf 'left  side'; rg foo")
        occurrence = evt.cmd.line.occurrences[1]
        replacement = rg_guards.rg_to(occurrence)
        assert replacement == "/fake/ccx code grep foo"
        assert evt.cmd.line.splice({occurrence.index: replacement}) == (
            "printf 'left  side'; /fake/ccx code grep foo"
        )

    def test_wrapped_rg_matches_but_never_rewrites(self) -> None:
        evt, occurrence = event_occurrence("sudo rg foo .")
        assert occurrence.command.unwrapped.executable == "rg"
        assert rg_guards.RgFlood().check_command_line(evt, evt.cmd.line) is True
        assert rg_guards.rg_to(occurrence) is None
        assert isinstance(rg_verdict("sudo rg foo ."), HookResult)


class TestInfraNoneAllows:
    """Fix 4: infra unavailability (no ``ccx`` on disk, or a required probe fails) runs the tree-shaped
    search raw — it never converts a failed rewrite into a block. Only a genuinely unmappable shape
    (a value-taking short like ``-t``) blocks, and that block still fires with ``ccx`` absent.
    """

    @pytest.fixture(autouse=True)
    def cwd(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: None)
        monkeypatch.setattr(common, "ccx_bin", lambda: None)

    def test_ccx_bin_none_allows_tree_rg(self) -> None:
        assert rg_verdict("rg needle") is None

    def test_ccx_bin_none_still_blocks_unmappable_shape(self) -> None:
        assert isinstance(rg_verdict("rg -t py foo"), HookResult)


class TestRgBigContextCount:
    """Fix 6: a context count past Python's int-string conversion limit forfeits the rewrite and runs
    raw — the emitter declines without raising, and the verdict allows.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    def test_overflow_count_forfeits_without_crash(self) -> None:
        command = "rg -A " + "9" * 5000 + " -B 1 needle"
        assert rg_rewrite(command) is None  # emitter forfeits, never raises ValueError
        assert rg_verdict(command) is None  # verdict runs raw
