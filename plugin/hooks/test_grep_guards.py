"""Tests for the version-gated grep rewrites in ``grep_guards``.

Run from the repo root against the captain-hook source env, with ``plugin/`` on the
path so the ``hooks`` package (and its relative imports) resolves::

    PYTHONPATH=plugin uv run --project ../captain-hook --with pytest \
        pytest plugin/hooks/test_grep_guards.py

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

from pathlib import Path
from types import SimpleNamespace

import pytest
from captain_hook import CommandLine
from captain_hook.context import HookContext
from captain_hook.events import PreToolUseEvent
from captain_hook.session import SessionStore

from conftest import NO_SUPPORT_HELP, REGEX_SUPPORTS_HELP, SUPPORTS_HELP, fake_run, make_evt, probe
from hooks import common, grep_guards, rg_guards, search_common
from hooks.common import ccx_supports


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
        assert grep_guards.grep_to(make_evt(command)) == expected

    @pytest.mark.parametrize("command", ["grep -i foo src/", "grep -w foo src/", "grep -i -w foo src/"])
    def test_blocks_when_flag_absent(self, monkeypatch: pytest.MonkeyPatch, command: str) -> None:
        # `--help` returns 0 but without `--ignore-case` (an older binary) → fall back to block.
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert grep_guards.grep_to(make_evt(command)) is None

    def test_blocks_when_probe_errors(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setattr(common.subprocess, "run", fake_run(1, stderr='unknown flag "--ignore-case"'))
        assert grep_guards.grep_to(make_evt("grep -i foo src/")) is None

    def test_ungated_shape_never_probes(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # A grep with no -i/-w must not shell the `--help` probe — it rewrites unconditionally. The
        # path-classification `git check-ignore` call is expected (and answered "not ignored"); only
        # a `--help` probe shelled from here is the failure.
        def no_probe(cmd: list[str], *_args: object, **_kwargs: object) -> SimpleNamespace:
            if cmd[:2] == ["git", "check-ignore"]:
                return SimpleNamespace(returncode=1, stdout="", stderr="")
            raise AssertionError("ccx_supports must not probe for a grep without -i/-w")

        monkeypatch.setattr(common.subprocess, "run", no_probe)
        assert grep_guards.grep_to(make_evt("grep -rn foo src/")) == "/fake/ccx code grep foo --glob 'src/**'"


class TestGrepNote:
    # Repo-wide shapes (no path) so the note is disk-independent: `grep_note` runs `grep_parse`,
    # which now classifies path operands against the filesystem.
    def test_discloses_l_fixed_and_expand_drops(self) -> None:
        note = grep_guards.grep_note(make_evt("grep -rlF -C 3 foo"))
        assert "`-l`" in note and "`-F`" in note and "--expand=3" in note

    def test_context_flag_discloses_count_drop(self) -> None:
        # Finding 6: the user's `-C N` count is dropped, and `--expand=3` is full-source, not context lines.
        note = grep_guards.grep_note(make_evt("grep -rn -C 3 foo"))
        assert "count was dropped" in note and "--expand=3" in note

    def test_dot_pattern_regex_rewrites_not_literal(self) -> None:
        # `.` is a dialect metachar, so grep now rewrites it faithfully as a regex — the note names
        # the engine, not the old any-char-literal disclosure. rg still literal-rewrites `.` (its
        # default engine reads `.` as a wildcard the literal search can't honor), so the
        # `.`-literal disclosure stays live there.
        grep_note = grep_guards.grep_note(make_evt("grep -rn foo.bar"))
        assert "regex on the rg engine" in grep_note and "any-char" not in grep_note
        assert "any-char" in rg_guards.rg_note(make_evt("rg foo.bar"))

    def test_no_dot_carries_no_dot_disclosure(self) -> None:
        note = grep_guards.grep_note(make_evt("grep -rn foobar"))
        assert "any-char" not in note

    def test_plain_rewrite_carries_no_disclosures(self) -> None:
        note = grep_guards.grep_note(make_evt("grep -rn foobar"))
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
            ("grep -rn -C 3 foo src/", "/fake/ccx code grep foo --glob 'src/**' --expand=3"),
            ("grep foo Makefile", "/fake/ccx code grep foo --glob Makefile"),  # extensionless FILE, not Makefile/**
            ("grep -rn foo v2.5", "/fake/ccx code grep foo --glob 'v2.5/**'"),  # dotted DIR, not a file glob
        ],
    )
    def test_disk_classified_globs(self, tree: Path, command: str, expected: str) -> None:
        assert grep_guards.grep_to(make_evt(command)) == expected

    @pytest.mark.parametrize(
        "command",
        [
            "grep -rn foo nonexistent/",  # absent path — no faithful glob → block
            "grep foo ghost.py",
            "grep -rn foo src/ ghost/",  # one real dir, one absent → block (never guess the absent one)
        ],
    )
    def test_nonexistent_path_blocks(self, tree: Path, command: str) -> None:
        assert grep_guards.grep_to(make_evt(command)) is None


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
            ("grep -rl foo .", "/fake/ccx code grep foo"),
            ("grep -rn foo . src/", "/fake/ccx code grep foo"),  # finding 3: `.` sibling → whole repo
            ("grep -rn foo src/ .", "/fake/ccx code grep foo"),  # `.` after a dir path, same widening
            ("grep -rn --include='*.go' foo .", "/fake/ccx code grep foo --glob '*.go'"),
            ("grep -rn --include='*.go' foo . src/", "/fake/ccx code grep foo --glob '*.go'"),  # finding 3 + include
        ],
    )
    def test_no_dir_glob(self, command: str, expected: str) -> None:
        assert grep_guards.grep_to(make_evt(command)) == expected


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
        assert grep_guards.grep_to(make_evt(command)) == expected

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
        assert grep_guards.grep_to(make_evt(command)) is None
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
        assert grep_guards.grep_to(make_evt("grep -E 'a+' f")) == "/fake/ccx code grep a+ --regex --glob f"

    def test_bre_plus_stays_literal(self, monkeypatch: pytest.MonkeyPatch) -> None:
        probe(monkeypatch, NO_SUPPORT_HELP)  # literal rewrite needs no --regex probe
        assert grep_guards.grep_to(make_evt("grep 'a+' f")) == "/fake/ccx code grep a+ --glob f"

    def test_fixed_metachar_never_flips_to_regex(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # `grep -F 'foo.*' f`: -F forces literal, `foo.*` isn't ccx-literal-safe → not rewritable.
        # `f` is a small existing file, so the grep is bounded → the condition never fires (allow),
        # and it certainly never rewrites with `--regex`.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        cl = CommandLine.parse("grep -F 'foo.*' f")
        assert grep_guards.grep_to(make_evt("grep -F 'foo.*' f")) is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep -F 'foo.*' f"), cl) is False


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
        assert grep_guards.grep_to(make_evt(command)) == expected

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
        assert grep_guards.grep_to(make_evt(command)) == expected

    def test_incident_bre_alternation_over_dir_rewrites(
        self, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
    ) -> None:
        # The getaway incident (minus `-c`): a BRE-alternation grep over an existing dir rewrites to
        # `ccx code grep --regex`, the `\|` alternation translated to Rust's `|`.
        (tmp_path / "src").mkdir()
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        out = grep_guards.grep_to(make_evt("grep -e 'hybrid\\|Onward\\|Bridge' src/"))
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
        assert grep_guards.grep_to(make_evt(command)) is None

    def test_ere_alternation_stays_bre_literal_without_dash_e(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # `a|b` under the BRE default does NOT rewrite; the same pattern under `-E` does.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert grep_guards.grep_to(make_evt("grep 'a|b' .")) is None
        assert grep_guards.grep_to(make_evt("grep -E 'a|b' .")) == "/fake/ccx code grep 'a|b' --regex"

    def test_probe_fail_over_existing_file_allows(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary (no `--regex`): a regex grep over an explicit existing file is bounded and
        # unrewritable → the condition never fires (genuine allow), never a block.
        (tmp_path / "real.py").write_text("x\n")
        probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' real.py")
        assert grep_guards.grep_to(make_evt("grep 'foo.*' real.py")) is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep 'foo.*' real.py"), cl) is False

    def test_probe_fail_tree_wide_blocks(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary + `.` (a dir, not a bounded file): unrewritable and unbounded → the condition fires
        # and `grep_to` is None → block.
        probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep 'foo.*' .")
        assert grep_guards.grep_to(make_evt("grep 'foo.*' .")) is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep 'foo.*' ."), cl) is True

    def test_regex_note_discloses_rg_engine(self) -> None:
        # The note for a regex rewrite names the engine; the dot-literal disclosure does not apply.
        note = grep_guards.grep_note(make_evt("grep 'foo.*' ."))
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
        assert grep_guards.grep_to(make_evt("grep foo a.py b.py")) == "/fake/ccx code grep foo -- a.py b.py"

    def test_flag_like_operand_stays_behind_separator(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Finding 3 repro: `grep 'a+' -- safe --regex` — grep's own `--` marks `--regex` a filename.
        # The emitted command must keep it a positional (behind ccx's `--`), never let it re-parse as
        # the ccx `--regex` flag and flip the literal `a+` search into a regex one.
        probe(monkeypatch, REGEX_SUPPORTS_HELP)
        assert (
            grep_guards.grep_to(make_evt("grep 'a+' -- safe --regex"))
            == "/fake/ccx code grep a+ -- safe --regex"
        )

    def test_single_file_keeps_glob_form(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # One explicit file stays on the old `--glob <file>` form (no `--regex` probe needed).
        probe(monkeypatch, NO_SUPPORT_HELP)
        assert grep_guards.grep_to(make_evt("grep foo a.py")) == "/fake/ccx code grep foo --glob a.py"

    def test_multi_file_probe_fail_allows(self, monkeypatch: pytest.MonkeyPatch) -> None:
        # Old binary lacking `--regex`/multi-file: unrewritable, but both operands are bounded existing
        # files → the condition never fires (genuine allow).
        probe(monkeypatch, SUPPORTS_HELP)
        cl = CommandLine.parse("grep foo a.py b.py")
        assert grep_guards.grep_to(make_evt("grep foo a.py b.py")) is None
        assert grep_guards.GrepFlood().check_command_line(make_evt("grep foo a.py b.py"), cl) is False


class TestGrepBoundedPassthrough:
    """`GrepFlood` stays silent (a genuine allow) only when every grep occurrence is a bounded search
    ccx can't rewrite, judged per occurrence on its own flags and operands in a fixed order of forfeits:
    a `GREP_OPTIONS` env, `grep -r` with no operand, env alongside path operands, an uninspectable `-f`
    pattern file, a flag-supplied or positional empty pattern, and (on the stat lane) `-o`. Two shapes
    stay bounded: every operand an explicit data-ext file (matched by suffix, no stat, `-o` allowed — rg
    parity) or every operand an existing regular file whose sizes sum under the large-read threshold. A
    pipe-sink grep with no operand is a bounded stdin filter; with file operands it is judged like any
    file search. Compound lines are judged per grep occurrence — one unbounded grep fires the whole line.
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
        ],
    )
    def test_bounded_existing_files_do_not_fire(self, command: str) -> None:
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is False

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo big.py",  # count mode skips the size cap on an existing file (the incident fix)
            "grep -L foo big.py",  # files-without-match — one line per operand
            "grep -q foo big.py",  # quiet / exit-code contract
            "grep -ci foo big.py",  # bundled count + ignore-case
            "grep --count foo big.py",  # long-form count
            "grep --files-with-matches foo big.py",  # long-form list-only (the short `-l` is a DROP flag → rewrites)
        ],
    )
    def test_output_bounded_skips_size_cap(self, command: str) -> None:
        # -c/-q/-l/-L output is one line per operand, not per match, so an over-cap existing file
        # (big.py > LARGE_READ_BYTES) stays bounded — the condition is silent and the grep runs as-is.
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is False

    @pytest.mark.parametrize(
        "command",
        [
            "grep -o foo big.py",  # -o forfeits the stat lane — the output-bounded skip is never reached
            "grep -oc foo big.py",  # the -o forfeit fires before the -c output-bounded skip
            "grep -c foo sub/",  # count over a directory is still tree-wide
            "grep -c foo ghost.py",  # count over a missing file — every operand must exist
        ],
    )
    def test_output_bounded_skip_stays_narrow(self, command: str) -> None:
        # The skip is only for -c/-q/-l/-L over existing regular files; -o, directories, and missing
        # operands still block (the condition fires and `grep_to` yields no rewrite).
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True
        assert grep_guards.grep_to(make_evt(command)) is None

    @pytest.mark.parametrize(
        "command",
        [
            "grep -c foo ghost.py",  # nonexistent operand → not bounded
            "grep -c foo sub/",  # directory operand → not bounded
            "grep -c foo real.py ghost.py",  # one real file, one absent → every operand must exist
            "grep -c foo",  # no operand at all → not bounded (tree-wide)
            "grep -o . big.py",  # -o forfeits the stat lane (big.py is over-threshold anyway) → block
            "grep -o foo real.py",  # -o forfeits the stat lane even under threshold (per-match prefixes)
            "grep -on foo real.py",  # -o bundled with -n still forfeits the stat lane
            "grep -oHnb . real.py",  # R1: -o + -H/-n/-b prefixes multiply output far past the size bound
            "grep -v foo real.py",  # -v (invert-match) isn't a bounded flag → unbounded when unpiped
            "grep -x foo big.py",  # -x is bounded but not output-bounded → the size cap still blocks over-cap
            "grep '' real.py",  # empty positional pattern floods every line → block
        ],
    )
    def test_unbounded_grep_fires_and_blocks(self, command: str) -> None:
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is True
        assert grep_guards.grep_to(make_evt(command)) is None

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
        command = "grep -x foo big.txt"
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is False

    def test_multi_file_sum_gates_the_stat_lane(self) -> None:
        # `-x` is bounded but NOT output-bounded, so it reaches the size-sum branch: two half-cap files
        # are bounded apart yet fire together once their sizes sum past the cap.
        bounded = "grep -x foo half_a.py"
        assert grep_guards.GrepFlood().check_command_line(make_evt(bounded), CommandLine.parse(bounded)) is False
        over = "grep -x foo half_a.py half_b.py"
        assert grep_guards.GrepFlood().check_command_line(make_evt(over), CommandLine.parse(over)) is True

    def test_recursive_count_does_not_ride_the_output_bounded_skip(self) -> None:
        # -c/-q/-l/-L is one line per operand only WITHOUT recursion; under -r/-R a count fans out one
        # line per file in the tree, so an over-cap recursive count falls back to the size cap and blocks.
        over = "grep -rc foo big.py"
        assert grep_guards.GrepFlood().check_command_line(make_evt(over), CommandLine.parse(over)) is True
        # A small recursive count/quiet grep still passes the size cap — recursion alone isn't a flood on
        # a small file (these stay count-mode so `grep_to` yields no rewrite that would mask the verdict).
        for ok in ("grep -rc foo real.py", "grep -rq foo real.py"):
            assert grep_guards.GrepFlood().check_command_line(make_evt(ok), CommandLine.parse(ok)) is False

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
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is False

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
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is False

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
            # a genuine pipe-sink filter (no operand, env allowed) stays a bounded stdin filter.
            ("grep -c foo real.py; printf x | LC_ALL=C grep pat", False),
        ],
    )
    def test_sink_grep_semantics(self, command: str, fires: bool) -> None:
        # Every case is a compound (primary is multi-part), so `grep_parse` bails before any live ccx probe.
        assert grep_guards.GrepFlood().check_command_line(make_evt(command), CommandLine.parse(command)) is fires


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
        res = grep_guards.grep_guard(self.bash_pre("grep foo ~/.claude/projects/main.jsonl; grep bar ."))
        assert res.action.value == "block"
        assert "floods context" in res.message and "cc-transcript" in res.message
        assert res.message != search_common.TRANSCRIPT_STEER  # not transcript-only

    def test_all_transcript_line_is_steer_only(self) -> None:
        res = grep_guards.grep_guard(self.bash_pre("grep -r foo ~/.claude/projects/"))
        assert res.action.value == "block"
        assert res.message == search_common.TRANSCRIPT_STEER
