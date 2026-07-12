"""ccxbench CLI: build the corpus, self-test graders, run, report.

  python -m ccxbench build-corpus      # generate fixture + tasks/*.json
  python -m ccxbench list-tasks
  python -m ccxbench selftest          # graders pass on gold, fail on wrong (no API cost)
  python -m ccxbench run [filters]     # real runs -> results/<session>/{runs.jsonl,RESULTS.md}
  python -m ccxbench pilot             # tiny real run to validate the harness end to end
  python -m ccxbench report SESSION    # rebuild RESULTS.md from a session's runs.jsonl
"""

from __future__ import annotations

import argparse
import asyncio
import json
import shutil
import sys
import tempfile
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

from . import goldgen, microbench, report, repos, taskgen
from .config import Config, load
from .grade import grade, synthetic_result
from .graders import GradeContext, grade_test_run
from .runner import Session, run_corpus
from .types import ARMS, Task

BENCH_DIR = Path(__file__).resolve().parent.parent
TASKS_DIR = BENCH_DIR / "tasks"
PATCHES_DIR = TASKS_DIR / "patches"


def needs_go(task: Task) -> bool:
    return task.grader.kind == "test_run" and "go test" in task.grader.spec.get("cmd", "")


def require_go(tasks: list[Task]) -> None:
    """Abort the build loudly if a task needs the Go toolchain but `go` is absent — never
    silently shrink the corpus."""
    if any(needs_go(t) for t in tasks) and shutil.which("go") is None:
        sys.exit("go toolchain required for go-test tasks but `go` is not on PATH")


def derive_golds(cfg: Config, tasks: list[Task]) -> None:
    """Fill every headline task's gold from its pinned checkout, and generate diff patches.

    Navigation/trace lines, large_context member sets, and diff_review symbol sets are all
    recomputed here — nothing is transcribed by hand — so a gold that drifts from its repo fails
    loudly at build time. Diff patches are (re)written to `tasks/patches/` deterministically.
    """
    PATCHES_DIR.mkdir(parents=True, exist_ok=True)
    for stale in PATCHES_DIR.glob("*.patch"):
        stale.unlink()
    for t in tasks:
        checkout = checkout_dir(cfg, t)
        if t.category in ("navigation", "trace"):
            t.gold["line"] = goldgen.resolve_decl_line(checkout, t.gold["file"], t.gold["decl"])
            for alt in t.gold.get("alt_sites", []):
                alt["line"] = goldgen.resolve_decl_line(checkout, alt["file"], alt["decl"])
        elif t.category == "large_context":
            members = goldgen.recompute_lc_predicate(checkout, t.gold["lc_predicate"], t.repo)
            if not members:
                sys.exit(f"task {t.id}: predicate matched no members in {t.repo}")
            t.gold["members"] = sorted(members)
        elif t.category == "diff_review":
            patch, files = goldgen.make_patch(checkout, t.gold["diff_spec"]["edits"])
            goldgen.check_patch_applies(checkout, patch)
            (PATCHES_DIR / f"{t.id}.patch").write_text(patch)
            symbols = goldgen.symbols_changed_by_patch(checkout, patch)
            if not symbols:
                sys.exit(f"task {t.id}: patch touched no attributable functions")
            t.gold["symbols"] = sorted(symbols)
            t.gold["traversal_files"] = files


def build_corpus(cfg: Config) -> list[Task]:
    repos.clone_all(cfg)
    cfg.fixtures_root.mkdir(parents=True, exist_ok=True)
    tasks = taskgen.all_tasks()
    require_go(tasks)
    derive_golds(cfg, [t for t in tasks if t.repo != "empty"])
    verify_oss(cfg, [t for t in tasks if t.repo != "empty"])
    TASKS_DIR.mkdir(parents=True, exist_ok=True)
    for stale in TASKS_DIR.glob("*.json"):  # non-recursive: committed patches/ survives
        stale.unlink()
    for t in tasks:
        (TASKS_DIR / f"{t.id}.json").write_text(json.dumps(t.to_dict(), indent=2))
    return tasks


def print_floor_table(cfg: Config, tasks: list[Task]) -> list[str]:
    """Print the per-headline-task traversal-bytes floor table; return under-floor failure lines."""
    rows = goldgen.floor_rows(cfg.min_traversal_bytes, tasks, lambda t: checkout_dir(cfg, t))
    print(f"\nsize floor: gold.traversal_files must total >= {cfg.min_traversal_bytes} bytes")
    print(f"  {'task':34}{'family':20}{'repo':14}{'bytes':>10}  verdict")
    for r in rows:
        print(f"  {r.task_id:34}{r.family:20}{r.repo:14}{r.nbytes:>10}  {'ok' if r.ok else 'UNDER'}")
    return [f"{r.task_id}: traversal {r.nbytes} < floor {cfg.min_traversal_bytes}" for r in rows if not r.ok]


