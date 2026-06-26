"""Generate the benchmark corpus from the fixture manifest plus curated tasks.

Navigation/callees/callers/intent tasks are derived directly from the manifest, so
their gold answers are ground truth by construction. diff_review, targeted_edit, and
non_regression tasks are curated literals: diff tasks apply an uncommitted edit whose
changed symbols are the gold; edit tasks are graded by running a check in the post-run
workdir; non_regression tasks (ccx_helps=False) prove ccx adds no harm where it can't help.
"""

from __future__ import annotations

from .types import Grader, Task

# Bumped whenever the category mix changes; stamped into meta.json so old and new
# sessions are never fused under one headline (the mix is not comparable across versions).
TASK_MIX_VERSION = "2-real-usage"

# Relative real-usage frequency per category, mined from 100 transcripts: tool-IO is
# 67.9% of session chars, dominated by Read (33.5%), Bash grep/cat/find/git-diff (38.9%),
# and Edit (9.9%). Read/understand and multi-file search/diff/find carry the most weight;
# callees/callers symbol-graph queries are a small real slice and are trimmed to a
# representative sample (see SYMBOL_GRAPH_SAMPLE). Weights sum to 1.0; the report imports
# them to weight per-category results by how often the operation actually burns tokens.
CATEGORY_WEIGHTS: dict[str, float] = {
    "navigation": 0.22,
    "intent_search": 0.18,
    "large_context": 0.14,
    "diff_review": 0.12,
    "structural_search": 0.08,
    "targeted_edit": 0.07,
    "structural_replace": 0.06,
    "callees": 0.04,
    "callers": 0.04,
    "non_regression": 0.05,
}

# Symbol-graph (callees/callers) tasks are derived one-per-manifest-symbol, which
# over-indexes the corpus on a small real-usage slice. Cap each at this many representative
# symbols (manifest order is stable, so the sample is deterministic).
SYMBOL_GRAPH_SAMPLE = 3

NAV_SCHEMA = {
    "type": "object",
    "properties": {"file": {"type": "string"}, "line": {"type": "integer"}},
    "required": ["file", "line"],
    "additionalProperties": False,
}
CALLEES_SCHEMA = {
    "type": "object",
    "properties": {"callees": {"type": "array", "items": {"type": "string"}}},
    "required": ["callees"],
    "additionalProperties": False,
}
CALLERS_SCHEMA = {
    "type": "object",
    "properties": {"callers": {"type": "array", "items": {"type": "string"}}},
    "required": ["callers"],
    "additionalProperties": False,
}
FILE_SCHEMA = {
    "type": "object",
    "properties": {"file": {"type": "string"}},
    "required": ["file"],
    "additionalProperties": False,
}
SYMBOLS_SCHEMA = {
    "type": "object",
    "properties": {"symbols": {"type": "array", "items": {"type": "string"}}},
    "required": ["symbols"],
    "additionalProperties": False,
}
EDIT_SCHEMA = {
    "type": "object",
    "properties": {"changed_file": {"type": "string"}, "summary": {"type": "string"}},
    "required": ["changed_file"],
    "additionalProperties": False,
}
ANSWER_SCHEMA = {
    "type": "object",
    "properties": {"answer": {"type": "string"}},
    "required": ["answer"],
    "additionalProperties": False,
}
CLASSES_SCHEMA = {
    "type": "object",
    "properties": {"classes": {"type": "array", "items": {"type": "string"}}},
    "required": ["classes"],
    "additionalProperties": False,
}
TYPES_SCHEMA = {
    "type": "object",
    "properties": {"types": {"type": "array", "items": {"type": "string"}}},
    "required": ["types"],
    "additionalProperties": False,
}

REPO = "fixture"

MUX_ROUTECOUNT_TEST = (
    "printf 'package mux\\nimport \"testing\"\\n"
    "func TestRouteCountBench(t *testing.T){ r:=NewRouter(); r.NewRoute(); r.NewRoute(); "
    "if r.RouteCount()!=2 { t.Fatal(r.RouteCount()) } }' > zz_routecount_test.go && "
    "go test -run TestRouteCountBench . >/dev/null 2>&1; rc=$?; rm -f zz_routecount_test.go; exit $rc"
)

