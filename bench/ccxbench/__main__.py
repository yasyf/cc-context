"""ccxbench CLI: build the corpus, self-test graders, cross-check cost, run, report.

  python -m ccxbench build-corpus      # generate fixture + tasks/*.json
  python -m ccxbench list-tasks
  python -m ccxbench selftest          # graders pass on gold, fail on wrong (no API cost)
  python -m ccxbench crosscheck FILE   # recomputed cost vs total_cost_usd on a raw envelope
  python -m ccxbench run [filters]     # real runs -> results/<session>/{runs.jsonl,RESULTS.md}
  python -m ccxbench pilot             # tiny real run to validate the harness end to end
  python -m ccxbench report SESSION    # rebuild RESULTS.md from a session's runs.jsonl
"""

from __future__ import annotations

import argparse
import ast
import asyncio
import json
import re
import shutil
import sys
import tempfile
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

from cc_transcript import parse_print_result

from . import fixtures, microbench, report, repos, taskgen
from .arms import apply_edits
from .config import Config, load
from .cost import crosscheck
from .grade import grade, synthetic_result
from .graders import GradeContext, grade_test_run
from .runner import Session, run_corpus
from .types import Task

BENCH_DIR = Path(__file__).resolve().parent.parent
TASKS_DIR = BENCH_DIR / "tasks"
GO_FUNC_RE = re.compile(r"^func (?:\(([^)]*)\)\s*)?([A-Za-z_]\w*)\s*\(")


def needs_go(task: Task) -> bool:
    return task.grader.kind == "test_run" and "go test" in task.grader.spec.get("cmd", "")


def available(task: Task) -> bool:
    return not (needs_go(task) and shutil.which("go") is None)


def build_corpus(cfg: Config) -> list[Task]:
    repos.clone_all(cfg)
    cfg.fixtures_root.mkdir(parents=True, exist_ok=True)
    fixture_dir = cfg.fixtures_root / fixtures.FIXTURE_NAME
    if fixture_dir.exists():
        shutil.rmtree(fixture_dir)
    manifest = fixtures.build(fixture_dir)
    oss = [t for t in (taskgen.oss_tasks() + taskgen.large_context_tasks()) if available(t)]
    verify_oss(cfg, oss)
    fixture_tasks = taskgen.generate(manifest) + taskgen.stale_anchor_tasks(cfg, fixture_dir)
    tasks = [t for t in fixture_tasks if available(t)] + oss
    if TASKS_DIR.exists():
        shutil.rmtree(TASKS_DIR)
    TASKS_DIR.mkdir(parents=True)
    for t in tasks:
        (TASKS_DIR / f"{t.id}.json").write_text(json.dumps(t.to_dict(), indent=2))
    return tasks


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


def recompute_lc_predicate(checkout: Path, pred: dict, repo: str) -> set[str]:
    """Independently recompute a large_context predicate's member set from the checkout."""
    kind = pred["kind"]
    if kind == "py_method":
        src = (checkout / pred["file"]).read_text()
        tree = ast.parse(src)
        members: set[str] = set()
        for node in tree.body:
            if isinstance(node, ast.ClassDef) and not node.name.startswith("_"):
                own = {
                    b.name
                    for b in node.body
                    if isinstance(b, (ast.FunctionDef, ast.AsyncFunctionDef))
                }
                if pred["target"] in own:
                    members.add(node.name)
        return members
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


