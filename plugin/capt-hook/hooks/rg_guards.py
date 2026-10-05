"""Rg guards: steer transcript and dependency searches, rewrite a tree-shaped ``rg`` to ``ccx code grep``."""

from __future__ import annotations

from typing import TYPE_CHECKING

from captain_hook import (
    Allow,
    Annotated,
    Block,
    Call,
    CommandSchema,
    Event,
    Input,
    Operand,
    Option,
    PreToolUseEvent,
    Rewrite,
    Rewritten,
    Tool,
    hook,
    rewrite_command_occurrences,
)

from .common import LITERAL_SAFE, ccx_supports, first_sight
from .search_common import (
    CONTEXT_OPTIONS,
    DEP_STEER,
    EXAMPLE_SESSION,
    TRANSCRIPT_STEER,
    TRANSCRIPT_SUFFIX,
    TRANSCRIPTS,
    GrepCall,
    SearchTargets,
    UnpipedSearch,
    bound_texts,
    build_ccx_grep,
    context_flags,
    forfeits_count,
    forfeits_operand,
    forfeits_substitution,
    glued_value,
    grep_glob,
    resolved_is_dir,
    scratch_tree,
    search_note,
    targets_dependency,
    targets_transcript,
    unparsed,
)

if TYPE_CHECKING:
    from captain_hook import HookResult, WalkContext
    from cc_transcript.command import Occurrence

RG_FLOOD = (
    "Raw `rg` over a directory floods context. "
    "Run `ccx code grep '<text>'` (`--regex` for a pattern), or rg explicit files."
)

RG = CommandSchema(
    "rg",
    operands=(Operand("operands", count="*"),),
    options=(
        Option("ignore_case", ("-i", "--ignore-case"), bool),
        Option("word", ("-w", "--word-regexp"), bool),
        Option("fixed", ("-F", "--fixed-strings"), bool),
        Option("files_with_matches", ("-l", "--files-with-matches"), bool),
        Option("cosmetic", ("-n", "-N", "-s", "-H", "-I", "--line-number", "--no-line-number"), bool),
        Option(
            "unmapped_flag",
            (
                "-S", "-L", "-c", "-o", "-v", "-x", "-u", "-p", "-a", "-h", "-q", "-z", "-0", "-b", "-P", "-U",
                "--count", "--count-matches", "--files-without-match", "--json", "--only-matching",
                "--case-sensitive", "--smart-case", "--with-filename", "--no-filename", "--invert-match",
                "--line-regexp", "--text", "--quiet", "--null", "--pcre2", "--multiline", "--byte-offset",
                "--heading", "--no-heading", "--line-buffered",
            ),
            bool,
        ),
        *CONTEXT_OPTIONS,
        Option("glob", ("-g", "--glob")),
        Option("color", ("--color",)),
        Option("pattern", ("-e", "--regexp")),
        Option("pattern_file", ("-f", "--file")),
        Option(
            "unmapped_value",
            (
                "-t", "-T", "-m", "-r", "-E", "-j", "-M", "-d", "--iglob", "--type", "--type-not", "--max-count",
                "--replace", "--encoding", "--threads", "--max-columns", "--max-depth", "--max-filesize", "--sort",
                "--sortr", "--colors", "--type-add", "--ignore-file",
            ),
        ),
    ),
)

UNMAPPED = frozenset({"unmapped_flag", "unmapped_value", "pattern_file"})

NO_VALUE_FLAGS = frozenset(
    {"--ignore-case", "--word-regexp", "--fixed-strings", "--files-with-matches", "--line-number", "--no-line-number"}
)


def rg_operands(call: Call) -> list[str] | None:
    """An ``rg``'s path operands (the pattern excluded), or ``None`` when an unknown flag stops the binding."""
    arguments = RG.bind(call)
    if unparsed(RG, arguments):
        return None
    positionals = bound_texts(arguments, "operands")
    pattern_from_flag = "pattern" in arguments.values or "pattern_file" in arguments.values
    return positionals if pattern_from_flag else positionals[1:]


def rg_parse(call: Call) -> GrepCall | None:
    """The ccx-rewritable shape of one direct, unpiped, unwrapped ``rg`` call, or ``None``.

    Only a literal pattern maps, and ``-g``/``--glob`` only as a basename glob.
    """
    source = call.occurrence.command
    if call.occurrence.prev_op == "|" or source.env or source.executable != "rg":
        return None
    arguments = RG.bind(call)
    if not arguments.complete or glued_value(call, NO_VALUE_FLAGS) or arguments.values.keys() & UNMAPPED:
        return None
    patterns = bound_texts(arguments, "pattern")
    positionals = bound_texts(arguments, "operands")
    globs = bound_texts(arguments, "glob")
    if len(patterns) > 1 or len(globs) > 1 or any("/" in glob for glob in globs):
        return None
    if patterns:
        pattern, paths = patterns[0], positionals
    elif positionals:
        pattern, paths = positionals[0], positionals[1:]
    else:
        return None
    if pattern.startswith("-") or "+" in pattern or not LITERAL_SAFE.match(pattern):
        return None
    context = context_flags(arguments)
    counts = [count for _, count in context]
    if any(not count.isdigit() for count in counts) or forfeits_count(counts):
        return None
    glob = grep_glob(paths, globs[0] if globs else None, cwd=call.cwd)
    if glob is None:
        return None
    native_context = bool(context) and ccx_supports("code", "grep", flag="--after-context")
    values = arguments.values
    return GrepCall(
        pattern,
        glob,
        "" if native_context or not counts else max(counts, key=int),
        tuple(context) if native_context else (),
        "ignore_case" in values,
        "word" in values,
        "files_with_matches" in values,
        "fixed" in values,
        count_dropped=False,
    )


