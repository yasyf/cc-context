"""Tests for bounded search sinks and capped filesystem walks."""

from __future__ import annotations

from pathlib import Path

import pytest

from hooks import common, search_common


@pytest.mark.parametrize(
    ("args", "expected"),
    [
        (("-n", "1,20p"), True),
        (("--quiet", "20p"), True),
        (("--silent", " 1 , 20 p "), True),
        (("-n", "-e", "1,20p"), True),
        (("-n", "-e1,20p"), True),
        (("-n", "--expression=1,20p"), True),
        (("20q",), True),
        (("-n", "1,2000000p"), False),
        (("2000000q",), False),
        (("1,20p",), False),
        (("-n", "1,20p", "extra.txt"), False),
        (("-n", "-e", "1p", "-e", "2p"), False),
        (("-n", "/needle/p"), False),
        (("-n", "1,20p;21q"), False),
        (("-f", "script.sed"), False),
        (("-n", "²p"), False),
        (("9" * 5000 + "q",), False),
        (("0q",), False),
        (("1,20p", "-n"), False),
    ],
    ids=[
        "quiet-short",
        "quiet-long",
        "silent-whitespace",
        "separate-expression",
        "glued-expression",
        "long-expression",
        "quit",
        "print-over-cap",
        "quit-over-cap",
        "autoprint",
        "second-positional",
        "second-expression",
        "regex-address",
        "joined-scripts",
        "flag",
        "unicode-digit",
        "over-long-count",
        "zero-address",
        "flag-after-script",
    ],
)
def test_sed_bounded(args: tuple[str, ...], expected: bool) -> None:
    assert search_common.sed_bounded(args) is expected


@pytest.mark.parametrize(
    ("args", "expected"),
    [
        (("NR<20",), True),
        (("NR <= 20",), True),
        (("NR==1",), True),
        (("NR == 2000000 { print }",), True),
        (("NR<=20 {print $0}",), True),
        (("NR<=2000000",), False),
        (("NR>=20",), False),
        (("NR!=2",), False),
        (("{print}",), False),
        (("-F,", "NR<=20"), False),
        (("--posix", "NR<=20"), False),
        (("NR<=20", "extra.txt"), False),
        (("NR<=²",), False),
        (("NR==" + "9" * 5000,), True),
        (("NR<=20\n{print}",), False),
    ],
    ids=[
        "less-than",
        "whitespace",
        "equal",
        "print-action-equal",
        "print-record-action",
        "over-cap",
        "greater-equal",
        "not-equal",
        "bare-action",
        "field-separator",
        "flag",
        "second-positional",
        "unicode-digit",
        "equal-over-long-count",
        "embedded-newline",
    ],
)
def test_awk_bounded(args: tuple[str, ...], expected: bool) -> None:
    assert search_common.awk_bounded(args) is expected


@pytest.mark.parametrize(
    ("flag", "count", "expected"),
    [
        ("-n", "2000", True),
        ("-n", "²", False),
        ("-c", "9" * 5000, False),
    ],
    ids=["in-cap", "unicode-digit", "over-long-count"],
)
def test_count_bounded(flag: str, count: str, expected: bool) -> None:
    assert search_common.count_bounded(flag, count) is expected


def test_tree_size_capped_accepts_file_and_directory_mix(tmp_path: Path) -> None:
    (tmp_path / "root.txt").write_text("root")
    directory = tmp_path / "d"
    directory.mkdir()
    (directory / "child.txt").write_text("child")

    assert search_common.tree_size_capped(["root.txt", "d"], cwd=tmp_path)


def test_tree_size_capped_rejects_over_cap_directory(tmp_path: Path) -> None:
    directory = tmp_path / "d"
    directory.mkdir()
    (directory / "huge").write_bytes(b"x" * (common.LARGE_READ_BYTES + 1))

    assert not search_common.tree_size_capped(["d"], cwd=tmp_path)


def test_tree_size_capped_rejects_entry_spam(tmp_path: Path) -> None:
    directory = tmp_path / "d"
    directory.mkdir()
    for i in range(search_common.DIR_WALK_ENTRY_CAP + 1):
        (directory / str(i)).touch()

    assert not search_common.tree_size_capped(["d"], cwd=tmp_path)


def test_tree_size_capped_skips_symlink_to_huge_file(tmp_path: Path) -> None:
    huge = tmp_path / "huge"
    huge.write_bytes(b"x" * (common.LARGE_READ_BYTES + 1))
    directory = tmp_path / "d"
    directory.mkdir()
    (directory / "small").write_text("small")
    (directory / "linked-huge").symlink_to(huge)

    assert search_common.tree_size_capped(["d"], cwd=tmp_path)


def test_tree_size_capped_follows_operand_symlinks(tmp_path: Path) -> None:
    (tmp_path / "small").write_text("small")
    (tmp_path / "huge").write_bytes(b"x" * (common.LARGE_READ_BYTES + 1))
    (tmp_path / "small-link").symlink_to(tmp_path / "small")
    (tmp_path / "huge-link").symlink_to(tmp_path / "huge")

    assert search_common.tree_size_capped(["small-link"], cwd=tmp_path)
    assert not search_common.tree_size_capped(["huge-link"], cwd=tmp_path)


def test_tree_size_capped_rejects_missing_operand(tmp_path: Path) -> None:
    assert not search_common.tree_size_capped(["missing"], cwd=tmp_path)
