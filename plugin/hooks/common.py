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

import re
from pathlib import Path

from captain_hook import resolve_binary

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
