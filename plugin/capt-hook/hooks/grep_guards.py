"""Grep guards: steer transcript and dependency searches, rewrite a tree-shaped ``grep`` to ``ccx code grep``."""

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
    forfeits_operand,
    forfeits_substitution,
    glued_value,
    grep_glob,
    loose_operands,
    resolve_operand,
    resolved_is_dir,
    scratch_tree,
    search_note,
    targets_dependency,
    targets_transcript,
    unparsed,
    word_text,
)

if TYPE_CHECKING:
    from pathlib import Path

    from captain_hook import HookResult, WalkContext
    from cc_transcript.command import Occurrence

GREP_FLOOD = (
    "Raw `grep` over a directory floods context. "
    "Run `ccx code grep '<text>'` (`--regex` for a pattern), or grep explicit files."
)

GREP = CommandSchema(
    "grep",
    operands=(Operand("operands", count="*"),),
    options=(
        Option("ignore_case", ("-i", "--ignore-case"), bool),
        Option("word", ("-w", "--word-regexp"), bool),
        Option("extended", ("-E", "--extended-regexp"), bool),
        Option("basic", ("-G", "--basic-regexp"), bool),
        Option("fixed", ("-F", "--fixed-strings"), bool),
        Option("files_with_matches", ("-l", "--files-with-matches"), bool),
        Option("recursive", ("-r", "-R", "--recursive", "--dereference-recursive"), bool),
        Option(
            "cosmetic",
            (
                "-n", "-H", "-h", "-s", "-I", "--line-number", "--with-filename", "--no-filename",
                "--no-messages", "--color", "--colour",
            ),
            bool,
        ),
        Option(
            "unmapped_flag",
            (
                "-x", "-c", "-q", "-L", "-P", "-z", "-a", "-U", "-b", "-T", "-o", "-v", "-y", "-Z", "-u", "-V",
                "--no-ignore-case", "--line-regexp", "--count", "--files-without-match", "--quiet", "--silent",
                "--perl-regexp", "--null", "--null-data", "--text", "--byte-offset", "--initial-tab", "--binary",
                "--only-matching", "--line-buffered", "--invert-match", "--unix-byte-offsets", "--version",
            ),
            bool,
        ),
        *CONTEXT_OPTIONS,
        Option("include", ("--include",)),
        Option("pattern", ("-e", "--regexp")),
        Option("pattern_file", ("-f", "--file")),
        Option(
            "unmapped_value",
            (
                "-m", "-d", "-D", "--max-count", "--directories", "--devices", "--exclude", "--include-dir",
                "--exclude-dir", "--exclude-from", "--binary-files", "--label", "--group-separator",
                "--context-separator",
            ),
        ),
    ),
)

UNMAPPED = frozenset({"unmapped_flag", "unmapped_value", "pattern_file"})

NO_VALUE_FLAGS = frozenset(
    {
        "--recursive", "--dereference-recursive", "--line-number", "--with-filename", "--no-filename",
        "--no-messages", "--files-with-matches", "--ignore-case", "--word-regexp", "--extended-regexp",
        "--basic-regexp", "--fixed-strings",
    }
)

BRE_METACHARS = frozenset(".*^$[\\")
ERE_METACHARS = BRE_METACHARS | frozenset("+?|(){}")

REGEX_ATOM = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_ .:@,=/-")

REGEX_ESCAPED_LITERAL = frozenset(".*[]^$\\")

SOURCE_SUFFIXES = frozenset(
    {
        ".bash", ".c", ".cc", ".cjs", ".clj", ".cljs", ".cpp", ".cs", ".cxx", ".dart",
        ".erl", ".ex", ".exs", ".go", ".h", ".hh", ".hpp", ".hs", ".hxx", ".java",
        ".js", ".jsx", ".kt", ".kts", ".lua", ".m", ".mjs", ".mm", ".php", ".pl",
        ".proto", ".py", ".pyi", ".rb", ".rs", ".scala", ".sh", ".sql", ".svelte",
        ".swift", ".ts", ".tsx", ".vue", ".zig", ".zsh",
    }
)


