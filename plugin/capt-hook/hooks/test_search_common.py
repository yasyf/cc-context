"""Tests for the surviving search-family primitives: policy screens, operand shape, and the glob emitter."""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from hooks import search_common


@pytest.mark.parametrize(
    ("path", "expected"),
    [
        ("~/.claude/projects/x.jsonl", True),
        (".claude/projects", True),
        ("/home/u/.claude/projects/sess/turn.jsonl", True),
        ("data.jsonl", False),
        ("src/claude/projects", False),
        ("docs/x.claude/projects-notes.md", False),  # lookalike substring, not consecutive segments
    ],
    ids=["home-jsonl", "bare-dir", "abs-nested", "plain-jsonl", "not-hidden", "substring-lookalike"],
)
def test_is_transcript_path(path: str, expected: bool) -> None:
    assert search_common.is_transcript_path(path) is expected


@pytest.mark.parametrize(
    ("path", "expected"),
    [
        (".git/objects", True),
        (".jj/repo", True),
        (".hg/store", True),
        (".svn/pristine", True),
        (".venv/lib/python3.13", True),
        ("node_modules/express", True),
        ("node_modules/.cache", True),
        ("~/.local/lib/python3.13/site-packages/pkg", True),
        ("/usr/lib/python3/dist-packages/foo", True),
        ("src/.hidden/x", False),
        ("~/.config/fish", False),
        (".github/workflows", False),
        ("~/.claude/plugins", False),
        ("./src", False),
        ("../src", False),
        ("src/internal", False),
        ("a.venv/b", False),
    ],
    ids=[
        "git",
        "jj",
        "hg",
        "svn",
        "venv",
        "node-modules",
        "node-modules-cache",
        "site-packages",
        "dist-packages",
        "mid-hidden",
        "config",
        "github",
        "claude-plugins",
        "dot",
        "dotdot",
        "plain",
        "not-a-segment",
    ],
)
def test_has_dependency_segment(path: str, expected: bool) -> None:
    assert search_common.has_dependency_segment(path) is expected


class TestAnyGitIgnored:
    """``any_git_ignored`` consults the cwd repo's ignore rules for directory operands only;
    every non-hit — an ignored plain file, an unignored dir, no repo, no cwd — is ``False``
    under fail-open, and a 128 on one operand never masks another."""

    @pytest.fixture
    def repo(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        # A host's global excludes must not leak into the tmp repo's ignore verdicts.
        monkeypatch.setenv("GIT_CONFIG_GLOBAL", os.devnull)
        monkeypatch.setenv("GIT_CONFIG_SYSTEM", os.devnull)
        subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
        (tmp_path / ".gitignore").write_text("build/\n*.log\n")
        (tmp_path / "build").mkdir()
        (tmp_path / "src").mkdir()
        (tmp_path / "app.log").write_text("err\n")
        return tmp_path

    def test_ignored_dir_hits(self, repo: Path) -> None:
        assert search_common.any_git_ignored(["build"], cwd=repo) is True

    def test_ignored_file_never_consulted(self, repo: Path) -> None:
        assert search_common.any_git_ignored(["app.log"], cwd=repo) is False

    def test_bad_operand_cannot_mask_an_ignored_dir(self, repo: Path) -> None:
        # `/` is outside the repo → the batch exits 128, and the fallback must still find `build`.
        assert search_common.any_git_ignored(["/", "build"], cwd=repo) is True

    def test_pathspec_magic_operand_skipped(self, repo: Path) -> None:
        (repo / ":(top)build").mkdir()
        assert search_common.any_git_ignored([":(top)build"], cwd=repo) is False

    def test_tilde_operand_expands(self, repo: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("HOME", str(repo))
        assert search_common.any_git_ignored(["~/build"], cwd=repo) is True

    def test_unignored_dirs_miss(self, repo: Path) -> None:
        assert search_common.any_git_ignored(["src", "."], cwd=repo) is False

    def test_not_a_repo(self, tmp_path: Path) -> None:
        assert search_common.any_git_ignored(["."], cwd=tmp_path) is False

    def test_untrusted_cwd(self) -> None:
        assert search_common.any_git_ignored(["build"], cwd=None) is False


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("grep foo $(printf /p)", True),
        ("grep foo `printf x`", True),
        ("grep foo '$(printf x)'", True),  # textual: a quoted `$(` still forfeits the rewrite
        ("grep -n foo $d/host.go", False),  # a bare `$VAR` is not a substitution
        ("grep foo src/", False),
    ],
    ids=["dollar-paren", "backtick", "quoted-subst", "var-only", "plain"],
)
def test_has_command_substitution(raw: str, expected: bool) -> None:
    assert search_common.has_command_substitution(raw) is expected


