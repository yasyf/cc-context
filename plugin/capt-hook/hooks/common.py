"""Shared constants and helpers for the cc-context guard pack; registers no hooks."""

from __future__ import annotations

import functools
import json
import re
import subprocess
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import BaseHookEvent, CommandLine, Deque, DurableState, resolve_binary

if TYPE_CHECKING:
    from collections.abc import Callable

    from captain_hook import Call, Command
    from cc_transcript.command import Occurrence

CCX_SERVERS = frozenset({"cc-context", "plugin_cc-context_cc-context"})

LARGE_READ_BYTES = 20_000

READ_WINDOW_LINES = 100

SNIFF_BYTES = 8_000

GIT_DIFF_SUMMARY_FLAGS = ("--stat", "--numstat", "--shortstat", "--name-only", "--name-status", "--dirstat")

IDENT_ALT = re.compile(r"\b[A-Za-z_]\w*(?:\|[A-Za-z_]\w*)+\b")

LITERAL_SAFE = re.compile(r"^[\w ./:@,=+-]+$")

JSON_FLAG_GLUED = re.compile(r"^(--json(=.*)?|-o=?json|--(output|format)=json)$")
JSON_VALUE_FLAGS = ("-o", "--output", "--format")

STREAMING_FLAGS = frozenset({"-w", "-f", "--watch", "--watch-only", "--follow"})

SHELL_WORD_EXECUTABLES = frozenset({"time", "command", "builtin", "exec", "eval", "source", "."})

SUBCOMMAND_TOKEN = re.compile(r"^[a-z][a-z0-9-]*$")

MAIN_CONTEXT = "main"

NOTE_SCOPE = "ccx-note"


def context_key(evt: BaseHookEvent) -> str:
    return f"agent:{evt.agent_id}" if evt.is_subagent else MAIN_CONTEXT


def first_sight(evt: BaseHookEvent, note: str) -> str | None:
    return note if evt.ctx.s.once(note, scope=f"{NOTE_SCOPE}:{context_key(evt)}") else None


def once_note(note: str) -> Callable[..., str | None]:
    return lambda evt, *_: first_sight(evt, note)


def rewrote_text(dst: str, gain: str = "same output, token-bounded") -> str:
    return f"Rewrote the command to `{dst}`: {gain}."


def rewrote_note(dst: str, gain: str = "same output, token-bounded") -> Callable[..., str | None]:
    return once_note(rewrote_text(dst, gain))


def call_of(evt: BaseHookEvent, occ: Occurrence) -> Call:
    return next(call for call in evt.cmd.calls() if call.occurrence.index == occ.index)


def command_expands(command: Command, markers: tuple[str, ...] = ("$", "`")) -> bool:
    return any(word.value is None or any(marker in word.raw for marker in markers) for word in command.words)


def is_large(path: Path) -> bool:
    return path.is_file() and path.stat().st_size > LARGE_READ_BYTES


def is_text(path: Path) -> bool:
    with path.open("rb") as f:
        return b"\0" not in f.read(SNIFF_BYTES)


def carries_expansion(token: str, *, tilde_only: bool = False) -> bool:
    """Whether ``token`` holds a ``~``/``$`` expansion that a single-quoted rewrite would freeze.

    ``tilde_only`` is for emitters that embed the token in double quotes, where ``$`` still expands.
    """
    return token.startswith("~") or (not tilde_only and "$" in token)


def ccx_bin() -> str | None:
    """Resolve ``ccx`` from ``$CLAUDE_PLUGIN_ROOT/bin``, the plugin's ``bin`` symlink, or ``PATH``."""
    return resolve_binary("ccx", extra_dirs=[Path(__file__).resolve().parents[2] / "bin"])


@functools.cache
def ccx_supports(*subcmd: str, flag: str | None = None) -> bool:
    """Whether ``ccx <subcmd…> --help`` exits 0 and, when ``flag`` is given, mentions it."""
    ccx = ccx_bin()
    if ccx is None:
        return False
    proc = subprocess.run([ccx, *subcmd, "--help"], capture_output=True, text=True)
    if proc.returncode != 0:
        return False
    return flag is None or flag in proc.stdout + proc.stderr


def json_flagged(args: tuple[str, ...]) -> bool:
    if any(JSON_FLAG_GLUED.match(a) for a in args):
        return True
    return any(args[i] in JSON_VALUE_FLAGS and args[i + 1] == "json" for i in range(len(args) - 1))


def has_json_output_flag(cl: CommandLine) -> bool:
    return json_flagged(cl.primary.args)


def head_has_json_output_flag(cl: CommandLine) -> bool:
    return json_flagged(cl.head.args)


def runs_ccx(command: Command) -> bool:
    return Path(command.unwrapped.executable).name == "ccx"


def is_ccx_command(cl: CommandLine) -> bool:
    return runs_ccx(cl.primary)


def already_wrapped(cl: CommandLine) -> bool:
    return cl.q.any_command(lambda command: runs_ccx(command) and command.unwrapped.args[:1] == ("format",))


def is_single_command(cl: CommandLine) -> bool:
    return len(cl.parts) == 1 and not cl.q.uses_redirect()


def has_streaming_flag(command: Command) -> bool:
    return any(a.split("=", 1)[0] in STREAMING_FLAGS for a in command.args)


def spells_argv(command: Command, text: str, span: tuple[int, int] | None) -> bool:
    """Whether ``span`` of ``text`` holds only ``command``'s own words, separated by whitespace.

    An env prefix, a shell-word executable, or anything the parser folded out of the
    words (a bare substitution, a redirect, a subshell paren) fails the check.
    """
    if span is None or command.env or command.executable in SHELL_WORD_EXECUTABLES:
        return False
    source = text.encode()
    cursor, end = span
    for word in command.words:
        if word.span is None or source[cursor : word.span[0]].strip():
            return False
        cursor = word.span[1]
    return not source[cursor:end].strip()


def is_plain_argv(cl: CommandLine) -> bool:
    return spells_argv(cl.primary, cl.raw, (0, len(cl.raw.encode())))


def command_shape(cl: CommandLine) -> str:
    """The executable, its leading subcommand words, and its sorted flag names, values dropped."""
    cmd = cl.primary
    subcommands: list[str] = []
    for a in cmd.args:
        if a.startswith("-"):
            break
        if SUBCOMMAND_TOKEN.match(a):
            subcommands.append(a)
    flags = sorted(a.split("=", 1)[0] for a in cmd.args if a.startswith("-"))
    return " ".join([cmd.executable, *subcommands, *flags])


def parses_as_json(text: str | bytes) -> bool:
    try:
        json.loads(text)
    except ValueError:
        return False
    return True


def looks_like_json(s: object) -> bool:
    """Whether ``s`` is text holding one JSON document or NDJSON lines, by a real parse."""
    if not isinstance(s, (str, bytes)) or not (trimmed := s.strip()):
        return False
    if parses_as_json(trimmed):
        return True
    lines = [ln for ln in trimmed.splitlines() if ln.strip()]
    return len(lines) >= 2 and all(parses_as_json(ln) for ln in lines)


class JsonShapes(DurableState, scope="global"):
    """Command shapes observed emitting JSON, oldest first."""

    shapes: Deque[256]


def load_shapes(evt: BaseHookEvent) -> set[str]:
    return set(JsonShapes.load(evt).shapes)


def record_shape(evt: BaseHookEvent, shape: str) -> None:
    with JsonShapes.mutate(evt) as s:
        if shape in s.shapes:
            s.shapes.remove(shape)
        s.shapes.append(shape)