def grep_targets(paths: list[str], include: str | None, *, cwd: Path | None) -> tuple[str, list[str]] | None:
    """Split path operands into ``(glob, file_operands)``: several explicit files ride as positionals, the rest as a glob."""
    if include == "*":
        include = None
    if include is None and not any(p in (".", "./") for p in paths) and len(paths) >= 2:
        if all(not resolved_is_dir(p, cwd) for p in paths):
            return "", [p.rstrip("/") for p in paths]
    glob = grep_glob(paths, include, cwd=cwd)
    return None if glob is None else (glob, [])


def valid_brace(body: str) -> bool:
    """Whether an interval body is ``m``, ``m,`` or ``m,n`` with every bound within GNU's 32767 ceiling."""
    low, comma, high = body.partition(",")
    if len(body) > 11 or not low.isdecimal() or (comma and high and not high.isdecimal()):
        return False
    return all(int(part) <= 32767 for part in (low, high) if part)


def translate_pattern(pattern: str, ere: bool) -> str | None:
    """Translate a grep BRE (or ERE when ``ere``) pattern to Rust-regex, or ``None`` when its meaning would change.

    Admits plain atoms, ``.``, non-leading unstacked ``*`` (``+``/``?`` under ERE), a leading ``^``, a
    trailing ``$``, alternation, balanced groups, digit intervals, and escaped literals; BRE's literal
    ``+ ? ( ) { } |`` are emitted escaped.
    """
    n = len(pattern)
    out: list[str] = []
    depth = 0
    quantifiable = False
    quantifier = False
    i = 0
    while i < n:
        c = pattern[i]
        if c == "\\":
            nxt = pattern[i + 1] if i + 1 < n else ""
            if nxt in REGEX_ESCAPED_LITERAL:
                out.append("\\" + nxt)
                quantifiable, quantifier = True, False
                i += 2
            elif not ere and nxt == "|":
                if not quantifiable:
                    return None
                out.append("|")
                quantifiable = quantifier = False
                i += 2
            elif not ere and nxt == "(":
                depth += 1
                out.append("(")
                quantifiable = quantifier = False
                i += 2
            elif not ere and nxt == ")":
                depth -= 1
                if depth < 0:
                    return None
                out.append(")")
                quantifiable, quantifier = True, False
                i += 2
            elif not ere and nxt in "+?":
                if not quantifiable or quantifier:
                    return None
                out.append(nxt)
                quantifier = True
                i += 2
            elif not ere and nxt == "{":
                if not quantifiable or quantifier:
                    return None
                close = pattern.find("\\}", i + 2)
                if close == -1 or not valid_brace(pattern[i + 2 : close]):
                    return None
                out.append("{" + pattern[i + 2 : close] + "}")
                quantifiable = quantifier = True
                i = close + 2
            else:
                return None
            continue
        if c in REGEX_ATOM:
            out.append(c)
            quantifiable, quantifier = True, False
        elif c == "*" or (ere and c in "+?"):
            if not quantifiable or quantifier:
                return None
            out.append(c)
            quantifier = True
        elif c == "^":
            if i != 0:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif c == "$":
            if i != n - 1:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == "|":
            if not quantifiable:
                return None
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == "(":
            depth += 1
            out.append(c)
            quantifiable = quantifier = False
        elif ere and c == ")":
            depth -= 1
            if depth < 0:
                return None
            out.append(c)
            quantifiable, quantifier = True, False
        elif ere and c == "{":
            if not quantifiable or quantifier:
                return None
            close = pattern.find("}", i)
            if close == -1 or not valid_brace(pattern[i + 1 : close]):
                return None
            out.append(pattern[i : close + 1])
            quantifiable = quantifier = True
            i = close
        elif not ere and c in "+?(){}|":
            out.append("\\" + c)
            quantifiable, quantifier = True, False
        else:
            return None
        i += 1
    return "".join(out) if depth == 0 else None


