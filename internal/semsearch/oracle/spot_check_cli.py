from __future__ import annotations

import json
import math
import os
import subprocess
import sys
import tempfile
from pathlib import Path

from common import (
    CORPUS_DIR,
    GOLDENS_DIR,
    load_queries,
    read_json,
    require_deterministic_hash_seed,
    resolve_model_snapshot,
)


QUERY_IDS = (
    "nl_validate_bearer_token",
    "nl_parse_http_response",
    "symbol_session_token_validator",
)
SCORE_REL_TOL = 1e-7
SCORE_ABS_TOL = 1e-8


def compare_results(actual: list[dict], expected: list[dict]) -> None:
    if len(actual) != len(expected):
        raise AssertionError(f"result count differs: {len(actual)} != {len(expected)}")
    for position, (actual_result, expected_result) in enumerate(zip(actual, expected, strict=True)):
        for field in ("file_path", "start_line", "end_line"):
            if actual_result[field] != expected_result[field]:
                raise AssertionError(
                    f"result {position} {field} differs: "
                    f"{actual_result[field]!r} != {expected_result[field]!r}"
                )
        if not math.isclose(
            actual_result["score"],
            expected_result["score"],
            rel_tol=SCORE_REL_TOL,
            abs_tol=SCORE_ABS_TOL,
        ):
            raise AssertionError(
                f"result {position} score differs: "
                f"{actual_result['score']!r} != {expected_result['score']!r}"
            )


def main() -> None:
    require_deterministic_hash_seed()
    queries_by_id = {case["id"]: case for case in load_queries()["queries"]}
    query_cases = [queries_by_id[query_id] for query_id in QUERY_IDS]

    search_golden = read_json(GOLDENS_DIR / "search_results.json")
    golden_by_id = {case["id"]: case for case in search_golden["queries"]}
    snapshot = resolve_model_snapshot()
    semble = Path(sys.executable).with_name("semble")
    evidence = []

    with tempfile.TemporaryDirectory(prefix="ccx-semsearch-spot-check-") as cache:
        env = os.environ.copy()
        env.update(
            {
                "NO_COLOR": "1",
                "PYTHONHASHSEED": "0",
                "SEMBLE_CACHE_LOCATION": cache,
                "SEMBLE_MODEL_NAME": snapshot,
            }
        )
        for case in query_cases:
            completed = subprocess.run(
                [
                    semble,
                    "search",
                    case["query"],
                    CORPUS_DIR,
                    "--top-k",
                    str(case["top_k"]),
                    "--max-snippet-lines",
                    "0",
                    "--content",
                    "all",
                ],
                check=True,
                capture_output=True,
                env=env,
                text=True,
            )
            actual = json.loads(completed.stdout)
            expected = golden_by_id[case["id"]]
            compare_results(actual["results"], expected["results"])
            evidence.append(
                {
                    "id": case["id"],
                    "kind": case["kind"],
                    "query": case["query"],
                    "result_count": len(actual["results"]),
                    "matched": True,
                }
            )

    print(json.dumps({"spot_checks": evidence}, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
