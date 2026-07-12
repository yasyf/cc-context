"""Build-time gold derivation and the traversal-bytes size floor.

Every OSS gold is re-derived here from the pinned checkout — line numbers and member sets
are recomputed, never hand-transcribed — so a gold that drifts from its repo fails loudly at
build time. `traversal_bytes` measures the naive-context floor a headline task must clear.
"""

from __future__ import annotations

import ast
import difflib
import fnmatch
import re
import shutil
import subprocess
import sys
import tempfile
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .types import DIAGNOSTIC_CATEGORY, Task

CONTROL_CATEGORY = "non_regression"

GO_FUNC_RE = re.compile(r"^func (?:\(([^)]*)\)\s*)?([A-Za-z_]\w*)\s*\(")


def go_funcs(text: str) -> list[tuple[str, str]]:
    """Brace-scan a Go file into (func_name, body_below_signature) pairs for top-level funcs."""
    lines = text.splitlines(keepends=True)
    out: list[tuple[str, str]] = []
    i, n = 0, len(lines)
    while i < n:
        m = GO_FUNC_RE.match(lines[i])
        if not m:
            i += 1
            continue
        depth, started, body, j = 0, False, [], i
        while j < n:
            for ch in lines[j]:
                if ch == "{":
                    depth += 1
                    started = True
                elif ch == "}":
                    depth -= 1
            body.append(lines[j])
            if started and depth == 0:
                break
            j += 1
        out.append((m.group(2), "".join(body[1:])))
        i = j + 1
    return out


def resolve_decl_line(checkout: Path, rel: str, decl: str) -> int:
    """1-based line of the unique line containing `decl` in `checkout/rel`.

    Fails loud unless `decl` matches exactly one line, so an ambiguous or drifted navigation
    anchor never yields a silently-wrong gold line number.
    """
    lines = (checkout / rel).read_text().splitlines()
    hits = [i + 1 for i, ln in enumerate(lines) if decl in ln]
    if len(hits) != 1:
        sys.exit(f"decl {decl!r} in {rel}: expected exactly 1 match, found lines {hits}")
    return hits[0]


def _base_name(base: ast.expr) -> str:
    """The rightmost identifier of a class base — bare (`Configurable`) or dotted (`x.Configurable`)."""
    return base.attr if isinstance(base, ast.Attribute) else getattr(base, "id", "")


def _predicate_files(checkout: Path, pred: dict) -> list[Path]:
    """Resolve a Python predicate's target files: a single `file`, or `files` glob patterns minus
    `exclude` glob patterns (matched against the checkout-relative path).

    Globs let a predicate span a whole package — `tornado/**/*.py` minus `tornado/test/*` — while a
    literal path globs to itself, so the single-file and multi-file forms share one resolver.
    """
    if "file" in pred:
        return [checkout / pred["file"]]
    excluded = pred.get("exclude", ())
    paths = sorted({p for pat in pred["files"] for p in checkout.glob(pat)})
    return [p for p in paths if not any(fnmatch.fnmatch(str(p.relative_to(checkout)), e) for e in excluded)]


def recompute_lc_predicate(checkout: Path, pred: dict, repo: str) -> set[str]:
    """Independently recompute a large_context predicate's member set from the checkout."""
    kind = pred["kind"]
    if kind == "py_subclass":
        base = pred["base"]
        members: set[str] = set()
        for path in _predicate_files(checkout, pred):
            for node in ast.parse(path.read_text()).body:
                if isinstance(node, ast.ClassDef) and not node.name.startswith("_"):
                    if base in [_base_name(b) for b in node.bases]:
                        members.add(node.name)
        return members
    if kind == "py_method":
        members: set[str] = set()
        for path in _predicate_files(checkout, pred):
            for node in ast.parse(path.read_text()).body:
                if isinstance(node, ast.ClassDef) and not node.name.startswith("_"):
                    own = {
                        b.name
                        for b in node.body
                        if isinstance(b, (ast.FunctionDef, ast.AsyncFunctionDef))
                    }
                    if pred["target"] in own:
                        members.add(node.name)
        return members
    if kind == "py_subclass_closure":
        base = pred["base"]
        children: dict[str, set[str]] = defaultdict(set)
        public: dict[str, bool] = {}
        for path in _predicate_files(checkout, pred):
            for node in ast.parse(path.read_text()).body:
                if isinstance(node, ast.ClassDef):
                    public[node.name] = not node.name.startswith("_")
                    for b in node.bases:
                        name = _base_name(b)
                        if name:
                            children[name].add(node.name)
        seen: set[str] = set()
        frontier = [base]
        while frontier:
            for child in children.get(frontier.pop(), ()):
                if child not in seen:
                    seen.add(child)
                    frontier.append(child)
        return {n for n in seen if public.get(n, False)}
    if kind == "go_callers":
        target = pred["target"]
        call = re.compile(rf"\b{re.escape(target)}\s*\(")
        members = set()
        for rel in pred["files"]:
            for name, body in go_funcs((checkout / rel).read_text()):
                if name != target and call.search(body):
                    members.add(name)
        return members
    if kind == "go_iface":
        method = pred["method"]
        # A param may be named (`req *http.Request`) or bare (`*http.Request`); match both.
        params = r"\s*,\s*".join(rf"(?:\w+\s+)?{re.escape(p)}" for p in pred["params"])
        impl = re.compile(
            rf"func\s+\(\s*\w+\s+\*?([A-Za-z_]\w*)\s*\)\s+{re.escape(method)}\s*\(\s*{params}\s*\)\s+{re.escape(pred['ret'])}\b"
        )
        members = set()
        for go in sorted(checkout.glob("*.go")):
            if go.name.endswith("_test.go"):
                continue
            for m in impl.finditer(go.read_text()):
                members.add(m.group(1))
        return members
    sys.exit(f"unknown lc_predicate kind {kind!r} (repo {repo})")


