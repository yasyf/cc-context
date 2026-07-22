from typing import Any

from semble.utils import resolve_chunk

from common import OracleIndex, chunk_location, load_oracle_index, load_queries, write_golden


def generate(oracle: OracleIndex, query_data: dict[str, Any]) -> dict[str, list[dict]]:
    index = oracle.public_index()
    related = []
    for case in query_data["related"]:
        source = resolve_chunk(oracle.chunks, case["file_path"], case["line"])
        if source is None:
            raise ValueError(f"no indexed chunk contains {case['file_path']}:{case['line']}")
        results = index.find_related(source, top_k=case["top_k"], max_snippet_lines=0)
        result_paths = {result.chunk.file_path for result in results}
        if result_paths.isdisjoint(case["relevant_paths"]):
            raise AssertionError(
                f"related case {case['id']} returned none of its relevant paths: "
                f"{case['relevant_paths']}"
            )
        related.append(
            {
                "id": case["id"],
                "source": chunk_location(source),
                "top_k": case["top_k"],
                "relevant_paths": case["relevant_paths"],
                "results": [
                    {
                        **chunk_location(result.chunk),
                        "score": result.score,
                        "semantic_score": result.score,
                    }
                    for result in results
                ],
            }
        )
    return {"related": related}


def main() -> None:
    oracle = load_oracle_index()
    write_golden("related_results.json", generate(oracle, load_queries()))


if __name__ == "__main__":
    main()