def verify_oss(cfg: Config, tasks: list[Task]) -> None:
    """Fail loudly at build time if any OSS gold disagrees with its pinned checkout."""
    for t in tasks:
        checkout = cfg.fixtures_root / t.repo
        if not checkout.exists():
            sys.exit(f"OSS task {t.id}: checkout missing {checkout}")
        if t.grader.kind in ("file_line", "file_match"):
            gold_file = checkout / t.gold["file"]
            if not gold_file.exists():
                sys.exit(f"OSS task {t.id}: gold file {t.gold['file']} absent from {t.repo}")
            decl = t.gold.get("verify_decl")
            if t.grader.kind == "file_line" and decl:
                lines = gold_file.read_text().splitlines()
                tol = int(t.grader.spec.get("line_tolerance", 2))
                lo, hi = max(0, t.gold["line"] - 1 - tol), min(len(lines), t.gold["line"] + tol)
                if not any(decl in ln for ln in lines[lo:hi]):
                    sys.exit(f"OSS task {t.id}: decl {decl!r} not within ±{tol} of line {t.gold['line']} in {t.gold['file']}")
        if t.grader.kind == "set_match" and "verify_decls" in t.gold:
            field = t.grader.spec.get("field", "items")
            decls_by_member: dict[str, str] = {}
            for rel, decl in t.gold["verify_decls"]:
                decl_file = checkout / rel
                if not decl_file.exists():
                    sys.exit(f"OSS task {t.id}: decl file {rel} absent from {t.repo}")
                if decl not in decl_file.read_text():
                    sys.exit(f"OSS task {t.id}: gold decl {decl!r} not found in {rel}")
                decls_by_member[decl] = rel
            for member in t.gold[field]:
                pat = re.compile(rf"\b{re.escape(member)}\b")
                if not any(pat.search(decl) for decl in decls_by_member):
                    sys.exit(f"OSS task {t.id}: gold member {member!r} has no matching verify_decls entry")
            if "lc_predicate" in t.gold:
                recomputed = recompute_lc_predicate(checkout, t.gold["lc_predicate"], t.repo)
                gold_set = {m.lower() for m in t.gold[field]}
                if {m.lower() for m in recomputed} != gold_set:
                    sys.exit(
                        f"OSS task {t.id}: predicate recompute {sorted(recomputed)} "
                        f"!= gold {sorted(t.gold[field])}"
                    )
        for edits in (t.setup.get("edits", []), t.gold.get("solution_edits", [])):
            for e in edits:
                text = (checkout / e["file"]).read_text()
                if e["find"] not in text:
                    sys.exit(f"OSS task {t.id}: edit find {e['find']!r} absent from {e['file']}")


def load_corpus() -> list[Task]:
    if not TASKS_DIR.exists() or not any(TASKS_DIR.glob("*.json")):
        sys.exit("no corpus; run `python -m ccxbench build-corpus` first")
    return [Task.from_dict(json.loads(p.read_text())) for p in sorted(TASKS_DIR.glob("*.json"))]


def correct_answer(task: Task) -> dict[str, object]:
    g = task.gold
    k = task.grader.kind
    if k == "file_line":
        return {"file": g["file"], "line": g["line"]}
    if k == "file_match":
        return {"file": g["file"]}
    if k == "set_match":
        field = task.grader.spec.get("field", "items")
        return {field: g[field]}
    if k == "keywords":
        if g.get("groups"):
            return {"answer": " ".join(group[0] for group in g["groups"])}
        return {"answer": " ".join(g["keywords"])}
    return {}


def wrong_answer(task: Task) -> dict[str, object]:
    k = task.grader.kind
    if k == "file_line":
        return {"file": "does/not/exist.go", "line": 999}
    if k == "file_match":
        alt = "docs/guide.md" if task.gold["file"] != "docs/guide.md" else "go.mod"
        return {"file": alt}
    if k == "set_match":
        field = task.grader.spec.get("field", "items")
        return {field: ["DefinitelyNotASymbol"]}
    if k == "keywords":
        return {"answer": "completely unrelated filler content"}
    return {"unexpected": True}


def checkout_dir(cfg: Config, task: Task) -> Path:
    return cfg.fixtures_root / (fixtures.FIXTURE_NAME if task.repo == "fixture" else task.repo)


