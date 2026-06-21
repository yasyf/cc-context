"""Deterministic graders over a run's structured output.

Each grader takes the model's structured answer, the task's gold reference, the
grader spec, and a context (result text + post-run workdir for test_run), and returns
a strict pass/fail with a detail string. Determinism keeps grading off the model under
test; an LLM judge lives in grade.py for the rare open-ended task.
"""

from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .types import GradeResult


@dataclass(frozen=True)
class GradeContext:
    result_text: str
    workdir: Path | None


def as_dict(answer: object) -> dict[str, Any] | None:
    return answer if isinstance(answer, dict) else None


def norm_path(p: str) -> str:
    return p.strip().lstrip("./").replace("\\", "/")


def file_matches(gold_file: str, ans_file: str) -> bool:
    """A lenient path match: same basename, or one path is a suffix of the other."""
    g, a = norm_path(gold_file), norm_path(ans_file)
    if not a:
        return False
    if g == a or g.endswith("/" + a) or a.endswith("/" + g):
        return True
    return os.path.basename(g) == os.path.basename(a)


def to_str_set(value: object, lower: bool) -> set[str]:
    if isinstance(value, str):
        items = [value]
    elif isinstance(value, (list, tuple)):
        items = [str(v) for v in value]
    else:
        return set()
    return {(s.strip().lower() if lower else s.strip()) for s in items if s.strip()}


def grade_file_line(answer: object, gold: dict[str, Any], spec: dict[str, Any], ctx: GradeContext) -> GradeResult:
    d = as_dict(answer)
    if d is None:
        return GradeResult(False, "no structured object")
    file_field = spec.get("file_field", "file")
    line_field = spec.get("line_field", "line")
    tol = int(spec.get("line_tolerance", 2))
    ans_file = str(d.get(file_field, ""))
    if not file_matches(gold["file"], ans_file):
        return GradeResult(False, f"file {ans_file!r} != gold {gold['file']!r}")
    try:
        ans_line = int(d.get(line_field))
    except (TypeError, ValueError):
        return GradeResult(False, f"line not an int: {d.get(line_field)!r}")
    if abs(ans_line - int(gold["line"])) > tol:
        return GradeResult(False, f"line {ans_line} not within {tol} of {gold['line']}")
    return GradeResult(True, f"{ans_file}:{ans_line} ~ {gold['file']}:{gold['line']}")


def grade_file_match(answer: object, gold: dict[str, Any], spec: dict[str, Any], ctx: GradeContext) -> GradeResult:
    d = as_dict(answer)
    if d is None:
        return GradeResult(False, "no structured object")
    ans_file = str(d.get(spec.get("file_field", "file"), ""))
    ok = file_matches(gold["file"], ans_file)
    return GradeResult(ok, f"{ans_file!r} {'==' if ok else '!='} {gold['file']!r}")


def grade_set_match(answer: object, gold: dict[str, Any], spec: dict[str, Any], ctx: GradeContext) -> GradeResult:
    d = as_dict(answer)
    if d is None:
        return GradeResult(False, "no structured object")
    field = spec.get("field", "items")
    mode = spec.get("mode", "equal")
    lower = bool(spec.get("lower", True))
    want = to_str_set(gold.get(field, gold.get("items", [])), lower)
    got = to_str_set(d.get(field, []), lower)
    if mode == "subset":
        ok = want.issubset(got)
    elif mode == "superset":
        ok = want.issubset(got)
    else:
        ok = want == got
    return GradeResult(ok, f"{mode}: got={sorted(got)} want={sorted(want)}")


def grade_keywords(answer: object, gold: dict[str, Any], spec: dict[str, Any], ctx: GradeContext) -> GradeResult:
    """Keyword grade. With gold["groups"], every group must contribute >=1 hit (any-of within
    a group, all-of across groups) — robust to paraphrase while rejecting off-topic answers.
    Otherwise falls back to gold["keywords"] with spec["min_hits"]."""
    d = as_dict(answer)
    field = spec.get("field")
    if field and d is not None:
        haystack = str(d.get(field, ""))
    elif field is None:
        haystack = ctx.result_text
    else:
        haystack = ""
    hay = haystack.lower()

    groups = gold.get("groups")
    if groups:
        per_group = [[k for k in (s.lower() for s in group) if k in hay] for group in groups]
        ok = all(per_group)
        missed = [groups[i] for i, hits in enumerate(per_group) if not hits]
        return GradeResult(ok, f"groups hit={[bool(h) for h in per_group]} missed={missed}")

    needed = [k.lower() for k in gold.get("keywords", [])]
    min_hits = int(spec.get("min_hits", len(needed)))
    hits = [k for k in needed if k in hay]
    ok = len(hits) >= min_hits
    return GradeResult(ok, f"{len(hits)}/{len(needed)} keywords (need {min_hits}): hit={hits}")


def grade_test_run(answer: object, gold: dict[str, Any], spec: dict[str, Any], ctx: GradeContext) -> GradeResult:
    if ctx.workdir is None:
        return GradeResult(False, "no workdir for test_run")
    cmd = spec["cmd"]
    expect = int(spec.get("expect_returncode", 0))
    try:
        proc = subprocess.run(
            cmd,
            shell=True,
            cwd=str(ctx.workdir),
            capture_output=True,
            text=True,
            timeout=int(spec.get("timeout_s", 120)),
        )
    except subprocess.TimeoutExpired:
        return GradeResult(False, "test_run timed out")
    ok = proc.returncode == expect
    tail = (proc.stdout + proc.stderr).strip().splitlines()[-3:]
    return GradeResult(ok, f"rc={proc.returncode} (want {expect}); {' | '.join(tail)}")
