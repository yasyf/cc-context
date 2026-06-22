"""cc-context guard pack — token-bomb tool calls rewritten to ``ccx`` or blocked.

These hooks steer Claude away from the handful of tool invocations that reliably
flood the context window — an unbounded ``Read`` of a huge file, a full ``git
diff``, raw ``grep`` file searches, ``sed -n A,Bp`` / ``cat`` line dumps, recursive
``ls``/``find`` trees — and toward the ``ccx`` tools that return the same
information compactly (``ccx outline``, ``ccx read --section``, ``ccx diff``, ``ccx
find``, ``ccx symbol``, ``ccx grep``). The MCP tools (``mcp__cc-context__outline``
and friends) mirror every command.

Tiering:

  * BLOCK the token-bombs with no information-equivalent rewrite: an unbounded
    large ``Read``, a full ``git diff``, and raw ``grep`` file search. Every block
    message names a concrete ``ccx`` replacement *and* an escape hatch for the rare
    case where the raw form is actually wanted (``ccx read --full``, ``git diff --
    <file>``, ``… | grep``, ...).
  * REWRITE the token-bombs that map cleanly to a ``ccx`` command: ``sed -n A,Bp
    <file>``, a bare single-file ``cat``, ``ls -R``, and ``find -name`` enumeration
    each become the equivalent ``ccx`` call in place (with a ``note`` back to the
    model). When ``ccx`` cannot be resolved on disk the rewrite falls back to the
    same hard block, so the guard never emits a broken ``ccx: command not found``.
  * WARN the judgment calls: an ``rg`` over an identifier alternation usually wants
    ``ccx symbol`` or ``ccx grep``, but it still runs.

All guards live in this one file so they share a single import-time registration
pass and never race each other across files. Per-repo opt-in is the pack's presence
in the manifest; there is no runtime availability probe, so the blocks are
unconditional once the pack is enabled.
"""

from __future__ import annotations

import os
import re
import shlex
import shutil
import tempfile
from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CustomCondition,
    Event,
    Input,
    Rewrite,
    Tool,
    Warn,
    block_command,
    hook,
    nudge,
    on,
)

# A Read with neither offset nor limit pulls the whole file into context. Past this
# size (~5k tokens) the dump is a token-bomb worth steering to an outline; below it
# the cost is negligible, so the block only bites genuinely large files. (50 KB was
# too lenient — a ~32 KB / 8k-token source file slipped through unblocked.)
LARGE_READ_BYTES = 20_000

# `git diff` is allowed when scoped (a pathspec after `--`) or summarized (one of
# these stat-only flags). A bare/range diff with no such narrowing is the bomb.
GIT_DIFF_SUMMARY_FLAGS = ("--stat", "--numstat", "--shortstat", "--name-only", "--name-status", "--dirstat")

# Identifier-alternation heuristic for the rg/grep nudge: at least two terms joined
# by `|`, each looking like a code identifier (letters/digits/underscore, no spaces).
IDENT_ALT = re.compile(r"\b[A-Za-z_]\w*(?:\|[A-Za-z_]\w*)+\b")

# Real on-disk fixtures so the size-based Read guard's inline tests exercise a real
# `stat()` — `Input(file=...)` only sets a path string, it does not create a file.
BIG_FILE = (Path(tempfile.mkdtemp(prefix="ccx_guard_")) / "big.txt").as_posix()
SMALL_FILE = (Path(tempfile.mkdtemp(prefix="ccx_guard_")) / "small.txt").as_posix()
Path(BIG_FILE).write_bytes(b"x" * (LARGE_READ_BYTES + 1))
Path(SMALL_FILE).write_bytes(b"x" * 1_024)


def is_large(path: Path) -> bool:
    """Report whether ``path`` exists and exceeds :data:`LARGE_READ_BYTES`.

    A missing path is not large — the Read of a nonexistent file fails on its own
    and never reaches a token budget worth guarding.
    """
    return path.is_file() and path.stat().st_size > LARGE_READ_BYTES


