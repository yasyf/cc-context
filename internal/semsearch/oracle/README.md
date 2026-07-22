# Semble 0.5.2 oracle

These scripts generate reference data for the native semantic-search port. They
run only the released `semble==0.5.2` package and the model revision recorded in
`../testdata/pins.json`. The checkout under `/tmp/ccx-fix-semble` is not imported
or added to `PYTHONPATH`.

## Set up the oracle environment

From the repository root:

```sh
uv venv --python 3.12 ~/.cache/ccx-semsearch/oracle/.venv
uv pip sync \
  --python ~/.cache/ccx-semsearch/oracle/.venv/bin/python \
  internal/semsearch/oracle/requirements.lock
```

The lock records the complete environment resolved from `semble==0.5.2`. Every
generator verifies the installed semble version against `pins.json`. It
resolves the pinned Hugging Face commit with `snapshot_download`, loads the
resulting local snapshot, and indexes the corpus as code, docs, and config
content.

## Generate all goldens

```sh
PYTHONHASHSEED=0 \
  ~/.cache/ccx-semsearch/oracle/.venv/bin/python \
  internal/semsearch/oracle/generate_all.py
```

`PYTHONHASHSEED=0` must be set before Python starts. Semble 0.5.2 forms a
candidate union from a set, so tied candidates can otherwise vary by process.
The command writes indented, key-sorted JSON with a final newline under
`internal/semsearch/testdata/goldens/`.

## Generate one golden

Run an individual script with the same interpreter and hash seed:

```sh
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_chunks.py
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_bm25_tokens.py
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_bm25_scores.py
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_search.py
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_related.py
PYTHONHASHSEED=0 ~/.cache/ccx-semsearch/oracle/.venv/bin/python internal/semsearch/oracle/generate_embeddings.py
```

The outputs are, respectively, chunk line boundaries plus a classification of
every corpus file, enriched BM25 document tokens, BM25 scores for every query
and chunk, fused and reranked searches, related-code searches, and float32
embedding samples. Search goldens record the query kind, resolved alpha, and
known relevant paths; generation fails if a case returns none of those paths.

Released 0.5.2 does not retain a semantic score on its final `SearchResult`.
`generate_search.py` runs the released `_search_semantic` candidate stage with
the same `top_k * 5` over-fetch used by the released `search` function, then
correlates those raw cosine similarities with the final released search output.
A BM25-only or query-boosted candidate has a null `semantic_score`. Related
results are semantic-only, so their `score` and `semantic_score` are equal.

Semble intentionally omits JSON and CSV data languages from all indexable
content families. It excludes a file below 128 bytes only when that file is
blank or whitespace-only; a short nonblank source file remains indexable.

## Spot-check the released CLI

After generating `search_results.json`, run:

```sh
PYTHONHASHSEED=0 \
  ~/.cache/ccx-semsearch/oracle/.venv/bin/python \
  internal/semsearch/oracle/spot_check_cli.py
```

The script runs the installed `semble search` command for two natural-language
query IDs and one symbol query ID with `--content all`, the pinned local model
snapshot, and an isolated cache. It compares file path, line range, and score
against `search_results.json`, prints JSON evidence, and exits nonzero on any
mismatch.
