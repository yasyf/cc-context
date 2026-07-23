// Package semsearch is the native semantic code-search engine behind
// ccx code search and ccx code related: a bit-exact port of semble's
// pipeline — tree-sitter chunking, model2vec embeddings, hand-rolled
// BM25, RRF fusion, and semble's ranking stack — gated on semble's own
// benchmark harness (NDCG@10 ≥ same-machine semble 0.5.2).
//
// Subpackages own one stage each: chunk (tree-sitter adapter + greedy
// sibling-packing), embed (resident model2vec WASM engine), rank
// (tokenizer, BM25, fusion, ranking), index (walker + per-repo cache).
package semsearch

// Chunk is one indexed unit of a file: a greedy sibling-packed span of
// AST nodes, or a line-window fallback. Lines are 1-based inclusive.
type Chunk struct {
	Path      string
	StartLine int
	EndLine   int
	Content   string
}

// Result is one ranked search hit. Score is the fused, boosted,
// penalty-adjusted effective score; SemanticScore is the raw cosine
// similarity (1 − distance) of the chunk embedding to the query, nil for
// hits outside the semantic candidate set (semble's None).
type Result struct {
	FilePath      string   `json:"file_path"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	Score         float64  `json:"score"`
	SemanticScore *float64 `json:"semantic_score,omitempty"`
	Content       string   `json:"content,omitempty"`
}