def ccx_bin() -> str | None:
    """Resolve an absolute, executable ``ccx`` path for the rewrite guards, or ``None``.

    Tries, in order: ``$CLAUDE_PLUGIN_ROOT/bin/ccx`` (wins in installed plugins),
    the ``plugin/bin/ccx`` shim relative to this file (``Path(__file__).parents[1]``
    — deterministic, and the candidate that lets inline tests resolve ``ccx`` while
    ``CLAUDE_PLUGIN_ROOT`` is unset), then ``shutil.which("ccx")``. Returns the first
    candidate that is a file and executable; ``None`` if none is, so the caller can
    fall back to a hard block instead of emitting a broken command.
    """
    candidates: list[Path] = []
    if root := os.environ.get("CLAUDE_PLUGIN_ROOT"):
        candidates.append(Path(root) / "bin" / "ccx")
    candidates.append(Path(__file__).resolve().parents[1] / "bin" / "ccx")
    for candidate in candidates:
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    return shutil.which("ccx")


class UnboundedLargeRead(CustomCondition):
    """Matches a ``Read`` of a large file with neither ``offset`` nor ``limit`` set.

    The whole point of the offset/limit knobs is to bound how much enters context;
    a Read that sets neither on a file over :data:`LARGE_READ_BYTES` is the
    unbounded dump this guard exists to stop.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        ti = evt._raw.get("tool_input", {})
        return ti.get("offset") is None and ti.get("limit") is None and bool(evt.file) and is_large(evt.file.path)


class GitDiffPager(CustomCondition):
    """Matches a ``git diff`` that is neither path-scoped nor a stat-only summary.

    `git diff -- <path>` and `git diff <ref> -- <path>` are scoped; `git diff
    --stat`/`--numstat`/`--name-only`/... are summaries. Everything else (`git diff`,
    `git diff HEAD~1`) dumps the full patch — that is what this matches.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        return cl.q.runs("git", "diff") and not cl.q.contains_token("--") and not any(
            cl.q.contains_token(flag) for flag in GIT_DIFF_SUMMARY_FLAGS
        )


# The Read guard is an @on handler, not the declarative `hook(...)` form, because the
# Allow case for a *bounded* large Read (offset/limit set) needs to read those keys
# off `evt._raw["tool_input"]` — `Input(...)` can carry `file` but not offset/limit,
# so the bounded path is exercised by checking `_raw` here directly.
@on(
    Event.PreToolUse,
    only_if=[Tool("Read"), UnboundedLargeRead()],
    tests={
        Input(tool="Read", file=BIG_FILE): Block(pattern="ccx outline"),
        Input(tool="Read", file=SMALL_FILE): Allow(),
    },
)
def block_unbounded_large_read(evt: BaseHookEvent) -> object:
    return evt.block(
        "BLOCKED: unbounded Read of a large file (>20KB) floods context. "
        "Map it first: `ccx outline <path>` (or mcp__cc-context__outline), then "
        "`ccx read <path> --section A-B` (or mcp__cc-context__read) for the part you need. "
        "Escape hatch — whole file: `ccx read <path> --full`, or re-run Read with offset/limit."
    )


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), GitDiffPager()],
    message=(
        "BLOCKED: `git diff` without a pathspec dumps the full patch into context. "
        "Use `ccx diff` for a compact summary (or mcp__cc-context__diff). "
        "Already know the file? `git diff -- <path>` / `git diff <ref> -- <path>` stays allowed, "
        "as do `git diff --stat`/`--numstat`/`--name-only`. Escape hatch for the full patch: `ccx diff --full`."
    ),
    block=True,
    tests={
        Input(command="git diff"): Block(pattern="ccx diff"),
        Input(command="git diff HEAD~1"): Block(),
        Input(command="git diff --stat"): Allow(),
        Input(command="git diff --numstat"): Allow(),
        Input(command="git diff --name-only"): Allow(),
        Input(command="git diff -- src/x.go"): Allow(),
        Input(command="git diff HEAD~1 -- src/x.go"): Allow(),
        Input(command="git status"): Allow(),
    },
)