MUX_CLEANPATH_TEST = (
    "printf 'package mux\\nimport \"testing\"\\n"
    "func TestCleanPathEmptyBench(t *testing.T){ if cleanPath(\"\")!=\"/index\" { t.Fatal(cleanPath(\"\")) } }' "
    "> zz_cleanpath_test.go && "
    "go test -run TestCleanPathEmptyBench . >/dev/null 2>&1; rc=$?; rm -f zz_cleanpath_test.go; exit $rc"
)

# A structural-replace prompt deliberately states the change as a pattern→rewrite over
# the code's shape, not "open the file and edit line N": the ccx arm can run a single
# `ccx replace '<pattern>' '<rewrite>'` (or the ccx_replace MCP tool) without reading the
# file, while the baseline must Read then Edit. Graded by test_run on the resulting code.


def nav_tasks(manifest: dict) -> list[Task]:
    tasks = []
    for s in manifest["symbols"]:
        tasks.append(
            Task(
                id=f"nav-{s['name']}",
                category="navigation",
                repo=REPO,
                prompt=(
                    f"In this repository, where is the function `{s['name']}` declared? "
                    "Respond with the file path relative to the repo root and the 1-based "
                    "line number of its declaration."
                ),
                schema=NAV_SCHEMA,
                grader=Grader("file_line", {"line_tolerance": 2}),
                gold={"file": s["file"], "line": s["line"]},
            )
        )
    return tasks


def callees_tasks(manifest: dict) -> list[Task]:
    tasks = []
    for s in manifest["symbols"]:
        if not s["callees"]:
            continue
        if len(tasks) >= SYMBOL_GRAPH_SAMPLE:
            break
        tasks.append(
            Task(
                id=f"callees-{s['name']}",
                category="callees",
                repo=REPO,
                prompt=(
                    f"Within this repository's own code, which functions does `{s['name']}` "
                    "call directly? List only functions defined in this repository; ignore "
                    "standard-library or built-in calls."
                ),
                schema=CALLEES_SCHEMA,
                grader=Grader("set_match", {"field": "callees", "mode": "equal", "lower": True}),
                gold={"callees": s["callees"]},
            )
        )
    return tasks


def callers_tasks(manifest: dict) -> list[Task]:
    tasks = []
    for s in manifest["symbols"]:
        if not s["callers"]:
            continue
        if len(tasks) >= SYMBOL_GRAPH_SAMPLE:
            break
        tasks.append(
            Task(
                id=f"callers-{s['name']}",
                category="callers",
                repo=REPO,
                prompt=(
                    f"Within this repository's own code, which functions call `{s['name']}` "
                    "directly? List the calling functions' names."
                ),
                schema=CALLERS_SCHEMA,
                grader=Grader("set_match", {"field": "callers", "mode": "equal", "lower": True}),
                gold={"callers": s["callers"]},
            )
        )
    return tasks


def intent_tasks(manifest: dict) -> list[Task]:
    tasks = []
    for i, it in enumerate(manifest["intents"]):
        tasks.append(
            Task(
                id=f"intent-{i}",
                category="intent_search",
                repo=REPO,
                prompt=(
                    f"Which single file contains the code that {it['phrase']}? "
                    "Respond with the file path relative to the repo root."
                ),
                schema=FILE_SCHEMA,
                grader=Grader("file_match", {}),
                gold={"file": it["file"]},
            )
        )
    return tasks


def diff_tasks() -> list[Task]:
    return [
        Task(
            id="diff-double",
            category="diff_review",
            repo=REPO,
            prompt=(
                "The working tree has uncommitted changes. Which top-level functions were "
                "modified relative to HEAD? List their names."
            ),
            schema=SYMBOLS_SCHEMA,
            grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
            gold={"symbols": ["Double"]},
            setup={"edits": [{"file": "internal/calc/calc.go", "find": "return Add(n, n)", "replace": "return Add(n, Add(n, 0))"}]},
        ),
        Task(
            id="diff-greet-compute",
            category="diff_review",
            repo=REPO,
            prompt=(
                "The working tree has uncommitted changes. Which top-level functions were "
                "modified relative to HEAD? List their names."
            ),
            schema=SYMBOLS_SCHEMA,
            grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
            gold={"symbols": ["salutation", "Max"]},
            setup={
                "edits": [
                    {"file": "internal/greet/greet.go", "find": 'return "Hello"', "replace": 'return "Hi"'},
                    {"file": "internal/calc/calc.go", "find": "best := 0", "replace": "best := xs[0]\n\t_ = best"},
                ]
            },
        ),
        Task(
            id="diff-slugify",
            category="diff_review",
            repo=REPO,
            prompt=(
                "The working tree has uncommitted changes. Which top-level functions were "
                "modified relative to HEAD? List their names."
            ),
            schema=SYMBOLS_SCHEMA,
            grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
            gold={"symbols": ["slugify"]},
            setup={"edits": [{"file": "pysrc/util.py", "find": '.replace(" ", "-")', "replace": '.replace(" ", "_")'}]},
        ),
    ]


