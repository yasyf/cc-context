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
"""

from __future__ import annotations

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
    return grep_guards.grep_to(occ, cwd=evt.cwd)


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
        # A grep with no -i/-w rewrites unconditionally — no `--help` probe, and no `git check-ignore`
        # now that the path-block screen is gone. Any subprocess from here is the failure.
        def no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            raise AssertionError("a grep without -i/-w must shell no subprocess")

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

    @pytest.mark.parametrize("command", ["grep -rn foo nonexistent/", "grep foo ghost.py"])
    def test_missing_lone_path_runs_raw(self, tree: Path, command: str) -> None:
        # A missing path is not a directory → not tree-shaped → runs raw under fail-open.
        assert grep_verdict(command) is None

    def test_real_dir_with_missing_sibling_blocks(self, tree: Path) -> None:
        # A real dir makes it tree-shaped, but the mixed dir+missing target has no single glob → block.
        assert isinstance(grep_verdict("grep -rn foo src/ ghost/"), HookResult)


class TestGrepCwdThreading:
    """The `visit=` walk threads `evt.cwd` through statically resolvable `cd` occurrences, so a grep's
    path operands classify against the effective cwd. Under the fail-open doctrine the threaded cwd is
    trusted as-is — no subshell or conditional-cd decline — so a wrong cwd at worst mis-stats one
    `is_dir` probe and fails open.
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
        # Both cds thread into the effective cwd, so `deep/` classifies against target/sub.
        (tmp_path / "target" / "sub" / "deep").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        target = tmp_path / "target"
        assert grep_verdict(f"cd {target} && cd sub && grep -rn foo deep/") == (
            f"cd {target} && cd sub && /fake/ccx code grep foo --glob 'deep/**'"
        )

    def test_mkdir_cd_in_chain_keeps_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # `newdir` is uncreated, so `resolve_cd` keeps the pre-cd cwd and `sub/` classifies against it.
        (tmp_path / "sub").mkdir()
        monkeypatch.chdir(tmp_path)
        assert grep_verdict("mkdir -p newdir && cd newdir && grep -rn foo sub/") == (
            "mkdir -p newdir && cd newdir && /fake/ccx code grep foo --glob 'sub/**'"
        )

    def test_unconditional_semicolon_cd_keeps_trust(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # A `;`-joined `cd` threads into the effective cwd exactly like an `&&`-joined one.
        (tmp_path / "target" / "src").mkdir(parents=True)
        (tmp_path / "elsewhere").mkdir()
        monkeypatch.chdir(tmp_path / "elsewhere")
        target = tmp_path / "target"
        assert grep_verdict(f"cd {target}; grep -rn foo src/") == (
            f"cd {target}; /fake/ccx code grep foo --glob 'src/**'"
        )


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

    def test_probe_fail_tree_wide_allows_infra(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Fix 4: old binary (no `--regex`) over `.` — the regex shape is mappable but the required probe
        # fails, so infra unavailability runs raw (allow), never a block. The emitter still declines and
        # the flood condition still matches; the allow lands at the visitor.
        probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' .")
        assert grep_rewrite("grep 'foo.*' .") is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep 'foo.*' ."), cl) is True
        assert grep_verdict("grep 'foo.*' .") is None

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
        replacement = grep_guards.grep_to(grep_occ)
        assert replacement == "/fake/ccx code grep foo"
        assert cl.splice({grep_occ.index: replacement}) == "echo x; /fake/ccx code grep foo"

    def test_one_flooding_grep_blocks_benign_siblings(self) -> None:
        # The `.` tree grep fires a block that aborts the whole line despite the benign `echo x` sibling.
        assert isinstance(grep_verdict("echo x; grep -c foo ."), HookResult)

    def test_wrapped_grep_matches_but_never_rewrites(self) -> None:
        evt, occ = event_occurrence("sudo grep foo .")
        assert occ.command.unwrapped.executable == "grep"
        assert grep_guards.GrepFlood().check_command_line(evt, evt.cmd.line) is True
        assert grep_guards.grep_to(occ) is None
        assert isinstance(grep_verdict("sudo grep foo ."), HookResult)

    def test_spanless_grep_rechecks_and_blocks(self) -> None:
        _evt, occ = event_occurrence("grep foo > out .")
        assert occ.command.span is None
        assert isinstance(grep_verdict("grep foo > out ."), HookResult)


class TestInfraNoneAllows:
    """Fix 4: infra unavailability (no ``ccx`` on disk, or a required probe fails) runs the tree-shaped
    search raw — it never converts a failed rewrite into a block. Only a genuinely unmappable shape
    (``-P``, an exotic regex) blocks, and that block still fires with ``ccx`` absent.
    """

    @pytest.fixture(autouse=True)
    def cwd(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(search_common, "ccx_bin", lambda: None)
        monkeypatch.setattr(common, "ccx_bin", lambda: None)

    def test_ccx_bin_none_allows_tree_grep(self) -> None:
        assert grep_verdict("grep needle .") is None

    def test_ccx_bin_none_still_blocks_unmappable_shape(self) -> None:
        assert isinstance(grep_verdict("grep -P 'x(?=y)' ."), HookResult)


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


class TestGrepBundleMap:
    """A MAP short (`-i`/`-w`) glued into a DROP bundle (`-ri`) maps instead of blocking. Needs the
    `--ignore-case` probe and a real tmp tree, so it lives in pytest with exact-equality assertions.
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

    def test_ri_bundle_relative_dir_maps_ignore_case(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, SUPPORTS_HELP)
        assert grep_rewrite("grep -n 'semble' -ri bench/ -l") == "/fake/ccx code grep semble -i --glob 'bench/**'"
