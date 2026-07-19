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

``path_blocked`` shells ``git check-ignore`` through that *same* module-global
``subprocess.run``, so :func:`fake_run` answers a check-ignore probe "not ignored" (exit 1)
and reserves the configured result for the ``--help`` call — a bare exit-code fake would read
as "path is ignored" and block every rewrite.
"""

from __future__ import annotations

import subprocess
from collections.abc import Callable
from pathlib import Path
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine
from captain_hook.events import PreToolUseEvent
from cc_transcript.command import Occurrence

from conftest import NO_SUPPORT_HELP, SUPPORTS_HELP, fake_run, make_evt, probe
from hooks import common, grep_guards, rg_guards, search_common
from hooks.common import ccx_supports


def event_occurrence(command: str, index: int = 0) -> tuple[PreToolUseEvent, Occurrence]:
    evt = make_evt(command)
    return evt, evt.command_line.occurrences[index]


def rg_rewrite(command: str, index: int = 0) -> str | None:
    evt, occ = event_occurrence(command, index)
    return rg_guards.rg_to(evt, occ)


def grep_rewrite(command: str) -> str | None:
    evt, occ = event_occurrence(command)
    return grep_guards.grep_to(evt, occ)


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
        # A bare `rg foo src/` (no -i/-w) rewrites without shelling the `--help` probe.
        def no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            if cmd[:2] == ["git", "check-ignore"]:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            raise AssertionError("ccx_supports must not probe for an rg without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", no_probe)
        assert rg_rewrite("rg foo src/") == "/fake/ccx code grep foo --glob 'src/**'"


class TestRgPathGlobbing:
    """rg path operands share grep's on-disk classifier (`grep_glob`): a directory → `dir/**`,
    an explicit file passes through, several dirs brace together, an absent path blocks. Exact
    equality catches a wrongly narrowed `--glob`.
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

    def test_nonexistent_path_blocks(self, tree: Path) -> None:
        # An absent path has no faithful glob → block, never guess.
        assert rg_rewrite("rg foo nonexistent/") is None


