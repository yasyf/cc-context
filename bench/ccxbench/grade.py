"""Dispatch a task's grader over a run's parsed result.

Every task in this corpus uses a deterministic grader (no LLM judge), so grading is
never done by the model under test. test_run grades against the post-run workdir;
all others grade the structured output. An errored run is incorrect by definition.
"""

from __future__ import annotations

import json
from pathlib import Path

from cc_transcript import PrintResult, parse_print_result

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


def synthetic_result(structured_output: object, result_text: str = "", is_error: bool = False) -> PrintResult:
    """A zero-cost PrintResult for grading a known answer (grader self-tests, unit tests)."""
    element = {
        "type": "result",
        "is_error": is_error,
        "result": result_text,
        "structured_output": structured_output,
        "total_cost_usd": 0.0,
        "num_turns": 0,
        "session_id": "synthetic",
        "usage": {
            "input_tokens": 0,
            "output_tokens": 0,
            "cache_read_input_tokens": 0,
            "cache_creation_input_tokens": 0,
        },
        "modelUsage": {},
        "permission_denials": [],
    }
    return parse_print_result(json.dumps([element]).encode())


def grade(task: Task, pr: PrintResult, workdir: Path | None) -> GradeResult:
    """Grade one run. Raises on an unknown grader kind (fail loud, not silently wrong)."""
    fn = GRADERS.get(task.grader.kind)
    if fn is None:
        raise ValueError(f"unknown grader kind: {task.grader.kind}")
    if pr.is_error:
        return GradeResult(False, f"run errored: {(pr.result or '')[:80]}")
    ctx = GradeContext(result_text=pr.result or "", workdir=workdir)
    return fn(pr.structured_output, task.gold, task.grader.spec, ctx)
