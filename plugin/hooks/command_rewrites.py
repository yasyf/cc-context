"""Rewrite token-bomb Bash commands to their compact ``ccx`` equivalent in place.

``sed -n A,Bp <file>``, a bounded ``head`` on a file, a bare single-file ``cat``,
``ls -R``, and ``find`` enumeration each map cleanly to a ``ccx`` call and are rewritten
with a ``note`` back to the model. Forms with no line-arithmetic equivalent — ``tail`` on
a file, a raw root-manifest ``cat``, a workspace-root ``ls`` — hard-block onto the right
``ccx`` entry point instead. When ``ccx`` cannot be resolved on disk the rewrite falls
back to a hard block, so the guard never emits a broken ``ccx: command not found``.
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

from .common import ccx_bin

ROOT_MANIFESTS = ("go.mod", "AGENTS.md", "CLAUDE.md", "pyproject.toml", "Taskfile.yml", "package.json")
WORKSPACE_ROOT = re.compile(r"^(~|\$HOME|\$\{HOME\})/Code/?$")


class SedLineRange(CustomCommandLineCondition):
    """Matches ``sed -n 'A,Bp' <file>`` — a numeric line-range extract from a file.

    Allows sed in a pipe (it consumes a stream, not a named file) and allows
    substitution (`sed 's/.../.../'`); only the standalone numeric-range print of a
    file argument is the `ccx code read --section` case this rewrites.
    """

    RANGE = re.compile(r"^(\d+),(\d+)p$")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # A piped sed reads a stream, not the trailing file token — leave it alone.
        return not cl.q.uses_redirect() and _sed_parts(cl) is not None


def _sed_parts(cl: CommandLine) -> tuple[str, str, str] | None:
    cmd = cl.primary
    if cmd is None or not cmd.runs("sed", "-n") or len(cmd.args) != 3:
        return None
    m = SedLineRange.RANGE.match(cmd.args[1].strip("'\""))
    if not m:
        return None
    return m.group(1), m.group(2), cmd.args[2]


def _sed_to(evt: BaseHookEvent) -> str | None:
    start, end, file = _sed_parts(evt.command_line)
    if ccx := ccx_bin():
        return f"{ccx} code read {shlex.quote(file)} --section {start}-{end}"
    return None


def _sed_note(evt: BaseHookEvent) -> str:
    start, end, file = _sed_parts(evt.command_line)
    return f"Rewrote `sed -n {start},{end}p {file}` → `ccx code read --section`: same lines, token-bounded."


rewrite_command(
    only_if=[SedLineRange()],
    to=_sed_to,
    block=(
        "BLOCKED: `sed -n A,Bp <file>` is a line-range dump. "
        "Use `ccx code read <file> --section A-B` (or mcp__cc-context__ccx_code_read) — it returns the "
        "same lines with structure. Escape hatch: pipe it (`cat <file> | sed -n 'A,Bp'`)."
    ),
    note=_sed_note,
    tests={
        Input(command="sed -n '10,40p' f.go"): Rewrite(pattern="code read f.go --section 10-40"),
        Input(command="sed -n 10,40p f.go"): Rewrite(pattern="--section 10-40"),
        Input(command="cat f | sed -n '1,2p'"): Allow(),
        Input(command="sed 's/a/b/' f"): Allow(),
        Input(command="sed -n '/start/,/end/p' f"): Allow(),  # non-numeric range
        # `ccx exec` pass-through is deliberate: the script is one opaque ccx token,
        # so a sed inside sh() never parses as this rule's sed.
        Input(
            command="ccx exec 'async def main(): return await sh(\"sed -n 10,40p f.go\")\nasyncio.run(main())'"
        ): Allow(),
    },
)


class HeadTailFile(CustomCommandLineCondition):
    """Matches ``head``/``tail`` reading a named file rather than a piped stream.

    A piped ``<cmd> | head -5`` uses head/tail as a stream *sink* to cap a pipe's output —
    the sanctioned bound for THIS hook — so a pipe or redirect is left alone here. (The pipe
    *source* stands on its own: an ``rg …`` heading such a pipe is now gated by
    ``search_guards``; this hook judges only the head/tail sink.) Only when head/tail's argv
    names a file operand is it dumping that file: ``head`` with a line count rewrites to
    a bounded ``ccx code read --section``, while ``tail`` (no start line is knowable) and
    the byte-mode (`-c`) or multi-file forms hard-block.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # A piped head/tail bounds a stream — leave it; only a named file operand fires.
        if cl.q.uses_redirect():
            return False
        parsed = _headtail_parse(cl)
        return parsed is not None and bool(parsed[3])