def traversal_bytes(checkout: Path, task: Task) -> int:
    """Sum the on-disk byte sizes of a task's `gold.traversal_files` within its pinned checkout.

    These are the files a naive baseline must traverse; their total is the size floor a headline
    task must clear. Fails loud if a declared traversal file is absent from the checkout.
    """
    total = 0
    for rel in task.traversal_files:
        p = checkout / rel
        if not p.is_file():
            raise FileNotFoundError(f"task {task.id}: traversal_file {rel!r} absent from {checkout}")
        total += p.stat().st_size
    return total


@dataclass(frozen=True)
class FloorRow:
    """One headline task's floor verdict: its traversal-byte total and whether it clears the floor.

    `exempt` marks a task the floor is waived for — a single-file control or a whole small repo
    genuinely below the floor — which always passes regardless of its byte total.
    """

    task_id: str
    family: str
    repo: str
    nbytes: int
    ok: bool
    exempt: bool = False


def floor_rows(min_bytes: int, tasks: list[Task], resolve: Callable[[Task], Path]) -> list[FloorRow]:
    """Per headline task (control and diagnostic families excluded), its traversal bytes and verdict.

    `resolve` maps a task to its pinned checkout root. A task clears the floor when the sum of its
    `gold.traversal_files` byte sizes is at least `min_bytes`, or when it is `floor_exempt`.
    """
    rows: list[FloorRow] = []
    for t in tasks:
        if t.category in (CONTROL_CATEGORY, DIAGNOSTIC_CATEGORY):
            continue
        n = traversal_bytes(resolve(t), t)
        rows.append(FloorRow(t.id, t.category, t.repo, n, t.floor_exempt or n >= min_bytes, t.floor_exempt))
    return rows


def py_func_ranges(text: str) -> list[tuple[str, int, int]]:
    """Every def/async-def in a Python source as (name, start_line, end_line), any nesting depth."""
    tree = ast.parse(text)
    return [
        (n.name, n.lineno, n.end_lineno)
        for n in ast.walk(tree)
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
    ]


def enclosing_symbol(ranges: list[tuple[str, int, int]], line: int) -> str | None:
    """Name of the innermost function whose line span contains `line`, or None if none does."""
    best: tuple[str, int, int] | None = None
    for name, start, end in ranges:
        if start <= line <= end and (best is None or start > best[1]):
            best = (name, start, end)
    return best[0] if best else None


def make_patch(checkout: Path, edits: list[dict]) -> tuple[str, list[str]]:
    """Deterministically transform the pinned checkout into a git-apply-able unified diff.

    Each edit is a unique `find` line replaced by `replace` in its file; files are diffed in
    sorted order with no timestamps, so two runs yield byte-identical patches. Returns the patch
    text and the sorted list of pre-image files it touches (the diff-review traversal set).
    """
    files = sorted({e["file"] for e in edits})
    chunks: list[str] = []
    for rel in files:
        pre = (checkout / rel).read_text()
        post = pre
        for e in edits:
            if e["file"] != rel:
                continue
            if pre.count(e["find"]) != 1:
                sys.exit(f"diff edit find {e['find']!r} in {rel}: expected 1 match, found {pre.count(e['find'])}")
            post = post.replace(e["find"], e["replace"], 1)
        chunks.append(
            "".join(
                difflib.unified_diff(
                    pre.splitlines(keepends=True),
                    post.splitlines(keepends=True),
                    fromfile=f"a/{rel}",
                    tofile=f"b/{rel}",
                )
            )
        )
    return "".join(chunks), files


def symbols_changed_by_patch(checkout: Path, patch_text: str) -> set[str]:
    """Recompute a diff's gold — the set of functions/methods it modified — from the patch itself.

    Walks each hunk, maps every removed pre-image line to its enclosing function (by AST span),
    and unions the names. Derived from the patch, so the gold can never drift from the diff.
    """
    changed: dict[str, set[int]] = defaultdict(set)
    rel: str | None = None
    old = 0
    for line in patch_text.splitlines():
        if line.startswith("--- a/"):
            rel = line[len("--- a/") :]
        elif line.startswith("@@"):
            old = int(re.match(r"@@ -(\d+)", line).group(1))
        elif line.startswith("-"):
            changed[rel].add(old)
            old += 1
        elif line.startswith("+"):
            continue
        else:
            old += 1
    syms: set[str] = set()
    for rel, lines in changed.items():
        if not rel.endswith(".py"):
            sys.exit(f"diff attribution unsupported for {rel!r} (only Python)")
        ranges = py_func_ranges((checkout / rel).read_text())
        for ln in lines:
            name = enclosing_symbol(ranges, ln)
            if name:
                syms.add(name)
    return syms


def check_patch_applies(checkout: Path, patch_text: str) -> None:
    """Fail loud unless the generated patch applies cleanly to a fresh copy of the checkout."""
    with tempfile.TemporaryDirectory() as tmp:
        wd = Path(tmp) / "wd"
        shutil.copytree(checkout, wd, ignore=shutil.ignore_patterns(".git"))
        subprocess.run(["git", "init", "-q"], cwd=wd, check=True, capture_output=True)
        subprocess.run(["git", "add", "-A"], cwd=wd, check=True, capture_output=True)
        pf = Path(tmp) / "generated.patch"
        pf.write_text(patch_text)
        chk = subprocess.run(["git", "apply", "--check", str(pf)], cwd=wd, capture_output=True, text=True)
        if chk.returncode != 0:
            sys.exit(f"generated patch does not apply cleanly: {chk.stderr.strip()}")