class TestIgnoredDirTargets:
    """The silent-0-match regression, both executables: a search aimed inside a hidden or
    gitignored directory must block with the dependency-source steer, never rewrite to a
    `--glob` a stale `ccx` silently 0-matches. The directory exists on disk — existence is
    exactly what the dropped rewrite trusted — so the block must fire regardless.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)

    def test_rg_hidden_dir_blocks(self, tmp_path: Path) -> None:
        (tmp_path / ".venv" / "lib").mkdir(parents=True)
        # The verbatim incident shape minus its pipe still reaches `rg_to`, and still blocks.
        assert rg_rewrite("rg -n 'class ToolUse' .venv/lib/ -A 20") is None

    def test_grep_hidden_dir_blocks(self, tmp_path: Path) -> None:
        (tmp_path / ".venv").mkdir()
        assert grep_rewrite("grep -rn foo .venv/") is None

    @pytest.mark.parametrize(
        "to, command",
        [
            (rg_rewrite, "rg foo vendor/"),
            (grep_rewrite, "grep -rn foo vendor/"),
        ],
    )
    def test_gitignored_dir_blocks(
        self, tmp_path: Path, to: Callable[[str], str | None], command: str
    ) -> None:
        # The `git check-ignore` arm of `path_blocked`: a real repo whose .gitignore lists the dir.
        subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
        (tmp_path / ".gitignore").write_text("vendor/\n")
        (tmp_path / "vendor").mkdir()
        assert to(command) is None

    def test_mixed_data_and_source_target_stays_gated(self) -> None:
        # A data-file operand does not exempt a line that also targets a source dir: the one
        # source-directed operand keeps the no-stat lane from skipping the gate.
        command = "rg foo app.log src/"
        assert rg_guards.bounded_file_rg(CommandLine.parse(command).primary) is False

    def test_trailing_slash_defeats_data_file_exemption(self) -> None:
        # `src.log/` is a directory, not a `.log` data file (`Path.suffix` strips the slash to
        # `.log`) — the trailing slash must defeat the exemption; the slashless sibling stays exempt.
        gated = "rg TODO src.log/"
        assert rg_guards.bounded_file_rg(CommandLine.parse(gated).primary) is False
        exempt = "rg TODO src.log"
        assert rg_guards.bounded_file_rg(CommandLine.parse(exempt).primary) is True

    def test_value_short_flag_defeats_data_file_exemption(self) -> None:
        # `-d` is rg's max-depth (a value short), not a boolean — the walk must consume its `1`
        # rather than leak it as a phantom pattern that leaves `app.log` a lone data operand.
        command = "rg -d 1 app.log"
        assert rg_guards.bounded_file_rg(CommandLine.parse(command).primary) is False


class TestRgOccurrenceRewrite:
    def test_compound_splices_only_rg(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        evt = make_evt("printf 'left  side'; rg foo")
        occurrence = evt.command_line.occurrences[1]
        replacement = rg_guards.rg_to(evt, occurrence)
        assert replacement == "/fake/ccx code grep foo"
        assert evt.command_line.splice({occurrence.index: replacement}) == (
            "printf 'left  side'; /fake/ccx code grep foo"
        )

    def test_wrapped_rg_matches_but_never_rewrites(self) -> None:
        evt, occurrence = event_occurrence("sudo rg foo .")
        assert occurrence.command.unwrapped.executable == "rg"
        assert rg_guards.RgFlood().check_command_line(evt, evt.command_line) is True
        assert rg_guards.rg_to(evt, occurrence) is None
        assert rg_guards.rg_block_if(evt, occurrence) is True


class TestRgBoundedPassthrough:
    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / "real.py").write_text("x\n")
        (tmp_path / "big.py").write_text("x" * (common.LARGE_READ_BYTES + 1))
        (tmp_path / "half_a.py").write_text("x" * (common.LARGE_READ_BYTES // 2 + 1))
        (tmp_path / "half_b.py").write_text("x" * (common.LARGE_READ_BYTES // 2 + 1))
        (tmp_path / "sub").mkdir()
        (tmp_path / "dir.json").mkdir()
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command",
        [
            "rg -v foo real.py",
            "rg foo missing.log",
        ],
    )
    def test_bounded_file_lanes_do_not_fire(self, command: str) -> None:
        evt = make_evt(command)
        assert rg_guards.bounded_file_rg(evt.command_line.primary) is True
        assert rg_guards.RgFlood().check_command_line(evt, evt.command_line) is False

    @pytest.mark.parametrize(
        "command",
        [
            "rg -c foo big.py",
            "rg -l foo big.py",
            "rg --count foo big.py",
            "rg --files-with-matches foo big.py",
            "rg --files-without-match foo big.py",
            "rg --count-matches foo big.py",
        ],
    )
    def test_output_bounded_flags_skip_size_cap(self, command: str) -> None:
        assert rg_guards.bounded_file_rg(CommandLine.parse(command).primary) is True

    @pytest.mark.parametrize(
        "command",
        [
            "rg -v foo big.py",
            "rg -v foo half_a.py half_b.py",
            "rg -c foo sub",
            "rg -c foo ghost.py",
            "rg -c foo",
            "rg -v foo '*.py'",
        ],
    )
    def test_unbounded_recursive_or_stat_shapes_fire(self, command: str) -> None:
        evt = make_evt(command)
        assert rg_guards.bounded_file_rg(evt.command_line.primary) is False
        assert rg_guards.RgFlood().check_command_line(evt, evt.command_line) is True

    @pytest.mark.parametrize(
        "command",
        [
            "rg -o foo real.py",
            "rg -o foo missing.log",
            "rg --only-matching foo real.py",
            "rg --json foo real.py",
            "rg --json foo missing.log",
            "RIPGREP_CONFIG_PATH=rg.conf rg foo real.py",
            "RIPGREP_CONFIG_PATH=rg.conf rg foo missing.log",
        ],
    )
    def test_forfeits_both_bounded_lanes(self, command: str) -> None:
        evt = make_evt(command)
        assert rg_guards.bounded_file_rg(evt.command_line.primary.unwrapped) is False
        assert rg_guards.RgFlood().check_command_line(evt, evt.command_line) is True
