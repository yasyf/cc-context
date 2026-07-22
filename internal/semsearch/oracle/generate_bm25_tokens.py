from semble.index.sparse import enrich_for_bm25
from semble.tokens import tokenize

from common import OracleIndex, chunk_location, load_oracle_index, write_golden


def generate(index: OracleIndex) -> dict[str, list[dict]]:
    documents = [
        {
            **chunk_location(chunk),
            "tokens": tokenize(enrich_for_bm25(chunk)),
        }
        for chunk in index.chunks
    ]
    return {"documents": documents}


def main() -> None:
    write_golden("bm25_tokens.json", generate(load_oracle_index()))


if __name__ == "__main__":
    main()