class SedLineRange(CustomCondition):
    """Matches ``sed -n 'A,Bp' <file>`` — a numeric line-range extract from a file.

    Allows sed in a pipe (it consumes a stream, not a named file) and allows
    substitution (`sed 's/.../.../'`); only the standalone numeric-range print of a
    file argument is the `ccx read --section` case this rewrites.
    """

    PATTERN = re.compile(r"sed\s+(?:-[a-zA-Z]+\s+)*-n\s+['\"]?(\d+),(\d+)p['\"]?\s+(\S+)")

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        # A piped sed reads a stream, not the trailing file token — leave it alone.
        return not cl.q.uses_redirect() and bool(self.PATTERN.search(evt.command or ""))


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SedLineRange()],
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
    },
)
def rewrite_sed_line_range(evt: BaseHookEvent) -> object:
    m = SedLineRange.PATTERN.search(evt.command or "")
    start, end, file = m.group(1), m.group(2), m.group(3).strip("'\"")
    if ccx := ccx_bin():
        new = f"{ccx} read {shlex.quote(file)} --section {start}-{end}"
        return evt.rewrite_command(new, note=f"Rewrote `sed -n {start},{end}p {file}` → `ccx read --section`: same lines, token-bounded.")
    return evt.block(
        "BLOCKED: `sed -n A,Bp <file>` is a line-range dump. "
        "Use `ccx read <file> --section A-B` (or mcp__cc-context__read) — it returns the "
        "same lines with structure. Escape hatch: pipe it (`cat <file> | sed -n 'A,Bp'`)."
    )


class BareCat(CustomCondition):
    """Matches ``cat <file>...`` with no pipe, redirect, heredoc, or flag.

    `cat f | cmd`, `cat > f`, and `cat << EOF` all use cat for streaming/writing,
    not for dumping a file's contents into context — only the bare read matches. A
    single file argument is rewritten to `ccx read --full`; multiple files (no
    single `--full` target) stay a hard block.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        cmd = evt.command or ""
        # Heredocs (`cat << EOF`) and redirects/pipes are streaming/writing uses.
        if "<<" in cmd or cl.q.uses_redirect():
            return False
        return cl.q.runs("cat") and bool(re.search(r"^\s*cat\s+\S", cmd)) and not re.search(r"^\s*cat\s+-\b", cmd)


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), BareCat()],
    tests={
        Input(command="cat main.go"): Rewrite(pattern="read main.go --full"),
        Input(command="cat a.go b.go"): Block(pattern="ccx outline"),
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
    },
)
def rewrite_bare_cat(evt: BaseHookEvent) -> object:
    files = evt.command_line.primary.args
    if len(files) == 1 and (ccx := ccx_bin()):
        file = files[0]
        new = f"{ccx} read {shlex.quote(file)} --full"
        return evt.rewrite_command(new, note=f"Rewrote `cat {file}` → `ccx read --full`: same content, token-bounded.")
    return evt.block(
        "BLOCKED: bare `cat <file>` dumps the whole file into context. "
        "Use `ccx outline <file>` to map it, then `ccx read <file> --section A-B` for the part "
        "you need (or the mcp__cc-context__ outline/read tools). "
        "Escape hatch — whole file: `ccx read <file> --full`."
    )


class LsRecursive(CustomCondition):
    """Matches ``ls -R [dir]`` — a recursive listing that walks the whole tree.

    Plain `ls` and `ls -la` stay allowed; only a recursive flag (`-R`, bundled like
    `-laR`, or `--recursive`) matches. The optional directory argument becomes the
    `ccx find "<dir>/**"` glob root, defaulting to `**`.
    """

    PATTERN = re.compile(r"ls\s+(?:-\w*R\w*|--recursive)\b|ls\s+(?:-\w+\s+)*-\w*R")

    def check(self, evt: BaseHookEvent) -> bool:
        return bool(self.PATTERN.search(evt.command or ""))


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), LsRecursive()],
    tests={
        Input(command="ls -R"): Rewrite(pattern='find "**"'),
        Input(command="ls -laR src"): Rewrite(pattern='find "src/**"'),
        Input(command="ls -R src"): Rewrite(pattern='find "src/**"'),
        Input(command="ls --recursive"): Rewrite(pattern='find "**"'),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
    },
)
def rewrite_ls_recursive(evt: BaseHookEvent) -> object:
    dirs = [a for a in evt.command_line.primary.args if not a.startswith("-")]
    glob = f"{dirs[0].rstrip('/')}/**" if dirs else "**"
    if ccx := ccx_bin():
        new = f'{ccx} find "{glob}"'
        return evt.rewrite_command(new, note=f'Rewrote `ls -R` → `ccx find "{glob}"`: same paths, token-bounded.')
    return evt.block(
        "BLOCKED: `ls -R` walks the whole tree into context. "
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool, '
        "to find paths by pattern. Plain `ls` and `ls -la` stay allowed."
    )


class FindEnumeration(CustomCondition):
    """Matches ``find <path> -name ...`` used to *list* matches (no action flag).

    A find that ends in an action — `-exec`, `-delete`, `-print0` (almost always
    `| xargs`) — is doing work, not flooding context; only the bare enumeration is
    the `ccx find` / Glob case.
    """

    ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
    NAME_FLAGS = ("-name", "-iname")

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        cmd = evt.command or ""
        return (
            cl.q.runs("find")
            and bool(re.search(r"\bfind\b.*\s-(?:name|iname|path|regex)\b", cmd))
            and not any(cl.q.contains_token(a) for a in self.ACTIONS)
            and not cl.q.uses_redirect()
        )


@on(
    Event.PreToolUse,
    only_if=[Tool("Bash"), FindEnumeration()],
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='find "**/*.go"'),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='find "src/**/*.PY"'),
        Input(command="find . -path '*/gen/*'"): Block(pattern="ccx find"),  # -path stays a block
        Input(command="find . -regex '.*\\.go'"): Block(),  # -regex stays a block
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # no -name, not an enumeration we steer
    },
)
def rewrite_find_enumeration(evt: BaseHookEvent) -> object:
    args = evt.command_line.primary.args
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    if flag and (ccx := ccx_bin()):
        path = args[0] if args and not args[0].startswith("-") else "."
        glob = args[args.index(flag) + 1]
        prefix = "" if path == "." else f"{path.rstrip('/')}/"
        new = f'{ccx} find "{prefix}**/{glob}"'
        return evt.rewrite_command(new, note=f'Rewrote `find {path} {flag} {glob}` → `ccx find "{prefix}**/{glob}"`: same paths, token-bounded.')
    return evt.block(
        "BLOCKED: `find ... -name` enumeration floods context. "
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool. '
        "Escape hatch — need an action: keep the `-exec`/`-delete`/`-print0 | xargs` form."
    )


class RgIdentAlternation(CustomCondition):
    """Matches an ``rg`` whose pattern is an identifier alternation.

    `rg 'fooBar|bazQux' src/` is almost always "find these symbols" — `ccx symbol`
    resolves a definition and its callers in one shot, and `ccx grep` groups hits
    compactly. A single-term search carries no such signal, so it does not match.
    The `grep -r` case is owned by the grep block, not this nudge.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        cmd = evt.command or ""
        if not re.search(r"^\s*rg\b", cmd):
            return False
        return bool(IDENT_ALT.search(cmd))