def grep_parse(call: Call) -> GrepCall | None:
    """The ccx-rewritable shape of one direct, unpiped, unwrapped ``grep`` call, or ``None``."""
    source = call.occurrence.command
    if call.occurrence.prev_op == "|" or source.env or source.executable != "grep":
        return None
    arguments = GREP.bind(call)
    if not arguments.complete or glued_value(call, NO_VALUE_FLAGS) or arguments.values.keys() & UNMAPPED:
        return None
    patterns = bound_texts(arguments, "pattern")
    positionals = bound_texts(arguments, "operands")
    includes = bound_texts(arguments, "include")
    if len(patterns) > 1 or len(includes) > 1:
        return None
    if patterns:
        pattern, paths = patterns[0], positionals
    elif positionals:
        pattern, paths = positionals[0], positionals[1:]
    else:
        return None
    if not pattern or pattern.startswith("-"):
        return None
    values = arguments.values
    fixed, ere, bre = "fixed" in values, "extended" in values, "basic" in values
    if fixed and (ere or bre):
        return None
    regex = False
    if fixed:
        if not LITERAL_SAFE.match(pattern):
            return None
    elif any(c in (ERE_METACHARS if ere else BRE_METACHARS) for c in pattern):
        if (translated := translate_pattern(pattern, ere)) is None:
            return None
        pattern, regex = translated, True
    elif not LITERAL_SAFE.match(pattern):
        return None
    context = context_flags(arguments)
    if any(not count.isdigit() for _, count in context):
        return None
    targets = grep_targets(paths, includes[0] if includes else None, cwd=call.cwd)
    if targets is None:
        return None
    glob, path_ops = targets
    native_context = bool(context) and ccx_supports("code", "grep", flag="--after-context")
    return GrepCall(
        pattern,
        glob,
        "" if not context or native_context else "3",
        tuple(context) if native_context else (),
        "ignore_case" in values,
        "word" in values,
        "files_with_matches" in values,
        fixed,
        count_dropped=bool(context) and not native_context,
        regex=regex,
        paths=tuple(path_ops),
    )


def grep_operands(call: Call) -> list[str] | None:
    """A ``grep``'s path operands (the pattern excluded), or ``None`` when an unknown flag stops the binding."""
    arguments = GREP.bind(call)
    if unparsed(GREP, arguments):
        return None
    positionals = bound_texts(arguments, "operands")
    pattern_from_flag = "pattern" in arguments.values or "pattern_file" in arguments.values
    return positionals if pattern_from_flag else positionals[1:]


def spells_recursive(text: str) -> bool:
    if text.startswith("--"):
        return text.partition("=")[0] in ("--recursive", "--dereference-recursive")
    return text.startswith("-") and ("r" in text or "R" in text)


def grep_recursive(call: Call) -> bool:
    """Whether a grep recurses: a bound ``-r``/``-R``, or a recursive spelling among the words an unknown flag left unread."""
    arguments = GREP.bind(call)
    if "recursive" in arguments.values:
        return True
    unread = [word_text(word) for word in arguments.unread]
    options = unread[: unread.index("--")] if "--" in unread else unread
    return any(spells_recursive(text) for text in options)


def grep_tree_shaped(call: Call) -> bool:
    """Whether a grep searches a directory: recursive with no operand, or a ``.``/``..``/directory operand.

    Under an unknown flag, only a recursive grep with a literal ``.``/``..`` operand counts.
    """
    recursive = grep_recursive(call)
    if (ops := grep_operands(call)) is None:
        return recursive and any(p.rstrip("/") in (".", "..") for p in loose_operands(call, GREP))
    return (recursive and not ops) or any(resolved_is_dir(p, call.cwd) for p in ops)


def is_source_file(p: str, cwd: Path | None) -> bool:
    path = resolve_operand(p, cwd)
    return path is not None and path.suffix.lower() in SOURCE_SUFFIXES and path.is_file()


