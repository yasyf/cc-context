from typing import Any

import numpy as np

from common import OracleIndex, chunk_location, load_oracle_index, load_queries, write_golden


def float32_vector(vector: np.ndarray) -> list[float]:
    return [float(value) for value in np.asarray(vector, dtype=np.float32)]


def generate(index: OracleIndex, query_data: dict[str, Any]) -> dict[str, Any]:
    # Semble created these vectors with singleton chunk batches.
    chunks = [
        {
            **chunk_location(chunk),
            "vector": float32_vector(vector),
        }
        for chunk, vector in zip(index.chunks, index.semantic.vectors, strict=True)
    ]
    queries = [
        {
            "id": query["id"],
            "query": query["query"],
            "vector": float32_vector(index.model.encode([query["query"]])[0]),
        }
        for query in query_data["queries"]
    ]
    return {"dims": index.model.dim, "chunks": chunks, "queries": queries}


def main() -> None:
    write_golden("embeddings.json", generate(load_oracle_index(), load_queries()))


if __name__ == "__main__":
    main()
