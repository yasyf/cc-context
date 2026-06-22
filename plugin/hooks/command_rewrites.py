"""Rewrite token-bomb Bash commands to their compact ``ccx`` equivalent in place.

``sed -n A,Bp <file>``, a bare single-file ``cat``, ``ls -R``, and ``find -name``
enumeration each map cleanly to a ``ccx`` call and are rewritten with a ``note`` back
to the model. When ``ccx`` cannot be resolved on disk the rewrite falls back to a hard
block, so the guard never emits a broken ``ccx: command not found``.
"""

from __future__ import annotations

import re
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

from ._common import ccx_bin


class SedLineRange(CustomCommandLineCondition):
    """Matches ``sed -n 'A,Bp' <file>`` — a numeric line-range extract from a file.

    Allows sed in a pipe (it consumes a stream, not a named file) and allows
    substitution (`sed 's/.../.../'`); only the standalone numeric-range print of a
    file argument is the `ccx read --section` case this rewrites.
    """

    PATTERN = re.compile(r"sed\s+(?:-[a-zA-Z]+\s+)*-n\s+['\"]?(\d+),(\d+)p['\"]?\s+(\S+)")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # A piped sed reads a stream, not the trailing file token — leave it alone.
        return not cl.q.uses_redirect() and bool(self.PATTERN.search(evt.command or ""))


def _sed_to(evt: BaseHookEvent) -> str | None:
    m = SedLineRange.PATTERN.search(evt.command or "")
    start, end, file = m.group(1), m.group(2), m.group(3).strip("'\"")
    if ccx := ccx_bin():
        return f"{ccx} read {shlex.quote(file)} --section {start}-{end}"
    return None


def _sed_note(evt: BaseHookEvent) -> str:
    m = SedLineRange.PATTERN.search(evt.command or "")
    start, end, file = m.group(1), m.group(2), m.group(3).strip("'\"")
    return f"Rewrote `sed -n {start},{end}p {file}` → `ccx read --section`: same lines, token-bounded."


rewrite_command(
    only_if=[SedLineRange()],
    to=_sed_to,
    block=(
        "BLOCKED: `sed -n A,Bp <file>` is a line-range dump. "
        "Use `ccx read <file> --section A-B` (or mcp__cc-context__read) — it returns the "
        "same lines with structure. Escape hatch: pipe it (`cat <file> | sed -n 'A,Bp'`)."
    ),
    note=_sed_note,
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
    },
)