def grep_source_shaped(call: Call) -> bool:
    """Whether a grep names only existing source files."""
    ops = grep_operands(call)
    return bool(ops) and all(is_source_file(p, call.cwd) for p in ops)


def grep_to(call: Call) -> str | None:
    return build_ccx_grep(parsed) if (parsed := grep_parse(call)) is not None else None


def grep_visit(evt: PreToolUseEvent, occ: Occurrence, ctx: WalkContext) -> str | Rewritten | HookResult | None:
    """Rewrite a tree-shaped or source-file grep, block an unmappable tree-shaped one, and run everything else raw.

    Substitutions, expanding operands, and a ``ccx`` too old to express the search all run raw.
    """
    call = Call(evt.cmd, occ, ctx.cwd)
    if call.name != "grep" or occ.prev_op == "|":
        return None
    flood = grep_tree_shaped(call)
    if not flood and not grep_source_shaped(call):
        return None
    searched = grep_operands(call)
    if flood and scratch_tree(loose_operands(call, GREP) if searched is None else searched, call.cwd):
        return None
    if forfeits_substitution(call):
        return None
    if (ops := grep_operands(call)) and any(forfeits_operand(p) for p in ops):
        return None
    if (parsed := grep_parse(call)) is None:
        return evt.block(GREP_FLOOD) if flood else None
    if (text := build_ccx_grep(parsed)) is None:
        return None
    if ctx.spliceable:
        return Rewritten(text, note=first_sight(evt, search_note(parsed)))
    return evt.block(GREP_FLOOD) if flood else None


hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SearchTargets(GREP, grep_operands, targets_transcript)],
    skip_if=[Annotated("raw")],
    message=TRANSCRIPT_STEER,
    block=True,
    tests={
        Input(command=f"grep -r foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"grep -r foo {TRANSCRIPTS}/ | head"): Block(pattern="cc-transcript"),
        Input(command=f"grep foo {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX}; grep -v bar ."): Block(pattern="cc-transcript"),
        Input(command=f"/usr/bin/grep -r foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"grep -n foo {EXAMPLE_SESSION}/subagents/agent-a1{TRANSCRIPT_SUFFIX}"): Block(
            pattern="cc-transcript"
        ),
        Input(command=f"grep -r foo {EXAMPLE_SESSION}/"): Block(pattern="cc-transcript"),
        Input(command=f"grep -r needle {EXAMPLE_SESSION}/*/"): Block(pattern="cc-transcript"),
        Input(command="grep -r foo ~/.claude/plugins/"): Allow(),
        Input(command=f"grep -r foo {TRANSCRIPTS}/ # ccx:raw"): Allow(),
        Input(command=f"echo '# ccx:raw'; grep -r foo {TRANSCRIPTS}/"): Block(pattern="cc-transcript"),
        Input(command=f"cat x | grep foo {EXAMPLE_SESSION}{TRANSCRIPT_SUFFIX}"): Allow(),
        Input(command=f"grep -E 'landing-desk' {EXAMPLE_SESSION}/tool-results/toolu_01H.txt"): Allow(),
        Input(command=f"grep -n lint notes.md {TRANSCRIPTS}/-Users-me-repo/memory/capt-hook-call-args.md"): Allow(),
    },
)

hook(
    Event.PreToolUse,
    only_if=[Tool("Bash"), SearchTargets(GREP, grep_operands, targets_dependency)],
    skip_if=[Annotated("raw")],
    message=DEP_STEER,
    block=True,
    tests={
        Input(command="grep -r foo .git/ | head"): Block(pattern="dep-reader"),
        Input(command="grep -rn foo .venv/lib/"): Block(pattern="dep-reader"),
        Input(command="grep -r foo node_modules/express | head"): Block(pattern="dep-reader"),
        Input(command="grep -rn '.venv' README.md"): Allow(),
        Input(command="grep -rn foo .venv/lib/ # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; grep -rn foo .venv/lib/"): Block(pattern="dep-reader"),
        Input(command="grep -rn foo . | grep -v node_modules"): Allow(),
    },
)