def edit_tasks() -> list[Task]:
    py = "python3 -c"
    return [
        Task(
            id="edit-slugify-underscore",
            category="targeted_edit",
            repo=REPO,
            prompt=(
                "In pysrc/util.py, change the `slugify` function so it joins words with an "
                "underscore (`_`) instead of a hyphen (`-`). Make no other behavioral change."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": f"{py} \"import sys; sys.path.insert(0,'pysrc'); import util; assert util.slugify('a b c')=='a_b_c', util.slugify('a b c')\""},
            ),
            gold={
                "check": "slugify('a b c') == 'a_b_c'",
                "solution_edits": [{"file": "pysrc/util.py", "find": '.replace(" ", "-")', "replace": '.replace(" ", "_")'}],
            },
        ),
        Task(
            id="edit-double-triple",
            category="targeted_edit",
            repo=REPO,
            prompt=(
                "In internal/calc/calc.go, change the `Double` function so it returns three "
                "times n instead of twice n. Keep the function name and signature unchanged."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": "printf 'package calc\\nimport \"testing\"\\nfunc TestD(t *testing.T){ if Double(4)!=12 { t.Fatal(Double(4)) } }' > internal/calc/zz_bench_test.go && go test ./internal/calc/ >/dev/null 2>&1; rc=$?; rm -f internal/calc/zz_bench_test.go; exit $rc", "timeout_s": 180},
            ),
            gold={
                "check": "Double(4) == 12",
                "solution_edits": [{"file": "internal/calc/calc.go", "find": "return Add(n, n)", "replace": "return Add(n, Add(n, n))"}],
            },
        ),
        Task(
            id="edit-titlecase-blank",
            category="targeted_edit",
            repo=REPO,
            prompt=(
                "In pysrc/util.py, add a new function `is_long(s)` that returns True when the "
                "normalized string has more than 10 characters, reusing the existing "
                "`normalize` helper. Do not change existing functions."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": f"{py} \"import sys; sys.path.insert(0,'pysrc'); import util; assert util.is_long('a'*11) is True and util.is_long('a b') is False and util.is_long('a   b   c   d') is False\""},
            ),
            gold={
                "check": "is_long longer-than-10 via normalize",
                "solution_edits": [
                    {
                        "file": "pysrc/util.py",
                        "find": '    return normalize(s) == ""',
                        "replace": '    return normalize(s) == ""\n\n\ndef is_long(s):\n    """Report whether the normalized string is longer than ten characters."""\n    return len(normalize(s)) > 10',
                    }
                ],
            },
        ),
    ]