nudge(
    "Searching for several identifiers? `ccx symbol <name>` (or mcp__cc-context__symbol) "
    "resolves a definition plus its callers in one call; `ccx grep <text>` groups hits "
    "compactly. This rg still runs — just consider the ccx tools for symbol lookups.",
    only_if=[Tool("Bash"), RgIdentAlternation()],
    events=Event.PreToolUse,
    tests={
        Input(command="rg 'fooBar|bazQux' src/"): Warn(pattern="ccx symbol"),
        Input(command="rg 'Foo|Bar|Baz' ."): Warn(),
        Input(command="rg TODO"): Allow(),
        Input(command="rg 'just one term' src/"): Allow(),
    },
)


class UnpipedGrep(CustomCondition):
    """Matches a ``grep`` that does not consume piped input.

    Allows the stream-filter idiom (`… | grep`) while still matching grep used for
    file searching, whether standalone, heading a pipe, or in a `&&`/`;` chain.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        return any(
            cmd.matches(r"^grep\b") and (i == 0 or cl.parts[i - 1][1] != "|") for i, (cmd, _) in enumerate(cl.parts)
        )


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), UnpipedGrep()],
    message=(
        "BLOCKED: raw `grep` for file search floods context. "
        "Use `ccx grep <text>` (or mcp__cc-context__grep) / `ccx search` for code; the "
        "built-in Grep tool or `rg` for literal content in non-source files. "
        "Escape hatch: pipe it (`… | grep`)."
    ),
    block=True,
    tests={
        Input(command="grep -rn foo src/"): Block(pattern="ccx grep"),
        Input(command="ls | grep foo"): Allow(),
        Input(command="cat x | grep foo | sort"): Allow(),
        Input(command="grep foo file.py | wc -l"): Block(),
        Input(command="grep foo a && echo done"): Block(),
        Input(command="git log --grep=fix"): Allow(),
        Input(command='git log --grep "fix bug"'): Allow(),
    },
)
