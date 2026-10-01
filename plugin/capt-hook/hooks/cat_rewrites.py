"""Rewrite a large bare single-file ``cat`` to ``ccx code read --full``; block a bare root manifest ``cat``."""

from __future__ import annotations

import os
import shlex
import subprocess
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Event,
    FileFixture,
    Input,
    Rewrite,
    Tool,
    hook,
    rewrite_command_occurrences,
)
from captain_hook.util.shell import normalize_executable

from .common import LARGE_READ_BYTES, ccx_bin, command_expands, is_large, rewrote_note

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

ROOT_MANIFESTS = ("go.mod", "AGENTS.md", "CLAUDE.md", "pyproject.toml", "Taskfile.yml", "package.json")

AT_ROOT = {"git -C /usr rev-parse": "/usr"}


def is_root_manifest(path: str) -> bool:
    base = path.rstrip("/").removeprefix("./")
    if "/" in base:
        return False
    return base in ROOT_MANIFESTS or base.startswith("README")


def line_has_heredoc(evt: BaseHookEvent) -> bool:
    return "<<" in evt.cmd.raw


def bare_cat_files(occ: Occurrence) -> tuple[str, ...] | None:
    cmd = occ.command
    if occ.nesting or occ.piped or cmd.redirects or normalize_executable(cmd.executable) != "cat":
        return None
    args = cmd.args
    if not args or args[0].startswith("-"):
        return None
    return args


def is_manifest_cat(occ: Occurrence) -> bool:
    cmd = occ.command
    if occ.piped or cmd.redirects or normalize_executable(cmd.executable) not in ("cat", "bat"):
        return False
    args = cmd.args
    return len(args) == 1 and not args[0].startswith("-") and is_root_manifest(args[0])


def single_cat_target(occ: Occurrence) -> str | None:
    files = bare_cat_files(occ)
    if files is None or len(files) != 1:
        return None
    operand = files[0]
    if command_expands(occ.command) or is_root_manifest(operand):
        return None
    return os.path.expanduser(operand)


def cat_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if line_has_heredoc(evt) or (target := single_cat_target(occ)) is None:
        return None
    if is_large(Path(target)) and (ccx := ccx_bin()):
        return f"{shlex.quote(ccx)} code read {shlex.quote(target)} --full"
    return None


def is_git_toplevel(cwd: Path | None) -> bool:
    if cwd is None:
        return False
    top = subprocess.run(["git", "-C", str(cwd), "rev-parse", "--show-toplevel"], capture_output=True, text=True)
    return top.returncode == 0 and Path(top.stdout.strip()).resolve() == cwd.resolve()


class ManifestCat(CustomCommandLineCondition):
    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return not line_has_heredoc(evt) and any(
            is_manifest_cat(call.occurrence) and is_git_toplevel(call.cwd) for call in evt.cmd.calls()
        )


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), ManifestCat()],
    message=(
        "`cat` of a root manifest dumps what `ccx repo overview` already summarizes. "
        "Run `ccx repo overview`, or `ccx code read <file> --full` for the raw file."
    ),
    block=True,
    tests={
        Input(command="cat go.mod", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command="cat README.md", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command="bat CLAUDE.md", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command="cat ./package.json", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx code read"),
        Input(command="cat go.mod; echo x", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command="/bin/cat go.mod", cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command='"cat" go.mod', cwd="/usr", commands=AT_ROOT): Block(pattern="ccx repo overview"),
        Input(command="cd /usr && cat pyproject.toml", cwd="/", commands=AT_ROOT): Block(
            pattern="ccx repo overview"
        ),
        Input(command="cd lib && cat pyproject.toml", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(command="cd bin && cat package.json", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(command="cat go.mod", cwd="/usr/lib", commands={"git -C /usr/lib rev-parse": "/usr"}): Allow(),
        Input(command="cat go.mod", cwd="/"): Allow(),
        Input(command="cat go.mod"): Allow(),
        Input(command="sudo cat go.mod", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(command="cat internal/go.mod", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(command="cat main.go", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(command="cat go.mod | grep module", cwd="/usr", commands=AT_ROOT): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("cat go.mod")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)


rewrite_command_occurrences(
    to=cat_to,
    note=rewrote_note("ccx code read --full", "same content, token-bounded"),
    tests={
        Input(command="cat {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Rewrite(
            pattern="code read /"
        ),
        Input(command="/bin/cat {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Rewrite(
            pattern="code read /"
        ),
        Input(command="sudo cat {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Allow(),
        Input(command="cat {file}", file=FileFixture(size=64, name="small.md")): Allow(),
        Input(command="cat /etc/hosts"): Allow(),
        Input(command="cat /nonexistent/trip.json"): Allow(),
        Input(command="cat /etc/hosts /etc/hosts"): Allow(),
        Input(command="cat {file} {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Allow(),
        Input(command="cat ~/big.md", file=FileFixture(home=True, name="big.md", size=LARGE_READ_BYTES + 1)): Rewrite(
            pattern="code read /"
        ),
        Input(command="cat ~/small.md", file=FileFixture(home=True, name="small.md", size=64)): Allow(),
        Input(command="cat ~/no-such-file.md"): Allow(),
        Input(
            command='cat {file}; echo "---TRIP.JSON---"; cat /nonexistent/trip.json',
            file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md"),
        ): Rewrite(pattern='; echo "---TRIP.JSON---"; cat /nonexistent/trip.json'),
        Input(command="cat $d/main.go"): Allow(),
        Input(command="cat '$d/main.go'"): Allow(),
        Input(command="cat go.mod"): Allow(),
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"cat main.go\")\nasyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("cat main.go")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)
