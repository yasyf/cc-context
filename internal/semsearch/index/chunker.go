package index

import (
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/chunk"
)

// Chunker splits one file's content into indexed chunks. Chunk.Path is set to
// indexedPath (repo-relative), lines 1-based inclusive.
type Chunker interface {
	// ID uniquely identifies the chunking behavior; the cache invalidates when
	// it changes, so a swapped chunker rebuilds transparently.
	ID() string
	// ChunkFile splits content (already read as text) for the file at
	// indexedPath, whose detected language is lang ("" if unknown).
	ChunkFile(indexedPath, lang, content string) []semsearch.Chunk
}

// DefaultChunker returns the tree-sitter AST chunker (semble chunk-boundary
// parity; language detected from the path's extension).
func DefaultChunker() Chunker { return treeChunker{} }

type treeChunker struct{}

// ID is versioned so a chunking-behavior change invalidates caches built
// before it.
func (treeChunker) ID() string { return "treesitter-v1" }

func (treeChunker) ChunkFile(indexedPath, _ /*lang*/, content string) []semsearch.Chunk {
	return chunk.Chunk(indexedPath, []byte(content))
}
