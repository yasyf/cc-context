"""Gate a redundant full-file re-read or edit-then-read — the top mined token sink.

A ``Read`` that pulls a whole text file the agent already fully read (or just edited)
this session floods context with bytes that are already there. This pair of hooks learns
what has been read/edited and blocks the redundant repeat:

* a **recorder** on ``PostToolUse`` (Read/Edit/Write/MultiEdit) logs each access, keyed by
  ``(context, resolved absolute path)`` with a content fingerprint of the file;
* a **gate** on ``PreToolUse`` (Read) blocks a *full-file* re-read of a logged path whose
  fingerprint is unchanged, steering the agent to context, a ``ccx`` slice, or an
  ``offset``/``limit`` Read.

``context`` is the reading agent's context window: the main agent (no ``agent_id``) or a
subagent (its own ``agent_id``). Keying by it confines the gate to a single context, so the
main agent is never blocked by a subagent's read and each subagent is gated only against its
own reads. (Claude Code does not send ``agent_transcript_path`` on tool-use events, so it
cannot separate contexts; sibling subagents are isolated only insofar as their ``agent_id``
values differ.)

Windowed reads never participate — a slice (``offset``/``limit``) or a paged PDF/image
(``pages``) leaves only part of the file in context, and binary media (images, PDFs) have no
text slice to fall back to. Compaction wipes the log — after a compact the model has genuinely
lost the contents and must be free to re-read. A file whose content changed (fingerprint drift)
always passes, even when its mtime and size are preserved.
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Literal

from captain_hook import (
    Allow,
    BaseHookEvent,
    Deque,
    Event,
    FileFixture,
    HookResult,
    Input,
    ReadCall,
    Tool,
    on,
    session_state,
)
from pydantic import BaseModel

# The log auto-evicts oldest-first past this many distinct accesses, so a long session
# stays bounded without ever growing without limit.
LOG_CAP = 512

# Binary media Claude Code renders visually rather than as text. The gate targets text/code
# token sinks; a media file has no line slice to fall back to, so it is never recorded or gated.
MEDIA_SUFFIXES = frozenset(
    {".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".ico", ".tif", ".tiff", ".pdf"}
)

# Sentinel context for the main agent, which carries no ``agent_id``.
MAIN_CONTEXT = "main"

READ_MESSAGE = (
    "BLOCKED: you already read {path} this session — reference it from context, "
    "jump to a slice with `ccx code read {path} --section A-B`, "
    "or re-run Read with offset/limit."
)

EDITED_MESSAGE = (
    "BLOCKED: you just edited {path} — the Edit result already confirmed the change; "
    "review with `ccx vcs diff`, read a targeted slice with `ccx code read {path} --section A-B`, "
    "or re-run Read with offset/limit."
)


class FileAccess(BaseModel):
    """One access record: which ``context`` touched which ``path``, how, and its ``digest`` then.

    ``digest`` is a content fingerprint of the file at record time; the gate compares it against
    the file's current fingerprint so any content change — even one that preserves size and
    mtime — reopens the file for re-reading.
    """

    context: str
    path: str
    kind: Literal["read", "edited"]
    digest: str


@session_state
class FileAccessLog(BaseModel):
    """Per-session log of full reads and edits, one entry per ``(context, path)`` pair.

    Bounded to the most recent :data:`LOG_CAP` accesses; appending past the cap evicts
    oldest-first. Reset wholesale on context compaction.
    """

    accesses: Deque[FileAccess, LOG_CAP]


def context_key(evt: BaseHookEvent) -> str:
    """Return the identity of the reading agent's context window.

    A subagent carries its own ``agent_id``; the main agent carries none. Keying the log by
    this value keeps a subagent's reads out of the main agent's namespace (and out of a sibling
    subagent's, when the two carry distinct ``agent_id`` values).
    """
    if evt.is_subagent:
        return f"agent:{evt._raw['agent_id']}"
    return MAIN_CONTEXT


def resolved_path(evt: BaseHookEvent) -> Path | None:
    """Return the event's file as a canonical absolute path, collapsing ``.``/``..`` and symlinks.

    Normalizing both on record and on gate means a relative and an absolute spelling of
    the same file resolve to one log entry.
    """
    return evt.file.path.resolve() if evt.file else None


def is_media(path: Path) -> bool:
    """Return whether ``path`` is binary media (an image or PDF) the gate must not touch."""
    return path.suffix.lower() in MEDIA_SUFFIXES


def is_windowed(evt: BaseHookEvent, call: ReadCall) -> bool:
    """Return whether a Read pulls only a window rather than the whole file.

    A window is an ``offset``/``limit`` slice or a PDF/image ``pages`` range. ``ReadCall`` has
    no ``pages`` field, so a paged read looks like a full read; the raw ``pages`` value in the
    tool input is the only signal that it is windowed.
    """
    return call.offset is not None or call.limit is not None or bool(evt._tool_input.get("pages"))


def file_digest(path: Path) -> str | None:
    """Return a content fingerprint of ``path``, or ``None`` when it cannot be read.

    The gate blocks only while this matches the recorded fingerprint, so a rewrite that keeps
    the file's size and mtime still reopens it for re-reading.
    """
    try:
        data = path.read_bytes()
    except OSError:
        return None
    return hashlib.blake2b(data, digest_size=16).hexdigest()


def remember(evt: BaseHookEvent, path: Path, kind: Literal["read", "edited"], digest: str) -> None:
    """Upsert the ``(context, path)`` record, moving an existing entry to newest.

    Dropping any prior entry for the pair before appending keeps exactly one record per
    file per context — the latest kind and digest — and preserves the bounded deque's
    oldest-first eviction.
    """
    cid = context_key(evt)
    key = str(path)
    log = evt.ctx.s.load(FileAccessLog)
    for existing in log.accesses:
        if existing.context == cid and existing.path == key:
            log.accesses.remove(existing)
            break
    log.accesses.append(FileAccess(context=cid, path=key, kind=kind, digest=digest))
    evt.ctx.s[FileAccessLog].set(log)


def lookup(evt: BaseHookEvent, path: Path) -> FileAccess | None:
    """Return the record for ``(this context, path)``, or ``None`` when unseen."""
    cid = context_key(evt)
    key = str(path)
    for record in reversed(evt.ctx.s.load(FileAccessLog).accesses):
        if record.context == cid and record.path == key:
            return record
    return None


@on(
    Event.PostToolUse,
    only_if=[Tool("Read|Edit|Write|MultiEdit")],
    tests={
        Input(tool="Read", file=FileFixture(size=64)): Allow(),
        Input(tool="Write", file=FileFixture(size=64), content="x"): Allow(),
        Input(tool="Edit", file=FileFixture(size=64), old="a", content="b"): Allow(),
        Input(tool="Read", tool_input={"file_path": "/tmp/report.pdf", "pages": "1-5"}): Allow(),
        Input(tool="Read", file="/tmp/diagram.png"): Allow(),
    },
)
def record_file_access(evt: BaseHookEvent) -> None:
    """Log a successful full read or an edit so the gate can spot the redundant repeat.

    A *sliced* or *paged* read leaves only part of the file in context, and binary media has no
    text slice, so neither is logged — logging them would let a partial view falsely block a
    later legitimate read. ``PostToolUse`` fires only on success, so a failed read never records.
    """
    path = resolved_path(evt)
    if path is None or is_media(path):
        return None
    if evt.tool_name == "Read":
        call = evt.as_input(ReadCall)
        if call is None or is_windowed(evt, call):
            return None
        kind: Literal["read", "edited"] = "read"
    else:
        kind = "edited"
    digest = file_digest(path)
    if digest is None:
        return None
    remember(evt, path, kind, digest)
    return None


@on(
    Event.PreToolUse,
    only_if=[Tool("Read")],
    tests={
        Input(tool="Read", file=FileFixture(size=64)): Allow(),
        Input(tool="Read", file=FileFixture(size=64), offset=1, limit=50): Allow(),
        Input(tool="Read", tool_input={"file_path": "/tmp/report.pdf", "pages": "6-10"}): Allow(),
        Input(tool="Read", file="/tmp/diagram.png"): Allow(),
    },
)
def gate_reread(evt: BaseHookEvent) -> HookResult | None:
    """Block a full-file Read of a text path this context already read/edited, content unchanged.

    Passes silently when the Read is windowed (``offset``/``limit``/``pages``), the file is
    binary media, the path is unseen, the file is gone, or its content changed since it was read.
    """
    call = evt.as_input(ReadCall)
    if call is None or is_windowed(evt, call):
        return None
    path = resolved_path(evt)
    if path is None or is_media(path):
        return None
    record = lookup(evt, path)
    if record is None or file_digest(path) != record.digest:
        return None
    template = EDITED_MESSAGE if record.kind == "edited" else READ_MESSAGE
    return evt.block(template.format(path=path))


@on(Event.PreCompact)
def reset_on_compact(evt: BaseHookEvent) -> None:
    """Wipe the access log on compaction; the model has lost the file contents and may re-read."""
    evt.ctx.s[FileAccessLog].delete()
    return None
