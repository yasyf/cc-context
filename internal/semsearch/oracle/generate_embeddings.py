from semble.index.dense import embed_chunks
from semble.types import Chunk

from common import OracleIndex, load_oracle_index, write_golden


SAMPLE_TEXTS = (
    "find the HTTP response handler",
    "HandlerStack",
    "normalize_user_record",
)


def generate(index: OracleIndex) -> dict[str, list[dict]]:
    chunks = [
        Chunk(text, f"sample-{position}.txt", 1, 1)
        for position, text in enumerate(SAMPLE_TEXTS)
    ]
    vectors = embed_chunks(index.model, chunks)
    samples = [
        {"text": text, "vector": [float(value) for value in vector]}
        for text, vector in zip(SAMPLE_TEXTS, vectors, strict=True)
    ]
    return {"samples": samples}


def main() -> None:
    write_golden("embeddings.json", generate(load_oracle_index()))


if __name__ == "__main__":
    main()