def selftest(cfg: Config) -> int:
    tasks = build_corpus(cfg)
    fails: list[str] = []
    by_cat: Counter[str] = Counter()
    for t in tasks:
        by_cat[t.category] += 1
        if t.grader.kind == "test_run":
            ok = selftest_edit(t, checkout_dir(cfg, t))
        else:
            good = grade(t, synthetic_result(correct_answer(t)), None)
            bad = grade(t, synthetic_result(wrong_answer(t)), None)
            ok = good.correct and not bad.correct
            if not good.correct:
                fails.append(f"{t.id}: gold answer graded INCORRECT ({good.detail})")
            if bad.correct:
                fails.append(f"{t.id}: wrong answer graded CORRECT (grader can't fail)")
        if not ok and t.grader.kind == "test_run":
            fails.append(f"{t.id}: test_run selftest failed")

    print(f"corpus: {len(tasks)} tasks across {len(by_cat)} categories")
    for cat, n in sorted(by_cat.items()):
        print(f"  {cat:16} {n}")
    if fails:
        print(f"\nFAIL ({len(fails)}):")
        for f in fails:
            print(f"  - {f}")
        return 1
    print("\nall graders pass on gold and fail on wrong answers")
    return 0


def selftest_edit(task: Task, src_dir: Path) -> bool:
    """Apply the task's known solution to a fresh checkout; the test_run grader must pass,
    and an unmodified checkout must fail."""
    solution = task.gold.get("solution_edits", [])
    if not solution:
        print(f"  WARN {task.id}: no solution_edits to self-test test_run grader")
        return True
    with tempfile.TemporaryDirectory() as tmp:
        good_dir = Path(tmp) / "good"
        bad_dir = Path(tmp) / "bad"
        shutil.copytree(src_dir, good_dir)
        shutil.copytree(src_dir, bad_dir)
        for e in solution:
            p = good_dir / e["file"]
            p.write_text(p.read_text().replace(e["find"], e["replace"], 1))
        ctx_good = GradeContext(result_text="", workdir=good_dir)
        ctx_bad = GradeContext(result_text="", workdir=bad_dir)
        good = grade_test_run({}, task.gold, task.grader.spec, ctx_good)
        bad = grade_test_run({}, task.gold, task.grader.spec, ctx_bad)
        if not good.correct:
            print(f"  {task.id}: solution did NOT pass test_run ({good.detail})")
        if bad.correct:
            print(f"  {task.id}: unmodified repo PASSED test_run (grader can't fail)")
        return good.correct and not bad.correct


def cmd_crosscheck(cfg: Config, path: Path) -> int:
    pr = parse_print_result(path.read_bytes())
    model = next(iter(pr.model_usage), "")
    cc = crosscheck(pr, model, cfg.cost_tolerance)
    print(f"models:     {list(pr.model_usage)}")
    print(f"reported:   ${cc.reported_usd:.6f}")
    print(f"recomputed: ${cc.recomputed_usd:.6f}")
    print(f"rel_delta:  {cc.rel_delta:.4f} (tolerance {cfg.cost_tolerance})")
    print(f"within:     {cc.within_tolerance}")
    if cc.note:
        print(f"note:       {cc.note}")
    return 0 if cc.within_tolerance else 1


def select(tasks: list[Task], args: argparse.Namespace) -> list[Task]:
    if args.tasks:
        wanted = set(args.tasks.split(","))
        tasks = [t for t in tasks if t.id in wanted]
    if args.categories:
        cats = set(args.categories.split(","))
        tasks = [t for t in tasks if t.category in cats]
    if args.sample:
        seen: Counter[str] = Counter()
        picked = []
        for t in tasks:
            if seen[t.category] < args.sample:
                picked.append(t)
                seen[t.category] += 1
        tasks = picked
    if args.limit:
        tasks = tasks[: args.limit]
    return tasks


