"""Tests for the version-gated grep/rg rewrites in ``search_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_search_guards.py

The ``-i``/``-w`` rewrites are gated on ``ccx_supports("code", "grep", flag="--ignore-case")``,
which shells out to ``ccx … --help`` — an environment-dependent probe, so those shapes can
never live in inline ``tests={}`` (they would rewrite or block depending on the local binary).
Here the probe boundary (``ccx_bin`` + ``subprocess.run``) is monkeypatched and the
``ccx_supports`` cache is cleared around every case so a result never leaks between them;
``search_guards.ccx_bin`` is pinned too so the rewritten command is deterministic.

``_path_blocked`` shells ``git check-ignore`` through that *same* module-global
``subprocess.run``, so :func:`_fake_run` answers a check-ignore probe "not ignored" (exit 1)
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

from hooks import common, search_guards
from hooks.common import ccx_supports

# `ccx code grep --help` text once the rg engine (v0.7.0+) lands vs. before it does. SUPPORTS_HELP
# carries `--ignore-case` but not `--regex`, so it doubles as an old binary (v0.7–v0.10): `-i`/`-w`
# rewrite, but regex/multi-file shapes fall through. REGEX_SUPPORTS_HELP adds `--regex` (v0.11.0+).
SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [--glob G] ..."
REGEX_SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [-E, --regex] [--glob G] ..."
NO_SUPPORT_HELP = "usage: ccx code grep [--glob G] [--expand int] ..."


def _fake_run(returncode: int, stdout: str = "", stderr: str = ""):
    """A ``subprocess.run`` double that carries the configured result only for the ``--help`` probe.

    ``_path_blocked`` shells ``git check-ignore`` through the same patched ``subprocess.run``, so
    a check-ignore call is answered "not ignored" (exit 1) here; only the ``ccx … --help`` probe
    sees ``returncode``/``stdout``. Without the split every rewrite would read its path as ignored.
    """

    def run(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
        if cmd[:2] == ["git", "check-ignore"]:
            return SimpleNamespace(returncode=1, stdout="", stderr="")
        return SimpleNamespace(returncode=returncode, stdout=stdout, stderr=stderr)

    return run


def _evt(command: str) -> SimpleNamespace:
    return SimpleNamespace(command_line=CommandLine.parse(command), command=command)


class TestGrepIgnoreCaseWord:
    @pytest.fixture(autouse=True)
    def _pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `src/` must exist on disk: `_grep_to` classifies path operands against the filesystem
        # (finding 1), so a bare cwd would block these at parse instead of exercising the gate.
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def _probe(self, monkeypatch: pytest.MonkeyPatch, supported: bool) -> None:
        help_text = SUPPORTS_HELP if supported else NO_SUPPORT_HELP
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout=help_text))

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -i foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),
            ("grep -w foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("grep -i -w foo src/", "/fake/ccx code grep foo -i -w --glob 'src/**'"),
            ("grep --ignore-case foo .", "/fake/ccx code grep foo -i"),  # long form, `.` → repo-wide
            ("grep --word-regexp foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("grep -i foo", "/fake/ccx code grep foo -i"),  # no path → repo-wide
        ],
    )
    def test_rewrites_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        self._probe(monkeypatch, True)
        assert search_guards._grep_to(_evt(command)) == expected

    @pytest.mark.parametrize("command", ["grep -i foo src/", "grep -w foo src/", "grep -i -w foo src/"])
    def test_blocks_when_flag_absent(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        # `--help` returns 0 but without `--ignore-case` (an older binary) → fall back to block.
        self._probe(monkeypatch, False)
        assert search_guards._grep_to(_evt(command)) is None

    def test_blocks_when_probe_errors(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common.subprocess, "run", _fake_run(1, stderr='unknown flag "--ignore-case"'))
        assert search_guards._grep_to(_evt("grep -i foo src/")) is None

    def test_ungated_shape_never_probes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A grep with no -i/-w must not shell the `--help` probe — it rewrites unconditionally. The
        # path-classification `git check-ignore` call is expected (and answered "not ignored"); only
        # a `--help` probe shelled from here is the failure.
        def _no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            if cmd[:2] == ["git", "check-ignore"]:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            raise AssertionError("ccx_supports must not probe for a grep without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", _no_probe)
        assert search_guards._grep_to(_evt("grep -rn foo src/")) == "/fake/ccx code grep foo --glob 'src/**'"


class TestGrepNote:
    # Repo-wide shapes (no path) so the note is disk-independent: `_grep_note` runs `_grep_parse`,
    # which now classifies path operands against the filesystem.
    def test_discloses_l_fixed_and_expand_drops(self) -> None:
        note = search_guards._grep_note(_evt("grep -rlF -C 3 foo"))
        assert "`-l`" in note and "`-F`" in note and "--expand=3" in note

    def test_context_flag_discloses_count_drop(self) -> None:
        # Finding 6: the user's `-C N` count is dropped, and `--expand=3` is full-source, not context lines.
        note = search_guards._grep_note(_evt("grep -rn -C 3 foo"))
        assert "count was dropped" in note and "--expand=3" in note

    def test_dot_pattern_discloses_literal_dot(self) -> None:
        # Finding 2: `.` is whitelisted (mostly literal-intent) but grep reads it as any-char, so disclose.
        note = search_guards._grep_note(_evt("grep -rn foo.bar"))
        assert "any-char" in note

    def test_no_dot_carries_no_dot_disclosure(self) -> None:
        note = search_guards._grep_note(_evt("grep -rn foobar"))
        assert "any-char" not in note

    def test_plain_rewrite_carries_no_disclosures(self) -> None:
        note = search_guards._grep_note(_evt("grep -rn foobar"))
        assert note.endswith("token-bounded.")


class TestGrepPathGlobbing:
    """Finding 1: path operands are classified against the filesystem (dir vs file vs absent),
    so these disk-dependent shapes run against a real tmp tree with a pinned cwd. Assertions
    are exact (finding 4) — a substring check would pass a command that wrongly narrowed with
    a bad --glob.
    """

    @pytest.fixture
    def _tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        (tmp_path / "src").mkdir()
        (tmp_path / "internal").mkdir()
        (tmp_path / "v2.5").mkdir()  # dotted directory — the old extension heuristic mis-read it as a file
        (tmp_path / "file.py").write_text("x\n")
        (tmp_path / "Makefile").write_text("all:\n")  # extensionless file — the old heuristic mis-read it as a dir
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)
        return tmp_path

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -rn foo src/", "/fake/ccx code grep foo --glob 'src/**'"),
            ("grep foo file.py", "/fake/ccx code grep foo --glob file.py"),
            ("grep -rn foo src/ internal/", "/fake/ccx code grep foo --glob '{src,internal}/**'"),
            ("grep -rn --include='*.go' foo src/", "/fake/ccx code grep foo --glob 'src/**/*.go'"),
            ("grep -rn -C 3 foo src/", "/fake/ccx code grep foo --glob 'src/**' --expand=3"),
            ("grep foo Makefile", "/fake/ccx code grep foo --glob Makefile"),  # extensionless FILE, not Makefile/**
            ("grep -rn foo v2.5", "/fake/ccx code grep foo --glob 'v2.5/**'"),  # dotted DIR, not a file glob
        ],
    )
    def test_disk_classified_globs(self, _tree: Path, command: str, expected: str) -> None:
        assert search_guards._grep_to(_evt(command)) == expected

    @pytest.mark.parametrize(
        "command",
        [
            "grep -rn foo nonexistent/",  # absent path — no faithful glob → block
            "grep foo ghost.py",
            "grep -rn foo src/ ghost/",  # one real dir, one absent → block (never guess the absent one)
        ],
    )
    def test_nonexistent_path_blocks(self, _tree: Path, command: str) -> None:
        assert search_guards._grep_to(_evt(command)) is None


class TestGrepRepoWide:
    """Finding 4: repo-wide shapes emit NO dir --glob — a bare recursive grep or a `.` operand
    covers the whole repo. Exact equality proves the absence of a narrowing --glob; the inline
    `Rewrite(pattern=...)` checks are substrings a wrongly-globbed command would still satisfy.
    """

    @pytest.fixture(autouse=True)
    def _pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -rn foo", "/fake/ccx code grep foo"),
            ("grep -rl foo .", "/fake/ccx code grep foo"),
            ("grep -rn foo . src/", "/fake/ccx code grep foo"),  # finding 3: `.` sibling → whole repo
            ("grep -rn foo src/ .", "/fake/ccx code grep foo"),  # `.` after a dir path, same widening
            ("grep -rn --include='*.go' foo .", "/fake/ccx code grep foo --glob '*.go'"),
            ("grep -rn --include='*.go' foo . src/", "/fake/ccx code grep foo --glob '*.go'"),  # finding 3 + include
        ],
    )
    def test_no_dir_glob(self, command: str, expected: str) -> None:
        assert search_guards._grep_to(_evt(command)) == expected


class TestRgIgnoreCaseWord:
    """`rg -i`/`-w`/`--ignore-case` gate exactly as grep's do — through the same
    `ccx_supports("code","grep",flag="--ignore-case")` probe, mocked at the `subprocess.run`
    boundary — and an rg without `-i`/`-w` must never shell that probe.
    """

    @pytest.fixture(autouse=True)
    def _pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `src/` must exist: `_rg_parse` classifies path operands against the filesystem, so a
        # bare cwd would block these at parse instead of exercising the gate.
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def _probe(self, monkeypatch: pytest.MonkeyPatch, supported: bool) -> None:
        help_text = SUPPORTS_HELP if supported else NO_SUPPORT_HELP
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout=help_text))

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("rg -i foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),
            ("rg -w foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("rg --ignore-case foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),  # long form
        ],
    )
    def test_rewrites_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        self._probe(monkeypatch, True)
        assert search_guards._rg_to(_evt(command)) == expected

    @pytest.mark.parametrize("command", ["rg -i foo src/", "rg -w foo src/"])
    def test_blocks_when_flag_absent(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        # `--help` exits 0 but without `--ignore-case` (an older binary) → fall back to block.
        self._probe(monkeypatch, False)
        assert search_guards._rg_to(_evt(command)) is None

    def test_blocks_when_probe_errors(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common.subprocess, "run", _fake_run(1, stderr='unknown flag "--ignore-case"'))
        assert search_guards._rg_to(_evt("rg -i foo src/")) is None

    def test_ungated_shape_never_probes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A bare `rg foo src/` (no -i/-w) rewrites without shelling the `--help` probe.
        def _no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            if cmd[:2] == ["git", "check-ignore"]:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            raise AssertionError("ccx_supports must not probe for an rg without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", _no_probe)
        assert search_guards._rg_to(_evt("rg foo src/")) == "/fake/ccx code grep foo --glob 'src/**'"


class TestRgPathGlobbing:
    """rg path operands share grep's on-disk classifier (`_grep_glob`): a directory → `dir/**`,
    an explicit file passes through, several dirs brace together, an absent path blocks. Exact
    equality catches a wrongly narrowed `--glob`.
    """

    @pytest.fixture
    def _tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        (tmp_path / "src").mkdir()
        (tmp_path / "internal").mkdir()
        (tmp_path / "file.py").write_text("x\n")
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
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
    def test_disk_classified_globs(self, _tree: Path, command: str, expected: str) -> None:
        assert search_guards._rg_to(_evt(command)) == expected

    def test_nonexistent_path_blocks(self, _tree: Path) -> None:
        # An absent path has no faithful glob → block, never guess.
        assert search_guards._rg_to(_evt("rg foo nonexistent/")) is None


class TestIgnoredDirTargets:
    """The silent-0-match regression, both executables: a search aimed inside a hidden or
    gitignored directory must block with the dependency-source steer, never rewrite to a
    `--glob` a stale `ccx` silently 0-matches. The directory exists on disk — existence is
    exactly what the dropped rewrite trusted — so the block must fire regardless.
    """

    @pytest.fixture(autouse=True)
    def _pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)

    def test_rg_hidden_dir_blocks(self, tmp_path: Path) -> None:
        (tmp_path / ".venv" / "lib").mkdir(parents=True)
        # The verbatim incident shape minus its pipe still reaches `_rg_to`, and still blocks.
        assert search_guards._rg_to(_evt("rg -n 'class ToolUse' .venv/lib/ -A 20")) is None

    def test_grep_hidden_dir_blocks(self, tmp_path: Path) -> None:
        (tmp_path / ".venv").mkdir()
        assert search_guards._grep_to(_evt("grep -rn foo .venv/")) is None

    @pytest.mark.parametrize(
        "to, command",
        [
            (search_guards._rg_to, "rg foo vendor/"),
            (search_guards._grep_to, "grep -rn foo vendor/"),
        ],
    )
    def test_gitignored_dir_blocks(
        self, tmp_path: Path, to: Callable[[SimpleNamespace], str | None], command: str
    ) -> None:
        # The `git check-ignore` arm of `_path_blocked`: a real repo whose .gitignore lists the dir.
        subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
        (tmp_path / ".gitignore").write_text("vendor/\n")
        (tmp_path / "vendor").mkdir()
        assert to(_evt(command)) is None

    def test_mixed_data_and_source_target_stays_gated(self) -> None:
        # A data-file operand does not exempt a line that also targets a source dir: the one
        # source-directed operand keeps `RgNonSourceTargets` from skipping the gate.
        command = "rg foo app.log src/"
        cl = CommandLine.parse(command)
        assert search_guards.RgNonSourceTargets().check_command_line(_evt(command), cl) is False

    def test_trailing_slash_defeats_data_file_exemption(self) -> None:
        # `src.log/` is a directory, not a `.log` data file (`Path.suffix` strips the slash to
        # `.log`) — the trailing slash must defeat the exemption; the slashless sibling stays exempt.
        gated = "rg TODO src.log/"
        assert search_guards.RgNonSourceTargets().check_command_line(_evt(gated), CommandLine.parse(gated)) is False
        exempt = "rg TODO src.log"
        assert search_guards.RgNonSourceTargets().check_command_line(_evt(exempt), CommandLine.parse(exempt)) is True

    def test_value_short_flag_defeats_data_file_exemption(self) -> None:
        # `-d` is rg's max-depth (a value short), not a boolean — the walk must consume its `1`
        # rather than leak it as a phantom pattern that leaves `app.log` a lone data operand.
        command = "rg -d 1 app.log"
        cl = CommandLine.parse(command)
        assert search_guards.RgNonSourceTargets().check_command_line(_evt(command), cl) is False


class TestGrepRegexRewrite:
    """A BRE/ERE-safe pattern rewrites to `ccx code grep --regex` when the local binary advertises
    `--regex`; on an older binary (SUPPORTS_HELP, no `--regex`) the same shape falls through. Dialect
    is load-bearing: `|` is BRE-literal (no rewrite) but ERE-meta (rewrites under `-E`); `-P` never maps.
    """

    @pytest.fixture(autouse=True)
    def _pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `.` (the cwd) widens to repo-wide, so these shapes are disk-independent apart from the probe.
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def _probe(self, monkeypatch: pytest.MonkeyPatch, help_text: str) -> None:
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout=help_text))

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep 'foo.*' .", "/fake/ccx code grep 'foo.*' --regex"),  # BRE `.`/`*` are meta in both dialects
            ("grep '^class ' .", "/fake/ccx code grep '^class ' --regex"),  # anchored — the silent-0-match shape
            ("grep -E 'a|b' .", "/fake/ccx code grep 'a|b' --regex"),  # ERE alternation rewrites
            ("grep -G 'foo.*' .", "/fake/ccx code grep 'foo.*' --regex"),  # -G confirms the BRE default
        ],
    )
    def test_regex_safe_rewrites(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        self._probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert search_guards._grep_to(_evt(command)) == expected

    @pytest.mark.parametrize(
        "command",
        [
            "grep 'a|b' .",  # `|` is BRE-literal — no faithful regex rewrite, and not LITERAL_SAFE
            "grep -P 'x(?=y)' .",  # PCRE lookahead — -P never maps
            "grep 'foo(bar' .",  # BRE-literal `(`, meta in Rust — excluded from REGEX_SAFE_BRE
            r"grep 'a\d' .",  # backslash escape — excluded from every whitelist (defense in depth)
        ],
    )
    def test_unmappable_regex_blocks(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        self._probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert search_guards._grep_to(_evt(command)) is None

    def test_ere_alternation_stays_bre_literal_without_dash_e(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # `a|b` under the BRE default does NOT rewrite; the same pattern under `-E` does.
        self._probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert search_guards._grep_to(_evt("grep 'a|b' .")) is None
        assert search_guards._grep_to(_evt("grep -E 'a|b' .")) == "/fake/ccx code grep 'a|b' --regex"

    def test_probe_fail_over_existing_file_allows(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary (no `--regex`): a regex grep over an explicit existing file is bounded and
        # unrewritable → the condition never fires (genuine allow), never a block.
        (tmp_path / "real.py").write_text("x\n")
        self._probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' real.py")
        assert search_guards._grep_to(_evt("grep 'foo.*' real.py")) is None
        assert search_guards.GrepFlood().check_command_line(_evt("grep 'foo.*' real.py"), cl) is False

    def test_probe_fail_tree_wide_blocks(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary + `.` (a dir, not a bounded file): unrewritable and unbounded → the condition fires
        # and `_grep_to` is None → block.
        self._probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' .")
        assert search_guards._grep_to(_evt("grep 'foo.*' .")) is None
        assert search_guards.GrepFlood().check_command_line(_evt("grep 'foo.*' ."), cl) is True

    def test_regex_note_discloses_rg_engine(self) -> None:
        # The note for a regex rewrite names the engine; the dot-literal disclosure does not apply.
        note = search_guards._grep_note(_evt("grep 'foo.*' ."))
        assert "regex on the rg engine" in note and "any-char" not in note


class TestGrepMultiFilePaths:
    """Two or more explicit existing files carry as `ccx code grep` positionals (multi-file form,
    ccx ≥ v0.11.0), gated on the same `--regex` probe — old binaries hard-error on extra operands.
    """

    @pytest.fixture(autouse=True)
    def _tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "a.py").write_text("x\n")
        (tmp_path / "b.py").write_text("y\n")
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def _probe(self, monkeypatch: pytest.MonkeyPatch, help_text: str) -> None:
        monkeypatch.setattr(common.subprocess, "run", _fake_run(0, stdout=help_text))

    def test_multi_file_carries_operands(self, monkeypatch: pytest.MonkeyPatch) -> None:
        self._probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert search_guards._grep_to(_evt("grep foo a.py b.py")) == "/fake/ccx code grep foo a.py b.py"

    def test_single_file_keeps_glob_form(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # One explicit file stays on the old `--glob <file>` form (no `--regex` probe needed).
        self._probe(monkeypatch, NO_SUPPORT_HELP)
        assert search_guards._grep_to(_evt("grep foo a.py")) == "/fake/ccx code grep foo --glob a.py"

    def test_multi_file_probe_fail_allows(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary lacking `--regex`/multi-file: unrewritable, but both operands are bounded existing
        # files → the condition never fires (genuine allow).
        self._probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep foo a.py b.py")
        assert search_guards._grep_to(_evt("grep foo a.py b.py")) is None
        assert search_guards.GrepFlood().check_command_line(_evt("grep foo a.py b.py"), cl) is False


class TestGrepBoundedPassthrough:
    """`_bounded_file_grep`: an unrewritable grep over explicit existing files is bounded, so the
    condition stays silent (genuine allow); a nonexistent path or a directory operand fires and blocks.
    """

    @pytest.fixture(autouse=True)
    def _tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "real.py").write_text("x\n")
        (tmp_path / "sub").mkdir()
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_guards, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo real.py",  # count mode — unmappable, but a bounded existing file
            "grep -o foo real.py",  # only-matching — unmappable
            "grep -q foo real.py",  # exit-code — unmappable
            "grep --binary-files=text foo real.py",  # value-taking long consumes its =value
            "grep -m 5 foo real.py",  # value-taking short `-m` consumes its `5`
        ],
    )
    def test_bounded_existing_files_do_not_fire(self, command: str) -> None:
        assert search_guards._bounded_file_grep(CommandLine.parse(command)) is True
        assert search_guards.GrepFlood().check_command_line(_evt(command), CommandLine.parse(command)) is False

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo ghost.py",  # nonexistent operand → not bounded
            "grep -c foo sub/",  # directory operand → not bounded
            "grep -c foo real.py ghost.py",  # one real file, one absent → every operand must exist
            "grep -c foo",  # no operand at all → not bounded (tree-wide)
        ],
    )
    def test_unbounded_grep_fires_and_blocks(self, command: str) -> None:
        assert search_guards._bounded_file_grep(CommandLine.parse(command)) is False
        assert search_guards.GrepFlood().check_command_line(_evt(command), CommandLine.parse(command)) is True
        assert search_guards._grep_to(_evt(command)) is None

    def test_unknown_flag_is_not_bounded(self) -> None:
        # Conservative lexer: an unknown flag leaves the grep unbounded (it enters the hook), never a
        # wrong allow — even over an existing file.
        assert search_guards._bounded_file_grep(CommandLine.parse("grep --frobnicate foo real.py")) is False
