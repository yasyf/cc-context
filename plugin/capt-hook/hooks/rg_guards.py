"""Rg guard: rewrite a tree-shaped ``rg`` file search to ``ccx code grep``, block the unmappable rest; everything else runs raw."""

from __future__ import annotations

from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    BaseHookEvent,
    Block,
    CommandLine,
    CustomCommandLineCondition,
    Input,
    PreToolUseEvent,
    Rewrite,
    Rewritten,
    rewrite_command_occurrences,
)

from .common import LITERAL_SAFE, ccx_supports
from .search_common import (
    CONTEXT_SHORT,
    DEP_STEER,
    GrepCall,
    build_ccx_grep,
    forfeits_count,
    forfeits_operand,
    grep_glob,
    has_command_substitution,
    has_hidden_segment,
    is_transcript_path,
    note_text,
    path_operands_raw,
    resolved_is_dir,
    search_block,
    unquote,
)

if TYPE_CHECKING:
    from pathlib import Path

    from captain_hook import HookResult, WalkContext
    from cc_transcript.command import Command, Occurrence


# ripgrep's own DROP table — never grep's: rg's short flags are false friends (`-r` takes a
# value, `-E` is encoding, `-I` is no-filename). `-n`/`-N`/`-s`/`-H`/`-I` are cosmetic; `-l`
# (files-with-matches) and `-F` (fixed-strings) disclose their drop through the note.
RG_DROP_SHORT = frozenset("nNsHIlF")

# rg flags whose next token is a value, for the tolerant `rg_operands` walk (separate from the
# strict rewrite parser). `-e`/`-f`/`--regexp`/`--file` supply the pattern and are handled apart.
RG_OP_VALUE_SHORT = frozenset("gtTABCmrEjMd")

# rg's boolean short flags — they take no value, so `rg_operands` may skip one (or an all-boolean
# bundle) without consuming the next token. A short outside this set ∪ RG_OP_VALUE_SHORT is unknown
# and makes `rg_operands` return None (`-d 1` is max-depth: its `1` must not leak in as a phantom pattern).
RG_BOOLEAN_SHORT = frozenset("iwnNsSHIlLcovxFupahqz0")

RG_BOOLEAN_LONG = frozenset(
    {
        "count",
        "count-matches",
        "files-with-matches",
        "files-without-match",
        "json",
        "only-matching",
        # Cosmetic/match-mode booleans (long spellings of RG_BOOLEAN_SHORT). The flood family
        # (`--hidden`, `--no-ignore*`, `--unrestricted`) stays absent — under fail-open an unknown
        # flag makes `rg_operands` return None, so the raw dir scan then decides the shape.
        "ignore-case",
        "word-regexp",
        "line-number",
        "no-line-number",
        "case-sensitive",
        "smart-case",
        "with-filename",
        "no-filename",
        "invert-match",
        "line-regexp",
        "fixed-strings",
        "text",
        "quiet",
        "null",
        "heading",
        "no-heading",
        "line-buffered",
    }
)

# Long flags rg's rewrite parser reads that take no value: rg errors on `--files-with-matches=oops`,
# so an attached value declines the rewrite instead of silently discarding it.
RG_NO_VALUE_LONG = frozenset(
    {"ignore-case", "word-regexp", "fixed-strings", "files-with-matches", "line-number", "no-line-number"}
)

RG_OP_VALUE_LONG = frozenset(
    {
        "glob",
        "iglob",
        "type",
        "type-not",
        "after-context",
        "before-context",
        "context",
        "max-count",
        "replace",
        "encoding",
        "threads",
        "max-columns",
        "max-depth",
        "max-filesize",
        "sort",
        "sortr",
        "color",
        "colors",
        "type-add",
        "ignore-file",
    }
)


