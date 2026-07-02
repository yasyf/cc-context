"""Shared constants and helpers for the cc-context guard pack.

The guards steer Claude away from the handful of tool invocations that reliably
flood the context window — an unbounded ``Read`` of a huge file, a full ``git
diff``, raw ``grep`` file searches, ``sed -n A,Bp`` / ``cat`` line dumps, recursive
``ls``/``find`` trees — and toward the ``ccx`` tools that return the same
information compactly (``ccx outline``, ``ccx read --section``, ``ccx diff``, ``ccx
find``, ``ccx symbol``, ``ccx grep``). The MCP tools (``mcp__cc-context__outline``
and friends) mirror every command.

The themed guard modules import these as ``from .common import ...``. The module
registers no hooks, so discovery loads it as a harmless no-op.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

from captain_hook import BaseHookEvent, CommandLine, Deque, DurableState, resolve_binary

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

# JSON-output flags that mark a command as worth wrapping in `ccx toon`. The glued
# forms (`--json`, `-ojson`, `-o=json`, `--output=json`, `--format=json`) are caught
# by a single regex over each arg; the two-token forms (`-o json`, `--output json`,
# `--format json`) need adjacency, handled by `has_json_output_flag`.
JSON_FLAG_GLUED = re.compile(r"^(--json(=.*)?|-o=?json|--(output|format)=json)$")
JSON_VALUE_FLAGS = ("-o", "--output", "--format")

# A command-shape subcommand token is a lowercase command word (`view`, `get`,
# `list`). Positional *values* (`123`, `/path`, `file.json`, `NAME=v`, uppercase
# refs) are not, so they drop out of the shape — `gh issue view 123` and `gh issue
# view 456` collapse to one shape, while `gh issue view` and `gh pr view` do not.
SUBCOMMAND_TOKEN = re.compile(r"^[a-z][a-z0-9-]*$")


def is_large(path: Path) -> bool:
    """Report whether ``path`` exists and exceeds :data:`LARGE_READ_BYTES`.

    A missing path is not large — the Read of a nonexistent file fails on its own
    and never reaches a token budget worth guarding.
    """
    return path.is_file() and path.stat().st_size > LARGE_READ_BYTES


def ccx_bin() -> str | None:
    """Resolve an absolute, executable ``ccx`` path for the rewrite guards, or ``None``.

    Tries ``$CLAUDE_PLUGIN_ROOT/bin/ccx``, the ``plugin/bin/ccx`` shim relative to
    this file, then ``shutil.which("ccx")``. Returns ``None`` when none resolves, so
    a rewrite guard can fall back to a hard block instead of emitting a broken command.
    """
    return resolve_binary("ccx", extra_dirs=[Path(__file__).resolve().parents[1] / "bin"])


def has_json_output_flag(cl: CommandLine) -> bool:
    """Report whether the primary command carries a JSON-output flag.

    Catches the glued forms (``--json``, ``-ojson``, ``-o=json``, ``--output=json``,
    ``--format=json``) directly, and the two-token forms (``-o json``, ``--output
    json``, ``--format json``) by scanning argument adjacency.
    """
    args = cl.primary.args
    if any(JSON_FLAG_GLUED.match(a) for a in args):
        return True
    return any(args[i] in JSON_VALUE_FLAGS and args[i + 1] == "json" for i in range(len(args) - 1))


def already_wrapped(cl: CommandLine) -> bool:
    """Report whether the command line is already a ``ccx toon`` wrap."""
    return "ccx toon" in cl.raw


def is_single_command(cl: CommandLine) -> bool:
    """Report whether the line is one command — no pipe, redirect, or ``&&``/``;`` chain."""
    return len(cl.parts) == 1 and not cl.q.uses_redirect()


def command_shape(cl: CommandLine) -> str:
    """Return a stable identity for a command, collapsing argument *values*.

    The shape is ``executable`` + subcommand tokens (lowercase command words like
    ``view``/``get``, per :data:`SUBCOMMAND_TOKEN`) + sorted flag *names* (values
    dropped), so ``gh issue view 123`` and ``gh issue view 456`` share one shape
    while ``gh issue view`` and ``gh pr view`` do not. A heuristic: it ignores flag
    *order* and positional/flag argument values, the right grain for "have I seen
    this kind of command emit JSON before". Distinguishing a subcommand from a
    positional value is command grammar, not syntax, so the rule errs toward
    distinctness — an unrecognized value-shaped token drops, a word-shaped one stays.
    """
    cmd = cl.primary
    subcommands: list[str] = []
    for a in cmd.args:
        if a.startswith("-"):
            break  # a flag begins; everything after is a flag arg or positional value
        if SUBCOMMAND_TOKEN.match(a):
            subcommands.append(a)
    flags = sorted(a.split("=", 1)[0] for a in cmd.args if a.startswith("-"))
    return " ".join([cmd.executable, *subcommands, *flags])


def looks_like_json(s: str) -> bool:
    """Report whether ``s`` is JSON or NDJSON, by a real parse (never a first-char sniff).

    Returns ``True`` when the trimmed text parses as a single JSON document, or when
    it is NDJSON — every non-empty line parses on its own. A first-character check
    would false-positive on prose that happens to start with ``[`` or ``{``, so the
    parse is mandatory.
    """
    trimmed = s.strip()
    if not trimmed:
        return False
    try:
        json.loads(trimmed)
        return True
    except ValueError:
        pass
    lines = [ln for ln in trimmed.splitlines() if ln.strip()]
    if len(lines) < 2:
        return False
    try:
        for ln in lines:
            json.loads(ln)
    except ValueError:
        return False
    return True


class JsonShapes(DurableState, scope="global"):
    """Cross-session store of command shapes observed emitting JSON.

    The bounded deque is capped at enough shapes to cover a session's worth of distinct
    JSON-emitting commands without growing unbounded; it auto-evicts oldest-first.
    """

    shapes: Deque[256]


def load_shapes(evt: BaseHookEvent) -> set[str]:
    """Load the set of command shapes observed emitting JSON, empty on a cold cache.

    Honors ``$CAPTAIN_HOOK_STATE_DIR`` via the durable store; a missing or corrupt store
    yields an empty set.
    """
    return set(JsonShapes.load(evt).shapes)


def record_shape(evt: BaseHookEvent, shape: str) -> None:
    """Record ``shape`` in the durable store, moving an already-present shape to newest.

    The read-modify-write runs under the durable store's file lock and persists atomically,
    so concurrent ``PostToolUse`` recorders never corrupt or lose the file. A plain append
    neither dedups nor refreshes recency, so a shape already present is removed before
    re-appending; the bounded deque then evicts oldest-first.
    """
    with JsonShapes.mutate(evt) as s:
        if shape in s.shapes:
            s.shapes.remove(shape)
        s.shapes.append(shape)
