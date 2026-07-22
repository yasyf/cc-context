package index

import (
	"strings"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// Chunker splits one file's content into indexed chunks. The tree-sitter
// chunker (a sibling lane) implements this; until it merges, Build defaults to
// lineChunker. Chunk.Path is set to indexedPath (repo-relative), lines 1-based
// inclusive.
type Chunker interface {
	// ID uniquely identifies the chunking behavior; the cache invalidates when
	// it changes, so a swapped chunker rebuilds transparently.
	ID() string
	// ChunkFile splits content (already read as text) for the file at
	// indexedPath, whose detected language is lang ("" if unknown).
	ChunkFile(indexedPath, lang, content string) []semsearch.Chunk
}

// DefaultChunker returns the interim line-window chunker used until the
// tree-sitter chunker (a sibling lane) merges and replaces it here.
func DefaultChunker() Chunker { return lineChunker{} }

// lineTargetChars and lineMaxLines bound the fallback line-window chunker.
const (
	lineTargetChars = 1000
	lineMaxLines    = 60
)

// lineChunker is the interim fallback: fixed line windows bounded by a character
// budget. It exists only so the pipeline runs end-to-end before the tree-sitter
// chunker merges; it is not gated on semble chunk-boundary parity.
type lineChunker struct{}

// ID is versioned so a future line-chunker change or the tree-sitter swap
// invalidates caches built by this one.
func (lineChunker) ID() string { return "line-v1" }

// ChunkFile packs consecutive lines into windows of at most lineMaxLines that
// stay under lineTargetChars once non-empty.
func (lineChunker) ChunkFile(indexedPath, _ /*lang*/, content string) []semsearch.Chunk {
	lines := strings.Split(content, "\n")
	// A trailing newline yields a final empty element; drop it so line numbers
	// track the file's real lines.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return nil
	}

	var chunks []semsearch.Chunk
	start := 0               // 0-based index of the window's first line
	chars := 0               // running character count of the window
	flush := func(end int) { // end is exclusive, 0-based
		chunks = append(chunks, semsearch.Chunk{
			Path:      indexedPath,
			StartLine: start + 1,
			EndLine:   end,
			Content:   strings.Join(lines[start:end], "\n"),
		})
	}
	for i, line := range lines {
		lineLen := len(line) + 1 // include the joining newline
		count := i - start
		if count > 0 && (chars+lineLen > lineTargetChars || count >= lineMaxLines) {
			flush(i)
			start = i
			chars = 0
		}
		chars += lineLen
	}
	flush(len(lines))
	return chunks
}
