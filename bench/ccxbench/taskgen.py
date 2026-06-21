"""Generate the benchmark corpus from the fixture manifest plus curated tasks.

Navigation/callees/callers/intent tasks are derived directly from the manifest, so
their gold answers are ground truth by construction. diff_review, targeted_edit, and
non_regression tasks are curated literals: diff tasks apply an uncommitted edit whose
changed symbols are the gold; edit tasks are graded by running a check in the post-run
workdir; non_regression tasks (ccx_helps=False) prove ccx adds no harm where it can't help.
"""

from __future__ import annotations

from .types import Grader, Task

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

REPO = "fixture"


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


def generate(manifest: dict) -> list[Task]:
    """Build the full corpus: manifest-derived tasks plus curated tasks."""
    return [
        *nav_tasks(manifest),
        *callees_tasks(manifest),
        *callers_tasks(manifest),
        *intent_tasks(manifest),
        *diff_tasks(),
        *edit_tasks(),
        *non_regression_tasks(),
    ]
