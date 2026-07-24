"""Rewrite a large bare single-file ``cat`` to ``ccx code read --full``; block a bare root
manifest as redundant with ``ccx repo overview``.

Both guards handle every occurrence of a compound line. Heredoc lines decline as a whole because
their bodies can contain inner ``cat`` commands that cannot be classified per occurrence.
"""

from __future__ import annotations

import os
import shlex
from pathlib import Path
from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    FileFixture,
    Input,
    Rewrite,
    rewrite_command,
    rewrite_command_occurrences,
)

from .common import LARGE_READ_BYTES, ccx_bin, is_large

if TYPE_CHECKING:
    from cc_transcript.command import Occurrence

ROOT_MANIFESTS = ("go.mod", "AGENTS.md", "CLAUDE.md", "pyproject.toml", "Taskfile.yml", "package.json")


def is_root_manifest(path: str) -> bool:
    base = path.rstrip("/").removeprefix("./")
    if "/" in base:  # a directory prefix means it isn't the repo-root manifest
        return False
    return base in ROOT_MANIFESTS or base.startswith("README")


def line_has_heredoc(evt: BaseHookEvent) -> bool:
    """Whether the Bash line carries a heredoc (``<<``), which declines the whole line.

    Per-occurrence heredoc detection is unsound — a ``cat << EOF`` body can contain any text,
    including an inner ``cat`` — so a compound with any heredoc stays untouched.
    """
    return "<<" in evt.cmd.raw


def bare_cat_files(occ: Occurrence) -> tuple[str, ...] | None:
    """The operands of a bare ``cat <file>...`` occurrence, or ``None`` when it isn't one.

    ``None`` when the occurrence is nested below top level (a ``$(…)``/``eval`` payload, whose
    splice can't survive its quote layers), is piped or carries a redirect (streaming/writing
    uses, not a context dump), or when it is not a flagless ``cat`` read (executable ``cat``, at
    least one arg, first arg not a flag). The line-level heredoc decline lives in
    :func:`line_has_heredoc`, not here — a heredoc is a property of the whole line.
    """
    cmd = occ.command
    if occ.nesting or occ.piped or cmd.redirects or cmd.executable != "cat":
        return None
    args = cmd.args
    if not args or args[0].startswith("-"):
        return None
    return args


def is_manifest_cat(occ: Occurrence) -> bool:
    """Whether ``occ`` is a bare single-file ``cat``/``bat`` of a repo-root manifest.

    The redundant raw dump :class:`ManifestCat` blocks — its rationale is redundancy with
    ``ccx repo overview``, not size, so there is no size gate. Piped or redirected occurrences
    decline (streaming/writing), and only a lone root-level manifest operand matches: a nested
    copy (``internal/go.mod``) falls through to the size-gated :class:`BareCat` lane.
    """
    cmd = occ.command
    if occ.piped or cmd.redirects or cmd.executable not in ("cat", "bat"):
        return False
    args = cmd.args
    return len(args) == 1 and not args[0].startswith("-") and is_root_manifest(args[0])


def single_cat_target(occ: Occurrence) -> str | None:
    """The ``expanduser``'d operand of a single-file bare ``cat``, or ``None`` when out of lane.

    ``None`` unless the occurrence is a single-operand bare cat whose operand carries no ``$``
    (only the real shell can expand it — a rewrite would freeze a literal ``$d/x`` that does not
    exist) and is not a repo-root manifest (:class:`ManifestCat` owns that block). A leading
    ``~`` is expanded here, so the emitted rewrite carries the real absolute path, never a
    frozen ``~`` the ccx command's quoting would leave unexpanded.
    """
    files = bare_cat_files(occ)
    if files is None or len(files) != 1:
        return None
    operand = files[0]
    if "$" in operand or is_root_manifest(operand):
        return None
    return os.path.expanduser(operand)


def cat_to(evt: BaseHookEvent, occ: Occurrence) -> str | None:
    if line_has_heredoc(evt) or (target := single_cat_target(occ)) is None:
        return None
    if is_large(Path(target)) and (ccx := ccx_bin()):
        return f"{shlex.quote(ccx)} code read {shlex.quote(target)} --full"
    return None


def cat_note(evt: BaseHookEvent, pairs: list[tuple[Occurrence, str]]) -> str:
    reads = ", ".join(f"`cat {bare_cat_files(occ)[0]}`" for occ, _ in pairs)
    return f"Rewrote {reads} → `ccx code read --full`: same content, token-bounded."