def rg_operands(cmd: Command) -> list[str] | None:
    """Extract an ``rg``'s explicit path operands (the pattern excluded), or ``None`` when unparseable.

    A tolerant walk: it separates path operands from the pattern and consumes the values of known
    value-taking flags; ``-e``/``-f``/``--regexp``/``--file`` mark that the pattern came from a flag
    (so no positional is the pattern). It feeds the policy steers (transcript / hidden segment) and
    tree-shape detection. Any unrecognized long or short flag returns ``None`` — the steers skip and
    shape detection falls back to a raw dir scan, never a wrong block.
    """
    args = cmd.args
    positionals: list[str] = []
    pattern_from_flag = False
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a == "-" or not a.startswith("-"):
            positionals.append(a)
            i += 1
            continue
        if a.startswith("--"):
            name, sep, _ = a[2:].partition("=")
            if name in ("regexp", "file"):
                pattern_from_flag = True
                if not sep:
                    i += 1
            elif name in RG_OP_VALUE_LONG:
                if not sep:
                    i += 1
            elif name not in RG_BOOLEAN_LONG:
                return None
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in ("e", "f"):
            pattern_from_flag = True
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif head in RG_OP_VALUE_SHORT:
            if len(a) == 2 and i + 1 < n:
                i += 1
        elif not all(ch in RG_BOOLEAN_SHORT for ch in body):
            return None  # unknown short or bundle with a non-boolean char → stay gated
        i += 1
    if not pattern_from_flag and positionals:
        return positionals[1:]
    return positionals


def fold_expand(current: str, cand: str) -> str | None:
    """Fold a context count into the old-binary ``--expand`` fallback max, or ``None`` when unparseable.

    A count past Python's int-string conversion limit (a pathological 5000-digit ``-A``) makes ``int()``
    raise; the fold returns ``None`` so the caller forfeits the rewrite rather than crash on it.
    """
    if not current:
        return cand
    try:
        return str(max(int(current), int(cand)))
    except ValueError:
        return None


