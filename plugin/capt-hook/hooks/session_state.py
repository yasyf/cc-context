"""Block a full re-read of a text file this context already read or edited, unchanged since."""

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

LOG_CAP = 512

MEDIA_SUFFIXES = frozenset(
    {".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".ico", ".tif", ".tiff", ".pdf"}
)

MAIN_CONTEXT = "main"

READ_MESSAGE = (
    "You already read this file this session. "
    "Run `ccx code read <path> --section A-B` for a slice, or re-run Read with offset/limit."
)

EDITED_MESSAGE = (
    "You just edited this file. "
    "Run `ccx vcs diff` to review the change, or `ccx code read <path> --section A-B` for a slice."
)


class FileAccess(BaseModel):
    context: str
    path: str
    kind: Literal["read", "edited"]
    digest: str


@session_state
class FileAccessLog(BaseModel):
    accesses: Deque[FileAccess, LOG_CAP]


def context_key(evt: BaseHookEvent) -> str:
    return f"agent:{evt.agent_id}" if evt.is_subagent else MAIN_CONTEXT


def resolved_path(evt: BaseHookEvent) -> Path | None:
    return evt.file.path.resolve() if evt.file else None


def is_media(path: Path) -> bool:
    """Return whether ``path`` is binary media (an image or PDF) the gate must not touch."""
    return path.suffix.lower() in MEDIA_SUFFIXES


def is_windowed(evt: BaseHookEvent, call: ReadCall) -> bool:
    return call.offset is not None or call.limit is not None or bool(evt.input.raw.get("pages"))


def file_digest(path: Path) -> str | None:
    if not path.is_file():
        return None
    return hashlib.blake2b(path.read_bytes(), digest_size=16).hexdigest()


def remember(evt: BaseHookEvent, path: Path, kind: Literal["read", "edited"], digest: str) -> None:
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
    return evt.block(template)


@on(Event.PreCompact)
def reset_on_compact(evt: BaseHookEvent) -> None:
    """Wipe the access log on compaction; the model has lost the file contents and may re-read."""
    evt.ctx.s[FileAccessLog].delete()
    return None
