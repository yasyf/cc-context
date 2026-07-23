"""Tests for the version-gated grep rewrites in ``grep_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin/capt-hook uv run --project ../captain-hook --with pytest \
        pytest plugin/capt-hook/hooks/test_grep_guards.py

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
from pathlib import Path
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore
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


def grep_rewrite(command: str, index: int = 0) -> str | None:
    evt, occ = event_occurrence(command, index)
    return grep_guards.grep_to(evt, occ, cwd=evt.cwd)


def grep_verdict(command: str) -> HookResult | str | None:
    """The whole-line verdict from the ``visit=`` walk: a block ``HookResult``, a rewrite string, or
    ``None`` for a genuine allow (the thinned ``GrepFlood`` gate is no longer the allow signal)."""
    return run_visit(make_evt(command), grep_guards.grep_visit)


def grep_rewrite_note(command: str) -> str:
    evt, occ = event_occurrence(command)
    parsed = grep_guards.grep_parse(occ, cwd=evt.cwd)
    assert parsed is not None
    return search_common.note_text(occ.command.raw, parsed)


class TestGrepIgnoreCaseWord:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `src/` must exist on disk: `grep_to` classifies path operands against the filesystem
        # (finding 1), so a bare cwd would block these at parse instead of exercising the gate.
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
            ("grep -i foo src/", "/fake/ccx code grep foo -i --glob 'src/**'"),
            ("grep -w foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("grep -i -w foo src/", "/fake/ccx code grep foo -i -w --glob 'src/**'"),
            ("grep --ignore-case foo .", "/fake/ccx code grep foo -i"),  # long form, `.` → repo-wide
            ("grep --word-regexp foo src/", "/fake/ccx code grep foo -w --glob 'src/**'"),
            ("grep -i foo", "/fake/ccx code grep foo -i"),  # no path → repo-wide
        ],
    )
    def test_rewrites_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        probe(monkeypatch, SUPPORTS_HELP)
        assert grep_rewrite(command) == expected

    @pytest.mark.parametrize("command", ["grep -i foo src/", "grep -w foo src/", "grep -i -w foo src/"])
    def test_blocks_when_flag_absent(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        # `--help` returns 0 but without `--ignore-case` (an older binary) → fall back to block.
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert grep_rewrite(command) is None

    def test_blocks_when_probe_errors(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common.subprocess, "run", fake_run(1, stderr='unknown flag "--ignore-case"'))
        assert grep_rewrite("grep -i foo src/") is None

    def test_ungated_shape_never_probes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A grep with no -i/-w must not shell the `--help` probe — it rewrites unconditionally. The
        # path-classification `git check-ignore` call is expected (and answered "not ignored"); only
        # a `--help` probe shelled from here is the failure.
        def no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            if cmd[:2] == ["git", "check-ignore"]:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            raise AssertionError("ccx_supports must not probe for a grep without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", no_probe)
        assert grep_rewrite("grep -rn foo src/") == "/fake/ccx code grep foo --glob 'src/**'"


class TestGrepNativeContext:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -A 12 -B5 --context=7 foo", "/fake/ccx code grep foo -A=12 -B=5 --context=7"),
            ("grep --after-context 9 foo", "/fake/ccx code grep foo --after-context=9"),
        ],
    )
    def test_maps_exact_flags_when_supported(
        self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str
    ) -> None:
        probe(monkeypatch, NATIVE_CONTEXT_HELP)
        assert grep_rewrite(command) == expected

    def test_old_binary_keeps_expand_fallback(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite("grep -A 20 foo") == "/fake/ccx code grep foo --expand=3"


class TestGrepFilesWithMatches:
    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize("command", ["grep -rl foo .", "grep --files-with-matches foo ."])
    def test_maps_when_supported(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        probe(monkeypatch, FILES_WITH_MATCHES_HELP)
        assert grep_rewrite(command) == "/fake/ccx code grep foo -l"
        assert grep_rewrite_note(command) == (
            f"Rewrote `{command}` → `ccx code grep`: same literal search, token-bounded."
        )

    @pytest.mark.parametrize("command", ["grep -rl foo .", "grep --files-with-matches foo ."])
    def test_old_binary_keeps_drop_and_disclosure(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        probe(monkeypatch, NATIVE_CONTEXT_HELP)
        assert grep_rewrite(command) == "/fake/ccx code grep foo"
        assert grep_rewrite_note(command) == (
            f"Rewrote `{command}` → `ccx code grep`: same literal search, token-bounded. "
            "`-l` dropped — ccx returns the matching lines, not just filenames."
        )

    def test_l_with_context_suppresses_context_flags(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # ccx hard-errors on `-l` with `-A/-B/-C`, and native grep -l ignores context — the rewrite
        # emits `-l` alone; without `-l` the same context flag still carries through.
        probe(monkeypatch, FILES_WITH_MATCHES_HELP)
        assert grep_rewrite("grep -rl foo . -A 2") == "/fake/ccx code grep foo -l"
        assert grep_rewrite("grep -rn foo . -A 2") == "/fake/ccx code grep foo -A=2"


class TestGrepNote:
    # Repo-wide shapes (no path) so the note is disk-independent: `grep_note` runs `grep_parse`,
    # which now classifies path operands against the filesystem.
    def test_discloses_l_fixed_without_native_context(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, NATIVE_CONTEXT_HELP)
        note = grep_rewrite_note("grep -rlF -C 3 foo")
        assert "`-l`" in note and "`-F`" in note and "--expand" not in note

    def test_context_fallback_discloses_count_drop(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        note = grep_rewrite_note("grep -rn -C 3 foo")
        assert "count was dropped" in note and "--expand=3` adds 3 context lines around each hit" in note

    def test_dot_pattern_regex_rewrites_not_literal(self) -> None:
        # `.` is a dialect metachar, so grep now rewrites it faithfully as a regex — the note names
        # the engine, not the old any-char-literal disclosure. rg still literal-rewrites `.` (its
        # default engine reads `.` as a wildcard the literal search can't honor), so the
        # `.`-literal disclosure stays live there.
        grep_note = grep_rewrite_note("grep -rn foo.bar")
        assert "regex on the rg engine" in grep_note and "any-char" not in grep_note
        rg_evt, rg_occ = event_occurrence("rg foo.bar")
        rg_parsed = rg_guards.rg_parse(rg_occ, cwd=rg_evt.cwd)
        assert rg_parsed is not None
        assert "any-char" in search_common.note_text(rg_occ.command.raw, rg_parsed)

    def test_no_dot_carries_no_dot_disclosure(self) -> None:
        note = grep_rewrite_note("grep -rn foobar")
        assert "any-char" not in note

    def test_plain_rewrite_carries_no_disclosures(self) -> None:
        note = grep_rewrite_note("grep -rn foobar")
        assert note.endswith("token-bounded.")


class TestGrepPathGlobbing:
    """Finding 1: path operands are classified against the filesystem (dir vs file vs absent),
    so these disk-dependent shapes run against a real tmp tree with a pinned cwd. Assertions
    are exact (finding 4) — a substring check would pass a command that wrongly narrowed with
    a bad --glob.
    """

    @pytest.fixture
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
        (tmp_path / "src").mkdir()
        (tmp_path / "internal").mkdir()
        (tmp_path / "v2.5").mkdir()  # dotted directory — the old extension heuristic mis-read it as a file
        (tmp_path / "file.py").write_text("x\n")
        (tmp_path / "Makefile").write_text("all:\n")  # extensionless file — the old heuristic mis-read it as a dir
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)
        return tmp_path

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -rn foo src/", "/fake/ccx code grep foo --glob 'src/**'"),
            ("grep foo file.py", "/fake/ccx code grep foo --glob file.py"),
            ("grep -rn foo src/ internal/", "/fake/ccx code grep foo --glob '{src,internal}/**'"),
            ("grep -rn --include='*.go' foo src/", "/fake/ccx code grep foo --glob 'src/**/*.go'"),
            ("grep -rn -C 3 foo src/", "/fake/ccx code grep foo --glob 'src/**' -C=3"),
            ("grep foo Makefile", "/fake/ccx code grep foo --glob Makefile"),  # extensionless FILE, not Makefile/**
            ("grep -rn foo v2.5", "/fake/ccx code grep foo --glob 'v2.5/**'"),  # dotted DIR, not a file glob
        ],
    )
    def test_disk_classified_globs(self, tree: Path, command: str, expected: str) -> None:
        assert grep_rewrite(command) == expected

    @pytest.mark.parametrize(
        "command",
        [
            "grep -rn foo nonexistent/",  # absent path — no faithful glob → block
            "grep foo ghost.py",
            "grep -rn foo src/ ghost/",  # one real dir, one absent → block (never guess the absent one)
        ],
    )
    def test_nonexistent_path_blocks(self, tree: Path, command: str) -> None:
        assert grep_rewrite(command) is None


class TestGrepCwdThreading:
    """The `visit=` walk threads `evt.cwd` through statically resolvable `cd` occurrences, so a grep's
    path operands classify against the effective cwd — but only when it is trustworthy. A bare `(`
    (subshell) or an unresolvable `cd` ($VAR / bare / `-`) declines the trust, and every stat lane then
    fails closed rather than falling back to the process cwd.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")

    def test_cd_target_threads_cwd_for_classification(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # `src/` exists only under the cd target, never in the process cwd — the `src/**` glob proves the
        # walk classified against the threaded cd target, not the process cwd.
        (tmp_path / "target" / "src").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        target = tmp_path / "target"
        assert grep_verdict(f"cd {target} && grep -rn foo src/") == (
            f"cd {target} && /fake/ccx code grep foo --glob 'src/**'"
        )

    def test_subshell_cd_declines_cwd_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # The parser flattens the subshell, so a `(cd …)` cwd must not leak to a sibling — `src/` (present
        # in the process cwd) stays unclassified and the line blocks: no wrong rewrite.
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        assert isinstance(grep_verdict("(cd /tmp && grep foo .) && grep bar src/"), HookResult)

    @pytest.mark.parametrize("prefix", ["cd $VAR", "cd", "cd -"])
    def test_unresolvable_cd_declines_stat_lane(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, prefix: str
    ) -> None:
        # $VAR (dynamic), bare `cd` (HOME), and `cd -` (OLDPWD) resolve to None — the following grep's
        # stat lane fails closed rather than trusting the process cwd, so `src/` blocks despite existing.
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        assert isinstance(grep_verdict(f"{prefix} && grep -rn foo src/"), HookResult)

    def test_cd_into_uncreated_dir_stays_conservative(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # `cd X` targets a dir only `mkdir X` creates at runtime, invisible to PreToolUse, so `resolve_cd`
        # keeps the pre-cd cwd; `.` widens repo-wide and no glob is fabricated from the phantom X.
        monkeypatch.chdir(tmp_path)
        assert grep_verdict("mkdir X && cd X && grep -rn foo .") == (
            "mkdir X && cd X && /fake/ccx code grep foo"
        )

    def test_chained_cd_in_and_chain_keeps_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # Both cds sit in the grep's own unbroken `&&` chain — grep runs only if they did — so the
        # threaded cwd stays trusted and `deep/` classifies against target/sub.
        (tmp_path / "target" / "sub" / "deep").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        target = tmp_path / "target"
        assert grep_verdict(f"cd {target} && cd sub && grep -rn foo deep/") == (
            f"cd {target} && cd sub && /fake/ccx code grep foo --glob 'deep/**'"
        )

    def test_mkdir_cd_in_chain_keeps_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # A gated `cd` inside the grep's own `&&` chain no longer declines trust (the old blanket
        # over-block); `newdir` is uncreated, so `sub/` classifies against the kept pre-cd cwd.
        (tmp_path / "sub").mkdir()
        monkeypatch.chdir(tmp_path)
        assert grep_verdict("mkdir -p newdir && cd newdir && grep -rn foo sub/") == (
            "mkdir -p newdir && cd newdir && /fake/ccx code grep foo --glob 'sub/**'"
        )

    def test_conditional_cd_declines_cwd_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # `false && cd` short-circuits, so the grep runs in the ORIGINAL cwd — trust is declined
        # and the stat lane fails closed: the pre-threading block, never a wrong-tree rewrite.
        (tmp_path / "small" / "src").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        small = tmp_path / "small"
        assert isinstance(grep_verdict(f"false && cd {small}; grep -rn foo src/"), HookResult)

    def test_unconditional_semicolon_cd_keeps_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # A `;`-joined (or line-heading) `cd` always runs — only a cd ITSELF gated behind `&&`/`||` declines.
        (tmp_path / "target" / "src").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        target = tmp_path / "target"
        assert grep_verdict(f"cd {target}; grep -rn foo src/") == (
            f"cd {target}; /fake/ccx code grep foo --glob 'src/**'"
        )

    def test_or_reached_cd_declines_cwd_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # `cd A || cd B && grep` runs in A when `cd A` succeeds, in B otherwise — the threaded cwd (B)
        # is unknowable, so a `||`-reached cd declines trust even inside the grep's `&&` chain.
        (tmp_path / "b" / "src").mkdir(parents=True)
        (tmp_path / "a").mkdir()
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        a, b = tmp_path / "a", tmp_path / "b"
        assert isinstance(grep_verdict(f"cd {a} || cd {b} && grep -rn foo src/"), HookResult)


class TestGrepRepoWide:
    """Finding 4: repo-wide shapes emit NO dir --glob — a bare recursive grep or a `.` operand
    covers the whole repo. Exact equality proves the absence of a narrowing --glob; the inline
    `Rewrite(pattern=...)` checks are substrings a wrongly-globbed command would still satisfy.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -rn foo", "/fake/ccx code grep foo"),
            ("grep -rn foo . src/", "/fake/ccx code grep foo"),  # finding 3: `.` sibling → whole repo
            ("grep -rn foo src/ .", "/fake/ccx code grep foo"),  # `.` after a dir path, same widening
            ("grep -rn --include='*.go' foo .", "/fake/ccx code grep foo --glob '*.go'"),
            ("grep -rn --include='*.go' foo . src/", "/fake/ccx code grep foo --glob '*.go'"),  # finding 3 + include
        ],
    )
    def test_no_dir_glob(self, command: str, expected: str) -> None:
        assert grep_rewrite(command) == expected


class TestRegexRewritable:
    """`translate_pattern`'s dialect translation, seen through the public `grep_to` rewrite: a pattern
    carrying an active-dialect metachar is handed to the translator, and its Rust-regex form rides out
    on `--regex` in the rewritten command (`-E` selects ERE, the default is BRE). A pattern the
    translator refuses is unrewritable, so over `.` (repo-wide) the grep falls through to the block.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `.` widens to repo-wide, so these shapes are disk-independent apart from the `--regex` probe.
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    @pytest.mark.parametrize(
        "command, expected",
        [
            ("grep -E 'a{2,3}' .", "/fake/ccx code grep 'a{2,3}' --regex"),  # digits-only interval on an atom, emitted verbatim
            (r"grep 'a\{2,3\}' .", "/fake/ccx code grep 'a{2,3}' --regex"),  # BRE backslashed interval → bare Rust form
            ("grep '^class ' .", "/fake/ccx code grep '^class ' --regex"),  # leading `^` anchor + plain atoms
            ("grep 'foo$' .", "/fake/ccx code grep 'foo$' --regex"),  # trailing `$` anchor accepted
            ("grep -E '(a|b)c' .", "/fake/ccx code grep '(a|b)c' --regex"),  # balanced group + ERE alternation
            ("grep -E 'a+' .", "/fake/ccx code grep a+ --regex"),  # ERE `+` quantifier, unchanged
            ("grep 'a+.' .", "/fake/ccx code grep 'a\\+.' --regex"),  # bare BRE `+` is literal → escaped `\+` (routed by `.`)
        ],
    )
    def test_admits_dialect_faithful(self, command: str, expected: str) -> None:
        assert grep_rewrite(command) == expected

    @pytest.mark.parametrize(
        "command",
        [
            "grep -E '*abc' .",  # leading `*` — literal in grep, "nothing to repeat" in Rust
            "grep -E 'a{b}' .",  # non-digit interval — literal `{b}` in grep, a parse error in Rust
            "grep -E '{2,3}' .",  # leading interval — literal in GNU ERE, a parse error in Rust
            "grep 'a**' .",  # stacked quantifier rejected
            r"grep 'a\+\{2\}' .",  # BRE interval stacked on `\+` — GNU BRE rejects, Rust would accept
            "grep -E 'a+{2}' .",  # ERE interval stacked on `+` — divergent, rejected
            r"grep 'a\{32768\}' .",  # BRE interval past GNU's 32767 ceiling — GNU errors, Rust compiles
            "grep -E 'a{32768}' .",  # ERE interval past the ceiling — same divergence
            "grep 'a^b' .",  # mid-pattern `^` — literal in BRE, an anchor in Rust
            "grep 'a$b' .",  # mid `$` — literal in grep, an anchor in Rust
            "grep -E '(a|b' .",  # unbalanced group rejected
            r"grep -E 'a\d' .",  # backslash escape rejected
            "grep -E 'a[bc]' .",  # bracket class rejected
        ],
    )
    def test_rejects_dialect_divergent(self, command: str) -> None:
        # Unrewritable over `.` (a tree-wide dir, unbounded): no rewrite, so the condition fires (block).
        assert grep_rewrite(command) is None
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True


class TestGrepDialectClassification:
    """Finding 2: classification is per active dialect. `-E 'a+'` (ERE `+` quantifier) rewrites onto
    `--regex`; `'a+'` under the BRE default is a literal rewrite; `-F 'foo.*'` forces the literal
    path and, being un-ccx-literal-safe, never flips to a `--regex` rewrite (the silent-corruption bug).
    """

    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "f").write_text("x\n")
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def test_ere_plus_rewrites_regex(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite("grep -E 'a+' f") == "/fake/ccx code grep a+ --regex --glob f"

    def test_bre_plus_stays_literal(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, NO_SUPPORT_HELP)  # literal rewrite needs no --regex probe
        assert grep_rewrite("grep 'a+' f") == "/fake/ccx code grep a+ --glob f"

    def test_fixed_metachar_never_flips_to_regex(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # `grep -F 'foo.*' f`: -F forces literal, `foo.*` isn't ccx-literal-safe → unrewritable, but
        # `f` is a small existing file → the walk returns None (allow), never a `--regex` flip.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite("grep -F 'foo.*' f") is None
        assert grep_verdict("grep -F 'foo.*' f") is None


class TestGrepRegexRewrite:
    """A validator-cleared pattern rewrites to `ccx code grep --regex` when the local binary advertises
    `--regex`; on an older binary (SUPPORTS_HELP, no `--regex`) the same shape falls through. Dialect
    is load-bearing: `|` is BRE-literal (no rewrite) but ERE-meta (rewrites under `-E`); `-P` never maps.
    """

    @pytest.fixture(autouse=True)
    def pin_ccx(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        # `.` (the cwd) widens to repo-wide, so these shapes are disk-independent apart from the probe.
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

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
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite(command) == expected

    @pytest.mark.parametrize(
        "command, expected",
        [
            # BRE escapes translate to their ERE/Rust spelling; the alternation needs a quantifiable atom:
            ("grep 'a\\|b' .", "/fake/ccx code grep 'a|b' --regex"),  # BRE `\|` → `|`
            ("grep 'x\\(ab\\)\\+' .", "/fake/ccx code grep 'x(ab)+' --regex"),  # BRE group + `\+` → `(ab)+`
            # A bare BRE `+` is a literal — escaped so Rust never reads it as a quantifier (routed by the `.`):
            ("grep 'a.b+' .", "/fake/ccx code grep 'a.b\\+' --regex"),
            # …but with no metachar to route it, `a+b` stays a plain literal rewrite (no `--regex`):
            ("grep 'a+b' .", "/fake/ccx code grep a+b"),
        ],
    )
    def test_bre_translation_rewrites(self, monkeypatch: pytest.MonkeyPatch, command: str, expected: str) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite(command) == expected

    def test_incident_bre_alternation_over_dir_rewrites(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
    ) -> None:
        # The getaway incident (minus `-c`): a BRE-alternation grep over an existing dir rewrites to
        # `ccx code grep --regex`, the `\|` alternation translated to Rust's `|`.
        (tmp_path / "src").mkdir()
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        out = grep_rewrite("grep -e 'hybrid\\|Onward\\|Bridge' src/")
        assert out is not None
        assert "--regex" in out and "'hybrid|Onward|Bridge'" in out

    @pytest.mark.parametrize(
        "command",
        [
            "grep 'a|b' .",  # `|` is BRE-literal — classified literal, but not ccx-literal-safe → block
            "grep -P 'x(?=y)' .",  # PCRE lookahead — -P never maps
            "grep 'foo(bar' .",  # BRE-literal `(` — classified literal, but not ccx-literal-safe → block
            r"grep 'a\d' .",  # backslash — a dialect metachar the validator rejects (defense in depth)
            r"grep 'a\1b' .",  # backref `\1` — Rust regex has none → refused
            r"grep 'a\bc' .",  # `\b` word boundary — no faithful cross-dialect form → refused
            r"grep 'x\(ab' .",  # unbalanced BRE `\(` group → refused
            r"grep '\+ab' .",  # leading BRE `\+` quantifier with nothing to bind → refused
        ],
    )
    def test_unmappable_regex_blocks(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite(command) is None

    def test_ere_alternation_stays_bre_literal_without_dash_e(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # `a|b` under the BRE default does NOT rewrite; the same pattern under `-E` does.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite("grep 'a|b' .") is None
        assert grep_rewrite("grep -E 'a|b' .") == "/fake/ccx code grep 'a|b' --regex"

    def test_probe_fail_over_existing_file_allows(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary (no `--regex`): a regex grep over an explicit existing file is bounded and
        # unrewritable → the condition never fires (genuine allow), never a block.
        (tmp_path / "real.py").write_text("x\n")
        probe(monkeypatch, SUPPORTS_HELP)
        assert grep_rewrite("grep 'foo.*' real.py") is None
        assert grep_verdict("grep 'foo.*' real.py") is None

    def test_probe_fail_tree_wide_blocks(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary + `.` (a dir, not a bounded file): unrewritable and unbounded → the condition fires
        # and `grep_to` is None → block.
        probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' .")
        assert grep_rewrite("grep 'foo.*' .") is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep 'foo.*' ."), cl) is True

    def test_regex_note_discloses_rg_engine(self) -> None:
        # The note for a regex rewrite names the engine; the dot-literal disclosure does not apply.
        note = grep_rewrite_note("grep 'foo.*' .")
        assert "regex on the rg engine" in note and "any-char" not in note


class TestGrepMultiFilePaths:
    """Two or more explicit existing files carry as `ccx code grep` positionals (multi-file form,
    ccx ≥ v0.11.0), gated on the same `--regex` probe — old binaries hard-error on extra operands.
    """

    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "a.py").write_text("x\n")
        (tmp_path / "b.py").write_text("y\n")
        (tmp_path / "safe").write_text("z\n")
        (tmp_path / "--regex").write_text("w\n")  # a real file whose name looks like a ccx flag
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def test_multi_file_carries_operands(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Finding 3: operands land after a literal `--` so cobra reads them as file positionals.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_rewrite("grep foo a.py b.py") == "/fake/ccx code grep foo -- a.py b.py"

    def test_flag_like_operand_stays_behind_separator(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Finding 3 repro: `grep 'a+' -- safe --regex` — grep's own `--` marks `--regex` a filename.
        # The emitted command must keep it a positional (behind ccx's `--`), never let it re-parse as
        # the ccx `--regex` flag and flip the literal `a+` search into a regex one.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert (
            grep_rewrite("grep 'a+' -- safe --regex")
            == "/fake/ccx code grep a+ -- safe --regex"
        )

    def test_single_file_keeps_glob_form(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # One explicit file stays on the old `--glob <file>` form (no `--regex` probe needed).
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert grep_rewrite("grep foo a.py") == "/fake/ccx code grep foo --glob a.py"

    def test_multi_file_probe_fail_allows(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary lacking `--regex`/multi-file: unrewritable, but both operands are bounded existing
        # files → the condition never fires (genuine allow).
        probe(monkeypatch, SUPPORTS_HELP)
        assert grep_rewrite("grep foo a.py b.py") is None
        assert grep_verdict("grep foo a.py b.py") is None


class TestGrepOccurrenceRewrite:
    def test_compound_splices_only_grep(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        evt = make_evt("echo x; grep -r foo .")
        cl = evt.cmd.line
        grep_occ = cl.occurrences[1]
        replacement = grep_guards.grep_to(evt, grep_occ)
        assert replacement == "/fake/ccx code grep foo"
        assert cl.splice({grep_occ.index: replacement}) == "echo x; /fake/ccx code grep foo"

    def test_one_flooding_grep_blocks_benign_siblings(self) -> None:
        # The `.` tree grep fires a block that aborts the whole line despite the benign `echo x` sibling.
        assert isinstance(grep_verdict("echo x; grep -c foo ."), HookResult)

    def test_wrapped_grep_matches_but_never_rewrites(self) -> None:
        evt, occ = event_occurrence("sudo grep foo .")
        assert occ.command.unwrapped.executable == "grep"
        assert grep_guards.GrepFlood().check_command_line(evt, evt.cmd.line) is True
        assert grep_guards.grep_to(evt, occ) is None
        assert isinstance(grep_verdict("sudo grep foo ."), HookResult)

    def test_spanless_grep_rechecks_and_blocks(self) -> None:
        evt, occ = event_occurrence("grep foo > out .")
        assert occ.command.span is None
        assert isinstance(grep_verdict("grep foo > out ."), HookResult)


class TestGrepBoundedPassthrough:
    """`GrepFlood` stays silent (a genuine allow) only when every grep occurrence is a bounded search
    ccx can't rewrite, judged per occurrence on its own flags and operands in a fixed order of forfeits:
    a `GREP_OPTIONS` env, `grep -r` with no operand, env alongside path operands, an uninspectable `-f`
    pattern file, a flag-supplied or positional empty pattern, and (on the stat lane) `-o`. Three shapes
    stay bounded: every operand an explicit data-ext file (matched by suffix, no stat, `-o` allowed — rg
    parity); a count/quiet/list-only grep over literal (glob-free) non-directory operands at any size or
    existence; or every
    operand an existing regular file whose sizes sum under the large-read threshold. A pipe-sink grep
    with no operand is a bounded stdin filter; with file operands it is judged like any file search.
    Compound lines are judged per grep occurrence — one unbounded grep fires the whole line.
    """

    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "real.py").write_text("x\n")
        (tmp_path / "big.txt").write_text("x" * (common.LARGE_READ_BYTES + 1))  # over threshold, data ext
        (tmp_path / "big.py").write_text("x" * (common.LARGE_READ_BYTES + 1))  # over threshold, source ext
        (tmp_path / "half_a.py").write_text("x" * (common.LARGE_READ_BYTES // 2 + 1))  # each under cap,
        (tmp_path / "half_b.py").write_text("x" * (common.LARGE_READ_BYTES // 2 + 1))  # together over it
        (tmp_path / "sub").mkdir()
        (tmp_path / "sub" / "real.py").write_text("y\n")  # a descendant reachable only via -r
        (tmp_path / "dir.json").mkdir()  # a real directory named like a data file
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo real.py",  # count mode — unmappable, but a bounded existing file under threshold
            "grep -q foo real.py",  # exit-code — unmappable
            "grep --binary-files=text foo real.py",  # value-taking long consumes its =value
            "grep -m 5 foo real.py",  # value-taking short `-m` consumes its `5`
            "grep -m1 foo real.py",  # numeric max-count is bounded but keeps the size cap
            "grep --max-count=1 foo real.py",  # long-form max-count follows the same lane
        ],
    )
    def test_bounded_existing_files_do_not_fire(self, command: str) -> None:
        assert grep_verdict(command) is None

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo big.py",  # count mode skips the size cap on an existing file (the incident fix)
            "grep -L foo big.py",  # files-without-match — one line per operand
            "grep -q foo big.py",  # quiet / exit-code contract
            "grep -ci foo big.py",  # bundled count + ignore-case
            "grep --count foo big.py",  # long-form count
        ],
    )
    def test_output_bounded_skips_size_cap(self, command: str) -> None:
        # -c/-q/-l/-L output is one line per operand, not per match, so an over-cap existing file
        # (big.py > LARGE_READ_BYTES) stays bounded — the walk returns None and the grep runs as-is.
        assert grep_verdict(command) is None

    def test_long_files_with_matches_rewrites(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Deliberately moved out of the bounded-output lane: the long `-l` spelling is now a DROP flag.
        probe(monkeypatch, FILES_WITH_MATCHES_HELP)
        command = "grep --files-with-matches foo big.py"
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True
        assert grep_rewrite(command) == "/fake/ccx code grep foo -l --glob big.py"

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo ghost.py",  # missing file → one grep error line, not a flood
            "grep -c foo real.py ghost.py",  # mixed existing/missing — still one line per operand
            "grep -q foo ghost.py",  # quiet over a missing file
            "curl -so page.html https://x.test/ && grep -c m page.html",  # created later in the same compound
        ],
    )
    def test_output_bounded_skips_existence(self, command: str) -> None:
        # PreToolUse fires before the compound's earlier commands run, so a bounded-output grep must
        # pass on operands that don't exist yet; directory-shaped operands still fire (stays_narrow).
        assert grep_verdict(command) is None

    @pytest.mark.parametrize(
        "command",
        [
            "grep -o foo big.py",  # -o forfeits the stat lane — the output-bounded skip is never reached
            "grep -oc foo big.py",  # the -o forfeit fires before the -c output-bounded skip
            "grep -c foo sub/",  # count over a directory is still tree-wide
            "grep -c foo *.py",  # glob operand — expansion can multiply operands past any bound
            "grep -c foo f{1..100}.py",  # brace operand — same multiplication
        ],
    )
    def test_output_bounded_skip_stays_narrow(self, command: str) -> None:
        # The skip is only for -c/-q/-l/-L over literal non-directory operands; -o, directories, and
        # glob/brace operands still block (the condition fires and `grep_to` yields no rewrite).
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True
        assert grep_rewrite(command) is None

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo sub/",  # directory operand → not bounded
            "grep -c foo",  # no operand at all → not bounded (tree-wide)
            "grep -o . big.py",  # -o forfeits the stat lane (big.py is over-threshold anyway) → block
            "grep -o foo real.py",  # -o forfeits the stat lane even under threshold (per-match prefixes)
            "grep -on foo real.py",  # -o bundled with -n still forfeits the stat lane
            "grep -oHnb . real.py",  # R1: -o + -H/-n/-b prefixes multiply output far past the size bound
            "grep -v foo real.py",  # -v (invert-match) isn't a bounded flag → unbounded when unpiped
            "grep -x foo big.py",  # -x is bounded but not output-bounded → the size cap still blocks over-cap
            "grep -m1 foo big.py",  # max-count limits lines, not their width, so the size cap remains
            "grep -m nope foo real.py",  # max-count joins the lane only with a numeric value
            "grep -m1 -r foo sub/",  # max-count does not bound a recursive directory search
            "grep '' real.py",  # empty positional pattern floods every line → block
        ],
    )
    def test_unbounded_grep_fires_and_blocks(self, command: str) -> None:
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True
        assert grep_rewrite(command) is None

    @pytest.mark.parametrize(
        "command",
        [
            "grep -r foo logs.json",  # -r forfeits the data-ext textual escape → unbounded → fires
            "grep -d recurse foo logs.json",  # -d (--directories) → assume recursive → same
            "grep foo logs.json/",  # a trailing slash defeats the data-ext textual escape
        ],
    )
    def test_recursion_or_directory_defeats_data_ext(self, command: str) -> None:
        # Recursion or a trailing slash forfeits the no-stat data-ext pass → unbounded → fires. The absent
        # `.json` name keeps `grep_to` at `None`, so no dir-glob rewrite masks the fire.
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True

    def test_data_ext_is_size_exempt(self) -> None:
        # Data-ext passes by suffix with no stat, so over-threshold `.txt` stays bounded; `-x` (bounded,
        # not output-bounded) would hit the stat lane and block absent that pass (contrast `-x … big.py`).
        assert grep_verdict("grep -x foo big.txt") is None

    def test_multi_file_sum_gates_the_stat_lane(self) -> None:
        # `-x` is bounded but NOT output-bounded, so it reaches the size-sum branch: two half-cap files
        # are bounded apart yet fire together once their sizes sum past the cap.
        assert grep_verdict("grep -x foo half_a.py") is None
        assert isinstance(grep_verdict("grep -x foo half_a.py half_b.py"), HookResult)

    def test_recursive_count_does_not_ride_the_output_bounded_skip(self) -> None:
        # -c/-q/-l/-L is one line per operand only WITHOUT recursion; under -r/-R a count fans out one
        # line per file in the tree, so an over-cap recursive count falls back to the size cap and blocks.
        assert isinstance(grep_verdict("grep -rc foo big.py"), HookResult)
        # A small recursive count/quiet grep still passes the size cap — recursion alone isn't a flood on
        # a small file (these stay count-mode so `grep_to` yields no rewrite that would mask the verdict).
        for ok in ("grep -rc foo real.py", "grep -rq foo real.py"):
            assert grep_verdict(ok) is None

    def test_unknown_flag_is_not_bounded(self) -> None:
        # Conservative lexer: an unknown flag leaves the grep unbounded (it fires), never a wrong allow —
        # even over an existing file.
        command = "grep --frobnicate foo real.py"
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True

    @pytest.mark.parametrize(
        "command",
        [
            # The incident: two `grep -oiE … b_jetblue_jun.json | …` statements in a `;`/`|` chain over a
            # file the redirect created earlier — data-ext, so each grep passes with no stat.
            "cd /tmp/scratch && gog --account user@example.com --readonly --json gmail get MSGID "
            "--json > b_jetblue_jun.json; grep -oiE '[0-9][0-9,]{2,} ?(points|TrueBlue)' b_jetblue_jun.json "
            "| head; grep -oiE 'mosaic( [0-9])?' b_jetblue_jun.json | sort | uniq -c",
            "grep -c foo real.py && grep -c bar real.py",  # both bounded existing files
            "grep -c foo real.py | wc -l",  # a bounded grep feeding a pipe
        ],
    )
    def test_compound_per_occurrence_allows(self, command: str) -> None:
        assert grep_verdict(command) is None

    def test_one_unbounded_grep_blocks_the_line(self) -> None:
        # A qualifying grep can't launder a sibling tree-wide grep: the `.` search fires the whole line.
        command = "grep -c foo real.py; grep foo ."
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True

    @pytest.mark.parametrize(
        "command",
        [
            "grep -oiE 'a{2,}' missing.json",  # -oiE bundle over a nonexistent data file
            "grep -i foo missing.json",  # nonexistent data file — proves the data-ext pass never stats
        ],
    )
    def test_data_ext_needs_no_stat(self, command: str) -> None:
        assert grep_verdict(command) is None

    @pytest.mark.parametrize(
        "command",
        [
            "GREP_OPTIONS=-r grep -o needle dir.json",  # R2: GREP_OPTIONS injects -r the parser never sees
            "LC_ALL=C grep -i pat notes.json",  # any env alongside path operands → conservative block
            "grep --regexp= data.json",  # R4: empty flag-supplied pattern floods every line
            "grep -e '' data.json",  # R4: empty -e pattern floods every line
            "grep -f pats.txt data.json",  # R4: an uninspectable -f pattern file
        ],
    )
    def test_env_and_flag_pattern_holes(self, command: str) -> None:
        # These forfeit before any stat → unbounded → fires; disk-independent (the named files need not exist).
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True

    @pytest.mark.parametrize(
        ("command", "fires"),
        [
            # R3: a sink grep with file operands ignores stdin and searches those files.
            ("grep -q localhost /etc/hosts | grep -r . /", True),
            # step 4: grep -r with no operand recurses the cwd even as a pipe sink.
            ("grep -c foo real.py; printf x | grep -r needle", True),
            # step 5 does not launder a sink grep that names an over-cap file operand (no output-bounded
            # flag, so the size cap still applies — `-c` here would be bounded regardless of size).
            ("grep -c foo real.py; printf y | grep . big.py", True),
            ("grep -c foo real.py; printf x | grep -Zr pat /", True),
            # a genuine pipe-sink filter (no operand, env allowed) stays a bounded stdin filter.
            ("grep -c foo real.py; printf x | LC_ALL=C grep pat", False),
        ],
    )
    def test_sink_grep_semantics(self, command: str, fires: bool) -> None:
        # Every unpiped grep uses an unmappable output flag, so `grep_to` returns before a live ccx probe.
        assert isinstance(grep_verdict(command), HookResult) is fires


class TestGrepDownstreamPathScreen:
    """The downstream-bounded verbatim allow keeps the policy screens: a git-ignored operand
    (`node_modules/`) declines the lane and blocks with its steer, while an ordinary directory
    under the same sink still runs verbatim. `path_blocked` shells `git check-ignore`, so a real
    repo pins the ignore verdict.
    """

    @pytest.fixture(autouse=True)
    def repo(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
        (tmp_path / ".gitignore").write_text("node_modules/\n")
        (tmp_path / "node_modules").mkdir()
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)

    def test_gitignored_operand_declines_the_sink(self) -> None:
        assert isinstance(grep_verdict("grep -r foo node_modules/ | head"), HookResult)

    def test_plain_dir_operand_keeps_the_sink(self) -> None:
        assert grep_verdict("grep -r foo src/ | head") is None


class TestTranscriptBlockMessage:
    """`search_block` tunes the block per the line's transcript operands: all-transcript → the
    cc-transcript steer alone; a transcript operand mixed with an ordinary flood → the default steer
    PLUS one appended cc-transcript line (never transcript-only).
    """

    def bash_pre(self, command: str) -> PreToolUseEvent:
        ctx = HookContext(session=SessionStore(None), transcript=None, settings=None)
        return PreToolUseEvent(_raw={"tool_name": "Bash", "tool_input": {"command": command}}, ctx=ctx)

    def test_mixed_line_carries_both_steers(self) -> None:
        # A transcript operand alongside a `.` tree flood → the block names BOTH the default steer and cc-transcript.
        evt = self.bash_pre("grep foo ~/.claude/projects/main.jsonl; grep bar .")
        message = grep_guards.grep_block(evt, evt.cmd.line)
        assert "floods context" in message and "cc-transcript" in message
        assert message != search_common.TRANSCRIPT_STEER  # not transcript-only

    def test_all_transcript_line_is_steer_only(self) -> None:
        evt = self.bash_pre("grep -r foo ~/.claude/projects/")
        assert grep_guards.grep_block(evt, evt.cmd.line) == search_common.TRANSCRIPT_STEER

    def test_wrapped_transcript_occurrence_contributes_to_mixed_message(self) -> None:
        evt = self.bash_pre("sudo grep foo ~/.claude/projects/main.jsonl; grep bar .")
        message = grep_guards.grep_block(evt, evt.cmd.line)
        assert "floods context" in message and "cc-transcript" in message


class TestBareParen:
    """Fix 2: `bare_paren` distrusts a cwd only for a `(` OUTSIDE all quoting — a real subshell or
    group. A paren living inside a quoted pattern (`grep -E '^(a|b)' f`) is not bare, so the line
    keeps its cwd trust; `$(…)` and a backslash-escaped `\\(` are classified by the same rule.
    """

    @pytest.mark.parametrize(
        "raw, expected",
        [
            ("grep -E '^(go|toolchain) ' go.mod", False),  # paren only inside a quoted pattern
            ("(cd x && grep a .)", True),  # a real subshell paren
            ('grep "a(b" f && (make)', True),  # quoted `(` ignored; the `&& (make)` paren is bare
            ("grep 'a(b' f", False),  # single-quoted paren
            (r"grep \( f", False),  # backslash-escaped `(` outside quotes
            ("grep foo .", False),  # no paren at all
            ("grep foo $(printf x)", True),  # `$(…)` substitution paren stays bare → still distrusted
            # ANSI-C `$'…'`: `\'` does NOT close, so parity must not desync and swallow a later bare `(`.
            (r"echo $'a\'b'; (cd sneaky && true); grep secret probe.py", True),  # the refuter's repro
            (r"echo $'a\'b'", False),  # ANSI-C token alone, no bare paren
            (r"grep $'\t(' f.txt", False),  # `(` inside ANSI-C is quoted
            (r"echo $'a\\' && (x)", True),  # escaped backslash then close → the `&& (x)` paren is bare
            ("grep \"a$'b\" f && (x)", True),  # `$'` inside double quotes never arms ANSI-C
        ],
    )
    def test_bare_paren(self, raw: str, expected: bool) -> None:
        assert search_common.bare_paren(raw) is expected


class TestGrepAnsiCQuotingCwdTrust:
    """End to end: an ANSI-C `$'…'` token must not desync `bare_paren` and swallow a later genuinely-bare
    `(cd sub && true)` subshell — the flattened cwd stays distrusted, so the grep blocks exactly as HEAD
    does rather than rewriting `probe.py` against the wrong directory.
    """

    def test_ansi_c_token_before_bare_subshell_blocks(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / "probe.py").write_text("secret\n")  # exists → a wrongly-trusted cwd would rewrite it
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        verdict = grep_verdict(r"echo $'a\'b'; (cd sneaky && true); grep secret probe.py")
        assert isinstance(verdict, HookResult)


class TestGrepQuotedParenCwdTrust:
    """Fix 2, end to end: a paren confined to the quoted pattern regains cwd trust, so an existing
    in-cap `go.mod` stats as a bounded file and runs verbatim instead of blocking (the regression).
    """

    def test_quoted_paren_line_allows_bounded_file(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / "go.mod").write_text("module x\n")
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        # An old binary (no --regex) declines the rewrite, so the bounded-file lane is what runs.
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert grep_verdict("grep -E '^(go|toolchain) ' go.mod") is None


class TestGrepBundleMapAndRelativize:
    """Fix 3: a MAP short (`-i`/`-w`) glued into a DROP bundle (`-ri`) maps instead of blocking.
    Fix 4: an in-repo absolute directory operand relativizes before classification. Both need the
    `--ignore-case` probe and a real tmp tree, so they live in pytest with exact-equality assertions.
    """

    @pytest.fixture(autouse=True)
    def tree(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
        (tmp_path / "bench").mkdir()
        monkeypatch.setattr(search_common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.setattr(common, "ccx_bin", lambda: "/fake/ccx")
        monkeypatch.chdir(tmp_path)
        ccx_supports.cache_clear()
        yield
        ccx_supports.cache_clear()

    def test_ri_bundle_maps_and_abs_dir_relativizes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A `-ri` bundle maps `-i`, an absolute in-repo dir relativizes to `bench/**`, `-l` drops.
        probe(monkeypatch, SUPPORTS_HELP)
        cwd = Path.cwd()
        assert grep_rewrite(f"grep -n foo -ri {cwd}/bench/ -l") == "/fake/ccx code grep foo -i --glob 'bench/**'"

    def test_ri_bundle_relative_dir_maps_ignore_case(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, SUPPORTS_HELP)
        assert grep_rewrite("grep -n 'semble' -ri bench/ -l") == "/fake/ccx code grep semble -i --glob 'bench/**'"


class TestBlockReason:
    """Fix 5: an advisory clause naming the first disqualifier is appended to the block steer.
    Diagnostic only — a shape matching no cheap, certain check leaves the message byte-identical.
    """

    def first_grep(self, command: str, cwd: str | Path | None = None) -> tuple[PreToolUseEvent, Occurrence]:
        evt = make_evt(command, cwd=cwd) if cwd is not None else make_evt(command)
        occ = next(o for o in evt.cmd.line.occurrences if o.command.unwrapped.executable == "grep")
        return evt, occ

    def test_subshell_names_untrusted_cwd(self) -> None:
        _evt, occ = self.first_grep("(cd /tmp && grep foo .) && grep bar src/")
        assert "cwd untrusted" in search_common.block_reason(occ, cwd=None)

    def test_quoted_paren_line_names_the_file_not_cwd(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # The paren is quoted, so the reason is the missing source file — never the cwd-untrusted clause.
        monkeypatch.chdir(tmp_path)
        _evt, occ = self.first_grep("grep -E '^(a|b)' ghost.py", cwd=tmp_path)
        reason = search_common.block_reason(occ, cwd=tmp_path)
        assert reason is not None and "cwd untrusted" not in reason and "ghost.py" in reason

    def test_out_of_repo_abs_operand(self) -> None:
        _evt, occ = self.first_grep("grep -rn foo /etc/")
        assert search_common.block_reason(occ, cwd=None) == "out-of-repo operand `/etc/`"

    def test_pcre_flag_appends_to_message(self) -> None:
        evt, occ = self.first_grep("grep -P 'x(?=y)' .")
        reason = search_common.block_reason(occ, cwd=None)
        assert reason == "PCRE (-P) never maps"
        assert grep_guards.grep_block(evt, evt.cmd.line, reason=reason).endswith("This block: PCRE (-P) never maps.")

    def test_glob_operand(self) -> None:
        _evt, occ = self.first_grep("grep -rn foo '*.py'")
        assert "glob operand" in search_common.block_reason(occ, cwd=None)

    def test_dash_leading_operand_after_double_dash_is_not_a_flag(self) -> None:
        # The `-Pxyz` after `--` is a filename, not a flag: the reason names the real blocker (`-m5`),
        # never "PCRE" — flags are scanned only before the first bare `--`.
        _evt, occ = self.first_grep("grep -m5 foo -- -Pxyz")
        reason = search_common.block_reason(occ, cwd=None)
        assert "PCRE" not in reason and "-m5" in reason

    def test_leading_value_short_in_bundle_names_the_bundle(self) -> None:
        # `-dr` glues the value-taking `-d` at the bundle head — named the same as the reordered `-rd`.
        _evt, occ = self.first_grep("grep -dr foo src/")
        assert search_common.block_reason(occ, cwd=None) == "`-dr` glues a value-taking short into a bundle"

    def test_head_context_short_is_never_blamed(self) -> None:
        # `-C3` maps at the head, so the value-short blame skips it — the `-P` on the line is the reason.
        _evt, occ = self.first_grep("grep -C3 -P x .")
        reason = search_common.block_reason(occ, cwd=None)
        assert reason == "PCRE (-P) never maps" and "-C3" not in reason

    def test_interior_context_short_is_blamed(self) -> None:
        # `-rnC3` glues the value-taking `-C` INTERIOR, where no parse maps it → named.
        _evt, occ = self.first_grep("grep -rnC3 foo src/")
        assert search_common.block_reason(occ, cwd=None) == "`-rnC3` glues a value-taking short into a bundle"

    def test_no_match_leaves_message_byte_identical(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / "src").mkdir()
        monkeypatch.chdir(tmp_path)
        evt, occ = self.first_grep("grep -r foo src/ | cat", cwd=tmp_path)
        reason = search_common.block_reason(occ, cwd=tmp_path)
        assert reason is None
        assert "This block:" not in grep_guards.grep_block(evt, evt.cmd.line, reason=reason)