def rg_parse(occ: Occurrence, *, cwd: Path | None = None) -> GrepCall | None:
    """Parse one direct, unpiped ``rg`` occurrence into its ccx-rewritable shape.

    Mirrors :func:`~hooks.grep_guards.grep_parse` over ripgrep's grammar with its own DROP table
    (:data:`RG_DROP_SHORT`). ``-A/-B/-C``/their long forms map to the same native ccx flags when
    available, with ``--expand`` retained for old binaries; ``-g/--glob`` fills the include slot,
    gated to a slash-free basename glob (rg globs are gitignore-style — only a basename composes
    faithfully). Any other long flag, a repeated ``-e``, a value-taking short, a regex pattern, or an
    out-of-repo path (via :func:`grep_glob`) declines the rewrite.
    """
    cmd = occ.command
    if occ.prev_op == "|" or cmd.executable != "rg" or cmd.env:
        return None
    args = cmd.args
    pattern: str | None = None
    e_count = 0
    include: str | None = None
    positionals: list[str] = []
    expand = ""
    context_args: list[tuple[str, str]] = []
    ignore_case = word = dropped_l = dropped_fixed = False
    i, n = 0, len(args)
    while i < n:
        a = args[i]
        if a == "--":
            positionals.extend(args[i + 1 :])
            break
        if a == "-" or not a.startswith("-"):
            positionals.append(a)
            i += 1
            continue
        if a.startswith("--"):
            name, sep, val = a[2:].partition("=")
            if sep and name in RG_NO_VALUE_LONG:
                return None  # rg rejects `--files-with-matches=oops` — never rewrite past its error
            if name == "ignore-case":
                ignore_case = True
            elif name == "word-regexp":
                word = True
            elif name in ("after-context", "before-context", "context"):
                if sep:
                    if not unquote(val).isdigit():
                        return None
                    cand = unquote(val)
                elif i + 1 < n and args[i + 1].isdigit():
                    cand = args[i + 1]
                    i += 1
                else:
                    return None
                context_args.append((f"--{name}", cand))
                if (expand := fold_expand(expand, cand)) is None:
                    return None
            elif name == "glob":
                if include is not None:
                    return None
                if sep:
                    include = unquote(val)
                elif i + 1 < n:
                    include = args[i + 1]
                    i += 1
                else:
                    return None
                if "/" in include:
                    return None
            elif name == "regexp":
                e_count += 1
                if sep:
                    pattern = unquote(val)
                elif i + 1 < n:
                    pattern = args[i + 1]
                    i += 1
                else:
                    return None
            elif name == "fixed-strings":
                dropped_fixed = True
            elif name == "files-with-matches":
                dropped_l = True
            elif name in ("line-number", "no-line-number"):
                pass
            elif name in ("color", "colour"):
                # rg's --color requires a value; consume the space-form token so it can't leak
                # into the positionals as a phantom pattern (the =glued form is already whole).
                if not sep and i + 1 < n:
                    i += 1
            else:
                return None
            i += 1
            continue
        body = a[1:]
        head = body[0]
        if head in CONTEXT_SHORT:
            if len(body) > 1:
                if not unquote(body[1:]).isdigit():
                    return None
                cand = unquote(body[1:])
            elif i + 1 < n and args[i + 1].isdigit():
                cand = args[i + 1]
                i += 1
            else:
                return None
            context_args.append((f"-{head}", cand))
            if (expand := fold_expand(expand, cand)) is None:
                return None
        elif head == "e":
            e_count += 1
            if len(body) > 1:
                pattern = unquote(body[1:])
            elif i + 1 < n:
                pattern = args[i + 1]
                i += 1
            else:
                return None
        elif head == "g":
            if include is not None:
                return None
            if len(body) > 1:
                include = unquote(body[1:])
            elif i + 1 < n:
                include = args[i + 1]
                i += 1
            else:
                return None
            if "/" in include:
                return None
        elif len(body) == 1:
            if head == "i":
                ignore_case = True
            elif head == "w":
                word = True
            elif head in RG_DROP_SHORT:
                dropped_l = dropped_l or head == "l"
                dropped_fixed = dropped_fixed or head == "F"
            else:
                return None  # -o -v -c -u -E -r -t -m -j -M …
        elif all(ch in RG_DROP_SHORT or ch in ("i", "w") for ch in body):
            dropped_l = dropped_l or "l" in body
            dropped_fixed = dropped_fixed or "F" in body
            ignore_case = ignore_case or "i" in body
            word = word or "w" in body
        else:
            return None  # a bundle carrying a non-DROP, non-MAP char (value short)
        i += 1
    if e_count > 1:
        return None
    if pattern is None:
        if not positionals:
            return None
        pattern, paths = positionals[0], positionals[1:]
    else:
        paths = positionals
    # rg's default engine reads `+` as a quantifier, so a literal ccx rewrite would under-match;
    # grep's BRE `+` is already literal, so this rejection is rg-only (LITERAL_SAFE admits `+`).
    if pattern.startswith("-") or "+" in pattern or not LITERAL_SAFE.match(pattern):
        return None
    glob = grep_glob(paths, include, cwd=cwd)
    if glob is None:
        return None
    native_context = bool(context_args) and ccx_supports("code", "grep", flag="--after-context")
    return GrepCall(
        pattern,
        glob,
        "" if native_context else expand,
        tuple(context_args) if native_context else (),
        ignore_case,
        word,
        dropped_l,
        dropped_fixed,
        count_dropped=False,
    )


def rg_tree_shaped(cmd: Command, *, cwd: Path | None) -> bool:
    """Whether an rg is a directory-wide flood — the one positive shape the block fires on.

    rg recurses by default, so a no-operand rg (recurses the cwd) or any ``.``/``..``/directory operand
    is tree-shaped; an explicit file or an unstattable ``$VAR``/missing operand runs raw. rg has no
    recursive flag, so when :func:`rg_operands` cannot map an unknown flag it returns ``None`` and the
    shape is unknown → not tree-shaped (allow): a raw-token stat would wrongly block a bounded
    explicit-file search (``rg --no-ignore plugin README.md``) whose pattern happens to name a real dir.
    """
    ops = rg_operands(cmd)
    if ops is None:
        return False
    return not ops or any(resolved_is_dir(p, cwd) for p in ops)


def rg_to(occ: Occurrence, *, cwd: Path | None = None) -> str | None:
    """The ``ccx code grep`` rewrite for an rg occurrence, or ``None`` when the emitter cannot map it."""
    parsed = rg_parse(occ, cwd=cwd)
    return build_ccx_grep(parsed) if parsed is not None else None