def rg_tree_shaped(call: Call) -> bool:
    """Whether an rg searches a directory: no operand or a ``.``/``..``/directory operand; an unknown flag runs raw."""
    if (ops := rg_operands(call)) is None:
        return False
    return not ops or any(resolved_is_dir(p, call.cwd) for p in ops)


def rg_to(call: Call) -> str | None:
    return build_ccx_grep(parsed) if (parsed := rg_parse(call)) is not None else None


def rg_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Rewrite a tree-shaped, unpiped rg, block an unmappable one, and run everything else raw.

    Substitutions, expanding operands, oversized counts, and a ``ccx`` too old to express the search all run raw.
    """
    call = Call(evt.cmd, occ, ctx.cwd)
    if call.name != "rg" or occ.prev_op == "|" or occ.next_op == "|":
        return None
    if not rg_tree_shaped(call) or forfeits_substitution(call) or scratch_tree(rg_operands(call) or [], call.cwd):
        return None
    if (ops := rg_operands(call)) and any(forfeits_operand(p) for p in ops):
        return None
    if forfeits_count(call.args):
        return None
    if (parsed := rg_parse(call)) is None:
        return evt.block(RG_FLOOD)
    if (text := build_ccx_grep(parsed)) is None:
        return None
    if ctx.spliceable:
        return Rewritten(text, note=first_sight(evt, search_note(parsed)))
    return evt.block(RG_FLOOD)


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SearchTargets(RG, rg_operands, targets_transcript)],
    skip_if=[Annotated("raw")],
    message=TRANSCRIPT_STEER,
    block=True,
    tests={
        Input(command=f"rg foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"rg --no-ignore foo {TRANSCRIPTS}/ | head"): Block(pattern="cc-transcript"),
        Input(command=f"rg foo {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX}; rg -v bar ."): Block(pattern="cc-transcript"),
        Input(command=f"/opt/homebrew/bin/rg foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"rg -o -m 3 'Exit code' {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX} | head -3"): Block(
            pattern="cc-transcript"
        ),
        Input(command=f"rg foo {TRANSCRIPTS}/proj/*/subagents/*{TRANSCRIPT_SUFFIX}"): Block(pattern="cc-transcript"),
        Input(command=f"rg needle {EXAMPLE_SESSION}/*/agent-a1{TRANSCRIPT_SUFFIX}"): Block(pattern="cc-transcript"),
        Input(command="rg -l x ~/.claude/plugins/"): Allow(),
        Input(command=f"rg foo {TRANSCRIPTS}/ # ccx:raw"): Allow(),
        Input(command=f"echo '# ccx:raw'; rg foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"cat x | rg foo {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX}"): Allow(),
        Input(command=f"rg -o '\"slug\": \"[^\"]*\"' {EXAMPLE_SESSION}/tool-results/bo61h71tu.txt"): Allow(),
        Input(
            command="cat >> ~/.claude/scratch/inbox/orca-desk.md <<'EOF'\n"
            f"- R459: retro, see {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX}\n"
            "EOF\n"
            f'rg -n "explicitly dropped" {TRANSCRIPTS}/-Users-me-repo/memory/release-v3.md'
        ): Allow(),
    },
)

hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SearchTargets(RG, rg_operands, targets_dependency)],
    skip_if=[Annotated("raw")],
    message=DEP_STEER,
    block=True,
    tests={
        Input(command="rg x .jj/repo | head"): Block(pattern="dep-reader"),
        Input(command="rg --hidden needle .venv/ | head"): Block(pattern="dep-reader"),
        Input(
            command='rg -n "class ToolUse" .venv/lib/python3.13/site-packages/cc_transcript/ -A 20 | head -40'
        ): Block(pattern="dep-reader"),
        Input(command="rg --hidden -g '*.py' needle node_modules/express | head"): Block(pattern="dep-reader"),
        Input(command="rg --hidden --glob 'node_modules/**' needle ."): Block(pattern="dep-reader"),
        Input(command="rg -n foo . | rg -v node_modules"): Allow(),
        Input(command="rg -n foo . | rg -P node_modules"): Allow(),
        Input(command="rg x .jj/repo | head # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; rg x .jj/repo | head"): Block(pattern="dep-reader"),
        Input(command="rg -n -l \"gpt-5.6-sol\" --hidden -g '!**/node_modules/**' . | head -20"): Allow(),
        Input(
            command="rg -n --no-messages -g '!**/node_modules/**' -g '!**/target/**' -e 'no_watch' "
            "--max-count 3 cc-skills/plugins 2>/dev/null | head"
        ): Allow(),
    },
)

rewrite_command_occurrences(
    only_if=[UnpipedSearch("rg")],
    skip_if=[Annotated("raw")],
    visit=rg_visit,
    tests={
        Input(command="rg foo"): Rewrite(pattern="code grep foo"),
        Input(command="rg -n foo"): Rewrite(pattern="code grep foo"),
        Input(command="rg -nl foo"): Rewrite(pattern="code grep foo"),
        Input(command="rg -F foo"): Rewrite(pattern="code grep foo"),
        Input(command="rg -in foo"): Rewrite(pattern="-i"),
        Input(command="rg -g '*.go' foo"): Rewrite(pattern="--glob '*.go'"),
        Input(command="rg -C 3 foo"): Rewrite(pattern="-C=3"),
        Input(command="rg -A 20 foo"): Rewrite(pattern="-A=20"),
        Input(command="rg -A 2 -B 5 TODO"): Rewrite(pattern="-A=2 -B=5"),
        Input(command="rg -B 5 -A 2 TODO"): Rewrite(pattern="-B=5 -A=2"),
        Input(command="rg --color always plugin"): Rewrite(pattern="code grep plugin"),
        Input(command="rg --color=always plugin"): Rewrite(pattern="code grep plugin"),
        Input(command="printf 'left  side'; rg foo"): Rewrite(pattern="printf 'left  side'; "),
        Input(command="rg 'foo.*' ."): Block(pattern="ccx code grep"),
        Input(command="rg -t py foo"): Block(pattern="ccx code grep"),
        Input(command="rg -P 'x(?=y)' ."): Block(),
        Input(command="rg -U foo ."): Block(),
        Input(command="rg -uu foo"): Block(),
        Input(command="rg foo # ccx:raw"): Allow(),
        Input(command="rg -v bar . # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; rg foo"): Rewrite(pattern="code grep foo"),
        Input(command="rg -r repl foo"): Block(),
        Input(command="rg -e a -e b ."): Block(),
        Input(command="rg -m 5 foo ."): Block(),
        Input(command="rg -d 1 app.log"): Block(),
        Input(command="rg --files-with-matches=oops foo"): Block(),
        Input(command="rg --no-ignore foo ."): Allow(),
        Input(command="rg --no-ignore plugin README.md"): Allow(),
        Input(command="RIPGREP_CONFIG_PATH=rg.conf rg foo ."): Block(),
        Input(command="rg foo /etc/hosts"): Allow(),
        Input(command="rg foo app.log"): Allow(),
        Input(command="rg -c foo app.log"): Allow(),
        Input(command="rg fo+ file.py"): Allow(),
        Input(command="rg foo ~/notes.md"): Allow(),
        Input(command="rg -n foo $d/host.go"): Allow(),
        Input(command="rg foo $d/app.log"): Allow(),
        Input(command="rg foo data.json config.yaml"): Allow(),
        Input(command="rg -o 'err.*timeout' server.log"): Allow(),
        Input(command="rg --files"): Allow(),
        Input(command="rg foo file.py | wc -l"): Allow(),
        Input(command="rg foo | sed -n '1,20p'"): Allow(),
        Input(command="rg foo | sed '1,20p'"): Allow(),
        Input(command="rg -l foo | cat"): Allow(),
        Input(command="rg -n foo src/ -A 20 | head -40"): Allow(),
        Input(command="rg --no-ignore foo . | head"): Allow(),
        Input(command="rg --fixed-strings foo internal/ | head"): Allow(),
        Input(command="rg --line-number foo internal/ | head"): Allow(),
        Input(command="rg --smart-case foo . | head -20"): Allow(),
        Input(command="rg foo logs/app.log | head -5"): Allow(),
        Input(command="rg -n foo . | rg -v node_modules"): Allow(),
        Input(command="cat f | rg foo"): Allow(),
        Input(command="journalctl | rg err | head -5"): Allow(),
        Input(command="rg -c foo"): Block(),
        Input(command="rg -c foo data.json && rg bar ."): Rewrite(pattern="rg -c foo data.json && "),
        Input(command="rg -c foo . && rg bar ."): Block(),
        Input(command="rg -v bar ."): Block(),
        Input(command="rg foo $(printf /tmp/target)"): Allow(),
        Input(command="rg -n foo `printf x`"): Allow(),
        Input(command="rg foo $(printf /tmp/t); rg bar ."): Rewrite(pattern="rg foo $(printf /tmp/t); "),
        Input(command="rg foo . $d"): Allow(),
        Input(command="rg -A " + "9" * 5000 + " -B 1 needle"): Allow(),
        Input(command="sudo rg foo ."): Block(),
        Input(command="/opt/homebrew/bin/rg foo ."): Block(),
        Input(command='"rg" foo .'): Block(),
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