@pytest.mark.parametrize(
    ("operand", "expected"),
    [
        ("$d", True),  # var expansion
        ("$d/host.go", True),
        ("~/notes.md", True),  # leading tilde
        ("src[old]/", True),  # bracket → char class
        ("*.go", True),  # glob star
        ("file?.py", True),  # glob question
        ("src/", False),  # plain dir
        ("foo~bar.go", False),  # mid-token tilde is literal
        (".", False),
    ],
    ids=["var", "var-path", "tilde", "bracket", "star", "question", "plain", "mid-tilde", "dot"],
)
def test_forfeits_operand(operand: str, expected: bool) -> None:
    assert search_common.forfeits_operand(operand) is expected


@pytest.mark.parametrize(
    ("args", "expected"),
    [
        (("-A", "9" * 5000, "-B", "1", "needle"), True),  # 5000-digit count past the int-string limit
        (("-A", "20", "foo"), False),  # ordinary count
        (("foo", "src/"), False),
    ],
    ids=["overflow", "ordinary", "no-count"],
)
def test_forfeits_count(args: tuple[str, ...], expected: bool) -> None:
    assert search_common.forfeits_count(args) is expected


@pytest.mark.parametrize(
    ("args", "expected"),
    [
        (("-rn", "foo", "src/"), ["foo", "src/"]),  # tolerant: over-includes the pattern
        (("-A", "3", "foo", "."), ["3", "foo", "."]),  # unknown value arity not resolved — that is fine here
        (("foo", "--", "-weird.py"), ["foo", "-weird.py"]),  # post `--` positionals kept
        (("-", "foo"), ["-", "foo"]),  # a lone `-` (stdin) is a positional
        (("--recursive", "foo"), ["foo"]),  # long flags dropped
    ],
    ids=["short-bundle", "value-flag", "double-dash", "stdin-dash", "long-flag"],
)
def test_path_operands_raw(args: tuple[str, ...], expected: list[str]) -> None:
    assert search_common.path_operands_raw(args) == expected


def test_resolved_is_dir(tmp_path: Path) -> None:
    (tmp_path / "d").mkdir()
    (tmp_path / "f.py").write_text("x\n")
    assert search_common.resolved_is_dir(".", tmp_path) is True
    assert search_common.resolved_is_dir("..", tmp_path) is True
    assert search_common.resolved_is_dir("d", tmp_path) is True
    assert search_common.resolved_is_dir("d/", tmp_path) is True
    assert search_common.resolved_is_dir("f.py", tmp_path) is False  # a file
    assert search_common.resolved_is_dir("ghost", tmp_path) is False  # missing
    assert search_common.resolved_is_dir("d", None) is False  # no trusted cwd → fail open


class TestGrepGlob:
    """The glob emitter shared by grep and rg: a directory → ``dir/**``, several → braced, a lone file
    passes through, and out-of-repo / mixed / multi-file shapes forfeit (``None``) so the caller blocks
    or falls through rather than emitting a glob ccx would 0-match.
    """

    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / "src").mkdir()
        (tmp_path / "internal").mkdir()
        (tmp_path / "file.py").write_text("x\n")
        monkeypatch.chdir(tmp_path)

    @pytest.mark.parametrize(
        ("paths", "include", "expected"),
        [
            (["."], None, ""),  # `.` widens repo-wide
            (["./"], None, ""),
            (["src"], None, "src/**"),
            (["src/"], None, "src/**"),  # trailing slash stripped
            (["src", "internal"], None, "{src,internal}/**"),  # braced multi-dir
            (["file.py"], None, "file.py"),  # a lone file passes through
            (["src"], "*.go", "src/**/*.go"),  # include composes onto the dir root
            (["."], "*.go", "*.go"),  # `.` + include → bare include glob
            (["src", "file.py"], None, None),  # mixed dir+file → no single glob
            (["file.py", "internal/x.py"], None, None),  # two non-dirs → multi-file lane, not a glob
            (["/abs/src"], None, None),  # absolute operand → out-of-repo forfeit
            (["~/src"], None, None),  # `~` operand → forfeit
            (["../src"], None, None),  # `..` segment → forfeit
        ],
        ids=[
            "dot", "dot-slash", "dir", "dir-slash", "multi-dir", "lone-file", "include-on-dir",
            "dot-include", "mixed", "two-files", "absolute", "tilde", "dotdot",
        ],
    )
    def test_grep_glob(self, paths: list[str], include: str | None, expected: str | None) -> None:
        assert search_common.grep_glob(paths, include, cwd=Path.cwd()) == expected


def test_brace() -> None:
    assert search_common.brace(["src"]) == "src"
    assert search_common.brace(["src", "internal"]) == "{src,internal}"


@pytest.mark.parametrize(
    ("raw", "expected"),
    [("'*.go'", "*.go"), ('"a b"', "a b"), ("plain", "plain"), ("'unbalanced", "'unbalanced")],
    ids=["single", "double", "plain", "unbalanced"],
)
def test_unquote(raw: str, expected: str) -> None:
    assert search_common.unquote(raw) == expected