def cmd_run(cfg: Config, args: argparse.Namespace) -> int:
    if args.models:
        cfg = replace_models(cfg, args.models.split(","))
    if args.repeats:
        cfg = replace_repeats(cfg, args.repeats)
    if args.budget is not None:
        cfg = replace_budget(cfg, args.budget)
    tasks = select(load_corpus(), args)
    if not tasks:
        sys.exit("no tasks selected")
    session_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    sess = Session(cfg=cfg, session_id=session_id)
    runs = len(tasks) * len(cfg.models) * cfg.repeats * 2
    print(f"session {session_id}: {len(tasks)} tasks x {len(cfg.models)} models x {cfg.repeats} repeats x 2 arms = {runs} runs")
    print(f"budget cap: ${cfg.budget_usd:.2f}")
    records = asyncio.run(run_corpus(sess, tasks))
    md = report.write_report(sess.jsonl_path, cfg.results_dir / session_id / "RESULTS.md")
    print(f"\nspent: ${sess.spent_usd:.4f} over {len(records)} runs")
    print(f"report: {cfg.results_dir / session_id / 'RESULTS.md'}")
    print("\n" + md.split("## Integrity")[0].split("### Token")[0])
    return 0


def replace_models(cfg: Config, models: list[str]) -> Config:
    return Config(**{**cfg.__dict__, "models": tuple(models)})


def replace_repeats(cfg: Config, repeats: int) -> Config:
    return Config(**{**cfg.__dict__, "repeats": repeats})


def replace_budget(cfg: Config, budget_usd: float) -> Config:
    return Config(**{**cfg.__dict__, "budget_usd": budget_usd})


def cmd_pilot(cfg: Config, args: argparse.Namespace) -> int:
    args.categories = "navigation,callees,diff_review,targeted_edit,structural_replace,structural_search,non_regression,intent_search"
    args.sample = 1
    args.tasks = None
    args.limit = None
    if not args.repeats:
        args.repeats = 1
    return cmd_run(cfg, args)


def main(argv: list[str] | None = None) -> int:
    cfg = load()
    p = argparse.ArgumentParser(prog="ccxbench")
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("build-corpus")
    sub.add_parser("list-tasks")
    sub.add_parser("selftest")

    cc = sub.add_parser("crosscheck")
    cc.add_argument("path", type=Path)

    for name in ("run", "pilot"):
        rp = sub.add_parser(name)
        rp.add_argument("--tasks", help="comma-separated task ids")
        rp.add_argument("--categories", help="comma-separated categories")
        rp.add_argument("--sample", type=int, help="max tasks per category")
        rp.add_argument("--limit", type=int, help="max total tasks")
        rp.add_argument("--models", help="override config models (comma-separated)")
        rp.add_argument("--repeats", type=int, help="override config repeats")
        rp.add_argument("--budget", type=float, help="override config budget_usd ceiling")

    rep = sub.add_parser("report")
    rep.add_argument("session")

    mb = sub.add_parser("microbench")
    mb.add_argument("--repo")

    args = p.parse_args(argv)

    if args.cmd == "build-corpus":
        tasks = build_corpus(cfg)
        print(f"built fixture + {len(tasks)} tasks -> {TASKS_DIR}")
        return 0
    if args.cmd == "list-tasks":
        tasks = load_corpus()
        by_cat = Counter(t.category for t in tasks)
        print(f"{len(tasks)} tasks:")
        for cat, n in sorted(by_cat.items()):
            print(f"  {cat:16} {n}")
        return 0
    if args.cmd == "selftest":
        return selftest(cfg)
    if args.cmd == "crosscheck":
        return cmd_crosscheck(cfg, args.path)
    if args.cmd == "run":
        return cmd_run(cfg, args)
    if args.cmd == "pilot":
        return cmd_pilot(cfg, args)
    if args.cmd == "report":
        jsonl = cfg.results_dir / args.session / "runs.jsonl"
        report.write_report(jsonl, cfg.results_dir / args.session / "RESULTS.md")
        print(f"wrote {cfg.results_dir / args.session / 'RESULTS.md'}")
        return 0
    if args.cmd == "microbench":
        return microbench.cmd_microbench(cfg, args)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