def _int_or_none(s: str) -> int | None:
    return int(s) if s.isdigit() else None


def _headtail_parse(cl: CommandLine) -> tuple[str, str, int | None, list[str]] | None:
    exe = cl.primary.executable
    if exe not in ("head", "tail"):
        return None
    mode = "line"
    count: int | None = None
    files: list[str] = []
    args = cl.primary.args
    i = 0
    while i < len(args):
        a = args[i]
        if a in ("-c", "--bytes"):
            mode = "byte"
            i += 2
        elif a.startswith(("--bytes=", "-c")):
            mode = "byte"
            i += 1
        elif a in ("-n", "--lines"):
            count = _int_or_none(args[i + 1].lstrip("+")) if i + 1 < len(args) else None
            i += 2
        elif a.startswith("--lines="):
            count = _int_or_none(a.split("=", 1)[1].lstrip("+"))
            i += 1
        elif a.startswith("-n"):
            count = _int_or_none(a[2:].lstrip("=+"))
            i += 1
        elif re.fullmatch(r"-\d+", a):
            count = int(a[1:])
            i += 1
        elif a.startswith("-"):
            i += 1
        else:
            files.append(a)
            i += 1
    return exe, mode, count, files


def _headtail_to(evt: BaseHookEvent) -> str | None:
    exe, mode, count, files = _headtail_parse(evt.command_line)
    if exe == "head" and mode == "line" and len(files) == 1 and (ccx := ccx_bin()):
        n = count if count is not None else 10
        return f"{ccx} code read {shlex.quote(files[0])} --section 1-{n}"
    return None


def _headtail_note(evt: BaseHookEvent) -> str:
    _exe, _mode, count, files = _headtail_parse(evt.command_line)
    n = count if count is not None else 10
    return f"Rewrote `head {files[0]}` → `ccx code read --section 1-{n}`: same lines, token-bounded."


rewrite_command(
    only_if=[HeadTailFile()],
    to=_headtail_to,
    block=(
        "BLOCKED: `head`/`tail` on a file dumps raw lines into context. "
        "Use `ccx code read <file> --section A-B` for a bounded range, or `ccx code outline <file>` "
        "to map it — the outline's line numbers show where the file ends, so you can bound a tail "
        "(or the mcp__cc-context__ccx_code_read/ccx_code_outline tools). "
        "Escape hatch — bounding a pipe's output: keep `<cmd> | head`/`| tail`."
    ),
    note=_headtail_note,
    tests={
        Input(command="head -40 f.go"): Rewrite(pattern="code read f.go --section 1-40"),
        Input(command="head -n 40 f.go"): Rewrite(pattern="--section 1-40"),
        Input(command="head f.go"): Rewrite(pattern="--section 1-10"),  # head defaults to 10 lines
        Input(command="tail -n 20 f.go"): Block(pattern="ccx code outline"),
        Input(command="tail -20 f.go"): Block(pattern="ccx code read"),
        Input(command="head -c 100 f.go"): Block(pattern="ccx code outline"),  # byte mode: no line math
        Input(command="head a.go b.go"): Block(pattern="ccx code read"),  # multi-file
        Input(command="rg foo | head -5"): Allow(),  # head-as-sink is fine for THIS hook; the rg source is gated by search_guards
        Input(command="cat f | tail -20"): Allow(),  # pipe sink — sanctioned
        Input(command="head -5"): Allow(),  # reads stdin, no file operand
        # `ccx exec` pass-through is deliberate: head inside sh() is the script's business.
        Input(
            command="ccx exec 'async def main(): return await sh(\"head -40 f.go\")\nasyncio.run(main())'"
        ): Allow(),
    },
)


def _is_root_manifest(path: str) -> bool:
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
        return len(a) == 1 and not a[0].startswith("-") and _is_root_manifest(a[0])


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
    single `--full` target) stay a hard block.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        # Heredocs (`cat << EOF`) and redirects/pipes are streaming/writing uses.
        if "<<" in (evt.command or "") or cl.q.uses_redirect():
            return False
        if not (cl.q.runs("cat") and bool(a := cl.primary.args) and not a[0].startswith("-")):
            return False
        # A repo-root manifest gets the `ccx repo overview` steer (ManifestCat), not a raw read.
        return not (len(a) == 1 and _is_root_manifest(a[0]))


