"""Filesystem-classifying tests for ``cat_rewrites``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the path so
the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_cat_rewrites.py

The single-file rewrite and the multi-file block classify operands against the filesystem
(``is_large`` / ``Path.stat``), so they can't live in inline ``tests={}`` — they would rewrite
or block depending on what happens to exist on disk. Here a ``tmp_path`` tree pins the sizes,
``$HOME`` is monkeypatched for ``~`` expansion, and ``cat_rewrites.ccx_bin`` is pinned so the
emitted command is deterministic and the "ccx unresolvable" block lane is exercised for real.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from conftest import make_evt

from hooks import cat_rewrites


def evt_occ(command: str, index: int = 0):
    evt = make_evt(command)
    return evt, evt.cmd.line.occurrences[index]


class TestBareCatFiles:
    @pytest.mark.parametrize(
        "command, expected",
        [
            ("cat a.md", ("a.md",)),
            ("cat a.md b.md", ("a.md", "b.md")),
            ("cat /etc/hosts", ("/etc/hosts",)),
        ],
        ids=["single", "multi", "absolute"],
    )
    def test_returns_operands(self, command: str, expected: tuple[str, ...]) -> None:
        _, occ = evt_occ(command)
        assert cat_rewrites.bare_cat_files(occ) == expected

    @pytest.mark.parametrize(
        "command, index",
        [
            ("cat -n a.md", 0),  # a flag first arg is not a bare read
            ("cat", 0),  # no operand
            ("bat a.md", 0),  # bat is not cat in this lane
            ("echo x", 0),  # not cat
            ("cat f | grep x", 0),  # piped
            ("grep x | cat", 1),  # piped (sink)
            ("cat > f", 0),  # redirect
            ("cat >> f", 0),  # redirect
        ],
        ids=["flag", "no-operand", "bat", "echo", "piped-head", "piped-sink", "redirect", "append"],
    )
    def test_declines(self, command: str, index: int) -> None:
        _, occ = evt_occ(command, index)
        assert cat_rewrites.bare_cat_files(occ) is None


class TestSingleCatTarget:
    def test_expansion_is_textual(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # `~` is swapped textually — no symlink resolution, no `..` collapse — so the emitted path
        # matches exactly what the shell would have handed cat.
        monkeypatch.setenv("HOME", str(tmp_path))
        _, occ = evt_occ("cat ~/sub/../notes.md")
        assert cat_rewrites.single_cat_target(occ) == f"{tmp_path}/sub/../notes.md"

    def test_mid_token_tilde_is_literal(self) -> None:
        _, occ = evt_occ("cat foo~bar.md")
        assert cat_rewrites.single_cat_target(occ) == "foo~bar.md"

    @pytest.mark.parametrize(
        "command",
        ["cat $d/main.go", "cat go.mod", "cat ./package.json", "cat a.md b.md", "cat -n a.md"],
        ids=["dollar", "manifest", "dotslash-manifest", "multi", "flag"],
    )
    def test_out_of_lane(self, command: str) -> None:
        _, occ = evt_occ(command)
        assert cat_rewrites.single_cat_target(occ) is None


class TestCatTo:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: "/fake/ccx")

    def test_large_file_rewrites_with_absolute_path(self, tmp_path: Path) -> None:
        big = tmp_path / "big.md"
        big.write_bytes(b"x" * (cat_rewrites.LARGE_READ_BYTES + 1))
        evt, occ = evt_occ(f"cat {big}")
        assert cat_rewrites.cat_to(evt, occ) == f"/fake/ccx code read {big} --full"

    def test_home_tilde_rewrites_expanded(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("HOME", str(tmp_path))
        (tmp_path / "big.md").write_bytes(b"x" * (cat_rewrites.LARGE_READ_BYTES + 1))
        evt, occ = evt_occ("cat ~/big.md")
        assert cat_rewrites.cat_to(evt, occ) == f"/fake/ccx code read {tmp_path}/big.md --full"

    @pytest.mark.parametrize("size", [0, 64, cat_rewrites.LARGE_READ_BYTES], ids=["empty", "small", "at-cap"])
    def test_small_or_at_cap_passes(self, tmp_path: Path, size: int) -> None:
        # Strictly greater than the cap rewrites; equal-to-cap does not (is_large uses `>`).
        f = tmp_path / "f.md"
        f.write_bytes(b"x" * size)
        evt, occ = evt_occ(f"cat {f}")
        assert cat_rewrites.cat_to(evt, occ) is None

    def test_nonexistent_passes(self, tmp_path: Path) -> None:
        evt, occ = evt_occ(f"cat {tmp_path / 'ghost.md'}")
        assert cat_rewrites.cat_to(evt, occ) is None

    def test_ccx_unresolvable_never_emits(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: None)
        big = tmp_path / "big.md"
        big.write_bytes(b"x" * (cat_rewrites.LARGE_READ_BYTES + 1))
        evt, occ = evt_occ(f"cat {big}")
        assert cat_rewrites.cat_to(evt, occ) is None

    def test_compound_rewrites_only_the_large_occurrence(self, tmp_path: Path) -> None:
        big = tmp_path / "big.md"
        big.write_bytes(b"x" * (cat_rewrites.LARGE_READ_BYTES + 1))
        evt = make_evt(f"cat {big}; echo x; cat {tmp_path / 'ghost.md'}")
        first, echo, ghost = evt.cmd.line.occurrences
        assert cat_rewrites.cat_to(evt, first) == f"/fake/ccx code read {big} --full"
        assert cat_rewrites.cat_to(evt, echo) is None
        assert cat_rewrites.cat_to(evt, ghost) is None


class TestCatBlock:
    def large(self, tmp_path: Path, name: str, size: int) -> Path:
        f = tmp_path / name
        f.write_bytes(b"x" * size)
        return f

    def test_single_large_blocks_only_without_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        big = self.large(tmp_path, "big.md", cat_rewrites.LARGE_READ_BYTES + 1)
        evt, occ = evt_occ(f"cat {big}")
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: "/fake/ccx")
        assert cat_rewrites.cat_block(evt, occ) is False  # ccx resolves → cat_to rewrites, no block
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: None)
        assert cat_rewrites.cat_block(evt, occ) is True  # ccx gone → never emit a broken command

    def test_single_small_passes(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: None)
        small = self.large(tmp_path, "small.md", 64)
        evt, occ = evt_occ(f"cat {small}")
        assert cat_rewrites.cat_block(evt, occ) is False

    @pytest.mark.parametrize("command", ["cat $d/main.go", "cat go.mod"], ids=["dollar", "manifest"])
    def test_single_out_of_lane_passes(
        self, command: str, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: None)
        evt, occ = evt_occ(command)
        assert cat_rewrites.cat_block(evt, occ) is False

    def test_multi_file_sum_over_cap_blocks(self, tmp_path: Path) -> None:
        a = self.large(tmp_path, "a.md", 15_000)
        b = self.large(tmp_path, "b.md", 15_000)
        evt, occ = evt_occ(f"cat {a} {b}")
        assert cat_rewrites.cat_block(evt, occ) is True

    def test_multi_file_sum_under_cap_passes(self, tmp_path: Path) -> None:
        a = self.large(tmp_path, "a.md", 5_000)
        b = self.large(tmp_path, "b.md", 5_000)
        evt, occ = evt_occ(f"cat {a} {b}")
        assert cat_rewrites.cat_block(evt, occ) is False

    def test_multi_file_nonexistent_and_dollar_operands_dont_count(self, tmp_path: Path) -> None:
        # Only stat-able operands sum; a $-expansion and a missing file contribute nothing, so the
        # 5 KB real file keeps the line under the cap.
        a = self.large(tmp_path, "a.md", 5_000)
        evt, occ = evt_occ(f"cat {a} $d/x.md {tmp_path / 'ghost.md'}")
        assert cat_rewrites.cat_block(evt, occ) is False

    def test_multi_file_one_over_cap_blocks_despite_dollar_sibling(self, tmp_path: Path) -> None:
        big = self.large(tmp_path, "big.md", cat_rewrites.LARGE_READ_BYTES + 1)
        evt, occ = evt_occ(f"cat {big} $d/x.md")
        assert cat_rewrites.cat_block(evt, occ) is True

    def test_home_multi_file_expands(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("HOME", str(tmp_path))
        self.large(tmp_path, "a.md", 15_000)
        self.large(tmp_path, "b.md", 15_000)
        evt, occ = evt_occ("cat ~/a.md ~/b.md")
        assert cat_rewrites.cat_block(evt, occ) is True


class TestManifestClassifier:
    @pytest.mark.parametrize(
        "command, expected",
        [
            ("cat go.mod", True),
            ("cat README.md", True),
            ("cat README-dev.md", True),  # README* prefix
            ("bat CLAUDE.md", True),  # ManifestCat keeps bat
            ("cat ./package.json", True),
            ("cat internal/go.mod", False),  # nested copy, not the root
            ("cat main.go", False),  # not a manifest
            ("cat go.mod | grep x", False),  # piped
            ("cat -n go.mod", False),  # flag, not a bare read
        ],
        ids=[
            "gomod", "readme", "readme-suffix", "bat-manifest", "dotslash",
            "nested", "source", "piped", "flag",
        ],
    )
    def test_is_manifest_cat(self, command: str, expected: bool) -> None:
        _, occ = evt_occ(command)
        assert cat_rewrites.is_manifest_cat(occ) is expected


class TestLineHasHeredoc:
    @pytest.mark.parametrize(
        "command, expected",
        [("cat << EOF", True), ("cat <<EOF", True), ("cat f.md", False), ("cat a << EOF", True)],
        ids=["spaced", "glued", "plain", "operand-plus-heredoc"],
    )
    def test_detects_heredoc(self, command: str, expected: bool) -> None:
        assert cat_rewrites.line_has_heredoc(make_evt(command)) is expected

    def test_heredoc_declines_rewrite_and_block(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # A large operand on a heredoc line still declines both lanes — per-occurrence heredoc
        # detection is unsound, so the whole line is left untouched.
        monkeypatch.setattr(cat_rewrites, "ccx_bin", lambda: None)
        big = tmp_path / "big.md"
        big.write_bytes(b"x" * (cat_rewrites.LARGE_READ_BYTES + 1))
        evt, occ = evt_occ(f"cat {big} <<EOF")
        assert cat_rewrites.cat_to(evt, occ) is None
        assert cat_rewrites.cat_block(evt, occ) is False