def replace_tasks() -> list[Task]:
    py = "python3 -c"
    go_test = (
        "printf 'package calc\\nimport \"testing\"\\n{test}' > internal/calc/zz_replace_test.go && "
        "go test ./internal/calc/ >/dev/null 2>&1; rc=$?; rm -f internal/calc/zz_replace_test.go; exit $rc"
    )
    return [
        Task(
            id="replace-slugify-underscore",
            category="structural_replace",
            repo=REPO,
            prompt=(
                "In pysrc/util.py, perform a structural find-replace: rewrite the expression "
                '`normalize(s).replace(" ", "-")` to `normalize(s).replace(" ", "_")` so '
                "`slugify` joins words with an underscore. Change nothing else."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": f"{py} \"import sys; sys.path.insert(0,'pysrc'); import util; assert util.slugify('a b c')=='a_b_c', util.slugify('a b c')\""},
            ),
            gold={
                "check": "slugify('a b c') == 'a_b_c'",
                "solution_edits": [{"file": "pysrc/util.py", "find": 'normalize(s).replace(" ", "-")', "replace": 'normalize(s).replace(" ", "_")'}],
            },
        ),
        Task(
            id="replace-double-thrice",
            category="structural_replace",
            repo=REPO,
            prompt=(
                "In internal/calc/calc.go, perform a structural find-replace inside `Double`: "
                "rewrite the statement `return Add(n, n)` to `return Add(n, Add(n, n))` so it "
                "returns three times n. Keep the signature unchanged."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": go_test.format(test='func TestDoubleBench(t *testing.T){ if Double(4)!=12 { t.Fatal(Double(4)) } }'), "timeout_s": 180},
            ),
            gold={
                "check": "Double(4) == 12",
                "solution_edits": [{"file": "internal/calc/calc.go", "find": "return Add(n, n)", "replace": "return Add(n, Add(n, n))"}],
            },
        ),
        Task(
            id="replace-triple-quad",
            category="structural_replace",
            repo=REPO,
            prompt=(
                "In internal/calc/calc.go, perform a structural find-replace inside `Triple`: "
                "rewrite the statement `return Add(Double(n), n)` to "
                "`return Add(Double(n), Double(n))`. Keep the signature unchanged."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": go_test.format(test='func TestTripleBench(t *testing.T){ if Triple(3)!=12 { t.Fatal(Triple(3)) } }'), "timeout_s": 180},
            ),
            gold={
                "check": "Triple(3) == 12",
                "solution_edits": [{"file": "internal/calc/calc.go", "find": "return Add(Double(n), n)", "replace": "return Add(Double(n), Double(n))"}],
            },
        ),
        Task(
            id="replace-sub-swap",
            category="structural_replace",
            repo=REPO,
            prompt=(
                "In internal/calc/calc.go, perform a structural find-replace using a pattern with "
                "metavariables: rewrite the return statement matching `return $A - $B` to "
                "`return $B - $A`, swapping the operands of `Sub`. Change only `Sub`."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader(
                "test_run",
                {"cmd": go_test.format(test='func TestSubSwapBench(t *testing.T){ if Sub(5,2)!=-3 { t.Fatal(Sub(5,2)) } }'), "timeout_s": 180},
            ),
            gold={
                "check": "Sub(5, 2) == -3",
                "solution_edits": [{"file": "internal/calc/calc.go", "find": "return a - b", "replace": "return b - a"}],
            },
        ),
    ]


def routing_tasks() -> list[Task]:
    """Paired search-routing tasks: a metavar/code-pattern query (routes structural via
    ast-grep) and a code-shape query, each answerable by locating a file/symbol. The
    semantic (NL-intent) side of the pair is already covered by intent_tasks; these add
    the structural-query side the router must classify as structural."""
    return [
        Task(
            id="route-struct-compute-loop",
            category="structural_search",
            repo=REPO,
            prompt=(
                "Search this repository for code matching the structural pattern "
                "`func $NAME(xs []int) int` (a function taking an int slice and returning an int). "
                "Which single file declares `Compute`, one of the functions that matches? Respond "
                "with the repo-relative file path."
            ),
            schema=FILE_SCHEMA,
            grader=Grader("file_match", {}),
            gold={"file": "internal/calc/calc.go"},
        ),
        Task(
            id="route-struct-greet-callsite",
            category="structural_search",
            repo=REPO,
            prompt=(
                "Search this repository for the structural call pattern `greet.Greet($A)` "
                "(a call to greet.Greet with any single argument). Which single file contains a "
                "call site matching it? Respond with the repo-relative file path."
            ),
            schema=FILE_SCHEMA,
            grader=Grader("file_match", {}),
            gold={"file": "cmd/app/main.go"},
        ),
        Task(
            id="route-nl-greeting",
            category="intent_search",
            repo=REPO,
            prompt=(
                "Using a natural-language code search, which single file contains the code that "
                "produces the welcome line shown to a user, addressing them by name? Respond with "
                "the repo-relative file path."
            ),
            schema=FILE_SCHEMA,
            grader=Grader("file_match", {}),
            gold={"file": "internal/greet/greet.go"},
        ),
    ]