def verify_oss(cfg: Config, tasks: list[Task]) -> None:
    """Fail loudly at build time if any derived OSS gold disagrees with its pinned checkout."""
    for t in tasks:
        checkout = cfg.fixtures_root / t.repo
        if not checkout.exists():
            sys.exit(f"OSS task {t.id}: checkout missing {checkout}")
        for rel in t.traversal_files:
            if not (checkout / rel).is_file():
                sys.exit(f"OSS task {t.id}: traversal_file {rel} absent from {t.repo}")
        if t.grader.kind == "file_line":
            tol = int(t.grader.spec.get("line_tolerance", 2))
            for site in (t.gold, *t.gold.get("alt_sites", [])):
                lines = (checkout / site["file"]).read_text().splitlines()
                lo, hi = max(0, site["line"] - 1 - tol), min(len(lines), site["line"] + tol)
                if not any(site["decl"] in ln for ln in lines[lo:hi]):
                    sys.exit(f"OSS task {t.id}: decl {site['decl']!r} not within ±{tol} of line {site['line']} in {site['file']}")
        elif t.grader.kind == "file_match":
            if not (checkout / t.gold["file"]).is_file():
                sys.exit(f"OSS task {t.id}: gold file {t.gold['file']} absent from {t.repo}")
        elif t.grader.kind == "set_match" and "lc_predicate" in t.gold:
            recomputed = {m.lower() for m in goldgen.recompute_lc_predicate(checkout, t.gold["lc_predicate"], t.repo)}
            if recomputed != {m.lower() for m in t.gold["members"]}:
                sys.exit(f"OSS task {t.id}: predicate recompute {sorted(recomputed)} != gold {t.gold['members']}")
        for e in t.gold.get("solution_edits", []):
            if e["find"] not in (checkout / e["file"]).read_text():
                sys.exit(f"OSS task {t.id}: solution find {e['find']!r} absent from {e['file']}")


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
    return cfg.fixtures_root / task.repo


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

    fails += print_floor_table(cfg, tasks)

    if fails:
        print(f"\nFAIL ({len(fails)}):")
        for f in fails:
            print(f"  - {f}")
        return 1
    print("\nall graders pass on gold and fail on wrong answers; all headline tasks clear the floor")
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
        # test_run graders build/run source only; skip .git (its live fsmonitor socket is uncopyable).
        no_git = shutil.ignore_patterns(".git")
        shutil.copytree(src_dir, good_dir, ignore=no_git)
        shutil.copytree(src_dir, bad_dir, ignore=no_git)
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
    if args.ceiling is not None:
        cfg = replace_ceiling(cfg, args.ceiling)
    tasks = select(load_corpus(), args)
    if not tasks:
        sys.exit("no tasks selected")
    session_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    sess = Session(cfg=cfg, session_id=session_id)
    runs = len(tasks) * len(cfg.models) * cfg.repeats * len(ARMS)
    print(f"session {session_id}: {len(tasks)} tasks x {len(cfg.models)} models x {cfg.repeats} repeats x {len(ARMS)} arms = {runs} runs")
    print(f"safety ceiling: ${cfg.safety_ceiling_usd:.2f}")
    records = asyncio.run(run_corpus(sess, tasks, concurrency=args.concurrency))
    md = report.write_report(sess.jsonl_path, cfg.results_dir / session_id / "RESULTS.md")
    print(f"\nspent: ${sess.spent_usd:.4f} over {len(records)} runs")
    print(f"report: {cfg.results_dir / session_id / 'RESULTS.md'}")
    print("\n" + md.split("## Integrity")[0].split("### Token")[0])
    return 0


def replace_models(cfg: Config, models: list[str]) -> Config:
    return Config(**{**cfg.__dict__, "models": tuple(models)})


def replace_repeats(cfg: Config, repeats: int) -> Config:
    return Config(**{**cfg.__dict__, "repeats": repeats})


def replace_ceiling(cfg: Config, ceiling_usd: float) -> Config:
    return Config(**{**cfg.__dict__, "safety_ceiling_usd": ceiling_usd})


def cmd_pilot(cfg: Config, args: argparse.Namespace) -> int:
    args.categories = "navigation,trace,large_context,diff_review,targeted_edit,intent_search,non_regression"
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

    for name in ("run", "pilot"):
        rp = sub.add_parser(name)
        rp.add_argument("--tasks", help="comma-separated task ids")
        rp.add_argument("--categories", help="comma-separated categories")
        rp.add_argument("--sample", type=int, help="max tasks per category")
        rp.add_argument("--limit", type=int, help="max total tasks")
        rp.add_argument("--models", help="override config models (comma-separated)")
        rp.add_argument("--repeats", type=int, help="override config repeats")
        rp.add_argument("--ceiling", type=float, help="override config safety_ceiling_usd")
        rp.add_argument("--concurrency", type=int, default=1, help="parallel in-flight runs (default 1, serial)")

    rep = sub.add_parser("report")
    rep.add_argument("session")

    mb = sub.add_parser("microbench")
    mb.add_argument("--repo")

    args = p.parse_args(argv)

    if args.cmd == "build-corpus":
        tasks = build_corpus(cfg)
        print(f"built {len(tasks)} tasks -> {TASKS_DIR}")
        rows = goldgen.floor_rows(cfg.min_traversal_bytes, tasks, lambda t: checkout_dir(cfg, t))
        under = [r for r in rows if not r.ok]
        for r in under:
            print(
                f"FLOOR VIOLATION: {r.task_id} ({r.family}, {r.repo}) traversal {r.nbytes} < {cfg.min_traversal_bytes}",
                file=sys.stderr,
            )
        return 1 if under else 0
    if args.cmd == "list-tasks":
        tasks = load_corpus()
        by_cat = Counter(t.category for t in tasks)
        print(f"{len(tasks)} tasks:")
        for cat, n in sorted(by_cat.items()):
            print(f"  {cat:16} {n}")
        return 0
    if args.cmd == "selftest":
        return selftest(cfg)
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