rewrite_command_occurrences(
    only_if=[UnpipedSearch("grep")],
    skip_if=[Annotated("raw")],
    visit=grep_visit,
    tests={
        Input(command="grep -rn foo"): Rewrite(pattern="code grep foo"),
        Input(command="grep --recursive foo ."): Rewrite(pattern="code grep foo"),
        Input(command="grep -rn --include='*.go' foo ."): Rewrite(pattern="--glob '*.go'"),
        Input(command="grep -A 7 foo ."): Rewrite(pattern="-A=7"),
        Input(command="grep -rn foo . src/"): Rewrite(pattern="code grep foo"),
        Input(command="echo x; grep -r foo ."): Rewrite(pattern="echo x; "),
        Input(command="grep -ri foo"): Rewrite(pattern="code grep foo"),
        Input(command="grep -riw foo"): Rewrite(pattern="-i -w"),
        Input(command="grep foo ."): Rewrite(pattern="code grep foo"),
        Input(command="grep 'foo.*' ."): Rewrite(pattern="--regex"),
        Input(command="grep -E 'a|b' ."): Rewrite(pattern="--regex"),
        Input(command="grep -E 'a+' ."): Rewrite(pattern="--regex"),
        Input(command="grep 'a+' ."): Rewrite(pattern="code grep a+"),
        Input(command="grep 'a\\|b' ."): Rewrite(pattern="--regex"),
        Input(command="grep 'x\\(ab\\)\\+' ."): Rewrite(pattern="--regex"),
        Input(command="grep 'foo$' ."): Rewrite(pattern="--regex"),
        Input(command="grep -rnC3 foo ."): Block(pattern="ccx code grep"),
        Input(command="grep -v foo ."): Block(pattern="ccx code grep"),
        Input(command="grep -rv foo ."): Block(),
        Input(command="grep -rn foo . # ccx:raw"): Allow(),
        Input(command="grep -v foo . # ccx:raw"): Allow(),
        Input(command="echo '# ccx:raw'; grep -rn foo ."): Rewrite(pattern="code grep foo"),
        Input(command="grep -rhoE 'log (append|show)' /tmp"): Allow(),
        Input(command="grep --recursive=oops foo ."): Block(),
        Input(command="grep -P 'x(?=y)' ."): Block(),
        Input(command="grep 'a^b' ."): Block(),
        Input(command="grep -F 'foo.*' ."): Block(),
        Input(command="grep -E -F foo ."): Block(),
        Input(command="grep -q foo ."): Block(),
        Input(command="grep -c foo ."): Block(),
        Input(command="grep -o foo ."): Block(),
        Input(command="grep -e foo -e bar ."): Block(),
        Input(command="grep -f patterns.txt ."): Block(),
        Input(command="GREP_OPTIONS=-v grep foo ."): Block(),
        Input(command="grep foo /var/log/x.log"): Allow(),
        Input(command="grep foo ~/notes.md"): Allow(),
        Input(command="grep foo ghost.py"): Allow(),
        Input(command="grep -n foo $d/host.go"): Allow(),
        Input(command="grep foo ~/app.log"): Allow(),
        Input(command="grep -q pat missing.html"): Allow(),
        Input(command="grep -l foo a.html b.html"): Allow(),
        Input(command="grep -o localhost /etc/hosts"): Allow(),
        Input(command="grep -oHnb . AGENTS.md"): Allow(),
        Input(command="grep -c needle *"): Allow(),
        Input(command="grep -c needle file{1..10000}"): Allow(),
        Input(command="grep -r foo logs.json"): Allow(),
        Input(
            command="curl -sL -o /tmp/ch-live.html https://yasyf.github.io/captain-hook/ && "
            "wc -c < /tmp/ch-live.html && for m in 'id=\"links\"' gd-hero; do "
            "printf '%s: %s\\n' \"$m\" \"$(grep -c \"$m\" /tmp/ch-live.html)\"; done"
        ): Allow(),
        Input(command="grep -i err app.log | head"): Allow(),
        Input(command="grep foo ghost.py | wc -l"): Allow(),
        Input(command="grep -oi points b_jetblue_jun.json"): Allow(),
        Input(command="grep -n adoptionPinRefusals build.log"): Allow(),
        Input(command="grep -n version package.json"): Allow(),
        Input(command="grep -n image compose.yaml"): Allow(),
        Input(command="cat api/src/Team.ts | grep adoptionPinRefusals"): Allow(),
        Input(command="grep -n adoptionPinRefusals api/src/Team.ts | head"): Allow(),
        Input(command="grep -n adoptionPinRefusals ghost.ts"): Allow(),
        Input(command="echo x > gen.json; grep -i points gen.json"): Allow(),
        Input(command="cd sub && grep foo notes.json"): Allow(),
        Input(command="grep -r foo src/ | head"): Allow(),
        Input(command="grep -r x . | head -n 100000"): Rewrite(pattern="code grep x"),
        Input(command="grep -r x . | head -c 100000000"): Rewrite(pattern="code grep x"),
        Input(command="grep -r x . | tee /tmp/out | head"): Rewrite(pattern="code grep x"),
        Input(command="grep -rn foo | sed '1,20p'"): Rewrite(pattern="code grep foo"),
        Input(command="grep -r foo src/ | grep -v x"): Allow(),
        Input(command="grep -rn foo . | grep -v node_modules"): Rewrite(pattern="code grep foo"),
        Input(command="grep -rn public_edge_test --include=* . | grep -v node_modules | head"): Rewrite(
            pattern="code grep public_edge_test |"
        ),
        Input(command="grep -rni goldens -l . | grep -v node_modules"): Rewrite(pattern="code grep goldens -i -l"),
        Input(command="grep -rn foo . | sort"): Rewrite(pattern="code grep foo"),
        Input(command="grep -r foo . | tail -f"): Rewrite(pattern="code grep foo"),
        Input(command="grep -i points data.json && grep foo ."): Rewrite(pattern="grep -i points data.json && "),
        Input(command="grep -c foo data.json && grep -rn bar ."): Rewrite(pattern="grep -c foo data.json && "),
        Input(command="grep foo $d/host.go; grep bar ."): Rewrite(pattern="grep foo $d/host.go; "),
        Input(command="grep -c foo . && grep -rn bar ."): Block(),
        Input(command="echo x; grep -c foo ."): Block(),
        Input(command="grep -r foo $(dir)"): Allow(),
        Input(command="grep foo $(printf /tmp/target)"): Allow(),
        Input(command="grep -n foo `printf x`"): Allow(),
        Input(command="grep -r . . '$(printf x)'"): Allow(),
        Input(command="grep -r . . ~/notes.md"): Allow(),
        Input(command="grep -r foo 'src[old]/' ."): Allow(),
        Input(command="grep -r --weird foo ."): Block(),
        Input(command="grep needle docs/notes.md"): Allow(),
        Input(command="sudo grep foo ."): Block(),
        Input(command="timeout 10 grep foo ."): Block(),
        Input(command="/usr/bin/grep -rn foo ."): Block(),
        Input(command='"grep" -rn foo .'): Block(),
        Input(command="ls | grep foo"): Allow(),
        Input(command="cat x | grep foo | sort"): Allow(),
        Input(command="git log --grep=fix"): Allow(),
        Input(command='git log --grep "fix bug"'): Allow(),
        Input(
            command="ccx exec 'async def main(): return await sh(\"grep -rn foo src/\")\nasyncio.run(main())'"
        ): Allow(),
        Input(
            command="ccx exec --file - <<'PY'\n"
            'async def main(): return await sh("grep -rn foo src/")\n'
            "asyncio.run(main())\nPY"
        ): Allow(),
    },
)