class ManifestCat(CustomCommandLineCondition):
    """Matches a bare ``cat``/``bat`` of a repo-root manifest in ANY occurrence of a
    ``;``/``&&``/``|``-joined line — a redundant raw dump.

    ``ccx repo overview`` already summarizes go.mod / README* / CLAUDE.md and friends, so dumping
    the raw file just wastes context. Only a single root-level manifest with no pipe, redirect,
    or flag matches per occurrence; nested copies (``internal/go.mod``) and piped uses fall
    through to :class:`BareCat` or stay allowed. A heredoc anywhere declines the whole line.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return not line_has_heredoc(evt) and any(is_manifest_cat(occ) for occ in cl.occurrences)


class BareCat(CustomCommandLineCondition):
    """Matches a line carrying a large single-file bare ``cat`` that can be rewritten.

    Every non-rewritable or ambiguous occurrence falls through; rewrites preserve siblings.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(cat_to(evt, occ) is not None for occ in cl.occurrences)


rewrite_command(
    only_if=[ManifestCat()],
    to=lambda evt: None,
    block=(
        "BLOCKED: `cat`/`bat` of a root manifest is redundant — orient with `ccx repo overview` "
        "(it already summarizes the manifest). "
        "Need the raw file? `ccx code read <file> --full`."
    ),
    tests={
        Input(command="cat go.mod"): Block(pattern="ccx repo overview"),
        Input(command="cat README.md"): Block(pattern="ccx repo overview"),
        Input(command="bat CLAUDE.md"): Block(pattern="ccx repo overview"),
        Input(command="cat ./package.json"): Block(pattern="ccx code read"),
        # Any-occurrence: a manifest cat past a `;` now blocks the line (the incident — the old
        # cl.primary-only rule wrongly allowed this, dumping go.mod while the `echo` ran).
        Input(command="cat go.mod; echo x"): Block(pattern="ccx repo overview"),
        Input(command="cat internal/go.mod"): Allow(),  # nested copy, not the root manifest
        Input(command="cat main.go"): Allow(),  # not a manifest — the BareCat lane size-gates it
        Input(command="cat go.mod | grep module"): Allow(),  # piped, not a raw dump
        # `ccx exec --file -` heredoc pass-through is deliberate — this rule's own `<<` decline.
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("cat go.mod")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)


rewrite_command_occurrences(
    only_if=[BareCat()],
    to=cat_to,
    note=cat_note,
    tests={
        # A large existing single file rewrites to `ccx code read --full` with the absolute path.
        Input(command="cat {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Rewrite(
            pattern="code read /"
        ),
        # Size-gate: a small existing file stays a bare cat (bounded — no rewrite).
        Input(command="cat {file}", file=FileFixture(size=64, name="small.md")): Allow(),
        # Small out-of-repo absolute (bounded) and a nonexistent absolute (fails on its own) both pass.
        Input(command="cat /etc/hosts"): Allow(),
        Input(command="cat /nonexistent/trip.json"): Allow(),
        # Multi-file cats are outside this rewrite lane regardless of aggregate size.
        Input(command="cat /etc/hosts /etc/hosts"): Allow(),
        Input(command="cat {file} {file}", file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md")): Allow(),
        # `~` is expanded for real: a large home file rewrites with the EXPANDED absolute path (proving no
        # frozen `~` — a frozen token would read `code read ~/…`, never `code read /…`).
        Input(command="cat ~/big.md", file=FileFixture(home=True, name="big.md", size=LARGE_READ_BYTES + 1)): Rewrite(
            pattern="code read /"
        ),
        Input(command="cat ~/small.md", file=FileFixture(home=True, name="small.md", size=64)): Allow(),
        Input(command="cat ~/no-such-file.md"): Allow(),
        # The incident shape: the leading large cat rewrites while the `echo` and the trailing nonexistent
        # cat survive verbatim (the old cl.primary-only guard silently dropped both).
        Input(
            command='cat {file}; echo "---TRIP.JSON---"; cat /nonexistent/trip.json',
            file=FileFixture(size=LARGE_READ_BYTES + 1, name="big.md"),
        ): Rewrite(pattern='; echo "---TRIP.JSON---"; cat /nonexistent/trip.json'),
        # A `$` operand declines — only the real shell can expand it; the raw cat runs.
        Input(command="cat $d/main.go"): Allow(),
        # Known-conservative: a single-quoted (literal) `$d` is dequoted to `$d/main.go` and declines either way.
        Input(command="cat '$d/main.go'"): Allow(),
        # A repo-root manifest is ManifestCat's to block, never this lane's to rewrite.
        Input(command="cat go.mod"): Allow(),
        # Pipe / heredoc / redirect are streaming/writing uses, not context dumps.
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
        # `ccx exec` pass-through is deliberate in both the quoted-script form (the cat inside sh() is one
        # opaque token) and this rule's heredoc short-circuit.
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
