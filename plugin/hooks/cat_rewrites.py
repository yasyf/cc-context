"""Rewrite a bare single-file ``cat <file>`` to ``ccx code read --full`` in place, with a
``note`` back to the model. A raw root-manifest ``cat`` (redundant with ``ccx repo
overview``) and the multi-file form hard-block onto the right ``ccx`` entry point instead.
When ``ccx`` cannot be resolved on disk the rewrite falls back to a hard block, so the guard
never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import shlex

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    Rewrite,
    rewrite_command,
)

from .common import carries_expansion, ccx_bin

ROOT_MANIFESTS = ("go.mod", "AGENTS.md", "CLAUDE.md", "pyproject.toml", "Taskfile.yml", "package.json")


def is_root_manifest(path: str) -> bool:
    base = path.rstrip("/").removeprefix("./")
    if "/" in base:  # a directory prefix means it isn't the repo-root manifest
        return False
    return base in ROOT_MANIFESTS or base.startswith("README")


class ManifestCat(CustomCommandLineCondition):
    """Matches a bare ``cat``/``bat`` of a repo-root manifest — a redundant raw dump.

    ``ccx repo overview`` already summarizes go.mod / README* / CLAUDE.md and friends, so
    dumping the raw file just wastes context. Only a single root-level manifest with no
    pipe, redirect, heredoc, or flag matches; nested copies (`internal/go.mod`) and piped
    uses fall through to :class:`BareCat` or stay allowed.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if "<<" in (evt.command or "") or cl.q.uses_redirect():
            return False
        if not (cl.q.runs("cat") or cl.q.runs("bat")):
            return False
        a = cl.primary.args
        return len(a) == 1 and not a[0].startswith("-") and is_root_manifest(a[0])


rewrite_command(
    only_if=[ManifestCat()],
    to=lambda evt: None,
    block=(
        "BLOCKED: `cat`/`bat` of a root manifest is redundant — orient with `ccx repo overview` "
        "(it already summarizes the manifest; or mcp__cc-context__ccx_repo_overview). "
        "Need the raw file? `ccx code read <file> --full`."
    ),
    tests={
        Input(command="cat go.mod"): Block(pattern="ccx repo overview"),
        Input(command="cat README.md"): Block(pattern="ccx repo overview"),
        Input(command="bat CLAUDE.md"): Block(pattern="ccx repo overview"),
        Input(command="cat ./package.json"): Block(pattern="ccx code read"),
        Input(command="cat internal/go.mod"): Allow(),  # nested copy, not the root manifest
        Input(command="cat main.go"): Allow(),  # not a manifest — BareCat rewrites it
        Input(command="cat go.mod | grep module"): Allow(),  # piped, not a raw dump
        # `ccx exec --file -` heredoc pass-through is deliberate — this rule's own
        # `<<` short-circuit, locked here because it is per-rule, not pack-wide.
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("cat go.mod")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)


class BareCat(CustomCommandLineCondition):
    """Matches ``cat <file>...`` with no pipe, redirect, heredoc, or flag.

    `cat f | cmd`, `cat > f`, and `cat << EOF` all use cat for streaming/writing,
    not for dumping a file's contents into context — only the bare read matches. A
    single file argument is rewritten to `ccx code read --full`; multiple files (no
    single `--full` target) stay a hard block. A file token carrying a shell expansion
    (``~``/``$``) declines — ``shlex.quote`` would freeze it, so the command falls
    through to Allow and the real shell expands the path.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # Heredocs (`cat << EOF`) and redirects/pipes are streaming/writing uses.
        if "<<" in (evt.command or "") or cl.q.uses_redirect():
            return False
        if not (cl.q.runs("cat") and bool(a := cl.primary.args) and not a[0].startswith("-")):
            return False
        if any(carries_expansion(p) for p in a):
            return False
        # A repo-root manifest gets the `ccx repo overview` steer (ManifestCat), not a raw read.
        return not (len(a) == 1 and is_root_manifest(a[0]))


def cat_to(evt: BaseHookEvent) -> str | None:
    files = evt.command_line.primary.args
    if len(files) == 1 and (ccx := ccx_bin()):
        return f"{ccx} code read {shlex.quote(files[0])} --full"
    return None


def cat_note(evt: BaseHookEvent) -> str:
    file = evt.command_line.primary.args[0]
    return f"Rewrote `cat {file}` → `ccx code read --full`: same content, token-bounded."


rewrite_command(
    only_if=[BareCat()],
    to=cat_to,
    block=(
        "BLOCKED: bare `cat <file>` dumps the whole file into context. "
        "Use `ccx code outline <file>` to map it, then `ccx code read <file> --section A-B` for the part "
        "you need (or the mcp__cc-context__ccx_code_outline/ccx_code_read tools). "
        "Escape hatch — whole file: `ccx code read <file> --full`."
    ),
    note=cat_note,
    tests={
        Input(command="cat main.go"): Rewrite(pattern="code read main.go --full"),
        Input(command="cat a.go b.go"): Block(pattern="ccx code outline"),
        # A `~`/`$` file token declines to rewrite — shlex.quote would freeze it; the real shell expands it.
        Input(command="cat ~/notes.md"): Allow(),
        Input(command="cat $d/main.go"): Allow(),
        Input(command="cat foo~bar.go"): Rewrite(pattern="code read 'foo~bar.go' --full"),  # mid-token `~` is literal
        # Known-conservative: the parser dequotes per token, so a single-quoted (literal) `$d` is
        # indistinguishable from an unquoted one and declines either way — a safe Allow, merely unguarded.
        Input(command="cat '$d/main.go'"): Allow(),
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
        # `ccx exec` pass-through is deliberate, in both the quoted-script form (the
        # cat inside sh() is one opaque token) and this rule's `<<` short-circuit.
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