class BareCat(CustomCommandLineCondition):
    """Matches ``cat <file>...`` with no pipe, redirect, heredoc, or flag.

    `cat f | cmd`, `cat > f`, and `cat << EOF` all use cat for streaming/writing,
    not for dumping a file's contents into context — only the bare read matches. A
    single file argument is rewritten to `ccx read --full`; multiple files (no
    single `--full` target) stay a hard block.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        cmd = evt.command or ""
        # Heredocs (`cat << EOF`) and redirects/pipes are streaming/writing uses.
        if "<<" in cmd or cl.q.uses_redirect():
            return False
        return cl.q.runs("cat") and bool(re.search(r"^\s*cat\s+\S", cmd)) and not re.search(r"^\s*cat\s+-\b", cmd)


def _cat_to(evt: BaseHookEvent) -> str | None:
    files = evt.command_line.primary.args
    if len(files) == 1 and (ccx := ccx_bin()):
        return f"{ccx} read {shlex.quote(files[0])} --full"
    return None


def _cat_note(evt: BaseHookEvent) -> str:
    file = evt.command_line.primary.args[0]
    return f"Rewrote `cat {file}` → `ccx read --full`: same content, token-bounded."


rewrite_command(
    only_if=[BareCat()],
    to=_cat_to,
    block=(
        "BLOCKED: bare `cat <file>` dumps the whole file into context. "
        "Use `ccx outline <file>` to map it, then `ccx read <file> --section A-B` for the part "
        "you need (or the mcp__cc-context__ outline/read tools). "
        "Escape hatch — whole file: `ccx read <file> --full`."
    ),
    note=_cat_note,
    tests={
        Input(command="cat main.go"): Rewrite(pattern="read main.go --full"),
        Input(command="cat a.go b.go"): Block(pattern="ccx outline"),
        Input(command="cat f | grep x"): Allow(),
        Input(command="cat <<EOF"): Allow(),
        Input(command="cat << EOF"): Allow(),
        Input(command="cat > f"): Allow(),
        Input(command="cat >> f"): Allow(),
    },
)


class LsRecursive(CustomCommandLineCondition):
    """Matches ``ls -R [dir]`` — a recursive listing that walks the whole tree.

    Plain `ls` and `ls -la` stay allowed; only a recursive flag (`-R`, bundled like
    `-laR`, or `--recursive`) matches. The optional directory argument becomes the
    `ccx find "<dir>/**"` glob root, defaulting to `**`.
    """

    PATTERN = re.compile(r"ls\s+(?:-\w*R\w*|--recursive)\b|ls\s+(?:-\w+\s+)*-\w*R")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return bool(self.PATTERN.search(evt.command or ""))


def _ls_glob(evt: BaseHookEvent) -> str:
    dirs = [a for a in evt.command_line.primary.args if not a.startswith("-")]
    return f"{dirs[0].rstrip('/')}/**" if dirs else "**"


def _ls_to(evt: BaseHookEvent) -> str | None:
    if ccx := ccx_bin():
        return f'{ccx} find "{_ls_glob(evt)}"'
    return None


def _ls_note(evt: BaseHookEvent) -> str:
    glob = _ls_glob(evt)
    return f'Rewrote `ls -R` → `ccx find "{glob}"`: same paths, token-bounded.'


rewrite_command(
    only_if=[LsRecursive()],
    to=_ls_to,
    block=(
        "BLOCKED: `ls -R` walks the whole tree into context. "
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool, '
        "to find paths by pattern. Plain `ls` and `ls -la` stay allowed."
    ),
    note=_ls_note,
    tests={
        Input(command="ls -R"): Rewrite(pattern='find "**"'),
        Input(command="ls -laR src"): Rewrite(pattern='find "src/**"'),
        Input(command="ls -R src"): Rewrite(pattern='find "src/**"'),
        Input(command="ls --recursive"): Rewrite(pattern='find "**"'),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
    },
)


class FindEnumeration(CustomCommandLineCondition):
    """Matches ``find <path> -name ...`` used to *list* matches (no action flag).

    A find that ends in an action — `-exec`, `-delete`, `-print0` (almost always
    `| xargs`) — is doing work, not flooding context; only the bare enumeration is
    the `ccx find` / Glob case.
    """

    ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
    NAME_FLAGS = ("-name", "-iname")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        cmd = evt.command or ""
        return (
            cl.q.runs("find")
            and bool(re.search(r"\bfind\b.*\s-(?:name|iname|path|regex)\b", cmd))
            and not any(cl.q.contains_token(a) for a in self.ACTIONS)
            and not cl.q.uses_redirect()
        )


def _find_parts(evt: BaseHookEvent) -> tuple[str, str, str] | None:
    args = evt.command_line.primary.args
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    if not flag:
        return None
    path = args[0] if args and not args[0].startswith("-") else "."
    glob = args[args.index(flag) + 1]
    prefix = "" if path == "." else f"{path.rstrip('/')}/"
    return flag, path, f"{prefix}**/{glob}"


def _find_to(evt: BaseHookEvent) -> str | None:
    parts = _find_parts(evt)
    if parts and (ccx := ccx_bin()):
        return f'{ccx} find "{parts[2]}"'
    return None


def _find_note(evt: BaseHookEvent) -> str:
    flag, path, full_glob = _find_parts(evt)
    args = evt.command_line.primary.args
    glob = args[args.index(flag) + 1]
    return f'Rewrote `find {path} {flag} {glob}` → `ccx find "{full_glob}"`: same paths, token-bounded.'


rewrite_command(
    only_if=[FindEnumeration()],
    to=_find_to,
    block=(
        "BLOCKED: `find ... -name` enumeration floods context. "
        'Use `ccx find "<glob>"` (or mcp__cc-context__find), or the built-in Glob tool. '
        "Escape hatch — need an action: keep the `-exec`/`-delete`/`-print0 | xargs` form."
    ),
    note=_find_note,
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='find "**/*.go"'),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='find "src/**/*.PY"'),
        Input(command="find . -path '*/gen/*'"): Block(pattern="ccx find"),  # -path stays a block
        Input(command="find . -regex '.*\\.go'"): Block(),  # -regex stays a block
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # no -name, not an enumeration we steer
    },
)
