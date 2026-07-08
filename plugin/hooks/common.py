"""Shared constants and helpers for the cc-context guard pack.

The guards steer Claude away from the handful of tool invocations that reliably
flood the context window — an unbounded ``Read`` of a huge file, a full ``git
diff``, raw ``grep`` file searches, ``sed -n A,Bp`` / ``cat`` line dumps, recursive
``ls``/``find`` trees, whole-page web fetches (``WebFetch``, an unpiped ``curl``/``wget``
page dump) — and toward the ``ccx`` tools that return the same information compactly
(``ccx code outline``, ``ccx code read --section``, ``ccx vcs diff``, ``ccx repo find``,
``ccx code symbol``, ``ccx code grep``, ``ccx web outline``, ``ccx web read --section``,
``ccx web search``). The MCP tools
(``mcp__cc-context__ccx_code_outline`` and friends) mirror the query surface, plus
``ccx_exec``/``ccx_exec_tools`` for sandboxed multi-call composition.

The themed guard modules import these as ``from .common import ...``. The module
registers no hooks, so discovery loads it as a harmless no-op.
"""

from __future__ import annotations

import json
import re
import shlex
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

# JSON-output flags that mark a command as worth wrapping in `ccx format`. The glued
# forms (`--json`, `-ojson`, `-o=json`, `--output=json`, `--format=json`) are caught
# by a single regex over each arg; the two-token forms (`-o json`, `--output json`,
# `--format json`) need adjacency, handled by `has_json_output_flag`.
JSON_FLAG_GLUED = re.compile(r"^(--json(=.*)?|-o=?json|--(output|format)=json)$")
JSON_VALUE_FLAGS = ("-o", "--output", "--format")

# Watch/follow flags mark a command that streams until killed. `ccx format`
# buffers the child's whole stdout and converts only after exit, so wrapping a
# never-exiting command yields zero output until the Bash tool times out.
STREAMING_FLAGS = frozenset({"-w", "-f", "--watch", "--watch-only", "--follow"})

# Shell words the parser accepts as an executable but whose meaning changes (or
# vanishes) outside bash: `time` is a keyword; `command`/`builtin`/`exec`/`eval`/
# `source`/`.` are builtins with no binary counterpart. After `ccx format --`
# they would exec as literal binaries, so the wrap bails.
SHELL_WORD_EXECUTABLES = frozenset({"time", "command", "builtin", "exec", "eval", "source", "."})

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

    Tries ``$CLAUDE_PLUGIN_ROOT/bin/ccx``, the ``plugin/bin/ccx`` symlink relative to
    this file (the installer points it at a brew binary, the downloaded payload, or a
    dev build), then ``shutil.which("ccx")``. Returns ``None`` when none resolves, so
    a rewrite guard can fall back to a hard block instead of emitting a broken command.
    """
    return resolve_binary("ccx", extra_dirs=[Path(__file__).resolve().parents[1] / "bin"])


def _json_flagged(args: tuple[str, ...]) -> bool:
    if any(JSON_FLAG_GLUED.match(a) for a in args):
        return True
    return any(args[i] in JSON_VALUE_FLAGS and args[i + 1] == "json" for i in range(len(args) - 1))


def has_json_output_flag(cl: CommandLine) -> bool:
    """Report whether the primary command carries a JSON-output flag.

    Catches the glued forms (``--json``, ``-ojson``, ``-o=json``, ``--output=json``,
    ``--format=json``) directly, and the two-token forms (``-o json``, ``--output
    json``, ``--format json``) by scanning argument adjacency.
    """
    return _json_flagged(cl.primary.args)


def head_has_json_output_flag(cl: CommandLine) -> bool:
    """Report whether the line's *first* command carries a JSON-output flag.

    :func:`has_json_output_flag` inspects ``cl.primary`` — the line's last command,
    the right grain for the single-command ``ccx format`` wrap. A pipe steer cares about
    the producer at the head of the pipeline instead (``<cmd --json> | jq``).
    """
    return _json_flagged(cl.head.args)


def is_ccx_command(cl: CommandLine) -> bool:
    """Report whether the primary command runs the ``ccx`` binary itself.

    ccx output is already token-bounded (``ccx exec`` return values are budget-capped
    and rendered in their leanest encoding), so the JSON-shape learner must skip it — a
    learned ``ccx exec`` shape would nudge wrapping ccx in ``ccx format``, which is
    wrong advice.
    """
    return Path(cl.primary.executable).name == "ccx"


def already_wrapped(cl: CommandLine) -> bool:
    """Report whether the command line is already a ``ccx format`` wrap.

    Load-bearing for the ``json_guards`` rewrite: the wrapped line still carries its
    JSON-output flag, so failing to recognize the wrap would re-wrap it forever.
    """
    return "ccx format" in cl.raw


def is_single_command(cl: CommandLine) -> bool:
    """Report whether the line is one command — no pipe, redirect, or ``&&``/``;`` chain."""
    return len(cl.parts) == 1 and not cl.q.uses_redirect()


def has_streaming_flag(cl: CommandLine) -> bool:
    """Report whether the primary command carries a watch/follow flag (:data:`STREAMING_FLAGS`).

    Catches both the bare (``--watch``, ``-w``) and glued (``--watch=true``) forms.
    """
    return any(a.split("=", 1)[0] in STREAMING_FLAGS for a in cl.primary.args)


def is_plain_argv(cl: CommandLine) -> bool:
    """Report whether the raw line is exactly the primary command's argv.

    The ``ccx format -- <raw>`` rewrite splices the raw text after ``--``, where
    bash re-parses it as plain words for ccx to exec directly: an env-assignment
    prefix becomes a bogus argv[0] (``exec`` fails), a subshell becomes a bash
    syntax error, and a shell keyword like ``time`` stops being a keyword. Safe
    iff the command carries no env prefix, its executable is a real word (not in
    :data:`SHELL_WORD_EXECUTABLES`), and the raw text word-splits to exactly the
    parsed executable + args. Structure the parser folded out of the argv (a bare
    command substitution, a redirect) fails that comparison and bails; quoted
    substitutions and variable expansions survive it verbatim and wrap safely —
    bash expands the spliced raw text after ``--`` exactly as it would the
    original line.
    """
    if cl.primary.env or cl.primary.executable in SHELL_WORD_EXECUTABLES:
        return False
    try:
        words = shlex.split(cl.raw)
    except ValueError:
        return False
    return words == [cl.primary.executable, *cl.primary.args]


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


def looks_like_json(s: object) -> bool:
    """Report whether ``s`` is JSON or NDJSON, by a real parse (never a first-char sniff).

    Returns ``True`` when the trimmed text parses as a single JSON document, or when
    it is NDJSON — every non-empty line parses on its own. A first-character check
    would false-positive on prose that happens to start with ``[`` or ``{``, so the
    parse is mandatory. A non-``str``/``bytes`` argument — a structured tool_response
    mapping reaching a caller — is never JSON text, so it returns ``False`` rather than
    raising on the missing ``.strip``.
    """
    if not isinstance(s, (str, bytes)):
        return False
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