def non_regression_tasks() -> list[Task]:
    # Each task uses synonym groups: a correct answer must hit >=1 term in EVERY group, so the
    # grader rejects off-topic answers yet accepts paraphrases. These tasks need no repo access;
    # they confirm ccx adds no cost/accuracy harm where it cannot help (ccx_helps=False).
    specs = [
        ("nonreg-binsearch", "Explain in two sentences how binary search works on a sorted array.",
         [["sort"], ["half", "halve", "middle", "mid", "divide"]]),
        ("nonreg-hashmap", "In two sentences, what is a hash map and what is its average lookup time?",
         [["hash", "dictionary", "key"], ["o(1)", "constant", "amortized", "average"]]),
        ("nonreg-recursion", "Define recursion in one sentence and give the term for its stopping condition.",
         [["itself", "self"], ["base case", "base-case", "stopping", "terminat"]]),
        ("nonreg-bigo", "In one sentence, what does Big-O notation describe about an algorithm?",
         [["time", "operation", "step", "memory", "space", "resource"], ["grow", "scale", "input", "size"]]),
    ]
    tasks = []
    for tid, prompt, groups in specs:
        tasks.append(
            Task(
                id=tid,
                category="non_regression",
                repo=REPO,
                prompt=prompt,
                schema=ANSWER_SCHEMA,
                grader=Grader("keywords", {"field": "answer"}),
                gold={"groups": groups},
                ccx_helps=False,
            )
        )
    return tasks


def nav_oss(tid: str, repo: str, prompt: str, file: str, line: int, decl: str) -> Task:
    return Task(
        id=tid,
        category="navigation",
        repo=repo,
        prompt=prompt,
        schema=NAV_SCHEMA,
        grader=Grader("file_line", {"line_tolerance": 2}),
        gold={"file": file, "line": line, "verify_decl": decl},
    )


def intent_oss(tid: str, repo: str, prompt: str, file: str) -> Task:
    return Task(
        id=tid,
        category="intent_search",
        repo=repo,
        prompt=prompt + " Respond with the file path relative to the repo root.",
        schema=FILE_SCHEMA,
        grader=Grader("file_match", {}),
        gold={"file": file},
    )