def _cat_to(evt: BaseHookEvent) -> str | None:
    files = evt.command_line.primary.args
    if len(files) == 1 and (ccx := ccx_bin()):
        return f"{ccx} code read {shlex.quote(files[0])} --full"
    return None


def _cat_note(evt: BaseHookEvent) -> str:
    file = evt.command_line.primary.args[0]
    return f"Rewrote `cat {file}` → `ccx code read --full`: same content, token-bounded."


rewrite_command(
    only_if=[BareCat()],
    to=_cat_to,
    block=(
        "BLOCKED: bare `cat <file>` dumps the whole file into context. "
        "Use `ccx code outline <file>` to map it, then `ccx code read <file> --section A-B` for the part "
        "you need (or the mcp__cc-context__ccx_code_outline/ccx_code_read tools). "
        "Escape hatch — whole file: `ccx code read <file> --full`."
    ),
    note=_cat_note,
    tests={
        Input(command="cat main.go"): Rewrite(pattern="code read main.go --full"),
        Input(command="cat a.go b.go"): Block(pattern="ccx code outline"),
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


class LsRecursive(CustomCommandLineCondition):
    """Matches ``ls -R [dir]`` — a recursive listing that walks the whole tree.

    Plain `ls` and `ls -la` stay allowed; only a recursive flag (`-R`, bundled like
    `-laR`, or `--recursive`) matches. The optional directory argument becomes the
    `ccx repo find "<dir>/**"` glob root, defaulting to `**`.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return cl.q.runs("ls") and any(
            x == "--recursive" or (x.startswith("-") and not x.startswith("--") and "R" in x)
            for x in cl.primary.args
        )


def _ls_glob(evt: BaseHookEvent) -> str:
    dirs = [a for a in evt.command_line.primary.args if not a.startswith("-")]
    return f"{dirs[0].rstrip('/')}/**" if dirs else "**"


def _ls_to(evt: BaseHookEvent) -> str | None:
    if ccx := ccx_bin():
        return f'{ccx} repo find "{_ls_glob(evt)}"'
    return None


def _ls_note(evt: BaseHookEvent) -> str:
    glob = _ls_glob(evt)
    return f'Rewrote `ls -R` → `ccx repo find "{glob}"`: same paths, token-bounded.'


rewrite_command(
    only_if=[LsRecursive()],
    to=_ls_to,
    block=(
        "BLOCKED: `ls -R` walks the whole tree into context. "
        'Use `ccx repo find "<glob>"` (or mcp__cc-context__ccx_repo_find), or the built-in Glob tool, '
        "to find paths by pattern. Plain `ls` and `ls -la` stay allowed."
    ),
    note=_ls_note,
    tests={
        Input(command="ls -R"): Rewrite(pattern='repo find "**"'),
        Input(command="ls -laR src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls -R src"): Rewrite(pattern='repo find "src/**"'),
        Input(command="ls --recursive"): Rewrite(pattern='repo find "**"'),
        Input(command="ls -la"): Allow(),
        Input(command="ls"): Allow(),
    },
)


class FindEnumeration(CustomCommandLineCondition):
    """Matches ``find`` used to *list* matches (no action flag) — a context flood.

    A ``-name``/``-iname``/``-path``/``-regex`` filter or a bare ``-type f`` walk both
    enumerate paths; a find that ends in an action — `-exec`, `-delete`, `-print0`
    (almost always `| xargs`) — is doing work, not flooding, so it is left alone. The
    steer is `ccx repo find` (scoped) or `ccx repo overview` (whole repo) / Glob.
    """

    ACTIONS = ("-exec", "-execdir", "-delete", "-print0", "-ok")
    NAME_FLAGS = ("-name", "-iname")

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        if not cl.q.runs("find") or cl.q.uses_redirect():
            return False
        if any(cl.q.contains_token(a) for a in self.ACTIONS):
            return False
        has_filter = any(cl.q.contains_token(f) for f in ("-name", "-iname", "-path", "-regex"))
        return has_filter or _args_type_f(cl.primary.args)


def _args_type_f(args: tuple[str, ...]) -> bool:
    return any(a == "-type" and i + 1 < len(args) and args[i + 1] == "f" for i, a in enumerate(args))


def _find_glob(args: tuple[str, ...]) -> str | None:
    """The ``ccx repo find`` glob body for an enumeration, or None to hard-block.

    A ``-name``/``-iname`` filter maps to `<dir>/**/<pat>`; a bare ``-type f`` walk to
    `<dir>/**`. A ``-path``/``-regex`` filter (no glob equivalent) and a ``-type f`` with
    no dir (orient the repo instead) both return None.
    """
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    if flag:
        path = args[0] if args and not args[0].startswith("-") else "."
        prefix = "" if path == "." else f"{path.rstrip('/')}/"
        return f"{prefix}**/{args[args.index(flag) + 1]}"
    if _args_type_f(args):
        path = args[0] if args and not args[0].startswith("-") else None
        if path is None:
            return None
        prefix = "" if path == "." else f"{path.rstrip('/')}/"
        return f"{prefix}**"
    return None


def _find_to(evt: BaseHookEvent) -> str | None:
    glob = _find_glob(evt.command_line.primary.args)
    if glob is not None and (ccx := ccx_bin()):
        return f'{ccx} repo find "{glob}"'
    return None


def _find_note(evt: BaseHookEvent) -> str:
    args = evt.command_line.primary.args
    flag = next((a for a in args if a in FindEnumeration.NAME_FLAGS), None)
    src = f"{flag} {args[args.index(flag) + 1]}" if flag else "-type f"
    path = args[0] if args and not args[0].startswith("-") else "."
    return f'Rewrote `find {path} {src}` → `ccx repo find "{_find_glob(args)}"`: same paths, token-bounded.'


rewrite_command(
    only_if=[FindEnumeration()],
    to=_find_to,
    block=(
        "BLOCKED: `find` enumeration floods context. "
        'Scoped to a dir? `ccx repo find "<dir>/**"` (or mcp__cc-context__ccx_repo_find), or the built-in '
        "Glob tool. Orienting the whole repo? `ccx repo overview`. "
        "Escape hatch — need an action: keep the `-exec`/`-delete`/`-print0 | xargs` form."
    ),
    note=_find_note,
    tests={
        Input(command="find . -name '*.go'"): Rewrite(pattern='repo find "**/*.go"'),
        Input(command="find src -iname '*.PY'"): Rewrite(pattern='repo find "src/**/*.PY"'),
        Input(command="find src -type f"): Rewrite(pattern='repo find "src/**"'),  # bare -type f, scoped
        Input(command="find . -type f"): Rewrite(pattern='repo find "**"'),
        Input(command="find -type f"): Block(pattern="ccx repo overview"),  # no dir → orient
        Input(command="find . -path '*/gen/*'"): Block(pattern="ccx repo find"),  # -path stays a block
        Input(command="find . -regex '.*\\.go'"): Block(),  # -regex stays a block
        Input(command="find . -name '*.go' -exec rm {} +"): Allow(),
        Input(command="find . -name '*.go' -delete"): Allow(),
        Input(command="find . -name '*.go' -print0 | xargs rm"): Allow(),
        Input(command="find . -type d"): Allow(),  # -type d is not the file flood we steer
    },
)


class LsWorkspaceRoot(CustomCommandLineCondition):
    """Matches ``ls`` of a workspace or Go module-cache root — a huge, noisy listing.

    ``ls ~/Code``, ``ls $HOME/Code``, and ``ls ~/go/pkg/mod/...`` dump every sibling repo
    or the whole module cache into context; the move is to resolve the one repo/module by
    name. Plain ``ls`` and ``ls <subdir>`` inside a project stay allowed, flags and all.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return cl.q.runs("ls") and any(_is_scan_root(a) for a in cl.primary.args if not a.startswith("-"))


def _is_scan_root(path: str) -> bool:
    return bool(WORKSPACE_ROOT.match(path)) or "go/pkg/mod" in path


rewrite_command(
    only_if=[LsWorkspaceRoot()],
    to=lambda evt: None,
    block=(
        "BLOCKED: `ls` of a workspace or module-cache root floods context. "
        "Locating a repo/module? `ccx repo locate <name>`. "
        "Orienting a project? `ccx repo overview` (or mcp__cc-context__ccx_repo_overview). "
        "Plain `ls` and `ls <subdir>` inside a project stay allowed."
    ),
    tests={
        Input(command="ls ~/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls $HOME/Code"): Block(pattern="ccx repo locate"),
        Input(command="ls -la ~/Code"): Block(pattern="ccx repo overview"),
        Input(command="ls ~/go/pkg/mod/github.com/foo"): Block(pattern="ccx repo locate"),
        Input(command="ls internal"): Allow(),  # a project subdir
        Input(command="ls"): Allow(),
        Input(command="ls src/Code"): Allow(),  # not the workspace root
    },
)