class RgFlood(CustomCommandLineCondition):
    """Match a line carrying any unpiped ``rg`` occurrence.

    A cheap structural gate; :func:`rg_visit` is authoritative, returning a per-occurrence verdict.
    Matching through ``cmd.unwrapped`` keeps wrapper prefixes transparent; a pure ``… | rg`` filter (no
    unpiped rg) stays outside the registration entirely.
    """

    def check_command_line(self, evt: BaseHookEvent, cl: CommandLine) -> bool:
        return any(occ.command.unwrapped.executable == "rg" and occ.prev_op != "|" for occ in cl.occurrences)


def rg_block(evt: PreToolUseEvent, cl: CommandLine) -> str:
    return search_block(
        evt,
        "rg",
        rg_operands,
        "BLOCKED: raw `rg` for a tree-wide file search floods context. "
        "Use `ccx code grep <text>` for literal text, "
        '`ccx code search "<question>"` for intent, `ccx repo find "<glob>"` to list files. '
        "Several terms? One call covers them: `ccx code grep 'a|b|c' --regex`. "
        "Dependency source (`.venv`, vendored pkgs): spawn the `cc-context:dep-reader` agent — cited "
        "conclusions, never the source; inline `ccx repo locate <pkg>` (CLI-only) then `ccx code grep`. "
        "Simple tree rg auto-rewrites to `ccx code grep`; this one didn't map — a regex pattern, an "
        "unmappable flag (`-t`/`-r`/`--no-ignore`/…), an env prefix, or an out-of-repo target. "
        "Escape hatch: pipe input into it (`… | rg`), or name explicit files.",
        cl=cl,
    )