def oss_tasks() -> list[Task]:
    """Complex, large-context tasks over pinned real repos (gold verified at build time).

    These are where ccx's value should show: big-file navigation (click core.py is 3k lines),
    semantic intent search across many files, and multi-file diff review.
    """
    diff_prompt = (
        "The working tree has uncommitted changes, possibly across multiple files. Which "
        "top-level functions or methods were modified relative to HEAD? List their names."
    )
    return [
        nav_oss("mux-nav-router-match", "gorilla-mux",
                "In this repository, on which file and 1-based line is the `Match` method of the "
                "`*Router` type declared (the router's Match, not `*Route`'s)? Respond with the "
                "repo-relative file path and the line of its declaration.",
                "mux.go", 138, "func (r *Router) Match("),
        nav_oss("mux-nav-getname", "gorilla-mux",
                "In this repository, where is the `GetName` method of the `*Route` type declared? "
                "Respond with the repo-relative file path and the 1-based declaration line.",
                "route.go", 162, "func (r *Route) GetName("),
        nav_oss("click-nav-command", "click",
                "In this repository, where is the `Command` class declared? Respond with the "
                "repo-relative file path and the 1-based line of its declaration.",
                "src/click/core.py", 1160, "class Command("),
        nav_oss("click-nav-option", "click",
                "In this repository, where is the `Option` class declared? Respond with the "
                "repo-relative file path and the 1-based line of its declaration.",
                "src/click/core.py", 2449, "class Option("),
        nav_oss("click-nav-split-opt", "click",
                "In this repository, where is the `split_opt` function declared? Respond with the "
                "repo-relative file path and the 1-based line of its declaration.",
                "src/click/parser.py", 109, "def split_opt("),
        intent_oss("mux-intent-dispatch", "gorilla-mux",
                   "Which file contains the code that dispatches an incoming HTTP request to the "
                   "matching route's handler (the http.Handler entry point of the router)?",
                   "mux.go"),
        intent_oss("mux-intent-regexp", "gorilla-mux",
                   "Which file implements the regular-expression machinery that compiles and "
                   "matches route path patterns and their variables?",
                   "regexp.go"),
        intent_oss("click-intent-parser", "click",
                   "Which file implements the parser that turns raw command-line argument strings "
                   "into parsed options and arguments?",
                   "src/click/parser.py"),
        intent_oss("click-intent-decorators", "click",
                   "Which file defines the decorators used to declare commands and options "
                   "(for example the @command and @option decorators)?",
                   "src/click/decorators.py"),
        Task(
            id="mux-diff-multifile",
            category="diff_review",
            repo="gorilla-mux",
            prompt=diff_prompt,
            schema=SYMBOLS_SCHEMA,
            grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
            gold={"symbols": ["NewRouter", "GetName"]},
            setup={
                "edits": [
                    {"file": "mux.go", "find": "return &Router{namedRoutes: make(map[string]*Route)}", "replace": "return &Router{namedRoutes: make(map[string]*Route)} //nolint"},
                    {"file": "route.go", "find": "return r.name", "replace": "return r.name //nolint"},
                ]
            },
        ),
        Task(
            id="click-diff-parser",
            category="diff_review",
            repo="click",
            prompt=diff_prompt,
            schema=SYMBOLS_SCHEMA,
            grader=Grader("set_match", {"field": "symbols", "mode": "equal", "lower": True}),
            gold={"symbols": ["split_opt", "normalize_opt"]},
            setup={
                "edits": [
                    {"file": "src/click/parser.py", "find": "    first = opt[:1]", "replace": "    first = opt[:1]  # nolint"},
                    {"file": "src/click/parser.py", "find": "    if ctx is None or ctx.token_normalize_func is None:", "replace": "    if ctx is None or ctx.token_normalize_func is None:  # nolint"},
                ]
            },
        ),
        Task(
            id="mux-edit-routecount",
            category="targeted_edit",
            repo="gorilla-mux",
            prompt=(
                "Add a method `func (r *Router) RouteCount() int` that returns the number of "
                "routes registered on the router (the length of its internal routes slice). Keep "
                "all existing behavior unchanged."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader("test_run", {"cmd": MUX_ROUTECOUNT_TEST, "timeout_s": 180}),
            gold={
                "check": "RouteCount() == len(routes)",
                "solution_edits": [
                    {"file": "mux.go", "find": "func NewRouter() *Router {", "replace": "func (r *Router) RouteCount() int { return len(r.routes) }\n\nfunc NewRouter() *Router {"}
                ],
            },
        ),
        Task(
            id="mux-replace-cleanpath-empty",
            category="structural_replace",
            repo="gorilla-mux",
            prompt=(
                "In mux.go, the unexported `cleanPath` function maps an empty path to `\"/\"`. "
                "Perform a structural find-replace on that guard so an empty path maps to "
                "`\"/index\"` instead: rewrite the `if p == \"\" { return \"/\" }` block's returned "
                "value from `\"/\"` to `\"/index\"`. Change nothing else."
            ),
            schema=EDIT_SCHEMA,
            grader=Grader("test_run", {"cmd": MUX_CLEANPATH_TEST, "timeout_s": 180}),
            gold={
                "check": 'cleanPath("") == "/index"',
                "solution_edits": [
                    {"file": "mux.go", "find": 'if p == "" {\n\t\treturn "/"\n\t}', "replace": 'if p == "" {\n\t\treturn "/index"\n\t}'}
                ],
            },
        ),
    ]


def large_context_tasks() -> list[Task]:
    """Whole-/many-file enumeration tasks whose gold is the complete set satisfying a body-level predicate."""
    return [
        Task(
            id="click-enum-get-command-classes",
            category="large_context",
            repo="click",
            prompt=(
                "In `src/click/core.py`, enumerate every public class defined at module scope "
                "(public = name does not start with an underscore; top-level only, not nested) "
                "whose own body defines a method named `get_command`. A class counts only if it "
                "declares `get_command` directly in its body — not if it merely inherits it from "
                "a base class. List each qualifying class name exactly once."
            ),
            schema=CLASSES_SCHEMA,
            grader=Grader("set_match", {"field": "classes", "mode": "equal", "lower": True}),
            gold={
                "classes": ["MultiCommand", "Group", "CommandCollection"],
                "lc_predicate": {"kind": "py_method", "file": "src/click/core.py", "target": "get_command"},
                "verify_decls": [
                    ("src/click/core.py", "class MultiCommand(Command):"),
                    ("src/click/core.py", "class Group(MultiCommand):"),
                    ("src/click/core.py", "class CommandCollection(MultiCommand):"),
                ],
            },
        ),
        Task(
            id="mux-enum-addmatcher-callers",
            category="large_context",
            repo="gorilla-mux",
            prompt=(
                "Across `mux.go` and `route.go`, enumerate every top-level function or method "
                "whose body calls the unexported `(*Route).addMatcher` method (a call of the form "
                "`r.addMatcher(...)`). List the enclosing functions' or methods' names — each name "
                "exactly once — and do not include `addMatcher` itself."
            ),
            schema=CALLERS_SCHEMA,
            grader=Grader("set_match", {"field": "callers", "mode": "equal", "lower": True}),
            gold={
                "callers": [
                    "addRegexpMatcher",
                    "Headers",
                    "HeadersRegexp",
                    "MatcherFunc",
                    "Methods",
                    "Schemes",
                    "Subrouter",
                ],
                "lc_predicate": {
                    "kind": "go_callers",
                    "files": ["mux.go", "route.go"],
                    "target": "addMatcher",
                },
                "verify_decls": [
                    ("route.go", "func (r *Route) addRegexpMatcher(tpl string, typ regexpType) error {"),
                    ("route.go", "func (r *Route) Headers(pairs ...string) *Route {"),
                    ("route.go", "func (r *Route) HeadersRegexp(pairs ...string) *Route {"),
                    ("route.go", "func (r *Route) MatcherFunc(f MatcherFunc) *Route {"),
                    ("route.go", "func (r *Route) Methods(methods ...string) *Route {"),
                    ("route.go", "func (r *Route) Schemes(schemes ...string) *Route {"),
                    ("route.go", "func (r *Route) Subrouter() *Router {"),
                ],
            },
        ),
        Task(
            id="mux-enum-matcher-impls",
            category="large_context",
            repo="gorilla-mux",
            prompt=(
                "In this repository's non-test Go files, enumerate every type that implements the "
                "unexported `matcher` interface — that is, every type (exported or not) that defines "
                "a method `Match(*http.Request, *RouteMatch) bool`. Go interfaces are satisfied "
                "implicitly, so match the method signature, not any declared `matcher` reference. "
                "List each implementing type's name exactly once (the bare type name, without a `*`)."
            ),
            schema=TYPES_SCHEMA,
            grader=Grader("set_match", {"field": "types", "mode": "equal", "lower": True}),
            gold={
                "types": [
                    "Router",
                    "Route",
                    "MatcherFunc",
                    "methodMatcher",
                    "schemeMatcher",
                    "headerMatcher",
                    "headerRegexMatcher",
                    "routeRegexp",
                ],
                "lc_predicate": {
                    "kind": "go_iface",
                    "method": "Match",
                    "params": ["*http.Request", "*RouteMatch"],
                    "ret": "bool",
                },
                "verify_decls": [
                    ("mux.go", "func (r *Router) Match(req *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (r *Route) Match(req *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (m MatcherFunc) Match(r *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (m methodMatcher) Match(r *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (m schemeMatcher) Match(r *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (m headerMatcher) Match(r *http.Request, match *RouteMatch) bool {"),
                    ("route.go", "func (m headerRegexMatcher) Match(r *http.Request, match *RouteMatch) bool {"),
                    ("regexp.go", "func (r *routeRegexp) Match(req *http.Request, match *RouteMatch) bool {"),
                ],
            },
        ),
    ]


def generate(manifest: dict) -> list[Task]:
    """Build the full corpus: manifest-derived tasks plus curated tasks."""
    return [
        *nav_tasks(manifest),
        *callees_tasks(manifest),
        *callers_tasks(manifest),
        *intent_tasks(manifest),
        *diff_tasks(),
        *edit_tasks(),
        *replace_tasks(),
        *routing_tasks(),
        *non_regression_tasks(),
    ]
