import generate_bm25_scores
import generate_bm25_tokens
import generate_chunks
import generate_embeddings
import generate_related
import generate_search
from common import load_oracle_index, load_queries, write_golden


def main() -> None:
    index = load_oracle_index()
    queries = load_queries()
    write_golden("chunks.json", generate_chunks.generate(index))
    write_golden("bm25_tokens.json", generate_bm25_tokens.generate(index))
    write_golden("bm25_scores.json", generate_bm25_scores.generate(index, queries))
    write_golden("search_results.json", generate_search.generate(index, queries))
    write_golden("related_results.json", generate_related.generate(index, queries))
    write_golden("embeddings.json", generate_embeddings.generate(index, queries))


if __name__ == "__main__":
    main()
