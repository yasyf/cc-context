"""cc-context guard pack — hard blocks on token-bomb tool calls, with escape hatches.

These hooks steer Claude away from the handful of tool invocations that reliably
flood the context window — an unbounded ``Read`` of a huge file, a full ``git
diff``, ``sed -n A,Bp`` / ``cat`` line dumps, recursive ``ls``/``find`` trees — and
toward the ``ccx`` tools that return the same information compactly (``ccx outline``,
``ccx read --section``, ``ccx diff``, ``ccx find``, ``ccx symbol``, ``ccx grep``).
The MCP tools (``mcp__cc-context__outline`` and friends) mirror every command.

Tiering:

  * BLOCK the clear token-bombs (guards 1-6). Every block message names a concrete
    ``ccx`` replacement *and* an escape hatch for the rare case where the raw dump
    is actually wanted (``ccx read --full``, ``git diff -- <file>``, the action
    forms of ``find``, ...).
  * WARN the judgment calls (guard 7): a ``rg``/``grep -r`` over an identifier
    alternation usually wants ``ccx symbol`` or ``ccx grep``, but it still runs.

All guards live in this one file so they share a single import-time registration
pass and never race each other across files. Per-repo opt-in is the pack's presence
in the manifest; there is no runtime availability probe, so the blocks are
unconditional once the pack is enabled.
"""

from __future__ import annotations

import re
import tempfile
from pathlib import Path

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CustomCondition,
    Event,
    Input,
    Tool,
    Warn,
    block_command,
    hook,
    nudge,
    on,
)

# A Read with neither offset nor limit pulls the whole file into context. Past this
# size the dump is a token-bomb; below it the cost is negligible, so the block only
# bites large files. Tuned to the task spec's ~50 KB threshold.
LARGE_READ_BYTES = 50_000

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
        "BLOCKED: unbounded Read of a large file (>50KB) floods context. "
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
    file argument is the `ccx read --section` case this blocks.
    """

    PATTERN = re.compile(r"sed\s+(?:-[a-zA-Z]+\s+)*-n\s+['\"]?\d+,\d+p['\"]?\s+\S")

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        # A piped sed reads a stream, not the trailing file token — leave it alone.
        return not cl.q.uses_redirect() and bool(self.PATTERN.search(evt.command or ""))


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SedLineRange()],
    message=(
        "BLOCKED: `sed -n A,Bp <file>` is a line-range dump. "
        "Use `ccx read <file> --section A-B` (or mcp__cc-context__read) — it returns the "
        "same lines with structure. Escape hatch: pipe it (`cat <file> | sed -n 'A,Bp'`)."
    ),
    block=True,
    tests={
        Input(command="sed -n '10,40p' f.go"): Block(pattern="ccx read"),
        Input(command="sed -n 10,40p f.go"): Block(),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
    },
)


class BareCat(CustomCondition):
    """Matches ``cat <file>...`` with no pipe, redirect, or heredoc.

    `cat f | cmd`, `cat > f`, and `cat << EOF` all use cat for streaming/writing,
    not for dumping a file's contents into context — only the bare read is blocked.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        if not (cl := evt.command_line):
            return False
        cmd = evt.command or ""
        # Heredocs (`cat << EOF`) and redirects/pipes are streaming/writing uses.
        if "<<" in cmd or cl.q.uses_redirect():
            return False
        return cl.q.runs("cat") and bool(re.search(r"^\s*cat\s+\S", cmd)) and not re.search(r"^\s*cat\s+-\b", cmd)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), BareCat()],
    message=(
        "BLOCKED: bare `cat <file>` dumps the whole file into context. "
        "Use `ccx outline <file>` to map it, then `ccx read <file> --section A-B` for the part "
        "you need (or the mcp__cc-context__ outline/read tools). "
        "Escape hatch — whole file: `ccx read <file> --full`."
    ),
    block=True,
    tests={
        Input(command="cat main.go"): Block(pattern="ccx outline"),
        Input(command="cat a.go b.go"): Block(),
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
    },
)


block_command(
    r"ls\s+(?:-\w*R\w*|--recursive)\b|ls\s+(?:-\w+\s+)*-\w*R",
    reason="`ls -R` walks the whole tree into context",
    hint=(
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool, '
        "to find paths by pattern. Plain `ls` and `ls -la` stay allowed"
    ),
    tests={
        Input(command="ls -R"): Block(pattern="ccx find"),
        Input(command="ls -laR src"): Block(),
        Input(command="ls -R src"): Block(),
        Input(command="ls --recursive"): Block(),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
    },
)


class FindEnumeration(CustomCondition):
    """Matches ``find <path> -name ...`` used to *list* matches (no action flag).

    A find that ends in an action — `-exec`, `-delete`, `-print0` (almost always
    `| xargs`) — is doing work, not flooding context; only the bare enumeration is
    the `ccx find` / Glob case.
    """

    ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")

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


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), FindEnumeration()],
    message=(
        "BLOCKED: `find ... -name` enumeration floods context. "
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool. '
        "Escape hatch — need an action: keep the `-exec`/`-delete`/`-print0 | xargs` form."
    ),
    block=True,
    tests={
        Input(command="find . -name '*.go'"): Block(pattern="ccx find"),
        Input(command="find src -iname '*.PY'"): Block(),
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # no -name, not an enumeration we steer
    },
)


class RgIdentAlternation(CustomCondition):
    """Matches an ``rg``/``grep -r`` whose pattern is an identifier alternation.

    `rg 'fooBar|bazQux' src/` is almost always "find these symbols" — `ccx symbol`
    resolves a definition and its callers in one shot, and `ccx grep` groups hits
    compactly. A single-term search carries no such signal, so it does not match.
    """

    def check(self, evt: BaseHookEvent) -> bool:
        cmd = evt.command or ""
        if not re.search(r"^\s*(?:rg\b|grep\s+(?:-\w*r|-\w*\s+-\w*r|--recursive))", cmd):
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
        Input(command="grep -r 'Foo|Bar' ."): Warn(),
        Input(command="rg TODO"): Allow(),
        Input(command="rg 'just one term' src/"): Allow(),
    },
)
