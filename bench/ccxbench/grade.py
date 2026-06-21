"""Dispatch a task's grader over a run's envelope.

Every task in this corpus uses a deterministic grader (no LLM judge), so grading is
never done by the model under test. test_run grades against the post-run workdir;
all others grade the structured output. An errored run is incorrect by definition.
"""

from __future__ import annotations

from pathlib import Path

from .envelope import Envelope
from .graders import (
    GradeContext,
    grade_file_line,
    grade_file_match,
    grade_keywords,
    grade_set_match,
    grade_test_run,
)
from .types import GradeResult, Task

GRADERS = {
    "file_line": grade_file_line,
    "file_match": grade_file_match,
    "set_match": grade_set_match,
    "keywords": grade_keywords,
    "test_run": grade_test_run,
}


def grade(task: Task, env: Envelope, workdir: Path | None) -> GradeResult:
    """Grade one run. Raises on an unknown grader kind (fail loud, not silently wrong)."""
    fn = GRADERS.get(task.grader.kind)
    if fn is None:
        raise ValueError(f"unknown grader kind: {task.grader.kind}")
    if env.is_error:
        return GradeResult(False, f"run errored: {env.result_text[:80]}")
    ctx = GradeContext(result_text=env.result_text, workdir=workdir)
    return fn(env.structured_output, task.gold, task.grader.spec, ctx)
