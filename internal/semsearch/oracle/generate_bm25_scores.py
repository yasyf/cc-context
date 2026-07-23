from typing import Any

from semble.tokens import tokenize

from common import OracleIndex, chunk_location, load_oracle_index, load_queries, write_golden


def generate(index: OracleIndex, query_data: dict[str, Any]) -> dict[str, list[dict]]:
    queries = []
    for query in query_data["queries"]:
        tokens = tokenize(query["query"])
        scores = index.bm25.get_scores(tokens)
        documents = [
            {**chunk_location(chunk), "score": float(score)}
            for chunk, score in zip(index.chunks, scores, strict=True)
        ]
        queries.append(
            {
                "id": query["id"],
                "kind": query["kind"],
                "query": query["query"],
                "tokens": tokens,
                "documents": documents,
            }
        )
    return {"queries": queries}


def main() -> None:
    index = load_oracle_index()
    write_golden("bm25_scores.json", generate(index, load_queries()))


if __name__ == "__main__":
    main()
