from typing import Any

from semble.ranking import resolve_alpha
from semble.search import _search_semantic, search

from common import OracleIndex, chunk_location, load_oracle_index, load_queries, write_golden


def generate(index: OracleIndex, query_data: dict[str, Any]) -> dict[str, list[dict]]:
    queries = []
    for query in query_data["queries"]:
        top_k = query["top_k"]
        semantic = _search_semantic(
            query["query"],
            index.model,
            index.semantic,
            index.chunks,
            top_k * 5,
            None,
        )
        semantic_scores = {result.chunk: result.score for result in semantic}
        results = search(
            query["query"],
            index.model,
            index.semantic,
            index.bm25,
            index.chunks,
            top_k,
        )
        result_paths = {result.chunk.file_path for result in results}
        if result_paths.isdisjoint(query["relevant_paths"]):
            raise AssertionError(
                f"query {query['id']} returned none of its relevant paths: {query['relevant_paths']}"
            )
        queries.append(
            {
                "id": query["id"],
                "kind": query["kind"],
                "query": query["query"],
                "top_k": top_k,
                "alpha": resolve_alpha(query["query"], None),
                "relevant_paths": query["relevant_paths"],
                "results": [
                    {
                        **chunk_location(result.chunk),
                        "score": result.score,
                        "semantic_score": semantic_scores.get(result.chunk),
                    }
                    for result in results
                ],
            }
        )
    return {"queries": queries}


def main() -> None:
    index = load_oracle_index()
    write_golden("search_results.json", generate(index, load_queries()))


if __name__ == "__main__":
    main()