def rg_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Per-occurrence verdict under the fail-open doctrine.

    Mirrors :func:`~hooks.grep_guards.grep_visit`. Non-rg occurrences pass. The policy steers scan the raw
    path-like tokens (:func:`path_operands_raw`), so an unparseable flag can't blind them: a transcript
    operand blocks with the cc-transcript steer, a hidden-segment operand (``.venv/…``) with the
    dep-reader steer — both fire even through pipes and may over-match. An rg consuming or feeding a pipe
    runs verbatim. An rg that is not tree-shaped runs raw. A tree-shaped rg whose raw text carries a
    ``$(…)``/backtick substitution runs raw. A path operand carrying a shell expansion or glob metachar,
    or a context count past Python's int-string limit, forfeits the rewrite and runs raw. Otherwise an
    unmappable shape blocks with the flood steer, while a mappable shape the local ``ccx`` binary is too
    old (or absent) to emit runs raw — infra unavailability never blocks. A block rides a
    :class:`HookResult` that aborts the walk, discarding any sibling rewrite.
    """
    inner = occ.command.unwrapped
    if inner.executable != "rg":
        return None
    raw_ops = path_operands_raw(inner.args)
    if any(is_transcript_path(p) for p in raw_ops):
        return evt.block(rg_block(evt, evt.cmd.line))
    if any(has_hidden_segment(p) for p in raw_ops):
        return evt.block(DEP_STEER)
    if occ.prev_op == "|" or occ.next_op == "|":
        return None
    if not rg_tree_shaped(inner, cwd=ctx.cwd):
        return None
    if has_command_substitution(occ.command.raw):
        return None
    if ((ops := rg_operands(inner)) and any(forfeits_operand(p) for p in ops)) or forfeits_count(inner.args):
        return None
    parsed = rg_parse(occ, cwd=ctx.cwd)
    if parsed is None:
        return evt.block(rg_block(evt, evt.cmd.line))
    text = build_ccx_grep(parsed)
    if text is None:
        return None
    if ctx.spliceable:
        return Rewritten(text, note=note_text(occ.command.raw, parsed))
    return evt.block(rg_block(evt, evt.cmd.line))


rewrite_command_occurrences(
    only_if=[RgFlood()],
    visit=rg_visit,
    tests={
        # Rewrite — tree-shaped (rg recurses by default, so a no-operand rg is tree-shaped; `.` too).
        # Path→glob shapes classify each operand against the filesystem, so they live in test_rg_guards.py.
        Input(command="rg foo"): Rewrite(pattern="code grep foo"),  # no path → repo-wide
        Input(command="rg -n foo"): Rewrite(pattern="code grep foo"),  # -n cosmetic
        Input(command="rg -nl foo"): Rewrite(pattern="code grep foo"),  # probe-specific -l suffix lives in pytest
        Input(command="rg -F foo"): Rewrite(pattern="code grep foo"),  # -F disclosed (ccx matches literally)
        Input(command="rg -in foo"): Rewrite(pattern="-i"),  # MAP `i` bundled with cosmetic `n` → mapped
        Input(command="rg -g '*.go' foo"): Rewrite(pattern="--glob '*.go'"),  # basename glob → include
        Input(command="rg -C 3 foo"): Rewrite(pattern="-C=3"),  # native context count preserved
        Input(command="rg -A 20 foo"): Rewrite(pattern="-A=20"),  # native context count preserved
        Input(command="rg -A 2 -B 5 TODO"): Rewrite(pattern="-A=2 -B=5"),  # native flags preserve both counts
        Input(command="rg -B 5 -A 2 TODO"): Rewrite(pattern="-B=5 -A=2"),  # native flag order preserved
        Input(command="rg --color always plugin"): Rewrite(pattern="code grep plugin"),  # --color eats its space-form value
        Input(command="rg --color=always plugin"): Rewrite(pattern="code grep plugin"),  # =glued --color still no-ops
        Input(command="printf 'left  side'; rg foo"): Rewrite(pattern="printf 'left  side'; "),
        # Block — tree-shaped (no operand, or `.`) but the emitter can't map the flag/pattern:
        Input(command="rg 'foo.*' ."): Block(),  # regex-metachar pattern (LITERAL_SAFE)
        Input(command="rg -t py foo"): Block(),  # -t takes a value (false friend of grep)
        Input(command="rg -uu foo"): Block(pattern="ccx repo locate"),  # unrestricted → tree, no map → dep steer in flood msg
        Input(command="rg -r repl foo"): Block(),  # -r takes a value — misparse guard
        Input(command="rg -e a -e b ."): Block(),  # multiple -e
        Input(command="rg -m 5 foo ."): Block(),  # -m takes a value
        Input(command="rg -d 1 app.log"): Block(),  # -d (max-depth) takes a value → app.log is the pattern, recurses cwd
        Input(command="rg --files-with-matches=oops foo"): Block(),  # rg rejects a value on a no-value long
        # Fix 3: an unknown rg flag makes `rg_operands` return None; rg has no recursive flag, so the shape
        # is unknown → not tree-shaped → runs raw. A raw-token stat would wrongly block these.
        Input(command="rg --no-ignore foo ."): Allow(),  # unknown flag → shape unknown → runs raw
        Input(command="rg --no-ignore plugin README.md"): Allow(),  # `plugin` names a real dir, but no stat of raw tokens → runs raw
        Input(command="RIPGREP_CONFIG_PATH=rg.conf rg foo ."): Block(),  # env-prefixed rg over `.` can't rewrite (env unseen)
        # Allow — explicit-file searches and unparseable no-dir shapes run raw:
        Input(command="rg foo /etc/hosts"): Allow(),  # absolute regular file → not tree-shaped
        Input(command="rg foo app.log"): Allow(),  # data-file target runs as-is
        Input(command="rg -c foo app.log"): Allow(),  # count-only over a file → not tree-shaped
        Input(command="rg fo+ file.py"): Allow(),  # single file operand → runs raw (the `+` no longer matters)
        Input(command="rg foo ~/notes.md"): Allow(),  # `~` file operand → runs raw
        Input(command="rg -n foo $d/host.go"): Allow(),  # `$VAR` operand → unstattable → runs raw
        Input(command="rg foo $d/app.log"): Allow(),  # `$VAR` data-file operand → runs raw
        Input(command="rg foo data.json config.yaml"): Allow(),  # two file operands → not tree-shaped
        Input(command="rg -o 'err.*timeout' server.log"): Allow(),  # -o over a single file → not tree-shaped
        Input(command="rg --files"): Allow(),  # `--files` unknown to the arity walk, no dir operand → runs raw
        # Downstream pipe → allow (any pipe is post-processing):
        Input(command="rg foo file.py | wc -l"): Allow(),
        Input(command="rg foo | sed -n '1,20p'"): Allow(),
        Input(command="rg foo | sed '1,20p'"): Allow(),  # unbounded sed no longer matters
        Input(command="rg -l foo | cat"): Allow(),  # non-sink terminal no longer matters
        Input(command="rg -n foo src/ -A 20 | head -40"): Allow(),  # non-hidden target + pipe → runs raw
        # Fix 1: the steers scan the raw path-like tokens, so an unparseable flag can't hide a
        # hidden-segment or transcript operand — both fire even through a downstream pipe.
        Input(command="rg --hidden needle .venv/ | head"): Block(pattern="ccx repo locate"),  # `.venv/` → dep steer
        Input(command="rg --no-ignore foo ~/.claude/projects/ | head"): Block(pattern="cc-transcript"),  # transcript steer
        Input(command="rg --no-ignore foo . | head"): Allow(),  # no hidden/transcript operand + pipe → runs raw
        Input(command="rg --fixed-strings foo internal/ | head"): Allow(),
        Input(command="rg --line-number foo internal/ | head"): Allow(),
        Input(command="rg --smart-case foo . | head -20"): Allow(),
        Input(command="rg foo logs/app.log | head -5"): Allow(),  # data-file head of a pipe
        Input(command="cat f | rg foo"): Allow(),  # piped sink (rg consumes stdin)
        Input(command="journalctl | rg err | head -5"): Allow(),
        # `rg -c foo` (no operand) is still tree-wide → block; the bounded sibling splices in a compound.
        Input(command="rg -c foo"): Block(),  # operand-less count is still tree-wide
        Input(command="rg -c foo data.json && rg bar ."): Rewrite(pattern="rg -c foo data.json && "),
        Input(command="rg -c foo . && rg bar ."): Block(),  # `-c` over `.` floods → vetoes the line
        # Policy steers fire even through a pipe (checked before the pipe):
        Input(command="rg foo ~/.claude/projects/"): Block(pattern="cc-transcript"),  # transcript → cc-transcript steer
        Input(command="rg foo ~/.claude/projects/main.jsonl; rg -v bar ."): Block(pattern="cc-transcript"),  # mixed
        Input(
            command='rg -n "class ToolUse" .venv/lib/python3.13/site-packages/cc_transcript/ -A 20 | head -40'
        ): Block(pattern="ccx repo locate"),  # hidden `.venv` segment → dep steer, even through the pipe
        # Substitution forfeits the rewrite (parser drops the operand); the rest runs raw under fail-open.
        Input(command="rg foo $(printf /tmp/target)"): Allow(),
        Input(command="rg -n foo `printf x`"): Allow(),
        Input(command="rg foo $(printf /tmp/t); rg bar ."): Rewrite(pattern="rg foo $(printf /tmp/t); "),  # sibling splices
        # Fix 5: a `$`/glob operand forfeits the rewrite → runs raw, never a silent scope-widening drop.
        Input(command="rg foo . $d"): Allow(),  # tree via `.`, but `$d` operand → forfeit → runs raw
        # Fix 6: a context count past Python's int-string limit forfeits the rewrite → runs raw, never a crash.
        Input(command="rg -A " + "9" * 5000 + " -B 1 needle"): Allow(),
        # Wrapper transparency (2026-07-17): a wrapped tree rg stays gated (direct-only rewrite).
        Input(command="sudo rg foo ."): Block(),
        # `ccx exec` pass-through is deliberate: sh("rg …") is in-sandbox and budget-capped on return.
        Input(
            command="ccx exec 'async def main(): return await sh(\"rg -n foo src/\")\nasyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("rg -n foo src/")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)
