"""Tests for the version-gated grep rewrite in ``search_guards``.

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
"""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine

from hooks import common, search_guards
from hooks.common import ccx_supports

# `ccx code grep --help` text once the rg engine (v0.7.0+) lands vs. before it does.
SUPPORTS_HELP = "usage: ccx code grep [-i, --ignore-case] [-w, --word] [--glob G] ..."
NO_SUPPORT_HELP = "usage: ccx code grep [--glob G] [--expand int] ..."


def _fake_run(returncode: int, stdout: str = "", stderr: str = ""):
    def run(*_args: object, **_kwargs: object) -> SimpleNamespace:
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
        # A grep with no -i/-w must not shell out to the probe at all — it rewrites unconditionally.
        def _boom(*_args: object, **_kwargs: object) -> object:
            raise AssertionError("ccx_supports must not probe for a grep without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", _boom)
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
